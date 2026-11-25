package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/enterprise"
)

func TestEnterpriseHTTPFlow(t *testing.T) {
	s := newEnterpriseHTTPTestServer(t)
	srv := httptest.NewUnstartedServer(nil)
	s.publicOrigin = "http://" + srv.Listener.Addr().String()
	s.oidc = newDemoIDP(s.publicOrigin, s.jwtKey)
	srv.Config.Handler = s.Handler()
	srv.Start()
	t.Cleanup(srv.Close)

	testEnterpriseDemoFlow(t, srv)
}

func TestEnterpriseOriginalHTTP(t *testing.T) {
	s := newEnterpriseHTTPTestServer(t)
	srv := httptest.NewUnstartedServer(nil)
	s.publicOrigin = "http://" + srv.Listener.Addr().String()
	s.oidc = newDemoIDP(s.publicOrigin, s.jwtKey)
	srv.Config.Handler = s.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	base := srv.URL + "/api/enterprise"
	admin := loginDemo(t, srv, "demo-admin")
	var sample enterprise.SampleResult
	if err := json.Unmarshal([]byte(apiJSON(t, admin, http.MethodPost, base+"/sample/import", "")), &sample); err != nil {
		t.Fatal(err)
	}
	if err := s.ent.EnsureWorkRoster(t.Context()); err != nil {
		t.Fatal(err)
	}
	applicant := loginDemo(t, srv, "demo-analyst")
	reviewer := loginDemo(t, srv, "demo-reviewer")
	var report enterprise.ReportRef
	reportPath := fmt.Sprintf("%s/findings/%d/report", base, sample.FindingID)
	if err := json.Unmarshal([]byte(apiJSON(t, applicant, http.MethodPost, reportPath, "")), &report); err != nil {
		t.Fatal(err)
	}
	var request enterprise.EvidenceRequest
	requestPath := base + "/reports/" + report.ID + "/original-requests"
	if err := json.Unmarshal([]byte(apiJSON(t, applicant, http.MethodPost, requestPath, `{"purpose":"核对画面"}`)), &request); err != nil {
		t.Fatal(err)
	}
	requestPath = base + "/original-requests/" + request.ID
	originalPath := base + "/originals/orig-demo-ns-001"
	const approval = `{"actions":["view_original","view_credential"]}`
	if status := apiStatus(t, applicant, http.MethodPost, requestPath+"/approve", approval); status != http.StatusForbidden {
		t.Fatalf("self approval status = %d", status)
	}
	if status := apiStatus(t, applicant, http.MethodGet, originalPath, ""); status != http.StatusNotFound {
		t.Fatalf("original before approval status = %d", status)
	}
	apiJSON(t, reviewer, http.MethodPost, requestPath+"/approve", approval)
	if err := json.Unmarshal([]byte(apiJSON(t, applicant, http.MethodGet, requestPath, "")), &request); err != nil || !request.Previewable {
		t.Fatalf("request after approval = %+v, err = %v", request, err)
	}
	resp, err := applicant.Get(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("\x89PNG\r\n\x1a\n")) || !bytes.Contains(body, []byte(enterprise.DemoSecret)) {
		t.Fatalf("original response status = %d, bytes = %d, err = %v", resp.StatusCode, len(body), err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("original cache or disposition headers: %v", resp.Header)
	}
	apiJSON(t, reviewer, http.MethodPost, requestPath+"/revoke", "")
	if status := apiStatus(t, applicant, http.MethodGet, originalPath, ""); status != http.StatusNotFound {
		t.Fatalf("original after revocation status = %d", status)
	}
}

func newEnterpriseHTTPTestServer(t *testing.T) *Server {
	t.Helper()
	dsn := "postgres://artex@127.0.0.1:55432/artex_enterprise_http?sslmode=disable"
	store, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.Exec(`DELETE FROM tasks WHERE remark = 'DEMO-RV-001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec(`DELETE FROM explorations e WHERE e.goal LIKE '%DEMO-RV-001%' AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.exploration_id = e.id)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec(`DELETE FROM assets WHERE url = 'https://login.portal.northstar.example/'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec(`TRUNCATE ent_review_records, ent_issues, ent_versions, ent_report_sources, ent_finding_report,
		ent_reports, ent_evidence_requests, ent_original_grants, ent_originals, ent_delegations, ent_audit_delegations,
		ent_todos, ent_join_requests, ent_duties, ent_members, ent_asset_obs, ent_project_tasks, ent_project_findings, ent_unclaimed,
		ent_cold_archives, ent_sample_loads, ent_audit, ent_notifications, ent_sessions, ent_identities, ent_projects,
		ent_init_grants, ent_users, ent_login_states, findings RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	svc, err := enterprise.New(store.DB, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &Server{
		m:       &Manager{pg: store},
		jwtKey:  []byte("0123456789abcdef0123456789abcdef"),
		entMode: true,
		ent:     svc,
		dataDir: t.TempDir(),
	}
}

func testEnterpriseDemoFlow(t *testing.T, srv *httptest.Server) {
	t.Helper()
	admin := loginDemo(t, srv, "demo-admin")
	res := apiJSON(t, admin, http.MethodPost, srv.URL+"/api/enterprise/sample/import", "")
	if !strings.Contains(res, `"findings":1`) || !strings.Contains(res, `"issued":0`) || !strings.Contains(res, `"cases":0`) {
		t.Fatalf("import: %s", res)
	}
	password := apiStatus(t, srv.Client(), http.MethodPost, srv.URL+"/api/auth/login", `{"username":"ARTEX","password":"anything"}`)
	if password != http.StatusForbidden {
		t.Fatalf("password login status=%d", password)
	}
	analyst := loginDemo(t, srv, "demo-analyst")
	if status := apiStatus(t, analyst, http.MethodGet, srv.URL+"/api/findings", ""); status != http.StatusNotFound {
		t.Fatalf("legacy findings status=%d", status)
	}
	if status := apiStatus(t, analyst, http.MethodGet, srv.URL+"/api/tasks", ""); status != http.StatusNotFound {
		t.Fatalf("unscoped task list status=%d", status)
	}
	if status := apiStatus(t, analyst, http.MethodPost, srv.URL+"/api/tasks/1/control", `{"action":"resume"}`); status != http.StatusNotFound {
		t.Fatalf("run control status=%d", status)
	}
	if status := apiStatus(t, analyst, http.MethodGet, srv.URL+"/api/workspace?path=/", ""); status != http.StatusNotFound {
		t.Fatalf("legacy workspace status=%d", status)
	}
	joined := apiJSON(t, analyst, http.MethodPost, srv.URL+"/api/enterprise/join", `{"code":"NS-A","purpose":"参加门户整改演示"}`)
	if !strings.Contains(joined, "JR-") || strings.Contains(joined, "门户整改") && strings.Contains(joined, `"project_name"`) {
		t.Fatalf("join response leaked project data: %s", joined)
	}
	me := apiJSON(t, analyst, http.MethodGet, srv.URL+"/api/enterprise/me", "")
	if !strings.Contains(me, `"gate":"ready"`) || strings.Contains(me, "沈昭") {
		t.Fatalf("analyst me: %s", me)
	}
	session := analyst.Jar.Cookies(mustURL(t, srv.URL))[0]
	if session.Name != "artex_token" {
		t.Fatalf("expected session cookie, got %s", session.Name)
	}
	const streamPath = "/api/exploration/activity/stream"
	if status := apiStatus(t, srv.Client(), http.MethodGet, srv.URL+streamPath+"?token="+url.QueryEscape(session.Value), ""); status != http.StatusUnauthorized {
		t.Fatalf("query token stream status=%d", status)
	}
	logoutURL := srv.URL + "/api/enterprise/logout"
	for _, origin := range []string{"", "http://other.example"} {
		req, err := http.NewRequest(http.MethodPost, logoutURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.AddCookie(session)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("logout from %q status=%d", origin, resp.StatusCode)
		}
	}
	req, err := http.NewRequest(http.MethodPost, logoutURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", srv.URL+"/enterprise")
	resp, err := analyst.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin Referer logout status=%d", resp.StatusCode)
	}
	for _, name := range []string{"artex_token", "artex_enterprise"} {
		if cookie := responseCookie(t, resp, name); cookie.MaxAge != -1 || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatalf("logout cookie %s: %#v", name, cookie)
		}
	}
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/enterprise/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d", resp.StatusCode)
	}
}

func loginDemo(t *testing.T, srv *httptest.Server, account string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Get(srv.URL + "/api/enterprise/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), account) {
		t.Fatalf("idp page status=%d", resp.StatusCode)
	}
	state := resp.Request.URL.Query().Get("state")
	form := url.Values{"state": {state}, "account": {account}, "password": {enterprise.DemoLoginPassword}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/demo-idp/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Path == "/enterprise/landing" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Request.URL.Path != callbackPath {
		t.Fatalf("callback finished at %d %s", resp.StatusCode, resp.Request.URL)
	}
	if cookie := responseCookie(t, resp, "artex_token"); !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 || cookie.Path != "/" {
		t.Fatalf("HTTP session cookie flags: HttpOnly=%v Secure=%v SameSite=%v MaxAge=%d Path=%s", cookie.HttpOnly, cookie.Secure, cookie.SameSite, cookie.MaxAge, cookie.Path)
	}
	return client
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func responseCookie(t *testing.T, resp *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response is missing cookie %s", name)
	return nil
}

func apiJSON(t *testing.T, client *http.Client, method, url, body string) string {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", req.URL.Scheme+"://"+req.URL.Host)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s -> %d %s", method, url, resp.StatusCode, data)
	}
	return string(data)
}

func apiStatus(t *testing.T, client *http.Client, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", req.URL.Scheme+"://"+req.URL.Host)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
