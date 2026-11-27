// Package report renders a deliverable Markdown report from the exploration
// graph: confirmed findings (with severity, proof strength, evidence, and coverage).
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

const reportMaxGaps = 200

// Input bundles what the report needs.
type Input struct {
	Title       string
	Goal        string
	GeneratedAt time.Time
	AssetCounts map[string]int
	Findings    []*db.Node
	Quality     map[int64]db.FindingQuality
	Coverage    *db.CoverageLedger
}

type findingView struct {
	NodeID    int64
	VulnClass string
	Name      string
	Severity  string
	Summary   string
	PoC       string
	Q         Quality
}

func parseFinding(n *db.Node, rows map[int64]db.FindingQuality) findingView {
	var p struct {
		VulnClass string `json:"vulnclass"`
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		Evidence  struct {
			PoC string `json:"poc"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	var q Quality
	if rows != nil {
		q = qualityFromRows(n.ID, rows)
	}
	return findingView{
		NodeID:    n.ID,
		VulnClass: p.VulnClass,
		Name:      p.Name,
		Severity:  p.Severity,
		Summary:   p.Summary,
		PoC:       p.Evidence.PoC,
		Q:         q,
	}
}

var sevRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "": 4}

// Markdown renders the report.
func Markdown(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 渗透测试报告 — %s\n\n", nz(in.Title, "未命名任务"))
	fmt.Fprintf(&b, "- **任务目标**：%s\n", nz(in.Goal, "（未指定）"))
	fmt.Fprintf(&b, "- **生成时间**：%s\n\n", in.GeneratedAt.Format("2006-01-02 15:04:05"))

	// summary
	fmt.Fprintf(&b, "## 摘要\n\n")
	fmt.Fprintf(&b, "- 确认发现：**%d** 个\n", len(in.Findings))
	fmt.Fprintf(&b, "- 资产：")
	var types []string
	for t := range in.AssetCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for i, t := range types {
		if i > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s %d", t, in.AssetCounts[t])
	}
	b.WriteString("\n\n")

	// findings
	fmt.Fprintf(&b, "## 发现\n\n")
	if len(in.Findings) == 0 {
		b.WriteString("_本次未确认漏洞。_\n\n")
	} else {
		fs := make([]findingView, 0, len(in.Findings))
		for _, n := range in.Findings {
			fs = append(fs, parseFinding(n, in.Quality))
		}
		sort.SliceStable(fs, func(i, j int) bool { return sevRank[fs[i].Severity] < sevRank[fs[j].Severity] })
		for i, f := range fs {
			fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), nz(f.Name, nz(f.VulnClass, "未分类")))
			if in.Quality != nil {
				b.WriteString(qualityBullets(f.Q, true))
			}
			fmt.Fprintf(&b, "%s\n\n", nz(f.Summary, ""))
			if in.Quality != nil {
				b.WriteString(qualityNarrative(f.Q))
				b.WriteString(staleReportNotice(f.Q.ReportStale()))
			}
			if f.PoC != "" {
				fmt.Fprintf(&b, "**PoC / 证据：**\n\n```\n%s\n```\n\n", f.PoC)
			}
			if in.Quality != nil {
				b.WriteString(detailedReport(f.Q.Report, "**详细报告：**"))
			}
		}
	}

	if c := in.Coverage; c != nil {
		b.WriteString("## 测试覆盖度\n\n")
		if c.Completeness != nil {
			fmt.Fprintf(&b, "- **完成度**：%s。%s\n", c.Completeness.State, c.Completeness.Caveat)
		}
		fmt.Fprintf(&b, "- 台账条目：%d，未闭环缺口：%d\n", len(c.Entries), len(c.Gaps))
		for i, g := range c.Gaps {
			if i == reportMaxGaps {
				fmt.Fprintf(&b, "- 另有 %d 项（详见覆盖度接口）\n", len(c.Gaps)-reportMaxGaps)
				break
			}
			fmt.Fprintf(&b, "- %s\n", g)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func nz(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
