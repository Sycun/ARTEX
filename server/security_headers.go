package server

import (
	"net/http"
	"net/netip"
)

// Security headers are split in two layers so the API and the operator console
// each get what applies to them.
//
// securityHeaders is safe on every response, including:
//   - OIDC top-level redirects (/api/enterprise/login, /api/enterprise/callback):
//     X-Frame-Options and frame-ancestors do not block a browser from following
//     Location, and these routes do not receive COOP/CORP.
//   - SSE (/api/logs/stream, /api/update/stream) and /mcp Streamable HTTP:
//     the wrapper implements Flush and does not set a document CORP/COOP policy.
//     Those two headers are applied only by consoleHeaders on the HTML console.

// securityHeaders applies the response headers that are safe for every response,
// including the JSON API. It wraps the whole handler tree so no route can forget
// them.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// Do not leak workspace paths and task ids to third parties.
		h.Set("Referrer-Policy", "same-origin")
		// The API is JSON, but a proxied error page must still not be framed.
		// SAMEORIGIN plus frame-ancestors 'self' does not affect fetch, SSE, or
		// a top-level OIDC navigation.
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
		if s.isHTTPS(r) {
			// Ignored by browsers over plain HTTP, so it only takes effect behind
			// the TLS-terminating proxy this deployment is documented to use.
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// consoleHeaders applies the headers that only make sense on documents, on top of
// the ones securityHeaders already set. Called from the console route only.
// COOP same-origin would sever window.opener on an OIDC popup, and CORP
// same-origin would hide /mcp and SSE from a cross-origin client, so neither
// header is set on /api, /mcp, or the OIDC redirect routes.
func consoleHeaders(h http.Header) {
	// The console needs no device APIs; keep them from being promptable.
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	// Separate the console's browsing context from the tabs it opens.
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// isHTTPS reports whether the request reached us over TLS, either directly or
// through a trusted proxy that says so. X-Forwarded-Proto is only believed when
// the peer is in ARTEX_TRUSTED_PROXIES, the same rule clientIP uses: a direct
// client could otherwise claim HTTPS and get a header it cannot back up.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") != "https" {
		return false
	}
	peer, err := netip.ParseAddr(peerIP(r))
	if err != nil {
		return false
	}
	return isTrusted(peer, s.trustedProxies)
}
