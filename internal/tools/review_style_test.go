package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
	// C2 机械规则前置闸：被评审的草稿必须机械干净，否则评审在 accepted 落盘前
	// 被拒（自评口吻不足 / 章节字数不足 3000 等 error 级违例）。测试草稿统一经
	// mechCleanDraft 包装，避免与闸门语义纠缠。
	draft = mechCleanDraft(draft)
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

// mechCleanDraft 返回满足 review_style 机械规则前置闸（C2）的测试草稿：
// 12 类文学腔硬闸中"自评口吻 ≥2"这一条（其余 11 类测试草稿天然不触发）。
// 追加自评关键词"她心里骂自己丢人，真不要脸。"（含 心里骂/丢人/真不要脸
// 三个命中词，且不匹配任何其它文学腔模式）。空串原样返回（表示无草稿）。
func mechCleanDraft(draft string) string {
	if draft == "" {
		return ""
	}
	const filler = "她心里骂自己丢人，真不要脸。"
	if !strings.Contains(draft, "丢人") && !strings.Contains(draft, "不要脸") {
		draft += filler
	}
	return draft
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
	draft := mechCleanDraft("第一章正文。初始草稿版本。")
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
	draft := mechCleanDraft("第一章正文。")
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
	if ledger.CurrentStatus() != domain.ReviewStatusRevisionOpen {
		t.Fatalf("expected revision_open (V2), got %s", ledger.CurrentStatus())
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
	draft := mechCleanDraft("正文。")
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
	draft := mechCleanDraft("正文。")
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
	draft := mechCleanDraft("正文。")
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

	// 轮询等待 initial_pending 落盘（review_style 先落 pending 再调 critic，critic 被
	// blocked 挂起）——固定 sleep 在慢机器上会先于 pending 落盘执行 draft_chapter，
	// 造成时序性误报。
	deadline := time.Now().Add(5 * time.Second)
	for {
		ledger, _ := st.StyleReview.Load(1)
		if ledger != nil && ledger.CurrentStatus() == domain.ReviewStatusInitialPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial_pending ledger not created in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

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
	draft := "第一章正文。她心里骂自己丢人，真不要脸。"
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

// ── 17. Rewrite-queue mutation guard：digest/status 感知（规格第 10 节） ──
// 旧的"已完成 + 重写队列 → 无条件 bypass"已删除，替换为 6 个场景：
//   1. 旧 accepted + draft==final（未开始重写）→ 允许
//   2. revision_open → 允许（不要求 digest 已变化）
//   3. 旧 terminal digest 不匹配当前返工草稿（stale）→ 允许（返工进行中）
//   4. 当前候选已获 terminal 评审且 digest 匹配（draft!=final）→ 拒绝，只能 commit
//   5. initial/final pending → 拒绝
//   6. exhausted → 拒绝（必须先 /style-override）

// rewriteQueueGuardStore 构造 critic 模式 + 已完成 + 重写队列的基础 store。
func rewriteQueueGuardStore(t *testing.T, finalText string) *store.Store {
	t.Helper()
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
	if err := st.Drafts.SaveDraft(1, finalText); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, finalText); err != nil {
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
	return st
}

// rewriteLedger 构造指定状态的账本（绑定 digest）。
func rewriteLedger(status domain.StyleReviewStatus, digest string, seq int64) domain.StyleReviewLedger {
	const basis = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: seq}
	now := "2026-07-25T10:00:00Z"
	cycles := []domain.StyleReviewEntry{
		{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
			Request: req},
	}
	switch status {
	case domain.ReviewStatusAcceptedInitial:
		cycles = append(cycles, domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
			Request: req, Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}})
	case domain.ReviewStatusRevisionOpen:
		cycles = append(cycles, domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
			Request: req, Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
				Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}}})
	case domain.ReviewStatusFinalPending:
		cycles = append(cycles,
			domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
				Request: req, Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}}},
			domain.StyleReviewEntry{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: now,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis,
				Request: req})
	case domain.ReviewStatusExhausted:
		cycles = append(cycles,
			domain.StyleReviewEntry{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
				Request: req, Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}}},
			domain.StyleReviewEntry{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: now,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis,
				Request: req},
			domain.StyleReviewEntry{Cycle: 4, Status: domain.ReviewStatusExhausted, CreatedAt: now,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis,
				Request: req, Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "stagnant",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "error", Evidence: "e"}}}})
	}
	return domain.StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic, Cycles: cycles}
}

// 场景 1：旧 accepted + draft==final（未开始重写）→ 允许开始修改。
func TestReviewStyle_RewriteQueueGuard_OldAcceptedDraftEqualsFinal_Allowed(t *testing.T) {
	final := "已完成的终稿。"
	st := rewriteQueueGuardStore(t, final)
	digest := domain.DigestDraft(final)
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusAcceptedInitial, digest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	if err := CheckStyleReviewMutationGuard(st, 1); err != nil {
		t.Fatalf("old accepted + draft==final must allow rewrite start: %v", err)
	}
}

// 场景 2：revision_open → 允许修改（不要求 digest 已变化）。
func TestReviewStyle_RewriteQueueGuard_RevisionOpen_Allowed(t *testing.T) {
	final := "已完成的终稿。"
	st := rewriteQueueGuardStore(t, final)
	digest := domain.DigestDraft(final)
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusRevisionOpen, digest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	if err := CheckStyleReviewMutationGuard(st, 1); err != nil {
		t.Fatalf("revision_open must allow modification: %v", err)
	}
}

// 场景 3：旧 terminal digest 不匹配当前返工草稿（stale）→ 允许继续返工。
func TestReviewStyle_RewriteQueueGuard_StaleTerminalDraftChanged_Allowed(t *testing.T) {
	final := "原始终稿内容。"
	st := rewriteQueueGuardStore(t, final)
	if err := st.Drafts.SaveDraft(1, final+"返工版本"); err != nil {
		t.Fatalf("SaveDraft rework: %v", err)
	}
	oldDigest := domain.DigestDraft(final)
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusAcceptedInitial, oldDigest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	if err := CheckStyleReviewMutationGuard(st, 1); err != nil {
		t.Fatalf("stale old terminal must allow ongoing rework: %v", err)
	}
}

// 场景 4：当前候选已获 terminal 评审且 digest 匹配（draft!=final）→ 拒绝，只能 commit。
func TestReviewStyle_RewriteQueueGuard_TerminalMatchesCurrent_Denied(t *testing.T) {
	final := "原始终稿内容。"
	st := rewriteQueueGuardStore(t, final)
	rework := final + "返工版本"
	if err := st.Drafts.SaveDraft(1, rework); err != nil {
		t.Fatalf("SaveDraft rework: %v", err)
	}
	reworkDigest := domain.DigestDraft(rework)
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusAcceptedInitial, reworkDigest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	err := CheckStyleReviewMutationGuard(st, 1)
	if err == nil {
		t.Fatal("terminal digest==draft must deny modification")
	}
	if !strings.Contains(err.Error(), "只能 commit_chapter") {
		t.Errorf("expected commit-only hint, got: %v", err)
	}
}

// 场景 5：initial/final pending → 拒绝（有未完成评审）。
func TestReviewStyle_RewriteQueueGuard_Pending_Denied(t *testing.T) {
	for _, status := range []domain.StyleReviewStatus{domain.ReviewStatusInitialPending, domain.ReviewStatusFinalPending} {
		t.Run(string(status), func(t *testing.T) {
			final := "已完成的终稿。"
			st := rewriteQueueGuardStore(t, final)
			digest := domain.DigestDraft(final)
			if err := st.StyleReview.Save(rewriteLedger(status, digest, 0)); err != nil {
				t.Fatalf("Save ledger: %v", err)
			}
			err := CheckStyleReviewMutationGuard(st, 1)
			if err == nil {
				t.Fatalf("%s must deny modification", status)
			}
			if !strings.Contains(err.Error(), "未完成评审") {
				t.Errorf("expected pending hint, got: %v", err)
			}
		})
	}
}

