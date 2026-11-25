package server

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type OIDCProvider interface {
	Issuer() string
	AuthorizationURL(context.Context, loginState) (string, error)
	Exchange(context.Context, string, loginState) (subject string, err error)
}

const (
	oidcHTTPTimeout = 10 * time.Second
	oidcJWKSMaxAge  = 5 * time.Minute
	oidcMaxBody     = 1 << 20
)

type oidcDiscovery struct {
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	JWKS          string   `json:"jwks_uri"`
	Algorithms    []string `json:"id_token_signing_alg_values_supported"`
	TokenAuth     []string `json:"token_endpoint_auth_methods_supported"`
	PKCE          []string `json:"code_challenge_methods_supported"`
}

type standardOIDC struct {
	issuer, clientID, clientSecret string
	client                         http.Client

	mu          sync.Mutex
	metadata    oidcDiscovery
	keys        []oidcPublicKey
	keysExpires time.Time
}

func newStandardOIDC(issuer, clientID, clientSecret string) (*standardOIDC, error) {
	u, err := oidcEndpoint(issuer)
	if err != nil || u.RawQuery != "" || u.ForceQuery || clientID == "" {
		return nil, errors.New("OIDC requires a valid issuer and client ID")
	}
	return &standardOIDC{
		issuer: issuer, clientID: clientID, clientSecret: clientSecret,
		client: http.Client{
			Timeout: oidcHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (p *standardOIDC) Issuer() string { return p.issuer }

func oidcEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") {
		return nil, errors.New("invalid OIDC endpoint")
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return u, nil
		}
	}
	return nil, errors.New("OIDC endpoint requires HTTPS")
}

func (p *standardOIDC) discover(ctx context.Context) (oidcDiscovery, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.metadata.Issuer != "" {
		return p.metadata, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return oidcDiscovery{}, fmt.Errorf("OIDC discovery request: %w", err)
	}
	var metadata oidcDiscovery
	if err := p.readJSON(req, &metadata); err != nil {
		return oidcDiscovery{}, fmt.Errorf("OIDC discovery: %w", err)
	}
	if metadata.Issuer != p.issuer {
		return oidcDiscovery{}, errors.New("OIDC discovery issuer mismatch")
	}
	for _, endpoint := range []string{metadata.Authorization, metadata.Token, metadata.JWKS} {
		if _, err := oidcEndpoint(endpoint); err != nil {
			return oidcDiscovery{}, err
		}
	}
	if len(metadata.PKCE) > 0 && !slices.Contains(metadata.PKCE, "S256") {
		return oidcDiscovery{}, errors.New("OIDC provider does not support PKCE S256")
	}
	if _, err := p.tokenAuthMethod(metadata); err != nil {
		return oidcDiscovery{}, err
	}
	p.metadata = metadata
	return metadata, nil
}

func (p *standardOIDC) tokenAuthMethod(metadata oidcDiscovery) (string, error) {
	if p.clientSecret == "" {
		if slices.Contains(metadata.TokenAuth, "none") {
			return "none", nil
		}
	} else {
		if len(metadata.TokenAuth) == 0 || slices.Contains(metadata.TokenAuth, "client_secret_basic") {
			return "client_secret_basic", nil
		}
		if slices.Contains(metadata.TokenAuth, "client_secret_post") {
			return "client_secret_post", nil
		}
	}
	return "", errors.New("OIDC provider does not support the configured token endpoint credentials")
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizationURL(endpoint, clientID string, login loginState) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("scope", "openid")
	q.Set("redirect_uri", login.redirectURI)
	q.Set("state", login.state)
	q.Set("nonce", login.nonce)
	q.Set("code_challenge", pkceChallenge(login.verifier))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *standardOIDC) AuthorizationURL(ctx context.Context, login loginState) (string, error) {
	metadata, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return authorizationURL(metadata.Authorization, p.clientID, login)
}

var oidcRSAAlgorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}

