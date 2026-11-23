package server

import (
	"net/http"
	"net/url"
	"testing"
)

func TestWorkbenchOpenDoesNotAuthorizeRun(t *testing.T) {
	kind, id, write := classifyWorkbench(http.MethodGet, "/api/tasks/15", nil)
	if kind != "task" || id != "15" || write {
		t.Fatalf("open task kind=%s id=%s write=%v", kind, id, write)
	}
	kind, _, write = classifyWorkbench(http.MethodGet, "/api/tasks", nil)
	if kind != "list" || write {
		t.Fatalf("task list kind=%s write=%v", kind, write)
	}
	for _, path := range []string{"/api/tasks/15/control", "/api/active", "/api/workspace", "/api/tasks/15/intelligence"} {
		kind, _, _ = classifyWorkbench(http.MethodPost, path, nil)
		if kind != "" {
			t.Fatalf("%s was opened as %s", path, kind)
		}
	}
	kind, id, write = classifyWorkbench(http.MethodGet, "/api/exploration/findings", url.Values{"task": {"15"}})
	if kind != "task-query" || id != "15" || write {
		t.Fatalf("findings kind=%s id=%s write=%v", kind, id, write)
	}
	kind, id, write = classifyWorkbench(http.MethodPatch, "/api/exploration/findings/4", nil)
	if kind != "finding" || id != "4" || !write {
		t.Fatalf("finding patch kind=%s id=%s write=%v", kind, id, write)
	}
	kind, _, _ = classifyWorkbench(http.MethodGet, "/api/exploration/activity/stream", url.Values{"task": {"15"}})
	if kind != "task-query" {
		t.Fatalf("activity stream kind=%s", kind)
	}
}
