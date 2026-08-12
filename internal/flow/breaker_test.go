package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// ── P1-7：章节无进展熔断器 ────────────────────────────────────────────
//
// 场景基础：PendingRewrites 队列头章节反复被派发 writer，但草稿/账本/checkpoint
// 恒不变（ch450 类：即使 FSM/guard 漏洞被修复，配置错误或数据损坏仍可能复发）。
// 熔断器在派发 writer 前记录状态快照：连续 3 轮同一章快照完全一致且期间无新
// checkpoint/草稿/ledger 变化 → 不再自动派发（Route 返回 nil）并标记
// manual_recovery_required。

// savePermissiveRules 放宽章节字数范围（0~100000），避免短测试草稿触发
// chapter_words 机械 error（与 tools 测试的 savePermissiveUserRules 同构），
// 让 FSM 稳定在 needs_polish（ch450 同类卡点）。
func savePermissiveRules(t *testing.T, st *storepkg.Store) {
	t.Helper()
	snap := rules.BuildSnapshot([]rules.Candidate{
		rules.SystemDefaults(),
		{Source: "test", Structured: rules.Structured{ChapterWords: &rules.WordRange{Min: 0, Max: 100000}}},
	})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}
}

// breakerTestStore 构造熔断测试 store：critic 模式 + 已完成 + 重写队列 + 草稿
// （与终稿不同 digest）+ fresh consistency checkpoint → FSM 稳定在 needs_polish
// （ch450 同类卡点：每次派发都要求 polish_draft，但若 guard/模型持续无产出，
// 状态将永不变化）。
func breakerTestStore(t *testing.T, draftText string) *storepkg.Store {
	t.Helper()
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("熔断测试", 10); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	savePermissiveRules(t, st)
	if err := st.Drafts.SaveDraft(1, draftText); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "原终稿（与返工草稿不同 digest）。"); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "熔断测试"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", domain.DigestDraft(draftText)); err != nil {
		t.Fatalf("Append consistency: %v", err)
	}
	return st
}

// testBreaker 构造与生产 Engine 同源的熔断器配置（pipeline 开启）。
func testBreaker() *NoProgressBreaker {
	return NewNoProgressBreaker(tools.ChapterFSMConfig{Enabled: true, PipelineEnabled: true})
}

// assertDispatched 断言本轮仍自动派发 writer（熔断未触发）。
func assertDispatched(t *testing.T, b *NoProgressBreaker, st *storepkg.Store) *Instruction {
	t.Helper()
	inst := b.Route(st)
	if inst == nil {
		t.Fatal("期望继续派发 writer，但熔断已拦截（返回 nil）")
	}
	if inst.Agent != "writer" || inst.Chapter != 1 {
		t.Fatalf("期望派发 writer 第 1 章, got %+v", inst)
	}
	return inst
}

// assertTripped 断言本轮熔断触发（不再自动派发 + manual_recovery_required）。
// stageWant/requiredWant 为空时只断言结构字段（不同干预后阶段不同）。
func assertTripped(t *testing.T, b *NoProgressBreaker, st *storepkg.Store, stageWant, requiredWant string) {
	t.Helper()
	if inst := b.Route(st); inst != nil {
		t.Fatalf("连续同状态派发必须熔断（期望 nil）, got %+v", inst)
	}
	if !b.ManualRecoveryRequired(1) {
		t.Fatal("熔断后必须标记 chapter=1 为 manual_recovery_required")
	}
	reason := b.BlockedReason(1)
	if reason == "" {
		t.Fatal("熔断必须输出确定性 blocked 原因")
	}
	for _, want := range []string{"chapter=1", "manual_recovery_required", "连续 3 轮"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("blocked 原因必须含 %q, got: %s", want, reason)
		}
	}
	if stageWant != "" && !strings.Contains(reason, "stage="+stageWant) {
		t.Fatalf("blocked 原因必须含 stage=%s, got: %s", stageWant, reason)
	}
	if requiredWant != "" && !strings.Contains(reason, "required="+requiredWant) {
		t.Fatalf("blocked 原因必须含 required=%s, got: %s", requiredWant, reason)
	}
}