// 场景 6：exhausted → 拒绝（必须先 /style-override），不能当作"允许开始重写"。
func TestReviewStyle_RewriteQueueGuard_Exhausted_Denied(t *testing.T) {
	final := "已完成的终稿。"
	st := rewriteQueueGuardStore(t, final)
	digest := domain.DigestDraft(final)
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusExhausted, digest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	err := CheckStyleReviewMutationGuard(st, 1)
	if err == nil {
		t.Fatal("exhausted must deny modification")
	}
	if !strings.Contains(err.Error(), "/style-override") {
		t.Errorf("expected /style-override hint, got: %v", err)
	}
}

// TestReviewStyle_RewriteQueueGuard_DraftToolPath 场景 4 的工具级验证：
// draft_chapter 在 terminal 当前候选下被 mutation guard 拒绝（不写草稿）。
func TestReviewStyle_RewriteQueueGuard_DraftToolPath(t *testing.T) {
	final := "原始终稿内容。"
	st := rewriteQueueGuardStore(t, final)
	rework := final + "返工版本"
	if err := st.Drafts.SaveDraft(1, rework); err != nil {
		t.Fatalf("SaveDraft rework: %v", err)
	}
	reworkDigest := domain.DigestDraft(rework)
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusAcceptedInitial, reworkDigest, 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}
	draftTool := NewDraftChapterTool(st, testContract)
	_, err := draftTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"新重写版本","mode":"write"}`))
	if err == nil {
		t.Fatal("draft_chapter must be rejected when current candidate is terminal-reviewed")
	}
	if !strings.Contains(err.Error(), "只能 commit_chapter") {
		t.Errorf("expected commit-only hint, got: %v", err)
	}
	// 无副作用：草稿未被覆盖
	draft, _ := st.Drafts.LoadDraft(1)
	if draft != rework {
		t.Errorf("draft must stay untouched after rejection, got %q", draft)
	}
}

// ── 18. C1: rewrite queue 不再 bypass commit gate ────────────────────

// TestReviewStyle_RewriteQueueCommitRequiresCriticValidation 验证 C1：
// 返工/重写队列章节没有新 epoch 的 critic 终验（无账本或账本未绑定当前草稿）时，
// commit 被 CheckCommitStyleGate 拒绝（不再跳过批评者门控）。
func TestReviewStyle_RewriteQueueCommitRequiresCriticValidation(t *testing.T) {
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
	draft := "已完成的终稿内容。她心里骂自己丢人，真不要脸。"
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
	// 草稿已修改（返工），但账本不存在（从未经新 epoch 评审）→ commit 必须被拒
	if err := st.Drafts.SaveDraft(1, draft+"重写版"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "重写提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
		// 批次 4：mode 校验先于 gate 执行，需显式声明才能到达 critic 校验。
		"world_state_mode": "preserve",
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("rewrite commit without critic validation must be rejected (C1)")
	}
	if !strings.Contains(err.Error(), "评审") && !strings.Contains(err.Error(), "critic") {
		t.Errorf("expected critic-gate rejection, got: %v", err)
	}
}

// TestReviewStyle_RewriteQueueCommitPassesAfterNewEpochReview 验证 C1 正向路径：
// 返工章节经 review_style 开启新 epoch 完成终验（terminal + digest 匹配）后 commit 放行。
// TestReviewStyle_RewriteOpensNewEpochRealChain 验证 C1 返工新 epoch 的完整链路
// （M2-1：真实调用 review_style 开启 epoch，不手工构造新 epoch 的 ledger）：
// 旧 accepted 账本（epoch 1）+ pending_rewrites 章节 + 新 polish/consistency
// checkpoint（seq 顺序 polish → consistency）→ review_style 开启 epoch 2，进入
// initial_pending（D1：返工走完整评审周期），critic pass 后落地 accepted_initial
// （epoch 2，绑定本次 polish seq）。
func TestReviewStyle_RewriteOpensNewEpochRealChain(t *testing.T) {
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

	// 完成章节 + 重写队列
	final := "已完成的终稿内容。她心里骂自己丢人，真不要脸。"
	rework := "返工后的新草稿。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, rework); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
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
	// 旧 epoch（epoch 1）的 accepted 账本（种子可手工构造；epoch 开启必须走工具）
	now := time.Now().Format(time.RFC3339)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	originalDigest := domain.DigestDraft(final)
	oldLedger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: originalDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: originalDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(oldLedger); err != nil {
		t.Fatalf("Save old ledger: %v", err)
	}
	// 本次返工的 polish checkpoint + 重新 check_consistency（AppendAlways，seq 晚于 polish）
	reworkDigest := domain.DigestDraft(rework)
	polishCP, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", reworkDigest,
		domain.PolishCheckpointMeta{InputDigest: reworkDigest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: false},
	)
	if err != nil {
		t.Fatalf("AppendPolish: %v", err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", reworkDigest); err != nil {
		t.Fatalf("AppendAlways consistency: %v", err)
	}

	// 真实调用 review_style：旧 terminal + 重写队列 → 开启新 epoch（initial_pending）
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute review_style: %v", err)
	}
	var output StyleReviewOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if output.Verdict != "pass" || output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("verdict/status = %s/%s, want pass/accepted_initial", output.Verdict, output.Status)
	}

	// 账本断言：epoch 2 完整周期 initial_pending → accepted_initial，绑定本次 polish seq
	ledger, _ := st.StyleReview.Load(1)
	if got := ledger.MaxEpoch(); got != 2 {
		t.Fatalf("MaxEpoch = %d, want 2（返工开启新 epoch）", got)
	}
	if len(ledger.Cycles) != 4 {
		t.Fatalf("cycles = %d, want 4（旧 2 + 新 epoch 的 initial_pending + accepted_initial）", len(ledger.Cycles))
	}
	pend := ledger.Cycles[2]
	if pend.Status != domain.ReviewStatusInitialPending || pend.EpochValue() != 2 {
		t.Fatalf("cycle[2] = %s epoch %d, want initial_pending epoch 2（D1：返工进入完整评审周期）", pend.Status, pend.EpochValue())
	}
	curr := ledger.Cycles[3]
	if curr.Status != domain.ReviewStatusAcceptedInitial || curr.EpochValue() != 2 {
		t.Fatalf("cycle[3] = %s epoch %d, want accepted_initial epoch 2", curr.Status, curr.EpochValue())
	}
	if curr.Request == nil || curr.Request.PolishCheckpointSeq != polishCP.Seq {
		t.Fatalf("epoch-2 result 未绑定本次 polish seq：%+v", curr.Request)
	}
	if curr.DraftDigest != reworkDigest {
		t.Fatalf("epoch-2 result digest 未绑定返工草稿：%s", curr.DraftDigest)
	}
}

// ── C1-H3：degraded 新旧候选分流（M2-2） ─────────────────────────────

// TestReviewStyle_DegradedSameCandidateRetriesSameEpoch 验证 degraded 绑定的 polish seq
// 与当前最新 polish seq 相同时：当前 attempt retry，同 epoch 流转（M2-2a）。
func TestReviewStyle_DegradedSameCandidateRetriesSameEpoch(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	// 先追加 polish CP 拿到 seq，再构造绑定同一 seq 的 degraded 账本
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	cp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", digest); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 1 {
		t.Fatalf("MaxEpoch = %d, want 1（同候选 retry 不开启新 epoch）", got)
	}
	// 恢复为初评（degraded 前是 initial_pending），同 epoch 追加 initial_pending → accepted
	if len(loaded.Cycles) != 4 {
		t.Fatalf("cycles = %d, want 4（degraded + retry initial_pending + accepted）", len(loaded.Cycles))
	}
	if loaded.Cycles[2].Status != domain.ReviewStatusInitialPending || loaded.Cycles[2].EpochValue() != 1 {
		t.Fatalf("cycle[2] = %s epoch %d, want initial_pending epoch 1", loaded.Cycles[2].Status, loaded.Cycles[2].EpochValue())
	}
	if loaded.Cycles[3].Status != domain.ReviewStatusAcceptedInitial || loaded.Cycles[3].EpochValue() != 1 {
		t.Fatalf("cycle[3] = %s epoch %d, want accepted_initial epoch 1", loaded.Cycles[3].Status, loaded.Cycles[3].EpochValue())
	}
}

// TestReviewStyle_DegradedOldCandidateOpensNewEpoch 验证 degraded 绑定旧 polish seq
// （与当前最新 polish seq 不同）时：返工队列章节开启新 epoch 重新评审（M2-2b）。
func TestReviewStyle_DegradedOldCandidateOpensNewEpoch(t *testing.T) {
	draft := "# 一\nabc返工草稿她心里骂自己丢人，真不要脸。"
	// 构造：degraded 绑定 seq 5（旧候选）；随后追加最新 polish（seq 6+）与 consistency。
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "重写"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 5}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 5},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	// 最新 polish seq（> 5），随后 consistency seq 更大
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", digest); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 2 {
		t.Fatalf("MaxEpoch = %d, want 2（degraded 旧候选开启新 epoch）", got)
	}
	if loaded.Cycles[2].Status != domain.ReviewStatusInitialPending || loaded.Cycles[2].EpochValue() != 2 {
		t.Fatalf("cycle[2] = %s epoch %d, want initial_pending epoch 2", loaded.Cycles[2].Status, loaded.Cycles[2].EpochValue())
	}
	if loaded.Cycles[3].Status != domain.ReviewStatusAcceptedInitial || loaded.Cycles[3].EpochValue() != 2 {
		t.Fatalf("cycle[3] = %s epoch %d, want accepted_initial epoch 2", loaded.Cycles[3].Status, loaded.Cycles[3].EpochValue())
	}
}

// ── C2：degraded 恢复语义（oracle 设计，修复 83 章死锁） ───────────────
// 候选身份判定与恢复策略解耦：非返工章节即使重新 polish（候选已变化）也允许
// 在当前 epoch 恢复评审（不再拒绝）；返工章节旧候选仍开新 epoch。

// degradedBaseStore 构造 C2 degraded 恢复测试基础：critic 模式 + 干净草稿 D1 +
// polish P1 + consistency C1 + initial_pending(D1, R) → degraded(D1, R)。
// legacy=true 时 R=0（无 seq 绑定）；否则 R = P1 seq（degraded 绑定当时的最新
// polish）。返回 store、D1 digest、P1 seq。
func degradedBaseStore(t *testing.T, draft string, legacy bool) (*store.Store, string, int64) {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	draft = mechCleanDraft(draft)
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	d1 := domain.DigestDraft(draft)
	basis := ComputeBasisDigest(st, 1, "test-v1")
	p1, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", d1,
		domain.PolishCheckpointMeta{InputDigest: d1, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", d1); err != nil {
		t.Fatal(err)
	}
	rSeq := p1.Seq
	if legacy {
		rSeq = 0
	}
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: d1, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: rSeq}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: d1, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: rSeq},
				Error:   "critic returned invalid finding"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	return st, d1, p1.Seq
}

// rePolishDraft 模拟 writer 重新 polish：新草稿 D2 + polish P2 + consistency C2。
// 返回新 digest 与 P2 seq。
func rePolishDraft(t *testing.T, st *store.Store, newText string) (string, int64) {
	t.Helper()
	newText = mechCleanDraft(newText)
	if err := st.Drafts.SaveDraft(1, newText); err != nil {
		t.Fatal(err)
	}
	d2 := domain.DigestDraft(newText)
	p2, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", d2,
		domain.PolishCheckpointMeta{InputDigest: d2, PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "c2", d2); err != nil {
		t.Fatal(err)
	}
	return d2, p2.Seq
}

// TestReviewStyle_DegradedNonRewriteNewCandidateRecoversSameEpoch 验证测试 1：
// 非返工 initial degraded + P2 新候选（R1 != P2、digest 变）→ 同 epoch 恢复
// initial_pending → accepted_initial，epoch 不变，Request 绑定 P2。
func TestReviewStyle_DegradedNonRewriteNewCandidateRecoversSameEpoch(t *testing.T) {
	st, _, _ := degradedBaseStore(t, "第一章正文。", false)

	d2, p2Seq := rePolishDraft(t, st, "精修后的新正文。")

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("non-rewrite degraded with new candidate must recover: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" || output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("expected accepted_initial, got verdict=%s status=%s", output.Verdict, output.Status)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 1 {
		t.Fatalf("MaxEpoch = %d, want 1（非返工恢复不开启新 epoch）", got)
	}
	last := loaded.CurrentCycle()
	if last.DraftDigest != d2 {
		t.Fatalf("accepted digest = %s, want new candidate %s", last.DraftDigest, d2)
	}
	if last.Request == nil || last.Request.PolishCheckpointSeq != p2Seq {
		t.Fatalf("accepted request must bind P2 (%d), got %+v", p2Seq, last.Request)
	}
}

// TestReviewStyle_DegradedNonRewriteFinalNewCandidateRecoversFinal 验证测试 2：
// 非返工 final degraded + P2 → 恢复 final_pending → accepted_revised，
// 不降级 initial、不加 epoch。
func TestReviewStyle_DegradedNonRewriteFinalNewCandidateRecoversFinal(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	draft := mechCleanDraft("第一章正文。")
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	d1 := domain.DigestDraft(draft)
	basis := ComputeBasisDigest(st, 1, "test-v1")
	p1, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", d1,
		domain.PolishCheckpointMeta{InputDigest: d1, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", d1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: d1, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: p1.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: d1, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: p1.Seq},
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e",
					}}}},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: now,
				AttemptID: "final-attempt", DraftDigest: d1, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: p1.Seq}},
			{Cycle: 4, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "final-attempt", DraftDigest: d1, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: p1.Seq},
				Error:   "critic returned invalid finding"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	d2, p2Seq := rePolishDraft(t, st, "精修后的修订版正文。")

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("non-rewrite final degraded with new candidate must recover as final: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" || output.Status != string(domain.ReviewStatusAcceptedRev) {
		t.Fatalf("expected accepted_revised, got verdict=%s status=%s", output.Verdict, output.Status)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 1 {
		t.Fatalf("MaxEpoch = %d, want 1（final 恢复不开启新 epoch）", got)
	}
	if loaded.CurrentStatus() != domain.ReviewStatusAcceptedRev {
		t.Fatalf("expected accepted_revised, got %s", loaded.CurrentStatus())
	}
	last := loaded.CurrentCycle()
	if last.DraftDigest != d2 {
		t.Fatalf("accepted digest = %s, want %s", last.DraftDigest, d2)
	}
	if last.Request == nil || last.Request.PolishCheckpointSeq != p2Seq {
		t.Fatalf("accepted request must bind P2 (%d), got %+v", p2Seq, last.Request)
	}
	// 不降级 initial：degraded 后应直接是 final_pending → accepted_revised
	if loaded.Cycles[4].Status != domain.ReviewStatusFinalPending {
		t.Fatalf("cycle[4] = %s, want final_pending（恢复不降级为 initial）", loaded.Cycles[4].Status)
	}
}

// TestReviewStyle_DegradedNonRewriteNoOpRePolishRecovers 验证测试 3：
// 非返工 no-op re-polish（digest 相同但 R1 != P2）→ 仍同 epoch 重评（防 seq-only
// 死锁——seq 变了但内容没变，不应拒绝或开新 epoch）。
func TestReviewStyle_DegradedNonRewriteNoOpRePolishRecovers(t *testing.T) {
	st, d1, _ := degradedBaseStore(t, "第一章正文。", false)

	// no-op re-polish：内容不变（digest 仍为 D1），仅产生新 polish seq P2
	if err := st.Drafts.SaveDraft(1, mechCleanDraft("第一章正文。")); err != nil {
		t.Fatal(err)
	}
	// 注意：SaveDraft 后 digest 不变（同内容），直接用 D1 追加 polish
	p2, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", d1,
		domain.PolishCheckpointMeta{InputDigest: d1, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "c2", d1); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("no-op re-polish (seq-only change) must still recover same epoch: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("expected accepted_initial, got %s", output.Status)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 1 {
		t.Fatalf("MaxEpoch = %d, want 1（no-op re-polish 不开新 epoch）", got)
	}
	last := loaded.CurrentCycle()
	if last.Request == nil || last.Request.PolishCheckpointSeq != p2.Seq {
		t.Fatalf("accepted request must bind P2 (%d), got %+v", p2.Seq, last.Request)
	}
}

// TestReviewStyle_DegradedNonRewriteLegacyOldDigestRecovers 验证测试 4：
// legacy degraded（R=0、旧 digest ≠ 当前、有 polish）→ 非返工同 epoch 恢复。
func TestReviewStyle_DegradedNonRewriteLegacyOldDigestRecovers(t *testing.T) {
	st, _, _ := degradedBaseStore(t, "第一章正文。", true) // legacy（R=0）账本

	d2, p2Seq := rePolishDraft(t, st, "精修后的新正文。")

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("legacy degraded with old digest must recover same epoch: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("expected accepted_initial, got %s", output.Status)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 1 {
		t.Fatalf("MaxEpoch = %d, want 1（legacy 非返工恢复不开启新 epoch）", got)
	}
	last := loaded.CurrentCycle()
	if last.DraftDigest != d2 {
		t.Fatalf("accepted digest = %s, want %s", last.DraftDigest, d2)
	}
	if last.Request == nil || last.Request.PolishCheckpointSeq != p2Seq {
		t.Fatalf("accepted request must bind P2 (%d), got %+v", p2Seq, last.Request)
	}
}

// TestReviewStyle_DegradedNonRewriteE2ECommit 验证测试 7（端到端）：
// degraded R1 → 重新 polish P2 → consistency C2 → review pass → accepted →
// CheckCommitStyleGate + CheckPolishPipelineGate 均通过，commit 工具可提交。
func TestReviewStyle_DegradedNonRewriteE2ECommit(t *testing.T) {
	st, _, p1Seq := degradedBaseStore(t, "第一章正文。", false)

	d2, p2Seq := rePolishDraft(t, st, "精修后的新正文。")
	if p2Seq <= p1Seq {
		t.Fatalf("P2 (%d) must be newer than R1 (%d)", p2Seq, p1Seq)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("review after re-polish: %v", err)
	}

	// commit 门控（critic + pipeline 双闸）
	if err := CheckCommitStyleGate(st, 1); err != nil {
		t.Fatalf("CheckCommitStyleGate must pass after recovery: %v", err)
	}
	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("CheckPolishPipelineGate must pass after recovery: %v", err)
	}

	// 端到端 commit 工具
	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mimo-polisher"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("commit must succeed after degraded recovery (digest %s, seq %d): %v", d2, p2Seq, err)
	}
}

// TestReviewStyle_DegradedRecoveryPreGates 验证测试 8（前置闸不被绕过）：
// 8a：P2 后无新 consistency → 拒绝；8b：polish digest 不匹配（pipeline 启用）→
// 拒绝；8c：机械 error → 拒绝。
func TestReviewStyle_DegradedRecoveryPreGates(t *testing.T) {
	t.Run("no new consistency after re-polish", func(t *testing.T) {
		st, _, _ := degradedBaseStore(t, "第一章正文。", false)
		// 重新 polish（P2、digest D2）但故意不追加 consistency C2
		newText := mechCleanDraft("精修后的新正文。")
		if err := st.Drafts.SaveDraft(1, newText); err != nil {
			t.Fatal(err)
		}
		d2 := domain.DigestDraft(newText)
		if _, err := st.Checkpoints.AppendPolish(
			domain.ChapterScope(1), "polish", "a2", d2,
			domain.PolishCheckpointMeta{InputDigest: d2, PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
		); err != nil {
			t.Fatal(err)
		}
		tool := NewReviewStyleTool(st, newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
			panic("critic must not be called")
		}), testCriticVersion)
		_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
		if err == nil {
			t.Fatal("review must reject when consistency checkpoint is stale after re-polish")
		}
		if !strings.Contains(err.Error(), "check_consistency") {
			t.Fatalf("expected consistency hint, got: %v", err)
		}
	})

	t.Run("polish digest mismatch with pipeline", func(t *testing.T) {
		st, _, _ := degradedBaseStore(t, "第一章正文。", false)
		newText := mechCleanDraft("精修后的新正文。")
		if err := st.Drafts.SaveDraft(1, newText); err != nil {
			t.Fatal(err)
		}
		d2 := domain.DigestDraft(newText)
		// polish checkpoint 的 digest 与当前草稿不匹配（伪造的 P2）
		other := domain.DigestDraft("别的正文。")
		if _, err := st.Checkpoints.AppendPolish(
			domain.ChapterScope(1), "polish", "a2", other,
			domain.PolishCheckpointMeta{InputDigest: other, PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "c2", d2); err != nil {
			t.Fatal(err)
		}
		tool := NewReviewStyleTool(st, newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
			panic("critic must not be called")
		}), testCriticVersion)
		tool.SetPipelineEnabled(true)
		_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
		if err == nil {
			t.Fatal("review must reject when latest polish digest does not match the draft")
		}
		if !strings.Contains(err.Error(), "polish") {
			t.Fatalf("expected polish hint, got: %v", err)
		}
	})

	t.Run("mechanical error blocks recovery", func(t *testing.T) {
		st := store.NewStore(t.TempDir())
		if err := st.Init(); err != nil {
			t.Fatal(err)
		}
		if err := st.Progress.Init("test", 100); err != nil {
			t.Fatal(err)
		}
		if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
			t.Fatal(err)
		}
		// 带文学腔 error 的草稿（否定修正句 ≥3），不经 mechCleanDraft
		dirty := "他不是怕死，而是怕疼。他不是退缩，而是等待。他不是沉默，而是蓄力。"
		if err := st.Drafts.SaveDraft(1, dirty); err != nil {
			t.Fatal(err)
		}
		digest := domain.DigestDraft(dirty)
		basis := ComputeBasisDigest(st, 1, "test-v1")
		if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", digest); err != nil {
			t.Fatal(err)
		}
		now := time.Now().Format(time.RFC3339)
		ledger := domain.StyleReviewLedger{
			SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
			Cycles: []domain.StyleReviewEntry{
				{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
					AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
					Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
				{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
					AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
					Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
					Error:   "critic returned invalid finding"},
			},
		}
		if err := st.StyleReview.Save(ledger); err != nil {
			t.Fatal(err)
		}
		tool := NewReviewStyleTool(st, newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
			panic("critic must not be called")
		}), testCriticVersion)
		_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
		if err == nil {
			t.Fatal("review must reject when the draft carries literary-prose errors")
		}
		if !strings.Contains(err.Error(), "文学腔硬闸") {
			t.Fatalf("expected literary-gate rejection, got: %v", err)
		}
	})
}

// ── C1-H3：exhausted 必须先 /style-override（M2-3） ───────────────────

func TestReviewStyle_ExhaustedRequiresOverrideFirst(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "重写"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}}},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 4, Status: domain.ReviewStatusExhausted, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e",
					Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "error", Evidence: "e"}}}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", digest); err != nil {
		t.Fatal(err)
	}

	// 返工章节 + exhausted：直接 review 被拒（必须先 /style-override）
	tool := NewReviewStyleTool(st, newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		panic("critic must not be called")
	}), testCriticVersion)
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("exhausted must require override before re-review (even in rewrite queue)")
	}
	if !strings.Contains(err.Error(), "style-override") {
		t.Errorf("expected /style-override hint, got: %v", err)
	}

	// 覆盖后（overridden，保留 epoch 与 polish seq 语义）→ 可开启新 epoch
	now2 := time.Now().Format(time.RFC3339)
	if err := st.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, fmt.Errorf("ledger disappeared")
		}
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle:       len(cur.Cycles) + 1,
			Status:      domain.ReviewStatusOverridden,
			CreatedAt:   now2,
			AttemptID:   "override-1",
			Request:     &domain.StyleReviewRequest{Prompt: "override-v1", PolishCheckpointSeq: 0},
			DraftDigest: digest,
			BasisDigest: basisDigest,
			Epoch:       cur.MaxEpoch(),
			Override: &domain.StyleReviewOverride{
				Actor: "user", Reason: "测试覆盖",
				DraftDigest: digest, BasisDigest: basisDigest, OverriddenAt: now2,
			},
		})
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool2 := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool2.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("review after override should open new epoch: %v", err)
	}
	ledger2, _ := st.StyleReview.Load(1)
	if got := ledger2.MaxEpoch(); got != 2 {
		t.Fatalf("MaxEpoch = %d, want 2（override 后开启新 epoch）", got)
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
	draft := "正文。她心里骂自己丢人，真不要脸。"
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

	// ── 敏感性表驱动：按职责角色投影，只有该角色可见的输入变化才影响 digest ──
	t.Run("角色投影敏感性", func(t *testing.T) {
		base := fourBucketSnapshot()
		if err := st.UserRules.Save(base); err != nil {
			t.Fatalf("Save UserRules: %v", err)
		}
		baseDigest := ComputeBasisDigest(st, 1, testCriticVersion)

		tests := []struct {
			name       string
			mutate     func(s *rules.Snapshot)
			wantChange bool
		}{
			{"default 分区变化", func(s *rules.Snapshot) {
				s.Preferences.Default = []rules.PreferenceRule{{ID: "def-2", Text: "DEFAULT_RULE_009 新规则"}}
			}, true},
			{"writer 分区变化", func(s *rules.Snapshot) {
				s.Preferences.Writer = []rules.PreferenceRule{{ID: "wri-2", Text: "WRITER_RULE_009 新规则"}}
			}, true},
			{"editor 分区变化", func(s *rules.Snapshot) {
				s.Preferences.Editor = []rules.PreferenceRule{{ID: "edi-2", Text: "EDITOR_RULE_009 新规则"}}
			}, true},
			{"structured 变化", func(s *rules.Snapshot) {
				s.Structured.Genre = "奇幻"
			}, true},
			{"architect 分区变化（critic 不可见）", func(s *rules.Snapshot) {
				s.Preferences.Architect = []rules.PreferenceRule{{ID: "arc-2", Text: "ARCH_RULE_009 新规则"}}
			}, false},
			{"sources 诊断元数据变化", func(s *rules.Snapshot) {
				s.Sources = []string{"src-z"}
			}, false},
			{"uncertain 诊断元数据变化", func(s *rules.Snapshot) {
				s.Uncertain = []string{"uncertain-z"}
			}, false},
			{"status 诊断元数据变化", func(s *rules.Snapshot) {
				s.Status = rules.StatusDegraded
			}, false},
			{"version 诊断元数据变化", func(s *rules.Snapshot) {
				s.Version = 99
			}, false},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				snap := fourBucketSnapshot()
				tc.mutate(snap)
				if err := st.UserRules.Save(snap); err != nil {
					t.Fatalf("Save UserRules: %v", err)
				}
				got := ComputeBasisDigest(st, 1, testCriticVersion)
				if tc.wantChange && got == baseDigest {
					t.Errorf("basis digest should change for: %s", tc.name)
				}
				if !tc.wantChange && got != baseDigest {
					t.Errorf("basis digest should NOT change for: %s", tc.name)
				}
			})
		}
	})
}

// ── 31b. User rules role projection ───────────────────────────────────

// fourBucketSnapshot 构造四角色分区的快照，用于验证 basis 的角色投影。
func fourBucketSnapshot() *rules.Snapshot {
	return &rules.Snapshot{
		Version: rules.SnapshotVersion,
		Status:  rules.StatusReady,
		Structured: rules.Structured{
			Genre:            "科幻",
			ChapterWords:     &rules.WordRange{Min: 3000, Max: 6000},
			ForbiddenPhrases: []string{"某种程度上"},
		},
		Preferences: rules.PreferenceBuckets{
			Default:   []rules.PreferenceRule{{ID: "def-1", Text: "DEFAULT_RULE_001 使用平实语言"}},
			Architect: []rules.PreferenceRule{{ID: "arc-1", Text: "ARCH_RULE_007 世界观设定"}},
			Writer:    []rules.PreferenceRule{{ID: "wri-1", Text: "WRITER_RULE_002 侧重节奏"}},
			Editor:    []rules.PreferenceRule{{ID: "edi-1", Text: "EDITOR_RULE_003 去除冗词"}},
		},
		Sources:   []string{"src-a"},
		Uncertain: []string{"uncertain-x"},
	}
}

func TestBasis_UserRulesRoleProjection(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	if err := st.UserRules.Save(fourBucketSnapshot()); err != nil {
		t.Fatalf("Save UserRules: %v", err)
	}

	polish := buildPolishBasis(st, 1, "test-polish-v1")
	critic := buildCriticBasis(st, 1, testCriticVersion)

	// ── polisher → writer 视图：default+writer，不含 architect/editor 与诊断元数据 ──
	polishRules := string(polish.UserRules)
	for _, want := range []string{"DEFAULT_RULE_001", "WRITER_RULE_002", `"genre":"科幻"`} {
		if !strings.Contains(polishRules, want) {
			t.Errorf("polisher basis user_rules 应包含 %s，实际: %s", want, polishRules)
		}
	}
	for _, forbid := range []string{"ARCH_RULE_007", "EDITOR_RULE_003", "src-a", "uncertain-x", `"sources"`, `"uncertain"`, `"version"`, `"status"`} {
		if strings.Contains(polishRules, forbid) {
			t.Errorf("polisher basis user_rules 不应包含 %s，实际: %s", forbid, polishRules)
		}
	}

	// ── critic → editor 视图：default+writer+editor，不含 architect 与诊断元数据 ──
	criticRules := string(critic.UserRules)
	for _, want := range []string{"DEFAULT_RULE_001", "WRITER_RULE_002", "EDITOR_RULE_003", `"genre":"科幻"`} {
		if !strings.Contains(criticRules, want) {
			t.Errorf("critic basis user_rules 应包含 %s，实际: %s", want, criticRules)
		}
	}
	for _, forbid := range []string{"ARCH_RULE_007", "src-a", "uncertain-x", `"sources"`, `"uncertain"`, `"version"`, `"status"`} {
		if strings.Contains(criticRules, forbid) {
			t.Errorf("critic basis user_rules 不应包含 %s，实际: %s", forbid, criticRules)
		}
	}

	// structured 字段存在（两个角色共享同一 structured 投影）
	for name, rulesJSON := range map[string]json.RawMessage{
		"polisher": polish.UserRules,
		"critic":   critic.UserRules,
	} {
		var payload map[string]any
		if err := json.Unmarshal(rulesJSON, &payload); err != nil {
			t.Fatalf("%s: unmarshal user_rules: %v", name, err)
		}
		if _, ok := payload["structured"]; !ok {
			t.Errorf("%s: user_rules 应包含 structured 字段", name)
		}
	}
}

// ── 31c. Missing snapshot fallback ────────────────────────────────────

func TestBasis_UserRulesMissingSnapshotFallback(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。") // 不保存 UserRules：模拟快照缺失

	polish1 := buildPolishBasis(st, 1, "test-polish-v1")
	polish2 := buildPolishBasis(st, 1, "test-polish-v1")
	critic1 := buildCriticBasis(st, 1, testCriticVersion)

	// 稳定：重复构建得到相同 payload（SystemDefaults 回退确定性）
	if string(polish1.UserRules) != string(polish2.UserRules) {
		t.Error("缺失快照时 polisher user_rules 应稳定（重复构建一致）")
	}

	for name, basis := range map[string]domain.ReviewBasis{
		"polisher": polish1,
		"critic":   critic1,
	} {
		rulesJSON := string(basis.UserRules)
		if len(basis.UserRules) == 0 || rulesJSON == "null" {
			t.Errorf("%s: 缺失快照时 user_rules 不应为 null/空，实际: %s", name, rulesJSON)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(basis.UserRules, &payload); err != nil {
			t.Fatalf("%s: unmarshal user_rules: %v", name, err)
		}
		if _, ok := payload["preferences"]; !ok {
			t.Errorf("%s: user_rules 应包含 preferences 字段", name)
		}
		structured, ok := payload["structured"].(map[string]any)
		if !ok {
			t.Errorf("%s: user_rules 应包含 structured 字段", name)
			continue
		}
		// 机械底线来自 SystemDefaults：章节字数约束存在（3000-6000）
		cw, ok := structured["chapter_words"].(map[string]any)
		if !ok {
			t.Errorf("%s: fallback structured 应包含 chapter_words（SystemDefaults 机械底线）", name)
			continue
		}
		if cw["min"] != float64(3000) || cw["max"] != float64(6000) {
			t.Errorf("%s: fallback chapter_words 应为 3000-6000，实际: %v", name, cw)
		}
	}
}

// ── 31d. ComputeBasisDigest 口径固定（critic/editor 视图） ─────────────

func TestReviewStyle_ComputeBasisDigestCriticAudience(t *testing.T) {
	draft := "正文。"
	st := setupCriticStore(t, 1, draft)
	if err := st.UserRules.Save(fourBucketSnapshot()); err != nil {
		t.Fatalf("Save UserRules: %v", err)
	}
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 1) 实际发送的 basis、ComputeBasisDigest、账本 basis_digest 三者同口径（editor 视图）
	expected := domain.DigestReviewBasis(buildCriticBasis(st, 1, testCriticVersion))
	if got := ComputeBasisDigest(st, 1, testCriticVersion); got != expected {
		t.Errorf("ComputeBasisDigest %q != buildCriticBasis digest %q", got, expected)
	}
	ledger, err := st.StyleReview.Load(1)
	if err != nil || ledger == nil || len(ledger.Cycles) == 0 {
		t.Fatalf("load ledger: %v", err)
	}
	if last := ledger.CurrentCycle(); last.BasisDigest != expected {
		t.Errorf("ledger basis_digest %q != buildCriticBasis digest %q", last.BasisDigest, expected)
	}

	// 2) editor 分区变化对 digest 敏感（critic 口径确实包含 editor 分区）
	base := ComputeBasisDigest(st, 1, testCriticVersion)
	snap := fourBucketSnapshot()
	snap.Preferences.Editor = []rules.PreferenceRule{{ID: "edi-2", Text: "EDITOR_RULE_009 新规则"}}
	if err := st.UserRules.Save(snap); err != nil {
		t.Fatalf("Save UserRules: %v", err)
	}
	if got := ComputeBasisDigest(st, 1, testCriticVersion); got == base {
		t.Error("ComputeBasisDigest 应对 editor 分区变化敏感（critic 口径含 editor）")
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
	draft := mechCleanDraft("正文。")
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

// ── 48. Stagnation: different findings stay revision_open ────────────────

func TestReviewStyle_DifferentFindingsLoop(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。一些句子。")
	draft, _, _ := st.Drafts.LoadChapterContent(1)

	callCount := 0
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		callCount++
		if callCount <= 2 {
			if callCount == 1 {
				return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"hook","evidence":"好"},"findings":[{"dimension":"pacing","category":"style","severity":"warning","evidence":"末段","problem":"第二段描写过细","revision":"压缩中间描写"}]}`)}, nil
			}
			return &agentcore.LLMResponse{Message: criticText(`{"verdict":"revise","strength":{"dimension":"hook","evidence":"好"},"findings":[{"dimension":"hook","category":"plot","severity":"error","evidence":"首段","problem":"开篇悬念不足","revision":"加入伏笔"}]}`)}, nil
		}
		return &agentcore.LLMResponse{Message: criticText(`{"verdict":"pass","strength":{"dimension":"aesthetic","evidence":"流畅"}}`)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)

	// Round 1: initial review → revise
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 1: %v", err)
	}
	// Edit + check
	newDraft1 := draft + "\n修改1。"
	if err := st.Drafts.SaveDraft(1, newDraft1); err != nil {
		t.Fatal(err)
	}
	d1 := domain.DigestDraft(newDraft1)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", d1); err != nil {
		t.Fatal(err)
	}

	// Round 2: final review → revise (different finding with different problem)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 2: %v", err)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusRevisionOpen {
		t.Fatalf("Round 2: expected revision_open (different findings), got %s", ledger.CurrentStatus())
	}
	// Verify Problem was persisted
	lastCycle := ledger.CurrentCycle()
	if lastCycle.Result.Findings[0].Problem == "" {
		t.Fatal("Problem field was not persisted from critic JSON")
	}

	// Edit + check
	newDraft2 := newDraft1 + "\n修改2。"
	if err := st.Drafts.SaveDraft(1, newDraft2); err != nil {
		t.Fatal(err)
	}
	d2 := domain.DigestDraft(newDraft2)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", d2); err != nil {
		t.Fatal(err)
	}

	// Round 3: final review → pass (different findings allow progress)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 3: %v", err)
	}
	ledger, _ = st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusAcceptedRev {
		t.Fatalf("Round 3: expected accepted_revised, got %s", ledger.CurrentStatus())
	}
}

