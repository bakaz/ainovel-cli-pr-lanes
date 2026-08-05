package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// TestCommitGate_ExhaustedBlocksCommit 验证 exhausted 状态拒绝 commit。
func TestCommitGate_ExhaustedBlocksCommit(t *testing.T) {
	st := newCriticStoreWithExhaustedLedger(t, 1, "正文。")

	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("commit should be blocked when ledger is exhausted")
	}
	if !strings.Contains(err.Error(), "exhausted") && !strings.Contains(err.Error(), "评审已耗尽") {
		t.Errorf("expected exhausted rejection, got: %v", err)
	}
}

// TestCommitGate_OverriddenAllowsCommit 验证 overridden 状态允许 commit。
func TestCommitGate_OverriddenAllowsCommit(t *testing.T) {
	draft := "正文内容。用于验证覆盖后可以提交。她心里骂自己丢人，真不要脸。"
	st := newCriticStoreWithExhaustedLedger(t, 1, draft)

	// 追加 overridden 条目
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := "2026-07-25T13:00:00Z"
	if err := st.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		nextCycle := len(cur.Cycles) + 1
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle:       nextCycle,
			Status:      domain.ReviewStatusOverridden,
			CreatedAt:   now,
			AttemptID:   "override-test",
			Request:     &domain.StyleReviewRequest{Prompt: "test-v1"},
			DraftDigest: draftDigest,
			BasisDigest: basisDigest,
			Override: &domain.StyleReviewOverride{
				Actor:        "user",
				Reason:       "测试覆盖",
				DraftDigest:  draftDigest,
				BasisDigest:  basisDigest,
				OverriddenAt: now,
			},
		})
		return cur, nil
	}); err != nil {
		t.Fatalf("append overridden: %v", err)
	}

	// 验证账本状态为 overridden
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusOverridden {
		t.Fatalf("expected overridden, got %s", ledger.CurrentStatus())
	}

	// commit 应通过
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "覆盖后的提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("commit should succeed after override, got: %v", err)
	}
}

// TestCommitGate_CommitRequiresCurrentDigest 验证修改草稿后未重新审核拒绝 commit。
func TestCommitGate_CommitRequiresCurrentDigest(t *testing.T) {
	draft := "初始正文。"
	st := newCriticStoreWithExhaustedLedger(t, 1, draft)

	// 追加 overridden
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := "2026-07-25T13:00:00Z"
	if err := st.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		nextCycle := len(cur.Cycles) + 1
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle: nextCycle, Status: domain.ReviewStatusOverridden,
			CreatedAt: now, AttemptID: "override-test",
			Request:     &domain.StyleReviewRequest{Prompt: "test-v1"},
			DraftDigest: draftDigest, BasisDigest: basisDigest,
			Override: &domain.StyleReviewOverride{
				Actor: "user", Reason: "测试", DraftDigest: draftDigest,
				BasisDigest: basisDigest, OverriddenAt: now,
			},
		})
		return cur, nil
	}); err != nil {
		t.Fatalf("append overridden: %v", err)
	}

	// 修改草稿（摘要不匹配）
	if err := st.Drafts.SaveDraft(1, "修改后的正文。与 overridden 条目摘要不同。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("commit should be rejected when draft digest doesn't match override entry")
	}
	if !strings.Contains(err.Error(), "摘要") && !strings.Contains(err.Error(), "digest") {
		t.Errorf("expected digest mismatch error, got: %v", err)
	}
}

// ── Freshness regression tests ────────────────────────────────────────
//
// 1. off 模式：check A → edit B → commit rejected（stale checkpoint 即使在 off 模式也被拦截）
func TestCommitGate_OffModeStaleCheckpointRejectsCommit(t *testing.T) {
	draftA := "版本 A 的正文。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	// off 模式（不设 critic mode）
	if err := st.Drafts.SaveDraft(1, draftA); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	// 创建 checkpoint（digest 匹配 draftA）
	draftDigestA := domain.DigestDraft(draftA)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check",
		"test-artifact", draftDigestA,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}
	// edit B：修改草稿但不更新 checkpoint
	if err := st.Drafts.SaveDraft(1, "版本 B 的正文内容已变更。"); err != nil {
		t.Fatalf("SaveDraft B: %v", err)
	}
	// commit 应被 checkpoint freshness 校验拦截
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("off mode: stale checkpoint should still reject commit (freshness before mode)")
	}
	if !strings.Contains(err.Error(), "草稿已变更") && !strings.Contains(err.Error(), "一致性检查") {
		t.Errorf("expected checkpoint freshness error, got: %v", err)
	}
}

