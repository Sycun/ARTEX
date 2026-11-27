package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestMarkdownCoverageAndFindingQuality(t *testing.T) {
	ledger := db.BuildCoverageLedger(
		[]db.CoverageEntry{{Surface: "/a", RiskArea: "xss", Outcome: db.OutcomeNeedsFollowUp}},
		[]string{"sqli"},
		db.CoverageCompletenessFor("timeout", false, false, true),
	)
	node := &db.Node{
		ID:      8,
		Kind:    db.KindFinding,
		Payload: []byte(fmt.Sprintf(`{"vulnclass":"cwe-602","name":%q,"severity":"high","summary":"bola summary","evidence":{"poc":"GET /orders/2"}}`, "BOLA")),
	}
	quality := map[int64]db.FindingQuality{
		8: {
			FindingMeta:           db.FindingMeta{ID: 1, Status: db.FindingConfirmed, TrafficCount: 2},
			PoeLevel:              3,
			VerificationStatus:    db.VerificationVerified,
			ConfidenceRationale:   "victim order returned",
			Counterevidence:       "PATCH returned 403",
			Report:                "## 详情\n\n已复现",
			EvidenceVersion:       4,
			ReportEvidenceVersion: 3,
		},
	}

	md := Markdown(Input{
		Title:       "test-task",
		GeneratedAt: time.Unix(0, 0),
		Findings:    []*db.Node{node},
		Quality:     quality,
		Coverage:    ledger,
	})
	for _, want := range []string{
		"**PoE 等级**：3",
		"verified",
		"victim order returned",
		"PATCH returned 403",
		"证据已变更，详细报告待更新。",
		"## 测试覆盖度",
		"timeout",
		"- unrecorded_risk_class sqli",
		"- needs_follow_up /a xss",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in markdown:\n%s", want, md)
		}
	}
}

func TestFindingsCSVFormulaGuardAndRemark(t *testing.T) {
	items := []*db.DBFinding{{
		ID:                 11,
		Status:             db.FindingConfirmed,
		Remark:             "=cmd|' /C calc'!A0",
		Severity:           "high",
		Name:               "\t@danger",
		Summary:            "normal",
		PoeLevel:           2,
		VerificationStatus: db.VerificationVerified,
	}}
	raw := FindingsCSV(items)
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected header + 1 row, got %d", len(rows))
	}
	foundGuardedName := false
	foundGuardedRemark := false
	for _, cell := range rows[1] {
		if cell == "'\t@danger" {
			foundGuardedName = true
		}
		if cell == "'=cmd|' /C calc'!A0" {
			foundGuardedRemark = true
		}
	}
	if !foundGuardedName || !foundGuardedRemark {
		t.Fatalf("expected formula-guarded name and remark in row: %#v", rows[1])
	}
}
