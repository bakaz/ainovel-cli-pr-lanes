package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── Mock polisher model ──────────────────────────────────────────────

type mockPolisherModel struct {
	fn  func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error)
	idx int
	// maxTokens 记录最近一次调用经 ResolveCallConfig 解析后的 MaxTokens，
	// 用于验证 subagent Config.MaxTokens → WithMaxTokens 透传链路。
	maxTokens int
}

func (m *mockPolisherModel) record(opts []agentcore.CallOption) {
	cfg := agentcore.ResolveCallConfig(opts)
	m.maxTokens = cfg.MaxTokens
}

func (m *mockPolisherModel) take(msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
	i := m.idx
	m.idx++
	return m.fn(i, msgs)
}

func (m *mockPolisherModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	m.record(opts)
	return m.take(msgs)
}

func (m *mockPolisherModel) GenerateStream(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	m.record(opts)
	resp, err := m.take(msgs)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *mockPolisherModel) SupportsTools() bool { return true }

func (m *mockPolisherModel) ModelName() string { return "mock-polisher-model" }

func newMockPolisher(fn func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error)) *subagent.Runner {
	return newMockPolisherCfg(fn, 15, "")
}

// newMockPolisherCfg 与 newMockPolisher 同构，但允许指定 MaxTurns 与
// LengthRecoveryPrompt（length 截断 recovery 场景的回归测试用）。
func newMockPolisherCfg(fn func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error), maxTurns int, recoveryPrompt string) *subagent.Runner {
	return newMockPolisherOpts(fn, maxTurns, recoveryPrompt, 0)
}

// newMockPolisherOpts 进一步允许指定 MaxTokens（max_tokens 透传场景的回归测试用）。
func newMockPolisherOpts(fn func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error), maxTurns int, recoveryPrompt string, maxTokens int) *subagent.Runner {
	model := &mockPolisherModel{fn: fn}
	cfg := subagent.Config{
		Name:                 "polisher",
		Description:          "mock polisher",
		Model:                model,
		MaxTurns:             maxTurns,
		LengthRecoveryPrompt: recoveryPrompt,
		MaxTokens:            maxTokens,
	}
	return subagent.NewRunner(cfg)
}

// testRecoveryPrompt 与 internal/agents/build.go 中 polisher 配置的
// LengthRecoveryPrompt 同义（整章重输出，而非默认的"从截断处续写"）。
const testRecoveryPrompt = "上一次输出被截断。不要从中间续写；请从章节标题开始，重新输出完整的精修后章节。只输出完整正文。"

func polisherText(text string) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(text)},
		StopReason: agentcore.StopReasonStop,
	}
}

// polisherLengthText 模拟 max_tokens 截断：StopReason=length（含部分正文）。
func polisherLengthText(text string) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(text)},
		StopReason: agentcore.StopReasonLength,
	}
}

// ── Test helpers ─────────────────────────────────────────────────────

const testPolisherVersion = "test-polisher-v1"

func setupPolishStore(t *testing.T, chapter int, draft string) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Drafts.SaveDraft(chapter, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	return st
}

func newEnabledPolishTool(st *store.Store, runner *subagent.Runner) *PolishDraftTool {
	tool := NewPolishDraftTool(st, runner, testPolisherVersion)
	tool.SetEnabled(true)
	return tool
}

func polishCheckpointOf(t *testing.T, st *store.Store, chapter int) *domain.Checkpoint {
	t.Helper()
	cp := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "polish")
	if cp == nil {
		t.Fatal("expected polish checkpoint")
	}
	return cp
}

// ── 1. Happy path ────────────────────────────────────────────────────

func TestPolishDraft_Success(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText("她倚窗而立。短句更有力，节奏明快。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished || !output.Changed {
		t.Fatalf("expected polished+changed output, got %+v", output)
	}
	if output.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("input_digest = %s, want %s", output.InputDigest, domain.DigestDraft(draft))
	}

	// 草稿已保存为精修后文本
	saved, _, err := st.Drafts.LoadChapterContent(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved != "她倚窗而立。短句更有力，节奏明快。" {
		t.Fatalf("draft not saved: %q", saved)
	}
	wantDigest := domain.DigestDraft("她倚窗而立。短句更有力，节奏明快。")
	if output.OutputDigest != wantDigest {
		t.Errorf("output_digest = %s, want %s", output.OutputDigest, wantDigest)
	}

	// polish checkpoint 含 input_digest/output_digest/polisher_model/stage/changed
	cp := polishCheckpointOf(t, st, 1)
	if cp.Digest != wantDigest {
		t.Errorf("checkpoint digest = %s, want %s", cp.Digest, wantDigest)
	}
	if cp.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint input_digest = %s, want %s", cp.InputDigest, domain.DigestDraft(draft))
	}
	if cp.PolisherModel != "mock-polisher-model" {
		t.Errorf("checkpoint polisher_model = %q, want mock-polisher-model", cp.PolisherModel)
	}
	if cp.Stage != "draft" {
		t.Errorf("checkpoint stage = %q, want draft", cp.Stage)
	}
	if !cp.Changed {
		t.Error("checkpoint changed should be true")
	}
	if output.PolisherModel != "mock-polisher-model" {
		t.Errorf("output polisher_model = %q", output.PolisherModel)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

// ── 2. No-op：输出与输入相同 → changed=false 仍成功 ──

func TestPolishDraft_NoOp(t *testing.T) {
	const draft = "这段文字已经很好，无需修改。她心里骂自己丢人，真不要脸。"
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(draft)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatal("expected polished=true for no-op")
	}
	if output.Changed {
		t.Fatal("no-op must report changed=false")
	}
	if output.InputDigest != output.OutputDigest {
		t.Fatalf("no-op digests must match: %s vs %s", output.InputDigest, output.OutputDigest)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Changed {
		t.Error("no-op checkpoint must have changed=false")
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("no-op checkpoint digest mismatch")
	}
}

// ── 3. 空输出重试：首次空 → 自动重试成功 ──

func TestPolishDraft_EmptyRetry(t *testing.T) {
	oldMax, oldBase := polisherEmptyRetryMax, polisherEmptyRetryBase
	polisherEmptyRetryMax, polisherEmptyRetryBase = 4, time.Millisecond
	defer func() {
		polisherEmptyRetryMax, polisherEmptyRetryBase = oldMax, oldBase
	}()

	const draft = "需要精修的草稿。"
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		if i == 0 {
			return &agentcore.LLMResponse{Message: polisherText("   ")}, nil
		}
		return &agentcore.LLMResponse{Message: polisherText("精修后的正文。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatalf("expected success after retry, got %+v", output)
	}
	if calls != 2 {
		t.Errorf("polisher calls = %d, want 2 (empty retry)", calls)
	}
}

// ── 4. 空输出重试耗尽 → 失败 ──

func TestPolishDraft_EmptyExhausted(t *testing.T) {
	oldMax, oldBase := polisherEmptyRetryMax, polisherEmptyRetryBase
	polisherEmptyRetryMax, polisherEmptyRetryBase = 2, time.Millisecond
	defer func() {
		polisherEmptyRetryMax, polisherEmptyRetryBase = oldMax, oldBase
	}()

	st := setupPolishStore(t, 1, "草稿。")
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error after empty retries exhausted")
	}
	if !strings.Contains(err.Error(), "空输出") {
		t.Errorf("expected empty-output error, got: %v", err)
	}
	// 校验失败不应落盘 checkpoint
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint should exist after failure")
	}
}

