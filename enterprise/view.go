package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type VersionView struct {
	ID           string   `json:"id"`
	Number       int      `json:"number"`
	Status       string   `json:"status"`
	StatusLabel  string   `json:"status_label"`
	Body         string   `json:"body,omitempty"`
	BodyHidden   bool     `json:"body_hidden"`
	BodyHash     string   `json:"body_hash,omitempty"`
	ReturnReason string   `json:"return_reason,omitempty"`
	Editors      []string `json:"editors"`
	Lineage      []string `json:"lineage"`
	Download     bool     `json:"download"`
}

type OriginalMeta struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	CreatedAt   time.Time `json:"created_at"`
	Notice      string    `json:"notice"`
	Previewable bool      `json:"previewable"`
}

type ReportView struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"project_id"`
	Title          string          `json:"title"`
	FindingID      int64           `json:"finding_id"`
	TaskID         int64           `json:"task_id,omitempty"`
	TaskName       string          `json:"task_name,omitempty"`
	SampleLabel    string          `json:"sample_label,omitempty"`
	FocusID        string          `json:"focus_id"`
	Versions       []VersionView   `json:"versions"`
	IssuedCount    int             `json:"issued_count"`
	NoIssuedNotice string          `json:"no_issued_notice,omitempty"`
	Waiting        string          `json:"waiting,omitempty"`
	NextStep       string          `json:"next_step,omitempty"`
	Actions        map[string]bool `json:"actions"`
	ActionHint     string          `json:"action_hint,omitempty"`
	Original       *OriginalMeta   `json:"original,omitempty"`
	CaseCount      int             `json:"case_count"`
	Limited        bool            `json:"limited"`
}

func (s *Service) versionView(ctx context.Context, a Actor, versionID string) (VersionView, error) {
	v, err := s.loadVersion(ctx, nil, versionID)
	if err != nil {
		return VersionView{}, err
	}
	view, err := s.Report(ctx, a, v.ReportID)
	if err != nil {
		return VersionView{}, err
	}
	for _, item := range view.Versions {
		if item.ID == versionID {
			return item, nil
		}
	}
	return VersionView{}, fail(404, "not_found", "找不到", nil)
}

func (s *Service) Report(ctx context.Context, a Actor, reportID string) (ReportView, error) {
	var projectID, title, createdBy string
	err := s.DB.QueryRowContext(ctx, `SELECT project_id, title, created_by FROM ent_reports WHERE id=$1`, reportID).Scan(&projectID, &title, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportView{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return ReportView{}, err
	}
	acc, memberErr := s.requireMember(ctx, projectID, a.ID)
	delegatedID := ""
	if memberErr != nil {
		delegatedID, err = s.matchingDelegation(ctx, a.ID, reportID)
		if err != nil || delegatedID == "" {
			return ReportView{}, fail(404, "not_found", "找不到", nil)
		}
		acc = access{}
	}
	rows, err := s.DB.QueryContext(ctx, versionSelect+` WHERE v.report_id=$1 ORDER BY v.number`, reportID)
	if err != nil {
		return ReportView{}, err
	}
	defer rows.Close()
	var versions []versionRow
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return ReportView{}, err
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return ReportView{}, err
	}
	var findingID int64
	_ = s.DB.QueryRowContext(ctx, `SELECT finding_id FROM ent_report_sources WHERE report_id=$1 LIMIT 1`, reportID).Scan(&findingID)
	view := ReportView{
		ID: reportID, ProjectID: projectID, Title: title, FindingID: findingID,
		Versions: []VersionView{}, Actions: map[string]bool{}, CaseCount: 0, Limited: delegatedID != "",
	}
	s.attachTask(ctx, &view, findingID, projectID)
	if readOnlyRole(acc.Role) || acc.Post == "leader" {
		for _, v := range versions {
			if v.Status != "issued" {
				continue
			}
			view.Versions = append(view.Versions, s.visibleVersion(ctx, a, acc, v, false))
		}
		view.IssuedCount = len(view.Versions)
		if view.IssuedCount == 0 {
			view.NoIssuedNotice = "尚无签发版"
			if acc.Post == "leader" {
				view.NextStep = "签发完成前，领导看不到草稿和结论。"
			} else {
				view.NextStep = "签发完成前不能下载正文。只读审核不能编写或签发。"
			}
		} else {
			view.Actions["download"] = true
			if acc.Post == "leader" {
				view.NextStep = "阅读结论、下载这次冻结的签发文件。需要时再查看来源任务。"
			} else {
				view.NextStep = "下载脱敏版，然后可以返回原来的任务。核对审计记录需要审计职责。"
			}
		}
		view.FocusID = focusID(view.Versions)
		return view, nil
	}
	for _, v := range versions {
		if delegatedID != "" && v.ID != delegatedID {
			continue
		}
		delegated := delegatedID != "" && v.ID == delegatedID
		item := s.visibleVersion(ctx, a, acc, v, delegated)
		if item.Status == "issued" {
			view.IssuedCount++
		}
		view.Versions = append(view.Versions, item)
	}
	view.FocusID = focusID(view.Versions)
	if orig, err := s.originalForFinding(ctx, findingID); err == nil {
		view.Original = &orig
	}
	var focus *versionRow
	for i := range versions {
		if versions[i].ID == view.FocusID {
			focus = &versions[i]
		}
	}
	if focus != nil {
		s.fillActions(ctx, &view, a, acc, *focus, delegatedID != "")
	}
	return view, nil
}

func (s *Service) attachTask(ctx context.Context, view *ReportView, findingID int64, projectID string) {
	if findingID == 0 {
		return
	}
	var taskID int64
	var taskName, sample string
	err := s.DB.QueryRowContext(ctx, `
		SELECT t.id, COALESCE(NULLIF(t.name,''),'未命名任务'), COALESCE(pt.sample_key,'')
		FROM findings f
		JOIN tasks t ON t.id=f.task_id
		JOIN ent_project_tasks pt ON pt.task_id=t.id AND pt.project_id=$2
		WHERE f.id=$1`, findingID, projectID).Scan(&taskID, &taskName, &sample)
	if err != nil {
		return
	}
	view.TaskID = taskID
	view.TaskName = taskName
	view.SampleLabel = sampleLabel(sample)
}

func focusID(items []VersionView) string {
	for _, item := range items {
		if item.Status == "draft" || item.Status == "in_review" {
			return item.ID
		}
	}
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1].ID
}

func (s *Service) visibleVersion(ctx context.Context, a Actor, acc access, v versionRow, delegated bool) VersionView {
	item := VersionView{
		ID: v.ID, Number: v.Number, Status: v.Status, StatusLabel: statusLabel(v.Status),
		Editors: s.names(ctx, v.Editors), Lineage: s.names(ctx, v.Lineage),
	}
	see := canSeeBody(acc, a, v, delegated)
	if see {
		item.Body = v.Body
		item.BodyHash = v.BodyHash
	} else {
		item.BodyHidden = true
	}
	if v.Status == "issued" && acc.Role != "" {
		item.Download = true
	}
	if reason, ok := s.returnReasonFor(ctx, a, acc, v); ok && see {
		item.ReturnReason = reason
	}
	return item
}

func canSeeBody(acc access, a Actor, v versionRow, delegated bool) bool {
	if delegated && v.Status == "in_review" {
		return true
	}
	if acc.Role == "" {
		return false
	}
	if readOnlyRole(acc.Role) || acc.Post == "leader" {
		return v.Status == "issued"
	}
	switch v.Status {
	case "draft":
		return acc.Role == "owner" || hasID(v.Editors, a.ID) || a.ID == v.CreatedBy
	case "in_review", "issued":
		return true
	default:
		return acc.Role == "owner" || hasID(v.Editors, a.ID) || a.ID == v.CreatedBy
	}
}

func (s *Service) returnReasonFor(ctx context.Context, a Actor, acc access, v versionRow) (string, bool) {
	versionID := v.ID
	if v.Status == "draft" && v.SourceID != "" {
		versionID = v.SourceID
	}
	var display, actorID string
	var removed bool
	err := s.DB.QueryRowContext(ctx, `SELECT reason_display, actor_id, credential_removed FROM ent_review_records WHERE version_id=$1`, versionID).Scan(&display, &actorID, &removed)
	if err != nil {
		return "", false
	}
	if strings.Contains(display, DemoSecret) {
		return "", false
	}
	allowed := acc.Role == "owner" || actorID == a.ID || hasID(v.Editors, a.ID) || a.ID == v.CreatedBy
	if v.Status == "draft" {
		allowed = acc.Role == "owner" || actorID == a.ID || a.ID == v.CreatedBy || hasID(v.Lineage, a.ID) || hasID(v.Editors, a.ID)
	}
	if !allowed || readOnlyRole(acc.Role) || acc.Post == "leader" {
		return "", false
	}
	return display, true
}

func (s *Service) fillActions(ctx context.Context, view *ReportView, a Actor, acc access, v versionRow, delegated bool) {
	view.Actions = map[string]bool{}
	if delegated {
		if !hasID(v.avoidance(), a.ID) && v.Status == "in_review" {
			view.Actions["issue"] = true
		}
		view.Waiting = "你被委派签发这个冻结版本。不能下载，也不能打开其他报告。"
		view.NextStep = "核对版本号和脱敏结果后，签发这个冻结版本。草稿如果改过，这份委派就失效。"
		view.ActionHint = "回避集合是本版编辑者和来源贡献人。你不在其中才能签发。"
		return
	}
	switch v.Status {
	case "draft":
		if !readOnlyRole(acc.Role) && acc.Post != "leader" {
			view.Actions["save"] = true
			view.Actions["submit"] = strings.TrimSpace(v.Body) != ""
			view.Actions["request_original"] = view.Original != nil
		}
		if hasID(v.avoidance(), a.ID) {
			view.ActionHint = "你写过这一版，或这份正文来自你写过的版本，不能签发。"
		}
		view.NextStep = "保存脱敏正文后送审。送审前复核人看不到正文。"
		view.Waiting = "还没有送审，没有人在等这份草稿。"
	case "in_review":
		if a.ID == v.Submitted || acc.Role == "owner" {
			view.Actions["withdraw"] = true
		}
		if hasDuty(acc.Duties, "review") && !hasID(v.Editors, a.ID) {
			view.Actions["return"] = true
		}
		if hasDuty(acc.Duties, "review") && !hasID(v.avoidance(), a.ID) {
			view.Actions["issue"] = true
		}
		if hasID(v.avoidance(), a.ID) {
			view.ActionHint = "你写过这一版，或这份正文来自你写过的版本，不能签发。"
		}
		names := s.reviewerNames(ctx, v)
		if names != "" {
			view.Waiting = "正在等待" + names + "复核这个冻结版本。"
		} else {
			view.Waiting = "正在等待合格复核人处理。"
		}
		view.NextStep = "这一版已冻结，不能再改字。可以撤回，或等待退回、签发。"
	case "issued":
		view.Actions["download"] = true
		if !readOnlyRole(acc.Role) && acc.Post != "leader" {
			view.Actions["supplement"] = true
		}
		view.NextStep = "下载的是签发时冻住的文件。补证会打开新草稿，不会改变这一版的字节。"
		view.Waiting = "这一版已经签发。"
	}
}

func (s *Service) reviewerNames(ctx context.Context, v versionRow) string {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT u.display_name FROM ent_todos t JOIN ent_users u ON u.id=t.assignee_id
		WHERE t.subject_id=$1 AND t.kind='report_review' AND t.status='open'`, v.ID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return ""
		}
		names = append(names, name)
	}
	return strings.Join(names, "、")
}

