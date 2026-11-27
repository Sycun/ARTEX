package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "未分类"))
}

func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# 漏洞发现汇总报告\n\n")
	fmt.Fprintf(&b, "- **生成时间**：%s\n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **发现总数**：%d 个\n\n", len(items))

	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## 摘要\n\n")
	b.WriteString("| 严重等级 | 数量 |\n| --- | --- |\n")
	for _, s := range []struct{ key, label string }{
		{"critical", "严重"}, {"high", "高危"}, {"medium", "中危"}, {"low", "低危"},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	if counts["unassessed"] > 0 {
		fmt.Fprintf(&b, "| 待评估 | %d |\n", counts["unassessed"])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_无匹配的漏洞。_\n")
		return b.String()
	}

	b.WriteString("## 漏洞明细\n\n")
	for i, f := range items {
		q := qualityFromDBFinding(f)
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **类别**：%s\n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- **状态**：%s\n", nz(f.Status, "pending"))
		if rs := strings.TrimSpace(f.ReportState); rs != "" && rs != "none" {
			fmt.Fprintf(&b, "- **报告状态**：%s\n", rs)
		}
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **所属任务**：%s\n", desc)
		}
		fmt.Fprintf(&b, "- **发现时间**：%s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		b.WriteString(qualityBullets(q, false))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		b.WriteString(qualityNarrative(q))
		if rm := strings.TrimSpace(f.Remark); rm != "" {
			fmt.Fprintf(&b, "**备注：**\n\n%s\n\n", rm)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**证据：**\n\n```\n%s\n```\n\n", e)
		}
		b.WriteString(detailedReportFull(f.Report, "**详细报告：**"))
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	q := qualityFromDBFinding(f)
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- **类别**：%s\n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- **严重等级**：%s\n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- **状态**：%s\n", nz(f.Status, "pending"))
	if rs := strings.TrimSpace(f.ReportState); rs != "" && rs != "none" {
		fmt.Fprintf(&b, "- **报告状态**：%s\n", rs)
	}
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **所属任务**：%s\n", desc)
	}
	fmt.Fprintf(&b, "- **发现时间**：%s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **生成时间**：%s\n", generatedAt.Format("2006-01-02 15:04:05"))
	b.WriteString(qualityBullets(q, false))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## 概述\n\n%s\n\n", s)
	}
	b.WriteString(qualityNarrative(q))
	if rm := strings.TrimSpace(f.Remark); rm != "" {
		fmt.Fprintf(&b, "## 备注\n\n%s\n\n", rm)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## 证据\n\n```\n%s\n```\n\n", e)
	}
	b.WriteString(detailedReportFull(f.Report, "## 详细报告"))
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

func stripCombiningAndFormat(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := stripCombiningAndFormat(findingTitle(f))
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	if f.ExportCaseID > 0 {
		name = fmt.Sprintf("case_%d_%s_%s", f.ExportCaseID, sev, title)
	}
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	name = path.Base(name)
	name = truncateUTF8Bytes(name, 120)
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	return name + ".md"
}

func csvCell(s string) string {
	if s == "" {
		return ""
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "名称", "类别", "严重等级", "状态", "核验状态", "PoE等级", "报告状态", "所属任务", "发现时间", "概述", "备注", "流量证据数量", "流量证据ID"})
	for _, f := range items {
		id := fmt.Sprintf("%d", f.ID)
		if f.ExportCaseID > 0 {
			id = fmt.Sprintf("case:%d", f.ExportCaseID)
		}
		_ = w.Write([]string{
			id,
			csvCell(findingTitle(f)),
			csvCell(f.VulnClass),
			nz(f.Severity, "info"),
			nz(f.Status, "pending"),
			nz(f.VerificationStatus, "unverified"),
			fmt.Sprint(f.PoeLevel),
			nz(f.ReportState, "none"),
			csvCell(f.TaskDescription),
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			csvCell(strings.TrimSpace(f.Summary)),
			csvCell(strings.TrimSpace(f.Remark)),
			fmt.Sprint(len(f.TrafficBindings)),
			findingTrafficIDs(f),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := reportStale(f.Report, f.EvidenceVersion, f.ReportEvidenceVersion)
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n## 关联流量证据\n\n")
	fmt.Fprintf(&out, "证据版本：%d；绑定数量：%d。\n\n", f.EvidenceVersion, len(f.TrafficBindings))
	out.WriteString(staleReportNotice(stale))
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **证据 #%d · %s** — `%s %s`，状态码 %d\n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [请求报文](evidence/%d/%d/request.http) · [响应报文](evidence/%d/%d/response.http)\n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
