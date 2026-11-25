package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/enterprise"
	"github.com/golang-jwt/jwt/v5"
)

type actorKey struct{}

func (s *Server) bootEnterprise() {
	if os.Getenv("ARTEX_ENTERPRISE") != "1" || s.m == nil || s.m.pg == nil {
		return
	}
	svc, err := enterprise.New(s.m.pg.DB, s.dataDir)
	if err != nil {
		log.Fatalf("[enterprise] %v", err)
	}
	s.ent = svc
	s.entMode = true
	s.publicOrigin = strings.TrimRight(os.Getenv("ARTEX_PUBLIC_ORIGIN"), "/")
	if !sameHTTPOrigin(s.origin(), s.origin(), true) || (!strings.HasPrefix(s.origin(), "http://") && !strings.HasPrefix(s.origin(), "https://")) {
		log.Fatal("[enterprise] ARTEX_PUBLIC_ORIGIN 必须是 HTTP(S) origin")
	}
	if os.Getenv("ARTEX_DEMO_IDP") == "1" {
		if err := svc.Seed(s.ctx); err != nil {
			log.Fatalf("[enterprise] seed: %v", err)
		}
		pack, err := svc.EnsureReviewPack(s.ctx)
		if err != nil {
			log.Fatalf("[enterprise] review pack: %v", err)
		}
		log.Printf("[enterprise] review pack ready project=%s task=%d reused=%v", pack.ProjectID, pack.TaskID, pack.Reused)
		if err := svc.EnsureWorkRoster(s.ctx); err != nil {
			log.Fatalf("[enterprise] work roster: %v", err)
		}
		s.oidc = newDemoIDP(s.origin(), s.jwtKey)
	} else if issuer := os.Getenv("ARTEX_OIDC_ISSUER"); issuer != "" {
		p, err := newStandardOIDC(issuer, os.Getenv("ARTEX_OIDC_CLIENT_ID"), os.Getenv("ARTEX_OIDC_CLIENT_SECRET"))
		if err != nil {
			log.Fatalf("[enterprise] %v", err)
		}
		s.oidc = p
	}
	log.Printf("[enterprise] 企业模式已开启，身份源=%T，数据目录=%s", s.oidc, s.dataDir)
}

func (s *Server) origin() string {
	if s.publicOrigin != "" {
		return strings.TrimRight(s.publicOrigin, "/")
	}
	return "http://127.0.0.1:5173"
}

func (s *Server) enterprisePublic(path string) bool {
	switch path {
	case "/api/enterprise/status", "/api/enterprise/login", "/api/enterprise/callback":
		return true
	}
	_, demo := s.oidc.(*demoIDP)
	return demo && strings.HasPrefix(path, "/api/demo-idp/")
}

func enterpriseLimited(r *http.Request) bool {
	p := r.URL.Path
	return (p == "/api/enterprise/me" && r.Method == http.MethodGet) ||
		(p == "/api/enterprise/logout" && r.Method == http.MethodPost)
}

func enterpriseAllows(path string) bool {
	if strings.HasPrefix(path, "/api/enterprise/") || strings.HasPrefix(path, "/api/demo-idp/") {
		return true
	}
	switch path {
	case "/api/health", "/api/version", "/api/auth/status", "/api/basic-auth/check":
		return true
	default:
		return false
	}
}

func (s *Server) enterpriseSessionID(token string) (string, bool) {
	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) {
		return s.jwtKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid {
		return "", false
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || claims["ent"] != true {
		return "", false
	}
	issuer, _ := claims["iss"].(string)
	_, demo := s.oidc.(*demoIDP)
	if s.oidc == nil || (issuer != s.oidc.Issuer() && !(demo && issuer == "")) {
		return "", false
	}
	sid, _ := claims["sid"].(string)
	return sid, sid != ""
}

func (s *Server) signEnterprise(user enterprise.User, sessionID string, exp time.Time) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  user.ID,
		"iss":  s.oidc.Issuer(),
		"sid":  sessionID,
		"ent":  true,
		"name": user.Name,
		"exp":  exp.Unix(),
		"iat":  time.Now().Unix(),
	}).SignedString(s.jwtKey)
}

