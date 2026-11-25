package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

const (
	ViewOriginal     = "view_original"
	ViewCredential   = "view_credential"
	originalGrantTTL = 8 * time.Hour
)

type EvidenceRequest struct {
	ID                 string          `json:"id"`
	ReportID           string          `json:"report_id"`
	Status             string          `json:"status"`
	StatusLabel        string          `json:"status_label"`
	Purpose            string          `json:"purpose,omitempty"`
	Reason             string          `json:"reason,omitempty"`
	CredentialNotice   string          `json:"credential_notice,omitempty"`
	Previewable        bool            `json:"previewable"`
	OriginalID         string          `json:"original_id,omitempty"`
	OriginalFilename   string          `json:"original_filename,omitempty"`
	RequiresCredential bool            `json:"requires_credential"`
	CanApprove         bool            `json:"can_approve"`
	CanReject          bool            `json:"can_reject"`
	CanRevoke          bool            `json:"can_revoke"`
	Grants             []OriginalGrant `json:"grants,omitempty"`
}

type OriginalGrant struct {
	Action    string    `json:"action"`
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`
	requestID string
}

type OriginalApproval struct {
	Actions   []string  `json:"actions"`
	ExpiresAt time.Time `json:"expires_at"`
}

type evidenceRecord struct {
	EvidenceRequest
	projectID, originalID, filename, applicant, decidedBy string
	purposeRemoved, reasonRemoved, originalPresent        bool
}

func (s *Service) RequestOriginal(ctx context.Context, a Actor, reportID, purpose string) (EvidenceRequest, error) {
	text := Sanitize(purpose)
	if text.Display == "" {
		return EvidenceRequest{}, fail(400, "purpose_required", "请填写用途后再提交。例如：核对演示发现。", nil)
	}
	var out EvidenceRequest
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var findingID int64
		err := tx.QueryRowContext(ctx, `
			SELECT r.project_id, s.finding_id FROM ent_reports r
			JOIN ent_report_sources s ON s.report_id=r.id WHERE r.id=$1 LIMIT 1`, reportID).Scan(&projectID, &findingID)
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
		if acc.Role == "" {
			return fail(404, "not_found", "找不到", nil)
		}
		if !canHoldOriginal(acc) {
			return fail(403, "reader", "当前岗位不能申请原件", nil)
		}
		var originalID string
		var requiresCredential bool
		err = tx.QueryRowContext(ctx, `SELECT id, contains_credentials FROM ent_originals
			WHERE finding_id=$1 AND project_id=$2 ORDER BY created_at, id LIMIT 1`, findingID, projectID).
			Scan(&originalID, &requiresCredential)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "not_found", "这条发现没有需要授权的原件", nil)
		}
		if err != nil {
			return err
		}
		out = EvidenceRequest{ID: newID(), ReportID: reportID, Status: "pending", StatusLabel: "待审批", Purpose: text.Display,
			CredentialNotice: notice(text.Removed), RequiresCredential: requiresCredential, CanReject: canDecideOriginal(acc)}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_evidence_requests (
				id, project_id, report_id, finding_id, original_id, applicant_id, purpose_display, purpose_sealed, purpose_credential_removed, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending')`,
			out.ID, projectID, reportID, findingID, originalID, a.ID, text.Display, text.Sealed, text.Removed); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT m.user_id FROM ent_members m
			JOIN ent_users u ON u.id=m.user_id
			JOIN ent_duties d ON d.project_id=m.project_id AND d.user_id=m.user_id
			WHERE m.project_id=$1 AND m.user_id<>$2 AND d.duty IN ('review','approve')
			  AND u.directory_status='verified' AND u.employment='active'`, projectID, a.ID)
		if err != nil {
			return err
		}
		var reviewers []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			reviewers = append(reviewers, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range reviewers {
			if err := s.openTodo(ctx, tx, "original_request", projectID, id, "evidence", out.ID, "原件申请"); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, tx, projectID, a.ID, "original_requested", "原件申请", "原件申请", "evidence", out.ID, "", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, "original_requested", "有一张原件申请。请登录后查看待办。")
	})
	return out, err
}

func (s *Service) ApproveOriginal(ctx context.Context, a Actor, requestID string, in OriginalApproval) (EvidenceRequest, error) {
	actions := make([]string, 0, 2)
	for _, action := range in.Actions {
		if action != ViewOriginal && action != ViewCredential {
			return EvidenceRequest{}, fail(400, "bad_action", "请选择查看原件或查看凭据", nil)
		}
		if !slices.Contains(actions, action) {
			actions = append(actions, action)
		}
	}
	if len(actions) == 0 {
		return EvidenceRequest{}, fail(400, "bad_action", "至少选择一项授权", nil)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadEvidence(ctx, tx, requestID, true)
		if err != nil {
			return err
		}
		if r.applicant == a.ID {
			return fail(403, "self_approval", "不能批准自己的原件申请", nil)
		}
		acc, err := originalMember(ctx, tx, r.projectID, a.ID)
		if err != nil {
			return err
		}
		if !canHoldOriginal(acc) || !canDecideOriginal(acc) {
			return fail(403, "not_reviewer", "你没有批准原件申请的职责", nil)
		}
		if r.Status != "pending" {
			return nil
		}
		applicant, err := originalMember(ctx, tx, r.projectID, r.applicant)
		if err != nil {
			return err
		}
		if !canHoldOriginal(applicant) {
			return fail(409, "applicant_ineligible", "申请人当前不能获得原件授权", nil)
		}
		var originalID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM ent_originals WHERE id=$1 AND project_id=$2 FOR SHARE`, r.originalID, r.projectID).Scan(&originalID)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "not_found", "找不到原件", nil)
		}
		if err != nil {
			return err
		}
		now := s.now()
		expires := in.ExpiresAt
		if expires.IsZero() {
			expires = now.Add(originalGrantTTL)
		}
		if !expires.After(now) || expires.After(now.Add(originalGrantTTL)) {
			return fail(400, "bad_expiry", "到期时间须在未来 8 小时内", nil)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ent_evidence_requests
			SET status='approved', decided_by=$2, decided_at=$3 WHERE id=$1`, r.ID, a.ID, now); err != nil {
			return err
		}
		for _, action := range actions {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ent_original_grants (id, request_id, original_id, user_id, action, expires_at)
				VALUES ($1,$2,$3,$4,$5,$6)`, newID(), r.ID, originalID, r.applicant, action, expires); err != nil {
				return err
			}
		}
		if err := s.closeTodos(ctx, tx, r.ID, "original_request"); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, r.projectID, a.ID, "original_approved", "原件申请已批准", "原件申请已批准", "evidence", r.ID, "", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, "original_approved", "有一张原件申请已处理。请登录后查看。")
	})
	if err != nil {
		return EvidenceRequest{}, err
	}
	return s.GetEvidence(ctx, a, requestID)
}

