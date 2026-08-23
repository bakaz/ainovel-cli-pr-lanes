package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── 多轮候选工具协议路径集成测试（包 6：polish_draft 运行时接线） ────────────
// 覆盖计划 §13 + schema §13 要求：正常流程 / no_op / 顺序违规 / 32-edit 边界 /
// 重叠 anchor / 事实保护 / 技术预算 / CAS 并发 / 旧路径回归 / 审计边界。

// testAccHolder 是 PolishAccumulatorHolder 的测试实现（agents 包不可被 tools 测试
// 引用——agents 依赖 tools，反向引用会成环）。
type testAccHolder struct{ acc *PolishAccumulator }

func (h *testAccHolder) Set(acc *PolishAccumulator) { h.acc = acc }

// candidateStep 是候选协议 mock 的一步：工具名 + 参数构造器（接收从任务文本提取的
// operation_id/baseline_digest）。before 在返回该步工具调用前执行（测试副作用，
// 如模拟并发修改草稿）。
type candidateStep struct {
	tool   string
	args   func(opID, digest string) map[string]any
	before func()
}

// extractProtocolIDs 从任务文本的"候选工具协议"段提取 operation_id / baseline_digest。
func extractProtocolIDs(msgs []agentcore.Message) (string, string) {
	var task string
	for _, m := range msgs {
		if m.Role == agentcore.RoleUser {
			task = m.TextContent()
			break
		}
	}
	idx := strings.Index(task, "### 候选工具协议")
	if idx < 0 {
		return "", ""
	}
	opID, digest := "", ""
	for _, line := range strings.Split(task[idx:], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- operation_id: ") {
			opID = strings.TrimPrefix(line, "- operation_id: ")
		} else if strings.HasPrefix(line, "- baseline_digest: ") {
			digest = strings.TrimPrefix(line, "- baseline_digest: ")
		}
	}
	return opID, digest
}

// newCandidateProtocolRunner 构造按脚本执行候选工具协议的多轮 mock runner：
// 每步返回一个工具调用（args 由 opID/digest 构造）；steps 用尽后返回纯文本结束
// turn。StopAfterTools=["finish_polish"] 与生产配置一致。
func newCandidateProtocolRunner(steps []candidateStep, maxTurns int, tools []agentcore.Tool) *subagent.Runner {
	model := &mockPolisherModel{fn: func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		opID, digest := extractProtocolIDs(msgs)
		if i >= len(steps) {
			return &agentcore.LLMResponse{Message: polisherText("done")}, nil
		}
		step := steps[i]
		if step.before != nil {
			step.before()
		}
		raw, _ := json.Marshal(step.args(opID, digest))
		return &agentcore.LLMResponse{Message: agentcore.Message{
			Role:       agentcore.RoleAssistant,
			Content:    []agentcore.ContentBlock{agentcore.ToolCallBlock(agentcore.ToolCall{ID: fmt.Sprintf("call-%d", i), Name: step.tool, Args: raw})},
			StopReason: agentcore.StopReasonStop,
		}}, nil
	}}
	cfg := subagent.Config{
		Name:           "polisher",
		Description:    "mock polisher",
		Model:          model,
		MaxTurns:       maxTurns,
		Tools:          tools,
		StopAfterTools: []string{"finish_polish"},
	}
	return subagent.NewRunner(cfg)
}

// newFullContextPolishTool 构造启用多轮路径的 polish_draft 工具：创建三个候选
// 工具 → 注册进 mock runner → 注入 polish_draft（holder + 工具 + flag=true）。
func newFullContextPolishTool(st *store.Store, steps []candidateStep, maxTurns int) (*PolishDraftTool, *subagent.Runner) {
	plan := NewSubmitPolishPlanTool()
	batch := NewSubmitEditBatchTool()
	finish := NewFinishPolishTool()
	runner := newCandidateProtocolRunner(steps, maxTurns, []agentcore.Tool{plan, batch, finish})
	tool := NewPolishDraftTool(st, runner, testPolisherVersion)
	tool.SetEnabled(true)
	tool.SetAccumulatorHolder(&testAccHolder{})
	tool.SetCandidateTools(plan, batch, finish)
	tool.SetFullContextEnabled(true)
	return tool, runner
}

