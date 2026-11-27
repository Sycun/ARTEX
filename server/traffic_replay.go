package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/traffic"
)

func (s *Server) getTrafficStream(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "traffic disabled")
		return
	}
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("exchange"))
	if id == "" {
		writeErr(w, 400, "missing exchange")
		return
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	direction := strings.ToLower(strings.TrimSpace(q.Get("direction")))
	page, err := tr.StreamMessagePageByDirection(id, direction, offset, limit)
	if errors.Is(err, traffic.ErrBadStreamDirection) {
		writeErr(w, 400, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, page)
}

const maxReplayRequestBytes = 16 << 20

// replayTraffic sends an edited request through the recording proxy.
// apply_rules defaults to false, so rewrite rules stay off unless the caller opts in.
// The independent Repeater workspace is unchanged.
func (s *Server) replayTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 503, "traffic disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxReplayRequestBytes)
	var body struct {
		Method     string                 `json:"method"`
		URL        string                 `json:"url"`
		Headers    []traffic.ReplayHeader `json:"headers"`
		Body       string                 `json:"body"`
		BodyBase64 bool                   `json:"body_base64"`
		ApplyRules bool                   `json:"apply_rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, 413, "replay request too large")
			return
		}
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	payload := []byte(body.Body)
	if body.BodyBase64 {
		decoded, err := base64.StdEncoding.DecodeString(body.Body)
		if err != nil {
			writeErr(w, 400, "body is not valid base64: "+err.Error())
			return
		}
		payload = decoded
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := tr.Replay(ctx, traffic.ReplayRequest{
		Method: body.Method, URL: body.URL, Headers: body.Headers, Body: payload, ApplyRules: body.ApplyRules,
	})
	switch {
	case errors.Is(err, traffic.ErrReplayTooLarge):
		writeErr(w, 413, err.Error())
		return
	case errors.Is(err, traffic.ErrReplayInvalid):
		writeErr(w, 400, err.Error())
		return
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, 504, "replay timed out")
		return
	case err != nil:
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"status": res.Status, "headers": res.Headers, "raw": res.Raw, "duration_ms": res.DurationMS,
		"truncated": res.Truncated, "recorded": res.Recorded,
	})
}