func (s *Service) RevokeOriginal(ctx context.Context, a Actor, requestID string) (EvidenceRequest, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadEvidence(ctx, tx, requestID, true)
		if err != nil {
			return err
		}
		acc, err := originalMember(ctx, tx, r.projectID, a.ID)
		if err != nil {
			return err
		}
		if !canHoldOriginal(acc) || !canDecideOriginal(acc) {
			return fail(403, "not_reviewer", "你没有撤销原件授权的职责", nil)
		}
		if r.Status != "approved" {
			return fail(409, "not_approved", "该申请尚未获得授权", nil)
		}
		result, err := tx.ExecContext(ctx, `WITH active AS (
			SELECT id FROM ent_original_grants WHERE request_id=$1 AND revoked_at IS NULL AND expires_at>$2
			ORDER BY action, expires_at, id FOR UPDATE
		) UPDATE ent_original_grants g SET revoked_at=$2 FROM active WHERE g.id=active.id`, r.ID, s.now())
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		return s.audit(ctx, tx, r.projectID, a.ID, "original_revoked", "原件授权已撤销", "原件授权已撤销", "evidence", r.ID, "", nil)
	})
	if err != nil {
		return EvidenceRequest{}, err
	}
	return s.GetEvidence(ctx, a, requestID)
}

