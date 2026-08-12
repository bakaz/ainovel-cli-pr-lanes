package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── P1-6：FSM 与 mutation guard 权威一致性 ────────────────────────────
//
// 不变量：FSM（ComputeChapterStage/ResolveChapterStage）允许的每个动作，在同一
// 状态快照下必须通过对应二级 guard（polish_draft → CheckStyleReviewMutationGuard、
// commit_chapter → CheckCommitStyleGate + CheckPolishPipelineGate）；FSM 拒绝的
// 动作 guard 必须同样拒绝（拒绝原因一致）。
//
// P1-5 已在 FSM 侧修复缺口（terminal mismatch 早于 pipeline freshness 判定），
// 本文件用真实 store 验证"同一快照"下 FSM 与 guard 的联合行为。

// setupFSMGuardStore 构造 P1-6 一致性测试的基础 store：critic 模式 + 草稿 +
// consistency checkpoint（seq=1）。polish checkpoint 与账本由各测试按需追加。
// 放宽章节字数规则避免短测试草稿触发 chapter_words 机械 error。
func setupFSMGuardStore(t *testing.T, draftText string) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	savePermissiveUserRules(t, st)
	if err := st.Drafts.SaveDraft(1, draftText); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	digest := domain.DigestDraft(draftText)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatalf("Append consistency checkpoint: %v", err)
	}
	return st
}

// appendPolish 追加一条 polish checkpoint 并返回其 seq。
func appendPolish(t *testing.T, st *store.Store, digest string, meta domain.PolishCheckpointMeta) int64 {
	t.Helper()
	cp, err := st.Checkpoints.AppendPolish(domain.ChapterScope(1), "polish", "a1", digest, meta)
	if err != nil {
		t.Fatalf("AppendPolish: %v", err)
	}
	return cp.Seq
}

// appendPostPolishCheck 追加一条必不幂等去重的 post-polish consistency checkpoint
// （Append 会按 Scope+Step+Digest 幂等去重，同 digest 的第二次追加不会产生新 seq，
// 必须用 AppendAlways 保证 seq > polish seq）。
func appendPostPolishCheck(t *testing.T, st *store.Store, digest string) {
	t.Helper()
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", digest); err != nil {
		t.Fatalf("AppendAlways consistency: %v", err)
	}
}

// setupFSMGuardStoreNoDraft 构造无草稿的一致性测试 store（阻塞项 8：terminal
// ledger + 草稿丢失）：critic 模式 + 可选账本 + 可选重写队列标记（无草稿/
// 无 checkpoint）。
func setupFSMGuardStoreNoDraft(t *testing.T, ledger *domain.StyleReviewLedger, rewriteQueue bool) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	savePermissiveUserRules(t, st)
	if rewriteQueue {
		if err := st.Progress.Init("重写测试", 10); err != nil {
			t.Fatalf("Progress.Init: %v", err)
		}
		if err := st.Drafts.SaveFinalChapter(1, "原终稿。"); err != nil {
			t.Fatalf("SaveFinalChapter: %v", err)
		}
		if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
			t.Fatalf("MarkChapterComplete: %v", err)
		}
		if err := st.Progress.SetPendingRewrites([]int{1}, "重写测试"); err != nil {
			t.Fatalf("SetPendingRewrites: %v", err)
		}
		if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
			t.Fatalf("SetFlow: %v", err)
		}
	}
	if ledger != nil {
		if err := st.StyleReview.Save(*ledger); err != nil {
			t.Fatalf("Save ledger: %v", err)
		}
	}
	return st
}

// guardConsistencyLedger 构造可落盘的账本（绑定 digest 与 polish seq；seq=0 表示
// 不绑定）。结构经 ValidateLedger 校验（与 rewriteLedger 同构）。
func guardConsistencyLedger(status domain.StyleReviewStatus, digest string, polishSeq int64) *domain.StyleReviewLedger {
	const basis = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: polishSeq}
	now := "2026-07-25T10:00:00Z"
	cycles := []domain.StyleReviewEntry{
		{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
	}
	switch status {
	case domain.ReviewStatusAcceptedInitial:
		cycles = append(cycles, domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
			Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}})
	case domain.ReviewStatusAcceptedRev:
		cycles = append(cycles,
			domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}}},
			domain.StyleReviewEntry{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: now,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
			domain.StyleReviewEntry{Cycle: 4, Status: domain.ReviewStatusAcceptedRev, CreatedAt: now,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}})
	case domain.ReviewStatusDegraded:
		cycles = append(cycles, domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
			Error: "critic call failed"})
	case domain.ReviewStatusInitialPending:
		// 保持单周期 pending
	}
	return &domain.StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic, Cycles: cycles}
}