func currentActor(r *http.Request) (enterprise.Actor, bool) {
	actor, ok := r.Context().Value(actorKey{}).(enterprise.Actor)
	return actor, ok
}

func writeEnt(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var fail *enterprise.Fail
	if errors.As(err, &fail) {
		payload := map[string]any{"error": fail.Message, "code": fail.Code, "message": fail.Message}
		for key, value := range fail.Fields {
			payload[key] = value
		}
		status := fail.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, payload)
		return true
	}
	log.Printf("[enterprise] %v", err)
	writeProductError(w, 500, "server_error", "这一步没有完成，请稍后重试", nil)
	return true
}

func (s *Server) wantEnt(w http.ResponseWriter, r *http.Request) (enterprise.Actor, bool) {
	if !s.entMode || s.ent == nil {
		writeProductError(w, 404, "not_found", "找不到", nil)
		return enterprise.Actor{}, false
	}
	actor, ok := currentActor(r)
	if !ok {
		writeProductError(w, 401, "unauthorized", "请先使用企业账号登录", nil)
		return enterprise.Actor{}, false
	}
	return actor, true
}

func (s *Server) registerEnterprise(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/enterprise/status", s.enterpriseStatus)
	mux.HandleFunc("GET /api/enterprise/login", s.enterpriseLogin)
	mux.HandleFunc("GET /api/enterprise/callback", s.enterpriseCallback)
	mux.HandleFunc("POST /api/enterprise/logout", s.enterpriseLogout)
	mux.HandleFunc("GET /api/enterprise/me", s.enterpriseMe)
	mux.HandleFunc("GET /api/enterprise/entry", s.enterpriseEntry)
	mux.HandleFunc("GET /api/enterprise/workbench", s.enterpriseWorkbenchView)
	mux.HandleFunc("POST /api/enterprise/session/project", s.enterpriseSelectProject)
	mux.HandleFunc("GET /api/enterprise/conclusions", s.enterpriseConclusions)
	mux.HandleFunc("GET /api/enterprise/conclusions/{id}", s.enterpriseConclusion)
	mux.HandleFunc("GET /api/enterprise/runtime", s.enterpriseRuntime)
	mux.HandleFunc("POST /api/enterprise/join", s.enterpriseJoin)
	mux.HandleFunc("GET /api/enterprise/join/{id}", s.enterpriseJoinGet)
	mux.HandleFunc("POST /api/enterprise/join/{id}/approve", s.enterpriseJoinApprove)
	mux.HandleFunc("POST /api/enterprise/join/{id}/reject", s.enterpriseJoinReject)
	mux.HandleFunc("GET /api/enterprise/todos", s.enterpriseTodos)
	mux.HandleFunc("GET /api/enterprise/directory", s.enterpriseDirectory)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/members", s.enterpriseMembers)
	mux.HandleFunc("POST /api/enterprise/projects/{id}/members", s.enterpriseAddMember)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/summary", s.enterpriseSummary)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/home", s.enterpriseHome)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/tasks", s.enterpriseTasks)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/tasks/{taskID}", s.enterpriseTask)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/reports", s.enterpriseReports)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/assets", s.enterpriseAssets)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/audit", s.enterpriseAudit)
	mux.HandleFunc("GET /api/enterprise/projects/{id}/audit/export", s.enterpriseAuditExport)
	mux.HandleFunc("POST /api/enterprise/sample/import", s.enterpriseImport)
	mux.HandleFunc("GET /api/enterprise/sample", s.enterpriseSample)
	mux.HandleFunc("GET /api/enterprise/findings", s.enterpriseFindings)
	mux.HandleFunc("GET /api/enterprise/findings/{id}", s.enterpriseFinding)
	mux.HandleFunc("POST /api/enterprise/findings/{id}/report", s.enterpriseOpenReport)
	mux.HandleFunc("GET /api/enterprise/reports/{id}", s.enterpriseReport)
	mux.HandleFunc("PUT /api/enterprise/reports/{id}/draft", s.enterpriseSaveDraft)
	mux.HandleFunc("POST /api/enterprise/reports/{id}/versions/{vid}/submit", s.enterpriseSubmit)
	mux.HandleFunc("POST /api/enterprise/reports/{id}/versions/{vid}/return", s.enterpriseReturn)
	mux.HandleFunc("POST /api/enterprise/reports/{id}/versions/{vid}/withdraw", s.enterpriseWithdraw)
	mux.HandleFunc("POST /api/enterprise/reports/{id}/versions/{vid}/issue", s.enterpriseIssue)
	mux.HandleFunc("POST /api/enterprise/reports/{id}/supplement", s.enterpriseSupplement)
	mux.HandleFunc("GET /api/enterprise/reports/{id}/versions/{vid}/download", s.enterpriseDownload)
	mux.HandleFunc("POST /api/enterprise/reports/{id}/original-requests", s.enterpriseRequestOriginal)
	mux.HandleFunc("GET /api/enterprise/original-requests/{id}", s.enterpriseEvidence)
	mux.HandleFunc("POST /api/enterprise/original-requests/{id}/approve", s.enterpriseApproveOriginal)
	mux.HandleFunc("POST /api/enterprise/original-requests/{id}/reject", s.enterpriseRejectOriginal)
	mux.HandleFunc("POST /api/enterprise/original-requests/{id}/revoke", s.enterpriseRevokeOriginal)
	mux.HandleFunc("GET /api/enterprise/originals/{id}", s.enterpriseOriginalBytes)
	mux.HandleFunc("POST /api/enterprise/delegations", s.enterpriseDelegate)
	mux.HandleFunc("POST /api/enterprise/audit-delegations", s.enterpriseAuditDelegate)
	if _, demo := s.oidc.(*demoIDP); demo {
		mux.HandleFunc("GET /api/demo-idp/authorize", s.demoAuthorize)
		mux.HandleFunc("POST /api/demo-idp/login", s.demoLogin)
		mux.HandleFunc("POST /api/demo-idp/token", s.demoToken)
		mux.HandleFunc("GET /api/demo-idp/.well-known/openid-configuration", s.demoDiscovery)
	}
}