func (s *Service) matchingDelegation(ctx context.Context, userID, reportID string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `
		SELECT g.version_id FROM ent_delegations g
		JOIN ent_versions v ON v.id=g.version_id
		WHERE g.grantee_id=$1 AND v.report_id=$2 AND g.action='issue' AND g.expires_at>$3
		  AND v.status='in_review' AND v.body_hash=g.body_hash
		ORDER BY g.created_at DESC LIMIT 1`, userID, reportID, s.now()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Service) originalForFinding(ctx context.Context, findingID int64) (OriginalMeta, error) {
	var meta OriginalMeta
	err := s.DB.QueryRowContext(ctx, `SELECT id, filename, created_at FROM ent_originals WHERE finding_id=$1`, findingID).Scan(&meta.ID, &meta.Filename, &meta.CreatedAt)
	if err != nil {
		return OriginalMeta{}, err
	}
	meta.Notice = "含凭据，需另行授权"
	meta.Previewable = false
	return meta, nil
}

type Download struct {
	Filename string
	Bytes    []byte
	Hash     string
}

func (s *Service) Download(ctx context.Context, a Actor, versionID string) (Download, error) {
	v, err := s.loadVersion(ctx, nil, versionID)
	if err != nil {
		return Download{}, err
	}
	acc, err := s.requireMember(ctx, v.ProjectID, a.ID)
	if err != nil {
		return Download{}, fail(404, "not_found", "找不到", nil)
	}
	if v.Status != "issued" {
		return Download{}, fail(404, "not_found", "找不到可下载的签发文件", nil)
	}
	var file []byte
	var hash, filename string
	var key sql.NullString
	err = s.DB.QueryRowContext(ctx, `
		SELECT blob_key, CASE WHEN NULLIF(blob_key, '') IS NULL THEN file_bytes END, file_hash, filename
		FROM ent_issues WHERE version_id=$1`, versionID).
		Scan(&key, &file, &hash, &filename)
	if errors.Is(err, sql.ErrNoRows) {
		return Download{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return Download{}, err
	}
	if key.Valid && key.String != "" {
		file, err = s.blobs.Get(ctx, key.String)
		if err != nil {
			return Download{}, err
		}
	}
	if Hash(file) != hash {
		return Download{}, fail(500, "hash_mismatch", "签发文件和记录的哈希不一致，已停止下载", nil)
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		return s.audit(ctx, tx, v.ProjectID, a.ID, "report_exported", "导出脱敏报告", "下载脱敏报告", "version", versionID, hash, &v.Number)
	})
	if err != nil {
		return Download{}, err
	}
	_ = acc
	return Download{Filename: filename, Bytes: file, Hash: hash}, nil
}

