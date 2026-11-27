package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

func TestSameLLMEndpoint(t *testing.T) {
	for _, tc := range []struct {
		provA, baseA, provB, baseB string
		want                       bool
	}{
		{"anthropic", "https://gw.example", "anthropic", "https://gw.example/", true},
		{"anthropic", "https://gw.example/v1/messages", "anthropic", "https://gw.example", true},
		{"", "", "anthropic", "", true},
		{"openai", " https://gw.example/v1/chat/completions ", "openai", "https://gw.example/v1", true},
		{"anthropic", "https://evil.example", "anthropic", "https://gw.example", false},
		{"anthropic", "", "anthropic", "https://gw.example", false},
		{"openai", "", "anthropic", "", false},
		{"openai", "https://gw.example/v1", "openai-responses", "https://gw.example/v1", false},
		{"gemini", "https://gw.example/v1beta", "gemini", "https://gw.example/v1beta/", true},
		{"jev", "https://gw.example/v1", "jev", "https://gw.example/v1/", true},
		{"jev", "https://evil.example", "jev", "https://gw.example", false},
	} {
		if got := sameLLMEndpoint(tc.provA, tc.baseA, tc.provB, tc.baseB); got != tc.want {
			t.Errorf("sameLLMEndpoint(%q,%q,%q,%q)=%v want %v", tc.provA, tc.baseA, tc.provB, tc.baseB, got, tc.want)
		}
	}
}

func TestLLMTestGlobalKeyOnlyForGlobalEndpoint(t *testing.T) {
	var savedHits atomic.Int32
	var savedKey atomic.Value
	saved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		savedHits.Add(1)
		savedKey.Store(r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(saved.Close)
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)

	s := &Server{}
	s.llmCfg = agent.ConfigFrom("anthropic", "m", saved.URL, "global-secret", "")
	post := func(base string) map[string]any {
		b, _ := json.Marshal(map[string]any{
			"provider": "anthropic", "model": "m", "base_url": base, "api_key": "", "streaming": false,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/llm/test", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.testLLM(rec, req)
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp
	}
	if resp := post(other.URL); otherHits.Load() != 0 || resp["ok"] != false {
		t.Fatalf("global key went to a form-supplied host: hits=%d resp=%v", otherHits.Load(), resp)
	}
	post(saved.URL + "/")
	if k, _ := savedKey.Load().(string); savedHits.Load() != 1 || k != "global-secret" {
		t.Fatalf("global key not used for its own endpoint: hits=%d key=%q", savedHits.Load(), k)
	}
}
