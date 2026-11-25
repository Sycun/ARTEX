package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testOIDCClientID = "artex-client"
	testOIDCSecret   = "local:secret+value"
	testOIDCSubject  = "employee-123"
)

type oidcFixture struct {
	issuer    string
	server    *httptest.Server
	discovery atomic.Int64
	jwks      atomic.Int64
	exchanges atomic.Int64
	mu        sync.Mutex
	key       *rsa.PrivateKey
	kid       string
	grants    map[string]oidcTestGrant
}

type oidcTestGrant struct {
	token, challenge, redirectURI string
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	idp := &oidcFixture{key: testRSAKey(t), kid: "key-1", grants: make(map[string]oidcTestGrant)}
	idp.server = httptest.NewUnstartedServer(http.HandlerFunc(idp.serve))
	idp.issuer = "http://" + idp.server.Listener.Addr().String() + "/tenant"
	idp.server.Start()
	t.Cleanup(idp.server.Close)
	return idp
}

func (f *oidcFixture) provider(t *testing.T) *standardOIDC {
	t.Helper()
	provider, err := newStandardOIDC(f.issuer, testOIDCClientID, testOIDCSecret)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func (f *oidcFixture) rotate(t *testing.T) {
	t.Helper()
	key := testRSAKey(t)
	f.mu.Lock()
	f.key, f.kid = key, "key-2"
	f.mu.Unlock()
}

func (f *oidcFixture) issue(q url.Values, change func(*jwt.Token) any) (string, error) {
	if q.Get("client_id") != testOIDCClientID || q.Get("scope") != "openid" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("redirect_uri") == "" {
		return "", errors.New("invalid authorization request")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": f.issuer, "sub": testOIDCSubject, "aud": testOIDCClientID,
		"nonce": q.Get("nonce"), "iat": now.Add(-time.Second).Unix(), "exp": now.Add(time.Minute).Unix(),
	})
	token.Header["kid"] = f.kid
	var key any = f.key
	if change != nil {
		if replacement := change(token); replacement != nil {
			key = replacement
		}
	}
	signed, err := token.SignedString(key)
	if err != nil {
		return "", err
	}
	code := randomURL(18)
	f.grants[code] = oidcTestGrant{signed, q.Get("code_challenge"), q.Get("redirect_uri")}
	return code, nil
}