// ── 5. 校验失败：非法 UTF-8 ──

func TestPolishDraft_InvalidUTF8(t *testing.T) {
	st := setupPolishStore(t, 1, "原草稿。")
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(string([]byte{0xff, 0xfe, 0x00, 0x01}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for invalid UTF-8 output")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("expected UTF-8 error, got: %v", err)
	}
	// 草稿与 checkpoint 均不应被污染
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "原草稿。" {
		t.Errorf("draft must remain unchanged, got %q", saved)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint should exist after validation failure")
	}
}

// ── 6. 校验失败：超长输出 ──

func TestPolishDraft_TooLong(t *testing.T) {
	st := setupPolishStore(t, 1, "原草稿。")
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(strings.Repeat("长", maxPolishOutputRunes+1))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for overlong output")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("expected length-limit error, got: %v", err)
	}
}

// ── 6b. 校验失败：输出过短（"好的，已完成精修"式短文本，非整章正文） ──

func TestPolishDraft_TooShort(t *testing.T) {
	st := setupPolishStore(t, 1, strings.Repeat("长", 100))
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("好的，已完成精修。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for too-short output")
	}
	if !strings.Contains(err.Error(), "40%") {
		t.Errorf("expected min-length error, got: %v", err)
	}
	// 校验失败不应落盘 checkpoint
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint should exist after validation failure")
	}
}

// ── 6c. 校验失败：纯 JSON 输出（非正文） ──

func TestPolishDraft_JSONOutput(t *testing.T) {
	st := setupPolishStore(t, 1, strings.Repeat("长", 10))
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(`{"result":"已完成精修","changed":true,"summary":"调整了句式节奏"}`)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for pure-JSON output")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("expected JSON-rejection error, got: %v", err)
	}
	// 校验失败不应落盘 checkpoint
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint should exist after validation failure")
	}
}

// ── 6d. 校验失败：代码围栏整体包裹（非裸正文） ──

func TestPolishDraft_FencedOutput(t *testing.T) {
	st := setupPolishStore(t, 1, strings.Repeat("长", 10))
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("```\n这是精修后的完整正文，句式更紧凑。\n```")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for fenced output")
	}
	if !strings.Contains(err.Error(), "围栏") {
		t.Errorf("expected fence-rejection error, got: %v", err)
	}
	// 校验失败不应落盘 checkpoint
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint should exist after validation failure")
	}
}

// ── 7. pipeline 关闭 → skipped，不调用 polisher ──

func TestPolishDraft_DisabledSkipped(t *testing.T) {
	st := setupPolishStore(t, 1, "草稿。")
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText("x")}, nil
	})
	// 不调用 SetEnabled → 默认关闭
	tool := NewPolishDraftTool(st, polisher, testPolisherVersion)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Skipped {
		t.Fatalf("expected skipped output, got %+v", output)
	}
	if calls != 0 {
		t.Errorf("polisher must not be called when disabled, got %d calls", calls)
	}
}

// ── 8. 无草稿 → 前置条件错误 ──

func TestPolishDraft_NoDraft(t *testing.T) {
	st := setupPolishStore(t, 1, "")
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("x")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected precondition error for missing draft")
	}
}

// ── 9. terminal 评审状态 → fail-fast（不启动 polisher） ──

func TestPolishDraft_TerminalLedgerRejected(t *testing.T) {
	st := setupPolishStore(t, 1, "草稿。")
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	digest := domain.DigestDraft("草稿。")
	basisDigest := "sha256:" + strings.Repeat("b", 64)
	if err := st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "v1", Model: "m"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "v1", Model: "m"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText("x")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected mutation-guard rejection for terminal ledger")
	}
	if calls != 0 {
		t.Errorf("polisher must not run on terminal ledger, got %d calls", calls)
	}
}

// ── 10. rewrite 队列 → stage=rewrite ──

