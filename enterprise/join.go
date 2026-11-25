package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SubmitJoin 按申请人内部 ID 和规范化项目编号复用未结束申请。
func (s *Service) SubmitJoin(ctx context.Context, a Actor, code, purpose string) (Receipt, error) {
	if a.Gate() != "ready" {
		return Receipt{}, fail(403, "not_joinable", "当前身份不能请求加入项目", nil)
	}
	code = normalizeCode(code)
	text := Sanitize(purpose)
	var rc Receipt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, a.ID+"|"+code); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		var existing Receipt
		var removed bool
		err := tx.QueryRowContext(ctx, `
			SELECT id, receipt_no, project_code, status, purpose_display, credential_removed
			FROM ent_join_requests
			WHERE applicant_id=$1 AND project_code=$2 AND status='pending'`, a.ID, code).
			Scan(&existing.ID, &existing.ReceiptNo, &existing.ProjectCode, &existing.Status, &existing.Purpose, &removed)
		if err == nil {
			existing.StatusLabel = joinStatusLabel(existing.Status)
			existing.Reused = true
			if removed {
				existing.CredentialNotice = CredentialNotice
			}
			rc = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("select existing: %w", err)
		}
		var projectID sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT id FROM ent_projects WHERE code=$1 AND status='active'`, code).Scan(&projectID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("project: %w", err)
		}
		rc = Receipt{
			ID: newID(), ReceiptNo: randomReceipt(), ProjectCode: code, Status: "pending",
			StatusLabel: "待处理", Purpose: text.Display, CredentialNotice: notice(text.Removed),
		}
		var pid any
		if projectID.Valid {
			pid = projectID.String
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_join_requests (
				id, receipt_no, applicant_id, project_code, project_id, purpose_display, purpose_sealed, credential_removed, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending')`,
			rc.ID, rc.ReceiptNo, a.ID, code, pid, text.Display, text.Sealed, text.Removed); err != nil {
			return fmt.Errorf("insert request: %w", err)
		}
		if projectID.Valid {
			rows, err := tx.QueryContext(ctx, `SELECT user_id FROM ent_members WHERE project_id=$1 AND role='owner'`, projectID.String)
			if err != nil {
				return fmt.Errorf("owners: %w", err)
			}
			var owners []string
			for rows.Next() {
				var owner string
				if err := rows.Scan(&owner); err != nil {
					rows.Close()
					return err
				}
				owners = append(owners, owner)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, owner := range owners {
				if err := s.openTodo(ctx, tx, "join_request", projectID.String, owner, "join", rc.ID, "加入请求"); err != nil {
					return fmt.Errorf("todo: %w", err)
				}
			}
		} else {
			admins, err := s.platformAdmins(ctx, tx)
			if err != nil {
				return err
			}
			for _, admin := range admins {
				if err := s.openTodo(ctx, tx, "join_request", "", admin, "join", rc.ID, "加入请求"); err != nil {
					return err
				}
			}
		}
		if err := s.audit(ctx, tx, projectID.String, a.ID, "join_requested", "加入请求", "加入请求 "+rc.ReceiptNo, "join", rc.ID, "", nil); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		if err := s.notify(ctx, tx, "join_requested"); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
		return nil
	})
	return rc, err
}

func notice(removed bool) string {
	if removed {
		return CredentialNotice
	}
	return ""
}