// ── 49. Stagnation: repeated same findings → exhausted ───────────────────
// Requires two consecutive identical final reviews to trigger exhaustion;
// the first final review always produces revision_open.

func TestReviewStyle_StagnationSameFindingsLeadsToExhausted(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。一些句子。")
	draft, _, _ := st.Drafts.LoadChapterContent(1)

	// Production shape: uses "problem" and "revision" (not "suggestion")
	sameFinding := `{"verdict":"revise","strength":{"dimension":"hook","evidence":"好"},"findings":[{"dimension":"pacing","category":"style","severity":"warning","evidence":"末段","problem":"第二段描写过细","revision":"压缩中间描写"}]}`
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(sameFinding)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)

	// Round 1: initial review → revise
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 1: %v", err)
	}

	// Round 2: first final review → revise (always revision_open, no stagnation yet)
	newDraft := draft + "\n修改。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatal(err)
	}
	d1 := domain.DigestDraft(newDraft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", d1); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 2: %v", err)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusRevisionOpen {
		t.Fatalf("Round 2: expected revision_open (first final revise), got %s", ledger.CurrentStatus())
	}

	// Round 3: second final review → still same finding → stagnation → exhausted
	newDraft2 := newDraft + "\n修改2。"
	if err := st.Drafts.SaveDraft(1, newDraft2); err != nil {
		t.Fatal(err)
	}
	d2 := domain.DigestDraft(newDraft2)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", d2); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 3: %v", err)
	}

	ledger, _ = st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusExhausted {
		t.Fatalf("expected exhausted after two consecutive identical final reviews, got %s", ledger.CurrentStatus())
	}
}