func TestPolishDraft_RewriteStage(t *testing.T) {
	st := setupPolishStore(t, 1, "已完成的终稿文本。")
	if err := st.Drafts.SaveFinalChapter(1, "已完成的终稿文本。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatal(err)
	}
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("打磨后的终稿文本，更流畅。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if output.Stage != "rewrite" {
		t.Errorf("stage = %q, want rewrite", output.Stage)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Stage != "rewrite" {
		t.Errorf("checkpoint stage = %q, want rewrite", cp.Stage)
	}
}

// ── 11. length 截断 recovery：半章 → 整章重输出（ora-1 核心回归） ──

// TestPolishDraft_LengthRecoveryFullChapter 覆盖生产 55 章 rewrite 卡死的修复路径：
// MaxTurns=3 + 专用整章重输出 recovery prompt 下，第一轮输出部分正文后被截断
// （StopReason=length），第二轮收到专用 recovery prompt（而非默认续写提示）并输出
// 完整章节 → 最终保存的是完整章，不是半章/尾段。
func TestPolishDraft_LengthRecoveryFullChapter(t *testing.T) {
	const draft = "她站在窗前，望着远方的灯火。这个句子写得拖沓冗长，读起来非常累。"
	half := strings.Repeat("这是被截断的半章正文，精修尚未完成，后面还有大量内容没有输出。", 30)
	full := strings.Repeat("这是精修后的完整章节正文，覆盖了从开头到结尾的全部内容，句式干净利落。", 40)

	st := setupPolishStore(t, 1, draft)
	calls := 0
	var secondCallLastUser string
	polisher := newMockPolisherCfg(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		if i == 0 {
			// 第一轮：只输出半章即被 max_tokens 截断
			return &agentcore.LLMResponse{Message: polisherLengthText(half)}, nil
		}
		// 第二轮：应收到专用 recovery prompt（要求整章重输出），然后输出完整章
		if len(msgs) > 0 {
			secondCallLastUser = msgs[len(msgs)-1].TextContent()
		}
		return &agentcore.LLMResponse{Message: polisherText(full)}, nil
	}, 3, testRecoveryPrompt)
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatalf("expected success after length recovery, got %+v", output)
	}
	if calls != 2 {
		t.Fatalf("polisher calls = %d, want 2 (length → recovery)", calls)
	}
	// 第二次调用注入的是专用 recovery prompt，不是 agentcore 默认续写提示
	if !strings.Contains(secondCallLastUser, "重新输出完整的精修后章节") {
		t.Errorf("2nd call recovery prompt = %q, want full-chapter re-output prompt", secondCallLastUser)
	}
	if strings.Contains(secondCallLastUser, "Resume directly") || strings.Contains(secondCallLastUser, "Pick up mid-thought") {
		t.Errorf("2nd call must NOT use agentcore default resume prompt, got %q", secondCallLastUser)
	}

	// 保存的必须是第二轮完整章，绝不能是半章/尾段（截头覆盖风险）
	saved, _, err := st.Drafts.LoadChapterContent(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved != full {
		t.Fatalf("saved draft = %q..., want full chapter (got half-chapter tail?); len saved=%d want=%d",
			snippet(saved), len(saved), len(full))
	}
	if output.OutputDigest != domain.DigestDraft(full) {
		t.Errorf("output_digest = %s, want digest of full chapter", output.OutputDigest)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Digest != domain.DigestDraft(full) {
		t.Errorf("checkpoint digest = %s, want digest of full chapter", cp.Digest)
	}
}

// TestPolishDraft_LengthRecoveryThinkingOnly：第一轮仅 thinking 无正文即被截断
// （mimo 实测 thinking 75-97K 字符场景），第二轮完整输出 → 草稿保存完整、checkpoint 写入。
func TestPolishDraft_LengthRecoveryThinkingOnly(t *testing.T) {
	const draft = "她站在窗前，望着远方的灯火。这个句子写得拖沓冗长，读起来非常累。"
	full := strings.Repeat("这是精修后的完整章节正文，覆盖了从开头到结尾的全部内容。", 20)

	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		if i == 0 {
			// 仅 thinking 块 + length 截断，无任何正文输出
			return &agentcore.LLMResponse{Message: agentcore.Message{
				Role:       agentcore.RoleAssistant,
				Content:    []agentcore.ContentBlock{agentcore.ThinkingBlock("思考如何精修这段文字……思考很长直到被截断")},
				StopReason: agentcore.StopReasonLength,
			}}, nil
		}
		return &agentcore.LLMResponse{Message: polisherText(full)}, nil
	}, 3, testRecoveryPrompt)
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatalf("expected success, got %+v", output)
	}
	if calls != 2 {
		t.Errorf("polisher calls = %d, want 2", calls)
	}
	saved, _, err := st.Drafts.LoadChapterContent(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved != full {
		t.Fatalf("saved draft = %q..., want full chapter", snippet(saved))
	}
	polishCheckpointOf(t, st, 1) // 存在即通过
}

// TestPolishDraft_LengthRecoveryExhaustedDegraded：连续三轮 length 截断 →
// 只调用 3 次、返回 MaxTurnsError → 降级为 degraded polish checkpoint
// （ErrorCategory=max_turns）：草稿保持原样（不保存半章），但写入绑定当前草稿
// digest 的 degraded 记录，FSM 可继续 post-polish check → review，不再死锁。
// MaxTurns=3 比 agentcore 内部 recovery 预算（defaultMaxLengthRecoveries=3）更保守：
// 第 4 次调用前即报错，不会走到 StopGuard 放行截断结果。
func TestPolishDraft_LengthRecoveryExhaustedDegraded(t *testing.T) {
	draft := strings.Repeat("这是原始草稿。", 30)

	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherLengthText(strings.Repeat("又一段被截断的输出。", 5))}, nil
	}, 3, testRecoveryPrompt)
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded {
		t.Fatalf("expected degraded output after MaxTurns exhaustion, got %+v", output)
	}
	if output.ErrorCategory != "max_turns" {
		t.Errorf("error_category = %q, want max_turns", output.ErrorCategory)
	}
	if output.Changed {
		t.Error("degraded must report changed=false (正文未变)")
	}
	if output.OutputDigest != domain.DigestDraft(draft) {
		t.Errorf("output_digest = %s, want digest of original draft", output.OutputDigest)
	}
	if calls != 3 {
		t.Errorf("polisher calls = %d, want 3 (1 initial + 2 recoveries, then MaxTurns)", calls)
	}
	// 草稿保持原样（不保存半章/尾段），但 degraded polish checkpoint 已落盘
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Errorf("draft must remain unchanged after failure")
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded {
		t.Fatal("expected degraded polish checkpoint after MaxTurns failure")
	}
	if cp.ErrorCategory != "max_turns" {
		t.Errorf("checkpoint error_category = %q, want max_turns", cp.ErrorCategory)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint digest = %s, want current draft digest", cp.Digest)
	}
	if cp.Changed {
		t.Error("degraded checkpoint must have changed=false")
	}
}

// TestPolishDraft_LengthTwiceThenComplete：连续两次 length 截断（两轮半章输出）
// 后在第三次调用完整输出（StopReason=stop）→ 最终保存的是完整章，不是半章、
// 不是两段拼接。MaxTurns=3 恰好覆盖 1 次初始 + 2 次 recovery，是
// length-recovery 精确变体（1×length→complete 与 3×length→fail-closed 之间的边界）。
func TestPolishDraft_LengthTwiceThenComplete(t *testing.T) {
	const draft = "这是第三章的原始草稿，句子冗长拖沓，需要精修。"
	half := strings.Repeat("这是第一次被截断的半章正文，精修尚未完成，后面还有大量内容没有输出。", 30)
	full := strings.Repeat("这是第三次输出的完整精修章节正文，覆盖了从开头到结尾的全部内容，句式干净利落。", 40)

	st := setupPolishStore(t, 3, draft)
	calls := 0
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		switch i {
		case 0, 1:
			// 前两轮：均被 max_tokens 截断，只输出半章
			return &agentcore.LLMResponse{Message: polisherLengthText(half)}, nil
		default:
			// 第三轮：完整输出整章（StopReason=stop）
			return &agentcore.LLMResponse{Message: polisherText(full)}, nil
		}
	}, 3, testRecoveryPrompt)
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatalf("expected success after two length recoveries, got %+v", output)
	}
	if calls != 3 {
		t.Fatalf("polisher calls = %d, want 3 (length → length → complete)", calls)
	}

	// 保存的必须是第三轮完整章：不是第一/二轮半章，也不是两段拼接
	saved, _, err := st.Drafts.LoadChapterContent(3)
	if err != nil {
		t.Fatal(err)
	}
	if saved != full {
		t.Fatalf("saved draft = %q..., want full chapter (not half/concatenated); len saved=%d want=%d",
			snippet(saved), len(saved), len(full))
	}
	if output.OutputDigest != domain.DigestDraft(full) {
		t.Errorf("output_digest = %s, want digest of full chapter", output.OutputDigest)
	}
	cp := polishCheckpointOf(t, st, 3)
	if cp.Digest != domain.DigestDraft(full) {
		t.Errorf("checkpoint digest = %s, want digest of full chapter", cp.Digest)
	}
}

