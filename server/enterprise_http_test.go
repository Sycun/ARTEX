package server

import (
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
		ent_init_grants, ent_users, findings RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	svc, err := enterprise.New(store.DB, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		m:       &Manager{pg: store},
		jwtKey:  []byte("0123456789abcdef0123456789abcdef"),
		entMode: true,
		ent:     svc,
		demo:    newDemoIDP(),
		dataDir: t.TempDir(),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	s.publicOrigin = srv.URL

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
	resp, err = client.PostForm(srv.URL+"/api/demo-idp/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusOK {
		t.Fatalf("callback finished at %d %s", resp.StatusCode, resp.Request.URL)
	}
	return client
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
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
