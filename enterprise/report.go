package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type versionRow struct {
	ID        string
	ReportID  string
	Number    int
	Status    string
	Body      string
	BodyHash  string
	Editors   []string
	Lineage   []string
	SourceID  string
	Submitted string
	ProjectID string
	Title     string
	CreatedBy string
}

func (v versionRow) avoidance() []string { return avoidance(v.Editors, v.Lineage) }

func scanVersion(row interface{ Scan(...any) error }) (versionRow, error) {
	var v versionRow
	var editors, lineage []byte
	var source sql.NullString
	err := row.Scan(&v.ID, &v.ReportID, &v.Number, &v.Status, &v.Body, &v.BodyHash, &editors, &lineage, &source, &v.Submitted, &v.ProjectID, &v.Title, &v.CreatedBy)
	if err != nil {
		return versionRow{}, err
	}
	v.Editors = idsFromJSON(editors)
	v.Lineage = idsFromJSON(lineage)
	if source.Valid {
		v.SourceID = source.String
	}
	return v, nil
}

const versionSelect = `
	SELECT v.id, v.report_id, v.number, v.status, v.body, v.body_hash, v.editors, v.lineage,
	       v.source_version_id, v.submitted_by, r.project_id, r.title, r.created_by
	FROM ent_versions v JOIN ent_reports r ON r.id=v.report_id`