// TestPolishDraft_MaxTurnsErrorNoOuterRetry：复现生产旧配置（MaxTurns=1）下
// length 截断 → MaxTurnsError 的传播路径。runner 错误必须立即返回，不进入空输出
// 重试循环（调用次数不得因 polisherEmptyRetryMax=4 而翻倍）；随后降级为 degraded
// polish checkpoint（ErrorCategory=max_turns）。
func TestPolishDraft_MaxTurnsErrorNoOuterRetry(t *testing.T) {
	oldMax, oldBase := polisherEmptyRetryMax, polisherEmptyRetryBase
	polisherEmptyRetryMax, polisherEmptyRetryBase = 4, time.Millisecond
	defer func() {
		polisherEmptyRetryMax, polisherEmptyRetryBase = oldMax, oldBase
	}()

	draft := strings.Repeat("这是原始草稿。", 30)
	st := setupPolishStore(t, 1, draft)
	calls := 0
	// MaxTurns=1、无专用 recovery prompt：与生产故障（55 章 rewrite 卡死）同构
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherLengthText("被截断的正文")}, nil
	}, 1, "")
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded || output.ErrorCategory != "max_turns" {
		t.Fatalf("expected degraded(max_turns) output, got %+v", output)
	}
	// 关键断言：max_turns 错误不触发空输出重试，调用次数不乘以 polisherEmptyRetryMax
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1 (runner error must return immediately, not enter empty-output retry)", calls)
	}
}

// TestPolishDraft_MaxTurns3NormalSingleCall：MaxTurns=3 下普通成功路径仍只需
// 1 次模型调用（不因 recovery 配额放宽而改变正常路径行为）。
func TestPolishDraft_MaxTurns3NormalSingleCall(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText("她倚窗而立。短句更有力，节奏明快。")}, nil
	}, 3, testRecoveryPrompt)
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatalf("expected success, got %+v", output)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1 on normal path", calls)
	}
}

// TestPolishDraft_MaxTokensPassthrough：polisher runner 配置 MaxTokens>0 时，
// 每次 LLM 调用必须携带 WithMaxTokens(131072)（经 LoopConfig → callLLM 透传），
// 覆盖"默认 65536 截断 thinking"放大器的修复链路。
func TestPolishDraft_MaxTokensPassthrough(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	model := &mockPolisherModel{fn: func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("她倚窗而立。短句更有力，节奏明快。")}, nil
	}}
	polisher := subagent.NewRunner(subagent.Config{
		Name:        "polisher",
		Description: "mock polisher",
		Model:       model,
		MaxTurns:    3,
		MaxTokens:   131072,
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Polished {
		t.Fatalf("expected success, got %+v", output)
	}
	// 模型层实测：本次调用解析出的 per-call MaxTokens 必须是 131072
	//（WithMaxTokens 已由 callLLM 追加进 CallOptions）
	if got := model.maxTokens; got != 131072 {
		t.Errorf("resolved per-call MaxTokens = %d, want 131072 (WithMaxTokens passthrough)", got)
	}
}

// ── 12. degraded polish checkpoint（Oracle 方案第 3 步） ────────────────
//
// polisher 经有限重试仍失败（可恢复类错误）→ 不原样失败，而是写入绑定当前草稿
// digest 的 degraded polish checkpoint（正文未变），FSM 据此推进 post-polish check
// → review → commit，消除"polish 失败 → 永远 needs_polish → 无脑重派同一章"
// 的生产死锁（ch71 类）。

// TestPolishDraft_DegradedOnStreamIdle：mock polisher 连续返回 stream idle 超时
// → 写 degraded checkpoint（Digest=当前草稿、Degraded=true、ErrorCategory=stream_idle）
// → 返回成功摘要含 Degraded=true。
func TestPolishDraft_DegradedOnStreamIdle(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return nil, fmt.Errorf("provider stream idle timeout: %w", agentcore.ErrProviderStreamIdle)
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded {
		t.Fatalf("expected degraded output, got %+v", output)
	}
	if output.ErrorCategory != "stream_idle" {
		t.Errorf("error_category = %q, want stream_idle", output.ErrorCategory)
	}
	if !output.Polished {
		t.Error("degraded output must still report polished=true (工具完成留痕，调用方继续推进)")
	}
	if output.Changed {
		t.Error("degraded must report changed=false (正文未变)")
	}
	if output.InputDigest != domain.DigestDraft(draft) || output.OutputDigest != domain.DigestDraft(draft) {
		t.Errorf("degraded digests must both equal current draft digest, got in=%s out=%s",
			output.InputDigest, output.OutputDigest)
	}
	if output.Stage != "draft" {
		t.Errorf("stage = %q, want draft", output.Stage)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1 (runner error returns immediately)", calls)
	}

	// degraded checkpoint：Digest=当前草稿、Degraded=true、ErrorCategory=stream_idle
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded {
		t.Fatal("expected degraded polish checkpoint")
	}
	if cp.ErrorCategory != "stream_idle" {
		t.Errorf("checkpoint error_category = %q, want stream_idle", cp.ErrorCategory)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint digest = %s, want current draft digest", cp.Digest)
	}
	if cp.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint input_digest = %s, want current draft digest", cp.InputDigest)
	}
	if cp.Changed {
		t.Error("degraded checkpoint must have changed=false")
	}
	// 草稿保持原样（degraded 不保存任何新正文）
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Errorf("draft must remain unchanged after degraded, got %q", snippet(saved))
	}
}

