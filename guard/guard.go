// Package guard implements the safety boundary layer (docs §11): an audit log,
// user-configured intercept-rule evaluation, and Observer/G5 failure attribution.
// Every tool call passes through the PreToolUse hook before executing.
// (The RoE authorization-scope mechanism was removed; a replacement may be added
// later.) Destructive/exfil gating is no longer hard-coded here — it lives in the
// DB intercept rules (seeded as ordinary [内置] rules, so users can disable or
// delete them), evaluated via applyIntercept.
package guard

import (
	"context"
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/hook"
)

// AuditEntry records one gated tool call.
type AuditEntry struct {
	TS      int64  `json:"ts"`
	Tool    string `json:"tool"`
	Action  string `json:"action"` // allow|block
	Reason  string `json:"reason,omitempty"`
	Command string `json:"command,omitempty"`
}

// Guard enforces the side-effect policy via agent-core hooks.
type Guard struct {
	mu          sync.Mutex
	audit       []AuditEntry
	attrib      map[string]int // failure attribution counts (Observer / G5)
	reg         *hook.Registry
	interceptor *intercept.Interceptor // optional; nil disables user-configured rules
	gate        *intercept.Interceptor // same interceptor; target gate stays off unless the scope enables it
}

// New creates a Guard without user-configured intercept rules (used for pentest
// tasks where the Interceptor is not yet available).
func New() *Guard { return newGuard(nil) }

// NewWithInterceptor creates a Guard with user-configured intercept rules.
func NewWithInterceptor(ic *intercept.Interceptor) *Guard { return newGuard(ic) }

func newGuard(ic *intercept.Interceptor) *Guard {
	g := &Guard{attrib: map[string]int{}, interceptor: ic, gate: ic}
	g.reg = hook.NewRegistry().
		On(hook.PreToolUse, g.preToolUse).
		On(hook.PostToolUse, g.postToolUse)
	return g
}

// Hooks returns the hook registry to attach to an agent session.
func (g *Guard) Hooks() *hook.Registry { return g.reg }

func (g *Guard) preToolUse(ctx context.Context, ev hook.Event) hook.Result {
	// Extract the shell-command surface for the audit log: Bash + the interactive-shell
	// tools (shell_open's command, shell_send's text). Destructive/exfil gating is no
	// longer hard-coded here — it now lives in the DB intercept rules, evaluated by
	// applyIntercept below. Other tools record an empty command.
	var cmd string
	switch ev.ToolName {
	case "Bash", "shell_open":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Command
	case "shell_send":
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Text
	}
	g.record(ev.ToolName, "allow", "", cmd)
	return g.applyIntercept(ctx, ev)
}

// applyIntercept evaluates user-configured intercept rules against the tool call.
// Both rules and the fallback judge receive the complete tool input.
func (g *Guard) gateScope(ctx context.Context) (intercept.TargetScope, bool) {
	scope, ok := intercept.TargetScopeFromContext(ctx)
	return scope, ok && g.gate != nil && scope.Enabled != nil && scope.Enabled()
}

func (g *Guard) applyIntercept(ctx context.Context, ev hook.Event) hook.Result {
	scope, gated := g.gateScope(ctx)
	if g.interceptor == nil {
		return hook.Result{}
	}
	rulesOn := g.interceptor.IsToolEnabled(ev.ToolName)
	if !rulesOn && !gated {
		return hook.Result{}
	}
	ctx = intercept.WithCall(ctx, ev.ToolName, ev.Input)
	var dec intercept.Decision
	if rulesOn {
		matched := false
		dec, matched = g.interceptor.Match(ev.ToolName, ev.Input)
		if !matched {
			if judged, ok := g.interceptor.Judge(ctx, ev.ToolName, ev.Input); ok {
				dec = judged
			}
		}
	}
	if dec.Action == "deny" {
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "denied")
		return g.block(ev.ToolName, systemBlockMessage(dec.Message), "")
	}
	var unpinned []intercept.TargetRequest
	if gated {
		var res intercept.TargetResult
		if unpinned, res = g.gate.CheckTargets(ctx, scope, ev.ToolName, ev.Input); res.Code != "" {
			return g.block(ev.ToolName, TargetBlockMessage(res), "")
		}
	}
	ask := dec.Action == "ask"
	if dec.Action == "allow" {
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "allowed")
	}
	if len(unpinned) > 0 {
		res := g.gate.RequestTargets(ctx, scope, ev.ToolName, ev.Input, unpinned, askDecision(ask, dec))
		if res.Code != "" {
			return g.block(ev.ToolName, TargetBlockMessage(res), "")
		}
		if res.AskMerged {
			ask = false
		}
	}
	if ask {
		if ctx.Err() != nil {
			return g.block(ev.ToolName, systemBlockMessage("工作已取消，平台安全管控阻止执行"), "")
		}
		if !g.interceptor.HandleAsk(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input) {
			return g.block(ev.ToolName, systemBlockMessage("人工审批未通过（用户拒绝或审批超时）"), "")
		}
	}
	return hook.Result{}
}