func (s *Service) lockReport(ctx context.Context, tx *sql.Tx, reportID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM ent_reports WHERE id=$1 FOR UPDATE`, reportID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(404, "not_found", "找不到", nil)
	}
	return err
}

func (s *Service) loadVersion(ctx context.Context, tx *sql.Tx, versionID string) (versionRow, error) {
	q := versionSelect + ` WHERE v.id=$1`
	var row versionRow
	var err error
	if tx != nil {
		row, err = scanVersion(tx.QueryRowContext(ctx, q, versionID))
	} else {
		row, err = scanVersion(s.DB.QueryRowContext(ctx, q, versionID))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return versionRow{}, fail(404, "not_found", "找不到", nil)
	}
	return row, err
}

type ReportRef struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	VersionID string `json:"version_id"`
	Number    int    `json:"number"`
	Status    string `json:"status"`
	Reused    bool   `json:"reused"`
}

func (s *Service) OpenReport(ctx context.Context, a Actor, findingID int64) (ReportRef, error) {
	var projectID string
	err := s.DB.QueryRowContext(ctx, `SELECT project_id FROM ent_project_findings WHERE finding_id=$1`, findingID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportRef{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return ReportRef{}, err
	}
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return ReportRef{}, err
	}
	if readOnlyRole(acc.Role) || acc.Post == "leader" {
		return ReportRef{}, fail(403, "reader", "当前岗位不能编写报告。可以在签发后阅读结论并下载签发版。", nil)
	}
	var ref ReportRef
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var reportID string
		err := tx.QueryRowContext(ctx, `SELECT report_id FROM ent_finding_report WHERE finding_id=$1`, findingID).Scan(&reportID)
		if err == nil {
			v, err := s.openOrLatest(ctx, tx, reportID)
			if err != nil {
				return err
			}
			ref = ReportRef{ID: reportID, ProjectID: projectID, VersionID: v.ID, Number: v.Number, Status: v.Status, Reused: true, Title: v.Title}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var title, summary string
		if err := tx.QueryRowContext(ctx, `SELECT name, summary FROM findings WHERE id=$1`, findingID).Scan(&title, &summary); err != nil {
			return err
		}
		reportID = newID()
		versionID := newID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO ent_reports (id, project_id, title, created_by) VALUES ($1,$2,$3,$4)`, reportID, projectID, title, a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ent_report_sources (report_id, finding_id) VALUES ($1,$2)`, reportID, findingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ent_finding_report (finding_id, report_id) VALUES ($1,$2)`, findingID, reportID); err != nil {
			return err
		}
		body := summary
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_versions (id, report_id, number, status, body, body_hash, editors, lineage)
			VALUES ($1,$2,1,'draft',$3,$4,$5,'[]')`,
			versionID, reportID, body, Hash([]byte(body)), idsToJSON([]string{a.ID})); err != nil {
			return err
		}
		ref = ReportRef{ID: reportID, ProjectID: projectID, Title: title, VersionID: versionID, Number: 1, Status: "draft"}
		return s.audit(ctx, tx, projectID, a.ID, "report_opened", "尚无签发", "从单条发现创建报告草稿", "report", reportID, "", nil)
	})
	return ref, err
}

func (s *Service) openOrLatest(ctx context.Context, tx *sql.Tx, reportID string) (versionRow, error) {
	row, err := scanVersion(tx.QueryRowContext(ctx, versionSelect+` WHERE v.report_id=$1 AND v.status IN ('draft','in_review')`, reportID))
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return versionRow{}, err
	}
	row, err = scanVersion(tx.QueryRowContext(ctx, versionSelect+` WHERE v.report_id=$1 ORDER BY v.number DESC LIMIT 1`, reportID))
	return row, err
}

func (s *Service) SaveDraft(ctx context.Context, a Actor, reportID, body string) (VersionView, error) {
	body = strings.TrimSpace(body)
	if containsSecret(body) {
		return VersionView{}, fail(400, "suspected_secret", "正文里有疑似口令，没有保存。请改成不含凭据的说明后再保存。", nil)
	}
	if body == "" {
		return VersionView{}, fail(400, "empty_body", "正文还是空的，写上脱敏说明后再保存", nil)
	}
	var versionID string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.lockReport(ctx, tx, reportID); err != nil {
			return err
		}
		v, err := scanVersion(tx.QueryRowContext(ctx, versionSelect+` WHERE v.report_id=$1 AND v.status='draft'`, reportID))
		if errors.Is(err, sql.ErrNoRows) {
			return fail(409, "not_draft", "当前没有可编辑的草稿。请刷新后查看最新状态。", nil)
		}
		if err != nil {
			return err
		}
		acc, err := s.requireMember(ctx, v.ProjectID, a.ID)
		if err != nil {
			return err
		}
		if readOnlyRole(acc.Role) || acc.Post == "leader" {
			return fail(403, "reader", "当前岗位不能修改报告", nil)
		}
		editors := unionIDs(v.Editors, []string{a.ID})
		hash := Hash([]byte(body))
		if _, err := tx.ExecContext(ctx, `
			UPDATE ent_versions SET body=$2, body_hash=$3, editors=$4::jsonb, updated_at=$5 WHERE id=$1`,
			v.ID, body, hash, idsToJSON(editors), s.now()); err != nil {
			return err
		}
		versionID = v.ID
		return nil
	})
	if err != nil {
		return VersionView{}, err
	}
	return s.versionView(ctx, a, versionID)
}

type ActionResult struct {
	OK               bool   `json:"ok"`
	Reused           bool   `json:"reused"`
	Status           string `json:"status"`
	VersionID        string `json:"version_id,omitempty"`
	SuccessorID      string `json:"successor_id,omitempty"`
	Message          string `json:"message"`
	Next             string `json:"next,omitempty"`
	ActorName        string `json:"actor_name,omitempty"`
	CredentialNotice string `json:"credential_notice,omitempty"`
	FileHash         string `json:"file_hash,omitempty"`
}

func (s *Service) Submit(ctx context.Context, a Actor, versionID, bodyHash string) (ActionResult, error) {
	var out ActionResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		v, err := s.lockedVersion(ctx, tx, versionID)
		if err != nil {
			return err
		}
		acc, err := s.requireMember(ctx, v.ProjectID, a.ID)
		if err != nil {
			return err
		}
		if readOnlyRole(acc.Role) || acc.Post == "leader" {
			return fail(403, "reader", "当前岗位不能送审", nil)
		}
		if v.Status == "in_review" && v.BodyHash == bodyHash {
			out = ActionResult{OK: true, Reused: true, Status: "in_review", VersionID: v.ID, Message: "这一版已经在待复核，没有新建版本，也没有新的待办"}
			return nil
		}
		if v.Status != "draft" {
			return s.conflict(ctx, tx, a, v, "submit")
		}
		if v.BodyHash != bodyHash {
			return fail(409, "hash_mismatch", "正文已经变了。请刷新后再送审，不要对旧内容操作。", map[string]any{"next": "刷新当前版本"})
		}
		if strings.TrimSpace(v.Body) == "" || containsSecret(v.Body) {
			return fail(400, "suspected_secret", "发现疑似口令。请删掉凭据后再送审。这一版仍是草稿。", nil)
		}
		qualified, err := s.qualifiedIssuers(ctx, tx, v)
		if err != nil {
			return err
		}
		if len(qualified) == 0 {
			return s.nobodyCanSign(ctx, tx, v)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE ent_versions SET status='in_review', submitted_by=$2, frozen_at=$3, updated_at=$3 WHERE id=$1 AND status='draft'`,
			v.ID, a.ID, s.now()); err != nil {
			return err
		}
		for _, person := range qualified {
			if err := s.openTodo(ctx, tx, "report_review", v.ProjectID, person.ID, "version", v.ID, fmt.Sprintf("报告 v%d 待复核", v.Number)); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, tx, v.ProjectID, a.ID, "report_submitted", "报告已送审", fmt.Sprintf("v%d 待复核", v.Number), "version", v.ID, v.BodyHash, &v.Number); err != nil {
			return err
		}
		out = ActionResult{OK: true, Status: "in_review", VersionID: v.ID, Message: "v" + fmt.Sprint(v.Number) + " 已冻结，正在等待复核"}
		return s.notify(ctx, tx, "report_submitted")
	})
	return out, err
}