// assertFSMBlocked 断言 ResolveChapterStage 判为 blocked：不允许任何动作，
// blocked 必须携带人工恢复指引。
func assertFSMBlocked(t *testing.T, st *store.Store, cfg ChapterFSMConfig) ChapterStageDecision {
	t.Helper()
	d, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatalf("ResolveChapterStage: %v", err)
	}
	if d.Stage != ChapterStageBlocked {
		t.Fatalf("FSM stage = %s, want blocked; reason=%q recovery=%q", d.Stage, d.Reason, d.Recovery)
	}
	if len(d.Allowed) != 0 {
		t.Fatalf("blocked 不得允许任何动作, got %v", d.Allowed)
	}
	if d.Recovery == "" {
		t.Fatal("blocked 必须携带人工恢复指引")
	}
	return d
}

// TestFSMGuardConsistency_TerminalMismatchStalePolish 覆盖 ch450 死锁组合：
// accepted_revised/accepted_initial + ledger digest 与当前草稿不匹配 + stale polish。
// P1-5 后 FSM 判 blocked（不再 needs_polish），mutation guard 同样拒绝修改——
// "FSM 拒绝的动作 guard 拒绝原因一致"，且 polish_draft 工具入口以 blocked 拦截。
func TestFSMGuardConsistency_TerminalMismatchStalePolish(t *testing.T) {
	for _, status := range []domain.StyleReviewStatus{
		domain.ReviewStatusAcceptedInitial, domain.ReviewStatusAcceptedRev,
	} {
		t.Run(string(status), func(t *testing.T) {
			oldDigest := dig("旧候选正文")
			draftV2 := mechCleanDraft("当前草稿版本二。")
			st := setupFSMGuardStore(t, draftV2)
			appendPolish(t, st, oldDigest, domain.PolishCheckpointMeta{
				InputDigest: oldDigest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
			})
			if err := st.StyleReview.Save(*guardConsistencyLedger(status, oldDigest, 0)); err != nil {
				t.Fatalf("Save ledger: %v", err)
			}

			cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}
			// FSM：blocked（不是 needs_polish——ch450 死锁根因修复）。
			d := assertFSMBlocked(t, st, cfg)
			if !strings.Contains(d.Reason, "不匹配") {
				t.Fatalf("blocked reason 必须说明 ledger 与草稿不匹配, got %q", d.Reason)
			}
			if d.Allows(ChapterActionPolish) || d.Allows(ChapterActionDraft) || d.Allows(ChapterActionEdit) {
				t.Fatalf("blocked 不得允许 polish/draft/edit, allowed=%v", d.Allowed)
			}

			// guard：terminal 锁定拒绝修改（与 FSM 拒绝一致）。
			err := CheckStyleReviewMutationGuard(st, 1)
			if err == nil {
				t.Fatal("mutation guard 必须拒绝 terminal 锁定章节的修改")
			}
			if !strings.Contains(err.Error(), "不允许修改") {
				t.Fatalf("guard 拒绝原因不符, got: %v", err)
			}

			// 工具入口：polish_draft 被 FSM 以 blocked 拦截（此前是 needs_polish →
			// 模型照 required 调用 polish_draft → guard 拒绝 → 无限重派）。
			perr := RequireChapterAction(st, 1, ChapterActionPolish, cfg)
			if perr == nil {
				t.Fatal("polish_draft 必须被 blocked 拦截")
			}
			var te *ChapterTransitionError
			if !errors.As(perr, &te) || te.Stage != ChapterStageBlocked {
				t.Fatalf("拦截必须是 blocked 阶段错误, got %v", perr)
			}
		})
	}
}