// TestNoProgressBreaker_ConsecutiveSameStateTripsAtThird 核心场景：连续同状态
// 派发 3 轮 → 第 3 轮熔断触发（不再自动派发，标记人工恢复）。
func TestNoProgressBreaker_ConsecutiveSameStateTripsAtThird(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	assertDispatched(t, b, st)                              // 第 1 轮：记录快照
	assertDispatched(t, b, st)                              // 第 2 轮：连续一致
	assertTripped(t, b, st, "needs_polish", "polish_draft") // 第 3 轮：熔断
}

// TestNoProgressBreaker_StateChangeResetsCount 中间有任何变化（草稿/账本/状态）
// → 重置计数：不触发；变化后再次连续 3 轮一致 → 重新熔断。
func TestNoProgressBreaker_StateChangeResetsCount(t *testing.T) {
	st := breakerTestStore(t, "返工草稿 v1。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	assertDispatched(t, b, st)
	assertDispatched(t, b, st)

	// 人工干预：修改草稿（digest 变化）→ 快照变化。
	v2 := "返工草稿 v2。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", domain.DigestDraft(v2)); err != nil {
		t.Fatal(err)
	}

	assertDispatched(t, b, st) // 变化轮：重置计数，不触发
	assertDispatched(t, b, st)
	assertTripped(t, b, st, "", "") // 变化后连续第 3 轮一致 → 重新熔断
}

// TestNoProgressBreaker_CheckpointAdvanceResetsCount 期间出现新 checkpoint
// （即使 FSM 判定不变）→ 重置计数：checkpoint 变化是"有进展"信号。
func TestNoProgressBreaker_CheckpointAdvanceResetsCount(t *testing.T) {
	draft := "返工草稿。她心里骂自己丢人，真不要脸。"
	st := breakerTestStore(t, draft)
	b := testBreaker()

	assertDispatched(t, b, st)
	assertDispatched(t, b, st)

	// 新 checkpoint（任意 step）：FSM 判定不变（仍 needs_polish），但 checkSeq 前进了。
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "draft", "drafts/01.draft.md", domain.DigestDraft(draft)); err != nil {
		t.Fatal(err)
	}

	assertDispatched(t, b, st) // checkpoint 变化轮：重置
	assertDispatched(t, b, st)
	assertTripped(t, b, st, "", "") // 之后连续第 3 轮一致 → 熔断
}

// TestNoProgressBreaker_RecoveryAfterManualIntervention 恢复条件：人工干预后
// 状态变化 → 熔断解除、可继续自动派发。
func TestNoProgressBreaker_RecoveryAfterManualIntervention(t *testing.T) {
	draftText := "返工草稿。她心里骂自己丢人，真不要脸。"
	st := breakerTestStore(t, draftText)
	b := testBreaker()

	assertDispatched(t, b, st)
	assertDispatched(t, b, st)
	assertTripped(t, b, st, "", "")
	if !b.ManualRecoveryRequired(1) {
		t.Fatal("熔断后必须标记 manual_recovery_required")
	}

	// 人工修复 ledger/候选：替换账本为可推进状态（revision_open 允许继续返工）。
	if err := st.StyleReview.Save(*guardFlowLedger(domain.ReviewStatusRevisionOpen, domain.DigestDraft(draftText))); err != nil {
		t.Fatalf("人工干预（写账本）: %v", err)
	}

	assertDispatched(t, b, st) // 状态变化：熔断解除、计数重置
	if b.ManualRecoveryRequired(1) {
		t.Fatal("人工干预后熔断必须解除（manual_recovery_required 清除）")
	}
	if reason := b.BlockedReason(1); reason != "" {
		t.Fatalf("熔断解除后 blocked reason 必须清空, got: %s", reason)
	}
	// 再次无进展 → 重新熔断（防护不失效）：重置后连续第 3 轮一致触发。
	assertDispatched(t, b, st) // repeats=2
	assertTripped(t, b, st, "", "")
}