// planStep 构造 submit_polish_plan 步骤（n 个 action=edit 的 issue）。
func planStep(n int) candidateStep {
	return candidateStep{tool: "submit_polish_plan", args: func(opID, digest string) map[string]any {
		issues := make([]map[string]any, 0, n)
		for i := 1; i <= n; i++ {
			issues = append(issues, polishPlanIssue(fmt.Sprintf("p-%03d", i), "edit"))
		}
		return map[string]any{
			"operation_id":       opID,
			"baseline_digest":    digest,
			"overall_assessment": "全章节奏偏慢，需要精修。",
			"planned_edit_count": n,
			"issues":             issues,
		}
	}}
}

// batchStep 构造 submit_edit_batch 步骤（batchIndex 批，edits 为 (issueID, old, new) 三元组）。
func batchStep(batchIndex int, edits ...[3]string) candidateStep {
	return candidateStep{tool: "submit_edit_batch", args: func(opID, digest string) map[string]any {
		items := make([]map[string]any, 0, len(edits))
		for _, e := range edits {
			items = append(items, polishBatchEdit(e[0], e[1], e[2]))
		}
		return map[string]any{
			"operation_id":    opID,
			"baseline_digest": digest,
			"batch_index":     batchIndex,
			"edits":           items,
		}
	}}
}

// finishStep 构造 finish_polish 步骤。unresolved 为 nil 时用空数组
//（schema 声明 array 类型，null 会被 agentcore 的 schema 校验拒绝）。
func finishStep(status string, submitted int, covered []string, unresolved []map[string]any) candidateStep {
	if unresolved == nil {
		unresolved = []map[string]any{}
	}
	return candidateStep{tool: "finish_polish", args: func(opID, digest string) map[string]any {
		return map[string]any{
			"operation_id":         opID,
			"baseline_digest":      digest,
			"status":               status,
			"submitted_edit_count": submitted,
			"covered_issue_ids":    covered,
			"unresolved":           unresolved,
			"summary":              "本次精修完成。",
		}
	}}
}

// ── 1. 新路径正常流程：plan → 2 批 → finish(complete) ────────────────────

func TestPolishDraft_CandidateToolsHappyPath(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)

	steps := []candidateStep{
		planStep(2),
		batchStep(1, [3]string{"p-001", "她站在窗前", "她倚窗而立"}),
		batchStep(2, [3]string{"p-002", "望着远处的灯火", "望向远方的灯火"}),
		finishStep("complete", 2, []string{"p-001", "p-002"}, nil),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected polished+changed non-degraded output, got %+v", output)
	}
	if output.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("input_digest mismatch")
	}

	want := "她倚窗而立，望向远方的灯火。她心里骂自己丢人，真不要脸。"
	saved, _, err := st.Drafts.LoadChapterContent(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}

	// 恰好一个 polish checkpoint（一次 SaveDraft + 一次逻辑 polish checkpoint，计划 §13）
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1", n)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Method != "candidate_tools" {
		t.Errorf("checkpoint method = %q, want candidate_tools", cp.Method)
	}
	if cp.EditCount != 2 {
		t.Errorf("checkpoint edit_count = %d, want 2", cp.EditCount)
	}
	if cp.ProposedEditCount != 2 || cp.DroppedEditCount != 0 || cp.Partial {
		t.Errorf("checkpoint audit wrong: proposed=%d dropped=%d partial=%v",
			cp.ProposedEditCount, cp.DroppedEditCount, cp.Partial)
	}
	// schema §9 新审计字段
	if !strings.HasPrefix(cp.OperationID, "pol-1-") {
		t.Errorf("checkpoint operation_id = %q, want pol-1- prefix", cp.OperationID)
	}
	if cp.RunRejectionCode != "" {
		t.Errorf("checkpoint run_rejection_code = %q, want empty on happy path", cp.RunRejectionCode)
	}
	if cp.PlanIssueCount != 2 {
		t.Errorf("checkpoint plan_issue_count = %d, want 2", cp.PlanIssueCount)
	}
	if cp.BatchCount != 2 {
		t.Errorf("checkpoint batch_count = %d, want 2", cp.BatchCount)
	}
	if cp.FinishStatus != "complete" {
		t.Errorf("checkpoint finish_status = %q, want complete", cp.FinishStatus)
	}
	if cp.UnresolvedCount != 0 {
		t.Errorf("checkpoint unresolved_count = %d, want 0", cp.UnresolvedCount)
	}
	if len(cp.PlanDigest) != 64 {
		t.Errorf("checkpoint plan_digest = %q, want 64-hex sha256", cp.PlanDigest)
	}
	if cp.Digest != domain.DigestDraft(want) || cp.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint digests wrong: digest=%s input=%s", cp.Digest, cp.InputDigest)
	}
	if !cp.Changed || cp.Stage != "draft" || cp.PolisherModel != "mock-polisher-model" {
		t.Errorf("checkpoint meta wrong: %+v", cp)
	}
}

