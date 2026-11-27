package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// TriageCandidate is the slice of a finding a duplicate check needs.
type TriageCandidate struct {
	FindingID   int64
	NodeID      int64
	Name        string
	VulnClass   string
	Severity    string
	Summary     string
	RelatedTask int64
}

// DuplicateAnswer is a triage verdict. DuplicateOf 0 means the finding is distinct.
type DuplicateAnswer struct {
	DuplicateOf     int64
	DuplicateOfNode int64
	Confidence      float64
}

// TriageClient answers whether a new finding matches one already registered.
// A nil client leaves only the deterministic fingerprint check.
type TriageClient interface {
	Duplicate(ctx context.Context, cand TriageCandidate, existing []TriageCandidate) (DuplicateAnswer, error)
}

func (t *ToolSet) SetTriageClient(c TriageClient) { t.triage = c }

func findingTriageBlocksGoal(status string) bool {
	switch status {
	case db.FindingFalsePositive, db.FindingDuplicate, db.FindingIgnored:
		return true
	default:
		return false
	}
}

func triageDuplicateHit(ans DuplicateAnswer) bool {
	return ans.DuplicateOf > 0 && ans.Confidence >= 0.8
}

func (t *ToolSet) triageDuplicate(ctx context.Context, in db.RecordFindingInput) (DuplicateAnswer, bool) {
	if t == nil || t.triage == nil {
		return DuplicateAnswer{}, false
	}
	ans, err := t.triage.Duplicate(ctx, TriageCandidate{
		Name: in.Name, VulnClass: in.VulnClass, Severity: in.Severity, Summary: in.Summary,
	}, nil)
	if err != nil {
		return DuplicateAnswer{}, false
	}
	return ans, true
}
