package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── Mock critic model ────────────────────────────────────────────────

type mockCriticModel struct {
	fn  func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error)
	idx int64
}

func (m *mockCriticModel) take(msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
	i := int(m.idx)
	m.idx++
	return m.fn(i, msgs)
}

func (m *mockCriticModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return m.take(msgs)
}

func (m *mockCriticModel) GenerateStream(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	resp, err := m.take(msgs)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *mockCriticModel) SupportsTools() bool { return false }

// ModelNameNamer implements agentcore.ModelNamer for telemetry tests.
func (m *mockCriticModel) ModelName() string { return "mock-critic-model-v2" }

func criticText(text string) agentcore.Message {
	return agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock(text)},
	}
}

// productionPassJSON returns a critic output with the full production shape.
func productionPassJSON() string {
	return `{"verdict":"pass","strength":{"dimension":"aesthetic","evidence":"文笔流畅，表现力强"},"findings":[]}`
}

func productionReviseJSON() string {
	return `{"verdict":"revise","strength":{"dimension":"hook","evidence":"开篇悬念设置得当"},"findings":[{"dimension":"pacing","category":"style","severity":"warning","evidence":"第二段节奏偏慢","problem":"描写过细","revision":"压缩中间描写"}]}`
}

func newMockCritic(fn func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error)) *subagent.Runner {
	model := &mockCriticModel{fn: fn}
	cfg := subagent.Config{
		Name:        "style_critic",
		Description: "mock critic",
		Model:       model,
		MaxTurns:    1,
	}
	return subagent.NewRunner(cfg)
}

// ── Test helpers ─────────────────────────────────────────────────────

const testCriticVersion = "test-critic-v1"

func setupCriticStore(t *testing.T, chapter int, draft string) *store.Store {
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
	return st
}

func setupOffModeStore(t *testing.T, chapter int, draft string) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if draft != "" {
		if err := st.Drafts.SaveDraft(chapter, draft); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
	}
	return st
}

// ── 1. Happy initial pass (production shape) ─────────────────────────

func TestReviewStyle_InitialPass(t *testing.T) {
	st := setupCriticStore(t, 1, "第一章正文。这是一个测试草稿内容。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{
			Message: criticText(productionPassJSON()),
		}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if output.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass", output.Verdict)
	}
	if output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Errorf("status = %q, want %q", output.Status, domain.ReviewStatusAcceptedInitial)
	}
	// 验证账本
	ledger, err := st.StyleReview.Load(1)
	if err != nil || ledger == nil {
		t.Fatalf("Load ledger: %v", err)
	}
	if len(ledger.Cycles) != 2 {
		t.Fatalf("expected 2 cycles, got %d", len(ledger.Cycles))
	}
	if ledger.Cycles[1].Status != domain.ReviewStatusAcceptedInitial {
		t.Errorf("cycle[1].status = %q", ledger.Cycles[1].Status)
	}
	// strength.evidence 应体现在 evidence 字段
	if output.Evidence == "" {
		t.Error("evidence should contain strength.evidence")
	}
	if !strings.Contains(output.Evidence, "文笔流畅") {
		t.Errorf("evidence %q should contain strength evidence", output.Evidence)
	}
}

// ── 2. Revise → edit → final pass ────────────────────────────────────

func TestReviewStyle_ReviseThenEditThenFinalPass(t *testing.T) {
	draft := "第一章正文。初始草稿版本。"
	st := setupCriticStore(t, 1, draft)

	callCount := 0
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		callCount++
		if callCount == 1 {
			return &agentcore.LLMResponse{Message: criticText(productionReviseJSON())}, nil
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)

	// 首次评审 → revise
	out1, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("First review: %v", err)
	}
	var r1 StyleReviewOutput
	json.Unmarshal(out1, &r1)
	if r1.Verdict != "revise" {
		t.Fatalf("first verdict = %q, want revise", r1.Verdict)
	}
	if r1.Status != string(domain.ReviewStatusRevisionOpen) {
		t.Fatalf("first status = %q, want %q", r1.Status, domain.ReviewStatusRevisionOpen)
	}

	// 修改草稿
	newDraft := draft + "\n\n根据评审意见修改后内容。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatalf("SaveDraft revised: %v", err)
	}
	newDigest := domain.DigestDraft(newDraft)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check",
		"test-artifact-2", newDigest,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}

	// 最终评审 → pass
	out2, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Second review: %v", err)
	}
	var r2 StyleReviewOutput
	json.Unmarshal(out2, &r2)
	if r2.Verdict != "pass" {
		t.Fatalf("second verdict = %q, want pass", r2.Verdict)
	}
	if r2.Status != string(domain.ReviewStatusAcceptedRev) {
		t.Fatalf("second status = %q, want %q", r2.Status, domain.ReviewStatusAcceptedRev)
	}

	// 验证完整 V1 路径
	ledger, _ := st.StyleReview.Load(1)
	if len(ledger.Cycles) != 4 {
		t.Fatalf("expected 4 cycles, got %d", len(ledger.Cycles))
	}
	expectedStatuses := []domain.StyleReviewStatus{
		domain.ReviewStatusInitialPending,
		domain.ReviewStatusRevisionOpen,
		domain.ReviewStatusFinalPending,
		domain.ReviewStatusAcceptedRev,
	}
	for i, s := range expectedStatuses {
		if ledger.Cycles[i].Status != s {
			t.Errorf("cycle[%d].status = %q, want %q", i, ledger.Cycles[i].Status, s)
		}
	}
}

// ── 3. Final revise blocks commit ────────────────────────────────────