func (s *Service) RejectOriginal(ctx context.Context, a Actor, requestID, reason string) (EvidenceRequest, error) {
	text := Sanitize(reason)
	if strings.TrimSpace(reason) == "" || text.Display == "" {
		return EvidenceRequest{}, fail(400, "reason_required", "拒绝理由必填。请写明为什么不查看原件，再提交。", nil)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadEvidence(ctx, tx, requestID, true)
		if err != nil {
			return err
		}
		acc, err := originalMember(ctx, tx, r.projectID, a.ID)
		if err != nil {
			return err
		}
		if !canDecideOriginal(acc) {
			return fail(403, "not_reviewer", "你没有处理原件申请的职责", nil)
		}
		if r.Status != "pending" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE ent_evidence_requests
			SET status='rejected', reason_display=$2, reason_sealed=$3, reason_credential_removed=$4, decided_by=$5, decided_at=$6
			WHERE id=$1 AND status='pending'`, requestID, text.Display, text.Sealed, text.Removed, a.ID, s.now()); err != nil {
			return err
		}
		if err := s.closeTodos(ctx, tx, requestID, "original_request"); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, r.projectID, a.ID, "original_rejected", "原件申请被拒绝", "原件申请被拒绝", "evidence", requestID, "", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, "original_rejected", "有一张原件申请已处理。请登录后查看待办。")
	})
	if err != nil {
		return EvidenceRequest{}, err
	}
	return s.GetEvidence(ctx, a, requestID)
}

func (s *Service) GetEvidence(ctx context.Context, a Actor, requestID string) (EvidenceRequest, error) {
	var out EvidenceRequest
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := loadEvidence(ctx, tx, requestID, false)
		if err != nil {
			return err
		}
		acc, err := originalMember(ctx, tx, r.projectID, a.ID)
		if err != nil {
			return err
		}
		handler := canDecideOriginal(acc)
		if a.ID != r.applicant && a.ID != r.decidedBy && acc.Role != "owner" && !handler {
			return fail(404, "not_found", "找不到", nil)
		}
		out = r.EvidenceRequest
		out.StatusLabel = evidenceLabel(out.Status)
		purpose, reason := Sanitize(out.Purpose), Sanitize(out.Reason)
		out.Purpose, out.Reason = purpose.Display, reason.Display
		out.CredentialNotice = notice(r.purposeRemoved || r.reasonRemoved || purpose.Removed || reason.Removed)
		out.CanReject = out.Status == "pending" && handler
		out.CanApprove = out.CanReject && canHoldOriginal(acc) && a.ID != r.applicant
		if out.Status != "approved" {
			return nil
		}
		grants, err := loadOriginalGrants(ctx, tx, r.originalID, r.applicant, r.ID, s.now())
		if err != nil {
			return err
		}
		now := s.now()
		for _, grant := range grants {
			if grant.requestID == r.ID {
				out.Grants = append(out.Grants, grant)
				out.CanRevoke = out.CanRevoke || grant.active(now) && handler && canHoldOriginal(acc)
			}
		}
		out.Previewable = r.originalPresent && a.ID == r.applicant && canHoldOriginal(acc) && hasOriginalGrants(grants, out.RequiresCredential, now)
		if out.Previewable {
			out.OriginalID = r.originalID
			out.OriginalFilename = r.filename
		}
		return nil
	})
	return out, err
}

func loadEvidence(ctx context.Context, tx *sql.Tx, requestID string, lock bool) (evidenceRecord, error) {
	var r evidenceRecord
	query := `SELECT e.id, e.report_id, e.project_id, e.original_id, e.applicant_id, e.status, e.purpose_display,
		e.reason_display, e.purpose_credential_removed, e.reason_credential_removed, e.decided_by,
		o.id IS NOT NULL, COALESCE(o.filename, ''), COALESCE(o.contains_credentials, true)
		FROM ent_evidence_requests e LEFT JOIN ent_originals o ON o.id=e.original_id AND o.project_id=e.project_id
		WHERE e.id=$1`
	if lock {
		query += ` FOR UPDATE OF e`
	}
	err := tx.QueryRowContext(ctx, query, requestID).
		Scan(&r.ID, &r.ReportID, &r.projectID, &r.originalID, &r.applicant, &r.Status, &r.Purpose,
			&r.Reason, &r.purposeRemoved, &r.reasonRemoved, &r.decidedBy, &r.originalPresent, &r.filename, &r.RequiresCredential)
	if errors.Is(err, sql.ErrNoRows) {
		return evidenceRecord{}, fail(404, "not_found", "找不到", nil)
	}
	return r, err
}

func originalMember(ctx context.Context, tx *sql.Tx, projectID, userID string) (access, error) {
	var acc access
	var stored, duties string
	// Hold the eligibility rows until the grant or read audit commits.
	err := tx.QueryRowContext(ctx, `SELECT m.role, m.post FROM ent_members m
		JOIN ent_projects p ON p.id=m.project_id JOIN ent_users u ON u.id=m.user_id
		WHERE m.project_id=$1 AND m.user_id=$2 AND p.status='active'
		  AND u.directory_status='verified' AND u.employment='active'
		FOR SHARE OF m, p, u`, projectID, userID).Scan(&acc.Role, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return access{}, nil
	}
	if err != nil {
		return access{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(string_agg(duty, ' '), '') FROM ent_duties
		WHERE project_id=$1 AND user_id=$2`, projectID, userID).Scan(&duties)
	acc.Duties = strings.Fields(duties)
	acc.Post = effectivePost(acc.Role, acc.Duties, stored)
	return acc, err
}