type person struct {
	ID   string
	Name string
}

func (s *Service) qualifiedIssuers(ctx context.Context, tx *sql.Tx, v versionRow) ([]person, error) {
	blocked := map[string]bool{}
	for _, id := range v.avoidance() {
		blocked[id] = true
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.display_name
		FROM ent_duties d JOIN ent_users u ON u.id=d.user_id
		WHERE d.project_id=$1 AND d.duty='review' AND u.directory_status='verified' AND u.employment='active'`, v.ProjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []person
	seen := map[string]bool{}
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		if blocked[p.ID] || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	drows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.display_name
		FROM ent_delegations g JOIN ent_users u ON u.id=g.grantee_id
		WHERE g.project_id=$1 AND g.version_id=$2 AND g.body_hash=$3 AND g.action='issue'
		  AND g.expires_at>$4 AND u.directory_status='verified' AND u.employment='active'`,
		v.ProjectID, v.ID, v.BodyHash, s.now())
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var p person
		if err := drows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		if blocked[p.ID] || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	return out, drows.Err()
}

func (s *Service) nobodyCanSign(ctx context.Context, tx *sql.Tx, v versionRow) error {
	blocked := map[string]bool{}
	for _, id := range v.avoidance() {
		blocked[id] = true
	}
	owners, err := s.owners(ctx, tx, v.ProjectID)
	if err != nil {
		return err
	}
	outside := false
	rows, err := tx.QueryContext(ctx, `
		SELECT m.user_id FROM ent_members m
		LEFT JOIN ent_duties d ON d.project_id=m.project_id AND d.user_id=m.user_id AND d.duty='review'
		WHERE m.project_id=$1 AND m.role IN ('owner','member') AND d.id IS NULL`, v.ProjectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ownerName string
	if len(owners) > 0 {
		ownerName = owners[0].Name
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !blocked[id] {
			outside = true
		}
	}
	if outside {
		for _, owner := range owners {
			if err := s.openTodo(ctx, tx, "assign_reviewer", v.ProjectID, owner.ID, "version", v.ID, "指定复核人"); err != nil {
				return err
			}
		}
		msg := "现在没有能签发这一版的复核人。"
		if ownerName != "" {
			msg += "已通知负责人" + ownerName + "指定复核人。"
		}
		msg += "这一版仍是草稿。"
		return fail(409, "no_reviewer", msg, map[string]any{"next": "等待负责人指定复核人", "actor_name": ownerName})
	}
	admins, err := s.platformAdmins(ctx, tx)
	if err != nil {
		return err
	}
	for _, admin := range admins {
		if err := s.openTodo(ctx, tx, "delegate_issue", v.ProjectID, admin, "version", v.ID, "对象级限时委派"); err != nil {
			return err
		}
	}
	return fail(409, "need_delegation", "项目里没有能签发这一版的人。已通知平台管理员做限时委派。这一版仍是草稿。", map[string]any{"next": "等待平台管理员委派"})
}

func (s *Service) owners(ctx context.Context, tx *sql.Tx, projectID string) ([]person, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.display_name FROM ent_members m JOIN ent_users u ON u.id=m.user_id
		WHERE m.project_id=$1 AND m.role='owner'`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) lockedVersion(ctx context.Context, tx *sql.Tx, versionID string) (versionRow, error) {
	var reportID string
	if err := tx.QueryRowContext(ctx, `SELECT report_id FROM ent_versions WHERE id=$1`, versionID).Scan(&reportID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return versionRow{}, fail(404, "not_found", "找不到", nil)
		}
		return versionRow{}, err
	}
	if err := s.lockReport(ctx, tx, reportID); err != nil {
		return versionRow{}, err
	}
	return s.loadVersion(ctx, tx, versionID)
}

func (s *Service) conflict(ctx context.Context, tx *sql.Tx, a Actor, v versionRow, action string) error {
	if action == "issue" && v.Status == "issued" {
		return s.idempotentIssue(ctx, tx, v)
	}
	if (action == "return" && v.Status == "returned") || (action == "withdraw" && v.Status == "withdrawn") {
		var successor string
		_ = tx.QueryRowContext(ctx, `SELECT id FROM ent_versions WHERE source_version_id=$1 ORDER BY number LIMIT 1`, v.ID).Scan(&successor)
		return fail(200, "replayed", "这个动作已经完成，没有新建版本或待办", map[string]any{
			"ok": true, "reused": true, "status": v.Status, "version_id": v.ID, "successor_id": successor,
		})
	}
	latest := v.Status
	actor := s.winningActor(ctx, tx, v.ID)
	next := "刷新当前版本"
	switch latest {
	case "issued":
		next = "下载该签发版"
	case "returned", "withdrawn":
		next = "打开已经存在的后继草稿"
	}
	return fail(409, "conflict", "这一版的状态已经变了。当前是"+statusLabel(latest)+"。操作者是"+actor+"。下一步："+next, map[string]any{
		"latest_status": latest,
		"actor_name":    actor,
		"next":          next,
	})
}

func (s *Service) idempotentIssue(ctx context.Context, tx *sql.Tx, v versionRow) error {
	var hash string
	if err := tx.QueryRowContext(ctx, `SELECT file_hash FROM ent_issues WHERE version_id=$1`, v.ID).Scan(&hash); err != nil {
		return err
	}
	return fail(200, "replayed", "这一版已经签发，没有新建版本或待办", map[string]any{
		"ok": true, "reused": true, "status": "issued", "version_id": v.ID, "file_hash": hash, "next": "下载该签发版",
	})
}

func (s *Service) winningActor(ctx context.Context, tx *sql.Tx, versionID string) string {
	var actor string
	err := tx.QueryRowContext(ctx, `
		SELECT actor_id FROM ent_audit
		WHERE subject_id=$1 AND action IN ('report_issued','report_returned','report_withdrawn')
		ORDER BY created_at DESC LIMIT 1`, versionID).Scan(&actor)
	if err != nil {
		return "另一位操作者"
	}
	return s.nameOf(ctx, tx, actor)
}

func (s *Service) Return(ctx context.Context, a Actor, versionID, bodyHash, reason string) (ActionResult, error) {
	text := Sanitize(reason)
	if text.Display == "" {
		return ActionResult{}, fail(400, "reason_required", "退回要填写理由。请写明要改什么，再提交。", nil)
	}
	var out ActionResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		v, err := s.lockedVersion(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if v.Status != "in_review" || v.BodyHash != bodyHash {
			if v.Status == "returned" && v.BodyHash == bodyHash {
				return s.conflict(ctx, tx, a, v, "return")
			}
			if v.Status != "in_review" {
				return s.conflict(ctx, tx, a, v, "return")
			}
			return fail(409, "hash_mismatch", "冻结内容已经对不上。请刷新当前版本，不要退回另一份正文。", map[string]any{"next": "刷新当前版本"})
		}
		acc, err := s.requireMember(ctx, v.ProjectID, a.ID)
		if err != nil {
			return err
		}
		if !hasDuty(acc.Duties, "review") {
			return fail(403, "not_reviewer", "你没有复核职责，不能退回这一版", nil)
		}
		if hasID(v.Editors, a.ID) {
			return fail(403, "edited", "你编辑过这一版，不能退回。请让没有保存过这一版的复核人处理。", nil)
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE ent_versions SET status='returned', updated_at=$2 WHERE id=$1 AND status='in_review' AND body_hash=$3`, v.ID, now, bodyHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			fresh, err := s.loadVersion(ctx, tx, v.ID)
			if err != nil {
				return err
			}
			return s.conflict(ctx, tx, a, fresh, "return")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_review_records (version_id, reason_display, reason_sealed, credential_removed, actor_id)
			VALUES ($1,$2,$3,$4,$5)`, v.ID, text.Display, text.Sealed, text.Removed, a.ID); err != nil {
			return err
		}
		nextID := newID()
		nextNo := v.Number + 1
		lineage := avoidance(v.Editors, v.Lineage)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_versions (id, report_id, number, status, body, body_hash, editors, lineage, source_version_id)
			VALUES ($1,$2,$3,'draft',$4,$5,'[]',$6::jsonb,$7)`,
			nextID, v.ReportID, nextNo, v.Body, v.BodyHash, idsToJSON(lineage), v.ID); err != nil {
			return err
		}
		if err := s.closeTodos(ctx, tx, v.ID, "report_review"); err != nil {
			return err
		}
		authors := v.Editors
		if len(authors) == 0 {
			authors = []string{v.CreatedBy}
		}
		for _, author := range authors {
			if err := s.openTodo(ctx, tx, "report_returned", v.ProjectID, author, "version", nextID, "报告已退回"); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, tx, v.ProjectID, a.ID, "report_returned", "报告已退回", "报告已退回", "version", v.ID, v.BodyHash, &v.Number); err != nil {
			return err
		}
		if err := s.notify(ctx, tx, "report_returned"); err != nil {
			return err
		}
		out = ActionResult{OK: true, Status: "returned", VersionID: v.ID, SuccessorID: nextID, Message: "已退回，并打开了下一版草稿", CredentialNotice: notice(text.Removed)}
		return nil
	})
	if result, ok := replayResult(err); ok {
		return result, nil
	}
	return out, err
}