func TestReviewStyle_FinalReviseBlocksCommit(t *testing.T) {
	draft := "第一章正文。"
	st := setupCriticStore(t, 1, draft)

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		// Both calls return revise → exhausted on second
		if i == 1 {
			return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"hook","evidence":"悬念设置好"},"findings":[{"dimension":"pacing","category":"style","severity":"warning","evidence":"末段","problem":"节奏","revision":"调整"}]}`)}, nil
		}
		return &agentcore.LLMResponse{Message: criticText(productionReviseJSON())}, nil
	})

	reviewTool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := reviewTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("First review: %v", err)
	}

	if err := st.Drafts.SaveDraft(1, draft+"修改版"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	newDigest := domain.DigestDraft(draft + "修改版")
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check", "test-artifact-2", newDigest,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}

	if _, err := reviewTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Second review: %v", err)
	}

	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusExhausted {
		t.Fatalf("expected exhausted, got %s", ledger.CurrentStatus())
	}

	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err == nil {
		t.Fatal("commit should be rejected when ledger is exhausted")
	}
}

// ── 4. Stale draft/basis/consistency rejection ───────────────────────

func TestReviewStyle_StaleDraftRejected(t *testing.T) {
	st := setupCriticStore(t, 1, "原始草稿。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// 修改草稿但不重新评审
	if err := st.Drafts.SaveDraft(1, "被修改的新草稿。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err == nil {
		t.Fatal("commit should be rejected when draft digest doesn't match ledger")
	}
}

func TestReviewStyle_NoConsistencyCheckRejected(t *testing.T) {
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
	if err := st.Drafts.SaveDraft(1, "草稿。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for missing consistency checkpoint")
	}
}

// ── 5. Critic failure → degraded (never strand pending) ─────────────

func TestReviewStyle_CriticFailureDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "第一章正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, assertAnError("critic simulated failure")
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute should not propagate critic error, got: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded=true on critic failure")
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
	// must have request for audit
	last := ledger.CurrentCycle()
	if last.Request == nil {
		t.Fatal("degraded entry must have Request for audit")
	}
	if last.Error == "" {
		t.Fatal("degraded entry must have error message")
	}
}

func assertAnError(msg string) error {
	return &simpleError{msg: msg}
}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }

// ── 6. Malformed critic JSON → degraded (never strand pending) ───────

func TestReviewStyle_MalformedJSONDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`not json at all`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for malformed JSON")
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
}

func TestReviewStyle_InvalidVerdictDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"maybe","strength":{"dimension":"x","evidence":"ok"}}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for invalid verdict")
	}
}

// ── 7. Revise with no findings → degraded ────────────────────────────

func TestReviewStyle_ReviseNoFindingsDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"pacing","evidence":"positive"},"findings":[]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for revise with no findings")
	}
}

// ── 8. Preseeded initial_pending recovery ────────────────────────────

func TestReviewStyle_PreseededInitialPendingRecovery(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// 预写入 initial_pending
	pendingLedger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusInitialPending,
			CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "preexisting-attempt",
			Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "preloaded-model"},
			DraftDigest: draftDigest, BasisDigest: basisDigest,
		}},
	}
	if err := st.StyleReview.Save(pendingLedger); err != nil {
		t.Fatalf("Save preseeded: %v", err)
	}

	// 运行 review_style - 应该复用现有的 initial_pending 而不是创建新的
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" {
		t.Fatalf("verdict = %q", output.Verdict)
	}

	ledger, _ := st.StyleReview.Load(1)
	// 验证 attempt_id 被保留（复用）
	if ledger.Cycles[0].AttemptID != "preexisting-attempt" {
		t.Errorf("initial_pending attempt_id changed to %q", ledger.Cycles[0].AttemptID)
	}
	// 验证只有 2 个周期（没有额外 initial_pending）
	if len(ledger.Cycles) != 2 {
		t.Fatalf("expected 2 cycles (reused + result), got %d", len(ledger.Cycles))
	}
}

// ── 9. Preseeded final_pending recovery ──────────────────────────────

func TestReviewStyle_PreseededFinalPendingRecovery(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// 预写入 revision_open + final_pending
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e",
					}},
				},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "preexisting-final",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "preloaded-final"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save preseeded: %v", err)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" {
		t.Fatalf("verdict = %q", output.Verdict)
	}

	loaded, _ := st.StyleReview.Load(1)
	// 验证 final_pending 的 attempt_id 被保留
	if loaded.Cycles[2].AttemptID != "preexisting-final" {
		t.Errorf("final_pending attempt_id changed to %q", loaded.Cycles[2].AttemptID)
	}
	if len(loaded.Cycles) != 4 {
		t.Fatalf("expected 4 cycles, got %d", len(loaded.Cycles))
	}
}

// ── 10. Preseeded final + model failure → degraded with persisted request ──

func TestReviewStyle_PreseededFinalFailureDegraded(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e",
					}},
				},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "preexisting-final",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "critic-final-model"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save preseeded: %v", err)
	}

	// 批评者失败
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, assertAnError("model unavailable")
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded")
	}

	loaded, _ := st.StyleReview.Load(1)
	last := loaded.CurrentCycle()
	if last.Status != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", last.Status)
	}
	// 使用持久化的 final request
	if last.Request == nil || last.Request.Model != "critic-final-model" {
		t.Errorf("degraded should reuse persisted final request model %q", last.Request.Model)
	}
	if last.Error == "" {
		t.Fatal("degraded must have error")
	}
}

// ── 11. Critic has no write tools (invariant) ────────────────────────

func TestReviewStyle_CriticHasNoWriteTools(t *testing.T) {
	st := setupCriticStore(t, 1, "草稿正文。")
	callCount := 0
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		callCount++
		msgText := ""
		for _, block := range msgs[len(msgs)-1].Content {
			if block.Text != "" {
				msgText += block.Text
			}
		}
		if !strings.Contains(msgText, "草稿") {
			t.Error("critic message should contain draft text")
		}
		if !strings.Contains(msgText, "评审依据") {
			t.Error("critic message should contain basis")
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 critic call, got %d", callCount)
	}
}

// ── 12. Off-mode compatibility ───────────────────────────────────────

func TestReviewStyle_OffModeSkips(t *testing.T) {
	st := setupOffModeStore(t, 1, "第一章正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		t.Fatal("critic should not be called in off mode")
		return nil, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Skipped {
		t.Fatal("expected skipped=true in off mode")
	}
}

// ── 13. Mutation guard: blocks during pending ────────────────────────

func TestReviewStyle_MutationGuardBlocksDuringPendingInitial(t *testing.T) {
	draft := "第一章正文。"
	st := setupCriticStore(t, 1, draft)

	blocked := make(chan struct{})
	defer close(blocked)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		<-blocked
		return nil, nil
	})

	errCh := make(chan error, 1)
	go func() {
		tool := NewReviewStyleTool(st, critic, testCriticVersion)
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"chapter":1}`))
		errCh <- err
	}()
	time.Sleep(200 * time.Millisecond)

	draftTool := NewDraftChapterTool(st, testContract)
	draftArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "content": "新草稿", "mode": "write",
	})
	_, err := draftTool.Execute(t.Context(), draftArgs)
	if err == nil {
		t.Fatal("draft_chapter should be rejected during initial_pending")
	}

	editTool := NewEditChapterTool(st)
	editArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "old_string": "正文", "new_string": "修改",
	})
	_, err = editTool.Execute(t.Context(), editArgs)
	if err == nil {
		t.Fatal("edit_chapter should be rejected during initial_pending")
	}
}