// 2. off 模式：check A → edit B → check B → commit succeeds
func TestCommitGate_OffModeFreshCheckpointAllowsCommit(t *testing.T) {
	draftA := "版本 A。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, draftA); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	draftDigestA := domain.DigestDraft(draftA)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check",
		"a1", draftDigestA,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}
	// 编辑为 B
	draftB := "版本 B。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, draftB); err != nil {
		t.Fatalf("SaveDraft B: %v", err)
	}
	// 重新 check
	draftDigestB := domain.DigestDraft(draftB)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check",
		"a2", draftDigestB,
	); err != nil {
		t.Fatalf("Append checkpoint B: %v", err)
	}
	// commit 应通过
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("off mode: fresh checkpoint should allow commit, got: %v", err)
	}
}

// 3. critic 模式 + terminal ledger + stale digest → freshness 拦截（在 ledger 检查之前）
func TestCommitGate_CriticTerminalLedgerStaleDigestRejects(t *testing.T) {
	draftA := "版本 A。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, draftA); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	draftDigestA := domain.DigestDraft(draftA)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check",
		"a1", draftDigestA,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}
	// 创建 terminal ledger（accepted_initial）
	now := "2026-07-26T10:00:00Z"
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: draftDigestA, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: draftDigestA, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test-model"},
				Result: &domain.StyleReviewResult{
					Verdict:  domain.ReviewVerdictPass,
					Evidence: "ok",
				}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	// 修改草稿（checkpoint 变 stale）
	if err := st.Drafts.SaveDraft(1, "版本 B——与 checkpoint 不一致。"); err != nil {
		t.Fatalf("SaveDraft B: %v", err)
	}
	// commit 应因 checkpoint freshness 失败，而非 style error
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("critic + stale checkpoint should reject commit before ledger check")
	}
	// 拒绝原因是 checkpoint freshness，而非 ledger style 语义
	if !strings.Contains(err.Error(), "草稿已变更") && !strings.Contains(err.Error(), "一致性检查") {
		t.Errorf("expected checkpoint freshness error (draft changed), got style-ledger error: %v", err)
	}
}

// 4. critic 模式 + rewrite queue + stale digest → freshness 拦截（绕过 rewrite queue 捷径）
func TestCommitGate_RewriteQueueStaleDigestRejects(t *testing.T) {
	draftA := "已完成终稿。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, draftA); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, draftA); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "重写"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	// 创建与 draftA 匹配的 checkpoint
	draftDigestA := domain.DigestDraft(draftA)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check",
		"a1", draftDigestA,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}
	// 编辑草稿（不更新 checkpoint）
	if err := st.Drafts.SaveDraft(1, "重写版本——与 checkpoint 不一致。"); err != nil {
		t.Fatalf("SaveDraft rewrite: %v", err)
	}
	// commit 应因 checkpoint freshness 失败（即使 rewrite queue）
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "重写提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
		// 批次 4：mode 校验先于 gate 执行，需显式声明才能到达 freshness 校验。
		"world_state_mode": "preserve",
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("rewrite queue with stale checkpoint should be rejected by freshness check")
	}
	if !strings.Contains(err.Error(), "草稿已变更") && !strings.Contains(err.Error(), "一致性检查") {
		t.Errorf("expected checkpoint freshness error, got: %v", err)
	}
}

// 5. off 模式 + 无 checkpoint → 允许 commit（兼容边界）
func TestCommitGate_OffModeNoCheckpointAllowsCommit(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, "一些正文。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	// 不创建任何 checkpoint
	// commit 应通过（off 模式无 checkpoint = 允许）
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("off mode + no checkpoint should allow commit, got: %v", err)
	}
}

// newCriticStoreWithExhaustedLedger 创建含 exhausted 账本的测试 store。
func newCriticStoreWithExhaustedLedger(t *testing.T, chapter int, draft string) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	if draft != "" {
		if err := st.Drafts.SaveDraft(chapter, draft); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
	}
	draftDigest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(chapter), "consistency_check",
		"test-artifact", draftDigest,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}

	basisDigest := ComputeBasisDigest(st, chapter, "test-v1")
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: chapter, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
				AttemptID: "a1", Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test"},
				Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "revise", Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a1", Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 4, Status: domain.ReviewStatusExhausted, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a1", Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test"},
				Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "final revise", Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "error", Evidence: "e"}}},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	return st
}