func (p *standardOIDC) Exchange(ctx context.Context, code string, login loginState) (string, error) {
	metadata, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	method, err := p.tokenAuthMethod(metadata)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {login.verifier},
		"redirect_uri":  {login.redirectURI},
	}
	if method != "client_secret_basic" {
		form.Set("client_id", p.clientID)
	}
	if method == "client_secret_post" {
		form.Set("client_secret", p.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("OIDC token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if method == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(p.clientID), url.QueryEscape(p.clientSecret))
	}
	var response struct {
		IDToken string `json:"id_token"`
	}
	if err := p.readJSON(req, &response); err != nil {
		return "", fmt.Errorf("OIDC code exchange: %w", err)
	}
	return verifyOIDCToken(response.IDToken, p.issuer, p.clientID, login.nonce, oidcRSAAlgorithms, func(t *jwt.Token) (any, error) {
		alg := t.Method.Alg()
		if !slices.Contains(metadata.Algorithms, alg) {
			return nil, errors.New("OIDC ID token uses an unadvertised algorithm")
		}
		var kid string
		if id, present := t.Header["kid"]; present {
			var ok bool
			if kid, ok = id.(string); !ok || kid == "" {
				return nil, errors.New("invalid OIDC key ID")
			}
		}
		return p.verificationKeys(ctx, metadata.JWKS, kid, alg)
	})
}

func verifyOIDCToken(token, issuer, clientID, nonce string, algorithms []string, keys jwt.Keyfunc) (string, error) {
	var claims struct {
		jwt.RegisteredClaims
		Nonce           string          `json:"nonce"`
		AuthorizedParty json.RawMessage `json:"azp"`
	}
	_, err := jwt.ParseWithClaims(token, &claims, keys, jwt.WithValidMethods(algorithms),
		jwt.WithExpirationRequired(), jwt.WithIssuer(issuer), jwt.WithAudience(clientID), jwt.WithIssuedAt(),
		jwt.WithStrictDecoding())
	if err != nil {
		return "", fmt.Errorf("OIDC ID token validation: %w", err)
	}
	if claims.Subject == "" || claims.IssuedAt == nil || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return "", errors.New("invalid OIDC subject, issued-at time or nonce")
	}
	for _, audience := range claims.Audience {
		if audience != clientID {
			return "", errors.New("untrusted OIDC audience")
		}
	}
	if len(claims.Audience) > 1 || claims.AuthorizedParty != nil {
		var party string
		if json.Unmarshal(claims.AuthorizedParty, &party) != nil || party != clientID {
			return "", errors.New("invalid OIDC authorized party")
		}
	}
	return claims.Subject, nil
}

type oidcJWK struct {
	ID         string   `json:"kid"`
	Type       string   `json:"kty"`
	Use        string   `json:"use"`
	Operations []string `json:"key_ops"`
	Algorithm  string   `json:"alg"`
	Modulus    string   `json:"n"`
	Exponent   string   `json:"e"`
}

type oidcPublicKey struct {
	id, algorithm string
	key           *rsa.PublicKey
}

func parseOIDCPublicKeys(keys []oidcJWK) []oidcPublicKey {
	var publicKeys []oidcPublicKey
	for _, key := range keys {
		if key.Type != "RSA" || (key.Use != "" && key.Use != "sig") ||
			(len(key.Operations) > 0 && !slices.Contains(key.Operations, "verify")) {
			continue
		}
		nBytes, nErr := base64.RawURLEncoding.DecodeString(key.Modulus)
		eBytes, eErr := base64.RawURLEncoding.DecodeString(key.Exponent)
		n, e := new(big.Int).SetBytes(nBytes), new(big.Int).SetBytes(eBytes)
		if nErr != nil || eErr != nil || n.BitLen() < 2048 || e.BitLen() > 31 || e.Int64() < 3 || e.Bit(0) == 0 {
			continue
		}
		publicKeys = append(publicKeys, oidcPublicKey{key.ID, key.Algorithm, &rsa.PublicKey{N: n, E: int(e.Int64())}})
	}
	return publicKeys
}

func oidcMatchingKeys(keys []oidcPublicKey, kid, alg string) jwt.VerificationKeySet {
	var set jwt.VerificationKeySet
	for _, key := range keys {
		if (kid == "" || key.id == kid) && (key.algorithm == "" || key.algorithm == alg) {
			set.Keys = append(set.Keys, key.key)
		}
	}
	return set
}

func (p *standardOIDC) verificationKeys(ctx context.Context, endpoint, kid, alg string) (jwt.VerificationKeySet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.keysExpires) {
		if set := oidcMatchingKeys(p.keys, kid, alg); len(set.Keys) > 0 {
			return set, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return jwt.VerificationKeySet{}, fmt.Errorf("OIDC JWKS request: %w", err)
	}
	var jwks struct {
		Keys []oidcJWK `json:"keys"`
	}
	if err := p.readJSON(req, &jwks); err != nil {
		return jwt.VerificationKeySet{}, fmt.Errorf("OIDC JWKS refresh: %w", err)
	}
	p.keys, p.keysExpires = parseOIDCPublicKeys(jwks.Keys), time.Now().Add(oidcJWKSMaxAge)
	set := oidcMatchingKeys(p.keys, kid, alg)
	if len(set.Keys) == 0 {
		return set, errors.New("no matching OIDC verification key")
	}
	return set, nil
}

func (p *standardOIDC) readJSON(req *http.Request, result any) error {
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcMaxBody+1))
	if err != nil {
		return err
	}
	if len(body) > oidcMaxBody {
		return errors.New("OIDC response is too large")
	}
	return json.Unmarshal(body, result)
}