func (s *Service) Withdraw(ctx context.Context, a Actor, versionID, bodyHash string) (ActionResult, error) {
	var out ActionResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		v, err := s.lockedVersion(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if v.Status == "withdrawn" {
			return s.conflict(ctx, tx, a, v, "withdraw")
		}
		if v.Status != "in_review" || v.BodyHash != bodyHash {
			if v.Status != "in_review" {
				return s.conflict(ctx, tx, a, v, "withdraw")
			}
			return fail(409, "hash_mismatch", "冻结内容已经对不上。请刷新当前版本。", map[string]any{"next": "刷新当前版本"})
		}
		acc, err := s.requireMember(ctx, v.ProjectID, a.ID)
		if err != nil {
			return err
		}
		if a.ID != v.Submitted && acc.Role != "owner" {
			return fail(403, "not_submitter", "只有送审人或项目负责人可以撤回送审", nil)
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE ent_versions SET status='withdrawn', updated_at=$2 WHERE id=$1 AND status='in_review' AND body_hash=$3`, v.ID, now, bodyHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			fresh, err := s.loadVersion(ctx, tx, v.ID)
			if err != nil {
				return err
			}
			return s.conflict(ctx, tx, a, fresh, "withdraw")
		}
		nextID := newID()
		lineage := avoidance(v.Editors, v.Lineage)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_versions (id, report_id, number, status, body, body_hash, editors, lineage, source_version_id)
			VALUES ($1,$2,$3,'draft',$4,$5,'[]',$6::jsonb,$7)`,
			nextID, v.ReportID, v.Number+1, v.Body, v.BodyHash, idsToJSON(lineage), v.ID); err != nil {
			return err
		}
		if err := s.closeTodos(ctx, tx, v.ID, "report_review"); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, v.ProjectID, a.ID, "report_withdrawn", "报告已撤回", "报告已撤回", "version", v.ID, v.BodyHash, &v.Number); err != nil {
			return err
		}
		out = ActionResult{OK: true, Status: "withdrawn", VersionID: v.ID, SuccessorID: nextID, Message: "已撤回，并打开了下一版草稿"}
		return nil
	})
	if result, ok := replayResult(err); ok {
		return result, nil
	}
	return out, err
}