func (f *oidcFixture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/tenant/.well-known/openid-configuration":
		f.discovery.Add(1)
		writeJSON(w, http.StatusOK, oidcDiscovery{
			Issuer: f.issuer, Authorization: f.issuer + "/authorize?realm=employees",
			Token: f.issuer + "/token", JWKS: f.issuer + "/keys",
			Algorithms: []string{"RS256", "HS256"}, PKCE: []string{"S256"},
			TokenAuth: []string{"client_secret_basic"},
		})
	case "/tenant/authorize":
		code, err := f.issue(r.URL.Query(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		q := url.Values{"code": {code}, "state": {r.URL.Query().Get("state")}}
		http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?"+q.Encode(), http.StatusFound)
	case "/tenant/token":
		f.exchanges.Add(1)
		user, secret, ok := r.BasicAuth()
		if r.Method != http.MethodPost || !ok || user != url.QueryEscape(testOIDCClientID) || secret != url.QueryEscape(testOIDCSecret) ||
			r.ParseForm() != nil || r.PostForm.Get("grant_type") != "authorization_code" {
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		code := r.PostForm.Get("code")
		f.mu.Lock()
		grant, exists := f.grants[code]
		delete(f.grants, code)
		f.mu.Unlock()
		if !exists || grant.challenge != pkceChallenge(r.PostForm.Get("code_verifier")) || grant.redirectURI != r.PostForm.Get("redirect_uri") {
			http.Error(w, "invalid grant", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id_token": grant.token})
	case "/tenant/keys":
		f.jwks.Add(1)
		f.mu.Lock()
		key, kid := f.key.PublicKey, f.kid
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"keys": []oidcJWK{{
			ID: kid, Type: "RSA", Algorithm: "RS256", Use: "sig", Operations: []string{"verify"},
			Modulus:  base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			Exponent: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	default:
		http.NotFound(w, r)
	}
}

func TestStandardOIDCDiscoveryPKCEAndKeyRotation(t *testing.T) {
	f := newOIDCFixture(t)
	provider := f.provider(t)
	login := loginState{
		state: "login-state", nonce: "login-nonce", verifier: randomURL(32),
		redirectURI: "https://artex.example" + callbackPath,
	}
	location, err := provider.AuthorizationURL(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	u := mustURL(t, location)
	q := u.Query()
	if u.Path != "/tenant/authorize" || q.Get("realm") != "employees" || q.Get("state") != login.state ||
		q.Get("nonce") != login.nonce || q.Get("redirect_uri") != login.redirectURI || q.Get("code_challenge") != pkceChallenge(login.verifier) {
		t.Fatalf("authorization parameters: %s", location)
	}
	exchange := func(t *testing.T, request loginState, change func(*jwt.Token) any) (string, error) {
		t.Helper()
		code, err := f.issue(q, change)
		if err != nil {
			t.Fatal(err)
		}
		return provider.Exchange(t.Context(), code, request)
	}
	for _, use := range []struct {
		rotate          bool
		discovery, jwks int64
	}{{false, 1, 1}, {false, 1, 1}, {true, 1, 2}} {
		if use.rotate {
			f.rotate(t)
		}
		sub, err := exchange(t, login, nil)
		if err != nil || sub != testOIDCSubject || f.discovery.Load() != use.discovery || f.jwks.Load() != use.jwks {
			t.Fatalf("subject=%q discovery=%d JWKS=%d err=%v", sub, f.discovery.Load(), f.jwks.Load(), err)
		}
	}
	for _, field := range []string{"verifier", "redirect URI"} {
		t.Run("wrong "+field, func(t *testing.T) {
			request := login
			if field == "verifier" {
				request.verifier = randomURL(32)
			} else {
				request.redirectURI += "/other"
			}
			if _, err := exchange(t, request, nil); err == nil {
				t.Fatal("invalid code exchange accepted")
			}
		})
	}

	wrongKey := testRSAKey(t)
	setClaim := func(name string, value any) func(*jwt.Token) any {
		return func(token *jwt.Token) any {
			claims := token.Claims.(jwt.MapClaims)
			if value == nil {
				delete(claims, name)
			} else {
				claims[name] = value
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*jwt.Token) any
	}{
		{"wrong signature", func(*jwt.Token) any { return wrongKey }},
		{"unknown kid", func(token *jwt.Token) any { token.Header["kid"] = "unpublished"; return nil }},
		{"malformed kid", func(token *jwt.Token) any { token.Header["kid"] = true; return nil }},
		{"symmetric algorithm", func(token *jwt.Token) any {
			token.Method = jwt.SigningMethodHS256
			token.Header["alg"] = "HS256"
			return []byte(testOIDCSecret)
		}},
		{"unadvertised RSA algorithm", func(token *jwt.Token) any {
			token.Method = jwt.SigningMethodRS512
			token.Header["alg"] = "RS512"
			return nil
		}},
		{"wrong issuer", setClaim("iss", f.issuer+"/other")},
		{"missing issuer", setClaim("iss", nil)},
		{"wrong nonce", setClaim("nonce", "other-nonce")},
		{"missing nonce", setClaim("nonce", nil)},
		{"wrong audience", setClaim("aud", []string{"other-client"})},
		{"missing audience", setClaim("aud", nil)},
		{"wrong authorized party", setClaim("azp", "other-client")},
		{"empty authorized party", setClaim("azp", "")},
		{"null authorized party", setClaim("azp", json.RawMessage("null"))},
		{"multiple audiences without authorized party", setClaim("aud", []string{testOIDCClientID, "other-client"})},
		{"untrusted audience with authorized party", func(token *jwt.Token) any {
			claims := token.Claims.(jwt.MapClaims)
			claims["aud"] = []string{testOIDCClientID, "other-client"}
			claims["azp"] = testOIDCClientID
			return nil
		}},
		{"missing subject", setClaim("sub", nil)},
		{"missing issued at", setClaim("iat", nil)},
		{"missing expiration", setClaim("exp", nil)},
		{"expired", setClaim("exp", time.Now().Add(-time.Minute).Unix())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if sub, err := exchange(t, login, tc.change); err == nil {
				t.Fatalf("invalid ID token accepted for %q", sub)
			}
		})
	}
	sub, err := exchange(t, login, func(token *jwt.Token) any {
		claims := token.Claims.(jwt.MapClaims)
		claims["aud"] = []string{testOIDCClientID}
		claims["azp"] = testOIDCClientID
		return nil
	})
	if err != nil || sub != testOIDCSubject {
		t.Fatalf("valid authorized party rejected: %q %v", sub, err)
	}
}

func TestOIDCDiscoveryRejectsMismatchedIssuerAndUnsupportedPKCE(t *testing.T) {
	for _, field := range []string{"issuer", "PKCE"} {
		t.Run(field, func(t *testing.T) {
			idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				issuer := "http://" + r.Host
				metadata := oidcDiscovery{
					Issuer: issuer, Authorization: issuer + "/authorize", Token: issuer + "/token", JWKS: issuer + "/keys",
					Algorithms: []string{"RS256"}, PKCE: []string{"S256"},
				}
				if field == "issuer" {
					metadata.Issuer += "/different-issuer"
				} else {
					metadata.PKCE = []string{"plain"}
				}
				writeJSON(w, http.StatusOK, metadata)
			}))
			defer idp.Close()
			provider, err := newStandardOIDC(idp.URL, testOIDCClientID, testOIDCSecret)
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.AuthorizationURL(t.Context(), loginState{})
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("unexpected discovery result: %v", err)
			}
		})
	}
}

func TestEnterpriseStandardOIDCStateAndSession(t *testing.T) {
	f := newOIDCFixture(t)
	loginServer := newEnterpriseHTTPTestServer(t)
	loginServer.publicOrigin = "https://artex.example"
	loginServer.oidc = f.provider(t)
	callbackServer := &Server{entMode: true, ent: loginServer.ent, jwtKey: loginServer.jwtKey,
		publicOrigin: loginServer.publicOrigin, oidc: f.provider(t)}
	loginHandler, callbackHandler := loginServer.Handler(), callbackServer.Handler()
	if _, err := loginServer.ent.DB.ExecContext(t.Context(), `
		INSERT INTO ent_users (id, display_name, directory_status, employment, external_id)
		VALUES ('directory-test-user','测试员工','verified','active',$1)`, testOIDCSubject); err != nil {
		t.Fatal(err)
	}

	begin := func(t *testing.T) (string, *http.Cookie, string) {
		t.Helper()
		w := httptest.NewRecorder()
		loginHandler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/enterprise/login?prefer=demo-reader-a", nil))
		if w.Code != http.StatusFound {
			t.Fatalf("login status=%d body=%s", w.Code, w.Body)
		}
		location := w.Header().Get("Location")
		q := mustURL(t, location).Query()
		state := q.Get("state")
		binding := responseCookie(t, w.Result(), "__Host-artex_login_"+state)
		if !binding.HttpOnly || !binding.Secure || binding.SameSite != http.SameSiteLaxMode || binding.Path != "/" {
			t.Fatal("invalid login cookie attributes")
		}
		var nonce, verifier, redirect, prefer string
		if err := loginServer.ent.DB.QueryRowContext(t.Context(), `
			SELECT nonce, code_verifier, redirect_uri, prefer FROM ent_login_states
			WHERE state=$1 AND expires_at > now()`, state).Scan(&nonce, &verifier, &redirect, &prefer); err != nil {
			t.Fatal(err)
		}
		if nonce != q.Get("nonce") || pkceChallenge(verifier) != q.Get("code_challenge") || redirect != loginServer.origin()+callbackPath || prefer != "demo-reader-a" {
			t.Fatalf("incorrect persisted login parameters")
		}
		client := f.server.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Get(location)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		callback, err := resp.Location()
		if err != nil || resp.StatusCode != http.StatusFound {
			t.Fatalf("IdP authorize status=%d location=%v", resp.StatusCode, err)
		}
		return callback.RequestURI(), binding, state
	}
	callback, binding, state := begin(t)
	call := func(callback string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, callback, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		callbackHandler.ServeHTTP(w, r)
		return w
	}
	if w := call(callback, nil); w.Code != http.StatusBadRequest || f.exchanges.Load() != 0 {
		t.Fatalf("unbound callback status=%d exchanges=%d", w.Code, f.exchanges.Load())
	}
	invalid := *binding
	invalid.Value += "changed"
	if w := call(callback, &invalid); w.Code != http.StatusBadRequest || f.exchanges.Load() != 0 {
		t.Fatalf("wrong binding callback status=%d exchanges=%d", w.Code, f.exchanges.Load())
	}

	// Both callbacks carry the original browser cookie and arrive at a different server instance.
	attempts := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { attempts <- call(callback, binding) }()
	}
	var session *http.Cookie
	for range 2 {
		w := <-attempts
		switch w.Code {
		case http.StatusFound:
			if session != nil || w.Header().Get("Location") != loginServer.origin()+"/enterprise/landing" {
				t.Fatalf("unexpected callback redirect: %v", w.Header())
			}
			session = responseCookie(t, w.Result(), "artex_token")
			if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.MaxAge <= 0 {
				t.Fatal("invalid session cookie attributes")
			}
		case http.StatusBadRequest:
		default:
			t.Fatalf("unexpected callback status=%d body=%s", w.Code, w.Body)
		}
	}
	if session == nil || f.exchanges.Load() != 1 {
		t.Fatalf("session issued=%v exchanges=%d", session != nil, f.exchanges.Load())
	}
	if verifyJWT(session.Value, loginServer.jwtKey) {
		t.Fatal("enterprise session accepted as a standalone administrator token")
	}
	if w := call(callback, binding); w.Code != http.StatusBadRequest || f.exchanges.Load() != 1 {
		t.Fatalf("replay status=%d exchanges=%d", w.Code, f.exchanges.Load())
	}
	var persisted bool
	if err := loginServer.ent.DB.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM ent_login_states WHERE state=$1)", state).Scan(&persisted); err != nil || persisted {
		t.Fatalf("state remains=%v err=%v", persisted, err)
	}
	me := httptest.NewRequest(http.MethodGet, "/api/enterprise/me", nil)
	me.AddCookie(session)
	w := httptest.NewRecorder()
	loginHandler.ServeHTTP(w, me)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"gate":"ready"`) || !strings.Contains(w.Body.String(), `"user_id":"directory-test-user"`) {
		t.Fatalf("OIDC session is unusable on login instance: %d %s", w.Code, w.Body)
	}

	expiredCallback, expiredCookie, expiredState := begin(t)
	result, err := loginServer.ent.DB.ExecContext(t.Context(), "UPDATE ent_login_states SET expires_at=now()-interval '1 second' WHERE state=$1", expiredState)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("expired login rows=%d err=%v", rows, err)
	}
	if w := call(expiredCallback, expiredCookie); w.Code != http.StatusBadRequest || f.exchanges.Load() != 1 {
		t.Fatalf("expired state status=%d exchanges=%d", w.Code, f.exchanges.Load())
	}

	for _, path := range []string{"/api/demo-idp/authorize", "/api/demo-idp/.well-known/openid-configuration"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(session)
		loginHandler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("demo endpoint %s in OIDC mode: %d", path, w.Code)
		}
	}
}