// TestPolishDraft_DegradedOnProviderTimeout：provider timeout（DeadlineExceeded
// 分类）→ ErrorCategory=timeout。
func TestPolishDraft_DegradedOnProviderTimeout(t *testing.T) {
	const draft = "需要精修的草稿。她心里骂自己丢人，真不要脸。"
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, fmt.Errorf("provider deadline exceeded: %w", context.DeadlineExceeded)
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded || output.ErrorCategory != "timeout" {
		t.Fatalf("expected degraded(timeout) output, got %+v", output)
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "timeout" {
		t.Fatalf("expected degraded(timeout) checkpoint, got degraded=%v category=%q", cp.Degraded, cp.ErrorCategory)
	}
}

// TestPolishDraft_DegradedOnNetworkError：network 类错误（IsFailoverEligible）→
// ErrorCategory=network。
func TestPolishDraft_DegradedOnNetworkError(t *testing.T) {
	const draft = "需要精修的草稿。她心里骂自己丢人，真不要脸。"
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, fmt.Errorf("dial tcp: connection refused: %w", agentcore.ErrProviderNetwork)
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded || output.ErrorCategory != "network" {
		t.Fatalf("expected degraded(network) output, got %+v", output)
	}
}

// TestPolishDraft_DegradedRewriteStage：重写队列章节降级 → stage=rewrite。
func TestPolishDraft_DegradedRewriteStage(t *testing.T) {
	const draft = "已完成的终稿文本。"
	st := setupPolishStore(t, 1, draft)
	if err := st.Drafts.SaveFinalChapter(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatal(err)
	}
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, agentcore.ErrProviderStreamIdle
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded || output.Stage != "rewrite" {
		t.Fatalf("expected degraded output with stage=rewrite, got %+v", output)
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.Stage != "rewrite" {
		t.Fatalf("expected degraded rewrite checkpoint, got degraded=%v stage=%q", cp.Degraded, cp.Stage)
	}
}

// TestPolishDraft_NoDegradeOnCanceled：context.Canceled（用户取消/Host 关闭）→
// 不可降级：原样返回错误，不写 degraded checkpoint。
func TestPolishDraft_NoDegradeOnCanceled(t *testing.T) {
	const draft = "需要精修的草稿。她心里骂自己丢人，真不要脸。"
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return nil, context.Canceled
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for context.Canceled (不可降级)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in chain", err)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no degraded checkpoint should exist after context.Canceled")
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

// TestPolishDraft_NoDegradeOnContentFilter：content filter / 安全拒绝 → 不可降级：
// 原样返回错误，不写 degraded checkpoint（安全拒绝不能静默绕过）。
func TestPolishDraft_NoDegradeOnContentFilter(t *testing.T) {
	const draft = "需要精修的草稿。她心里骂自己丢人，真不要脸。"
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return nil, agentcore.ErrProviderContentFilter
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for content filter (不可降级)")
	}
	if !errors.Is(err, agentcore.ErrProviderContentFilter) {
		t.Errorf("err = %v, want ErrProviderContentFilter in chain", err)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no degraded checkpoint should exist after content filter rejection")
	}
}

// TestPolishDraft_DegradedOnlyOnce：同一 digest 已有 degraded 记录后再次 polish
// 失败 → 不重复降级（原样返回错误），账本只有一条 degraded 记录。防
// "降级 → 重派 → 再降级"的自循环（正常流程中 FSM 在 degraded 后已不再允许
// polish_draft，本守卫是纵深防御）。
func TestPolishDraft_DegradedOnlyOnce(t *testing.T) {
	const draft = "需要精修的草稿。她心里骂自己丢人，真不要脸。"
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return nil, agentcore.ErrProviderStreamIdle
	})
	tool := newEnabledPolishTool(st, polisher)

	// 第一次：降级成功
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !output.Degraded {
		t.Fatalf("first call must degrade, got %+v", output)
	}

	// 第二次（同一 digest 再次失败）：不重复降级，原样返回错误
	_, err = tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("second failure on same digest must NOT degrade again (must return error)")
	}
	if !errors.Is(err, agentcore.ErrProviderStreamIdle) {
		t.Errorf("second err = %v, want ErrProviderStreamIdle in chain", err)
	}
	if calls != 2 {
		t.Errorf("polisher calls = %d, want 2 (one per Execute)", calls)
	}
	// 账本中只有一条 polish 记录，且为 degraded
	all := st.Checkpoints.All()
	var polishCount int
	for _, cp := range all {
		if cp.Scope.Matches(domain.ChapterScope(1)) && cp.Step == "polish" {
			polishCount++
			if !cp.Degraded {
				t.Error("the single polish checkpoint must be degraded")
			}
		}
	}
	if polishCount != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1 (no repeated degradation)", polishCount)
	}
}

// snippet 截断长字符串用于错误信息（测试辅助）。
func snippet(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}

// ── edit list 路径（ora-1 形态 2）──────────────────────────────────────

