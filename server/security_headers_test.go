package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersStayOffDocumentPolicyForAPIAndMCP(t *testing.T) {
	s := &Server{}
	var flushed bool
	h := s.securityHeaders(limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("SSE and MCP responses must still be able to flush")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: ok\n\n"))
		flusher.Flush()
		flushed = true
	})))
	for _, path := range []string{"/mcp", "/api/enterprise/login", "/api/enterprise/callback", "/api/logs/stream"} {
		flushed = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if !flushed {
			t.Fatalf("%s: handler did not flush", path)
		}
		hdr := rec.Header()
		if hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: missing nosniff", path)
		}
		if hdr.Get("X-Frame-Options") != "SAMEORIGIN" {
			t.Fatalf("%s: X-Frame-Options=%q", path, hdr.Get("X-Frame-Options"))
		}
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'self'") {
			t.Fatalf("%s: CSP=%q", path, hdr.Get("Content-Security-Policy"))
		}
		if hdr.Get("Cross-Origin-Opener-Policy") != "" || hdr.Get("Cross-Origin-Resource-Policy") != "" {
			t.Fatalf("%s: document isolation headers leaked onto an API route: %v", path, hdr)
		}
		if hdr.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("%s: content type %q", path, hdr.Get("Content-Type"))
		}
	}
}

func TestConsoleHeadersAreDocumentOnly(t *testing.T) {
	h := http.Header{}
	consoleHeaders(h)
	if h.Get("Cross-Origin-Opener-Policy") != "same-origin" || h.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("console headers = %v", h)
	}
}

func TestTrustedProxyRequiredForHSTS(t *testing.T) {
	s := &Server{}
	h := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/enterprise/callback", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "203.0.113.8:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("untrusted peer must not be able to force HSTS")
	}

	s.trustedProxies = mustTrusted(t, "203.0.113.8/32")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("trusted proxy HTTPS did not set HSTS")
	}
}