func (s *Service) Issue(ctx context.Context, a Actor, versionID, bodyHash string) (ActionResult, error) {
	var out ActionResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		v, err := s.lockedVersion(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if v.Status == "issued" {
			return s.conflict(ctx, tx, a, v, "issue")
		}
		if v.Status != "in_review" || v.BodyHash != bodyHash {
			if v.Status != "in_review" {
				return s.conflict(ctx, tx, a, v, "issue")
			}
			return fail(409, "hash_mismatch", "冻结内容已经对不上，不能签发。请刷新当前版本。", map[string]any{"next": "刷新当前版本"})
		}
		if containsSecret(v.Body) {
			return fail(400, "suspected_secret", "脱敏核验没有通过，只能退回，不能在这里改正文。", nil)
		}
		ok, err := s.canIssue(ctx, tx, a, v)
		if err != nil {
			return err
		}
		if !ok {
			if hasID(v.avoidance(), a.ID) {
				return fail(403, "avoided", "你写过这一版，或这份正文来自你写过的版本，不能签发。", map[string]any{"next": "刷新当前版本"})
			}
			return fail(403, "not_reviewer", "你不能签发这一版。需要复核职责，或一份仍匹配当前冻结内容的限时委派。", nil)
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE ent_versions SET status='issued', updated_at=$2 WHERE id=$1 AND status='in_review' AND body_hash=$3`, v.ID, now, bodyHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			fresh, err := s.loadVersion(ctx, tx, v.ID)
			if err != nil {
				return err
			}
			return s.conflict(ctx, tx, a, fresh, "issue")
		}
		var projectName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM ent_projects WHERE id=$1`, v.ProjectID).Scan(&projectName); err != nil {
			return err
		}
		file := issuedMarkdown(projectName, v.Title, v.Number, v.Body)
		hash := Hash(file)
		filename := fmt.Sprintf("%s-v%d-脱敏报告.md", projectName, v.Number)
		if err := s.closeTodos(ctx, tx, v.ID, "report_review"); err != nil {
			return err
		}
		recipients := unionIDs(v.Editors, []string{v.CreatedBy})
		owners, err := s.owners(ctx, tx, v.ProjectID)
		if err != nil {
			return err
		}
		for _, owner := range owners {
			recipients = unionIDs(recipients, []string{owner.ID})
		}
		for _, id := range recipients {
			if err := s.openTodo(ctx, tx, "report_issued", v.ProjectID, id, "version", v.ID, "报告已签发"); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, tx, v.ProjectID, a.ID, "report_issued", "报告已签发", "报告已签发", "version", v.ID, hash, &v.Number); err != nil {
			return err
		}
		if err := s.notify(ctx, tx, "report_issued"); err != nil {
			return err
		}
		if err := s.writeIssue(ctx, tx, v.ID, a.ID, hash, filename, file, now); err != nil {
			return err
		}
		out = ActionResult{OK: true, Status: "issued", VersionID: v.ID, FileHash: hash, Message: "已签发这个冻结版本", Next: "下载该签发版"}
		return nil
	})
	if result, ok := replayResult(err); ok {
		return result, nil
	}
	return out, err
}