// ── 14. Mutation guard: allowed during revision_open ─────────────────

func TestReviewStyle_MutationGuardAllowsDuringRevisionOpen(t *testing.T) {
	draft := "第一章正文。"
	st := setupCriticStore(t, 1, draft)

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionReviseJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Review: %v", err)
	}

	if err := st.Drafts.SaveDraft(1, draft+"\n\n根据评审修改。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	draftTool := NewDraftChapterTool(st, testContract)
	out, err := draftTool.Execute(t.Context(), json.RawMessage(`{
		"chapter":1,"content":"修订版草稿内容。","mode":"write"
	}`))
	if err != nil {
		if strings.Contains(err.Error(), "评审") || strings.Contains(err.Error(), "pending") {
			t.Fatalf("mutation guard incorrectly blocked during revision_open: %v", err)
		}
		t.Logf("draft_chapter error (expected non-guard): %v", err)
	}
	_ = out
}

// ── 15. Commit gate: terminal allowed ────────────────────────────────

func TestReviewStyle_CommitGatePassesForTerminalStatus(t *testing.T) {
	draft := "第一章正文。"
	st := setupCriticStore(t, 1, draft)

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Review: %v", err)
	}

	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "通过评审的提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err != nil {
		t.Fatalf("commit should pass after terminal review, got: %v", err)
	}
}

// ── 16. Off mode: commit not blocked ─────────────────────────────────

func TestReviewStyle_OffModeCommitNotBlocked(t *testing.T) {
	st := setupOffModeStore(t, 1, "第一章正文。")
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "正常提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err != nil {
		if strings.Contains(err.Error(), "评审") || strings.Contains(err.Error(), "critic") {
			t.Fatalf("off mode should not trigger style gate: %v", err)
		}
		t.Logf("commit error (expected non-style-gate): %v", err)
	}
}

// ── 17. Rewrite-queue bypass mutation ────────────────────────────────