// ── 2. no_op 终态：finish(no_op) → 不落盘、changed=false ──────────────────

func TestPolishDraft_CandidateToolsNoOp(t *testing.T) {
	draft := mechCleanDraft("这段文字已经很好，无需修改。")
	st := setupPolishStore(t, 1, draft)

	steps := []candidateStep{
		planStep(0),
		finishStep("no_op", 0, []string{}, nil),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || output.Changed {
		t.Fatalf("no-op must report polished=true changed=false, got %+v", output)
	}
	if output.Degraded {
		t.Fatal("no-op must NOT be degraded（与现有 edits=[] 语义一致）")
	}
	if output.InputDigest != output.OutputDigest {
		t.Fatalf("no-op digests must match: %s vs %s", output.InputDigest, output.OutputDigest)
	}
	// 不落盘：草稿保持原样
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Errorf("draft must remain unchanged on no-op, got %q", saved)
	}
	// 与现有 edits=[] 语义一致：仍写一条 Changed=false 的 polish checkpoint
	//（validPolish 需要 polish 记录推进 FSM seq，计划 §5：一次逻辑 polish checkpoint）
	cp := polishCheckpointOf(t, st, 1)
	if cp.Changed || cp.EditCount != 0 || cp.Method != "candidate_tools" || cp.Degraded {
		t.Errorf("no-op checkpoint meta wrong: changed=%v edit_count=%d method=%q degraded=%v",
			cp.Changed, cp.EditCount, cp.Method, cp.Degraded)
	}
	if cp.FinishStatus != "no_op" {
		t.Errorf("checkpoint finish_status = %q, want no_op", cp.FinishStatus)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("no-op checkpoint digest = %s, want current draft digest", cp.Digest)
	}
}

// ── 3. 工具调用顺序违规：跳过 plan 直接 batch → 协议错误不落盘 ────────────

func TestPolishDraft_CandidateToolsOrderViolation(t *testing.T) {
	draft := mechCleanDraft("她站在窗前。")
	st := setupPolishStore(t, 1, draft)

	// 模型跳过 plan 直接提交 batch（被拒 not_planned），随后直接结束 turn（不调用 finish）。
	steps := []candidateStep{
		batchStep(1, [3]string{"p-001", "她站在窗前", "她倚窗而立"}),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected protocol error when run ends without finish_polish")
	}
	if !strings.Contains(err.Error(), "finish_polish") {
		t.Errorf("expected finish_polish protocol error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after protocol violation")
	}
}

// ── 4. 32-edit 边界：4 批 × 8 = 32 全部接受并应用 ────────────────────────

func TestPolishDraft_CandidateTools32EditBoundary(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&sb, "第%d段，内容内容内容。", i)
	}
	draft := mechCleanDraft(sb.String())
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)

	steps := []candidateStep{planStep(32)}
	for b := 1; b <= 4; b++ {
		b := b
		steps = append(steps, candidateStep{tool: "submit_edit_batch", args: func(opID, digest string) map[string]any {
			edits := make([]map[string]any, 0, 8)
			for j := 0; j < 8; j++ {
				n := (b-1)*8 + j + 1
				edits = append(edits, polishBatchEdit(fmt.Sprintf("p-%03d", n), fmt.Sprintf("第%d段", n), fmt.Sprintf("第%d段改", n)))
			}
			return map[string]any{
				"operation_id":    opID,
				"baseline_digest": digest,
				"batch_index":     b,
				"edits":           edits,
			}
		}})
	}
	covered := make([]string, 0, 32)
	for i := 1; i <= 32; i++ {
		covered = append(covered, fmt.Sprintf("p-%03d", i))
	}
	steps = append(steps, finishStep("complete", 32, covered, nil))
	tool, _ := newFullContextPolishTool(st, steps, 15)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected polished+changed, got %+v", output)
	}
	if output.ProposedEditCount != 32 || output.DroppedEditCount != 0 {
		t.Errorf("audit proposed/dropped = %d/%d, want 32/0", output.ProposedEditCount, output.DroppedEditCount)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 32 || cp.BatchCount != 4 || cp.PlanIssueCount != 32 {
		t.Errorf("checkpoint audit wrong: edit_count=%d batch_count=%d plan_issue_count=%d",
			cp.EditCount, cp.BatchCount, cp.PlanIssueCount)
	}
	// 32 条全部应用：前 32 段带"改"后缀，其余原样
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	for i := 1; i <= 32; i++ {
		if !strings.Contains(saved, fmt.Sprintf("第%d段改", i)) {
			t.Errorf("saved draft missing applied edit for segment %d", i)
		}
	}
	if strings.Contains(saved, "第33段改") {
		t.Error("segment 33 must remain unedited")
	}
}