func askDecision(ask bool, dec intercept.Decision) *intercept.Decision {
	if !ask {
		return nil
	}
	return &dec
}

func TargetBlockMessage(res intercept.TargetResult) string {
	reason := res.Reason
	if res.Host != "" && !strings.Contains(reason, res.Host) {
		target := res.Host
		if res.Port != 0 {
			target = net.JoinHostPort(res.Host, strconv.Itoa(res.Port))
		}
		reason = "target " + target + ": " + reason
	}
	return "【ARTEX 平台管控·非目标防御】" + res.Code + "：" + reason + "。" + targetGuidance(res.Code)
}

func targetGuidance(code string) string {
	switch code {
	case intercept.TargetApprovalPending:
		return "调用未执行。需要操作者批准，批准后请原样重试，不要改写命令或替换 IP。"
	case intercept.TargetDenied:
		return "调用未执行。操作者未批准该目标，不要换路径重试。"
	case intercept.TargetResolveFailed:
		return "调用未执行，也没有发起审批。这是平台解析失败，请稍后原样重试。"
	case intercept.TargetRuleBlocked:
		return "调用未执行。操作者的资产规则排除了该目标。"
	case intercept.TargetTooMany:
		return "调用未执行。请减少单次调用中的新目标数量。"
	default:
		return "调用未执行。这是平台侧失败，请稍后原样重试。"
	}
}

// systemBlockMessage frames an intercept block as an ARTEX platform-governance
// decision so the agent does not mistake it for a target-side defense.
//
// The bare reasons ("禁止执行此工具" / "用户拒绝") read exactly like a WAF/403 on
// the target, so a pentest agent's instinct is to bypass them — rewrite the
// command, swap the payload, re-encode, retry. That is both futile (the platform
// blocks the class of action, not one string) and wrong (it's a policy decision,
// not an obstacle to defeat). This prefix states plainly that the block comes
// from the platform, is not the target's protection, and that the operation is
// forbidden — so the agent pivots to another approach instead of evading it.
// Audit/history rows keep the raw reason (see Interceptor.Log); only the
// model-facing tool_result carries this framing.
func systemBlockMessage(reason string) string {
	return "【ARTEX 平台管控·非目标防御】此调用被平台拦截。" +
		"原因：" + reason + "。此操作被禁止。"
}

var reBlocked = regexp.MustCompile(`(?i)\b(403|forbidden|waf|blocked|rate.?limit|429|captcha|denied)\b`)

// postToolUse is the Observer failure-attribution hook (G5): it classifies tool
// results into blocked / error / ok so the planner can change strategy instead
// of giving up at a WAF.
func (g *Guard) postToolUse(_ context.Context, ev hook.Event) hook.Result {
	if ev.ToolName != "Bash" {
		return hook.Result{}
	}
	class := "ok"
	switch {
	case reBlocked.Match(ev.Result):
		class = "blocked"
	case ev.IsError:
		class = "error"
	}
	g.mu.Lock()
	g.attrib[class]++
	g.mu.Unlock()
	return hook.Result{}
}

// Attributions returns failure-attribution counts (Observer / G5).
func (g *Guard) Attributions() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.attrib))
	for k, v := range g.attrib {
		out[k] = v
	}
	return out
}

func (g *Guard) block(tool, reason, cmd string) hook.Result {
	g.record(tool, "block", reason, cmd)
	return hook.Result{Decision: "block", Message: reason}
}

func (g *Guard) record(tool, action, reason, cmd string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.audit = append(g.audit, AuditEntry{TS: time.Now().Unix(), Tool: tool, Action: action, Reason: reason, Command: cmd})
	if len(g.audit) > 2000 {
		g.audit = g.audit[len(g.audit)-2000:]
	}
}

// Audit returns a snapshot of recent gated calls (most recent last).
func (g *Guard) Audit() []AuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]AuditEntry, len(g.audit))
	copy(out, g.audit)
	return out
}