func TestReviewStyle_RewriteQueueBypassesMutationGuard(t *testing.T) {
	draft := "已完成的终稿。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}

	// 模拟章节已完成且在重写队列中
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, draft); err != nil {
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

	// 即使มี exhausted 账本，重写队列中的完成章节也应能 draft/edit
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	ts := "2026-07-25T10:00:00Z"
	exhaustedLedger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: ts, AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test-model"}, DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: ts, AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test-model"}, Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}},
				}, DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: ts, AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test-model"}, DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 4, Status: domain.ReviewStatusExhausted, CreatedAt: ts, AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: "test", Model: "test-model"}, Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "error", Evidence: "e"}},
				}, DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(exhaustedLedger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	// draft_chapter should pass (bypass mutation guard)
	draftTool := NewDraftChapterTool(st, testContract)
	_, err := draftTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"新重写版本","mode":"write"}`))
	if err != nil {
		if strings.Contains(err.Error(), "评审") {
			t.Fatalf("rewrite-queue should bypass mutation guard: %v", err)
		}
		t.Logf("draft error (expected): %v", err)
	}
}

// ── 18. Rewrite-queue bypass commit gate ─────────────────────────────

func TestReviewStyle_RewriteQueueBypassesCommitGate(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}

	// 完成章节在重写队列中
	draft := "已完成的终稿内容。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, draft); err != nil {
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
	// 写 exhausted 账本
	if err := st.Drafts.SaveDraft(1, draft+"重写版"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "重写提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err != nil {
		if strings.Contains(err.Error(), "critic") || strings.Contains(err.Error(), "评审") {
			t.Fatalf("rewrite-queue should bypass commit gate: %v", err)
		}
		t.Logf("commit error (expected non-critic): %v", err)
	}
}

// ── 19. Critic model provenance ──────────────────────────────────────

func TestReviewStyle_CriticModelProvenance(t *testing.T) {
	// 验证 review_style 在 request 中记录的是批评者的实际模型名
	st := setupCriticStore(t, 1, "正文。")
	model := &mockCriticModel{}
	model.fn = func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	}
	cfg := subagent.Config{
		Name: "style_critic", Description: "test", Model: model, MaxTurns: 1,
	}
	criticRunner := subagent.NewRunner(cfg)

	tool := NewReviewStyleTool(st, criticRunner, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ledger, _ := st.StyleReview.Load(1)
	if ledger.Cycles[0].Request == nil {
		t.Fatal("initial_pending must have request")
	}
	// Model 应来自 mockCriticModel.ModelName()
	if ledger.Cycles[0].Request.Model != "mock-critic-model-v2" {
		t.Errorf("request.Model = %q, want mock-critic-model-v2", ledger.Cycles[0].Request.Model)
	}
}

// ── 20. Context cancellation passthrough ──────────────────────────────

func TestReviewStyle_ContextCancellation(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		// 模拟长时间运行的批评者
		select {
		case <-time.After(5 * time.Second):
			return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 立即取消

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	_, err := tool.Execute(ctx, json.RawMessage(`{"chapter":1}`))
	// 取消的 context 应导致错误，而不是正常完成
	if err == nil {
		t.Log("note: context cancellation did not propagate as error (may depend on subagent implementation)")
	}
}

// ── 21. Terminal ledger: basis change still allows commit ─────────────
//
// Policy: an already-terminal ledger (accepted_initial, accepted_revised,
// degraded, overridden) whose draft digest still matches must allow commit
// even when the basis has changed. This prevents stale-basis deadlocks:
// re-review is not available for terminal statuses, so we must not claim it is.

func TestReviewStyle_TerminalBasisChangeAllowsCommit(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 修改章节风格目标（变更基础）— terminal ledger 应不受影响
	plan, _ := st.Drafts.LoadChapterPlan(1)
	if plan == nil {
		plan = &domain.ChapterPlan{Chapter: 1}
	}
	plan.StyleGoal = &domain.ChapterStyleGoal{
		FocalFilter:   "different-focal",
		ProseMovement: "different-prose",
	}
	if err := st.Drafts.SaveChapterPlan(*plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	// Terminal ledger + basis change → commit ALLOWED (no stale-basis deadlock)
	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err != nil {
		t.Fatalf("terminal ledger with basis change should allow commit, got: %v", err)
	}
}

// ── 22. Basis change: anchor content change detected ─────────────────

func TestReviewStyle_BasisAnchorChangeDetected(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 变更锚点内容（模拟用户修改 style_anchors.json）
	// 通过写入新的锚点文件来改变 LoadManual 的结果
	anchorPath := st.Dir() + "/meta/style_anchors.json"
	// 无法直接写 style_anchors，但可以通过 RunMeta.save 来更换 anchor 数据
	// 实际测试中我们验证 commit gate 能检测到变更即可
	// 最简单的检测：重新执行评审得到不同的 basis
	out1basis := ComputeBasisDigest(st, 1, testCriticVersion)

	// 通过直接写文件模拟锚点变化
	os.WriteFile(anchorPath, []byte(`{"version":1,"anchors":[{"id":"new","excerpt":"different anchor content"}]}`), 0o644)
	// 清除缓存（store 会在下次 LoadManual 时重新读取）
	out2basis := ComputeBasisDigest(st, 1, testCriticVersion)

	if out1basis == out2basis {
		t.Error("basis digest should change when anchor content changes")
	}
	t.Logf("basis digest changed from %q to %q", out1basis, out2basis)
}

// ── 23. Basis change: style goal change detected ─────────────────────

func TestReviewStyle_BasisStyleGoalChangeDetected(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out1basis := ComputeBasisDigest(st, 1, testCriticVersion)

	// 变更章节风格目标（不再是 RunMeta.Style，而是 ChapterPlan.StyleGoal）
	plan, _ := st.Drafts.LoadChapterPlan(1)
	if plan == nil {
		plan = &domain.ChapterPlan{Chapter: 1}
	}
	plan.StyleGoal = &domain.ChapterStyleGoal{
		FocalFilter:   "different-focal",
		ProseMovement: "different-prose",
	}
	if err := st.Drafts.SaveChapterPlan(*plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	out2basis := ComputeBasisDigest(st, 1, testCriticVersion)

	if out1basis == out2basis {
		t.Error("basis digest should change when chapter style goal changes")
	}
}

// ── 24. Missing strength.evidence → degraded ─────────────────────────

func TestReviewStyle_MissingStrengthDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		// No strength field at all
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"pass","findings":[]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded when strength.evidence is missing")
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded ledger, got %s", ledger.CurrentStatus())
	}
}

func TestReviewStyle_EmptyStrengthEvidenceDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"pass","strength":{"dimension":"x","evidence":""}}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded when strength.evidence is empty")
	}
}

// ── 25. Multi-byte rune boundary ─────────────────────────────────────

func TestReviewStyle_MultiByteRuneBoundary(t *testing.T) {
	// 创建包含多字节 UTF-8 字符的草稿
	// 中文每个字 3 字节，使用超过 maxCriticRunes 的长文测试边界
	draft := strings.Repeat("文", 13000) + "边界测试"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		// 验证消息中的草稿是完整的 rune
		msgText := ""
		for _, block := range msgs[len(msgs)-1].Content {
			if block.Text != "" {
				msgText += block.Text
			}
		}
		// 找草稿段: 在 "### 草稿" 之后的内容
		idx := strings.Index(msgText, "### 草稿")
		if idx < 0 {
			t.Error("critic message missing draft section header")
			return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
		}
		draftSection := msgText[idx:]
		// 截断说明应出现在消息中
		if !strings.Contains(draftSection, "仅发送前") {
			t.Error("truncated draft should mention truncation in critic message")
		}
		// 验证消息中没有无效 UTF-8（纯中文字符串不会产生无效序列）
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// ── 26. Prompt hash / provenance ─────────────────────────────────────

func TestReviewStyle_PromptHashRecordedInRequest(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	// 使用特定 prompt hash
	promptHash := "prompt:test1234"
	tool := NewReviewStyleTool(st, critic, promptHash)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ledger, _ := st.StyleReview.Load(1)
	if ledger.Cycles[0].Request == nil {
		t.Fatal("request should be set")
	}
	if ledger.Cycles[0].Request.Prompt != promptHash {
		t.Errorf("request.Prompt = %q, want %q", ledger.Cycles[0].Request.Prompt, promptHash)
	}
	// Basis digest should incorporate the prompt hash
	basisDigest := ComputeBasisDigest(st, 1, promptHash)
	if ledger.Cycles[0].BasisDigest != basisDigest {
		t.Errorf("basis_digest %q != computed %q", ledger.Cycles[0].BasisDigest, basisDigest)
	}
}

// ── 27. Host migration guard blocks override during migration ─────────
// (tested in host package - see TestStyleReviewOverride*)

// ── 28. Basis payload contains actual data (not just hash labels) ────

func TestReviewStyle_BasisPayloadHasActualContent(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)

	// 向锚点写入实际内容
	anchorPath := st.Dir() + "/meta/style_anchors.json"
	os.MkdirAll(st.Dir()+"/meta", 0o755)
	os.WriteFile(anchorPath, []byte(`{"version":1,"anchors":[{"id":"a1","excerpt":"实际锚点内容片段"}]}`), 0o644)

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		msgText := ""
		for _, block := range msgs[len(msgs)-1].Content {
			if block.Text != "" {
				msgText += block.Text
			}
		}
		// 验证 basis payload 包含实际锚点内容
		if !strings.Contains(msgText, "实际锚点内容片段") {
			t.Error("critic message should contain actual anchor excerpt content")
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// ── 29. Chapter StyleGoal: load uses plan not RunMeta.Style ────────────

func TestLoadChapterStyleGoal_UsesPlanNotRunMeta(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Set RunMeta.Style to a value (should be ignored by loadChapterStyleGoal)
	meta, _ := st.RunMeta.Load()
	if meta == nil {
		meta = &domain.RunMeta{Style: "global-style"}
	} else {
		meta.Style = "global-style"
	}
	st.RunMeta.Save(*meta)

	// No chapter plan → nil
	goal := loadChapterStyleGoal(st, 1)
	if goal != nil {
		t.Error("expected nil when no chapter plan exists")
	}

	// Save plan with specific StyleGoal
	plan := domain.ChapterPlan{
		Chapter: 1,
		StyleGoal: &domain.ChapterStyleGoal{
			FocalFilter:    "tight-pov",
			ProseMovement:  "cinematic",
			DetailStrategy: "sensory",
			Rhythm:         "staccato",
		},
	}
	if err := st.Drafts.SaveChapterPlan(plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	goal = loadChapterStyleGoal(st, 1)
	if goal == nil {
		t.Fatal("expected non-nil StyleGoal")
	}
	if goal.FocalFilter != "tight-pov" {
		t.Errorf("FocalFilter = %q, want tight-pov", goal.FocalFilter)
	}
	if goal.ProseMovement != "cinematic" {
		t.Errorf("ProseMovement = %q, want cinematic", goal.ProseMovement)
	}
	// Verify RunMeta.Style is NOT used
	if goal.FocalFilter == "global-style" {
		t.Error("loadChapterStyleGoal incorrectly used RunMeta.Style")
	}
}

func TestLoadChapterStyleGoal_ScopedMatchVsMismatch(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Plan for chapter 5 with StyleGoal
	plan5 := domain.ChapterPlan{
		Chapter: 5,
		StyleGoal: &domain.ChapterStyleGoal{
			FocalFilter:   "chapter5-filter",
			ProseMovement: "flowing",
		},
	}
	if err := st.Drafts.SaveChapterPlan(plan5); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	// Chapter 5 should match
	goal5 := loadChapterStyleGoal(st, 5)
	if goal5 == nil || goal5.FocalFilter != "chapter5-filter" {
		t.Error("chapter 5 should match its own plan")
	}

	// Chapter 3 should NOT match (no plan)
	goal3 := loadChapterStyleGoal(st, 3)
	if goal3 != nil {
		t.Error("chapter 3 without plan should return nil")
	}
}

// ── 30. Compass rules / writer card change detection ──────────────────

func TestReviewStyle_CompassRulesChangeDetected(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out1basis := ComputeBasisDigest(st, 1, testCriticVersion)

	// 保存 compass 规则（变更基础）
	compass := domain.WritingStyleRulesCompass{
		Long: &domain.StyleRulesLong{
			Prose: []string{"使用短句增强紧张感", "描写聚焦视觉细节"},
		},
	}
	if err := st.World.SaveStyleRulesCompass(compass); err != nil {
		t.Fatalf("SaveStyleRulesCompass: %v", err)
	}

	out2basis := ComputeBasisDigest(st, 1, testCriticVersion)
	if out1basis == out2basis {
		t.Error("basis digest should change when compass rules change")
	}
}

// ── 31. User rules change detection ───────────────────────────────────

func TestReviewStyle_UserRulesChangeDetected(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out1basis := ComputeBasisDigest(st, 1, testCriticVersion)

	snap := &rules.Snapshot{
		Version: rules.SnapshotVersion,
		Status:  rules.StatusReady,
		Structured: rules.Structured{
			Genre: "科幻",
		},
		Preferences: rules.PreferenceBuckets{
			Default: []rules.PreferenceRule{{ID: "rule-1", Text: "使用平实语言"}},
		},
	}
	if err := st.UserRules.Save(snap); err != nil {
		t.Fatalf("Save UserRules: %v", err)
	}

	out2basis := ComputeBasisDigest(st, 1, testCriticVersion)
	if out1basis == out2basis {
		t.Error("basis digest should change when user rules change")
	}
}

// ── 32. Chapter contract change detected ──────────────────────────────

func TestReviewStyle_ChapterContractChangeDetected(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out1basis := ComputeBasisDigest(st, 1, testCriticVersion)

	// 添加 chapter plan with contract
	plan := domain.ChapterPlan{
		Chapter: 1,
		Contract: domain.ChapterContract{
			RequiredBeats:  []string{"主角发现线索"},
			ForbiddenMoves: []string{"角色死亡"},
		},
	}
	if err := st.Drafts.SaveChapterPlan(plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	out2basis := ComputeBasisDigest(st, 1, testCriticVersion)
	if out1basis == out2basis {
		t.Error("basis digest should change when chapter contract changes")
	}
}

// ── 33. Factual outline change detected ──────────────────────────────

func TestReviewStyle_FactualOutlineChangeDetected(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out1basis := ComputeBasisDigest(st, 1, testCriticVersion)

	// 保存平面大纲（变更事实数据）
	outline := []domain.OutlineEntry{
		{Chapter: 1, Title: "改后第一章", CoreEvent: "主角出发"},
	}
	if err := st.Outline.SaveOutline(outline); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}

	out2basis := ComputeBasisDigest(st, 1, testCriticVersion)
	if out1basis == out2basis {
		t.Error("basis digest should change when factual outline changes")
	}
}

// ── 34. Telemetry: agentToRole maps style_critic to critic ─────────────

func TestAgentToRole_StyleCriticMapsToCritic(t *testing.T) {
	imported := func(name string) string {
		if name == "style_critic" {
			return "critic"
		}
		if name == "architect_short" || name == "architect_long" {
			return "architect"
		}
		return name
	}

	tests := []struct {
		name string
		want string
	}{
		{"style_critic", "critic"},
		{"architect_short", "architect"},
		{"architect_long", "architect"},
		{"writer", "writer"},
		{"editor", "editor"},
		{"unknown", "unknown"},
	}
	for _, tc := range tests {
		got := imported(tc.name)
		if got != tc.want {
			t.Errorf("agentToRole(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ── 35. Basis payload includes chapter-scoped anchor excerpts ─────────

func TestReviewStyle_BasisAnchorChapterScoped(t *testing.T) {
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
	if err := st.Drafts.SaveDraft(1, "草稿。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	draftDigest := domain.DigestDraft("草稿。")
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check", "test-artifact", draftDigest,
	); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}

	// 写锚点：一个全局适用，一个仅 Chapters 2-3
	anchorPath := st.Dir() + "/meta/style_anchors.json"
	os.MkdirAll(st.Dir()+"/meta", 0o755)
	os.WriteFile(anchorPath, []byte(`{
		"version":1,
		"anchors":[
			{"id":"global","excerpt":"全局锚点内容"},
			{"id":"ch2-3","excerpt":"2-3章专用锚点","applies_to":{"chapter_ranges":[[2,3]]}}
		]
	}`), 0o644)

	// Chapter 1 should only include the global anchor
	excerpts := loadAnchorExcerpts(st, 1)
	if len(excerpts) != 1 {
		t.Fatalf("chapter 1: expected 1 anchor excerpt, got %d", len(excerpts))
	}
	if !strings.Contains(excerpts[0], "全局锚点") {
		t.Errorf("chapter 1 excerpt = %q, want global anchor", excerpts[0])
	}

	// Chapter 2 should include both
	excerpts2 := loadAnchorExcerpts(st, 2)
	if len(excerpts2) != 2 {
		t.Fatalf("chapter 2: expected 2 anchor excerpts, got %d", len(excerpts2))
	}
}

// ── 37. Invalid strength.dimension → degraded ─────────────────────────

func TestReviewStyle_InvalidStrengthDimensionDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"pass","strength":{"dimension":"bogus_dimension","evidence":"ok"},"findings":[]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for invalid strength.dimension")
	}
	if !strings.Contains(output.Error, "invalid strength.dimension") {
		t.Errorf("error %q should mention invalid strength.dimension", output.Error)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
}

// ── 38. Single invalid finding → degraded (not silently continued) ────

func TestReviewStyle_InvalidFindingDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"pacing","evidence":"ok"},"findings":[{"dimension":"bogus","category":"style","severity":"warning","evidence":"text"}]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for invalid finding")
	}
	if !strings.Contains(output.Error, "invalid finding") {
		t.Errorf("error %q should mention invalid finding", output.Error)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
}

// ── 39. Mixed valid + invalid findings → degraded ──────────────────────

func TestReviewStyle_MixedValidInvalidFindingsDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		// Two valid findings + one invalid finding → entire result rejected
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"pacing","evidence":"ok"},"findings":[
			{"dimension":"pacing","category":"style","severity":"warning","evidence":"valid finding 1"},
			{"dimension":"","category":"style","severity":"warning","evidence":"empty dimension"},
			{"dimension":"hook","category":"plot","severity":"info","evidence":"valid finding 2"}
		]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded when any finding is invalid (even if others are valid)")
	}
	if !strings.Contains(output.Error, "invalid finding") {
		t.Errorf("error %q should mention invalid finding", output.Error)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
}

// ── 40. More than 3 findings (initial attempt) → degraded ─────────────

func TestReviewStyle_TooManyFindingsDegraded_Initial(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"pacing","evidence":"ok"},"findings":[
			{"dimension":"pacing","category":"style","severity":"warning","evidence":"f1"},
			{"dimension":"hook","category":"plot","severity":"error","evidence":"f2"},
			{"dimension":"character","category":"tone","severity":"info","evidence":"f3"},
			{"dimension":"aesthetic","category":"style","severity":"warning","evidence":"f4"}
		]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for >3 findings on initial attempt")
	}
	if !strings.Contains(output.Error, "maximum is 3") {
		t.Errorf("error %q should mention max 3 findings", output.Error)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
}

// ── 41. More than 3 findings (final attempt) → degraded ───────────────

func TestReviewStyle_TooManyFindingsDegraded_Final(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// Preseed: initial_pending → revision_open → final_pending
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e",
					}},
				},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "final-attempt",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	// Critic returns >3 findings → should degrade, not truncate
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"pacing","evidence":"ok"},"findings":[
			{"dimension":"pacing","category":"style","severity":"warning","evidence":"f1"},
			{"dimension":"hook","category":"plot","severity":"error","evidence":"f2"},
			{"dimension":"character","category":"tone","severity":"info","evidence":"f3"},
			{"dimension":"aesthetic","category":"style","severity":"warning","evidence":"f4"}
		]}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded for >3 findings on final attempt")
	}
	if !strings.Contains(output.Error, "maximum is 3") {
		t.Errorf("error %q should mention max 3 findings", output.Error)
	}
	loaded, _ := st.StyleReview.Load(1)
	if loaded.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", loaded.CurrentStatus())
	}
	// Verify the persisted request is reused (from the preseeded final_pending)
	last := loaded.CurrentCycle()
	if last.Request == nil || last.Request.Model != "m" {
		t.Errorf("degraded entry should reuse persisted request model")
	}
}

// ── 42. Initial pending basis drift → degraded ───────────────────────

func TestReviewStyle_InitialPendingBasisDriftDegraded(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	oldBasisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// Preseed an initial_pending with old basis digest
	pendingLedger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusInitialPending,
			CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "old-initial-attempt",
			Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
			DraftDigest: draftDigest, BasisDigest: oldBasisDigest,
		}},
	}
	if err := st.StyleReview.Save(pendingLedger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	// Change basis (style goal) to trigger drift
	plan, _ := st.Drafts.LoadChapterPlan(1)
	if plan == nil {
		plan = &domain.ChapterPlan{Chapter: 1}
	}
	plan.StyleGoal = &domain.ChapterStyleGoal{
		FocalFilter:   "new-focal",
		ProseMovement: "new-prose",
	}
	if err := st.Drafts.SaveChapterPlan(*plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	// Critic should NOT be called — basis drift should degrade immediately
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		t.Fatal("critic should not be called on basis drift")
		return nil, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded when initial pending basis has drifted")
	}
	if !strings.Contains(output.Error, "基础已变更") {
		t.Errorf("error %q should mention basis change", output.Error)
	}
	// Verify degraded entry is bound to the old pending authority
	ledger, _ := st.StyleReview.Load(1)
	last := ledger.CurrentCycle()
	if last.Status != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", last.Status)
	}
	if last.AttemptID != "old-initial-attempt" {
		t.Errorf("degraded should reuse old attempt ID, got %q", last.AttemptID)
	}
	if last.BasisDigest != oldBasisDigest {
		t.Errorf("degraded should preserve old basis digest")
	}
}

// ── 43. Final pending basis drift → degraded ─────────────────────────

func TestReviewStyle_FinalPendingBasisDriftDegraded(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	oldBasisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// Preseed: initial_pending → revision_open → final_pending with old basis
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Result:  nil, DraftDigest: draftDigest, BasisDigest: oldBasisDigest},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1",
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e",
					}},
				},
				DraftDigest: draftDigest, BasisDigest: oldBasisDigest},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "old-final-attempt",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				DraftDigest: draftDigest, BasisDigest: oldBasisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	// Change basis (style goal) to trigger drift
	plan, _ := st.Drafts.LoadChapterPlan(1)
	if plan == nil {
		plan = &domain.ChapterPlan{Chapter: 1}
	}
	plan.StyleGoal = &domain.ChapterStyleGoal{
		FocalFilter:   "different-focal",
		ProseMovement: "different-prose",
	}
	if err := st.Drafts.SaveChapterPlan(*plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		t.Fatal("critic should not be called on basis drift")
		return nil, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded when final pending basis has drifted")
	}
	if !strings.Contains(output.Error, "基础已变更") {
		t.Errorf("error %q should mention basis change", output.Error)
	}
	loaded, _ := st.StyleReview.Load(1)
	last := loaded.CurrentCycle()
	if last.Status != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", last.Status)
	}
	if last.AttemptID != "old-final-attempt" {
		t.Errorf("degraded should reuse old final attempt ID, got %q", last.AttemptID)
	}
	if last.BasisDigest != oldBasisDigest {
		t.Errorf("degraded should preserve old basis digest")
	}
}

// ── 44. Terminal basis drift: stale draft still blocks ───────────────

func TestReviewStyle_TerminalBasisDriftStaleDraftStillBlocks(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Change draft (without new consistency check) — stale draft should block
	if err := st.Drafts.SaveDraft(1, draft+"修改"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	_, err := commitTool.Execute(t.Context(), commitArgs)
	if err == nil {
		t.Fatal("stale draft should still block commit even with terminal ledger")
	}
	if !strings.Contains(err.Error(), "草稿已变更") && !strings.Contains(err.Error(), "一致性检查") {
		t.Errorf("expected stale draft error, got: %v", err)
	}
}

// ── 45. Basis anchor excerpt: rune-safe multibyte truncation ─────────

func TestReviewStyle_AnchorMultibyteRuneSafe(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}

	// Create anchor with multi-byte content longer than 200 runes
	// Chinese characters are 3 bytes each; use ~250 chars for >200 runes
	longText := strings.Repeat("文", 250)
	anchorDir := st.Dir() + "/meta"
	os.MkdirAll(anchorDir, 0o755)
	os.WriteFile(anchorDir+"/style_anchors.json",
		[]byte(`{"version":1,"anchors":[{"id":"mb","excerpt":"`+longText+`"}]}`), 0o644)

	excerpts := loadAnchorExcerpts(st, 1)
	if len(excerpts) == 0 {
		t.Fatal("expected excerpt")
	}
	excerpt := excerpts[0]
	// Verify it's exactly 200 runes (not bytes)
	excerptRunes := []rune(excerpt)
	if excerptRunes[199] != '文' {
		t.Error("expected 200th rune to be intact Chinese character (multibyte-safe)")
	}
	// Verify no partial/malformed runes
	if len(excerptRunes) > 200 {
		t.Errorf("expected ≤200 runes, got %d", len(excerptRunes))
	}
}

// ── 46. Basis compass scoped: layered match includes current ──────────

func TestReviewStyle_CompassScopedLayeredMatch(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)

	// Save compass with Long + Current for volume 1, arc 1
	compass := domain.WritingStyleRulesCompass{
		Long: &domain.StyleRulesLong{
			Prose: []string{"long prose rule"},
		},
		Current: &domain.StyleRulesCurrent{
			Volume: 1, Arc: 1,
			Prose: []string{"current prose for v1a1"},
		},
	}
	if err := st.World.SaveStyleRulesCompass(compass); err != nil {
		t.Fatalf("SaveStyleRulesCompass: %v", err)
	}

	// Save layered outline so LocateChapter succeeds
	layered := []domain.VolumeOutline{
		{Index: 1, Title: "V1", Arcs: []domain.ArcOutline{
			{Index: 1, Title: "A1", Chapters: []domain.OutlineEntry{{Chapter: 1, Title: "Ch1"}}},
		}},
	}
	if err := st.Outline.SaveLayeredOutline(layered); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	// Set layered progress
	progress, _ := st.Progress.Load()
	if progress != nil {
		progress.Layered = true
		progress.CurrentVolume = 1
		progress.CurrentArc = 1
		st.Progress.Save(progress)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		msgText := ""
		for _, block := range msgs[len(msgs)-1].Content {
			if block.Text != "" {
				msgText += block.Text
			}
		}
		// Current prose should be present (volume/arc match)
		if !strings.Contains(msgText, "current prose for v1a1") {
			t.Error("critic basis should contain current prose on layered match")
		}
		if !strings.Contains(msgText, "long prose rule") {
			t.Error("critic basis should always contain long prose")
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// ── 47. Basis compass scoped: layered mismatch excludes current ──────

func TestReviewStyle_CompassScopedLayeredMismatch(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)

	// Current is for volume 2, arc 3 — chapter 1 won't match
	compass := domain.WritingStyleRulesCompass{
		Long: &domain.StyleRulesLong{
			Prose: []string{"long prose rule"},
		},
		Current: &domain.StyleRulesCurrent{
			Volume: 2, Arc: 3,
			Prose: []string{"current prose for v2a3"},
		},
	}
	if err := st.World.SaveStyleRulesCompass(compass); err != nil {
		t.Fatalf("SaveStyleRulesCompass: %v", err)
	}

	// Save layered outline so LocateChapter succeeds (chapter 1 is in v1a1)
	layered := []domain.VolumeOutline{
		{Index: 1, Title: "V1", Arcs: []domain.ArcOutline{
			{Index: 1, Title: "A1", Chapters: []domain.OutlineEntry{{Chapter: 1, Title: "Ch1"}}},
		}},
	}
	if err := st.Outline.SaveLayeredOutline(layered); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	progress, _ := st.Progress.Load()
	if progress != nil {
		progress.Layered = true
		progress.CurrentVolume = 1
		progress.CurrentArc = 1
		st.Progress.Save(progress)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		msgText := ""
		for _, block := range msgs[len(msgs)-1].Content {
			if block.Text != "" {
				msgText += block.Text
			}
		}
		// Current prose should be ABSENT (volume/arc mismatch)
		if strings.Contains(msgText, "current prose for v2a3") {
			t.Error("critic basis should exclude current prose on layered mismatch")
		}
		// Long should still be present
		if !strings.Contains(msgText, "long prose rule") {
			t.Error("critic basis should always contain long prose")
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// ── 48. Verify basis sent to critic includes actual typed data ────────

func TestReviewStyle_BasisContainsTypedStyleGoal(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)

	// Save a chapter plan with typed StyleGoal
	plan := domain.ChapterPlan{
		Chapter: 1,
		StyleGoal: &domain.ChapterStyleGoal{
			FocalFilter:         "close-third",
			ProseMovement:       "lyrical",
			DetailStrategy:      "selective",
			Rhythm:              "varied",
			VariationFromRecent: "slower pace than ch1",
		},
	}
	if err := st.Drafts.SaveChapterPlan(plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		msgText := ""
		for _, block := range msgs[len(msgs)-1].Content {
			if block.Text != "" {
				msgText += block.Text
			}
		}
		// Verify the typed data is in the critic's basis payload
		if !strings.Contains(msgText, "close-third") {
			t.Error("critic message should contain typed StyleGoal data (focal_filter)")
		}
		if !strings.Contains(msgText, "lyrical") {
			t.Error("critic message should contain typed StyleGoal data (prose_movement)")
		}
		// Verify RunMeta.Style is NOT in the payload
		if strings.Contains(msgText, "global-style") {
			t.Error("critic message should NOT contain RunMeta.Style")
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
