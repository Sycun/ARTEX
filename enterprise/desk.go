package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Entry 是登录完成后服务端认定的第一屏，不接受浏览器里的角色切换。
type Entry struct {
	Kind        string         `json:"kind"`
	Href        string         `json:"href"`
	Name        string         `json:"name"`
	Post        string         `json:"post,omitempty"`
	PostLabel   string         `json:"post_label,omitempty"`
	ProjectID   string         `json:"project_id,omitempty"`
	ProjectName string         `json:"project_name,omitempty"`
	Projects    []ProjectBrief `json:"projects"`
}

// Workbench 描述员工打开原工作台时的当前项目和可做的操作。
type Workbench struct {
	Active      bool           `json:"active"`
	Name        string         `json:"name"`
	Post        string         `json:"post"`
	PostLabel   string         `json:"post_label"`
	Platform    bool           `json:"platform"`
	ProjectID   string         `json:"project_id,omitempty"`
	ProjectName string         `json:"project_name,omitempty"`
	ProjectCode string         `json:"project_code,omitempty"`
	CanEdit     bool           `json:"can_edit"`
	CanRun      bool           `json:"can_run"`
	CanIssue    bool           `json:"can_issue"`
	Projects    []ProjectBrief `json:"projects"`
}

func (s *Service) Entry(ctx context.Context, a Actor) (Entry, error) {
	me, err := s.Me(ctx, a)
	if err != nil {
		return Entry{}, err
	}
	out := Entry{Kind: "enterprise", Href: "/enterprise", Name: a.Name, Projects: me.Projects}
	if a.Gate() != "ready" {
		return out, nil
	}
	var work, review, lead []ProjectBrief
	for _, project := range me.Projects {
		switch project.Post {
		case "employee":
			work = append(work, project)
		case "reviewer":
			review = append(review, project)
		case "leader":
			lead = append(lead, project)
		}
	}
	switch {
	case len(work) == 1:
		if err := s.rememberProject(ctx, a.SessionID, work[0].ID); err != nil {
			return Entry{}, err
		}
		out.Kind = "workbench"
		out.Href = "/function/tasks"
		out.Post = "employee"
		out.PostLabel = postLabel("employee")
		out.ProjectID = work[0].ID
		out.ProjectName = work[0].Name
	case len(work) > 1:
		out.Kind = "pick"
		out.Href = "/enterprise/pick"
		out.Post = "employee"
		out.PostLabel = postLabel("employee")
		out.Projects = work
	case len(review) > 0:
		out.Kind = "review"
		out.Href = "/enterprise/todos"
		out.Post = "reviewer"
		out.PostLabel = postLabel("reviewer")
		out.ProjectID = review[0].ID
		out.ProjectName = review[0].Name
	case len(lead) > 0:
		out.Kind = "conclusions"
		out.Href = "/enterprise/conclusions"
		out.Post = "leader"
		out.PostLabel = postLabel("leader")
		out.ProjectID = lead[0].ID
		out.ProjectName = lead[0].Name
		if len(lead) > 1 {
			out.ProjectID = ""
			out.ProjectName = ""
		}
	case a.Platform():
		out.Kind = "admin"
		out.Href = "/enterprise/admin"
		out.Post = "admin"
		out.PostLabel = "平台管理员"
	}
	return out, nil
}