type Todo struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	KindLabel string `json:"kind_label"`
	ProjectID string `json:"project_id,omitempty"`
	SubjectID string `json:"subject_id"`
	ReportID  string `json:"report_id,omitempty"`
	Preview   string `json:"preview"`
	Status    string `json:"status"`
}

func (s *Service) Todos(ctx context.Context, a Actor) ([]Todo, error) {
	if a.Gate() != "ready" {
		return []Todo{}, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id, t.kind, COALESCE(t.project_id,''), t.subject_id, t.preview, t.status,
		       COALESCE(v.report_id, ev.report_id, '')
		FROM ent_todos t
		LEFT JOIN ent_versions v ON t.subject_type='version' AND v.id=t.subject_id
		LEFT JOIN ent_evidence_requests ev ON t.subject_type='evidence' AND ev.id=t.subject_id
		WHERE t.assignee_id=$1 AND t.status='open' ORDER BY t.created_at DESC`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Kind, &t.ProjectID, &t.SubjectID, &t.Preview, &t.Status, &t.ReportID); err != nil {
			return nil, err
		}
		t.KindLabel = todoLabel(t.Kind)
		if strings.Contains(t.Preview, DemoSecret) {
			return nil, fail(500, "redaction_failed", "待办预览含有凭据", nil)
		}
		out = append(out, t)
	}
	if out == nil {
		out = []Todo{}
	}
	return out, rows.Err()
}

func todoLabel(kind string) string {
	switch kind {
	case "join_request":
		return "加入请求"
	case "original_request":
		return "原件申请"
	case "report_review":
		return "报告复核"
	case "report_returned":
		return "报告已退回"
	case "report_issued":
		return "报告已签发"
	case "assign_reviewer":
		return "指定复核人"
	case "delegate_issue":
		return "对象级限时委派"
	default:
		return kind
	}
}

type AuditEvent struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Reason      string `json:"structured_reason"`
	Summary     string `json:"summary"`
	Actor       string `json:"actor"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	ReportID    string `json:"report_id,omitempty"`
	VersionNo   *int   `json:"version_no,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	CreatedAt   string `json:"created_at"`
	Note        string `json:"note,omitempty"`
}

func (s *Service) Audit(ctx context.Context, a Actor, projectID string) ([]AuditEvent, error) {
	if err := s.canReadAudit(ctx, a, projectID); err != nil {
		return nil, err
	}
	return s.auditRows(ctx, projectID, a)
}

func (s *Service) canReadAudit(ctx context.Context, a Actor, projectID string) error {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err == nil {
		if acc.Role == "reader" && !hasDuty(acc.Duties, "audit") && acc.Role != "owner" {
			if !hasDuty(acc.Duties, "audit") {
				return fail(404, "not_found", "找不到", nil)
			}
		}
		if acc.Role == "reader" && hasDuty(acc.Duties, "audit") {
			return nil
		}
		if acc.Role == "owner" || hasDuty(acc.Duties, "audit") {
			return nil
		}
		return fail(404, "not_found", "找不到", nil)
	}
	var id string
	qerr := s.DB.QueryRowContext(ctx, `
		SELECT id FROM ent_audit_delegations
		WHERE project_id=$1 AND grantee_id=$2 AND expires_at>$3`, projectID, a.ID, s.now()).Scan(&id)
	if qerr == nil {
		return nil
	}
	return fail(404, "not_found", "找不到", nil)
}

func (s *Service) auditRows(ctx context.Context, projectID string, a Actor) ([]AuditEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT e.id, e.action, e.structured_reason, e.summary, u.display_name, e.subject_type, e.subject_id,
		       COALESCE((SELECT report_id FROM ent_versions WHERE id=e.subject_id AND e.subject_type='version'), ''),
		       e.version_no, e.content_hash, e.created_at
		FROM ent_audit e JOIN ent_users u ON u.id=e.actor_id
		WHERE e.project_id=$1 ORDER BY e.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		var version sql.NullInt64
		var created time.Time
		if err := rows.Scan(&ev.ID, &ev.Action, &ev.Reason, &ev.Summary, &ev.Actor, &ev.SubjectType, &ev.SubjectID, &ev.ReportID, &version, &ev.ContentHash, &created); err != nil {
			return nil, err
		}
		if version.Valid {
			n := int(version.Int64)
			ev.VersionNo = &n
		}
		ev.CreatedAt = created.Format(time.RFC3339)
		if ev.Reason == "报告已退回" || ev.Reason == "原件申请被拒绝" {
			ev.Note = "理由仅当事人可在原单查看"
		}
		if strings.Contains(ev.Reason+ev.Summary+ev.Note, DemoSecret) {
			return nil, fail(500, "redaction_failed", "审计摘要含有凭据", nil)
		}
		out = append(out, ev)
	}
	if out == nil {
		out = []AuditEvent{}
	}
	_ = a
	return out, rows.Err()
}

func (s *Service) AuditExport(ctx context.Context, a Actor, projectID string) ([]AuditEvent, error) {
	return s.Audit(ctx, a, projectID)
}

type AssetView struct {
	Name        string    `json:"name"`
	Observation string    `json:"observation"`
	ObservedAt  time.Time `json:"observed_at"`
}

func (s *Service) Assets(ctx context.Context, a Actor, projectID string) ([]AssetView, error) {
	if _, err := s.requireMember(ctx, projectID, a.ID); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT asset_name, observation, observed_at FROM ent_asset_obs WHERE project_id=$1 ORDER BY asset_name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetView
	for rows.Next() {
		var item AssetView
		if err := rows.Scan(&item.Name, &item.Observation, &item.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if out == nil {
		out = []AssetView{}
	}
	return out, rows.Err()
}

type FindingView struct {
	ID         int64         `json:"id"`
	Title      string        `json:"title"`
	Summary    string        `json:"summary"`
	Severity   string        `json:"severity"`
	CaseID     string        `json:"case_id"`
	ProjectID  string        `json:"project_id,omitempty"`
	TaskID     int64         `json:"task_id,omitempty"`
	TaskName   string        `json:"task_name,omitempty"`
	ReportID   string        `json:"report_id,omitempty"`
	Issued     bool          `json:"issued"`
	CanCompose bool          `json:"can_compose"`
	Original   *OriginalMeta `json:"original,omitempty"`
}

type FindingQuery struct {
	Text     string
	Page     int
	PageSize int
}

type FindingPage struct {
	Items    []FindingView `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

func (s *Service) Findings(ctx context.Context, a Actor, projectID string) ([]FindingView, error) {
	page, err := s.SearchFindings(ctx, a, projectID, FindingQuery{})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func (s *Service) SearchFindings(ctx context.Context, a Actor, projectID string, q FindingQuery) (FindingPage, error) {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return FindingPage{}, err
	}
	page, size := normalizePage(q.Page, q.PageSize)
	text := strings.TrimSpace(q.Text)
	var total int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM findings f JOIN ent_project_findings p ON p.finding_id=f.id
		WHERE p.project_id=$1 AND ($2='' OR f.name ILIKE '%'||$2||'%' OR f.summary ILIKE '%'||$2||'%')`,
		projectID, text).Scan(&total); err != nil {
		return FindingPage{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT f.id, f.name, f.summary, f.severity, COALESCE(f.task_id,0), COALESCE(t.name,''),
		       COALESCE(fr.report_id,''),
		       EXISTS (
		         SELECT 1 FROM ent_versions v
		         WHERE fr.report_id IS NOT NULL AND v.report_id=fr.report_id AND v.status='issued')
		FROM findings f
		JOIN ent_project_findings p ON p.finding_id=f.id
		LEFT JOIN tasks t ON t.id=f.task_id
		LEFT JOIN ent_finding_report fr ON fr.finding_id=f.id
		WHERE p.project_id=$1 AND ($2='' OR f.name ILIKE '%'||$2||'%' OR f.summary ILIKE '%'||$2||'%')
		ORDER BY f.id LIMIT $3 OFFSET $4`, projectID, text, size, (page-1)*size)
	if err != nil {
		return FindingPage{}, err
	}
	defer rows.Close()
	out := []FindingView{}
	for rows.Next() {
		var f FindingView
		if err := rows.Scan(&f.ID, &f.Title, &f.Summary, &f.Severity, &f.TaskID, &f.TaskName, &f.ReportID, &f.Issued); err != nil {
			return FindingPage{}, err
		}
		f.ProjectID = projectID
		f.CanCompose = canEdit(acc.Role, acc.Duties, acc.Post)
		if f.TaskID == 0 || !s.taskInProject(ctx, projectID, f.TaskID) {
			f.TaskID = 0
			f.TaskName = ""
		}
		if acc.Post != "leader" {
			if orig, err := s.originalForFinding(ctx, f.ID); err == nil {
				f.Original = &orig
			}
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return FindingPage{}, err
	}
	return FindingPage{Items: out, Total: total, Page: page, PageSize: size}, nil
}

func (s *Service) Finding(ctx context.Context, a Actor, id int64) (FindingView, error) {
	var projectID string
	err := s.DB.QueryRowContext(ctx, `SELECT project_id FROM ent_project_findings WHERE finding_id=$1`, id).Scan(&projectID)
	if err != nil {
		return FindingView{}, fail(404, "not_found", "找不到", nil)
	}
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return FindingView{}, err
	}
	var f FindingView
	err = s.DB.QueryRowContext(ctx, `
		SELECT f.id, f.name, f.summary, f.severity, COALESCE(f.task_id,0), COALESCE(t.name,''),
		       COALESCE(fr.report_id,''),
		       EXISTS (
		         SELECT 1 FROM ent_versions v
		         WHERE fr.report_id IS NOT NULL AND v.report_id=fr.report_id AND v.status='issued')
		FROM findings f
		LEFT JOIN tasks t ON t.id=f.task_id
		LEFT JOIN ent_finding_report fr ON fr.finding_id=f.id
		WHERE f.id=$1`, id).Scan(&f.ID, &f.Title, &f.Summary, &f.Severity, &f.TaskID, &f.TaskName, &f.ReportID, &f.Issued)
	if err != nil {
		return FindingView{}, fail(404, "not_found", "找不到", nil)
	}
	f.ProjectID = projectID
	f.CanCompose = canEdit(acc.Role, acc.Duties, acc.Post)
	if f.TaskID == 0 {
		f.TaskName = ""
	}
	if !s.taskInProject(ctx, projectID, f.TaskID) {
		f.TaskID = 0
		f.TaskName = ""
	}
	if acc.Post != "leader" {
		if orig, err := s.originalForFinding(ctx, f.ID); err == nil {
			f.Original = &orig
		}
	}
	return f, nil
}

func (s *Service) taskInProject(ctx context.Context, projectID string, taskID int64) bool {
	if taskID == 0 {
		return false
	}
	var one int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM ent_project_tasks WHERE project_id=$1 AND task_id=$2`, projectID, taskID).Scan(&one)
	return err == nil
}

func (s *Service) ReadOriginal(ctx context.Context, a Actor, originalID string) ([]byte, error) {
	var body []byte
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, path, hash string
		var requiresCredential bool
		err := tx.QueryRowContext(ctx, `SELECT project_id, storage_path, sha256, contains_credentials
			FROM ent_originals WHERE id=$1 FOR SHARE`, originalID).Scan(&projectID, &path, &hash, &requiresCredential)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "not_found", "找不到", nil)
		}
		if err != nil {
			return err
		}
		acc, err := originalMember(ctx, tx, projectID, a.ID)
		if err != nil {
			return err
		}
		if !canHoldOriginal(acc) {
			return fail(404, "not_found", "找不到", nil)
		}
		grants, err := loadOriginalGrants(ctx, tx, originalID, a.ID, "", s.now())
		if err != nil {
			return err
		}
		if !hasOriginalGrants(grants, requiresCredential, s.now()) {
			return fail(404, "not_found", "找不到可读取的原件", nil)
		}
		body, err = s.readOriginalBlob(ctx, path)
		if err != nil {
			return err
		}
		if Hash(body) != hash {
			return fail(500, "hash_mismatch", "原件与记录的哈希不一致，已停止读取", nil)
		}
		return s.audit(ctx, tx, projectID, a.ID, "original_read", "读取受限原件", "读取受限原件", "original", originalID, hash, nil)
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

type Summary struct {
	Findings int `json:"findings"`
	Cases    int `json:"cases"`
	Issued   int `json:"issued"`
	Running  int `json:"running"`
	Queued   int `json:"queued"`
}

func (s *Service) Summary(ctx context.Context, a Actor, projectID string) (Summary, error) {
	if _, err := s.requireMember(ctx, projectID, a.ID); err != nil {
		return Summary{}, err
	}
	var sum Summary
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_project_findings WHERE project_id=$1`, projectID).Scan(&sum.Findings); err != nil {
		return Summary{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM finding_cases c
		WHERE EXISTS (
			SELECT 1 FROM finding_case_members m
			JOIN ent_project_findings p ON p.finding_id=m.finding_id
			WHERE m.case_id=c.id AND p.project_id=$1)`, projectID).Scan(&sum.Cases); err != nil {
		return Summary{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM ent_versions v JOIN ent_reports r ON r.id=v.report_id
		WHERE r.project_id=$1 AND v.status='issued'`, projectID).Scan(&sum.Issued); err != nil {
		return Summary{}, err
	}
	running, queued, err := s.Runtime(ctx)
	if err != nil {
		return Summary{}, err
	}
	sum.Running, sum.Queued = running, queued
	return sum, nil
}

func (s *Service) NotificationsContain(ctx context.Context, secret string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_notifications WHERE summary LIKE '%' || $1 || '%' OR event_type LIKE '%' || $1 || '%'`, secret).Scan(&n)
	return n > 0, err
}

var _ = fmt.Sprintf
