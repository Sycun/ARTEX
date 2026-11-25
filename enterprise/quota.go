package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProjectQuota is the configured ceiling and the current consumption.
// A limit of 0 means that dimension is not enforced.
type ProjectQuota struct {
	ProjectID          string `json:"project_id"`
	MaxConcurrentTasks int    `json:"max_concurrent_tasks"`
	DailyTokenLimit    int64  `json:"daily_token_limit"`
	MonthlyTokenLimit  int64  `json:"monthly_token_limit"`
	ActiveTasks        int    `json:"active_tasks"`
	DailyTokens        int64  `json:"daily_tokens"`
	MonthlyTokens      int64  `json:"monthly_tokens"`
}

// ProjectQuotaInput replaces the three ceilings. Zero keeps the dimension open.
type ProjectQuotaInput struct {
	MaxConcurrentTasks int   `json:"max_concurrent_tasks"`
	DailyTokenLimit    int64 `json:"daily_token_limit"`
	MonthlyTokenLimit  int64 `json:"monthly_token_limit"`
}

// GetProjectQuota returns the ceilings and current usage. Members can read
// their project. A platform admin can read any project.
func (s *Service) GetProjectQuota(ctx context.Context, a Actor, projectID string) (ProjectQuota, error) {
	projectID = strings.TrimSpace(projectID)
	if err := s.allowQuotaRead(ctx, a, projectID); err != nil {
		return ProjectQuota{}, err
	}
	return s.loadProjectQuota(ctx, projectID, time.Now())
}

// UpdateProjectQuota replaces the ceilings. Only a platform admin can write,
// and the change is appended to ent_audit.
func (s *Service) UpdateProjectQuota(ctx context.Context, a Actor, projectID string, in ProjectQuotaInput) (ProjectQuota, error) {
	projectID = strings.TrimSpace(projectID)
	if !a.Platform() {
		return ProjectQuota{}, fail(403, "forbidden", "仅平台管理员可以修改项目配额", nil)
	}
	if projectID == "" {
		return ProjectQuota{}, fail(404, "not_found", "找不到", nil)
	}
	if in.MaxConcurrentTasks < 0 || in.DailyTokenLimit < 0 || in.MonthlyTokenLimit < 0 {
		return ProjectQuota{}, fail(400, "invalid_quota", "配额不能为负数", nil)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE ent_projects
SET max_concurrent_tasks=$2, daily_token_limit=$3, monthly_token_limit=$4
WHERE id=$1`, projectID, in.MaxConcurrentTasks, in.DailyTokenLimit, in.MonthlyTokenLimit)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fail(404, "not_found", "找不到", nil)
		}
		summary := fmt.Sprintf("并发上限 %d，日 token %d，月 token %d", in.MaxConcurrentTasks, in.DailyTokenLimit, in.MonthlyTokenLimit)
		return s.audit(ctx, tx, projectID, a.ID, "project_quota_updated", "", summary, "project", projectID, "", nil)
	})
	if err != nil {
		return ProjectQuota{}, err
	}
	return s.loadProjectQuota(ctx, projectID, time.Now())
}

// CheckProjectConcurrency rejects admission when the project is already at
// max_concurrent_tasks. A zero ceiling is not enforced.
func (s *Service) CheckProjectConcurrency(ctx context.Context, projectID string) error {
	return s.CheckProjectConcurrencyExcept(ctx, projectID, 0)
}

// CheckProjectConcurrencyExcept is CheckProjectConcurrency ignoring one task
// that is already active and is the subject of this admission.
func (s *Service) CheckProjectConcurrencyExcept(ctx context.Context, projectID string, exceptTaskID int64) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var max int
	err = tx.QueryRowContext(ctx, `SELECT max_concurrent_tasks FROM ent_projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&max)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return err
	}
	if max <= 0 {
		return nil
	}
	active, err := projectActiveTasks(ctx, tx, projectID, exceptTaskID)
	if err != nil {
		return err
	}
	if active >= max {
		return fail(429, "project_concurrency_limit", "项目并发任务已达上限", map[string]any{
			"limit":  max,
			"active": active,
		})
	}
	return nil
}

// CheckProjectTokenBudget rejects an LLM call once today's or this month's
// token spend has reached a configured ceiling. A zero ceiling is not enforced.
func (s *Service) CheckProjectTokenBudget(ctx context.Context, projectID string, now time.Time) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var daily, monthly int64
	err := s.DB.QueryRowContext(ctx, `
SELECT daily_token_limit, monthly_token_limit FROM ent_projects WHERE id=$1`, projectID).Scan(&daily, &monthly)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return err
	}
	if daily <= 0 && monthly <= 0 {
		return nil
	}
	if daily > 0 {
		used, err := s.projectTokensSince(ctx, projectID, startOfDay(now))
		if err != nil {
			return err
		}
		if used >= daily {
			return fail(429, "project_token_budget_exceeded", "项目 token 预算已用尽", map[string]any{
				"period": "daily",
				"limit":  daily,
				"used":   used,
			})
		}
	}
	if monthly > 0 {
		used, err := s.projectTokensSince(ctx, projectID, startOfMonth(now))
		if err != nil {
			return err
		}
		if used >= monthly {
			return fail(429, "project_token_budget_exceeded", "项目 token 预算已用尽", map[string]any{
				"period": "monthly",
				"limit":  monthly,
				"used":   used,
			})
		}
	}
	return nil
}

