package chatbridge

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// AllowLocalTargetsEnv lets a local test server use http://127.0.0.1.
// The chat bridge does not share the enterprise notification client.
const AllowLocalTargetsEnv = "ARTEX_CHAT_BRIDGE_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

func blockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func blockLocalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot resolve %q", host)
	}
	if blockedDialIP(ip) {
		return fmt.Errorf("refusing loopback or link-local address %s (set %s=1 for a local test server)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

func newGuardedClient(timeout time.Duration) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	transport := &http.Transport{}
	if ok {
		transport = base.Clone()
	}
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockLocalDial}).DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 {
				return errors.New("chat bridge does not follow redirects")
			}
			return nil
		},
	}
}

func validateEndpointURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("API URL must be an http(s) URL without user info")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && blockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing loopback or link-local address %s (set %s=1 for a local test server)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	return redactURLs(err.Error())
}

func redactURLs(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			if u, err := url.Parse(rest[:end]); err == nil && u.Host != "" {
				b.WriteString(u.Scheme + "://" + u.Host + "/…")
			} else {
				b.WriteString("(unparsed address)")
			}
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