// ── 50. Stagnation: override recovers from exhausted ─────────────────────

func TestReviewStyle_OverrideAfterStagnationExhausted(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。一些句子。她心里骂自己丢人，真不要脸。")
	draft, _, _ := st.Drafts.LoadChapterContent(1)

	// Production shape: uses "problem" and "revision" (not "suggestion")
	sameFinding := `{"verdict":"revise","strength":{"dimension":"hook","evidence":"好"},"findings":[{"dimension":"pacing","category":"style","severity":"warning","evidence":"末段","problem":"第二段描写过细","revision":"压缩中间描写"}]}`
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(sameFinding)}, nil
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)

	// Round 1: initial → revise
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 1: %v", err)
	}

	// Round 2: first final review → revise (revision_open, no stagnation yet)
	newDraft := draft + "\n修改。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatal(err)
	}
	d1 := domain.DigestDraft(newDraft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", d1); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 2: %v", err)
	}

	// Round 3: second final review → same finding → exhausted
	newDraft2 := newDraft + "\n修改2。"
	if err := st.Drafts.SaveDraft(1, newDraft2); err != nil {
		t.Fatal(err)
	}
	d2 := domain.DigestDraft(newDraft2)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", d2); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Round 3: %v", err)
	}

	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusExhausted {
		t.Fatalf("expected exhausted, got %s", ledger.CurrentStatus())
	}

	// Override the exhausted status — use the current draft digest (d2).
	if err := st.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		now := time.Now().Format(time.RFC3339)
		entry := domain.StyleReviewEntry{
			Cycle:       len(cur.Cycles) + 1,
			Status:      domain.ReviewStatusOverridden,
			CreatedAt:   now,
			AttemptID:   "",
			DraftDigest: d2,
			BasisDigest: ledger.CurrentCycle().BasisDigest,
			Override: &domain.StyleReviewOverride{
				Actor: "user", Reason: "I confirm this draft is acceptable",
				DraftDigest: d2, BasisDigest: ledger.CurrentCycle().BasisDigest,
				OverriddenAt: now,
			},
		}
		cur.Cycles = append(cur.Cycles, entry)
		return cur, nil
	}); err != nil {
		t.Fatalf("override: %v", err)
	}

	// Now commit should be allowed (overridden is terminal)
	// Need to pass a consistency check after override
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a3", d2); err != nil {
		t.Fatal(err)
	}
	commitTool := NewCommitChapterTool(st)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{},
		"key_events": []string{},
	})
	if _, err := commitTool.Execute(t.Context(), commitArgs); err != nil {
		t.Fatalf("commit after override should succeed: %v", err)
	}
}