func (s *Server) enterpriseStatus(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"enterprise":     s.entMode,
		"idp_available":  s.oidc != nil,
		"password_login": !s.entMode,
	}
	switch s.oidc.(type) {
	case *demoIDP:
		body["idp"] = "demo"
		body["idp_note"] = "当前接的是本机演示身份源，不是企业的真实身份源。真实 IdP 还没有配置。"
	case *standardOIDC:
		body["idp"] = "oidc"
	case nil:
		if s.entMode {
			body["idp"] = "unconfigured"
			body["idp_note"] = "企业模式已打开，但没有接上身份源。真实 IdP 还没有配置。"
		}
	}
	writeJSON(w, 200, body)
}

func (s *Server) enterpriseEntry(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	entry, err := s.ent.Entry(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, entry)
}

func (s *Server) enterpriseWorkbenchView(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	view, err := s.ent.Workbench(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, view)
}

func (s *Server) enterpriseSelectProject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if err := decode(r, &req); err != nil || req.ProjectID == "" {
		writeProductError(w, 400, "project_required", "请选择项目", nil)
		return
	}
	brief, err := s.ent.SelectProject(r.Context(), actor, req.ProjectID)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, brief)
}

func (s *Server) enterpriseConclusions(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.Conclusions(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	if items == nil {
		items = []enterprise.Conclusion{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseConclusion(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.Conclusion(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseMe(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	me, err := s.ent.Me(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, me)
}

func (s *Server) enterpriseLogout(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	if writeEnt(w, s.ent.Logout(r.Context(), actor.SessionID)) {
		return
	}
	s.setEnterpriseCookies(w, r, "", -1)
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已退出。可以重新使用企业账号登录。"})
}

func (s *Server) enterpriseRuntime(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.wantEnt(w, r); !ok {
		return
	}
	running, queued, err := s.ent.Runtime(r.Context())
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"running": running, "queued": queued})
}

func (s *Server) enterpriseJoin(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		Code    string `json:"code"`
		Purpose string `json:"purpose"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请填写项目编号和用途", nil)
		return
	}
	rc, err := s.ent.SubmitJoin(r.Context(), actor, req.Code, req.Purpose)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, rc)
}

func (s *Server) enterpriseJoinGet(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	rc, err := s.ent.GetJoin(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, rc)
}

func (s *Server) enterpriseJoinApprove(w http.ResponseWriter, r *http.Request) {
	s.decideJoin(w, r, "approve")
}

func (s *Server) enterpriseJoinReject(w http.ResponseWriter, r *http.Request) {
	s.decideJoin(w, r, "reject")
}

func (s *Server) decideJoin(w http.ResponseWriter, r *http.Request, decision string) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	_ = decode(r, &req)
	if req.Role == "" {
		req.Role = "member"
	}
	rc, err := s.ent.DecideJoin(r.Context(), actor, r.PathValue("id"), decision, req.Role)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, rc)
}

func (s *Server) enterpriseTodos(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.Todos(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseDirectory(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.SearchDirectory(r.Context(), actor, r.URL.Query().Get("project"), r.URL.Query().Get("q"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseMembers(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.ListMembers(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseAddMember(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req enterprise.MemberInput
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请选择已核验的人和基础角色", nil)
		return
	}
	if writeEnt(w, s.ent.AddMember(r.Context(), actor, r.PathValue("id"), req)) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已加入项目"})
}

func (s *Server) enterpriseHome(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	home, err := s.ent.ProjectHome(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, home)
}

func (s *Server) enterpriseTasks(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	list, err := s.ent.ListTasks(r.Context(), actor, r.PathValue("id"), enterprise.TaskQuery{
		Text:     r.URL.Query().Get("q"),
		Status:   r.URL.Query().Get("status"),
		Page:     atoiDefault(r.URL.Query().Get("page"), 1),
		PageSize: atoiDefault(r.URL.Query().Get("page_size"), 20),
	})
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) enterpriseTask(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	taskID := int64(atoiDefault(r.PathValue("taskID"), 0))
	if taskID == 0 {
		writeProductError(w, 404, "not_found", "找不到这个任务", nil)
		return
	}
	item, err := s.ent.Task(r.Context(), actor, r.PathValue("id"), taskID)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) enterpriseReports(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.ListReports(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	if items == nil {
		items = []enterprise.ReportListItem{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseSummary(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	sum, err := s.ent.Summary(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, sum)
}

func (s *Server) enterpriseAssets(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.Assets(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) enterpriseAudit(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.Audit(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	issued := 0
	history := ""
	for _, item := range items {
		if item.Action == "report_issued" {
			issued++
		}
		if item.Action == "review_sample_loaded" {
			history = "历史审核样例的签发版在任务和报告里，不改变「尚无签发版」。这句只表示本轮复核流程还没有新的签发。"
		}
	}
	notice := ""
	if issued == 0 {
		notice = "尚无签发版"
	}
	writeJSON(w, 200, map[string]any{"items": items, "no_issued_notice": notice, "history_note": history})
}

func (s *Server) enterpriseAuditExport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	items, err := s.ent.AuditExport(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "note": "导出只含结构化原因，不含拒绝理由、退回理由或加入用途原文"})
}

func (s *Server) enterpriseImport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	result, err := s.ent.ImportSample(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) enterpriseSample(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	result, err := s.ent.SampleStatus(r.Context(), actor)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) enterpriseFindings(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	page, err := s.ent.SearchFindings(r.Context(), actor, r.URL.Query().Get("project"), enterprise.FindingQuery{
		Text:     r.URL.Query().Get("q"),
		Page:     atoiDefault(r.URL.Query().Get("page"), 1),
		PageSize: atoiDefault(r.URL.Query().Get("page_size"), 20),
	})
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, page)
}

func (s *Server) enterpriseFinding(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	item, err := s.ent.Finding(r.Context(), actor, int64(atoiDefault(r.PathValue("id"), 0)))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) enterpriseOpenReport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	ref, err := s.ent.OpenReport(r.Context(), actor, int64(atoiDefault(r.PathValue("id"), 0)))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, ref)
}

func (s *Server) enterpriseReport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	view, err := s.ent.Report(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, view)
}

func (s *Server) enterpriseSaveDraft(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请填写报告正文", nil)
		return
	}
	view, err := s.ent.SaveDraft(r.Context(), actor, r.PathValue("id"), req.Body)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, view)
}

type hashReq struct {
	BodyHash string `json:"body_hash"`
	Reason   string `json:"reason"`
}

func (s *Server) enterpriseSubmit(w http.ResponseWriter, r *http.Request) {
	s.versionAction(w, r, func(ctx context.Context, actor enterprise.Actor, id string, req hashReq) (enterprise.ActionResult, error) {
		return s.ent.Submit(ctx, actor, id, req.BodyHash)
	})
}

func (s *Server) enterpriseReturn(w http.ResponseWriter, r *http.Request) {
	s.versionAction(w, r, func(ctx context.Context, actor enterprise.Actor, id string, req hashReq) (enterprise.ActionResult, error) {
		return s.ent.Return(ctx, actor, id, req.BodyHash, req.Reason)
	})
}

func (s *Server) enterpriseWithdraw(w http.ResponseWriter, r *http.Request) {
	s.versionAction(w, r, func(ctx context.Context, actor enterprise.Actor, id string, req hashReq) (enterprise.ActionResult, error) {
		return s.ent.Withdraw(ctx, actor, id, req.BodyHash)
	})
}

func (s *Server) enterpriseIssue(w http.ResponseWriter, r *http.Request) {
	s.versionAction(w, r, func(ctx context.Context, actor enterprise.Actor, id string, req hashReq) (enterprise.ActionResult, error) {
		return s.ent.Issue(ctx, actor, id, req.BodyHash)
	})
}

func (s *Server) versionAction(w http.ResponseWriter, r *http.Request, fn func(context.Context, enterprise.Actor, string, hashReq) (enterprise.ActionResult, error)) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req hashReq
	_ = decode(r, &req)
	out, err := fn(r.Context(), actor, r.PathValue("vid"), req)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseSupplement(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	out, err := s.ent.Supplement(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseDownload(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	file, err := s.ent.Download(r.Context(), actor, r.PathValue("vid"))
	if writeEnt(w, err) {
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"report.md\"; filename*=UTF-8''"+url.PathEscape(file.Filename))
	w.Header().Set("X-Content-SHA256", file.Hash)
	w.WriteHeader(200)
	_, _ = w.Write(file.Bytes)
}

func (s *Server) enterpriseRequestOriginal(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		Purpose string `json:"purpose"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请填写用途", nil)
		return
	}
	out, err := s.ent.RequestOriginal(r.Context(), actor, r.PathValue("id"), req.Purpose)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	out, err := s.ent.GetEvidence(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseApproveOriginal(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req enterprise.OriginalApproval
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请填写授权动作及有效期", nil)
		return
	}
	out, err := s.ent.ApproveOriginal(r.Context(), actor, r.PathValue("id"), req)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseRejectOriginal(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "reason_required", "拒绝理由必填", nil)
		return
	}
	out, err := s.ent.RejectOriginal(r.Context(), actor, r.PathValue("id"), req.Reason)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseRevokeOriginal(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	out, err := s.ent.RevokeOriginal(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) enterpriseOriginalBytes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	body, err := s.ent.ReadOriginal(r.Context(), actor, r.PathValue("id"))
	if writeEnt(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.WriteHeader(200)
	if _, err := w.Write(body); err != nil {
		log.Printf("[enterprise] write original %s: %v", r.PathValue("id"), err)
	}
}

func (s *Server) enterpriseDelegate(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req enterprise.DelegationInput
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请填写项目、版本、正文哈希、动作、受委人和到期时间", nil)
		return
	}
	id, err := s.ent.GrantDelegation(r.Context(), actor, req)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "message": "已授予限时签发委派。受委人不会因此成为项目成员。"})
}

func (s *Server) enterpriseAuditDelegate(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return
	}
	var req struct {
		ProjectID string    `json:"project_id"`
		GranteeID string    `json:"grantee_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := decode(r, &req); err != nil {
		writeProductError(w, 400, "invalid_request", "请填写委派范围和到期时间", nil)
		return
	}
	id, err := s.ent.GrantAuditDelegation(r.Context(), actor, req.ProjectID, req.GranteeID, req.ExpiresAt)
	if writeEnt(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "message": "已授予审计摘要委派。受委人不能下载报告，也不能看委派范围外的事件。"})
}

type demoAccount struct {
	Sub  string
	Name string
	Role string
}

func demoAccounts() []demoAccount {
	return []demoAccount{
		{"demo-admin", "林衡", "平台管理员"},
		{"demo-biz", "顾清", "领导，查看已签发报告和结论"},
		{enterprise.ReaderLogin, enterprise.ReaderName, "项目 A 只读审核"},
		{"demo-owner-a", "沈昭", "项目 A 负责人"},
		{"demo-analyst", "周宁", "员工，进入原有任务工作台"},
		{"demo-reviewer", "陈溯", "复核人，处理送审版本"},
		{"demo-auditor", "许衡", "审计员，开始时没有项目"},
		{"demo-owner-b", "江澄", "项目 B 负责人"},
		{"demo-member-b", "苏晚", "项目 B 员工"},
		{"demo-delegate", "裴独立", "可接受限时委派，不是项目成员"},
		{"demo-unmatched", "未匹配示例", "目录里没有这个人"},
		{"demo-disabled", "停用示例", "目录中已停用"},
	}
}

const (
	loginStateTTL = 10 * time.Minute
	callbackPath  = "/api/enterprise/callback"
)

type loginState struct {
	state, nonce, verifier, redirectURI, prefer string
}

func (s *Server) enterpriseLogin(w http.ResponseWriter, r *http.Request) {
	if !s.entMode {
		writeProductError(w, 404, "not_found", "找不到", nil)
		return
	}
	if s.oidc == nil {
		http.Redirect(w, r, s.origin()+"/login?idp=down", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	login := loginState{
		state: randomURL(18), nonce: randomURL(18), verifier: randomURL(32),
		redirectURI: s.origin() + callbackPath,
	}
	if r.URL.Query().Get("prefer") == enterprise.ReaderLogin {
		login.prefer = enterprise.ReaderLogin
	}
	location, err := s.oidc.AuthorizationURL(r.Context(), login)
	if err != nil {
		log.Printf("[enterprise] OIDC authorization: %v", err)
		http.Redirect(w, r, s.origin()+"/login?idp=down", http.StatusFound)
		return
	}
	_, err = s.ent.DB.ExecContext(r.Context(), `
		WITH expired AS (DELETE FROM ent_login_states WHERE expires_at <= now())
		INSERT INTO ent_login_states (state, nonce, code_verifier, redirect_uri, prefer, expires_at)
		VALUES ($1,$2,$3,$4,$5,now() + $6::integer * interval '1 second')`,
		login.state, login.nonce, login.verifier, login.redirectURI, login.prefer, int(loginStateTTL.Seconds()))
	if writeEnt(w, err) {
		return
	}
	http.SetCookie(w, s.loginStateCookie(r, login.state, int(loginStateTTL.Seconds())))
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *Server) secureEnterpriseCookie(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.origin(), "https://")
}

func (s *Server) setEnterpriseCookies(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	cookie := http.Cookie{Path: "/", MaxAge: maxAge, SameSite: http.SameSiteLaxMode, Secure: s.secureEnterpriseCookie(r)}
	cookie.Name, cookie.Value, cookie.HttpOnly = "artex_token", token, true
	http.SetCookie(w, &cookie)
	cookie.Name, cookie.Value, cookie.HttpOnly = "artex_enterprise", "1", false
	if maxAge < 0 {
		cookie.Value = ""
	}
	http.SetCookie(w, &cookie)
}

func (s *Server) loginStateCookie(r *http.Request, state string, maxAge int) *http.Cookie {
	mac := hmac.New(sha256.New, s.jwtKey)
	mac.Write([]byte("artex-oidc-state:" + s.oidc.Issuer() + ":" + state))
	cookie := &http.Cookie{
		Name: "artex_login_" + state, Value: base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
		Path: callbackPath, MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.secureEnterpriseCookie(r),
	}
	if cookie.Secure {
		cookie.Name = "__Host-" + cookie.Name
		cookie.Path = "/"
	}
	if maxAge < 0 {
		cookie.Value = ""
	}
	return cookie
}

func (s *Server) enterpriseCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.oidc == nil || !s.entMode {
		http.Redirect(w, r, s.origin()+"/login?idp=down", http.StatusFound)
		return
	}
	query := r.URL.Query()
	login := loginState{state: query.Get("state")}
	expected := s.loginStateCookie(r, login.state, 0)
	cookie, err := r.Cookie(expected.Name)
	if login.state == "" || err != nil || !hmac.Equal([]byte(cookie.Value), []byte(expected.Value)) {
		http.Error(w, "登录状态无效，请回到登录页重新使用企业账号登录。", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, s.loginStateCookie(r, login.state, -1))
	var active bool
	err = s.ent.DB.QueryRowContext(r.Context(), `
		DELETE FROM ent_login_states WHERE state=$1
		RETURNING nonce, code_verifier, redirect_uri, prefer, expires_at > now()`, login.state).
		Scan(&login.nonce, &login.verifier, &login.redirectURI, &login.prefer, &active)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "登录状态已过期或已使用，请回到登录页重新使用企业账号登录。", http.StatusBadRequest)
		return
	}
	if writeEnt(w, err) {
		return
	}
	if !active || query.Get("code") == "" || query.Get("error") != "" {
		http.Error(w, "企业登录没有完成，请回到登录页重试。", http.StatusBadRequest)
		return
	}

	sub, err := s.oidc.Exchange(r.Context(), query.Get("code"), login)
	if err != nil {
		log.Printf("[enterprise] OIDC exchange: %v", err)
		http.Error(w, "企业登录没有完成，请回到登录页重试。", http.StatusBadRequest)
		return
	}
	if _, demo := s.oidc.(*demoIDP); !demo {
		for _, account := range demoAccounts() {
			if sub == account.Sub {
				http.Error(w, "该企业身份不可用于登录。", http.StatusForbidden)
				return
			}
		}
	}
	user, err := s.ent.BindLogin(r.Context(), s.oidc.Issuer(), sub)
	if writeEnt(w, err) {
		return
	}
	sessionID, exp, err := s.ent.StartSession(r.Context(), user.ID)
	if writeEnt(w, err) {
		return
	}
	signed, err := s.signEnterprise(user, sessionID, exp)
	if writeEnt(w, err) {
		return
	}
	s.setEnterpriseCookies(w, r, signed, int(time.Until(exp).Seconds()))
	http.Redirect(w, r, s.origin()+"/enterprise/landing", http.StatusFound)
}

type demoIDP struct {
	issuer string
	key    []byte
	mu     sync.Mutex
	codes  map[string]demoCode
}

type demoCode struct {
	sub, nonce, challenge, redirectURI string
	exp                                time.Time
}

func newDemoIDP(origin string, key []byte) *demoIDP {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("artex-demo-idp"))
	return &demoIDP{issuer: origin + "/api/demo-idp", key: mac.Sum(nil), codes: map[string]demoCode{}}
}

func (p *demoIDP) Issuer() string { return p.issuer }

func (p *demoIDP) AuthorizationURL(_ context.Context, login loginState) (string, error) {
	return authorizationURL(p.issuer+"/authorize", "artex", login)
}

func (p *demoIDP) Exchange(_ context.Context, code string, login loginState) (string, error) {
	token, err := p.exchangeCode(code, login.verifier, login.redirectURI)
	if err != nil {
		return "", err
	}
	return verifyOIDCToken(token, p.issuer, "artex", login.nonce, []string{"HS256"}, func(*jwt.Token) (any, error) {
		return p.key, nil
	})
}

func (s *Server) readDemoLogin(ctx context.Context, state string) (loginState, error) {
	login := loginState{state: state}
	err := s.ent.DB.QueryRowContext(ctx, `
		SELECT nonce, code_verifier, redirect_uri, prefer FROM ent_login_states
		WHERE state=$1 AND expires_at > now()`, state).
		Scan(&login.nonce, &login.verifier, &login.redirectURI, &login.prefer)
	return login, err
}

func (s *Server) demoAuthorize(w http.ResponseWriter, r *http.Request) {
	login, err := s.readDemoLogin(r.Context(), r.URL.Query().Get("state"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "登录请求已失效，请回到 ARTEX 重新开始。", http.StatusBadRequest)
		return
	}
	if writeEnt(w, err) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := demoLoginPage.Execute(w, map[string]any{
		"State":    login.state,
		"Accounts": demoAccounts(),
		"Password": enterprise.DemoLoginPassword,
		"Prefer":   login.prefer,
	}); err != nil {
		log.Printf("[enterprise] render demo login: %v", err)
	}
}

func (s *Server) demoLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "登录表单无法读取", http.StatusBadRequest)
		return
	}
	sub := r.PostForm.Get("account")
	if !hmac.Equal([]byte(r.PostForm.Get("password")), []byte(enterprise.DemoLoginPassword)) {
		http.Error(w, "演示口令不正确。请使用登录页上写明的虚构口令。", http.StatusUnauthorized)
		return
	}
	found := false
	for _, account := range demoAccounts() {
		if account.Sub == sub {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "没有这个演示账号。", http.StatusBadRequest)
		return
	}
	login, err := s.readDemoLogin(r.Context(), r.PostForm.Get("state"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "登录请求已失效，请回到 ARTEX 重新开始。", http.StatusBadRequest)
		return
	}
	if writeEnt(w, err) {
		return
	}
	demo := s.oidc.(*demoIDP)
	code := randomURL(24)
	now := time.Now()
	demo.mu.Lock()
	for id, grant := range demo.codes {
		if !now.Before(grant.exp) {
			delete(demo.codes, id)
		}
	}
	demo.codes[code] = demoCode{
		sub: sub, nonce: login.nonce, challenge: pkceChallenge(login.verifier),
		redirectURI: login.redirectURI, exp: now.Add(5 * time.Minute),
	}
	demo.mu.Unlock()
	q := url.Values{"code": {code}, "state": {login.state}}
	http.Redirect(w, r, login.redirectURI+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) demoToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeProductError(w, 400, "invalid_request", "令牌请求无法读取", nil)
		return
	}
	demo := s.oidc.(*demoIDP)
	token, err := demo.exchangeCode(r.PostForm.Get("code"), r.PostForm.Get("code_verifier"), r.PostForm.Get("redirect_uri"))
	if err != nil {
		writeProductError(w, 400, "invalid_grant", "授权码无效或已使用", nil)
		return
	}
	writeJSON(w, 200, map[string]any{"id_token": token, "token_type": "Bearer"})
}

func (p *demoIDP) exchangeCode(code, verifier, redirectURI string) (string, error) {
	p.mu.Lock()
	grant, ok := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	if !ok || !time.Now().Before(grant.exp) || grant.challenge != pkceChallenge(verifier) || grant.redirectURI != redirectURI {
		return "", errors.New("invalid demo authorization code")
	}
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": p.issuer, "sub": grant.sub, "aud": "artex", "nonce": grant.nonce,
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
	}).SignedString(p.key)
}

func (s *Server) demoDiscovery(w http.ResponseWriter, r *http.Request) {
	issuer := s.oidc.Issuer()
	writeJSON(w, 200, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"HS256"},
	})
}

func randomURL(n int) string {
	buf := make([]byte, n)
	rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

var demoLoginPage = template.Must(template.New("idp").Parse(`<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>北宸示例 · 企业登录</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f6f7f9;margin:0}
main{max-width:32rem;margin:4rem auto;background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:1.5rem}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:#4b5563;line-height:1.5}
label{display:block;margin:.75rem 0 .25rem;font-size:.875rem}
select,input,button{width:100%;box-sizing:border-box;padding:.6rem;border-radius:8px;border:1px solid #d1d5db;font-size:1rem}
button{margin-top:1rem;background:#111827;color:#fff;border:0;cursor:pointer}
</style></head>
<body><main>
<h1>使用企业账号登录</h1>
<p>这是本地演示身份源，不是生产目录。选择下面的虚构账号，口令使用页面上的演示口令。</p>
<form method="post" action="/api/demo-idp/login">
<input type="hidden" name="state" value="{{.State}}">
<label for="account">演示账号</label>
<select id="account" name="account">
{{range .Accounts}}<option value="{{.Sub}}" {{if eq .Sub $.Prefer}}selected{{end}}>{{.Name}} · {{.Role}}</option>{{end}}
</select>
<label for="password">演示口令</label>
<input id="password" name="password" type="password" value="{{.Password}}" autocomplete="off">
<button type="submit">继续并返回 ARTEX</button>
</form>
<p>口令是虚构的 <code>{{.Password}}</code>。截图里的另一串口令不能用来登录。</p>
</main></body></html>`))