func (s *Service) platformAdmins(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM ent_users WHERE platform_role='admin' AND directory_status='verified' AND employment='active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) GetJoin(ctx context.Context, a Actor, id string) (Receipt, error) {
	var rc Receipt
	var applicant, decidedBy, purpose, sealed string
	var removed bool
	var projectID sql.NullString
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, receipt_no, applicant_id, project_code, project_id, status, purpose_display, purpose_sealed,
		       credential_removed, structured_reason, decided_by
		FROM ent_join_requests WHERE id=$1`, id).
		Scan(&rc.ID, &rc.ReceiptNo, &applicant, &rc.ProjectCode, &projectID, &rc.Status, &purpose, &sealed, &removed, &rc.StructuredReason, &decidedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return Receipt{}, err
	}
	allowed := applicant == a.ID || decidedBy == a.ID
	if !allowed && projectID.Valid {
		ok, err := s.isOwner(ctx, projectID.String, a.ID)
		if err != nil {
			return Receipt{}, err
		}
		allowed = ok
	}
	if !allowed && !projectID.Valid && a.Platform() {
		allowed = true
	}
	if !allowed {
		return Receipt{}, fail(404, "not_found", "找不到", nil)
	}
	rc.StatusLabel = joinStatusLabel(rc.Status)
	rc.Purpose = purpose
	if removed {
		rc.CredentialNotice = CredentialNotice
		if strings.Contains(rc.Purpose, DemoSecret) || strings.Contains(sealed, DemoSecret) && strings.Contains(rc.Purpose, DemoSecret) {
			return Receipt{}, fail(500, "redaction_failed", "凭据过滤失败", nil)
		}
	}
	if rc.Status == "approved" && projectID.Valid && applicant == a.ID {
		rc.ProjectID = projectID.String
		_ = s.DB.QueryRowContext(ctx, `SELECT name FROM ent_projects WHERE id=$1`, projectID.String).Scan(&rc.ProjectName)
	}
	return rc, nil
}

func (s *Service) isOwner(ctx context.Context, projectID, userID string) (bool, error) {
	var role string
	err := s.DB.QueryRowContext(ctx, `SELECT role FROM ent_members WHERE project_id=$1 AND user_id=$2`, projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return role == "owner", err
}

// DecideJoin 在同一事务里再次检查申请人、项目和处理人授权。批准和拒绝只有一个结果。
func (s *Service) DecideJoin(ctx context.Context, a Actor, id, decision, role string) (Receipt, error) {
	if decision != "approve" && decision != "reject" {
		return Receipt{}, fail(400, "bad_decision", "只能批准或拒绝", nil)
	}
	if decision == "approve" && role != "member" && role != "reader" && role != "owner" {
		role = "member"
	}
	var rc Receipt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var applicant, status, code string
		var projectID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT applicant_id, status, project_code, project_id
			FROM ent_join_requests WHERE id=$1 FOR UPDATE`, id).Scan(&applicant, &status, &code, &projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "not_found", "找不到", nil)
		}
		if err != nil {
			return err
		}
		if status != "pending" {
			rc, err = s.GetJoin(ctx, a, id)
			if err != nil {
				return err
			}
			rc.Reused = true
			return nil
		}
		if !projectID.Valid {
			if !a.Platform() {
				return fail(403, "not_authorized", "这张回执目前由平台管理员处理", nil)
			}
			return fail(409, "unroutable", "这个编号当前不能加入。请拒绝为无法受理。", nil)
		}
		var approverRole string
		err = tx.QueryRowContext(ctx, `SELECT role FROM ent_members WHERE project_id=$1 AND user_id=$2`, projectID.String, a.ID).Scan(&approverRole)
		if errors.Is(err, sql.ErrNoRows) || approverRole != "owner" {
			return fail(403, "not_authorized", "你现在没有批准加入的授权。请让仍有审批职责的负责人处理。", nil)
		}
		var duty string
		err = tx.QueryRowContext(ctx, `SELECT duty FROM ent_duties WHERE project_id=$1 AND user_id=$2 AND duty='approve'`, projectID.String, a.ID).Scan(&duty)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(403, "not_authorized", "你现在没有批准加入的授权。请让仍有审批职责的负责人处理。", nil)
		}
		if err != nil {
			return err
		}
		var projectStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM ent_projects WHERE id=$1`, projectID.String).Scan(&projectStatus); err != nil {
			return err
		}
		if projectStatus != "active" {
			return fail(409, "project_closed", "这个项目当前不能加入", nil)
		}
		if decision == "reject" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE ent_join_requests
				SET status='rejected', structured_reason='未获批准', decided_by=$2, decided_at=$3
				WHERE id=$1 AND status='pending'`, id, a.ID, s.now()); err != nil {
				return err
			}
			if err := s.closeTodos(ctx, tx, id, "join_request"); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, projectID.String, a.ID, "join_rejected", "未获批准", "加入请求已拒绝", "join", id, "", nil); err != nil {
				return err
			}
			return s.notify(ctx, tx, "join_rejected")
		}
		var dir, employment string
		if err := tx.QueryRowContext(ctx, `SELECT directory_status, employment FROM ent_users WHERE id=$1 FOR UPDATE`, applicant).Scan(&dir, &employment); err != nil {
			return err
		}
		if dir != "verified" || employment != "active" {
			return fail(409, "applicant_not_active", "申请人当前不是在职的已核验身份，不能加入项目", nil)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE ent_join_requests
			SET status='approved', role_granted=$2, decided_by=$3, decided_at=$4, structured_reason='已批准'
			WHERE id=$1 AND status='pending'`, id, role, a.ID, s.now())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ent_members (project_id, user_id, role) VALUES ($1,$2,$3)
				ON CONFLICT (project_id, user_id) DO NOTHING`, projectID.String, applicant, role); err != nil {
				return err
			}
			if err := s.closeTodos(ctx, tx, id, "join_request"); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, projectID.String, a.ID, "member_added", "添加成员", "添加成员", "member", applicant, "", nil); err != nil {
				return err
			}
			if err := s.notify(ctx, tx, "member_added"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if rc.ID != "" {
		return rc, nil
	}
	return s.GetJoin(ctx, a, id)
}