// TestNoProgressBreaker_RecordErrorSameCodeKeepsCount 相同错误码不重置计数
// （同因失败累计无进展）；第 3 轮照常熔断。
func TestNoProgressBreaker_RecordErrorSameCodeKeepsCount(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	assertDispatched(t, b, st)
	b.RecordError(1, "chapter_fsm_denied")
	assertDispatched(t, b, st)
	b.RecordError(1, "chapter_fsm_denied")
	assertTripped(t, b, st, "", "") // 第 3 轮：相同错误码保持计数 → 熔断
}

// TestNoProgressBreaker_RecordErrorChangedCodeResets 错误码变化 = 状态变化 →
// 重置计数（不同的失败原因不累计为同一无进展序列）。
func TestNoProgressBreaker_RecordErrorChangedCodeResets(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	assertDispatched(t, b, st) // repeats=1
	b.RecordError(1, "chapter_fsm_denied")
	assertDispatched(t, b, st)      // repeats=2
	b.RecordError(1, "max_turns")   // 错误码变化 → 重置
	assertDispatched(t, b, st)      // repeats=2
	assertTripped(t, b, st, "", "") // 变化后连续第 3 轮一致 → 熔断
}

// TestNoProgressBreaker_NonWriterInstructionsUntracked 非 writer 指令（nil/终态）
// 原样透传，不参与熔断跟踪；RecordError 对未跟踪章节 no-op（不 panic）。
func TestNoProgressBreaker_NonWriterInstructionsUntracked(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("终态", 1); err != nil {
		t.Fatal(err)
	}
	// 终态 store：Route 恒返回 nil（语义停机），熔断器不得吞掉或改写 nil。
	if err := st.Progress.UpdatePhase(domain.PhaseComplete); err != nil {
		t.Fatal(err)
	}
	b := testBreaker()
	for i := 0; i < 5; i++ {
		if inst := b.Route(st); inst != nil {
			t.Fatalf("第 %d 轮：终态必须返回 nil（不被熔断器改写）, got %+v", i+1, inst)
		}
	}
	if b.ManualRecoveryRequired(1) {
		t.Fatal("非 writer 派发不得触发熔断标记")
	}
	b.RecordError(1, "worker_error") // 未跟踪章节：no-op，不 panic
}

// TestNoProgressBreaker_GuardObservesExternalInstructions 阻塞项 9.3：统一观察
// 入口 Guard 覆盖任意来源的 writer 指令（Engine initial / Arbiter reroute /
// 干预派发，不经 Route）——连续 3 轮同状态同样熔断。
func TestNoProgressBreaker_GuardObservesExternalInstructions(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	// 模拟 Arbiter reroute / 干预派发来源的 writer 指令（非 Route 产生）。
	// Guard 只预演，实际派发（Commit）才提交计数——与 Engine 的调用契约一致。
	inst := &Instruction{Agent: "writer", Task: "重写第 1 章", Chapter: 1, Reason: "arbiter reroute"}
	if got := b.Guard(st, inst); got == nil {
		t.Fatal("第 1 轮不得熔断")
	}
	b.Commit(1) // 实际派发
	if got := b.Guard(st, inst); got == nil {
		t.Fatal("第 2 轮不得熔断")
	}
	b.Commit(1) // 实际派发
	if got := b.Guard(st, inst); got != nil {
		t.Fatalf("第 3 轮必须熔断（返回 nil）, got %+v", got)
	}
	if !b.ManualRecoveryRequired(1) {
		t.Fatal("外部来源派发熔断后必须标记 manual_recovery_required")
	}
	if reason := b.BlockedReason(1); !strings.Contains(reason, "chapter=1") {
		t.Fatalf("blocked 原因必须含 chapter=1, got: %s", reason)
	}
}

