package enterprise

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestProjectQuotaLimits(t *testing.T) {
	svc := testService(t)
	store, err := db.Open("postgres://artex@127.0.0.1:55432/artex_enterprise_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	projectID := fmt.Sprintf("proj-quota-%d", stamp)
	if _, err := store.ExecContext(ctx, `
INSERT INTO ent_projects (id, code, name, status) VALUES ($1,$2,'quota','active')`, projectID, fmt.Sprintf("Q%x", stamp)); err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTaskWithOptions("quota task", "goal", db.TaskCreateOptions{Name: fmt.Sprintf("quota-%d", stamp)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTaskWithOptions("quota task 2", "goal", db.TaskCreateOptions{Name: fmt.Sprintf("quota-b-%d", stamp)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.ExecContext(context.Background(), `DELETE FROM llm_usage WHERE task_id=$1 OR task_id=$2`, strconv.FormatInt(first.ID, 10), strconv.FormatInt(second.ID, 10))
		_ = store.DeleteTask(first.ID)
		_ = store.DeleteTask(second.ID)
		_, _ = store.ExecContext(context.Background(), `DELETE FROM ent_projects WHERE id=$1`, projectID)
	})
	for _, id := range []int64{first.ID, second.ID} {
		if _, err := store.ExecContext(ctx, `INSERT INTO ent_project_tasks(project_id, task_id) VALUES ($1,$2)`, projectID, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO ent_members(project_id, user_id, role) VALUES ($1,$2,'owner')`, projectID, UserOwnerA); err != nil {
		t.Fatal(err)
	}
	admin := svc.as(t, UserAdmin)
	owner := svc.as(t, UserOwnerA)
	if _, err := svc.UpdateProjectQuota(ctx, owner, projectID, ProjectQuotaInput{MaxConcurrentTasks: 9}); err == nil {
		t.Fatal("member updated quota")
	} else {
		var fail *Fail
		if !errors.As(err, &fail) || fail.Status != 403 || fail.Code != "forbidden" {
			t.Fatalf("member update err = %v", err)
		}
	}
	var audits int
	if err := store.QueryRowContext(ctx, `SELECT count(*) FROM ent_audit WHERE project_id=$1 AND action='project_quota_updated'`, projectID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatalf("audits after rejected update = %d", audits)
	}
	if _, err := svc.GetProjectQuota(ctx, svc.as(t, UserOwnerB), projectID); err == nil {
		t.Fatal("non-member read quota")
	} else {
		var fail *Fail
		if !errors.As(err, &fail) || fail.Status != 404 {
			t.Fatalf("non-member get err = %v", err)
		}
	}
	view, err := svc.UpdateProjectQuota(ctx, admin, projectID, ProjectQuotaInput{MaxConcurrentTasks: 1})
	if err != nil || view.MaxConcurrentTasks != 1 || view.DailyTokenLimit != 0 {
		t.Fatalf("update = %+v err=%v", view, err)
	}
	if err := store.QueryRowContext(ctx, `SELECT count(*) FROM ent_audit WHERE project_id=$1 AND action='project_quota_updated'`, projectID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("audits = %d", audits)
	}
	if err := svc.CheckProjectConcurrency(ctx, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `UPDATE tasks SET status='running', paused=false, queued=false WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	err = svc.CheckProjectConcurrency(ctx, projectID)
	var fail *Fail
	if !errors.As(err, &fail) || fail.Status != 429 || fail.Code != "project_concurrency_limit" {
		t.Fatalf("running over limit err = %v", err)
	}
	if _, err := svc.UpdateProjectQuota(ctx, admin, projectID, ProjectQuotaInput{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckProjectConcurrency(ctx, projectID); err != nil {
		t.Fatalf("zero concurrency ceiling: %v", err)
	}
	if _, err := svc.UpdateProjectQuota(ctx, admin, projectID, ProjectQuotaInput{MaxConcurrentTasks: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `UPDATE tasks SET status='paused', paused=true, queued=false WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckProjectConcurrency(ctx, projectID); err != nil {
		t.Fatalf("paused task counted: %v", err)
	}
	if _, err := store.EnqueueExecutionJob(ctx, second.ID, projectID, "default"); err != nil {
		t.Fatal(err)
	}
	err = svc.CheckProjectConcurrency(ctx, projectID)
	if !errors.As(err, &fail) || fail.Status != 429 || fail.Code != "project_concurrency_limit" {
		t.Fatalf("queued job over limit err = %v", err)
	}
	if err := store.CancelExecutionJobByTask(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckProjectConcurrency(ctx, projectID); err != nil {
		t.Fatalf("after cancel: %v", err)
	}

	now := time.Now()
	if _, err := store.ExecContext(ctx, `
INSERT INTO llm_usage(task_id, input_tokens, output_tokens, cache_read, cache_write, ts)
VALUES ($1, 60, 40, 0, 0, $2)`, strconv.FormatInt(first.ID, 10), now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateProjectQuota(ctx, admin, projectID, ProjectQuotaInput{DailyTokenLimit: 100, MonthlyTokenLimit: 1000}); err != nil {
		t.Fatal(err)
	}
	err = svc.CheckProjectTokenBudget(ctx, projectID, now)
	if !errors.As(err, &fail) || fail.Status != 429 || fail.Code != "project_token_budget_exceeded" || fail.Fields["period"] != "daily" {
		t.Fatalf("daily budget err = %v", err)
	}
	if _, err := svc.UpdateProjectQuota(ctx, admin, projectID, ProjectQuotaInput{MonthlyTokenLimit: 100}); err != nil {
		t.Fatal(err)
	}
	err = svc.CheckProjectTokenBudget(ctx, projectID, now)
	if !errors.As(err, &fail) || fail.Status != 429 || fail.Code != "project_token_budget_exceeded" || fail.Fields["period"] != "monthly" {
		t.Fatalf("monthly budget err = %v", err)
	}
	if _, err := svc.UpdateProjectQuota(ctx, admin, projectID, ProjectQuotaInput{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckProjectTokenBudget(ctx, projectID, now); err != nil {
		t.Fatalf("zero token ceiling: %v", err)
	}
	view, err = svc.GetProjectQuota(ctx, owner, projectID)
	if err != nil || view.DailyTokens < 100 || view.MonthlyTokens < 100 {
		t.Fatalf("usage view = %+v err=%v", view, err)
	}
}