// TestFSMGuardConsistency_TerminalMismatchFreshPolish 对照：terminal mismatch 但
// polish 新鲜（digest 匹配当前草稿）→ FSM blocked（绑定校验失败路径），
// CheckPolishPipelineGate 同因拒绝 commit——拒绝原因一致。
func TestFSMGuardConsistency_TerminalMismatchFreshPolish(t *testing.T) {
	draft := mechCleanDraft("已评审候选正文。")
	d := dig(draft)
	st := setupFSMGuardStore(t, draft)
	if err := st.StyleReview.Save(*guardConsistencyLedger(domain.ReviewStatusAcceptedInitial, d, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	// 评审先于 polish 完成（legacy 墙钟回退路径的边界：CreatedAt 固定为 2026-07-25，
	// polish 是当前时刻 → critic 早于 polish 超过 1s → 未绑定）。
	appendPolish(t, st, d, domain.PolishCheckpointMeta{
		InputDigest: d, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
	})
	// 补一条更新的 consistency checkpoint（seq > polish seq；同 digest 必须
	// AppendAlways，Append 会幂等去重）。
	appendPostPolishCheck(t, st, d)

	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}
	d2 := assertFSMBlocked(t, st, cfg)

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("pipeline gate 必须拒绝未绑定的 terminal 候选 commit")
	}
	if !strings.Contains(err.Error(), "重新执行 polish_draft") {
		t.Fatalf("gate 拒绝原因不符（应要求重新走流水线）, got: %v", err)
	}
	if d2.Reason == "" {
		t.Fatal("FSM blocked reason 不得为空")
	}
}

// TestFSMGuardConsistency_TerminalInFlightPolish 覆盖"terminal + concurrent
// in-flight polish（polish checkpoint 比 ledger 绑定更新）"：FSM blocked 且
// CheckPolishPipelineGate 拒绝（R != P）——两侧一致拒绝 commit。
func TestFSMGuardConsistency_TerminalInFlightPolish(t *testing.T) {
	draft := mechCleanDraft("已终审候选。")
	d := dig(draft)
	st := setupFSMGuardStore(t, draft)
	boundSeq := appendPolish(t, st, d, domain.PolishCheckpointMeta{
		InputDigest: d, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
	})
	if err := st.StyleReview.Save(*guardConsistencyLedger(domain.ReviewStatusAcceptedInitial, d, boundSeq)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	// in-flight polish 完成：更新的 polish checkpoint（同一 digest，新 seq）。
	appendPolish(t, st, d, domain.PolishCheckpointMeta{
		InputDigest: d, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
	})
	// 对应的 post-polish check（seq > 最新 polish seq）。
	appendPostPolishCheck(t, st, d)

	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}
	assertFSMBlocked(t, st, cfg)

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("pipeline gate 必须拒绝：评审绑定 seq 已不是当前 polish（R != P）")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("gate 拒绝原因应指向绑定 seq 不一致, got: %v", err)
	}
}

// TestFSMGuardConsistency_PendingInFlightPolish 覆盖"pending review + in-flight
// polish completion"：FSM 允许 review（needs_review），commit 级 guard
// （CheckCommitStyleGate 活跃拒绝 + CheckPolishPipelineGate 绑定不一致拒绝）
// 同样拒绝 commit——FSM 允许的动作 guard 不拒绝（review 无 guard 拦截），
// FSM 拒绝的动作（commit）guard 拒绝原因一致。
func TestFSMGuardConsistency_PendingInFlightPolish(t *testing.T) {
	draft := mechCleanDraft("待评审候选。")
	d := dig(draft)
	st := setupFSMGuardStore(t, draft)
	boundSeq := appendPolish(t, st, d, domain.PolishCheckpointMeta{
		InputDigest: d, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
	})
	if err := st.StyleReview.Save(*guardConsistencyLedger(domain.ReviewStatusInitialPending, d, boundSeq)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	// in-flight polish 完成（新 seq）。
	appendPolish(t, st, d, domain.PolishCheckpointMeta{
		InputDigest: d, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
	})
	appendPostPolishCheck(t, st, d)

	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}
	fsm, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatalf("ResolveChapterStage: %v", err)
	}
	if fsm.Stage != ChapterStageNeedsReview {
		t.Fatalf("FSM stage = %s, want needs_review（pending 评审绑定当前草稿）", fsm.Stage)
	}
	if !fsm.Allows(ChapterActionReview) || fsm.Allows(ChapterActionCommit) {
		t.Fatalf("FSM 应只允许 review, allowed=%v", fsm.Allowed)
	}

	// commit 双闸全部拒绝（与 FSM 拒绝 commit 一致）。
	if err := CheckCommitStyleGate(st, 1); err == nil {
		t.Fatal("CheckCommitStyleGate 必须拒绝 pending 状态的 commit")
	}
	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err == nil {
		t.Fatal("CheckPolishPipelineGate 必须拒绝绑定 seq 不一致的 commit")
	}
}

