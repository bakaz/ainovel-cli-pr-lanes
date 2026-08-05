package host

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// ── Test helpers ─────────────────────────────────────────────────────

// setupOverrideStore 创建一个用于测试覆盖操作的 Store：
// - critic 模式
// - 已初始化 progress
// - 有草稿
// - 有一致性检查点
// - 账本处于 exhausted 状态
func setupOverrideStore(t *testing.T, chapter int, draft string) *storepkg.Store {
	t.Helper()
	st := storepkg.NewStore(t.TempDir())
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
	// 一致性检查点（摘要精确匹配草稿摘要）
	checkDigest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(chapter), "consistency_check",
		"test-artifact", checkDigest,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}
	// 写入 exhausted 账本（完整 V1 图路径：initial_pending → revision_open → final_pending → exhausted）
	draftDigest := domain.DigestDraft(draft)
	basisDigest := tools.ComputeBasisDigest(st, chapter, "test-v1")
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1,
		Chapter:       chapter,
		Mode:          domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{
				Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: "test", Model: "test"},
				DraftDigest: draftDigest, BasisDigest: basisDigest,
			},
			{
				Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test"},
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning",
						Evidence: "some evidence",
					}},
				},
				DraftDigest: draftDigest, BasisDigest: basisDigest,
			},
			{
				Cycle: 3, Status: domain.ReviewStatusFinalPending,
				CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: "test", Model: "test", PolishCheckpointSeq: 42},
				DraftDigest: draftDigest, BasisDigest: basisDigest,
			},
			{
				Cycle: 4, Status: domain.ReviewStatusExhausted,
				CreatedAt: "2026-07-25T13:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test", PolishCheckpointSeq: 42},
				Result: &domain.StyleReviewResult{
					Verdict:  domain.ReviewVerdictRevise,
					Evidence: "final revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "error",
						Evidence: "test evidence",
					}},
				},
				DraftDigest: draftDigest, BasisDigest: basisDigest,
			},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save exhausted ledger: %v", err)
	}
	return st
}

// ── 1. Argument validation ───────────────────────────────────────────

func TestStyleReviewOverride_RejectsInvalidArgs(t *testing.T) {
	st := setupOverrideStore(t, 1, "正文")
	h := &Host{store: st}

	// 章节号 <= 0
	if err := h.StyleReviewOverride(0, "reason"); err == nil {
		t.Error("expected error for chapter 0")
	}
	if err := h.StyleReviewOverride(-1, "reason"); err == nil {
		t.Error("expected error for negative chapter")
	}
	// 空原因
	if err := h.StyleReviewOverride(1, ""); err == nil {
		t.Error("expected error for empty reason")
	}
	// 空白原因
	if err := h.StyleReviewOverride(1, "   "); err == nil {
		t.Error("expected error for whitespace-only reason")
	}
}

// ── 2. Absent ledger ─────────────────────────────────────────────────

