package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (段 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `你是渗透测试目标分解器。你的职责是从用户输入中识别出**最终要达成的结果**，而不是规划攻击步骤。

**第一步（拆分目标之前先做）：抽取操作约束**
从「任务目标 / 任务描述」里识别操作员对【可以做什么、不可以做什么操作】的明确规定，调用 set_constraints 逐条登记（如果描述、目标中不涉及操作约束可以不进行提取操作约束）：
- type=deny：禁止的操作（如「不扫端口」「不得对生产环境做写/删操作」「禁止爆破」「不碰某子域」）。
- type=allow：明确允许/限定的操作范围（如「只允许被动侦察」「仅针对某域名」）。
- 约束 ≠ 目标，也 ≠ 攻击步骤：它是对操作行为边界的规定。
- **约束必须【自包含、写死具体目标】**：把「当前目标/当前端口/当前IP/当前域名/本站」这类**指代词**替换成任务目标/描述里的**具体值**。约束会被单独注入到执行阶段的提示里，脱离上下文后指代词无法判断指谁。
  例：目标是 https://abc.example.net → 写「只允许测试 abc.example.net」而不是「只允许测试当前目标」；「仅测目标端口 443，不扫其他端口」而不是「只测当前端口」。若原文只说「当前目标」但目标地址已明确，就把地址填进去。
- **只登记目标/描述里【明确写出或强调】的约束，严禁臆造**；拿不准类型时用 deny（更保守）。
- 若目标/描述里确实没有任何操作约束，则**不要**调用 set_constraints。
登记完约束（如有）后，再进行下面的目标拆分。

**目标 = 最终可交付/可核验的结果**

**不是目标的内容（禁止列为子目标）**：
- 信息收集、侦察、端点扫描
- 漏洞分析与验证过程
- 攻击步骤、利用手段
- 结果验证步骤

**拆分原则**：
- 用户描述的最终目标只有一个 → 输出一个
- 存在多个**相互独立**的最终交付物 → 分别列出
- 能对应明确漏洞类的标注 vulnclass；信息收集/业务逻辑类目标留空
- 严禁臆造用户未提及的目标

调用 set_goals 提交结果。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**额外职责：登记测试资产范围**
除拆分目标外，你还要从「任务目标 / 任务描述」里识别出**明确给出的测试资产范围**，调用 add_task_scope 登记（本任务的授权边界，也是资产测试覆盖度的分母）。**最小范围原则：只登记用户明确点到的那一个目标，绝不擅自放大。**
- 目标是 URL 或带主机名的地址（如 https://xxx.example.com/path、app.example.com）→ 取其**完整主机名**，kind=subdomain，value=完整主机名。
  例：目标 https://a1b2c3.lab.example.net/path → kind=subdomain，value=a1b2c3.lab.example.net（**不是** example.net）。
  **严禁**把带子域的主机名缩成根域名——看到 xxx.example.com 就登记整个 example.com 会把范围扩到用户目标之外，违背最小范围原则。
- 仅当用户给的就是**裸根域名、且不含任何子域**（如直接写 example.com），或明确说“整个站点 / 所有子域 / 全域名” → 才用 kind=root_domain，value=example.com。
- 纯 IP 或网段 → kind=ip / cidr，value=IP 或 CIDR。
- **不要**登记公司范围（company）——任务刚建立、资产系统里通常还没有这家公司，登记不上，公司级范围交由后续 plan 阶段处理。
其它规则：
- 只登记**目标/描述里明确写出**的范围；严禁臆造或推断未提及的域名/IP。
- reason 简述依据来自哪句话，便于审计。
- 若目标/描述中没有任何明确资产范围，则**不要**调用 add_task_scope。
先用 add_task_scope 登记范围（如有），再调用 set_goals 提交目标。`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// ErrGoalDecompositionContract reports that the Goal Decomposer did not submit
// at least one non-empty goal through a structured set_goals tool call even
// after the single strict retry.
var ErrGoalDecompositionContract = errors.New("goal decomposition did not produce a structured set_goals tool call")

// ErrGoalDecompositionEmptyInput reports that goal decomposition was invoked
// with an empty task goal.
var ErrGoalDecompositionEmptyInput = errors.New("goal decomposition requires a non-empty task goal")

// ErrGoalDecompositionNoProvider reports that goal decomposition cannot run
// because no LLM provider is configured. Callers distinguish this from a
// structured tool-call contract failure (ErrGoalDecompositionContract).
var ErrGoalDecompositionNoProvider = errors.New("goal decomposition failed: no LLM provider configured")

const goalsStrictRetryPrompt = "你刚才没有通过结构化工具调用提交任何目标。禁止以纯文本输出 set_goals 或 JSON；现在请立即调用结构化工具 set_goals 提交至少一条非空目标。"

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). It enforces
// the structured set_goals tool-call contract: if no non-empty goal is
// submitted on the first pass, it retries once with a stricter instruction and
// then returns an explicit error instead of materializing empty goals.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) ([]GoalSpec, error) {
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) ([]GoalSpec, error) {
	if prov == nil {
		return nil, ErrGoalDecompositionNoProvider
	}
	if ts == nil {
		return nil, errors.New("goal decomposition failed: exploration store is not initialized")
	}
	trimmedGoal := strings.TrimSpace(goalText)
	if trimmedGoal == "" {
		return nil, ErrGoalDecompositionEmptyInput
	}
	beforeIDs, err := existingGoalIDs(ts)
	if err != nil {
		return nil, fmt.Errorf("goal decomposition failed to inspect existing goals: %w", err)
	}
	// 目标拆解是一次性调用：不挂 transcript store，所以 agentcore 不会往 ctx 上挂
	// session id（它只在有 writer 时才挂，见 agentcore.Prompt）。显式挂一个稳定 id。
	ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "任务目标：\n" + trimmedGoal
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\n任务描述（背景信息，可能含靶标范围/数量要求/交战说明；仅供参考，不要臆造其中未提及的内容）：\n" + d
	}
	// Use captureRunSession on one session so a single strict retry keeps any
	// prior tool results (constraints/scope) and shows the model its prior output.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	session := newAgentSession(agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 步(抽约束 → 登记范围 → 拆目标)各需一次工具调用,给足回合避免收尾前漏调 set_goals。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    maxTokens,    // 0 = 不发上限,由服务端默认值决定
	})
	defer session.Close()

	finalText, _, runErr := captureRunSession(ctx, session, userMsg, captureEmit)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		return nil, fmt.Errorf("goal decomposition failed: %w", runErr)
	}
	out, err := readNewGoals(ts, beforeIDs)
	if err != nil {
		return nil, fmt.Errorf("goal decomposition failed to read persisted goals: %w", err)
	}
	if len(out) > 0 {
		return out, nil
	}

	// No valid structured set_goals call succeeded on the first pass: allow
	// exactly one strict retry before failing explicitly.
	log.Printf("[goals] task %d: goal decomposer produced no structured set_goals tool call; retrying once with stricter instruction", taskID)
	finalText, _, runErr = captureRunSession(ctx, session, goalsStrictRetryPrompt, captureEmit)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		return nil, fmt.Errorf("goal decomposition retry failed: %w", runErr)
	}
	out, err = readNewGoals(ts, beforeIDs)
	if err != nil {
		return nil, fmt.Errorf("goal decomposition failed to read persisted goals after retry: %w", err)
	}
	if len(out) > 0 {
		return out, nil
	}
	if strings.TrimSpace(finalText) != "" {
		return nil, fmt.Errorf("%w after retry (model returned plain text instead of a structured tool call)", ErrGoalDecompositionContract)
	}
	return nil, fmt.Errorf("%w after retry", ErrGoalDecompositionContract)
}

func existingGoalIDs(ts *db.ExplorationStore) (map[int64]struct{}, error) {
	nodes, err := ts.ListByKind(db.KindGoal, 10000)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]struct{}, len(nodes))
	for _, n := range nodes {
		ids[n.ID] = struct{}{}
	}
	return ids, nil
}

func readNewGoals(ts *db.ExplorationStore, before map[int64]struct{}) ([]GoalSpec, error) {
	nodes, err := ts.ListByKind(db.KindGoal, 10000)
	if err != nil {
		return nil, err
	}
	var out []GoalSpec
	for _, n := range nodes {
		if _, existed := before[n.ID]; existed {
			continue
		}
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if text := strings.TrimSpace(p.Text); text != "" {
			out = append(out, GoalSpec{Text: text, VulnClass: strings.TrimSpace(p.VulnClass)})
		}
	}
	return out, nil
}