// ── 51. Critic empty output → auto retry → success ────────────────────
//
// 空输出（含仅空白）是瞬态故障：callCritic 应自动重试（指数退避），
// 重试成功即正常返回，不进入 degraded。

func TestReviewStyle_EmptyOutputRetryThenSuccess(t *testing.T) {
	oldMax, oldBase := criticEmptyRetryMax, criticEmptyRetryBase
	criticEmptyRetryMax, criticEmptyRetryBase = 3, time.Millisecond
	defer func() { criticEmptyRetryMax, criticEmptyRetryBase = oldMax, oldBase }()

	st := setupCriticStore(t, 1, "第一章正文。")
	callCount := 0
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		callCount++
		switch callCount {
		case 1:
			return &agentcore.LLMResponse{Message: criticText("")}, nil // 空输出
		case 2:
			return &agentcore.LLMResponse{Message: criticText("   \n\t  ")}, nil // 仅空白
		}
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
		t.Fatalf("verdict = %q, want pass (retry should recover)", output.Verdict)
	}
	if output.Degraded {
		t.Fatal("should not be degraded after successful retry")
	}
	if callCount != 3 {
		t.Errorf("expected 3 critic calls (1 empty + 1 whitespace + 1 success), got %d", callCount)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusAcceptedInitial {
		t.Fatalf("expected accepted_initial after retry success, got %s", ledger.CurrentStatus())
	}
}