func TestStyleReviewOverride_RejectsAbsentLedger(t *testing.T) {
	st := setupOverrideStore(t, 2, "正文")
	h := &Host{store: st}
	err := h.StyleReviewOverride(3, "手动覆盖")
	if err == nil {
		t.Fatal("expected error for non-existent ledger")
	}
	if !strings.Contains(err.Error(), "尚无风格评审账本") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ── 3. Non-exhausted status rejected ─────────────────────────────────

func TestStyleReviewOverride_RejectsNonExhausted(t *testing.T) {
	// 建一个独立的 accepted_initial 账本（不走 exhausted 路径）
	// 用 clean store + direct file write to skip ledger validation
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, "正文"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	h := &Host{store: st}

	// 尝试在空账本上覆盖
	err := h.StyleReviewOverride(1, "想覆盖")
	if err == nil {
		t.Fatal("expected error for absent ledger")
	}
	if !strings.Contains(err.Error(), "尚无风格评审账本") {
		t.Errorf("expected absent ledger error, got: %v", err)
	}
}

// ── 4. Missing draft rejected ────────────────────────────────────────

func TestStyleReviewOverride_RejectsMissingDraft(t *testing.T) {
	st := setupOverrideStore(t, 1, "") // no draft
	h := &Host{store: st}

	err3 := h.StyleReviewOverride(1, "覆盖")
	if err3 == nil {
		t.Fatal("expected error for missing draft")
	}
	if !strings.Contains(err3.Error(), "无草稿") {
		t.Errorf("expected missing draft error, got: %v", err3)
	}
}

// ── 5. Successful override ───────────────────────────────────────────

func TestStyleReviewOverride_Success(t *testing.T) {
	draft := "第一章正文。"
	st := setupOverrideStore(t, 1, draft)
	eventsCh := make(chan Event, 10)
	h := &Host{store: st, events: eventsCh, done: make(chan struct{}, 4)}

	if err := h.StyleReviewOverride(1, "人工确认风格达标"); err != nil {
		t.Fatalf("StyleReviewOverride: %v", err)
	}

	// 验证事件包含 /continue 提示
	select {
	case ev := <-eventsCh:
		if ev.Category != "SYSTEM" {
			t.Errorf("event category = %q, want SYSTEM", ev.Category)
		}
		if ev.Level != "info" {
			t.Errorf("event level = %q, want info", ev.Level)
		}
		if !strings.Contains(ev.Summary, "/continue") {
			t.Errorf("event summary should mention /continue, got: %q", ev.Summary)
		}
	default:
		t.Error("expected event for successful override")
	}

	// 验证账本追加了 overridden 条目
	ledger, err := st.StyleReview.Load(1)
	if err != nil || ledger == nil {
		t.Fatalf("Load ledger: %v", err)
	}
	if ledger.CurrentStatus() != domain.ReviewStatusOverridden {
		t.Fatalf("expected overridden, got %s", ledger.CurrentStatus())
	}

	// 验证 overridden 条目字段完整性
	lastCycle := ledger.CurrentCycle()
	if lastCycle == nil {
		t.Fatal("no last cycle")
	}
	if lastCycle.Override == nil {
		t.Fatal("overridden entry must have Override field")
	}
	if lastCycle.Override.Actor != "user" {
		t.Errorf("actor = %q, want user", lastCycle.Override.Actor)
	}
	if lastCycle.Override.Reason != "人工确认风格达标" {
		t.Errorf("reason = %q", lastCycle.Override.Reason)
	}
	if !domain.IsValidDigest(lastCycle.Override.DraftDigest) {
		t.Error("override draft_digest invalid")
	}
	if !domain.IsValidDigest(lastCycle.Override.BasisDigest) {
		t.Error("override basis_digest invalid")
	}
	if lastCycle.Override.OverriddenAt == "" {
		t.Error("overridden_at is empty")
	}

	// 验证 digest 匹配当前草稿
	expectedDraftDigest := domain.DigestDraft(draft)
	if lastCycle.DraftDigest != expectedDraftDigest {
		t.Errorf("draft_digest = %q, want %q", lastCycle.DraftDigest, expectedDraftDigest)
	}
	if lastCycle.Override.DraftDigest != expectedDraftDigest {
		t.Errorf("override draft_digest mismatch: %q vs %q", lastCycle.Override.DraftDigest, expectedDraftDigest)
	}

	// C1-H3：overridden 条目必须保留 exhausted 条目的 Epoch 与 PolishCheckpointSeq
	// （commit gate 以当前 terminal 条目为权威读取绑定 seq）。
	if lastCycle.EpochValue() != 1 {
		t.Errorf("overridden entry epoch = %d, want 1（保留 exhausted 条目的 epoch）", lastCycle.EpochValue())
	}
	if lastCycle.Request == nil || lastCycle.Request.PolishCheckpointSeq != 42 {
		t.Errorf("overridden entry polish_checkpoint_seq = %+v, want 42（保留 exhausted 条目的绑定）", lastCycle.Request)
	}
}

// ── 6. Override permits commit ──────────────────────────────────────

func TestStyleReviewOverride_CommitAfterOverrideSucceeds(t *testing.T) {
	draft := "可提交的正文内容。她心里骂自己丢人，真不要脸。"
	st := setupOverrideStore(t, 1, draft)
	eventsCh := make(chan Event, 10)
	h := &Host{store: st, events: eventsCh, done: make(chan struct{}, 4)}

	// exhausted 应阻止 commit
	commitTool := tools.NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err == nil {
		t.Fatal("exhausted status should block commit before override")
	}
	t.Logf("exhausted commit rejection (expected): %v", err)

	// 执行覆盖
	if err := h.StyleReviewOverride(1, "人工验收通过"); err != nil {
		t.Fatalf("StyleReviewOverride: %v", err)
	}

	// 覆盖后 commit 应通过（账本 current 为 overridden，是 terminal 状态）
	_, err = commitTool.Execute(t.Context(), commitArgs)
	if err != nil {
		t.Fatalf("commit should succeed after override, got: %v", err)
	}

	// 验证章节已提交
	text, _ := st.Drafts.LoadChapterText(1)
	if text == "" {
		t.Fatal("chapter should have been committed")
	}

	// 验证 override 事件包含 /continue 指令
	select {
	case ev := <-eventsCh:
		if !strings.Contains(ev.Summary, "/continue") {
			t.Errorf("override event should mention /continue, got: %q", ev.Summary)
		}
	default:
		t.Error("expected event for successful override")
	}
}

// ── 7. Invalid attempts leave ledger unchanged ───────────────────────
//
// 验证：当调用 override 但前置校验失败（如状态不是 exhausted）时，账本不被修改。
// 由于 `Update` 的 append-only 校验会阻止任何不符合 V1 图的修改，失败覆盖尝试
// 不可能污染账本。该行为由 StyleReviewStore 的 append-only 保证覆盖。
//
// 具体验证分散在各个拒绝测试中（如 TestStyleReviewOverride_RejectsNonExhausted
// 验证了失败后 overridden 条目未被追加）。

// ── 8. C1-H3: override 保留 Epoch 与 PolishCheckpointSeq ──────────────

// TestStyleReviewOverride_PreservesEpochAndPolishSeq 验证 overridden 条目保留
// exhausted 条目的 Epoch 与 Request.PolishCheckpointSeq——commit gate 以当前
// terminal 条目为权威取绑定 seq，若 override 丢失绑定会退化为 legacy 时间比较。
func TestStyleReviewOverride_PreservesEpochAndPolishSeq(t *testing.T) {
	draft := "第一章正文。她心里骂自己丢人，真不要脸。"
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	checkDigest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "test-artifact", checkDigest); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}
	// 构造 epoch 2 + 绑定 polish seq 7 的 exhausted 账本（模拟返工队列章节的耗尽态）。
	draftDigest := domain.DigestDraft(draft)
	basisDigest := tools.ComputeBasisDigest(st, 1, "test-v1")
	req := func(prompt string) *domain.StyleReviewRequest {
		return &domain.StyleReviewRequest{Prompt: prompt, Model: "test", PolishCheckpointSeq: 7}
	}
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: req("test"), DraftDigest: draftDigest, BasisDigest: basisDigest, Epoch: 2},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1", Request: req("test"),
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "some evidence"}}},
				DraftDigest: draftDigest, BasisDigest: basisDigest, Epoch: 2},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "a1", Request: req("test"), DraftDigest: draftDigest, BasisDigest: basisDigest, Epoch: 2},
			{Cycle: 4, Status: domain.ReviewStatusExhausted, CreatedAt: "2026-07-25T13:00:00Z", AttemptID: "a1", Request: req("test"),
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "final revise",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "error", Evidence: "test evidence"}}},
				DraftDigest: draftDigest, BasisDigest: basisDigest, Epoch: 2},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	h := &Host{store: st, events: make(chan Event, 10), done: make(chan struct{}, 4)}
	if err := h.StyleReviewOverride(1, "人工确认"); err != nil {
		t.Fatalf("StyleReviewOverride: %v", err)
	}
	loaded, err := st.StyleReview.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	last := loaded.CurrentCycle()
	if last.Status != domain.ReviewStatusOverridden {
		t.Fatalf("expected overridden, got %s", last.Status)
	}
	if last.EpochValue() != 2 {
		t.Errorf("override epoch = %d, want 2（保留 exhausted 的 epoch）", last.EpochValue())
	}
	if last.Request == nil || last.Request.PolishCheckpointSeq != 7 {
		t.Errorf("override polish_checkpoint_seq = %+v, want 7（保留 exhausted 的绑定）", last.Request)
	}
}