func (s *Service) canIssue(ctx context.Context, tx *sql.Tx, a Actor, v versionRow) (bool, error) {
	if hasID(v.avoidance(), a.ID) {
		return false, nil
	}
	if a.Gate() != "ready" {
		return false, nil
	}
	var duty string
	err := tx.QueryRowContext(ctx, `SELECT duty FROM ent_duties WHERE project_id=$1 AND user_id=$2 AND duty='review'`, v.ProjectID, a.ID).Scan(&duty)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM ent_delegations
		WHERE project_id=$1 AND version_id=$2 AND body_hash=$3 AND action='issue' AND grantee_id=$4 AND expires_at>$5`,
		v.ProjectID, v.ID, v.BodyHash, a.ID, s.now()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func issuedMarkdown(project, title string, number int, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "项目：%s\n\n", project)
	fmt.Fprintf(&b, "版本：v%d\n\n", number)
	fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(body))
	b.WriteString("证据清单：\n")
	fmt.Fprintf(&b, "- 脱敏发现：%s\n", title)
	return []byte(b.String())
}

func replayResult(err error) (ActionResult, bool) {
	var f *Fail
	if !errors.As(err, &f) || f.Code != "replayed" {
		return ActionResult{}, false
	}
	out := ActionResult{OK: true, Reused: true, Message: f.Message}
	if f.Fields != nil {
		if v, ok := f.Fields["status"].(string); ok {
			out.Status = v
		}
		if v, ok := f.Fields["version_id"].(string); ok {
			out.VersionID = v
		}
		if v, ok := f.Fields["successor_id"].(string); ok {
			out.SuccessorID = v
		}
		if v, ok := f.Fields["file_hash"].(string); ok {
			out.FileHash = v
		}
		if v, ok := f.Fields["next"].(string); ok {
			out.Next = v
		}
	}
	return out, true
}

func statusLabel(status string) string {
	switch status {
	case "draft":
		return "草稿"
	case "in_review":
		return "待复核"
	case "withdrawn":
		return "已撤回"
	case "returned":
		return "已退回"
	case "issued":
		return "已签发"
	default:
		return status
	}
}

// Supplement 在没有未结束版本时打开新草稿，不改已签发文件。
func (s *Service) Supplement(ctx context.Context, a Actor, reportID string) (ActionResult, error) {
	var out ActionResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.lockReport(ctx, tx, reportID); err != nil {
			return err
		}
		var openID string
		var openNo int
		var openStatus, actor string
		err := tx.QueryRowContext(ctx, `
			SELECT v.id, v.number, v.status, COALESCE(v.submitted_by, r.created_by)
			FROM ent_versions v JOIN ent_reports r ON r.id=v.report_id
			WHERE v.report_id=$1 AND v.status IN ('draft','in_review')`, reportID).Scan(&openID, &openNo, &openStatus, &actor)
		if err == nil {
			name := s.nameOf(ctx, tx, actor)
			next := "继续编辑或送审"
			if openStatus == "in_review" {
				next = "等待复核，或由送审人、负责人撤回"
			}
			return fail(409, "version_open", fmt.Sprintf("已经有未结束的 v%d（%s）。最后操作者是%s。", openNo, statusLabel(openStatus), name), map[string]any{
				"next": next, "actor_name": name, "latest_status": openStatus,
			})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		v, err := scanVersion(tx.QueryRowContext(ctx, versionSelect+` WHERE v.report_id=$1 AND v.status='issued' ORDER BY v.number DESC LIMIT 1`, reportID))
		if errors.Is(err, sql.ErrNoRows) {
			return fail(409, "no_issue", "还没有已签发版本，不能补证", nil)
		}
		if err != nil {
			return err
		}
		acc, err := s.requireMember(ctx, v.ProjectID, a.ID)
		if err != nil {
			return err
		}
		if readOnlyRole(acc.Role) || acc.Post == "leader" {
			return fail(403, "reader", "当前岗位不能补证", nil)
		}
		nextID := newID()
		lineage := avoidance(v.Editors, v.Lineage)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_versions (id, report_id, number, status, body, body_hash, editors, lineage, source_version_id)
			VALUES ($1,$2,$3,'draft',$4,$5,'[]',$6::jsonb,$7)`,
			nextID, reportID, v.Number+1, v.Body, v.BodyHash, idsToJSON(lineage), v.ID); err != nil {
			return err
		}
		out = ActionResult{OK: true, Status: "draft", VersionID: v.ID, SuccessorID: nextID, Message: "已打开补证草稿。已签发文件的字节没有变。"}
		return nil
	})
	return out, err
}

