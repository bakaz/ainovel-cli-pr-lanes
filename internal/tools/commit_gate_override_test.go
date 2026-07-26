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
	draft := "正文内容。用于验证覆盖后可以提交。"
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
