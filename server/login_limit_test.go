package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func TestLoginRateLimitReturns429WithRetryAfter(t *testing.T) {
	clk := newFakeClock()
	s := &Server{loginLimit: newLoginLimiter(clk.now)}
	const remote = "192.0.2.10:40000"
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"ARTEX","password":"wrong"}`))
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		s.authLogin(w, r)
		return w
	}
	for i := 0; i < loginMaxAttempts; i++ {
		if w := post(); w.Code == 429 {
			t.Fatalf("attempt %d blocked early: %s", i+1, w.Body.String())
		}
	}
	w := post()
	if w.Code != 429 {
		t.Fatalf("status=%d want 429; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After=%q want 60", got)
	}
	if strings.Contains(w.Body.String(), `"token"`) {
		t.Fatalf("blocked login issued a token: %s", w.Body.String())
	}
}

func mustTrusted(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseTrustedProxies(t *testing.T) {
	if p, err := ParseTrustedProxies(" "); err != nil || len(p) != 0 {
		t.Fatalf("empty: %v, %v; want no proxies", p, err)
	}
	p, err := ParseTrustedProxies(" 127.0.0.1/32, 10.1.2.3/8 ,::1")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(p); got != "[127.0.0.1/32 10.0.0.0/8 ::1/128]" {
		t.Fatalf("got %s", got)
	}
	for _, bad := range []string{"proxy.local", "10.0.0.0/33", "127.0.0.1,nope"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Fatalf("%q: want an error", bad)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted := mustTrusted(t, "127.0.0.1/32,10.0.0.0/8")
	for _, tc := range []struct {
		name, remote string
		xff          []string
		trusted      []netip.Prefix
		want         string
	}{
		{"direct client, no proxies configured", "198.51.100.9:5000", nil, nil, "198.51.100.9"},
		{"direct client, proxies configured", "198.51.100.9:5000", nil, trusted, "198.51.100.9"},
		{"spoofed header, no proxies configured", "198.51.100.9:5000", []string{"203.0.113.1"}, nil, "198.51.100.9"},
		{"spoofed header from an untrusted peer", "198.51.100.9:5000", []string{"203.0.113.1"}, trusted, "198.51.100.9"},
		{"one trusted proxy", "127.0.0.1:5000", []string{"198.51.100.7"}, trusted, "198.51.100.7"},
		{"client-supplied entries left of the real hop", "127.0.0.1:5000", []string{"203.0.113.1, 10.0.0.9, 198.51.100.7"}, trusted, "198.51.100.7"},
		{"chain of trusted proxies", "127.0.0.1:5000", []string{"203.0.113.1, 198.51.100.7, 10.0.0.5"}, trusted, "198.51.100.7"},
		{"IPv6 client behind the proxy", "127.0.0.1:5000", []string{"2001:db8::7"}, trusted, "2001:db8::7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, tc.trusted); got != tc.want {
				t.Fatalf("clientIP=%s want %s", got, tc.want)
			}
		})
	}
}

func TestLoginLimiterSweepsIdleEntries(t *testing.T) {
	clk := newFakeClock()
	l := newLoginLimiter(clk.now)
	l.allow("192.0.2.20")
	clk.advance(loginMaxBlock - time.Second)
	l.allow("192.0.2.21")
	if len(l.entries) != 2 {
		t.Fatalf("entries=%d want 2 before either is idle for %v", len(l.entries), loginMaxBlock)
	}
	clk.advance(loginSweepEvery)
	l.allow("192.0.2.22")
	if _, ok := l.entries["192.0.2.20"]; ok || len(l.entries) != 2 {
		t.Fatalf("idle entry not swept: %d entries", len(l.entries))
	}
}

func TestLoginLimiterCapsEntries(t *testing.T) {
	l := newLoginLimiter(newFakeClock().now)
	for i := 0; i <= loginMaxEntries; i++ {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if len(l.entries) != loginMaxEntries {
		t.Fatalf("entries=%d want the cap %d", len(l.entries), loginMaxEntries)
	}
}

func TestEvictOldestKeepsBlockedEntries(t *testing.T) {
	clk := newFakeClock()
	l := newLoginLimiter(clk.now)
	now := clk.now()
	l.entries["blocked"] = &loginEntry{lastSeen: now.Add(-2 * time.Hour), blockedUntil: now.Add(time.Hour), block: time.Minute}
	l.entries["idle"] = &loginEntry{lastSeen: now.Add(-time.Hour)}
	l.evictOldest()
	if _, ok := l.entries["blocked"]; !ok {
		t.Fatal("evicted a blocked address while an idle one existed")
	}
	if _, ok := l.entries["idle"]; ok {
		t.Fatal("kept the idle address")
	}
}
