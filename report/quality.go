package report

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

// Quality describes the machine-checked proof strength of a finding.
type Quality struct {
	PoE                   int
	Verification          string
	VerificationNote      string
	ConfidenceRationale   string
	Counterevidence       string
	TrafficCount          int
	RecordID              int64
	Report                string
	EvidenceVersion       int64
	ReportEvidenceVersion int64
	Missing               bool
	Unavailable           bool
}

func (q Quality) ReportStale() bool {
	return reportStale(q.Report, q.EvidenceVersion, q.ReportEvidenceVersion)
}

func reportStale(report string, evidenceVersion, reportEvidenceVersion int64) bool {
	return report != "" && evidenceVersion != reportEvidenceVersion
}

func qualityFromRows(nodeID int64, rows map[int64]db.FindingQuality) Quality {
	if rows == nil {
		return Quality{Unavailable: true}
	}
	q, ok := rows[nodeID]
	if !ok {
		return Quality{Missing: true}
	}
	return Quality{
		PoE:                   q.PoeLevel,
		Verification:          q.VerificationStatus,
		VerificationNote:      q.VerificationNote,
		ConfidenceRationale:   q.ConfidenceRationale,
		Counterevidence:       q.Counterevidence,
		TrafficCount:          q.TrafficCount,
		RecordID:              q.ID,
		Report:                q.Report,
		EvidenceVersion:       q.EvidenceVersion,
		ReportEvidenceVersion: q.ReportEvidenceVersion,
	}
}

func qualityFromDBFinding(f *db.DBFinding) Quality {
	return Quality{
		PoE:                   f.PoeLevel,
		Verification:          f.VerificationStatus,
		VerificationNote:      f.VerificationNote,
		ConfidenceRationale:   f.ConfidenceRationale,
		Counterevidence:       f.Counterevidence,
		TrafficCount:          f.TrafficCount,
		RecordID:              f.ID,
		Report:                f.Report,
		EvidenceVersion:       f.EvidenceVersion,
		ReportEvidenceVersion: f.ReportEvidenceVersion,
	}
}

func qualityBullets(q Quality, alwaysProof bool) string {
	var b strings.Builder
	if q.Unavailable {
		b.WriteString("- **核验状态**：未知（质量查询失败，证明强度暂不可用）\n\n")
		return b.String()
	}
	if q.Missing {
		b.WriteString("- **核验状态**：未评估（无独立漏洞记录）\n\n")
		return b.String()
	}
	if q.RecordID > 0 {
		fmt.Fprintf(&b, "- **漏洞记录**：#%d\n", q.RecordID)
	}
	fmt.Fprintf(&b, "- **PoE 等级**：%d\n", q.PoE)
	fmt.Fprintf(&b, "- **核验状态**：%s\n", verificationLabel(q.Verification))
	if alwaysProof || q.TrafficCount == 0 {
		if q.TrafficCount > 0 {
			fmt.Fprintf(&b, "- **流量证据**：已关联 %d 条流量记录\n", q.TrafficCount)
		} else {
			b.WriteString("- **流量证据**：未关联流量证据\n")
		}
	}
	if note := oneLine(q.VerificationNote); note != "" {
		fmt.Fprintf(&b, "- **核验说明**：%s\n", note)
	}
	b.WriteString("\n")
	return b.String()
}

func qualityNarrative(q Quality) string {
	if q.Missing || q.Unavailable {
		return ""
	}
	var b strings.Builder
	if s := strings.TrimSpace(q.ConfidenceRationale); s != "" {
		fmt.Fprintf(&b, "**置信依据：**\n\n%s\n\n", s)
	}
	if s := strings.TrimSpace(q.Counterevidence); s != "" {
		fmt.Fprintf(&b, "**反证排查：**\n\n%s\n\n", s)
	}
	return b.String()
}

func staleReportNotice(stale bool) string {
	if !stale {
		return ""
	}
	return "证据已变更，详细报告待更新。\n\n"
}

const reportMaxReportBytes = 8 << 10

func detailedReport(body, heading string) string {
	return detailedReportLimit(body, heading, reportMaxReportBytes)
}

func detailedReportFull(body, heading string) string {
	return detailedReportLimit(body, heading, 0)
}

func detailedReportLimit(body, heading string, limit int) string {
	s := strings.TrimSpace(body)
	if s == "" {
		return ""
	}
	if limit > 0 && len(s) > limit {
		cut := s[:limit]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		cut = strings.TrimSpace(cut)
		if unbalancedFence(cut) {
			cut += "\n```"
		}
		return heading + "\n\n" + cut +
			fmt.Sprintf("\n\n_详细报告已在 %d KiB 处截断，完整内容请查阅独立漏洞记录。_\n\n", limit>>10)
	}
	return heading + "\n\n" + s + "\n\n"
}

func unbalancedFence(s string) bool {
	fences := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			fences++
		}
	}
	return fences%2 == 1
}

func verificationLabel(s string) string {
	switch s {
	case "", db.VerificationUnverified:
		return "未核验 (unverified)"
	case db.VerificationVerified:
		return "已核验 (verified)"
	case db.VerificationBlocked:
		return "受阻 (blocked)"
	case db.VerificationNotAssessed:
		return "未评估 (not_assessed)"
	case db.VerificationFalsePositive:
		return "误报 (false_positive)"
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