// TestNoProgressBreaker_GuardNonWriterPassthrough Guard 对非 writer 指令与 nil
// 原样透传（不观察、不熔断）。
func TestNoProgressBreaker_GuardNonWriterPassthrough(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	if got := b.Guard(st, nil); got != nil {
		t.Fatalf("nil 必须原样透传, got %+v", got)
	}
	editor := &Instruction{Agent: "editor", Task: "弧末评审", Reason: "arc review"}
	for i := 0; i < 5; i++ {
		if got := b.Guard(st, editor); got != editor {
			t.Fatalf("editor 指令必须原样透传, got %+v", got)
		}
	}
	if b.ManualRecoveryRequired(1) {
		t.Fatal("非 writer 指令不得触发熔断标记")
	}
}

// TestNoProgressBreaker_ClearErrorKeepsCount 阻塞项 9.2：worker 成功后清理错误码
// 不重置无进展计数（成功但无状态变化仍累计停滞），且熔断理由不再报告旧错误码。
func TestNoProgressBreaker_ClearErrorKeepsCount(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	assertDispatched(t, b, st) // repeats=1
	b.RecordError(1, "chapter_fsm_denied")
	assertDispatched(t, b, st)      // repeats=2
	b.ClearError(1)                 // worker 成功：清理错误码（不重置计数）
	assertTripped(t, b, st, "", "") // 第 3 轮：计数未被清理重置 → 熔断
	reason := b.BlockedReason(1)
	if strings.Contains(reason, "chapter_fsm_denied") {
		t.Fatalf("熔断理由不得报告已清理的错误码, got: %s", reason)
	}
	if !strings.Contains(reason, "error_code=none") {
		t.Fatalf("熔断理由应显示 error_code=none, got: %s", reason)
	}
}

// TestNoProgressBreaker_PerChapterCheckpointSignal 建议项：进展信号按章而非全局
// ——其他章节产生新 checkpoint 不得掩盖本章停滞（全局 seq 会前进而掩盖）。
func TestNoProgressBreaker_PerChapterCheckpointSignal(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()

	assertDispatched(t, b, st)
	assertDispatched(t, b, st)
	// 其他章节（chapter 2）产生新 checkpoint：全局 seq 前进，本章 seq 不变。
	if _, err := st.Checkpoints.Append(domain.ChapterScope(2), "draft", "drafts/02.draft.md",
		domain.DigestDraft("其他章节内容")); err != nil {
		t.Fatal(err)
	}
	assertTripped(t, b, st, "", "") // 第 3 轮：本章停滞不被其他章 checkpoint 掩盖 → 熔断
}

// TestNoProgressBreaker_UncommittedRoundsDoNotCount 缺口 2：只计实际派发——
// Guard 预演后未 Commit 的轮次（trackDeadlock 咨询/reroute 等未派发分支）不
// 累计停滞计数；熔断在第 3 次同状态实际派发后触发，咨询轮不推进计数。
func TestNoProgressBreaker_UncommittedRoundsDoNotCount(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()
	inst := &Instruction{Agent: "writer", Task: "重写第 1 章", Chapter: 1}
	// guardAndCommit 模拟一轮"Guard 预演 + 实际派发提交"。
	guardAndCommit := func() *Instruction {
		got := b.Guard(st, inst)
		if got != nil {
			b.Commit(1)
		}
		return got
	}

	// R1、R2：实际派发（同状态）。
	if got := guardAndCommit(); got == nil {
		t.Fatal("R1 不得熔断")
	}
	if got := guardAndCommit(); got == nil {
		t.Fatal("R2 不得熔断")
	}
	// R3：草稿被编辑（状态变化）+ 咨询轮——Guard 预演（重置为 1）但不 Commit。
	v2 := "返工草稿 v2。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, v2); err != nil {
		t.Fatal(err)
	}
	if got := b.Guard(st, inst); got == nil {
		t.Fatal("R3 预演不得熔断（尚未派发）")
	}
	// R4：实际派发（新状态第 1 次）→ 不熔断。
	if got := guardAndCommit(); got == nil {
		t.Fatal("R4 不得熔断")
	}
	// R5：实际派发（新状态第 2 次）→ 不熔断（R3 咨询轮未被计数）。
	if got := guardAndCommit(); got == nil {
		t.Fatal("R5 不得熔断（咨询轮不计数，尚未达到 3 次实际派发）")
	}
	// R6：新状态第 3 次实际派发 → 熔断。
	if got := b.Guard(st, inst); got != nil {
		t.Fatalf("R6 必须熔断, got %+v", got)
	}
	if !b.ManualRecoveryRequired(1) {
		t.Fatal("熔断后必须标记 manual_recovery_required")
	}
}

