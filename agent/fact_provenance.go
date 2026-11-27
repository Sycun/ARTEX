package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	factProvenanceRecorded   = "recorded_activity"
	factProvenanceCaptured   = "captured_traffic"
	factProvenanceOperator   = "operator_input"
	factProvenanceUnverified = "unverified"
	factEvidenceMinRunes     = 12
	factEvidenceMaxLines     = 20
	factCaptureWindowBefore  = 10 * time.Minute
	factCaptureWindowAfter   = 2 * time.Minute
)

// FactTrafficLookup is the proxy capture store seen from the fact layer.
type FactTrafficLookup interface {
	ResponseContaining(lines []string, fromTS, toTS int64) (id string, ok bool, err error)
}

func (t *ToolSet) SetFactTraffic(l FactTrafficLookup) { t.factTraffic = l }

func (t *ToolSet) SetTrafficProofChecker(fn func(ctx context.Context, trafficID string) (bool, error)) {
	t.trafficProofChecker = fn
}

func normalizeFactSource(s string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(s))
	if norm == "" {
		return factSourceInference, nil
	}
	for _, v := range factSources {
		if norm == v {
			return v, nil
		}
	}
	return "", fmt.Errorf("source %q is not valid: use one of %s (omit it for inference)", s, strings.Join(factSources, ", "))
}

func factEvidenceLines(evidence string) []string {
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, raw := range strings.Split(evidence, "\n") {
		line := strings.TrimSpace(raw)
		if len([]rune(line)) < factEvidenceMinRunes || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
		if len(out) == factEvidenceMaxLines {
			break
		}
	}
	return out
}

func (t *ToolSet) factProvenance(source, evidence string, intent int64) (status string, activityID int64, trafficID, note string) {
	if source == factSourceOperatorInput {
		return factProvenanceOperator, 0, "", ""
	}
	if intent <= 0 || t == nil || t.ts == nil {
		return factProvenanceUnverified, 0, "", "no intent was recorded, so no activity could corroborate the evidence"
	}
	lines := factEvidenceLines(evidence)
	if len(lines) == 0 {
		return factProvenanceUnverified, 0, "", "evidence is missing or too short to quote a recorded activity; a fact proves a goal only when its evidence is tied to one"
	}
	id, at, ok, err := t.ts.ActivityContainingEvidence(intent, lines)
	if err != nil {
		return factProvenanceUnverified, 0, "", "the recorded activity could not be read, so the evidence is unverified"
	}
	if !ok {
		return factProvenanceUnverified, 0, "", "evidence does not quote any line of this intent's recorded activity; quote the output verbatim instead of paraphrasing it"
	}
	if t.factTraffic != nil && !at.IsZero() {
		from, to := at.Add(-factCaptureWindowBefore).Unix(), at.Add(factCaptureWindowAfter).Unix()
		if tid, hit, _ := t.factTraffic.ResponseContaining(lines, from, to); hit && tid != "" {
			return factProvenanceCaptured, id, tid, ""
		}
	}
	return factProvenanceRecorded, id, "", ""
}
