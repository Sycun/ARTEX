package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	jwtTTL         = 7 * 24 * time.Hour
	keyChars       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// 下限与 setup 页的前端校验一致——校验只放在前端等于没放，直接打 API 就能
	// 绕过。上限是 bcrypt 的硬限制：超过 72 字节 GenerateFromPassword 会返回
	// ErrPasswordTooLong，提前挡掉好过让用户收到一句含义不明的「密码加密失败」。
	minPasswordRunes = 8
	maxPasswordBytes = 72
)

// errDataSourceUnavailable 是密码相关读操作失败时统一的回复。这些 handler 绝不能
// 把"读不到"当成"没有设置"：authInit 曾因此在数据库报错时放行，让未认证请求覆盖
// 掉已有的管理员密码。
const errDataSourceUnavailable = "数据源暂时不可用，请稍后重试"

// validatePassword 返回空串表示通过，否则返回可直接展示给用户的中文原因。
func validatePassword(pw string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return fmt.Sprintf("密码长度至少 %d 位", minPasswordRunes)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("密码长度不能超过 %d 字节", maxPasswordBytes)
	}
	return ""
}

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. Legacy installs kept it at dataDir/jwt.key; if present there and not yet
// at the new location, it is migrated (key preserved, so sessions stay valid) and
// the old file removed so it disappears from the workspace. On first run a random
// key is generated and persisted.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	// one-time migration out of the old in-workspace location.
	if legacy := filepath.Join(dataDir, jwtKeyFilename); legacy != path {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if data, rerr := os.ReadFile(legacy); rerr == nil {
				if werr := os.WriteFile(path, data, 0o600); werr == nil {
					_ = os.Remove(legacy)
					log.Printf("[auth] JWT key 已从 %s 迁移到 %s（移出可浏览工作区）", legacy, path)
				}
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] 新 JWT key 已写入 %s", path)
	return buf, nil
}

// signJWT issues a 7-day HS256 token for user ARTEX.
func signJWT(key []byte) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(key)
}

// approver is who a human decision is recorded for. Enterprise sessions use the
// actor id; the standalone session token's subject is ARTEX.
func (s *Server) approver(r *http.Request) string {
	if actor, ok := currentActor(r); ok && actor.ID != "" {
		return actor.ID
	}
	if verifyJWT(s.extractToken(r), s.jwtKey) {
		return "ARTEX"
	}
	return ""
}

// verifyJWT returns true when tokenStr is a valid, non-expired HS256 token.
func verifyJWT(tokenStr string, key []byte) bool {
	claims := jwt.MapClaims{}
	t, err := jwt.ParseWithClaims(tokenStr, claims, func(*jwt.Token) (any, error) {
		return key, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithSubject("ARTEX"), jwt.WithExpirationRequired())
	return err == nil && t.Valid && claims["ent"] == nil
}

// extractToken also accepts legacy SSE query tokens in standalone mode.
func (s *Server) extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("artex_token"); err == nil && c.Value != "" {
		return c.Value
	}
	if s.entMode {
		return ""
	}
	return r.URL.Query().Get("token")
}

func (s *Server) enterpriseSameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return len(r.Header.Values("Referer")) == 1 && sameHTTPOrigin(r.Referer(), s.origin(), false)
	}
	return len(values) == 1 && sameHTTPOrigin(values[0], s.origin(), true)
}