func canHoldOriginal(acc access) bool {
	return acc.Role != "" && !readOnlyRole(acc.Role) && acc.Post != "reader" && acc.Post != "leader"
}

func canDecideOriginal(acc access) bool {
	return hasDuty(acc.Duties, "review") || hasDuty(acc.Duties, "approve")
}

func loadOriginalGrants(ctx context.Context, tx *sql.Tx, originalID, userID, requestID string, now time.Time) ([]OriginalGrant, error) {
	query := `SELECT request_id, action, expires_at, revoked_at IS NOT NULL
		FROM ent_original_grants WHERE original_id=$1 AND user_id=$2
		  AND (request_id=$3 OR (revoked_at IS NULL AND expires_at>$4))
		ORDER BY action, expires_at, id`
	if requestID == "" {
		query += ` FOR SHARE`
	}
	rows, err := tx.QueryContext(ctx, query, originalID, userID, requestID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []OriginalGrant
	for rows.Next() {
		var grant OriginalGrant
		var revoked bool
		if err := rows.Scan(&grant.requestID, &grant.Action, &grant.ExpiresAt, &revoked); err != nil {
			return nil, err
		}
		grant.Status = "active"
		if revoked {
			grant.Status = "revoked"
		} else if !grant.ExpiresAt.After(now) {
			grant.Status = "expired"
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (g OriginalGrant) active(now time.Time) bool {
	return g.Status == "active" && g.ExpiresAt.After(now)
}

func hasOriginalGrants(grants []OriginalGrant, requiresCredential bool, now time.Time) bool {
	var view, credential bool
	for _, grant := range grants {
		if grant.active(now) {
			view = view || grant.Action == ViewOriginal
			credential = credential || grant.Action == ViewCredential
		}
	}
	return view && (!requiresCredential || credential)
}

func evidenceLabel(status string) string {
	switch status {
	case "pending":
		return "待审批"
	case "rejected":
		return "已拒绝"
	case "approved":
		return "已批准"
	default:
		return status
	}
}