// editListJSON 构造 polisher 协议的 edit 列表 JSON（测试辅助）。
func editListJSON(items ...[2]string) string {
	var sb strings.Builder
	sb.WriteString(`{"version":1,"edits":[`)
	for i, it := range items {
		if i > 0 {
			sb.WriteString(",")
		}
		o, _ := json.Marshal(it[0])
		n, _ := json.Marshal(it[1])
		sb.WriteString(`{"old_string":` + string(o) + `,"new_string":` + string(n) + `}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

func polishCheckpointCount(t *testing.T, st *store.Store, chapter int) int {
	t.Helper()
	n := 0
	for _, cp := range st.Checkpoints.All() {
		if cp.Scope.Matches(domain.ChapterScope(chapter)) && cp.Step == "polish" {
			n++
		}
	}
	return n
}

// TestPolishDraft_EditListSuccess：单 edit 成功路径——草稿落盘为应用后文本、
// 恰好一个 polish checkpoint（Method=edit_list、EditCount=1、InputDigest/Digest/
// Stage/Changed 正确）。
func TestPolishDraft_EditListSuccess(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st) // 机械门禁激活：输入 clean，候选也必须 clean
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"她站在窗前", "她倚窗而立"}))}, nil
	})
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
		t.Fatalf("expected polished+changed output, got %+v", output)
	}
	if output.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("input_digest mismatch")
	}

	want := "她倚窗而立，望着远处的灯火。她心里骂自己丢人，真不要脸。"
	saved, _, err := st.Drafts.LoadChapterContent(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}

	cp := polishCheckpointOf(t, st, 1)
	if cp.Method != "edit_list" {
		t.Errorf("checkpoint method = %q, want edit_list", cp.Method)
	}
	if cp.EditCount != 1 {
		t.Errorf("checkpoint edit_count = %d, want 1", cp.EditCount)
	}
	if cp.InputDigest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint input_digest = %s, want %s", cp.InputDigest, domain.DigestDraft(draft))
	}
	if cp.Digest != domain.DigestDraft(want) {
		t.Errorf("checkpoint digest = %s, want digest of applied text", cp.Digest)
	}
	if !cp.Changed || cp.Stage != "draft" || cp.PolisherModel != "mock-polisher-model" {
		t.Errorf("checkpoint meta wrong: %+v", cp)
	}
	if output.OutputDigest != domain.DigestDraft(want) {
		t.Errorf("output_digest mismatch")
	}
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1 per call", n)
	}
}

// TestPolishDraft_EditListMultipleEditsReverseOrder：多 edit 全部基于同一输入
// 快照定位、按 offset 应用，最终草稿为逐条替换后的完整文本。
func TestPolishDraft_EditListMultipleEditsReverseOrder(t *testing.T) {
	draft := mechCleanDraft("她站在窗前。他坐在桌边。猫趴在角落。不远处传来犬吠。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"猫趴在角落。", "猫蜷在窗台。"},
			[2]string{"她站在窗前。", "她倚窗而立。"},
			[2]string{"他坐在桌边。", "他立在门前。"},
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "她倚窗而立。他立在门前。猫蜷在窗台。不远处传来犬吠。她心里骂自己丢人，真不要脸。"
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}
	if cp := polishCheckpointOf(t, st, 1); cp.EditCount != 3 {
		t.Errorf("edit_count = %d, want 3", cp.EditCount)
	}
}

// TestPolishDraft_EditListEmptyNoOp：edits=[] 合法 no-op——changed=false、
// 仍 AppendPolish 推进 seq（连续两次调用产生两个递增 seq 的 checkpoint）。
func TestPolishDraft_EditListEmptyNoOp(t *testing.T) {
	draft := mechCleanDraft("这段文字已经很好，无需修改。")
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(`{"version":1,"edits":[]}`)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute #1: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || output.Changed {
		t.Fatalf("no-op must report polished=true changed=false, got %+v", output)
	}
	if output.InputDigest != output.OutputDigest {
		t.Fatalf("no-op digests must match: %s vs %s", output.InputDigest, output.OutputDigest)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Errorf("draft must remain unchanged on no-op, got %q", saved)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Changed || cp.EditCount != 0 || cp.Method != "edit_list" {
		t.Errorf("no-op checkpoint meta wrong: changed=%v edit_count=%d method=%q", cp.Changed, cp.EditCount, cp.Method)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("no-op checkpoint digest must equal current draft")
	}

	// 第二次 no-op：AppendPolish 不做 digest 去重，seq 必须递增。
	cp1 := cp.Seq
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("Execute #2: %v", err)
	}
	cp2 := polishCheckpointOf(t, st, 1)
	if cp2.Seq <= cp1 {
		t.Errorf("no-op polish must advance seq: cp2=%d cp1=%d", cp2.Seq, cp1)
	}
	if n := polishCheckpointCount(t, st, 1); n != 2 {
		t.Errorf("polish checkpoints = %d, want 2", n)
	}
}

// TestPolishDraft_EditListFallbackFullText：非 JSON 输出 → 回退整章模式
// （旧协议全保留），checkpoint Method=full_text。
func TestPolishDraft_EditListFallbackFullText(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("她倚窗而立。短句更有力，节奏明快。")}, nil
	})
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
		t.Fatalf("expected full-text polish success, got %+v", output)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "她倚窗而立。短句更有力，节奏明快。" {
		t.Fatalf("saved draft = %q", saved)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Method != "full_text" {
		t.Errorf("checkpoint method = %q, want full_text", cp.Method)
	}
	if cp.EditCount != 0 {
		t.Errorf("full_text checkpoint edit_count = %d, want 0", cp.EditCount)
	}
}

// TestPolishDraft_EditListFencedFallsBackRejected：围栏包裹的 edit plan JSON
// → 回退整章模式 → 整章模式的围栏检查拒绝（错误含"围栏"），草稿不变。
func TestPolishDraft_EditListFencedFallsBackRejected(t *testing.T) {
	st := setupPolishStore(t, 1, strings.Repeat("长", 10))
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("```json\n{\"version\":1,\"edits\":[{\"old_string\":\"长\",\"new_string\":\"短\"}]}\n```")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected fence rejection")
	}
	if !strings.Contains(err.Error(), "围栏") {
		t.Errorf("expected fence-rejection error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != strings.Repeat("长", 10) {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after rejection")
	}
}

// TestPolishDraft_EditListContractVersionRejected：version 不受支持 → fail-closed
// 契约错误（不回退、不落盘）。
func TestPolishDraft_EditListContractVersionRejected(t *testing.T) {
	st := setupPolishStore(t, 1, "草稿。她心里骂自己丢人，真不要脸。")
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(`{"version":2,"edits":[]}`)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected contract error for version=2")
	}
	if !strings.Contains(err.Error(), "契约") || !strings.Contains(err.Error(), "version") {
		t.Errorf("expected contract error message, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "草稿。她心里骂自己丢人，真不要脸。" {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after contract error")
	}
}

// ── edit 路径失败：fail-closed（草稿原样、无 checkpoint） ───────────────

func TestPolishDraft_EditListAnchorMissing(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处。")
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"不存在的片段", "x"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for missing anchor")
	}
	if !strings.Contains(err.Error(), "内容校验") || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("expected content-validation error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after anchor failure")
	}
}

func TestPolishDraft_EditListAnchorMultiple(t *testing.T) {
	draft := mechCleanDraft("重复的片段，重复的片段。")
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"重复的片段", "x"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "出现 2 次") {
		t.Fatalf("expected multiplicity error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
}

func TestPolishDraft_EditListOverlap(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处。")
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"她站在窗前", "a"},
			[2]string{"在窗前，望着", "b"},
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "重叠") {
		t.Fatalf("expected overlap error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
}

// TestPolishDraft_EditListInvalidNthAtomic：第 N 条无效 → 整批拒绝，
// 前 N-1 条合法 edit 也绝不落盘（原子性）。
func TestPolishDraft_EditListInvalidNthAtomic(t *testing.T) {
	draft := mechCleanDraft("她站在窗前。他坐在桌边。猫趴在角落。")
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"她站在窗前。", "她倚窗而立。"},
			[2]string{"他坐在桌边。", "他立在门前。"},
			[2]string{"不存在的片段", "x"},
			[2]string{"猫趴在角落。", "猫蜷在窗台。"},
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("expected error on invalid 3rd edit, got: %v", err)
	}
	// 前两条合法 edit 未落盘：草稿保持原样。
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Fatalf("draft must remain unchanged (atomic): got %q", saved)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after atomic failure")
	}
}