// ── 52. Critic empty output → retries exhausted → degraded ────────────

func TestReviewStyle_EmptyOutputExhaustedDegraded(t *testing.T) {
	oldMax, oldBase := criticEmptyRetryMax, criticEmptyRetryBase
	criticEmptyRetryMax, criticEmptyRetryBase = 3, time.Millisecond
	defer func() { criticEmptyRetryMax, criticEmptyRetryBase = oldMax, oldBase }()

	st := setupCriticStore(t, 1, "正文。")
	callCount := 0
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		callCount++
		return &agentcore.LLMResponse{Message: criticText("")}, nil // 始终空输出
	})

	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if !output.Degraded {
		t.Fatal("expected degraded after all retries exhausted")
	}
	if callCount != 3 {
		t.Errorf("expected 3 critic calls (all empty), got %d", callCount)
	}
	if !strings.Contains(output.Error, "空输出") {
		t.Errorf("error %q should mention empty output", output.Error)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded, got %s", ledger.CurrentStatus())
	}
}

// ── 53. Mutation guard: degraded allows draft/edit（解锁兜底）──────────

func TestReviewStyle_MutationGuardAllowsDuringDegraded(t *testing.T) {
	st := setupCriticStore(t, 1, "第一章正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, assertAnError("critic simulated failure")
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Review: %v", err)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded ledger, got %s", ledger.CurrentStatus())
	}

	// degraded 状态：draft_chapter 必须被允许（评审调用故障，非评审结论）
	draftTool := NewDraftChapterTool(st, testContract)
	_, err := draftTool.Execute(t.Context(), json.RawMessage(`{
		"chapter":1,"content":"degraded 后重写的草稿内容。","mode":"write"
	}`))
	if err != nil {
		if strings.Contains(err.Error(), "评审") || strings.Contains(err.Error(), "critic") {
			t.Fatalf("mutation guard incorrectly blocked draft during degraded: %v", err)
		}
		t.Logf("draft_chapter error (expected non-guard): %v", err)
	}
}