// ── 5. 重叠 anchor：批 2 与批 1 重叠 → 部分接受（复用包 2 预检 + 最终验证） ──

func TestPolishDraft_CandidateToolsOverlapPartial(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)

	steps := []candidateStep{
		planStep(2),
		batchStep(1, [3]string{"p-001", "她站在窗前", "她倚窗而立"}),
		batchStep(2, [3]string{"p-002", "在窗前，望着", "x"}),
		finishStep("complete", 1, []string{"p-001"}, nil),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("overlap must be partial-accepted (no error), got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected partial success, got %+v", output)
	}
	if !output.Partial || output.ProposedEditCount != 2 || output.DroppedEditCount != 1 {
		t.Errorf("audit partial/proposed/dropped = %v/%d/%d, want true/2/1",
			output.Partial, output.ProposedEditCount, output.DroppedEditCount)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "anchor_overlap" {
		t.Errorf("drop_reasons = %v, want [anchor_overlap]", output.DropReasons)
	}
	want := "她倚窗而立，望着远处的灯火。她心里骂自己丢人，真不要脸。"
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 1 || cp.ProposedEditCount != 2 || cp.DroppedEditCount != 1 || !cp.Partial {
		t.Errorf("checkpoint audit wrong: applied=%d proposed=%d dropped=%d partial=%v",
			cp.EditCount, cp.ProposedEditCount, cp.DroppedEditCount, cp.Partial)
	}
	if cp.Degraded {
		t.Error("partial success must not be degraded")
	}
}

// ── 6. 事实保护：数字替换被拒（fact_changed）→ degraded 收敛 ──────────────

