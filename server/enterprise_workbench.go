package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/enterprise"
)

// allowEnterpriseWorkbench 只放行原工作台实际会调用、并且能按项目成员关系收窄的接口。
// 浏览、切标签和刷新都走读接口，不会启动任务。
func (s *Server) allowEnterpriseWorkbench(w http.ResponseWriter, r *http.Request, actor enterprise.Actor) bool {
	kind, resource, write := classifyWorkbench(r.Method, r.URL.Path, r.URL.Query())
	if kind == "" {
		writeProductError(w, http.StatusNotFound, "not_found", "找不到", nil)
		return false
	}
	switch kind {
	case "list", "meta":
		if !s.memberOfAny(r.Context(), actor) {
			writeProductError(w, http.StatusNotFound, "not_found", "找不到", nil)
			return false
		}
		return true
	case "task":
		taskID, err := strconv.ParseInt(resource, 10, 64)
		if err != nil || taskID <= 0 {
			writeProductError(w, http.StatusNotFound, "not_found", "找不到", nil)
			return false
		}
		if strings.HasPrefix(workbenchSubpath(r.URL.Path), "files") && !write {
			if err := s.ent.AllowsTaskFiles(r.Context(), actor, taskID); err != nil {
				writeEnt(w, err)
				return false
			}
			return true
		}
		if _, err := s.ent.TaskAccess(r.Context(), actor, taskID, write); err != nil {
			writeEnt(w, err)
			return false
		}
		return true
	case "task-query":
		taskID, err := strconv.ParseInt(resource, 10, 64)
		if err != nil || taskID <= 0 {
			writeProductError(w, http.StatusNotFound, "not_found", "找不到", nil)
			return false
		}
		if _, err := s.ent.TaskAccess(r.Context(), actor, taskID, false); err != nil {
			writeEnt(w, err)
			return false
		}
		return true
	case "finding":
		findingID, err := strconv.ParseInt(resource, 10, 64)
		if err != nil || findingID <= 0 {
			writeProductError(w, http.StatusNotFound, "not_found", "找不到", nil)
			return false
		}
		if _, err := s.ent.FindingAccess(r.Context(), actor, findingID, write); err != nil {
			writeEnt(w, err)
			return false
		}
		return true
	default:
		writeProductError(w, http.StatusNotFound, "not_found", "找不到", nil)
		return false
	}
}

func (s *Server) memberOfAny(ctx context.Context, actor enterprise.Actor) bool {
	var n int
	err := s.ent.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM ent_members m
		JOIN ent_projects p ON p.id=m.project_id
		WHERE m.user_id=$1 AND p.status='active'`, actor.ID).Scan(&n)
	return err == nil && n > 0
}

func classifyWorkbench(method, path string, q url.Values) (kind, resource string, write bool) {
	switch {
	case method == http.MethodGet && path == "/api/tasks":
		return "list", "", false
	case method == http.MethodGet && (path == "/api/task-categories" || path == "/api/llm/profiles"):
		return "meta", "", false
	case method == http.MethodGet && path == "/api/stats":
		return "task-query", q.Get("task"), false
	case method == http.MethodGet && path == "/api/report":
		return "task-query", q.Get("task"), false
	case method == http.MethodGet && (path == "/api/assets" || path == "/api/assets/counts"):
		return "task-query", q.Get("task_id"), false
	case method == http.MethodGet && strings.HasPrefix(path, "/api/intercept/task/"):
		return "task", strings.TrimPrefix(path, "/api/intercept/task/"), false
	}
	if rest, ok := strings.CutPrefix(path, "/api/tasks/"); ok && isTaskIDPath(rest) {
		id, sub, _ := strings.Cut(rest, "/")
		switch method {
		case http.MethodGet:
			if sub == "" || taskReadAllowed(sub) {
				return "task", id, false
			}
		case http.MethodPatch:
			if sub == "" {
				return "task", id, true
			}
		case http.MethodPost, http.MethodDelete:
			if sub == "credentials" || strings.HasPrefix(sub, "credentials/") {
				return "task", id, true
			}
		}
		return "", "", false
	}
	switch {
	case method == http.MethodGet && path == "/api/exploration/findings":
		return "task-query", q.Get("task"), false
	case method == http.MethodPatch && strings.HasPrefix(path, "/api/exploration/findings/"):
		id := strings.TrimPrefix(path, "/api/exploration/findings/")
		if isDigits(id) {
			return "finding", id, true
		}
	case method == http.MethodGet && strings.HasPrefix(path, "/api/exploration/findings/"):
		id := strings.TrimPrefix(path, "/api/exploration/findings/")
		if isDigits(id) {
			return "finding", id, false
		}
	case method == http.MethodGet && (path == "/api/exploration/finding-cases" || path == "/api/exploration/finding-case-suggestions"):
		return "task-query", q.Get("task_id"), false
	case method == http.MethodGet && path == "/api/exploration/finding-case-review":
		return "list", "", false
	case method == http.MethodGet && taskQueryPath(path):
		return "task-query", q.Get("task"), false
	}
	return "", "", false
}

func taskQueryPath(path string) bool {
	switch path {
	case "/api/exploration/intents",
		"/api/exploration/graph",
		"/api/exploration/nodes",
		"/api/exploration/activity",
		"/api/exploration/activity/history",
		"/api/exploration/activity/stream",
		"/api/exploration/main-sessions",
		"/api/exploration/tokens":
		return true
	}
	return strings.HasPrefix(path, "/api/exploration/activity/") &&
		path != "/api/exploration/activity/history" &&
		path != "/api/exploration/activity/stream"
}

func taskReadAllowed(sub string) bool {
	switch sub {
	case "scope", "goals", "constraints", "coverage", "coverage-graph",
		"intercept-rules", "chat/status", "intent-assets", "asset-refs", "llm/resolution",
		"topology-mapping/status", "asset-mapping/status",
		"credentials", "outcome", "lessons",
		"files", "files/all", "files/read", "files/download", "files/preview":
		return true
	default:
		return false
	}
}

func workbenchSubpath(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/tasks/")
	if !ok {
		return ""
	}
	_, sub, _ := strings.Cut(rest, "/")
	return sub
}

func isTaskIDPath(rest string) bool {
	id, _, _ := strings.Cut(rest, "/")
	return isDigits(id)
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