// TestFSMGuardConsistency_DegradedGuardAllowsWhatFSMAllows 不变量正方向：
// degraded 账本 + digest 不匹配 + stale polish → FSM 判 needs_polish（允许 polish），
// mutation guard 必须放行（degraded 是瞬态故障语义，非锁定）——FSM allowed 的
// 动作在同一快照下 guard 不拒绝。
func TestFSMGuardConsistency_DegradedGuardAllowsWhatFSMAllows(t *testing.T) {
	oldDigest := dig("旧候选正文")
	draftV2 := mechCleanDraft("当前草稿版本二。")
	st := setupFSMGuardStore(t, draftV2)
	appendPolish(t, st, oldDigest, domain.PolishCheckpointMeta{
		InputDigest: oldDigest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false,
	})
	if err := st.StyleReview.Save(*guardConsistencyLedger(domain.ReviewStatusDegraded, oldDigest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}
	fsm, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatalf("ResolveChapterStage: %v", err)
	}
	if fsm.Stage != ChapterStageNeedsPolish {
		t.Fatalf("FSM stage = %s, want needs_polish（degraded 不被 P1-5 误伤）", fsm.Stage)
	}
	if !fsm.Allows(ChapterActionPolish) {
		t.Fatalf("FSM 必须允许 polish, allowed=%v", fsm.Allowed)
	}
	if err := CheckStyleReviewMutationGuard(st, 1); err != nil {
		t.Fatalf("mutation guard 必须放行 degraded 章节的 polish（FSM 允许的动作 guard 不拒绝）: %v", err)
	}
}

// TestFSMGuardConsistency_DuplicateSeqFailClosed 覆盖"duplicate checkpoint seq +
// review binding（BySeq 报错路径）"：重复 seq 是数据损坏，store 启动即 fail-closed
// （P0-2），FSM 与 guard 都无法在损坏的 seq 空间上运行——两侧拒绝行为一致
// （BySeq 对重复 seq 返回明确错误而非任取一条，见 store 层 TestCheckpointStore_BySeqDuplicate）。
func TestFSMGuardConsistency_DuplicateSeqFailClosed(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		`{"seq":1,"scope":{"kind":"chapter","chapter":1},"step":"plan","occurred_at":"2026-01-01T00:00:00Z"}`,
		`{"seq":1,"scope":{"kind":"chapter","chapter":1},"step":"polish","occurred_at":"2026-01-01T00:00:01Z"}`,
	}
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	jsonlPath := filepath.Join(dir, "meta", "checkpoints.jsonl")
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonlPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.NewStore(dir)
	err := st.Init()
	if err == nil {
		t.Fatal("重复 seq 数据损坏必须 fail-closed 拒绝启动（FSM 与 guard 均不可运行）")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("启动错误应指向重复 seq 数据损坏, got: %v", err)
	}
}

// ── 阻塞项 8：terminal ledger + 草稿丢失的 FSM/guard 一致性 ───────────
// FSM 判 blocked（不再 needs_draft），mutation guard 同样拒绝 draft_chapter——
// 拒绝原因一致；返工队列 terminal + 无草稿与 degraded + 无草稿两侧都放行。