type DelegationInput struct {
	ProjectID string    `json:"project_id"`
	VersionID string    `json:"version_id"`
	BodyHash  string    `json:"body_hash"`
	Action    string    `json:"action"`
	GranteeID string    `json:"grantee_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) GrantDelegation(ctx context.Context, a Actor, in DelegationInput) (string, error) {
	if !a.Platform() {
		return "", fail(404, "not_found", "找不到", nil)
	}
	if in.Action != "issue" {
		return "", fail(400, "bad_action", "限时委派的动作只能是签发", nil)
	}
	if in.ExpiresAt.Before(s.now()) {
		return "", fail(400, "expired", "到期时间要晚于现在", nil)
	}
	id := newID()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		v, err := s.loadVersion(ctx, tx, in.VersionID)
		if err != nil {
			return err
		}
		if v.ProjectID != in.ProjectID {
			return fail(400, "project_mismatch", "委派必须绑在这份报告所在的项目上", nil)
		}
		if v.Status != "draft" && v.Status != "in_review" {
			return fail(409, "not_open", "只能委派当前草稿或待复核版本", nil)
		}
		if v.BodyHash != in.BodyHash {
			return fail(409, "hash_mismatch", "委派哈希和当前正文不一致。草稿改过之后，旧委派不能签新内容。请按现在的正文重新委派。", nil)
		}
		var dir, employment string
		if err := tx.QueryRowContext(ctx, `SELECT directory_status, employment FROM ent_users WHERE id=$1`, in.GranteeID).Scan(&dir, &employment); err != nil {
			return fail(404, "not_found", "找不到受委人", nil)
		}
		if dir != "verified" || employment != "active" {
			return fail(409, "grantee_inactive", "受委人不是在职的已核验身份", nil)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_delegations (id, project_id, version_id, body_hash, action, grantee_id, granted_by, expires_at)
			VALUES ($1,$2,$3,$4,'issue',$5,$6,$7)`, id, in.ProjectID, in.VersionID, in.BodyHash, in.GranteeID, a.ID, in.ExpiresAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, in.ProjectID, a.ID, "delegation_granted", "限时委派", "已授予限时签发委派", "version", in.VersionID, in.BodyHash, &v.Number)
	})
	return id, err
}

func (s *Service) GrantAuditDelegation(ctx context.Context, a Actor, projectID, granteeID string, expires time.Time) (string, error) {
	if !a.Platform() {
		return "", fail(404, "not_found", "找不到", nil)
	}
	id := newID()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO ent_audit_delegations (id, project_id, grantee_id, granted_by, expires_at)
		VALUES ($1,$2,$3,$4,$5)`, id, projectID, granteeID, a.ID, expires)
	return id, err
}
