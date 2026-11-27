package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// InterceptContextEntry is a bounded, recorded session event, not model reasoning.
type InterceptContextEntry struct {
	Kind      string `json:"kind"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Text      string `json:"text"`
	IsError   bool   `json:"is_error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// InterceptAudit is captured at review time. It is deliberately excluded from
// polling/list responses; old rows have no audit instead of reconstructed data.
type InterceptAudit struct {
	RunID            string                  `json:"run_id,omitempty"`
	ToolUseID        string                  `json:"tool_use_id,omitempty"`
	Correlation      string                  `json:"correlation"` // exact | ambiguous | unavailable
	InputDigest      string                  `json:"input_digest"`
	UserMessage      string                  `json:"user_message"`
	UserTruncated    bool                    `json:"user_truncated,omitempty"`
	Context          []InterceptContextEntry `json:"context"`
	ContextTruncated bool                    `json:"context_truncated,omitempty"`
	CapturedAt       time.Time               `json:"captured_at"`
	ModelFallback    bool                    `json:"model_fallback,omitempty"`
	ModelInput       json.RawMessage         `json:"model_input,omitempty"`
	ModelInputDigest string                  `json:"model_input_digest,omitempty"`
	InitialAction    string                  `json:"initial_action"`
	InitialReason    string                  `json:"initial_reason"`
	EffectiveAction  string                  `json:"effective_action,omitempty"`
	DecisionReason   string                  `json:"decision_reason,omitempty"`
	RuleName         string                  `json:"rule_name,omitempty"`
	ConfigDigest     string                  `json:"config_digest,omitempty"`
	ProfileID        int64                   `json:"profile_id,omitempty"`
	JudgeAnswers     []JudgeAnswer           `json:"judge_answers,omitempty"`
	ExecutionStatus  string                  `json:"execution_status"`
	Output           string                  `json:"output,omitempty"`
	OutputTruncated  bool                    `json:"output_truncated,omitempty"`
	ExecutionEndedAt *time.Time              `json:"execution_ended_at,omitempty"`
	// Targets is set on target-gate rows only. MergedAsk marks a target request
	// that also carries a rule or judge ask.
	Targets      []InterceptTarget `json:"targets,omitempty"`
	MergedAsk    bool              `json:"merged_ask,omitempty"`
	Approver     string            `json:"approver,omitempty"`
	ApproverAddr string            `json:"approver_addr,omitempty"`
}

// JudgeAnswer is one closed-question answer. Answer is yes, no or unknown.
type JudgeAnswer struct {
	ID       string `json:"id"`
	Answer   string `json:"answer"`
	Evidence string `json:"evidence"`
}

// InterceptTarget is one asset target shown on a target-gate approval.
type InterceptTarget struct {
	Host              string    `json:"host"`
	Port              int       `json:"port"`
	Kind              string    `json:"kind,omitempty"`
	ResolvedIPs       []string  `json:"resolved_ips"`
	Resolver          string    `json:"resolver,omitempty"`
	ResolvedAt        time.Time `json:"resolved_at"`
	PinID             int64     `json:"pin_id,omitempty"`
	PinVersion        int       `json:"pin_version,omitempty"`
	PinnedIPs         []string  `json:"pinned_ips,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	Outcome           string    `json:"outcome"`
	NameOnlyEffective bool      `json:"name_only_effective,omitempty"`
	ExemptVoid        bool      `json:"exempt_void,omitempty"`
	RedirectFrom      string    `json:"redirect_from,omitempty"`
}

type InterceptDetail struct {
	InterceptApprovalRow
	Audit *InterceptAudit `json:"audit"`
}

func (d *DB) GetInterceptDetail(id int64) (*InterceptDetail, error) {
	var out InterceptDetail
	var raw []byte
	err := d.QueryRow(approvalRowSelectWithAudit+` WHERE ip.id=$1`, id).Scan(
		&out.ID, &out.RuleID, &out.ConversationID, &out.TaskID, &out.AgentName,
		&out.ToolName, &out.ToolInput, &out.Status, &out.Reason, &out.DecidedAt, &out.CreatedAt,
		&out.DecisionSource, &out.DecidedBy, &out.ConvTitle, &out.ConvAgentKey, &out.RuleName, &raw,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.Audit); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// ResolveIntercept atomically settles a pending request. An optional source is
// persisted with the final decision; the HTTP approval path passes "manual",
// while timeout/cancellation callers leave the original rule/model source.
// A timeout cannot overwrite a human decision and repeat decisions cannot
// rewrite history.
func (d *DB) ResolveIntercept(id int64, status, action, reason string, source ...string) (bool, error) {
	actor := ""
	if len(source) > 0 {
		actor = source[0]
	}
	decisionSource := ""
	decidedBy := ""
	switch actor {
	case "":
	case "manual":
		decisionSource = "manual"
		decidedBy = "human"
	case "human", "rule", "model", "system":
		decidedBy = actor
	default:
		return false, fmt.Errorf("无效审批来源 %q", actor)
	}
	return d.resolveIntercept(id, status, action, reason, decisionSource, decidedBy, "", "")
}

// ResolveInterceptBy records a human decision and the approver identity.
func (d *DB) ResolveInterceptBy(id int64, status, action, reason, decidedBy, approver, remoteAddr string) (bool, error) {
	switch decidedBy {
	case "human", "rule", "model", "system":
	default:
		return false, fmt.Errorf("decided_by must be human, rule, model or system, got %q", decidedBy)
	}
	decisionSource := ""
	if decidedBy == "human" {
		decisionSource = "manual"
	}
	return d.resolveIntercept(id, status, action, reason, decisionSource, decidedBy, approver, remoteAddr)
}

func (d *DB) resolveIntercept(id int64, status, action, reason, decisionSource, decidedBy, approver, remoteAddr string) (bool, error) {
	execution := "not_executed"
	if action == "allow" {
		execution = "awaiting_result"
	}
	fields := map[string]any{
		"effective_action": action, "decision_reason": reason, "execution_status": execution,
	}
	if approver != "" {
		fields["approver"] = approver
	}
	if remoteAddr != "" {
		fields["approver_addr"] = remoteAddr
	}
	patch, err := json.Marshal(fields)
	if err != nil {
		return false, err
	}
	r, err := d.Exec(`UPDATE intercept_pending SET status=$2, decided_at=NOW(),
		decision_source=CASE WHEN $5 <> '' THEN $5 ELSE decision_source END,
		decided_by=CASE WHEN $6 <> '' THEN $6 WHEN decided_by <> '' THEN decided_by ELSE 'system' END,
		audit=CASE WHEN audit IS NULL THEN NULL ELSE audit || $3::jsonb ||
        CASE WHEN $4='allow' AND audit->>'correlation' IS DISTINCT FROM 'exact'
        THEN '{"execution_status":"unknown"}'::jsonb ELSE '{}'::jsonb END END
		WHERE id=$1 AND status='pending'`, id, status, patch, action, decisionSource, decidedBy)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// CompleteIntercept only updates the exact recorded call after it was allowed.
// A blocked tool_result must never be presented as a failed execution.
func (d *DB) CompleteIntercept(id int64, runID, toolUseID, status, output string, truncated bool) error {
	patch, err := json.Marshal(map[string]any{
		"execution_status": status, "output": output, "output_truncated": truncated,
		"execution_ended_at": time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE intercept_pending SET audit=audit || $4::jsonb
		WHERE id=$1 AND audit->>'run_id'=$2 AND audit->>'tool_use_id'=$3
		AND audit->>'effective_action'='allow' AND audit->>'execution_status'='awaiting_result'`,
		id, runID, toolUseID, patch)
	return err
}