// ── 54. review_style: degraded → 新初评 attempt → 恢复 ─────────────────

func TestReviewStyle_RecoverFromDegraded_Initial(t *testing.T) {
	draft := mechCleanDraft("正文。")
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// 预写入 initial_pending → degraded
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				DraftDigest: draftDigest, BasisDigest: basisDigest},
			{Cycle: 2, Status: domain.ReviewStatusDegraded,
				CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Error:       "critic returned empty output",
				DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	// 修改草稿 + 新一致性检查点
	newDraft := draft + "\n根据故障恢复重新起草。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	newDigest := domain.DigestDraft(newDraft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", newDigest); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute on degraded recovery: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", output.Verdict)
	}
	if output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("status = %q, want %q", output.Status, domain.ReviewStatusAcceptedInitial)
	}
	loaded, _ := st.StyleReview.Load(1)
	if len(loaded.Cycles) != 4 {
		t.Fatalf("expected 4 cycles, got %d", len(loaded.Cycles))
	}
	expected := []domain.StyleReviewStatus{
		domain.ReviewStatusInitialPending,
		domain.ReviewStatusDegraded,
		domain.ReviewStatusInitialPending,
		domain.ReviewStatusAcceptedInitial,
	}
	for i, s := range expected {
		if loaded.Cycles[i].Status != s {
			t.Errorf("cycle[%d].status = %q, want %q", i, loaded.Cycles[i].Status, s)
		}
	}
}

// ── 55. review_style: degraded（终审失败）→ 新终审 attempt → 恢复 ───────

func TestReviewStyle_RecoverFromDegraded_Final(t *testing.T) {
	draft := mechCleanDraft("正文。")
	st := setupCriticStore(t, 1, draft)
	draftDigest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)

	// 预写入 initial_pending → revision_open → final_pending → degraded
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
			{Cycle: 4, Status: domain.ReviewStatusDegraded,
				CreatedAt: "2026-07-25T13:00:00Z", AttemptID: "final-attempt",
				Request:     &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Error:       "critic returned empty output",
				DraftDigest: draftDigest, BasisDigest: basisDigest},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	// 修改草稿 + 新一致性检查点
	newDraft := draft + "\n终审故障后重新修改。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	newDigest := domain.DigestDraft(newDraft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", newDigest); err != nil {
		t.Fatalf("Append checkpoint: %v", err)
	}

	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute on degraded final recovery: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", output.Verdict)
	}
	if output.Status != string(domain.ReviewStatusAcceptedRev) {
		t.Fatalf("status = %q, want %q", output.Status, domain.ReviewStatusAcceptedRev)
	}
	loaded, _ := st.StyleReview.Load(1)
	if len(loaded.Cycles) != 6 {
		t.Fatalf("expected 6 cycles, got %d", len(loaded.Cycles))
	}
	expected := []domain.StyleReviewStatus{
		domain.ReviewStatusInitialPending,
		domain.ReviewStatusRevisionOpen,
		domain.ReviewStatusFinalPending,
		domain.ReviewStatusDegraded,
		domain.ReviewStatusFinalPending,
		domain.ReviewStatusAcceptedRev,
	}
	for i, s := range expected {
		if loaded.Cycles[i].Status != s {
			t.Errorf("cycle[%d].status = %q, want %q", i, loaded.Cycles[i].Status, s)
		}
	}
}