func TestPolishDraft_CandidateToolsFactChangedRejected(t *testing.T) {
	draft := mechCleanDraft("窗外有5只麻雀在枝头。")
	st := setupPolishStore(t, 1, draft)

	steps := []candidateStep{
		planStep(1),
		batchStep(1, [3]string{"p-001", "窗外有5只麻雀", "窗外有6只麻雀"}),
		finishStep("complete", 0, []string{}, nil),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("all-rejected must converge internally (no error), got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "edit_plan_invalid" {
		t.Fatalf("expected degraded(edit_plan_invalid) convergence, got %+v", output)
	}
	if output.Changed {
		t.Error("converged output must report changed=false")
	}
	if output.ProposedEditCount != 1 || output.DroppedEditCount != 1 {
		t.Errorf("audit proposed/dropped = %d/%d, want 1/1", output.ProposedEditCount, output.DroppedEditCount)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "fact_changed" {
		t.Errorf("drop_reasons = %v, want [fact_changed]", output.DropReasons)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "edit_plan_invalid" || cp.Changed {
		t.Errorf("rejected checkpoint must be degraded(edit_plan_invalid) changed=false, got %+v", cp)
	}
	if cp.Method != "candidate_tools" || cp.EditCount != 0 || cp.ProposedEditCount != 1 || cp.DroppedEditCount != 1 {
		t.Errorf("rejected checkpoint audit wrong: %+v", cp)
	}
	if len(cp.DropReasons) != 1 || cp.DropReasons[0] != "fact_changed" {
		t.Errorf("checkpoint drop_reasons = %v, want [fact_changed]", cp.DropReasons)
	}
	if cp.FinishStatus != "complete" {
		t.Errorf("checkpoint finish_status = %q, want complete", cp.FinishStatus)
	}
}

// ── 7. 技术重试总预算：连续失败 >8 次调用后 degraded 收敛（budget_exhausted） ──

func TestPolishDraft_CandidateToolsBudgetExhausted(t *testing.T) {
	draft := mechCleanDraft("她站在窗前。")
	st := setupPolishStore(t, 1, draft)

	// 模型连续 9 次提交 batch（全部被拒 not_planned），第 10 次调用结束 turn
	// → Usage.Turns=10 > 硬上限 8 → budget_exhausted → degraded 收敛（计划 §7）。
	steps := make([]candidateStep, 0, 9)
	for i := 0; i < 9; i++ {
		steps = append(steps, batchStep(1, [3]string{"p-001", "她站在窗前", "她倚窗而立"}))
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("budget exhaustion must converge (no error), got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "budget_exhausted" {
		t.Fatalf("expected degraded(budget_exhausted), got %+v", output)
	}
	if output.Changed {
		t.Error("degraded must report changed=false")
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "budget_exhausted" {
		t.Errorf("checkpoint must be degraded(budget_exhausted), got %+v", cp)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint digest = %s, want current draft digest", cp.Digest)
	}
}

// ── 8. CAS 并发：模型调用期间草稿被改 → stale 不落盘 ─────────────────────

func TestPolishDraft_CandidateToolsCASStale(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)

	// 模型第一次调用（提交 plan）时并发修改草稿 → 最终 CommitPolishCandidate
	// CAS 失败（草稿 digest != baseline）→ stale 错误，不落盘、不写 checkpoint。
	steps := []candidateStep{
		{tool: "submit_polish_plan", before: func() {
			if err := st.Drafts.SaveDraft(1, "并发修改后的草稿。"); err != nil {
				t.Errorf("concurrent SaveDraft: %v", err)
			}
		}, args: func(opID, digest string) map[string]any {
			issues := []map[string]any{polishPlanIssue("p-001", "edit")}
			return map[string]any{
				"operation_id":       opID,
				"baseline_digest":    digest,
				"overall_assessment": "全章节奏偏慢。",
				"planned_edit_count": 1,
				"issues":             issues,
			}
		}},
		batchStep(1, [3]string{"p-001", "她站在窗前", "她倚窗而立"}),
		finishStep("complete", 1, []string{"p-001"}, nil),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected stale error when draft changed during polish")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("expected stale error, got: %v", err)
	}
	// 草稿保持并发修改后的内容（候选未覆盖）
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "并发修改后的草稿。" {
		t.Errorf("draft = %q, want concurrent modification preserved", saved)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after CAS stale")
	}
}

// ── 9. 审计边界：checkpoint 不含 edit 内容 ───────────────────────────────

func TestPolishDraft_CandidateToolsAuditNoEditContent(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)

	steps := []candidateStep{
		planStep(2),
		batchStep(1, [3]string{"p-001", "她站在窗前", "她倚窗而立"}),
		batchStep(2, [3]string{"p-002", "望着远处的灯火", "望向远方的灯火"}),
		finishStep("complete", 2, []string{"p-001", "p-002"}, nil),
	}
	tool, _ := newFullContextPolishTool(st, steps, 15)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 审计边界（计划 §10 / schema §9）：checkpoint 只含计数与 digest，
	// 绝不含 old_string/new_string 内容。
	cp := polishCheckpointOf(t, st, 1)
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"她站在窗前", "她倚窗而立", "望着远处的灯火", "望向远方的灯火"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("checkpoint JSON must not contain edit content %q", forbidden)
		}
	}
}

// ── 10. 旧路径回归：flag=false（默认）时走现有 one-shot edit_list 路径 ────

func TestPolishDraft_FullContextFlagOffUsesOneShot(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"她站在窗前", "她倚窗而立"}))}, nil
	})
	// 不调用 SetFullContextEnabled → 默认 false → 现有 one-shot 路径。
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed {
		t.Fatalf("expected one-shot edit_list success, got %+v", output)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Method != "edit_list" {
		t.Errorf("checkpoint method = %q, want edit_list (one-shot path)", cp.Method)
	}
	if cp.OperationID != "" || cp.FinishStatus != "" || cp.PlanDigest != "" {
		t.Errorf("one-shot checkpoint must not carry candidate_tools audit fields, got %+v", cp)
	}
}