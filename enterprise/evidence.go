package enterprise

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type EvidenceRequest struct {
	ID               string `json:"id"`
	ReportID         string `json:"report_id"`
	Status           string `json:"status"`
	StatusLabel      string `json:"status_label"`
	Purpose          string `json:"purpose,omitempty"`
	Reason           string `json:"reason,omitempty"`
	CredentialNotice string `json:"credential_notice,omitempty"`
	Previewable      bool   `json:"previewable"`
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
		acc, err := s.requireMember(ctx, projectID, a.ID)
		if err != nil {
			return err
		}
		if readOnlyRole(acc.Role) || acc.Post == "leader" {
			return fail(403, "reader", "当前岗位不能申请原件", nil)
		}
		var originalID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM ent_originals WHERE finding_id=$1`, findingID).Scan(&originalID); err != nil {
			return fail(404, "not_found", "这条发现没有需要授权的原件", nil)
		}
		out = EvidenceRequest{ID: newID(), ReportID: reportID, Status: "pending", StatusLabel: "待审批", Purpose: text.Display, CredentialNotice: notice(text.Removed), Previewable: false}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_evidence_requests (
				id, project_id, report_id, finding_id, original_id, applicant_id, purpose_display, purpose_sealed, purpose_credential_removed, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending')`,
			out.ID, projectID, reportID, findingID, originalID, a.ID, text.Display, text.Sealed, text.Removed); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT user_id FROM ent_duties WHERE project_id=$1 AND duty='review'`, projectID)
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

func (s *Service) RejectOriginal(ctx context.Context, a Actor, requestID, reason string) (EvidenceRequest, error) {
	text := Sanitize(reason)
	if strings.TrimSpace(reason) == "" || text.Display == "" {
		return EvidenceRequest{}, fail(400, "reason_required", "拒绝理由必填。请写明为什么不查看原件，再提交。", nil)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, status string
		err := tx.QueryRowContext(ctx, `SELECT project_id, status FROM ent_evidence_requests WHERE id=$1 FOR UPDATE`, requestID).Scan(&projectID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "not_found", "找不到", nil)
		}
		if err != nil {
			return err
		}
		if status != "pending" {
			return nil
		}
		acc, err := s.requireMember(ctx, projectID, a.ID)
		if err != nil {
			return err
		}
		if !hasDuty(acc.Duties, "review") && !hasDuty(acc.Duties, "approve") {
			return fail(403, "not_reviewer", "你没有处理原件申请的职责", nil)
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
		if err := s.audit(ctx, tx, projectID, a.ID, "original_rejected", "原件申请被拒绝", "原件申请被拒绝", "evidence", requestID, "", nil); err != nil {
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
	var applicant, decided, purpose, reason, projectID string
	var removed bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, report_id, project_id, applicant_id, status, purpose_display, reason_display, reason_credential_removed, decided_by
		FROM ent_evidence_requests WHERE id=$1`, requestID).
		Scan(&out.ID, &out.ReportID, &projectID, &applicant, &out.Status, &purpose, &reason, &removed, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceRequest{}, fail(404, "not_found", "找不到", nil)
	}
	if err != nil {
		return EvidenceRequest{}, err
	}
	owner, err := s.isOwner(ctx, projectID, a.ID)
	if err != nil {
		return EvidenceRequest{}, err
	}
	handler := false
	if !owner && a.ID != applicant && a.ID != decided {
		if acc, err := s.requireMember(ctx, projectID, a.ID); err == nil {
			handler = hasDuty(acc.Duties, "review") || hasDuty(acc.Duties, "approve")
		}
	}
	if a.ID != applicant && a.ID != decided && !owner && !handler {
		return EvidenceRequest{}, fail(404, "not_found", "找不到", nil)
	}
	out.StatusLabel = evidenceLabel(out.Status)
	out.Purpose = purpose
	out.Reason = reason
	out.Previewable = false
	if removed || strings.Contains(reason, DemoSecret) || strings.Contains(purpose, DemoSecret) {
		out.Reason = strings.ReplaceAll(out.Reason, DemoSecret, redactedMark)
		out.Purpose = strings.ReplaceAll(out.Purpose, DemoSecret, redactedMark)
		out.CredentialNotice = CredentialNotice
	}
	if strings.Contains(out.Reason+out.Purpose, DemoSecret) {
		return EvidenceRequest{}, fail(500, "redaction_failed", "原单仍包含凭据", nil)
	}
	return out, nil
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