// TestFSMGuardConsistency_TerminalNoDraft 非返工 terminal ledger + 草稿丢失：
// FSM blocked + guard 拒绝 draft + 工具入口以 blocked 拦截。
func TestFSMGuardConsistency_TerminalNoDraft(t *testing.T) {
	for _, status := range []domain.StyleReviewStatus{
		domain.ReviewStatusAcceptedInitial, domain.ReviewStatusAcceptedRev,
	} {
		t.Run(string(status), func(t *testing.T) {
			st := setupFSMGuardStoreNoDraft(t, guardConsistencyLedger(status, dig("已丢失草稿"), 0), false)
			cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}

			d := assertFSMBlocked(t, st, cfg)
			if !strings.Contains(d.Recovery, "不一致") {
				t.Fatalf("blocked recovery 必须说明 terminal 账本与草稿不一致, got %q", d.Recovery)
			}
			if d.Allows(ChapterActionDraft) {
				t.Fatalf("blocked 不得允许 draft_chapter, allowed=%v", d.Allowed)
			}

			// guard：terminal 锁定拒绝修改（与 FSM 拒绝一致）。
			err := CheckStyleReviewMutationGuard(st, 1)
			if err == nil {
				t.Fatal("mutation guard 必须拒绝 terminal 锁定章节的 draft")
			}
			if !strings.Contains(err.Error(), "不允许修改") {
				t.Fatalf("guard 拒绝原因不符, got: %v", err)
			}

			// 工具入口：draft_chapter 被 FSM 以 blocked 拦截（不再 needs_draft）。
			perr := RequireChapterAction(st, 1, ChapterActionDraft, cfg)
			if perr == nil {
				t.Fatal("draft_chapter 必须被 blocked 拦截")
			}
			var te *ChapterTransitionError
			if !errors.As(perr, &te) || te.Stage != ChapterStageBlocked {
				t.Fatalf("拦截必须是 blocked 阶段错误, got %v", perr)
			}
		})
	}
}

// TestFSMGuardConsistency_RewriteTerminalNoDraft 返工队列 terminal + 无草稿：
// FSM rewrite_not_started（允许 draft/edit），guard 放行重写开始（rewriteNotStarted
// + terminal）——FSM allowed 的动作 guard 不拒绝。
func TestFSMGuardConsistency_RewriteTerminalNoDraft(t *testing.T) {
	st := setupFSMGuardStoreNoDraft(t, guardConsistencyLedger(domain.ReviewStatusAcceptedInitial, dig("已丢失草稿"), 0), true)
	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}

	fsm, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatalf("ResolveChapterStage: %v", err)
	}
	if fsm.Stage != ChapterStageRewriteNotStarted {
		t.Fatalf("FSM stage = %s, want rewrite_not_started（返工草稿尚未播种）", fsm.Stage)
	}
	if !fsm.Allows(ChapterActionDraft) || !fsm.Allows(ChapterActionEdit) {
		t.Fatalf("FSM 必须允许 draft/edit 开始重写, allowed=%v", fsm.Allowed)
	}
	if err := CheckStyleReviewMutationGuard(st, 1); err != nil {
		t.Fatalf("guard 必须放行返工开始（rewriteNotStarted + terminal）, got: %v", err)
	}
}

// TestFSMGuardConsistency_DegradedNoDraft degraded + 无草稿：FSM needs_draft
// （允许起草），guard 放行（degraded 是瞬态故障语义）——FSM allowed 的动作
// guard 不拒绝。
func TestFSMGuardConsistency_DegradedNoDraft(t *testing.T) {
	st := setupFSMGuardStoreNoDraft(t, guardConsistencyLedger(domain.ReviewStatusDegraded, dig("已丢失草稿"), 0), false)
	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true}

	fsm, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatalf("ResolveChapterStage: %v", err)
	}
	if fsm.Stage != ChapterStageNeedsDraft {
		t.Fatalf("FSM stage = %s, want needs_draft（degraded 允许起草）", fsm.Stage)
	}
	if !fsm.Allows(ChapterActionDraft) {
		t.Fatalf("FSM 必须允许 draft, allowed=%v", fsm.Allowed)
	}
	if err := CheckStyleReviewMutationGuard(st, 1); err != nil {
		t.Fatalf("guard 必须放行 degraded 章节的起草（FSM 允许的动作 guard 不拒绝）: %v", err)
	}
}
