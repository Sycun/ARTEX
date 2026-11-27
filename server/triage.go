package server

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/intercept"
)

const (
	settingTriageProfile       = "triage_profile_id"
	settingTriageMinConfidence = "triage_min_confidence"
)

func allowPrivateLLMEndpoints() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_LLM_PRIVATE_ENDPOINTS"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// triageClient builds the advisory triage client from the configured profile, or
// nil when triage is off. Fail-open by design.
func (s *Server) triageClient() agent.TriageClient {
	id := s.triageProfileID()
	if id <= 0 {
		return nil
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return nil
	}
	if strings.TrimSpace(p.BaseURL) == "" || strings.TrimSpace(p.Model) == "" {
		return nil
	}
	client := s.triageHTTPClient(p.BaseURL)
	if client == nil {
		return nil
	}
	slog.Info("[triage] initialized triage client", "profile_id", id)
	return &agent.SystemOneClient{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model, HTTP: client}
}

func (s *Server) triageHTTPClient(baseURL string) *http.Client {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		slog.Warn("[triage] invalid base_url, dedup stays off", "err", err)
		return nil
	}
	allowPrivate := allowPrivateLLMEndpoints()
	if !allowPrivate {
		host := u.Hostname()
		if ip, err := netip.ParseAddr(host); err == nil && intercept.IsNonPublicAddr(ip) {
			slog.Warn("[triage] non-public base_url refused, dedup stays off", "host", host)
			return nil
		}
	}
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				host = address
			}
			ip, parseErr := netip.ParseAddr(host)
			if parseErr != nil {
				return fmt.Errorf("triage: non-IP dial address %q", address)
			}
			if intercept.IsNonPublicAddr(ip) {
				return fmt.Errorf("triage: refusing non-public address %s", ip)
			}
			return nil
		}
	}
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Transport: tr,
		Timeout:   agent.TriageHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("triage: redirects are not followed")
		},
	}
}

func (s *Server) applyTriageThreshold() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	raw, ok, err := s.m.pg.GetSetting(settingTriageMinConfidence)
	if err != nil || !ok {
		return
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && v >= 0 && v <= 1 {
		agent.SetTriageMinConfidence(v)
	}
}

func (s *Server) triageMinConfidence() float64 {
	if s.m == nil || s.m.pg == nil {
		return 0
	}
	raw, ok, err := s.m.pg.GetSetting(settingTriageMinConfidence)
	if err != nil || !ok {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return v
}

func (s *Server) triageProfileID() int64 {
	if s.m == nil || s.m.pg == nil {
		return 0
	}
	raw, ok, err := s.m.pg.GetSetting(settingTriageProfile)
	if err != nil || !ok {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}