func sameHTTPOrigin(source, target string, originHeader bool) bool {
	got, err := url.Parse(source)
	if err != nil || got.Hostname() == "" || got.User != nil || strings.Contains(source, "#") {
		return false
	}
	if originHeader && (got.Path != "" || got.RawQuery != "" || got.ForceQuery) {
		return false
	}
	want, err := url.Parse(target)
	if err != nil || got.Scheme != want.Scheme || !strings.EqualFold(got.Hostname(), want.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return port(got) == port(want)
}

// requireAuth wraps h with JWT validation.
// /api/auth/*, /api/health and the Basic Auth gate check are exempt.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.entMode && !s.enterpriseSameOrigin(r) {
			writeProductError(w, http.StatusForbidden, "invalid_origin", "请求来源与企业访问地址不一致", nil)
			return
		}
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" || p == "/api/ready" || p == "/api/basic-auth/check" || s.enterprisePublic(p) {
			h.ServeHTTP(w, r)
			return
		}
		tok := s.extractToken(r)
		if tok == "" {
			writeProductError(w, 401, "unauthorized", "未授权", nil)
			return
		}
		if s.entMode {
			if sid, ok := s.enterpriseSessionID(tok); ok {
				actor, err := s.ent.TouchSession(r.Context(), sid)
				if err != nil {
					writeEnt(w, err)
					return
				}
				if actor.Gate() != "ready" && !enterpriseLimited(r) {
					if actor.Gate() == "disabled" {
						writeProductError(w, 403, "account_disabled", "账号已停用。请由企业目录恢复账号。页面上的联系人是平台管理员林衡。", nil)
					} else {
						writeProductError(w, 403, "pending_bind", "目录身份尚未核验，还不能进入项目。", nil)
					}
					return
				}
				if r.Method == http.MethodGet && p == "/api/workers" {
					if !actor.Platform() {
						writeProductError(w, http.StatusForbidden, "forbidden", "仅平台管理员可访问", nil)
						return
					}
					h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
					return
				}
				if !enterpriseAllows(p) && !s.allowEnterpriseWorkbench(w, r, actor) {
					return
				}
				h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
				return
			}
			if verifyJWT(tok, s.jwtKey) {
				writeProductError(w, 401, "enterprise_sso", "请使用企业账号登录。共享密码登录已关闭。", nil)
				return
			}
			writeProductError(w, 401, "token_expired", "token 无效或已过期", nil)
			return
		}
		if !verifyJWT(tok, s.jwtKey) {
			writeProductError(w, 401, "token_expired", "token 无效或已过期", nil)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// GET /api/auth/status — reports whether the admin password has been initialised.
// 读失败必须回 503 而不是 initialized:false：前端在 initialized:false 时会把用户
// 送到 /setup 去设置密码（login/page.tsx），把数据库故障包装成 200 等于把用户往
// 覆盖已有密码的路上推。
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeProductError(w, 503, "data_unavailable", errDataSourceUnavailable, nil)
		return
	}
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// setupTokenOut is where the setup token is printed: the stderr of the process.
// It is a variable so that tests can capture it. The log sink keeps every line
// for /api/logs, so the token itself must not go through the standard logger.
var setupTokenOut io.Writer = os.Stderr

// enterpriseSkipsSetupToken is true when the enterprise SSO chain is the
// primary login. A setup token must not block that mode, including a worker
// that was started with ARTEX_ENTERPRISE=1 before bootEnterprise set entMode.
func (s *Server) enterpriseSkipsSetupToken() bool {
	if s != nil && s.entMode {
		return true
	}
	return os.Getenv("ARTEX_ENTERPRISE") == "1"
}

// initSetupToken generates the one-time setup token when no admin password is
// set and prints it on the server console. Without it, anyone who can reach
// the port before the operator could claim the instance through POST /api/auth/init.
func (s *Server) initSetupToken() {
	if s == nil || s.m == nil || s.m.pg == nil || s.enterpriseSkipsSetupToken() {
		return
	}
	var hash string
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		var ok bool
		hash, ok, err = s.m.pg.GetSetting(authPassKey)
		if err == nil {
			if ok && hash != "" {
				return
			}
			break
		}
		log.Printf("[auth] password lookup attempt %d/3: %v", attempt, err)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
	}
	if err != nil {
		log.Printf("[auth] password lookup failed; not issuing a setup token: %v", err)
		return
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("[auth] generate setup token: %v", err)
	}
	token := hex.EncodeToString(buf)
	s.setupMu.Lock()
	s.setupToken = token
	s.setupMu.Unlock()
	fmt.Fprintln(setupTokenOut, time.Now().Format("2006/01/02 15:04:05"), "[auth] no admin password is set. Setup token for the /setup page:", token)
	log.Printf("[auth] no admin password is set. The setup token for the /setup page was printed on the server console (stderr); it is not kept in this log")
}

// checkSetupToken compares got with the active setup token in constant time.
// It fails when no token is active.
func (s *Server) checkSetupToken(got string) bool {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.setupToken)) == 1
}

func (s *Server) loginWait(r *http.Request) time.Duration {
	if s == nil || s.loginLimit == nil {
		return 0
	}
	return s.loginLimit.allow(clientIP(r, s.trustedProxies))
}

func (s *Server) passwordChangeWait(r *http.Request) time.Duration {
	if s == nil || s.pwdLimit == nil {
		return 0
	}
	return s.pwdLimit.allow(clientIP(r, s.trustedProxies) + "|" + s.extractToken(r))
}

func writeRateLimited(w http.ResponseWriter, wait time.Duration) {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeProductError(w, 429, "rate_limited", fmt.Sprintf("尝试过于频繁，请在 %d 秒后重试", secs), map[string]any{"seconds": secs})
}