func TestPolishDraft_EditListTooManyEdits(t *testing.T) {
	draft := mechCleanDraft("草稿。")
	st := setupPolishStore(t, 1, draft)
	items := make([]string, 0, maxPolishEdits+1)
	for i := 0; i < maxPolishEdits+1; i++ {
		items = append(items, fmt.Sprintf(`{"old_string":"x%d","new_string":"y%d"}`, i, i))
	}
	body := `{"version":1,"edits":[` + strings.Join(items, ",") + `]}`
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(body)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("expected count-limit error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
}

func TestPolishDraft_EditListOutputTooLong(t *testing.T) {
	st := setupPolishStore(t, 1, "甲乙")
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"甲", strings.Repeat("长", maxPolishOutputRunes+1)}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("expected output-length error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "甲乙" {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after length failure")
	}
}

// ── 机械回归门禁与连续拒绝收敛（循环 B） ────────────────────────────────

// TestPolishDraft_EditListMechanicalRegressionRejected：输入 clean、edit 引入
// 禁词（"不知为何"是系统默认 forbidden phrase）→ 机械回归拒绝，fail-closed：
// 草稿不变、无 checkpoint、错误明确区分"机械回归"、不混入 Degraded=true。
func TestPolishDraft_EditListMechanicalRegressionRejected(t *testing.T) {
	draft := mechCleanDraft("她停住了，望着远处。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	// 前置断言：输入确实无 error 级机械违规（门禁激活的前提）。
	if hasErrorViolations(computeMechanicalViolations(st, draft, utf8.RuneCountInString(draft))) {
		t.Fatal("test precondition: input draft must be mechanically clean")
	}
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"她停住了", "她不知为何停住了"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected mechanical-regression rejection")
	}
	if !strings.Contains(err.Error(), "机械回归") {
		t.Errorf("expected mechanical-regression error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after mechanical regression rejection")
	}
}

// TestPolishDraft_EditListMechanicalRegressionConverges：连续第 2 次机械回归拒绝
// → 写 rejected 性质 polish checkpoint（Degraded=true + ErrorCategory=
// mechanical_regression，Digest=当前草稿、Changed=false）→ 返回成功摘要（FSM
// 收敛到 post-check），草稿始终不变。
func TestPolishDraft_EditListMechanicalRegressionConverges(t *testing.T) {
	draft := mechCleanDraft("她停住了，望着远处。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	badEdit := editListJSON([2]string{"她停住了", "她不知为何停住了"})
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(badEdit)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	// 第 1 次：fail-closed。
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "机械回归") {
		t.Fatalf("first rejection must fail-closed, got: %v", err)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Fatal("no checkpoint after first rejection")
	}

	// 第 2 次：收敛为 rejected checkpoint。
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("second rejection must converge (no error), got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "mechanical_regression" {
		t.Fatalf("converged output must be degraded(mechanical_regression), got %+v", output)
	}
	if output.Changed || output.OutputDigest != domain.DigestDraft(draft) {
		t.Fatalf("converged output must report changed=false with current digest, got %+v", output)
	}
	if !output.Polished {
		t.Error("converged output must report polished=true (工具完成留痕，调用方继续 post-check)")
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged even after convergence")
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "mechanical_regression" {
		t.Errorf("rejected checkpoint must be degraded(mechanical_regression), got %+v", cp)
	}
	if cp.Digest != domain.DigestDraft(draft) || cp.Changed {
		t.Errorf("rejected checkpoint must bind current digest with changed=false, got %+v", cp)
	}
	if cp.Method != "edit_list" || cp.EditCount != 1 {
		t.Errorf("rejected checkpoint method/edit_count wrong: %s/%d", cp.Method, cp.EditCount)
	}

	// 第 3 次：收敛后计数清零，重新 fail-closed（防无限收敛）。
	_, err = tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "机械回归") {
		t.Fatalf("after convergence streak resets; 3rd rejection must fail-closed, got: %v", err)
	}
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1 (rejected)", n)
	}
}

// ── FSM 集成：edit 路径与章节状态机 ────────────────────────────────────

// TestPolishDraft_EditListFSMIntegration：check → polish(edit list) → 必须再次
// check（needs_post_polish_check，review 被拒）→ check → needs_review；之后普通
// edit 修改草稿 → check 后 polish 变 stale → needs_polish（postPolishEdit 语义
// 原样生效）。
func TestPolishDraft_EditListFSMIntegration(t *testing.T) {
	st := fsmEnabledStore(t, 1, "她站在窗前，望着远处的灯火。")
	cfg := fsmEnabledCfg()

	checkTool := NewCheckConsistencyTool(st)
	checkTool.SetChapterFSMConfig(cfg)

	// 1. 首次 check（draft_dirty）→ needs_polish。
	out1, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check #1: %v", err)
	}
	var res1 map[string]any
	if err := json.Unmarshal(out1, &res1); err != nil {
		t.Fatal(err)
	}
	act1, _ := res1["required_next_action"].(map[string]any)
	if act1 == nil || act1["action"] != ActionPolishDraft {
		t.Fatalf("required_next_action #1 = %v, want polish_draft", act1)
	}

	// 2. edit-list polish（真实 edit 路径）。
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"她站在窗前", "她倚窗而立"}))}, nil
	})
	polishTool := newEnabledPolishTool(st, polisher)
	polishTool.SetChapterFSMConfig(cfg)
	pOut, err := polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("polish: %v", err)
	}
	var pRes PolishDraftOutput
	if err := json.Unmarshal(pOut, &pRes); err != nil {
		t.Fatal(err)
	}
	if !pRes.Polished || !pRes.Changed {
		t.Fatalf("polish result = %+v, want polished+changed", pRes)
	}
	polishCP := polishCheckpointOf(t, st, 1)
	if polishCP.Method != "edit_list" {
		t.Errorf("checkpoint method = %q, want edit_list", polishCP.Method)
	}

	// 3. FSM：精修产生新候选（digest 变 → consistency stale）→ draft_dirty，
	//    review/commit 被拒（必须重新 check；consistency seq > polish seq 之后
	//    才允许 review）。
	decision, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Stage != ChapterStageDraftDirty {
		t.Fatalf("stage after changing polish = %s, want draft_dirty", decision.Stage)
	}
	if err := RequireChapterAction(st, 1, ChapterActionReview, cfg); err == nil ||
		!strings.Contains(err.Error(), "check_consistency") {
		t.Fatalf("review must be rejected before post-polish check, got: %v", err)
	}

	// 4. 重新 check（consistency seq > polish seq）→ needs_review（顺序
	//    polish → consistency → critic 成立）。
	out2, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check #2: %v", err)
	}
	var res2 map[string]any
	if err := json.Unmarshal(out2, &res2); err != nil {
		t.Fatal(err)
	}
	act2, _ := res2["required_next_action"].(map[string]any)
	if act2 == nil || act2["action"] != ActionReviewStyle {
		t.Fatalf("required_next_action #2 = %v, want review_style", act2)
	}

	// 5. 后续普通 edit（模拟 edit_chapter 修改草稿）→ check → polish stale →
	//    needs_polish（postPolishEdit 语义原样生效）。
	edited := "她倚窗而立，望着远处的灯火，心头一紧。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, edited); err != nil {
		t.Fatal(err)
	}
	if _, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("check #3 after edit: %v", err)
	}
	decision2, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if decision2.Stage != ChapterStageNeedsPolish || decision2.Required != ChapterActionPolish {
		t.Fatalf("stage after edit+check = %s (required %s), want needs_polish/polish_draft",
			decision2.Stage, decision2.Required)
	}
}