type MemberInput struct {
	UserID string   `json:"user_id"`
	Role   string   `json:"role"`
	Post   string   `json:"post"`
	Duties []string `json:"duties"`
}

func (s *Service) AddMember(ctx context.Context, a Actor, projectID string, in MemberInput) error {
	if in.Role != "owner" && in.Role != "member" && in.Role != "reader" && in.Role != "leader" {
		return fail(400, "bad_role", "基础角色只能是负责人、成员、只读或领导", nil)
	}
	if in.Role == "leader" {
		in.Post = "leader"
		if !hasDuty(in.Duties, "lead") {
			in.Duties = append(in.Duties, "lead")
		}
	}
	if in.Post != "" && in.Post != "employee" && in.Post != "reviewer" && in.Post != "leader" && in.Post != "reader" {
		return fail(400, "bad_post", "岗位只能是员工、复核人、领导或只读", nil)
	}
	allowedDuty := map[string]bool{"approve": true, "review": true, "audit": true, "lead": true}
	for _, d := range in.Duties {
		if !allowedDuty[d] {
			return fail(400, "bad_duty", "不能授予这项职责", nil)
		}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.requireOwnerDuty(ctx, tx, projectID, a.ID); err != nil {
			return err
		}
		var dir, employment, name string
		err := tx.QueryRowContext(ctx, `SELECT display_name, directory_status, employment FROM ent_users WHERE id=$1`, in.UserID).Scan(&name, &dir, &employment)
		if errors.Is(err, sql.ErrNoRows) || dir == "pending_bind" {
			return fail(404, "not_found", "成员列表里没有这个未核验的人，不能填写邮箱硬加", nil)
		}
		if err != nil {
			return err
		}
		if dir != "verified" || employment != "active" {
			return fail(409, "user_disabled", name+" 已停用，不能加入项目。要由企业目录恢复账号。", nil)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_members (project_id, user_id, role, post) VALUES ($1,$2,$3,$4)
			ON CONFLICT (project_id, user_id) DO UPDATE SET
				role=EXCLUDED.role,
				post=CASE WHEN EXCLUDED.post<>'' THEN EXCLUDED.post ELSE ent_members.post END`,
			projectID, in.UserID, in.Role, in.Post); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ent_duties WHERE project_id=$1 AND user_id=$2`, projectID, in.UserID); err != nil {
			return err
		}
		for _, d := range in.Duties {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ent_duties (id, project_id, user_id, duty, granted_by, grant_ref)
				VALUES ($1,$2,$3,$4,$5,'member-page')`, newID(), projectID, in.UserID, d, a.ID); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, projectID, a.ID, "member_added", "添加成员", "添加成员 "+name, "member", in.UserID, "", nil)
	})
}

func (s *Service) requireOwnerDuty(ctx context.Context, tx *sql.Tx, projectID, userID string) error {
	var role string
	err := tx.QueryRowContext(ctx, `SELECT role FROM ent_members WHERE project_id=$1 AND user_id=$2`, projectID, userID).Scan(&role)
	if err != nil || role != "owner" {
		return fail(404, "not_found", "找不到", nil)
	}
	var duty string
	err = tx.QueryRowContext(ctx, `SELECT duty FROM ent_duties WHERE project_id=$1 AND user_id=$2 AND duty='approve'`, projectID, userID).Scan(&duty)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(403, "not_authorized", "你现在没有审批职责，不能添加成员", nil)
	}
	return err
}

type DirectoryHit struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Addable bool   `json:"addable"`
	Reason  string `json:"reason,omitempty"`
}

func (s *Service) SearchDirectory(ctx context.Context, a Actor, projectID, q string) ([]DirectoryHit, error) {
	if err := s.tx(ctx, func(tx *sql.Tx) error { return s.requireOwnerDuty(ctx, tx, projectID, a.ID) }); err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, display_name, directory_status, employment
		FROM ent_users
		WHERE platform_role='' AND directory_status IN ('verified','disabled')
		  AND ($1='' OR display_name ILIKE '%' || $1 || '%')
		ORDER BY display_name LIMIT 20`, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DirectoryHit
	for rows.Next() {
		var hit DirectoryHit
		var dir, employment string
		if err := rows.Scan(&hit.ID, &hit.Name, &dir, &employment); err != nil {
			return nil, err
		}
		if dir == "verified" && employment == "active" {
			hit.Addable = true
		} else {
			hit.Reason = "已停用，要由企业目录恢复账号"
		}
		out = append(out, hit)
	}
	if out == nil {
		out = []DirectoryHit{}
	}
	return out, rows.Err()
}

type MemberRow struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Duties []string `json:"duties"`
}

func (s *Service) ListMembers(ctx context.Context, a Actor, projectID string) ([]MemberRow, error) {
	if _, err := s.requireMember(ctx, projectID, a.ID); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT u.id, u.display_name, m.role
		FROM ent_members m JOIN ent_users u ON u.id=m.user_id
		WHERE m.project_id=$1 ORDER BY u.display_name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberRow
	for rows.Next() {
		var row MemberRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Role); err != nil {
			return nil, err
		}
		row.Duties, err = s.duties(ctx, projectID, row.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type access struct {
	Role   string
	Post   string
	Duties []string
}

func (s *Service) requireMember(ctx context.Context, projectID, userID string) (access, error) {
	var role, stored string
	err := s.DB.QueryRowContext(ctx, `
		SELECT m.role, m.post FROM ent_members m
		JOIN ent_projects p ON p.id=m.project_id
		WHERE m.project_id=$1 AND m.user_id=$2 AND p.status='active'`, projectID, userID).Scan(&role, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return access{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return access{}, err
	}
	duties, err := s.duties(ctx, projectID, userID)
	if err != nil {
		return access{}, err
	}
	return access{Role: role, Post: effectivePost(role, duties, stored), Duties: duties}, nil
}

func hasDuty(duties []string, duty string) bool {
	return hasID(duties, duty)
}

func (s *Service) projectByCode(ctx context.Context, code string) (id, name, status string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT id, name, status FROM ent_projects WHERE code=$1`, normalizeCode(code)).Scan(&id, &name, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", fail(404, "not_found", "找不到", nil)
	}
	return id, name, status, err
}

func (s *Service) ProjectByID(ctx context.Context, a Actor, projectID string) (ProjectBrief, error) {
	acc, err := s.requireMember(ctx, projectID, a.ID)
	if err != nil {
		return ProjectBrief{}, err
	}
	var p ProjectBrief
	if err := s.DB.QueryRowContext(ctx, `SELECT id, code, name FROM ent_projects WHERE id=$1`, projectID).Scan(&p.ID, &p.Code, &p.Name); err != nil {
		return ProjectBrief{}, err
	}
	p.Role = acc.Role
	p.Duties = acc.Duties
	return p, nil
}

func fmtNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "、")
}

func (s *Service) names(ctx context.Context, ids []string) []string {
	out := []string{}
	for _, id := range ids {
		out = append(out, s.nameOf(ctx, nil, id))
	}
	return out
}

// Ensure the unused import stays honest if fmt is needed by callers in this file.
var _ = fmt.Sprintf