// guardFlowLedger 构造 flow 测试用账本（经 ValidateLedger 可落盘）。
// revision_open：返工场景下 guard 允许修改，FSM 判 revision_open（恢复路径）。
func guardFlowLedger(status domain.StyleReviewStatus, digest string) *domain.StyleReviewLedger {
	const basis = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}
	now := "2026-07-25T10:00:00Z"
	cycles := []domain.StyleReviewEntry{
		{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
		{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
			Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
				Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}}},
	}
	return &domain.StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic, Cycles: cycles}
}

// TestNoProgressBreaker_ReadFailurePassesThrough 复核缺口 3：快照捕获读失败
// （store 数据损坏）→ 保守放行、不计数——连续多次派发也不得触发熔断
// （注释"任一读失败返回 ok=false（调用方保守放行）"与实现一致）。
func TestNoProgressBreaker_ReadFailurePassesThrough(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	// 损坏 style review 账本文件：ResolveChapterStage 与捕获的账本读取都会失败。
	ledgerPath := filepath.Join(st.Dir(), "meta", "style_review", "01.json")
	if err := os.WriteFile(ledgerPath, []byte("{corrupt!!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := testBreaker()
	inst := &Instruction{Agent: "writer", Task: "重写第 1 章", Chapter: 1}
	for i := 0; i < 5; i++ {
		if got := b.Guard(st, inst); got == nil {
			t.Fatalf("第 %d 轮：读失败必须保守放行（不计数、不熔断）", i+1)
		}
	}
	if b.ManualRecoveryRequired(1) {
		t.Fatal("读失败轮次不得触发 manual_recovery_required")
	}
	if reason := b.BlockedReason(1); reason != "" {
		t.Fatalf("读失败轮次不得产生 blocked 原因, got: %s", reason)
	}
}

// corruptLedger 损坏 chapter 的 style review 账本文件（捕获读取失败用）。
func corruptLedger(t *testing.T, st *storepkg.Store, chapter int) {
	t.Helper()
	ledgerPath := filepath.Join(st.Dir(), "meta", "style_review", fmt.Sprintf("%02d.json", chapter))
	if err := os.WriteFile(ledgerPath, []byte("{corrupt!!!"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNoProgressBreaker_StalePendingNotConsumedAfterCaptureFailure 回归（ora-1
// stale pending 契约）：第 N 轮 Guard 成功但未 Commit（咨询/reroute 等未派发
// 分支）遗留 pending → 下一轮同章 Guard 捕获失败（保守放行）→ Commit 不得消费
// 第 N 轮的旧观察——读失败轮次不累计停滞计数，且提交快照必为本轮 Guard 快照。
func TestNoProgressBreaker_StalePendingNotConsumedAfterCaptureFailure(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()
	inst := &Instruction{Agent: "writer", Task: "重写第 1 章", Chapter: 1}

	// R1：Guard 成功（pending 写入）但不 Commit——模拟咨询/reroute 未派发轮。
	if got := b.Guard(st, inst); got == nil {
		t.Fatal("R1 不得熔断")
	}
	if b.pending[1] == nil {
		t.Fatal("R1 Guard 后必须存在 pending 候选")
	}

	// R2：账本损坏 → Guard 捕获失败 → 保守放行；旧 pending 必须已失效。
	corruptLedger(t, st, 1)
	if got := b.Guard(st, inst); got == nil {
		t.Fatal("R2 捕获失败必须保守放行")
	}

	// R3：Engine 到实际派发点 Commit——不得消费 R1 的旧 pending。
	b.Commit(1)
	if cur := b.last[1]; cur != nil {
		t.Fatalf("捕获失败轮次不得提交旧 pending（last 不应建立）, got %+v", cur)
	}
	if b.pending[1] != nil {
		t.Fatalf("捕获失败后 pending 必须为空, got %+v", b.pending[1])
	}

	// 修复账本后：计数从 0 开始（读失败轮次不累计），第 3 次实际派发才熔断。
	if err := os.Remove(filepath.Join(st.Dir(), "meta", "style_review", "01.json")); err != nil {
		t.Fatal(err)
	}
	guardAndCommit := func() *Instruction {
		got := b.Guard(st, inst)
		if got != nil {
			b.Commit(1)
		}
		return got
	}
	if got := guardAndCommit(); got == nil {
		t.Fatal("R4 不得熔断")
	}
	if got := guardAndCommit(); got == nil {
		t.Fatal("R5 不得熔断")
	}
	if got := b.Guard(st, inst); got != nil {
		t.Fatalf("R6 必须熔断（读失败轮次未计数）, got %+v", got)
	}
	if !b.ManualRecoveryRequired(1) {
		t.Fatal("熔断后必须标记 manual_recovery_required")
	}
}

// TestNoProgressBreaker_TripClearsPending 回归（ora-1 stale pending 契约）：
// 熔断返回 nil 后该章 pending 为空——后续 Commit（即使引擎误调）不生效、
// 不改变已提交计数。
func TestNoProgressBreaker_TripClearsPending(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()
	inst := &Instruction{Agent: "writer", Task: "重写第 1 章", Chapter: 1}

	for i := 0; i < 2; i++ {
		if got := b.Guard(st, inst); got == nil {
			t.Fatalf("R%d 不得熔断", i+1)
		}
		b.Commit(1)
	}
	// R3：熔断（返回 nil）→ pending 必须为空。
	if got := b.Guard(st, inst); got != nil {
		t.Fatalf("R3 必须熔断, got %+v", got)
	}
	if p := b.pending[1]; p != nil {
		t.Fatalf("熔断后 pending 必须为空, got %+v", p)
	}
	// 后续 Commit 不生效：已提交计数保持 2，不叠加、不改变快照。
	b.Commit(1)
	if cur := b.last[1]; cur == nil || cur.repeats != 2 {
		t.Fatalf("Commit 不得消费/改变熔断轮状态, repeats=%v", cur)
	}
	if !b.ManualRecoveryRequired(1) {
		t.Fatal("熔断标记必须保持（等待人工）")
	}
}

// TestNoProgressBreaker_RouteIgnoresStalePending 回归（ora-1 stale pending 契约，
// Route 便捷组合语义一致）：Guard 成功未 Commit 遗留 pending 后，Route 的
// Guard 捕获失败 → 保守放行且不消费旧 pending（last 不建立）。
func TestNoProgressBreaker_RouteIgnoresStalePending(t *testing.T) {
	st := breakerTestStore(t, "返工草稿。她心里骂自己丢人，真不要脸。")
	b := testBreaker()
	inst := &Instruction{Agent: "writer", Task: "重写第 1 章", Chapter: 1}

	// 制造 stale pending：手动 Guard 但不 Commit（模拟咨询轮）。
	if got := b.Guard(st, inst); got == nil {
		t.Fatal("不得熔断")
	}
	// 账本损坏 → Route（Guard+Commit 便捷组合）不得消费旧 pending。
	corruptLedger(t, st, 1)
	if got := b.Route(st); got == nil {
		t.Fatal("Route 捕获失败必须保守放行（返回指令）")
	}
	if cur := b.last[1]; cur != nil {
		t.Fatalf("Route 不得消费旧 pending（last 不应建立）, got %+v", cur)
	}
	if b.ManualRecoveryRequired(1) {
		t.Fatal("读失败轮次不得触发熔断标记")
	}
}