// AuthorizeBatch gates batch APIs in enterprise mode. Readers can look,
// other members can create and cancel. An empty project is rejected.
func (s *Service) AuthorizeBatch(ctx context.Context, a Actor, projectID string, write bool) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fail(403, "forbidden", "企业模式下批次必须属于项目", nil)
	}
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return err
	}
	if write && acc.Role == "reader" {
		return fail(403, "forbidden", "只读成员不能修改批次", nil)
	}
	return nil
}

func (s *Service) allowQuotaRead(ctx context.Context, a Actor, projectID string) error {
	if projectID == "" {
		return fail(404, "not_found", "找不到", nil)
	}
	if a.Platform() {
		var one int
		err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM ent_projects WHERE id=$1`, projectID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "not_found", "找不到", nil)
		}
		return err
	}
	_, err := s.requireMember(ctx, projectID, a.ID)
	return err
}

func (s *Service) loadProjectQuota(ctx context.Context, projectID string, now time.Time) (ProjectQuota, error) {
	var q ProjectQuota
	err := s.DB.QueryRowContext(ctx, `
SELECT id, max_concurrent_tasks, daily_token_limit, monthly_token_limit
FROM ent_projects WHERE id=$1`, projectID).Scan(&q.ProjectID, &q.MaxConcurrentTasks, &q.DailyTokenLimit, &q.MonthlyTokenLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectQuota{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return ProjectQuota{}, err
	}
	q.ActiveTasks, err = projectActiveTasks(ctx, s.DB, projectID, 0)
	if err != nil {
		return ProjectQuota{}, err
	}
	q.DailyTokens, err = s.projectTokensSince(ctx, projectID, startOfDay(now))
	if err != nil {
		return ProjectQuota{}, err
	}
	q.MonthlyTokens, err = s.projectTokensSince(ctx, projectID, startOfMonth(now))
	if err != nil {
		return ProjectQuota{}, err
	}
	return q, nil
}

type quotaQuery interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func projectActiveTasks(ctx context.Context, q quotaQuery, projectID string, exceptTaskID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `
SELECT (
    SELECT count(*) FROM execution_jobs
    WHERE project_id=$1
      AND status IN ('queued','leased','running')
      AND ($2::bigint = 0 OR task_id <> $2)
) + (
    SELECT count(*) FROM tasks t
    JOIN ent_project_tasks pt ON pt.task_id=t.id
    WHERE pt.project_id=$1
      AND t.deleted_at IS NULL
      AND NOT t.paused
      AND (t.status='running' OR t.queued)
      AND ($2::bigint = 0 OR t.id <> $2)
      AND NOT EXISTS (
          SELECT 1 FROM execution_jobs j
          WHERE j.task_id=t.id AND j.status IN ('queued','leased','running')
      )
)`, projectID, exceptTaskID).Scan(&n)
	return n, err
}

func (s *Service) projectTokensSince(ctx context.Context, projectID string, since time.Time) (int64, error) {
	usage, err := tokenSum(ctx, s.DB, `
SELECT COALESCE(SUM(COALESCE(input_tokens,0) + COALESCE(output_tokens,0) + COALESCE(cache_read,0) + COALESCE(cache_write,0)), 0)::bigint
FROM llm_usage u
JOIN ent_project_tasks pt ON pt.task_id::text=u.task_id
WHERE pt.project_id=$1 AND u.ts >= $2`, projectID, since)
	if err != nil {
		return 0, err
	}
	records, err := tokenSum(ctx, s.DB, `
SELECT COALESCE(SUM(COALESCE(input_tokens,0) + COALESCE(output_tokens,0) + COALESCE(cache_read,0) + COALESCE(cache_write,0)), 0)::bigint
FROM llm_records r
JOIN ent_project_tasks pt ON pt.task_id::text=r.task_id
WHERE pt.project_id=$1 AND COALESCE(r.ts, r.created_at) >= $2`, projectID, since)
	if err != nil {
		return 0, err
	}
	if records > usage {
		return records, nil
	}
	return usage, nil
}

func tokenSum(ctx context.Context, q quotaQuery, query, projectID string, since time.Time) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, query, projectID, since).Scan(&n)
	return n, err
}

func startOfDay(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}

func startOfMonth(now time.Time) time.Time {
	y, m, _ := now.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, now.Location())
}
