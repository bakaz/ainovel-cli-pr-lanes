package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestPolishDraft_LengthRecoveryExhaustedFailClosed：连续三轮 length 截断 →
// 只调用 3 次、返回 MaxTurnsError（fail-closed）、不保存草稿、不写 polish checkpoint。
// MaxTurns=3 比 agentcore 内部 recovery 预算（defaultMaxLengthRecoveries=3）更保守：
// 第 4 次调用前即报错，不会走到 StopGuard 放行截断结果。
func TestPolishDraft_LengthRecoveryExhaustedFailClosed(t *testing.T) {
	draft := strings.Repeat("这是原始草稿。", 30)

	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherLengthText(strings.Repeat("又一段被截断的输出。", 5))}, nil
	}, 3, testRecoveryPrompt)
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error after consecutive length truncations")
	}
	if !errors.Is(err, agentcore.ErrMaxTurns) {
		t.Errorf("err = %v, want agentcore.ErrMaxTurns", err)
	}
	if calls != 3 {
		t.Errorf("polisher calls = %d, want 3 (1 initial + 2 recoveries, then fail-closed)", calls)
	}
	// fail-closed：草稿保持原样，不落 polish checkpoint
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Errorf("draft must remain unchanged after failure")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint should exist after MaxTurns failure")
	}
}

// TestPolishDraft_MaxTurnsErrorNoOuterRetry：复现生产旧配置（MaxTurns=1）下
// length 截断 → MaxTurnsError 的传播路径。runner 错误必须立即返回，不进入空输出
// 重试循环（调用次数不得因 polisherEmptyRetryMax=4 而翻倍）。
func TestPolishDraft_MaxTurnsErrorNoOuterRetry(t *testing.T) {
	oldMax, oldBase := polisherEmptyRetryMax, polisherEmptyRetryBase
	polisherEmptyRetryMax, polisherEmptyRetryBase = 4, time.Millisecond
	defer func() {
		polisherEmptyRetryMax, polisherEmptyRetryBase = oldMax, oldBase
	}()

	st := setupPolishStore(t, 1, strings.Repeat("这是原始草稿。", 30))
	calls := 0
	// MaxTurns=1、无专用 recovery prompt：与生产故障（55 章 rewrite 卡死）同构
	polisher := newMockPolisherCfg(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherLengthText("被截断的正文")}, nil
	}, 1, "")
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected MaxTurns error")
	}
	if !errors.Is(err, agentcore.ErrMaxTurns) {
		t.Errorf("err = %v, want agentcore.ErrMaxTurns", err)
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

// snippet 截断长字符串用于错误信息（测试辅助）。
func snippet(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}