// POST /api/auth/init — sets the password for the first time; rejected if already set.
// Standalone mode requires the setup token printed at startup. Enterprise mode
// (ARTEX_ENTERPRISE=1) does not.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeProductError(w, 503, "data_unavailable", errDataSourceUnavailable, nil)
		return
	}
	if existing != "" {
		writeProductError(w, 403, "password_already_set", "密码已设置", nil)
		return
	}
	var req struct {
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeProductError(w, 400, "password_required", "密码不能为空", nil)
		return
	}
	// 403, not 401: the frontend treats any 401 as an expired session and redirects to /login.
	if !s.enterpriseSkipsSetupToken() && !s.checkSetupToken(req.SetupToken) {
		writeProductError(w, 403, "invalid_setup_token", "安装口令不正确。服务启动时会在控制台打印一次性安装口令。", nil)
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		code, params := passwordValidationCode(req.Password)
		writeProductError(w, 400, code, msg, params)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeProductError(w, 500, "password_hash_failed", "密码加密失败", nil)
		return
	}
	// 用 INSERT ... ON CONFLICT DO NOTHING 而不是 upsert：上面那次 GetSetting 只是
	// 快速失败路径，真正"仅首次可设"的保证落在主键约束上。bcrypt 要跑几十毫秒，
	// 这期间别的请求完全可能先把密码设好，而读检查本身也可能因故障而失效。
	inserted, err := pg.InsertSettingIfAbsent(authPassKey, string(hash))
	if err != nil {
		writeProductError(w, 500, "save_failed", "保存失败: "+err.Error(), nil)
		return
	}
	if !inserted {
		writeProductError(w, 403, "password_already_set", "密码已设置", nil)
		return
	}
	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeProductError(w, 500, "token_failed", "token 生成失败", nil)
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}

// POST /api/auth/change-password — changes the admin password. Requires a valid
// token (this route is under /api/auth/* which requireAuth exempts, so the token
// is validated here) AND the current password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if !verifyJWT(s.extractToken(r), s.jwtKey) {
		writeProductError(w, 401, "unauthorized", "未授权", nil)
		return
	}
	if wait := s.passwordChangeWait(r); wait > 0 {
		writeRateLimited(w, wait)
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请求格式错误", nil)
		return
	}
	if req.NewPassword == "" {
		writeProductError(w, 400, "new_password_required", "新密码不能为空", nil)
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		code, params := passwordValidationCode(req.NewPassword)
		writeProductError(w, 400, code, msg, params)
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeProductError(w, 503, "data_unavailable", errDataSourceUnavailable, nil)
		return
	}
	if !ok || hash == "" {
		writeProductError(w, 403, "password_not_initialized", "密码未初始化，请先设置密码", nil)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeProductError(w, 401, "current_password_incorrect", "当前密码错误", nil)
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeProductError(w, 500, "password_hash_failed", "密码加密失败", nil)
		return
	}
	if err := pg.SetSetting(authPassKey, string(newHash)); err != nil {
		writeProductError(w, 500, "save_failed", "保存失败: "+err.Error(), nil)
		return
	}
	if s.pwdLimit != nil {
		s.pwdLimit.reset(clientIP(r, s.trustedProxies) + "|" + s.extractToken(r))
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if s.entMode {
		writeProductError(w, 403, "enterprise_sso", "企业账号登录已开启，密码登录已关闭。请使用企业账号登录。", nil)
		return
	}
	if wait := s.loginWait(r); wait > 0 {
		writeRateLimited(w, wait)
		return
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请求格式错误", nil)
		return
	}
	if req.Username != "ARTEX" {
		writeProductError(w, 401, "invalid_credentials", "用户名或密码错误", nil)
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeProductError(w, 503, "data_unavailable", errDataSourceUnavailable, nil)
		return
	}
	if !ok || hash == "" {
		writeProductError(w, 403, "password_not_initialized", "密码未初始化，请先设置密码", nil)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeProductError(w, 401, "invalid_credentials", "用户名或密码错误", nil)
		return
	}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeProductError(w, 500, "token_failed", "token 生成失败", nil)
		return
	}
	if s.loginLimit != nil {
		s.loginLimit.reset(clientIP(r, s.trustedProxies))
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}

func passwordValidationCode(password string) (string, map[string]any) {
	if utf8.RuneCountInString(password) < minPasswordRunes {
		return "password_too_short", map[string]any{"min": minPasswordRunes}
	}
	return "password_too_long", map[string]any{"max": maxPasswordBytes}
}