func (s *Service) rememberProject(ctx context.Context, sessionID, projectID string) error {
	if sessionID == "" || projectID == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE ent_sessions SET project_id=$2 WHERE id=$1`, sessionID, projectID)
	return err
}

// SelectProject 把员工会话固定到一个已授权项目。领导、复核人和只读不能借此进入员工工作台。
func (s *Service) SelectProject(ctx context.Context, a Actor, projectID string) (ProjectBrief, error) {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return ProjectBrief{}, err
	}
	acc.Post = effectivePost(acc.Role, acc.Duties, acc.Post)
	if acc.Post != "employee" {
		return ProjectBrief{}, fail(403, "not_employee", "当前岗位不能进入员工工作台", nil)
	}
	if err := s.rememberProject(ctx, a.SessionID, projectID); err != nil {
		return ProjectBrief{}, err
	}
	var brief ProjectBrief
	if err := s.DB.QueryRowContext(ctx, `SELECT id, code, name FROM ent_projects WHERE id=$1`, projectID).
		Scan(&brief.ID, &brief.Code, &brief.Name); err != nil {
		return ProjectBrief{}, err
	}
	brief.Role = acc.Role
	brief.Post = acc.Post
	brief.Duties = acc.Duties
	return brief, nil
}

func (s *Service) Workbench(ctx context.Context, a Actor) (Workbench, error) {
	me, err := s.Me(ctx, a)
	if err != nil {
		return Workbench{}, err
	}
	view := Workbench{
		Active: true, Name: a.Name, Platform: a.Platform(), Projects: me.Projects, CanRun: false,
	}
	post := ""
	for _, project := range me.Projects {
		if project.Post == "employee" {
			post = "employee"
			break
		}
	}
	if post == "" {
		for _, want := range []string{"reviewer", "leader", "reader"} {
			for _, item := range me.Projects {
				if item.Post == want {
					post = want
					break
				}
			}
			if post != "" {
				break
			}
		}
	}
	if post == "" && a.Platform() {
		post = "admin"
	}
	view.Post = post
	view.PostLabel = postLabel(post)
	if post == "admin" {
		view.PostLabel = "平台管理员"
	}
	view.CanEdit = post == "employee"
	for _, project := range me.Projects {
		if hasDuty(project.Duties, "review") {
			view.CanIssue = true
		}
	}
	for _, project := range me.Projects {
		if a.SessionProject != "" && project.ID != a.SessionProject {
			continue
		}
		if a.SessionProject == "" && project.Post != post {
			continue
		}
		view.ProjectID = project.ID
		view.ProjectName = project.Name
		view.ProjectCode = project.Code
		break
	}
	return view, nil
}

// AllowsTaskFiles 只让能修改本项目资料的员工读取任务文件。领导不从这里取得证据。
func (s *Service) AllowsTaskFiles(ctx context.Context, a Actor, taskID int64) error {
	acc, err := s.TaskAccess(ctx, a, taskID, false)
	if err != nil {
		return err
	}
	if !canEdit(acc.Role, acc.Duties, acc.Post) {
		return fail(404, "not_found", "找不到", nil)
	}
	return nil
}

// TaskAccess 按项目成员关系判断任务。写操作只允许员工岗位。会话里的当前项目收窄员工能看到的范围。
func (s *Service) TaskAccess(ctx context.Context, a Actor, taskID int64, write bool) (access, error) {
	var projectID string
	err := s.DB.QueryRowContext(ctx, `
		SELECT project_id FROM ent_project_tasks WHERE task_id=$1`, taskID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return access{}, fail(404, "not_found", "找不到这个任务", nil)
	}
	if err != nil {
		return access{}, err
	}
	return s.projectAccess(ctx, a, projectID, write)
}

func (s *Service) FindingAccess(ctx context.Context, a Actor, findingID int64, write bool) (access, error) {
	var projectID string
	err := s.DB.QueryRowContext(ctx, `
		SELECT project_id FROM ent_project_findings WHERE finding_id=$1`, findingID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return access{}, fail(404, "not_found", "找不到这条发现", nil)
	}
	if err != nil {
		return access{}, err
	}
	return s.projectAccess(ctx, a, projectID, write)
}

func (s *Service) projectAccess(ctx context.Context, a Actor, projectID string, write bool) (access, error) {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return access{}, fail(404, "not_found", "找不到", nil)
	}
	if a.SessionProject != "" && a.SessionProject != projectID && acc.Post == "employee" {
		return access{}, fail(404, "not_found", "找不到", nil)
	}
	if write && !canEdit(acc.Role, acc.Duties, acc.Post) {
		return access{}, fail(403, "read_only", "当前岗位不能修改任务、发现或启动运行", nil)
	}
	return acc, nil
}

// VisibleTaskIDs 是当前会话可以看到的任务。员工固定在已选项目里。
func (s *Service) VisibleTaskIDs(ctx context.Context, a Actor) ([]int64, error) {
	query := `
		SELECT pt.task_id
		FROM ent_project_tasks pt
		JOIN ent_members m ON m.project_id=pt.project_id AND m.user_id=$1
		JOIN ent_projects p ON p.id=pt.project_id AND p.status='active'`
	args := []any{a.ID}
	if a.SessionProject != "" {
		query += ` WHERE pt.project_id=$2`
		args = append(args, a.SessionProject)
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type FindingRecord struct {
	ID       int64
	Name     string
	Severity string
	Status   string
	Summary  string
	Evidence string
	Created  time.Time
	Redact   bool
}

// HidesEvidence 表示这个岗位不能看发现里的原始证据。
func (s *Service) HidesEvidence(ctx context.Context, a Actor, taskID int64) (bool, error) {
	acc, err := s.TaskAccess(ctx, a, taskID, false)
	if err != nil {
		return false, err
	}
	return !canEdit(acc.Role, acc.Duties, acc.Post), nil
}

// ProjectFindings 返回这个任务里已经归入项目的发现。领导不附带原始证据。
func (s *Service) ProjectFindings(ctx context.Context, a Actor, taskID int64) ([]FindingRecord, error) {
	acc, err := s.TaskAccess(ctx, a, taskID, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT f.id, f.name, f.severity, f.status, f.summary, f.evidence, f.created_at
		FROM findings f
		JOIN ent_project_findings p ON p.finding_id=f.id
		JOIN ent_project_tasks t ON t.task_id=f.task_id AND t.project_id=p.project_id
		WHERE f.task_id=$1
		  AND p.project_id IN (SELECT project_id FROM ent_members WHERE user_id=$2)
		  AND ($3='' OR p.project_id=$3)
		ORDER BY f.id`, taskID, a.ID, a.SessionProject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FindingRecord
	for rows.Next() {
		var item FindingRecord
		if err := rows.Scan(&item.ID, &item.Name, &item.Severity, &item.Status, &item.Summary, &item.Evidence, &item.Created); err != nil {
			return nil, err
		}
		if !canEdit(acc.Role, acc.Duties, acc.Post) {
			item.Evidence = ""
			item.Redact = true
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type ConclusionFinding struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Severity    string `json:"severity"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
}

type Remediation struct {
	Total    int `json:"total"`
	Open     int `json:"open"`
	Doing    int `json:"doing"`
	Done     int `json:"done"`
	Accepted int `json:"accepted"`
	Closed   int `json:"closed"`
}

type Conclusion struct {
	ProjectID   string              `json:"project_id"`
	ProjectName string              `json:"project_name"`
	ProjectCode string              `json:"project_code"`
	ReportID    string              `json:"report_id"`
	VersionID   string              `json:"version_id"`
	Number      int                 `json:"number"`
	Title       string              `json:"title"`
	Conclusion  string              `json:"conclusion"`
	IssuedAt    string              `json:"issued_at"`
	IssuerName  string              `json:"issuer_name"`
	FileHash    string              `json:"file_hash"`
	TaskID      int64               `json:"task_id,omitempty"`
	TaskName    string              `json:"task_name,omitempty"`
	Findings    []ConclusionFinding `json:"findings"`
	Remediation Remediation         `json:"remediation"`
}

// Conclusions 只返回领导已授权项目里的已签发版本。草稿和退回稿不在这里。
func (s *Service) Conclusions(ctx context.Context, a Actor) ([]Conclusion, error) {
	return s.conclusionQuery(ctx, a, "")
}

func (s *Service) Conclusion(ctx context.Context, a Actor, reportID string) ([]Conclusion, error) {
	items, err := s.conclusionQuery(ctx, a, reportID)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fail(404, "not_found", "找不到已签发的报告", nil)
	}
	return items, nil
}

func (s *Service) conclusionQuery(ctx context.Context, a Actor, reportID string) ([]Conclusion, error) {
	query := `
		SELECT p.id, p.code, p.name, r.id, r.title, v.id, v.number, v.body,
		       i.file_hash, i.issued_at, COALESCE(u.display_name,''),
		       COALESCE(f.id,0), COALESCE(NULLIF(f.name,''), COALESCE(f.summary,'')),
		       COALESCE(f.severity,''), COALESCE(f.status,''),
		       COALESCE(t.id,0), COALESCE(NULLIF(t.name,''),'')
		FROM ent_issues i
		JOIN ent_versions v ON v.id=i.version_id AND v.status='issued'
		JOIN ent_reports r ON r.id=v.report_id
		JOIN ent_projects p ON p.id=r.project_id AND p.status='active'
		JOIN ent_members m ON m.project_id=p.id AND m.user_id=$1
		LEFT JOIN ent_users u ON u.id=i.issuer_id
		LEFT JOIN ent_report_sources src ON src.report_id=r.id
		LEFT JOIN findings f ON f.id=src.finding_id
		LEFT JOIN tasks t ON t.id=f.task_id
		WHERE (m.role='leader' OR EXISTS (
			SELECT 1 FROM ent_duties d
			WHERE d.project_id=m.project_id AND d.user_id=m.user_id AND d.duty='lead'))`
	args := []any{a.ID}
	if reportID != "" {
		query += ` AND r.id=$2`
		args = append(args, reportID)
	}
	query += ` ORDER BY i.issued_at DESC, v.number DESC, f.id`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	order := []string{}
	byVersion := map[string]*Conclusion{}
	for rows.Next() {
		var item Conclusion
		var finding ConclusionFinding
		var issued time.Time
		if err := rows.Scan(
			&item.ProjectID, &item.ProjectCode, &item.ProjectName, &item.ReportID, &item.Title,
			&item.VersionID, &item.Number, &item.Conclusion, &item.FileHash, &issued, &item.IssuerName,
			&finding.ID, &finding.Title, &finding.Severity, &finding.Status, &item.TaskID, &item.TaskName,
		); err != nil {
			return nil, err
		}
		item.IssuedAt = issued.Format(time.RFC3339)
		current, seen := byVersion[item.VersionID]
		if !seen {
			item.Findings = []ConclusionFinding{}
			order = append(order, item.VersionID)
			copied := item
			byVersion[item.VersionID] = &copied
			current = &copied
		}
		if finding.ID == 0 {
			continue
		}
		finding.StatusLabel = findingStatusLabel(finding.Status)
		current.Findings = append(current.Findings, finding)
		addRemediation(&current.Remediation, finding.Status)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Conclusion, 0, len(order))
	for _, id := range order {
		out = append(out, *byVersion[id])
	}
	return out, nil
}

func addRemediation(item *Remediation, status string) {
	item.Total++
	switch status {
	case "fixed", "resolved":
		item.Done++
	case "in_progress":
		item.Doing++
	case "risk_accepted":
		item.Accepted++
	case "false_positive", "ignored", "duplicate":
		item.Closed++
	default:
		item.Open++
	}
}

func findingStatusLabel(status string) string {
	switch status {
	case "pending":
		return "待处理"
	case "in_progress":
		return "处理中"
	case "confirmed":
		return "已确认"
	case "resolved":
		return "已处理"
	case "fixed":
		return "已修复"
	case "false_positive":
		return "误报"
	case "ignored":
		return "已忽略"
	case "duplicate":
		return "重复"
	case "risk_accepted":
		return "风险接受"
	default:
		return status
	}
}