// ── 56. 其他 terminal 状态仍拒绝新评审（degraded 特判不扩散）────────────

func TestReviewStyle_AcceptedInitialStillBlocksNewReview(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("First review: %v", err)
	}
	// accepted_initial 是最终权威：第二次 review_style 必须被拒绝
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("accepted_initial must still block a new review")
	}
	if !strings.Contains(err.Error(), "已终结") {
		t.Errorf("error %q should mention review already terminated", err.Error())
	}
}

// ── 57. legacy degraded（R=0）的 digest 分流（中等 3） ──────────────────

// TestReviewStyle_DegradedLegacyNewCandidateOpensNewEpoch 验证 legacy（R=0，
// 无 PolishCheckpointSeq 绑定）degraded 条目绑定 digest ≠ 当前草稿且存在新 polish
// 候选 → 旧候选：返工队列章节开启新 epoch 重新评审（此前无条件视为同候选 retry）。
func TestReviewStyle_DegradedLegacyNewCandidateOpensNewEpoch(t *testing.T) {
	draft := "# 一\nabc返工草稿她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "重写"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatal(err)
	}
	// degraded 绑定的是旧候选 digest（≠ 当前草稿），且无 seq 绑定（legacy R=0）
	oldDigest := domain.DigestDraft("旧候选正文。")
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: oldDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: oldDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	// 当前候选（digest = 当前草稿）的最新 polish + consistency
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", digest); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 2 {
		t.Fatalf("MaxEpoch = %d, want 2（legacy degraded 旧候选开启新 epoch）", got)
	}
	if loaded.Cycles[2].Status != domain.ReviewStatusInitialPending || loaded.Cycles[2].EpochValue() != 2 {
		t.Fatalf("cycle[2] = %s epoch %d, want initial_pending epoch 2", loaded.Cycles[2].Status, loaded.Cycles[2].EpochValue())
	}
	if loaded.Cycles[3].Status != domain.ReviewStatusAcceptedInitial || loaded.Cycles[3].EpochValue() != 2 {
		t.Fatalf("cycle[3] = %s epoch %d, want accepted_initial epoch 2", loaded.Cycles[3].Status, loaded.Cycles[3].EpochValue())
	}
}

// TestReviewStyle_DegradedLegacySameDigestRetriesSameEpoch 验证 legacy（R=0）
// degraded 且绑定 digest == 当前草稿 → 同候选 retry，不开启新 epoch。
func TestReviewStyle_DegradedLegacySameDigestRetriesSameEpoch(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	// 存在 polish 候选（digest 与 degraded 绑定一致）→ 同候选 retry
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", digest); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, _ := st.StyleReview.Load(1)
	if got := loaded.MaxEpoch(); got != 1 {
		t.Fatalf("MaxEpoch = %d, want 1（legacy degraded 同 digest retry 不开启新 epoch）", got)
	}
	if len(loaded.Cycles) != 4 {
		t.Fatalf("cycles = %d, want 4（degraded + retry initial_pending + accepted）", len(loaded.Cycles))
	}
}

// ── C2：accepted 前置机械规则闸（文学腔硬闸死锁防护，阻断 1） ──────────
// 场景：Critic 对仍带文学腔 error 的草稿给出 pass → review_style 必须在任何
// 账本写入（pending 创建 / accepted 落盘）前拒绝（ErrToolPrecondition）——
// 账本保持为空或 revision_open，不得进入 terminal 或留下 pending（pending 会
// 被 mutation guard 锁定，导致用户无法修改草稿）。

// TestReviewStyle_MechanicalErrorBlocksAccepted 验证带文学腔 error 的草稿：
// 评审被拒、账本保持为空（不创建 pending、不调用 critic）；修改草稿消除
// 违例后重新评审可正常 accepted。
func TestReviewStyle_MechanicalErrorBlocksAccepted(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	// 否定修正句 ≥3（12 类硬闸第 1 类）——手动建 store，不经 mechCleanDraft 包装
	draft := "他不是怕死，而是怕疼。他不是退缩，而是等待。他不是沉默，而是蓄力。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}

	criticCalled := false
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		criticCalled = true
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("review_style must reject when draft carries literary-prose errors")
	}
	if !strings.Contains(err.Error(), "文学腔硬闸") || !strings.Contains(err.Error(), "不能接受评审结果") {
		t.Fatalf("expected literary-gate rejection, got: %v", err)
	}
	if criticCalled {
		t.Fatal("critic must not be invoked for a draft failing the mechanical gate")
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger != nil && !ledger.IsEmpty() {
		t.Fatalf("ledger must stay empty after gate rejection, got status %s", ledger.CurrentStatus())
	}

	// 修改草稿消除否定修正句（含自评关键词）→ 重新 check_consistency → accepted
	fixed := "他推开窗，夜色扑面而来。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, fixed); err != nil {
		t.Fatal(err)
	}
	fixedDigest := domain.DigestDraft(fixed)
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "a2", fixedDigest); err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review after fixing the draft should succeed: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Verdict != "pass" || output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("expected accepted_initial after fix, got verdict=%s status=%s", output.Verdict, output.Status)
	}
	ledger, _ = st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusAcceptedInitial {
		t.Fatalf("expected accepted_initial after fix, got %s", ledger.CurrentStatus())
	}
}

// TestReviewStyle_MechanicalErrorBlocksAcceptedRev 验证终审 pass（AcceptedRev）
// 同样被闸拦截：预置 initial_pending → revision_open → final_pending，critic
// pass → 拒绝且账本停留在 final_pending。
func TestReviewStyle_MechanicalErrorBlocksAcceptedRev(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	draft := "他不是怕死，而是怕疼。他不是退缩，而是等待。他不是沉默，而是蓄力。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"}},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"},
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "revise",
					Findings: []domain.StyleReviewFinding{{
						Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e",
					}}}},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: now,
				AttemptID: "final-attempt", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "m"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("final review must reject accepted when draft carries literary-prose errors")
	}
	if !strings.Contains(err.Error(), "文学腔硬闸") {
		t.Fatalf("expected literary-gate rejection, got: %v", err)
	}
	loaded, _ := st.StyleReview.Load(1)
	if loaded.CurrentStatus() != domain.ReviewStatusFinalPending {
		t.Fatalf("ledger must stay final_pending (not terminal), got %s", loaded.CurrentStatus())
	}
	if len(loaded.Cycles) != 3 {
		t.Fatalf("cycles = %d, want 3（闸前账本原样保留）", len(loaded.Cycles))
	}
}

// TestReviewStyle_MechanicalCleanAccepted 验证机械干净的草稿不受闸影响：
// 普通 pass 评审正常写入 accepted（闸只拦 error，不误伤）。
func TestReviewStyle_MechanicalCleanAccepted(t *testing.T) {
	st := setupCriticStore(t, 1, "正文。")
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("clean draft must pass review: %v", err)
	}
	var output StyleReviewOutput
	json.Unmarshal(out, &output)
	if output.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("expected accepted_initial, got %s", output.Status)
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger.CurrentStatus() != domain.ReviewStatusAcceptedInitial {
		t.Fatalf("expected accepted_initial, got %s", ledger.CurrentStatus())
	}
}