// TestPolishDraft_EditListNoOpFSMNeedsPostPolishCheck：no-op polish（digest 不变）
// 后 consistency 仍 fresh，但 consistency seq < polish seq → FSM 必须收敛到
// needs_post_polish_check（review 被拒，required=check_consistency）；重新 check
// 后 consistency seq > polish seq → needs_review。覆盖"edit 后需再次 check
// （consistency seq > polish seq）"的顺序绑定。
func TestPolishDraft_EditListNoOpFSMNeedsPostPolishCheck(t *testing.T) {
	st := fsmEnabledStore(t, 1, "她站在窗前，望着远处的灯火。")
	cfg := fsmEnabledCfg()

	checkTool := NewCheckConsistencyTool(st)
	checkTool.SetChapterFSMConfig(cfg)
	if _, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("check #1: %v", err)
	}

	// no-op edit-list polish（edits=[]，digest 不变）。
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(`{"version":1,"edits":[]}`)}, nil
	})
	polishTool := newEnabledPolishTool(st, polisher)
	polishTool.SetChapterFSMConfig(cfg)
	pOut, err := polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("polish: %v", err)
	}
	var pRes PolishDraftOutput
	if err := json.Unmarshal(pOut, &pRes); err != nil {
		t.Fatal(err)
	}
	if pRes.Changed {
		t.Fatal("no-op polish must report changed=false")
	}

	// 顺序绑定：consistency seq <= polish seq → needs_post_polish_check，review 被拒。
	decision, err := ResolveChapterStage(st, 1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Stage != ChapterStageNeedsPostPolishCheck {
		t.Fatalf("stage after no-op polish = %s, want needs_post_polish_check", decision.Stage)
	}
	if err := RequireChapterAction(st, 1, ChapterActionReview, cfg); err == nil ||
		!strings.Contains(err.Error(), "check_consistency") {
		t.Fatalf("review must be rejected while consistency seq <= polish seq, got: %v", err)
	}

	// 重新 check（新 seq）→ needs_review。
	out2, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check #2: %v", err)
	}
	var res2 map[string]any
	if err := json.Unmarshal(out2, &res2); err != nil {
		t.Fatal(err)
	}
	act2, _ := res2["required_next_action"].(map[string]any)
	if act2 == nil || act2["action"] != ActionReviewStyle {
		t.Fatalf("required_next_action #2 = %v, want review_style", act2)
	}
}

// ── 缓存优化阶段 2：Prompt Capsule 重排 ───────────────────────────────
//
// TestPolishDraft_TaskBasisBeforeDraft：断言 polisher task 文本的布局为
// "稳定书级内容（basis/findings/brief）在前、章节动态内容（章节号/字数/草稿
// 全文）最后、输出要求 footer 收尾"。跨 spawn 的内容前缀缓存（DeepSeek 磁盘
// 缓存按内容前缀匹配）依赖该顺序：basis 必须先于草稿全文出现。
func TestPolishDraft_TaskBasisBeforeDraft(t *testing.T) {
	const draft = "她站在窗前，望着远处的灯火。"
	st := setupPolishStore(t, 1, draft)

	// 追加 revision 账本（既有评审意见段）：验证 findings 也位于草稿之前。
	now := time.Now().Format(time.RFC3339)
	digest := domain.DigestDraft(draft)
	basisDigest := "sha256:" + strings.Repeat("b", 64)
	reviseResult := &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "ok",
		Findings: []domain.StyleReviewFinding{{Dimension: domain.FindingDimensionPacing,
			Category: domain.FindingCategoryStyle, Severity: domain.FindingSeverityWarning,
			Evidence: "第二段节奏偏慢"}}}
	if err := st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "v1", Model: "m"}},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "v1", Model: "m"},
				Result:  reviseResult},
		},
	}); err != nil {
		t.Fatal(err)
	}

	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText("x")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)
	task := tool.buildPolishTask(1, draft, utf8.RuneCountInString(draft))

	idx := func(marker string) int {
		i := strings.Index(task, marker)
		if i < 0 {
			t.Errorf("task 缺少段标记 %q\n任务文本:\n%s", marker, task)
		}
		return i
	}

	basisIdx := idx("### 精修依据")
	findingsIdx := idx("### 既有评审意见")
	draftIdx := idx("### 章节与草稿")
	footerIdx := idx("请严格按精修者提示词")

	// 稳定内容全部位于动态草稿段之前；草稿段位于 footer 之前。
	if !(basisIdx < findingsIdx && findingsIdx < draftIdx && draftIdx < footerIdx) {
		t.Errorf("task 段顺序错误：精修依据(%d) < 既有评审意见(%d) < 章节与草稿(%d) < footer(%d) 未成立\n任务文本:\n%s",
			basisIdx, findingsIdx, draftIdx, footerIdx, task)
	}

	// basis JSON（critic_version 是 basis 首个字段）必须出现在草稿全文之前。
	if strings.Index(task, `"critic_version"`) > strings.Index(task, draft) {
		t.Errorf("basis JSON（critic_version）必须出现在草稿全文之前\n任务文本:\n%s", task)
	}

	// 草稿全文只出现一次（动态段），且位于 basis JSON 之后。
	if strings.Count(task, draft) != 1 {
		t.Errorf("草稿全文应恰好出现一次（动态段），实际 %d 次", strings.Count(task, draft))
	}
}
