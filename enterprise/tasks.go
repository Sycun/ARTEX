package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type TaskQuery struct {
	Text     string
	Status   string
	Page     int
	PageSize int
}

type TaskList struct {
	Items       []TaskItem `json:"items"`
	Total       int        `json:"total"`
	Page        int        `json:"page"`
	PageSize    int        `json:"page_size"`
	ProjectID   string     `json:"project_id"`
	ProjectName string     `json:"project_name"`
}

type TaskItem struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	CompletedAt string `json:"completed_at,omitempty"`
	Findings    int    `json:"findings"`
	Sample      string `json:"sample,omitempty"`
	SampleLabel string `json:"sample_label,omitempty"`
}

type TaskAsset struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type TaskFinding struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	ReportID string `json:"report_id,omitempty"`
	Issued   bool   `json:"issued"`
}

type TaskDetail struct {
	ID          int64         `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Goal        string        `json:"goal"`
	Status      string        `json:"status"`
	StatusLabel string        `json:"status_label"`
	Queued      bool          `json:"queued"`
	Running     bool          `json:"running"`
	CompletedAt string        `json:"completed_at,omitempty"`
	Sample      string        `json:"sample,omitempty"`
	SampleLabel string        `json:"sample_label,omitempty"`
	ProjectID   string        `json:"project_id"`
	ProjectName string        `json:"project_name"`
	Findings    []TaskFinding `json:"findings"`
	Assets      []TaskAsset   `json:"assets"`
	ReadOnly    bool          `json:"read_only"`
	LimitNote   string        `json:"limit_note,omitempty"`
}

func (s *Service) ListTasks(ctx context.Context, a Actor, projectID string, q TaskQuery) (TaskList, error) {
	if _, err := s.requireMember(ctx, projectID, a.ID); err != nil {
		return TaskList{}, err
	}
	var name string
	if err := s.DB.QueryRowContext(ctx, `SELECT name FROM ent_projects WHERE id=$1`, projectID).Scan(&name); err != nil {
		return TaskList{}, fail(404, "not_found", "找不到", nil)
	}
	page, size := normalizePage(q.Page, q.PageSize)
	status := normalizeTaskStatus(q.Status)
	text := strings.TrimSpace(q.Text)
	args := []any{projectID, text, status}
	where := `WHERE pt.project_id=$1
		AND ($2='' OR t.name ILIKE '%'||$2||'%' OR t.description ILIKE '%'||$2||'%' OR t.goal ILIKE '%'||$2||'%')
		AND ($3='' OR t.status=$3)`
	var total int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM ent_project_tasks pt JOIN tasks t ON t.id=pt.task_id `+where, args...).Scan(&total); err != nil {
		return TaskList{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		WITH page_tasks AS (
			SELECT t.id, t.name, t.status, t.completed_at, pt.sample_key
			FROM ent_project_tasks pt JOIN tasks t ON t.id=pt.task_id
			`+where+`
			ORDER BY t.completed_at DESC NULLS LAST, t.id DESC LIMIT $4 OFFSET $5
		), finding_counts AS (
			SELECT f.task_id, count(*) AS total
			FROM page_tasks t
			JOIN findings f ON f.task_id=t.id
			JOIN ent_project_findings p ON p.finding_id=f.id AND p.project_id=$1
			GROUP BY f.task_id
		)
		SELECT t.id, t.name, t.status, t.completed_at, t.sample_key, COALESCE(f.total, 0)
		FROM page_tasks t LEFT JOIN finding_counts f ON f.task_id=t.id
		ORDER BY t.completed_at DESC NULLS LAST, t.id DESC`,
		append(args, size, (page-1)*size)...)
	if err != nil {
		return TaskList{}, err
	}
	defer rows.Close()
	items := []TaskItem{}
	for rows.Next() {
		var item TaskItem
		var completed sql.NullTime
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &completed, &item.Sample, &item.Findings); err != nil {
			return TaskList{}, err
		}
		item.StatusLabel = taskStatusLabel(item.Status)
		if item.Name == "" {
			item.Name = "未命名任务"
		}
		if completed.Valid {
			item.CompletedAt = completed.Time.Format(time.RFC3339)
		}
		item.SampleLabel = sampleLabel(item.Sample)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return TaskList{}, err
	}
	return TaskList{Items: items, Total: total, Page: page, PageSize: size, ProjectID: projectID, ProjectName: name}, nil
}

func (s *Service) Task(ctx context.Context, a Actor, projectID string, taskID int64) (TaskDetail, error) {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return TaskDetail{}, err
	}
	var detail TaskDetail
	var completed sql.NullTime
	var queued bool
	err = s.DB.QueryRowContext(ctx, `
		SELECT t.id, COALESCE(NULLIF(t.name,''),'未命名任务'), t.description, t.goal, t.status, t.queued,
		       t.completed_at, p.id, p.name, pt.sample_key
		FROM ent_project_tasks pt
		JOIN tasks t ON t.id=pt.task_id
		JOIN ent_projects p ON p.id=pt.project_id
		WHERE pt.project_id=$1 AND pt.task_id=$2 AND t.deleted_at IS NULL`, projectID, taskID).
		Scan(&detail.ID, &detail.Name, &detail.Description, &detail.Goal, &detail.Status, &queued,
			&completed, &detail.ProjectID, &detail.ProjectName, &detail.Sample)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskDetail{}, fail(404, "not_found", "找不到这个任务", nil)
	}
	if err != nil {
		return TaskDetail{}, err
	}
	detail.StatusLabel = taskStatusLabel(detail.Status)
	detail.Queued = queued
	detail.Running = detail.Status == "running" && !queued
	detail.SampleLabel = sampleLabel(detail.Sample)
	if completed.Valid {
		detail.CompletedAt = completed.Time.Format(time.RFC3339)
	}
	detail.ReadOnly = readOnlyRole(acc.Role) || acc.Post == "leader"
	if detail.ReadOnly {
		detail.LimitNote = "当前是只读审核。可以查看任务、关联资产、发现和已签发的脱敏报告。不能启动任务、修改数据或查看原始凭据。需要办理时，请项目负责人授予相应职责。"
	}
	assets, err := s.taskAssets(ctx, taskID)
	if err != nil {
		return TaskDetail{}, err
	}
	findings, err := s.taskFindings(ctx, projectID, taskID)
	if err != nil {
		return TaskDetail{}, err
	}
	detail.Assets = assets
	detail.Findings = findings
	return detail, nil
}

func (s *Service) taskAssets(ctx context.Context, taskID int64) ([]TaskAsset, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, type, COALESCE(domain,''), COALESCE(url,''), COALESCE(page_title,''), COALESCE(service_name,'')
		FROM assets WHERE task_ids @> ARRAY[$1]::bigint[] ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskAsset{}
	for rows.Next() {
		var item TaskAsset
		var domain, url, title, service string
		if err := rows.Scan(&item.ID, &item.Kind, &domain, &url, &title, &service); err != nil {
			return nil, err
		}
		item.Name = domain
		if item.Name == "" {
			item.Name = service
		}
		if item.Name == "" {
			item.Name = "关联资产"
		}
		item.URL = url
		item.Title = title
		if service != "" && item.Title == "" {
			item.Title = service
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) taskFindings(ctx context.Context, projectID string, taskID int64) ([]TaskFinding, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT f.id, f.name, f.summary, COALESCE(fr.report_id,''),
		       EXISTS (
		         SELECT 1 FROM ent_versions v
		         WHERE fr.report_id IS NOT NULL AND v.report_id=fr.report_id AND v.status='issued')
		FROM findings f
		JOIN ent_project_findings p ON p.finding_id=f.id AND p.project_id=$1
		LEFT JOIN ent_finding_report fr ON fr.finding_id=f.id
		WHERE f.task_id=$2
		ORDER BY f.id`, projectID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskFinding{}
	for rows.Next() {
		var item TaskFinding
		if err := rows.Scan(&item.ID, &item.Title, &item.Summary, &item.ReportID, &item.Issued); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type ReportListItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	VersionID   string `json:"version_id"`
	Number      int    `json:"number"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	FindingID   int64  `json:"finding_id,omitempty"`
	TaskID      int64  `json:"task_id,omitempty"`
	TaskName    string `json:"task_name,omitempty"`
	SampleLabel string `json:"sample_label,omitempty"`
}

func (s *Service) ListReports(ctx context.Context, a Actor, projectID string) ([]ReportListItem, error) {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT r.id, r.title, v.id, v.number, v.status,
		       COALESCE(src.finding_id,0), COALESCE(t.id,0), COALESCE(t.name,''), COALESCE(pt.sample_key,'')
		FROM ent_reports r
		JOIN ent_versions v ON v.report_id=r.id
		LEFT JOIN ent_report_sources src ON src.report_id=r.id
		LEFT JOIN findings f ON f.id=src.finding_id
		LEFT JOIN tasks t ON t.id=f.task_id
		LEFT JOIN ent_project_tasks pt ON pt.task_id=t.id AND pt.project_id=r.project_id
		WHERE r.project_id=$1
		ORDER BY r.created_at, v.number`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]ReportListItem{}
	var order []string
	for rows.Next() {
		var item ReportListItem
		if err := rows.Scan(&item.ID, &item.Title, &item.VersionID, &item.Number, &item.Status,
			&item.FindingID, &item.TaskID, &item.TaskName, &item.SampleLabel); err != nil {
			return nil, err
		}
		item.StatusLabel = statusLabel(item.Status)
		item.SampleLabel = sampleLabel(item.SampleLabel)
		prev, seen := byID[item.ID]
		if !seen {
			order = append(order, item.ID)
			byID[item.ID] = item
			continue
		}
		if readOnlyRole(acc.Role) || acc.Post == "leader" {
			if item.Status == "issued" && (prev.Status != "issued" || item.Number > prev.Number) {
				byID[item.ID] = item
			}
			continue
		}
		if item.Number >= prev.Number {
			byID[item.ID] = item
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []ReportListItem{}
	for _, id := range order {
		item := byID[id]
		if (readOnlyRole(acc.Role) || acc.Post == "leader") && item.Status != "issued" {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func normalizePage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 50 {
		size = 50
	}
	return page, size
}

func normalizeTaskStatus(status string) string {
	switch status {
	case "created", "running", "paused", "done", "failed", "timeout":
		return status
	default:
		return ""
	}
}

func taskStatusLabel(status string) string {
	switch status {
	case "created":
		return "未启动"
	case "running":
		return "运行中"
	case "paused":
		return "已暂停"
	case "done":
		return "已完成"
	case "failed":
		return "未成功"
	case "timeout":
		return "已超时"
	default:
		return "已记录"
	}
}

func sampleLabel(key string) string {
	switch key {
	case ReviewSampleKey:
		return "历史审核样例 DEMO-RV-001"
	case SampleKey:
		return "演示样例 DEMO-NS-001"
	default:
		return ""
	}
}

type ProjectHome struct {
	ID        string   `json:"id"`
	Code      string   `json:"code"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	RoleLabel string   `json:"role_label"`
	Duties    []string `json:"duties"`
	Tasks     int      `json:"tasks"`
	Findings  int      `json:"findings"`
	Cases     int      `json:"cases"`
	Issued    int      `json:"issued"`
	Running   int      `json:"running"`
	Queued    int      `json:"queued"`
	Notice    string   `json:"notice"`
	LimitNote string   `json:"limit_note,omitempty"`
	Modules   []string `json:"modules"`
}

func (s *Service) ProjectHome(ctx context.Context, a Actor, projectID string) (ProjectHome, error) {
	brief, err := s.ProjectByID(ctx, a, projectID)
	if err != nil {
		return ProjectHome{}, err
	}
	counts, err := s.projectCounts(ctx, projectID)
	if err != nil {
		return ProjectHome{}, err
	}
	running, queued, err := s.Runtime(ctx)
	if err != nil {
		return ProjectHome{}, err
	}
	home := ProjectHome{
		ID: brief.ID, Code: brief.Code, Name: brief.Name, Role: brief.Role, Duties: brief.Duties,
		RoleLabel: roleLabel(brief.Role),
		Tasks:     counts.Tasks, Findings: counts.Findings, Cases: counts.Cases, Issued: counts.Issued,
		Running: running, Queued: queued,
		Modules: []string{"home", "tasks", "findings", "assets", "reports", "todos"},
	}
	switch brief.Role {
	case "reader":
		home.Notice = "任务是这个项目的入口。打开一条任务可以看到关联的资产和发现，再从发现进入报告。已签发的脱敏版可以下载，然后返回原来的任务。"
		home.LimitNote = "当前是只读审核，可以完整浏览。不能启动任务、修改数据或查看原始凭据。编写、审批和签发需要项目负责人另行授予。项目审计也需要审计职责。"
	case "owner":
		home.Notice = "任务、发现和报告都在这个项目里。审批加入和授予职责在成员与职责，核对签发记录在项目审计。"
		home.Modules = append(home.Modules, "members", "audit")
	default:
		home.Notice = "从任务进入，可以看到它关联的资产和发现。需要写报告时，从发现里打开。"
		if hasDuty(brief.Duties, "audit") {
			home.Modules = append(home.Modules, "audit")
		}
	}
	if hasDuty(brief.Duties, "audit") && brief.Role != "owner" {
		seen := false
		for _, item := range home.Modules {
			if item == "audit" {
				seen = true
			}
		}
		if !seen {
			home.Modules = append(home.Modules, "audit")
		}
	}
	return home, nil
}

func roleLabel(role string) string {
	switch role {
	case "owner":
		return "负责人"
	case "member":
		return "成员"
	case "reader":
		return "只读"
	case "leader":
		return "领导"
	default:
		return "成员"
	}
}
