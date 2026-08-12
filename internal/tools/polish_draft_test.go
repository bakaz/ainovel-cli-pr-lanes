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
	// 输入为章节级长度（12 万 runes），输出超 maxPolishOutputRunes 但未超
	// 2 倍长度比例（P0-5）→ 命中 max 上限检查（"上限"错误信息）。
	st := setupPolishStore(t, 1, strings.Repeat("长", 120000))
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
	// 输入为章节级长度（与完整输出同量级），避免 P0-5 full-text 长度比例门禁
	// （输出 ≤ 输入 2 倍）误伤：完整章 ≈ 输入 1.1 倍，正常通过。
	draft := strings.Repeat("她站在窗前，望着远方的灯火。这个句子写得拖沓冗长，读起来非常累。", 40)
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
	// 输入为章节级长度（与完整输出同量级），避免 P0-5 full-text 长度比例门禁误伤。
	draft := strings.Repeat("她站在窗前，望着远方的灯火。这个句子写得拖沓冗长，读起来非常累。", 20)
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
	// 输入为章节级长度（与完整输出同量级），避免 P0-5 full-text 长度比例门禁误伤。
	draft := strings.Repeat("这是第三章的原始草稿，句子冗长拖沓，需要精修。", 50)
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
	if output.Degraded {
		t.Fatal("empty edits must be legal no-op, NOT degraded (④: 原始空列表非 degraded)")
	}
	if output.InputDigest != output.OutputDigest {
		t.Fatalf("no-op digests must match: %s vs %s", output.InputDigest, output.OutputDigest)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Errorf("draft must remain unchanged on no-op, got %q", saved)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.Changed || cp.EditCount != 0 || cp.Method != "edit_list" || cp.Degraded {
		t.Errorf("no-op checkpoint meta wrong: changed=%v edit_count=%d method=%q degraded=%v",
			cp.Changed, cp.EditCount, cp.Method, cp.Degraded)
	}
	if cp.ProposedEditCount != 0 || cp.DroppedEditCount != 0 || cp.Partial {
		t.Errorf("no-op checkpoint audit must be all zero, got %+v", cp)
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

// ── edit 路径失败收敛：④ 取代 P0-4（不再触发第二次模型纠错） ────────────
//
// 逐条局部验证 + 按优先级部分接受：单条无效只丢弃该条；原始非空但全部被丢弃
// （无安全 edit）→ 写 rejected/degraded polish checkpoint（ErrorCategory=
// edit_plan_invalid / coverage_exceeded，按全部 drop reasons 判定）→ FSM 收敛到
// post-check → 返回成功摘要（Degraded=true）。polisher 恰被调用 1 次（不再第 2
// 次模型纠错——④ 取代 P0-4 recoverEditPlanValidation），草稿始终不变。

func TestPolishDraft_EditListAnchorMissing(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处。")
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"不存在的片段", "x"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

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
	if output.DroppedEditCount != 1 || output.ProposedEditCount != 1 {
		t.Errorf("audit proposed/dropped = %d/%d, want 1/1", output.ProposedEditCount, output.DroppedEditCount)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "anchor_missing" {
		t.Errorf("drop_reasons = %v, want [anchor_missing]", output.DropReasons)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "edit_plan_invalid" || cp.Changed {
		t.Errorf("rejected checkpoint must be degraded(edit_plan_invalid) changed=false, got %+v", cp)
	}
	if cp.EditCount != 0 || cp.ProposedEditCount != 1 || cp.DroppedEditCount != 1 {
		t.Errorf("rejected checkpoint edit audit wrong: applied=%d proposed=%d dropped=%d",
			cp.EditCount, cp.ProposedEditCount, cp.DroppedEditCount)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint digest = %s, want current draft digest", cp.Digest)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1 (④: no second model correction call)", calls)
	}
}

func TestPolishDraft_EditListAnchorMultiple(t *testing.T) {
	draft := mechCleanDraft("重复的片段，重复的片段。")
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"重复的片段", "x"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("must converge internally, got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "edit_plan_invalid" {
		t.Fatalf("expected degraded(edit_plan_invalid), got %+v", output)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

// 重叠的 edit：按优先级部分接受——高优先级应用、低优先级丢弃（不再整批拒绝）。
func TestPolishDraft_EditListOverlapPartial(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处。")
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"她站在窗前", "她倚窗而立"},
			[2]string{"在窗前，望着", "x"},
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

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
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "overlap_lower_priority" {
		t.Errorf("drop_reasons = %v, want [overlap_lower_priority]", output.DropReasons)
	}
	want := "她倚窗而立，望着远处。她心里骂自己丢人，真不要脸。"
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
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

// 第 3 条 anchor missing：前后合法 edit 仍应用（部分接受），恰一个非 degraded
// checkpoint（EditCount=实际应用数 3）。
func TestPolishDraft_EditListInvalidNthPartial(t *testing.T) {
	draft := mechCleanDraft("甲乙丙丁戊己庚辛壬癸")
	st := setupPolishStore(t, 1, draft)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"甲", "子"},
			[2]string{"乙", "丑"},
			[2]string{"不存在的片段", "x"},
			[2]string{"丙", "寅"},
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("mid-list invalid edit must be partial-accepted, got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected partial success, got %+v", output)
	}
	if !output.Partial || output.ProposedEditCount != 4 || output.DroppedEditCount != 1 {
		t.Errorf("audit partial/proposed/dropped = %v/%d/%d, want true/4/1",
			output.Partial, output.ProposedEditCount, output.DroppedEditCount)
	}
	// 第 3 条被丢弃、其余三条全部应用：EditCount = 实际应用数 = 3。
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "子丑寅丁戊己庚辛壬癸她心里骂自己丢人，真不要脸。" {
		t.Fatalf("saved draft = %q", saved)
	}
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1", n)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 3 || cp.DroppedEditCount != 1 || !cp.Partial || cp.Degraded {
		t.Errorf("checkpoint audit wrong: %+v", cp)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
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
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(body)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("must converge internally, got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "edit_plan_invalid" {
		t.Fatalf("expected degraded(edit_plan_invalid), got %+v", output)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

func TestPolishDraft_EditListOutputTooLong(t *testing.T) {
	st := setupPolishStore(t, 1, "甲乙")
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"甲", strings.Repeat("长", maxPolishOutputRunes+1)}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("must converge internally, got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "edit_plan_invalid" {
		t.Fatalf("expected degraded(edit_plan_invalid), got %+v", output)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "甲乙" {
		t.Error("draft must remain unchanged")
	}
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1 (degraded)", n)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

// ── 覆盖超限：④ 部分接受/单调用收敛（取代 P0-4 带反馈重试） ──────────────
//
// 普通 draft 场景覆盖上限 50%：超限计划中"导致越线的 edit"被丢弃（部分接受），
// 其余合法 edit 仍应用；全部被丢弃 → 写 rejected/degraded checkpoint
// （ErrorCategory=coverage_exceeded）、polisher 恰被调用 1 次（不再第 2 次模型
// 纠错）、草稿不变。

// TestPolishDraft_EditListCoveragePartial：5 条 edit 中第 5 条（覆盖超限）被丢弃，
// 第 6 条较短仍可接受 → 部分成功（EditCount=5、DroppedEditCount=1、
// DropReasons=[coverage_limit]、Partial=true、恰 1 次 polisher 调用、非 degraded）。
func TestPolishDraft_EditListCoveragePartial(t *testing.T) {
	draft := mechCleanDraft("甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未申酉")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"甲", "壹"},
			[2]string{"乙", "贰"},
			[2]string{"丙", "叁"},
			[2]string{"丁", "肆"},
			[2]string{"戊己庚辛壬癸子丑寅卯辰巳午未", "X"}, // 14 runes → 4+14=18 > 17（含填充后预算）
			[2]string{"申", "伍"}, // 1 rune → 4+1=5 ≤ 17
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("coverage-exceeding edit must be dropped individually, got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected partial success, got %+v", output)
	}
	if !output.Partial || output.ProposedEditCount != 6 || output.DroppedEditCount != 1 {
		t.Errorf("audit partial/proposed/dropped = %v/%d/%d, want true/6/1",
			output.Partial, output.ProposedEditCount, output.DroppedEditCount)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "coverage_limit" {
		t.Errorf("drop_reasons = %v, want [coverage_limit]", output.DropReasons)
	}
	want := "壹贰叁肆戊己庚辛壬癸子丑寅卯辰巳午未伍酉她心里骂自己丢人，真不要脸。"
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 5 || cp.DroppedEditCount != 1 || !cp.Partial || cp.Degraded {
		t.Errorf("checkpoint audit wrong: applied=%d dropped=%d partial=%v degraded=%v",
			cp.EditCount, cp.DroppedEditCount, cp.Partial, cp.Degraded)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1 (no model retry for partial acceptance)", calls)
	}
}

// TestPolishDraft_EditListCoverageConverges：单条 63% 覆盖计划（超 50% 上限）全部
// 被丢弃 → 写 rejected/degraded polish checkpoint（ErrorCategory=coverage_exceeded，
// Digest=当前草稿、Changed=false、Method=edit_list、EditCount=0 实际应用数、
// ProposedEditCount=1、DropReasons=[coverage_limit]）→ 返回成功摘要（Degraded=true）
// → FSM 收敛；polisher 恰被调用 1 次（④ 不再第 2 次模型纠错），草稿不变。
func TestPolishDraft_EditListCoverageConverges(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。晚风拂过她的发梢。")
	st := setupPolishStore(t, 1, draft)
	calls := 0
	badPlan := editListJSON([2]string{"她站在窗前，望着远处的灯火。晚风拂过她的发梢。", "她倚窗而立，望向远方的灯火。晚风拂过她的发梢。"})
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(badPlan)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("all-dropped coverage must converge (no error), got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Degraded || output.ErrorCategory != "coverage_exceeded" {
		t.Fatalf("expected degraded(coverage_exceeded) convergence, got %+v", output)
	}
	if output.Changed || output.OutputDigest != domain.DigestDraft(draft) {
		t.Fatalf("converged output must report changed=false with current digest, got %+v", output)
	}
	if !output.Polished {
		t.Error("converged output must report polished=true (工具完成留痕，调用方继续 post-check)")
	}
	if output.ProposedEditCount != 1 || output.DroppedEditCount != 1 {
		t.Errorf("audit proposed/dropped = %d/%d, want 1/1", output.ProposedEditCount, output.DroppedEditCount)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "coverage_limit" {
		t.Errorf("drop_reasons = %v, want [coverage_limit]", output.DropReasons)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "coverage_exceeded" || cp.Changed {
		t.Errorf("rejected checkpoint must be degraded(coverage_exceeded) changed=false, got %+v", cp)
	}
	if cp.Digest != domain.DigestDraft(draft) {
		t.Errorf("checkpoint digest = %s, want current draft digest", cp.Digest)
	}
	if cp.Method != "edit_list" || cp.EditCount != 0 || cp.ProposedEditCount != 1 || cp.DroppedEditCount != 1 {
		t.Errorf("rejected checkpoint method/edit audit wrong: %+v", cp)
	}
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1 (degraded)", n)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1 (④: no second model correction call)", calls)
	}
}

// TestPolishDraft_Rewrite63PercentPipelineIntegration：~3951 rune 重写队列章节 +
// 63% 覆盖 edit plan 的完整链路（P1-6 集成）：rewrite 场景覆盖上限放宽到 70% →
// 63% 计划一次通过（无纠错重试）→ polish（checkpoint stage=rewrite、method=
// edit_list、edit_count=2）→ check → review（critic pass，epoch-2 绑定 polish
// seq）→ commit（pipeline 门控）→ 队列 drain、终稿覆盖。同一 63% 计划在普通
// draft 场景会被拒（50% 上限，见 TestPolishEditPlan_CoverageRewriteBoundary 与
// TestPolishDraft_EditListCoverageConverges）。
func TestPolishDraft_Rewrite63PercentPipelineIntegration(t *testing.T) {
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
	savePermissiveUserRules(t, st)

	// 已完成章节进入重写队列：终稿 = 旧版本，草稿 = 返工版本（~3951 runes）。
	// 带序号行（第%d段）保证 63% 覆盖计划的 old_string 唯一；序号不用"次"
	// 避免触发节拍账本硬闸（第N+计数单位 ≥3 → error 级文学腔违例）。
	const unit = "她站在窗前，望着远方的灯火。风从巷口涌来，卷起几片枯叶。"
	n := 118
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("第%d段，%s", i+1, unit)
	}
	rework := mechCleanDraft(strings.Join(lines, "\n"))
	if r := utf8.RuneCountInString(rework); r < 3800 || r > 4100 {
		t.Fatalf("rework draft runes = %d, want ~3951", r)
	}
	final := "# 一\n原始终稿内容。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, rework); err != nil {
		t.Fatalf("SaveDraft rework: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "返工"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	// 旧 epoch（epoch 1）terminal 账本：绑定原始终稿 digest。
	now := time.Now().Format(time.RFC3339)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)
	originalDigest := domain.DigestDraft(final)
	oldLedger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: originalDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: originalDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "critic-model"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(oldLedger); err != nil {
		t.Fatalf("Save old ledger: %v", err)
	}

	// 1) 无 polish → check_consistency 建议 polish_draft。
	checkTool := NewCheckConsistencyTool(st)
	checkTool.SetPipelineEnabled(true)
	out1, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check_consistency #1: %v", err)
	}
	var res1 map[string]any
	if err := json.Unmarshal(out1, &res1); err != nil {
		t.Fatal(err)
	}
	act1, _ := res1["required_next_action"].(map[string]any)
	if act1 == nil || act1["action"] != ActionPolishDraft {
		t.Fatalf("required_next_action #1 = %v, want %s", act1, ActionPolishDraft)
	}

	// 2) polish_draft：63% 覆盖 edit plan（两条 old_string：lines[0:37] 与
	// lines[37:74]，各 ≤2000 runes，合计 74/118 行 ≈ 63%）→ rewrite 场景上限
	// 70% 一次通过（无纠错重试，恰 1 次 polisher 调用）。
	polishedLines := make([]string, n)
	for i, l := range lines {
		polishedLines[i] = strings.Replace(l, "她站在窗前，望着远方的灯火。", "她临窗而立，望向远处的灯火。", 1)
	}
	oldA := strings.Join(lines[0:37], "\n")
	oldB := strings.Join(lines[37:74], "\n")
	newA := strings.Join(polishedLines[0:37], "\n")
	newB := strings.Join(polishedLines[37:74], "\n")
	planJSON := editListJSON([2]string{oldA, newA}, [2]string{oldB, newB})
	coverage := float64(utf8.RuneCountInString(oldA)+utf8.RuneCountInString(oldB)) / float64(utf8.RuneCountInString(rework))
	if coverage <= 0.60 || coverage > 0.70 {
		t.Fatalf("coverage = %.1f%%, want (60%%, 70%%] for rewrite acceptance", coverage*100)
	}
	pCalls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		pCalls++
		return &agentcore.LLMResponse{Message: polisherText(planJSON)}, nil
	})
	polishTool := newEnabledPolishTool(st, polisher)
	pOut, err := polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("polish_draft: %v", err)
	}
	var pRes PolishDraftOutput
	if err := json.Unmarshal(pOut, &pRes); err != nil {
		t.Fatal(err)
	}
	if !pRes.Polished || !pRes.Changed || pRes.Degraded {
		t.Fatalf("polish result = %+v, want polished+changed, non-degraded", pRes)
	}
	if pCalls != 1 {
		t.Errorf("polisher calls = %d, want 1 (63%% accepted at rewrite 70%% limit, no retry)", pCalls)
	}
	polishCP := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish")
	if polishCP == nil || polishCP.Stage != "rewrite" || polishCP.Method != "edit_list" || polishCP.EditCount != 2 {
		t.Fatalf("polish checkpoint = %+v, want stage=rewrite method=edit_list edit_count=2", polishCP)
	}
	// 候选已原子落盘：前 74 行精修（两条 edit：0:37 与 37:74）、其余行原样保留。
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	expected := strings.Join(append(append([]string{}, polishedLines[0:74]...), lines[74:]...), "\n") + "她心里骂自己丢人，真不要脸。"
	if saved != expected {
		t.Fatalf("saved draft 与 63%% edit plan 应用结果不一致：\ngot  %q...\nwant %q...", snippet(saved), snippet(expected))
	}

	// 3) 精修后 check_consistency（新 seq）→ 建议 review_style。
	out2, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check_consistency #2: %v", err)
	}
	var res2 map[string]any
	if err := json.Unmarshal(out2, &res2); err != nil {
		t.Fatal(err)
	}
	act2, _ := res2["required_next_action"].(map[string]any)
	if act2 == nil || act2["action"] != ActionReviewStyle {
		t.Fatalf("required_next_action #2 = %v, want %s", act2, ActionReviewStyle)
	}

	// 4) review_style（critic pass）→ 新 epoch（epoch 2）终验，绑定本次 polish seq。
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	reviewTool := NewReviewStyleTool(st, critic, testCriticVersion)
	reviewTool.SetPipelineEnabled(true)
	rOut, err := reviewTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review_style: %v", err)
	}
	var rRes StyleReviewOutput
	if err := json.Unmarshal(rOut, &rRes); err != nil {
		t.Fatal(err)
	}
	if rRes.Verdict != "pass" {
		t.Fatalf("review = %s, want pass", rRes.Verdict)
	}
	ledger, err := st.StyleReview.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	last := ledger.CurrentCycle()
	if last.Request == nil || last.Request.PolishCheckpointSeq != polishCP.Seq {
		t.Fatalf("epoch-2 result 绑定 polish seq = %+v, want %d", last.Request, polishCP.Seq)
	}

	// 5) commit（pipeline 门控开启）→ 放行并 drain 队列。
	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mock-polisher-model"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "63% 精修提交", "characters": []string{"主角"},
		"key_events":       []string{"事件"},
		"world_state_mode": "preserve",
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("commit after 63%% rewrite chain should pass: %v", err)
	}
	progress, err := st.Progress.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.PendingRewrites) != 0 {
		t.Fatalf("PendingRewrites = %v, want drained", progress.PendingRewrites)
	}
	finalText, _ := st.Drafts.LoadChapterText(1)
	if finalText == "" {
		t.Fatal("final chapter should have been overwritten")
	}
}

// ── P0-5：full-text 回退路径的机械回归门禁 ─────────────────────────────

// TestPolishDraft_FullTextMechanicalRegressionRejected：full-text 候选（非 JSON
// 纯正文）引入 error 级机械违规（"不知为何"禁词）→ 机械回归门禁拒绝保存：
// 草稿不变、无 polish checkpoint、错误明确区分"机械回归"、不混入 Degraded=true。
func TestPolishDraft_FullTextMechanicalRegressionRejected(t *testing.T) {
	draft := mechCleanDraft("她停住了，望着远处。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	if hasErrorViolations(computeMechanicalViolations(st, draft, utf8.RuneCountInString(draft))) {
		t.Fatal("test precondition: input draft must be mechanically clean")
	}
	// 非 JSON 纯正文（走 full-text 回退），长度在 40%~2x 范围内，含禁词"不知为何"。
	badText := "她不知为何停住了，望着远处。她心里骂自己丢人，真不要脸。"
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(badText)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected mechanical-regression rejection on full-text candidate")
	}
	if !strings.Contains(err.Error(), "机械回归") {
		t.Errorf("expected mechanical-regression error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("no polish checkpoint after full-text mechanical rejection")
	}
}

// TestPolishDraft_FullTextMechanicalRegressionConverges：full-text 候选连续第 2 次
// 机械回归拒绝（与 edit 路径共享 mechRejectStreak）→ 写 rejected polish checkpoint
// （ErrorCategory=mechanical_regression、Method=full_text）→ 返回成功摘要收敛。
func TestPolishDraft_FullTextMechanicalRegressionConverges(t *testing.T) {
	draft := mechCleanDraft("她停住了，望着远处。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	badText := "她不知为何停住了，望着远处。她心里骂自己丢人，真不要脸。"
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(badText)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	// 第 1 次：fail-closed。
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "机械回归") {
		t.Fatalf("first rejection must fail-closed, got: %v", err)
	}
	// 第 2 次：收敛为 rejected checkpoint（full_text method）。
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
	cp := polishCheckpointOf(t, st, 1)
	if !cp.Degraded || cp.ErrorCategory != "mechanical_regression" || cp.Method != "full_text" {
		t.Errorf("rejected checkpoint must be degraded(mechanical_regression) method=full_text, got %+v", cp)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Error("draft must remain unchanged")
	}
}

// ── 机械回归：④ 第 6 条（责任 edit 剔除 / mechRejectStreak 收敛） ───────
//
// 候选引入新的 error 级机械违规时：逐条单独应用找出责任 edit → 剔除 → 重建候选。
// 存在安全子集 → 部分接受成功（不增加 mechRejectStreak，审计记录 mechanical drop）；
// 仍无法安全（组合违规，无法定位责任 edit）→ mechRejectStreak（首次 fail-closed，
// 连续 2 次收敛为 rejected checkpoint，EditCount=0 实际应用数）。

// TestPolishDraft_EditListMechanicalDropKeepsOthers：三个 edit 中仅中间一个引入
// 禁词（"不知为何"）→ 该条被剔除（mechanical），前后合法 edit 仍应用 → 部分成功
// （Changed=true、Partial=true、DropReasons=[mechanical]、恰 1 次 polisher 调用、
// 非 degraded、mechRejectStreak 不增加）。
func TestPolishDraft_EditListMechanicalDropKeepsOthers(t *testing.T) {
	draft := mechCleanDraft("她停住了，望着远处。他走近了。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	if hasErrorViolations(computeMechanicalViolations(st, draft, utf8.RuneCountInString(draft))) {
		t.Fatal("test precondition: input draft must be mechanically clean")
	}
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"她停住了", "她缓缓停住"},
			[2]string{"望着远处", "她不知为何望着远处"}, // 引入禁词"不知为何"
			[2]string{"他走近了", "他快步走近"},
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("responsible edit must be dropped individually, got: %v", err)
	}
	var output PolishDraftOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected partial success, got %+v", output)
	}
	if !output.Partial || output.ProposedEditCount != 3 || output.DroppedEditCount != 1 {
		t.Errorf("audit partial/proposed/dropped = %v/%d/%d, want true/3/1",
			output.Partial, output.ProposedEditCount, output.DroppedEditCount)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "mechanical" {
		t.Errorf("drop_reasons = %v, want [mechanical]", output.DropReasons)
	}
	want := "她缓缓停住，望着远处。他快步走近。她心里骂自己丢人，真不要脸。"
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 2 || cp.DroppedEditCount != 1 || !cp.Partial || cp.Degraded {
		t.Errorf("checkpoint audit wrong: applied=%d dropped=%d partial=%v degraded=%v",
			cp.EditCount, cp.DroppedEditCount, cp.Partial, cp.Degraded)
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}

	// 机械安全子集部分接受是成功路径：不增加 mechRejectStreak（下一轮同样场景
	// 仍直接部分接受，而不是直接走到收敛）。
	out2, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("second call must also partial-accept (streak not incremented), got: %v", err)
	}
	var output2 PolishDraftOutput
	if err := json.Unmarshal(out2, &output2); err != nil {
		t.Fatal(err)
	}
	if output2.Degraded {
		t.Fatal("safe-subset success must never be degraded (streak not incremented)")
	}
}

// TestPolishDraft_EditListMechanicalCombinationConverges：两条 edit 单独应用都
// clean、组合后才形成"不知为何"（跨 edit 拼接的禁词）→ 无法定位责任 edit →
// 走 mechRejectStreak：第 1 次 fail-closed；第 2 次收敛为 rejected checkpoint
// （Degraded=true + ErrorCategory=mechanical_regression、Method=edit_list、
// EditCount=0 实际应用数、ProposedEditCount=2、DropReasons=[mechanical]）；
// 第 3 次（收敛后计数清零）重新 fail-closed（防无限收敛）。
func TestPolishDraft_EditListMechanicalCombinationConverges(t *testing.T) {
	draft := mechCleanDraft("甲乙说。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	if hasErrorViolations(computeMechanicalViolations(st, draft, utf8.RuneCountInString(draft))) {
		t.Fatal("test precondition: input draft must be mechanically clean")
	}
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"甲", "不知"}, // 单独应用 clean
			[2]string{"乙", "为何"}, // 单独应用 clean；组合后"不知为何" → error 级违规
		))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	// 第 1 次：fail-closed（组合违规无法剔除责任 edit）。
	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "机械回归") {
		t.Fatalf("first combination regression must fail-closed, got: %v", err)
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
	if cp.Method != "edit_list" || cp.EditCount != 0 || cp.ProposedEditCount != 2 || cp.DroppedEditCount != 2 {
		t.Errorf("rejected checkpoint edit audit wrong: method=%s applied=%d proposed=%d dropped=%d",
			cp.Method, cp.EditCount, cp.ProposedEditCount, cp.DroppedEditCount)
	}
	if len(cp.DropReasons) != 1 || cp.DropReasons[0] != "mechanical" {
		t.Errorf("rejected checkpoint drop_reasons = %v, want [mechanical]", cp.DropReasons)
	}

	// 第 3 次：收敛后计数清零，重新 fail-closed（防无限收敛）。
	_, err = tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "机械回归") {
		t.Fatalf("after convergence streak resets; 3rd rejection must fail-closed, got: %v", err)
	}
	if n := polishCheckpointCount(t, st, 1); n != 1 {
		t.Errorf("polish checkpoints = %d, want exactly 1 (rejected)", n)
	}
	if calls != 3 {
		t.Errorf("polisher calls = %d, want 3 (one per Execute)", calls)
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
	task := tool.buildPolishTask(1, draft, utf8.RuneCountInString(draft), "")

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

// ── 归一化匹配（exact + normalized 两级）工具层回归 ─────────────────────

// TestPolishDraft_EditListNormalizedUnique：old_string 精确缺失但白名单归一化后
// 唯一（智能引号 vs ASCII 引号）→ normalized 定位应用成功；checkpoint 审计
// NormalizedMatchCount=1、MatchModes=[normalized]，草稿落盘为应用后文本。
func TestPolishDraft_EditListNormalizedUnique(t *testing.T) {
	draft := mechCleanDraft("她说道：“你好，世界。”他说道：“再见。”这是一段保留原样的上下文，用于满足覆盖比例上限要求。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{`她说道:"你好,世界."`, `她说："你好呀。"`},
		))}, nil
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
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected normalized-apply success, got %+v", output)
	}
	if output.NormalizedMatchCount != 1 {
		t.Errorf("normalized_match_count = %d, want 1", output.NormalizedMatchCount)
	}
	if len(output.MatchModes) != 1 || output.MatchModes[0] != "normalized" {
		t.Errorf("match_modes = %v, want [normalized]", output.MatchModes)
	}
	want := `她说："你好呀。"他说道：“再见。”这是一段保留原样的上下文，用于满足覆盖比例上限要求。她心里骂自己丢人，真不要脸。`
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != want {
		t.Fatalf("saved draft = %q, want %q", saved, want)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 1 || cp.NormalizedMatchCount != 1 {
		t.Errorf("checkpoint audit wrong: edit_count=%d normalized=%d", cp.EditCount, cp.NormalizedMatchCount)
	}
	if len(cp.MatchModes) != 1 || cp.MatchModes[0] != "normalized" {
		t.Errorf("checkpoint match_modes = %v, want [normalized]", cp.MatchModes)
	}
}

// ── 部分接受审计（④）：checkpoint 审计字段与正文隔离 ────────────────────

// TestPolishDraft_EditListPartialCheckpointAudit：部分接受后 checkpoint 的
// EditCount=实际应用数、ProposedEditCount/DroppedEditCount/DropReasons/Partial/
// MatchModes 审计正确，且审计字段绝不含正文/old_string/new_string 内容。
func TestPolishDraft_EditListPartialCheckpointAudit(t *testing.T) {
	draft := mechCleanDraft("甲乙丙丁戊己庚辛壬癸")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText(editListJSON(
			[2]string{"甲", "子"},
			[2]string{"不存在的片段", "x"},
			[2]string{"乙", "丑"},
		))}, nil
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
	if !output.Polished || !output.Changed || output.Degraded {
		t.Fatalf("expected partial success, got %+v", output)
	}
	// EditCount == 实际应用数（2）；proposed=3、dropped=1（anchor_missing）。
	if output.ProposedEditCount != 3 || output.DroppedEditCount != 1 || !output.Partial {
		t.Errorf("audit proposed/dropped/partial = %d/%d/%v, want 3/1/true",
			output.ProposedEditCount, output.DroppedEditCount, output.Partial)
	}
	if len(output.DropReasons) != 1 || output.DropReasons[0] != "anchor_missing" {
		t.Errorf("drop_reasons = %v, want [anchor_missing]", output.DropReasons)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != "子丑丙丁戊己庚辛壬癸她心里骂自己丢人，真不要脸。" {
		t.Fatalf("saved draft = %q", saved)
	}
	cp := polishCheckpointOf(t, st, 1)
	if cp.EditCount != 2 || cp.ProposedEditCount != 3 || cp.DroppedEditCount != 1 || !cp.Partial {
		t.Errorf("checkpoint edit audit wrong: %+v", cp)
	}
	if len(cp.DropReasons) != 1 || cp.DropReasons[0] != "anchor_missing" {
		t.Errorf("checkpoint drop_reasons = %v, want [anchor_missing]", cp.DropReasons)
	}
	if len(cp.MatchModes) != 2 || cp.MatchModes[0] != "exact" || cp.MatchModes[1] != "exact" {
		t.Errorf("checkpoint match_modes = %v, want [exact exact]", cp.MatchModes)
	}
	if cp.NormalizedMatchCount != 0 {
		t.Errorf("checkpoint normalized_match_count = %d, want 0", cp.NormalizedMatchCount)
	}
	// 审计字段不得含正文内容（old_string/new_string/正文片段均不得出现在审计 JSON）。
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"不存在的片段", "子", "丑", "甲乙丙丁戊己庚辛壬癸"} {
		if strings.Contains(string(raw), frag) {
			t.Errorf("checkpoint 审计 JSON 不得含正文内容 %q（审计字段与正文隔离）", frag)
		}
	}
	if calls != 1 {
		t.Errorf("polisher calls = %d, want 1", calls)
	}
}

// ── P0-3：polish 落盘前原子 CAS（防 TOCTOU）────────────────────────────

// TestPolishDraft_CandidateStaleDraftChanged_FullText 覆盖 P0-3 核心场景
// （ora-1 死锁根因：critic 接受旧候选时另一在途 polish 覆盖草稿）：模型调用
// 期间草稿被并发修改 → full-text 候选被丢弃（草稿不被覆盖）、不写 polish
// checkpoint、返回明确 stale 错误。
func TestPolishDraft_CandidateStaleDraftChanged_FullText(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	concurrent := draft + "\n\n并发修改的内容。"
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		// 模拟模型调用期间另一在途流程覆盖草稿（TOCTOU 窗口）。
		if err := st.Drafts.SaveDraft(1, concurrent); err != nil {
			return nil, err
		}
		return &agentcore.LLMResponse{Message: polisherText("她倚窗而立。短句更有力，节奏明快。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected stale error when draft changed during polish")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error message, got: %v", err)
	}

	// 草稿必须保持并发修改后的内容，绝不能被 polisher 候选覆盖。
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != concurrent {
		t.Fatalf("draft must not be overwritten by stale candidate, got %q", snippet(saved))
	}
	// 候选被丢弃：不写 polish checkpoint。
	if n := polishCheckpointCount(t, st, 1); n != 0 {
		t.Errorf("no polish checkpoint should be written for stale candidate, got %d", n)
	}
}

// TestPolishDraft_CandidateStaleDraftChanged_EditList 同场景的 edit_list 路径：
// 模型返回 edit 候选、落盘前草稿已被并发修改 → 候选丢弃、草稿不被覆盖。
func TestPolishDraft_CandidateStaleDraftChanged_EditList(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	concurrent := draft + "\n\n并发修改的内容。"
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		if err := st.Drafts.SaveDraft(1, concurrent); err != nil {
			return nil, err
		}
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"她站在窗前", "她倚窗而立"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != concurrent {
		t.Fatalf("draft must not be overwritten by stale edit-list candidate, got %q", snippet(saved))
	}
	if n := polishCheckpointCount(t, st, 1); n != 0 {
		t.Errorf("no polish checkpoint should be written for stale candidate, got %d", n)
	}
}

// TestPolishDraft_CandidateStaleLedgerChanged 覆盖 P0-3 CAS #2：模型调用期间
// style review 账本被并发修改（新 pending 周期，草稿进入评审锁定期）→ 候选
// 同样被丢弃。
func TestPolishDraft_CandidateStaleLedgerChanged(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。")
	st := setupPolishStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		// 模拟模型调用期间另一评审流程创建 pending 账本周期。
		if err := st.StyleReview.Save(domain.StyleReviewLedger{
			SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
			Cycles: []domain.StyleReviewEntry{{
				Cycle: 1, Status: domain.ReviewStatusInitialPending,
				CreatedAt:   time.Now().Format(time.RFC3339),
				AttemptID:   "concurrent-attempt",
				DraftDigest: domain.DigestDraft(draft),
				BasisDigest: "sha256:" + strings.Repeat("b", 64),
				Request:     &domain.StyleReviewRequest{Prompt: "v1", Model: "m"},
			}},
		}); err != nil {
			return nil, err
		}
		return &agentcore.LLMResponse{Message: polisherText(editListJSON([2]string{"她站在窗前", "她倚窗而立"}))}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error when ledger changed during polish, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Fatalf("draft must remain unchanged when candidate stale, got %q", snippet(saved))
	}
	if n := polishCheckpointCount(t, st, 1); n != 0 {
		t.Errorf("no polish checkpoint should be written for stale candidate, got %d", n)
	}
}

// TestPolishDraft_CandidateStalePolishAdvanced 覆盖 P0-3 CAS #3：模型调用期间
// 出现更新的 polish checkpoint（顺序绑定被抢先）→ 候选丢弃。
func TestPolishDraft_CandidateStalePolishAdvanced(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		// 模拟模型调用期间另一流程抢先建立新 polish checkpoint。
		if _, err := st.Checkpoints.AppendPolish(
			domain.ChapterScope(1), "polish", "drafts/01.draft.md",
			"sha256:"+strings.Repeat("a", 64),
			domain.PolishCheckpointMeta{InputDigest: "sha256:" + strings.Repeat("b", 64), Stage: "draft"},
		); err != nil {
			return nil, err
		}
		return &agentcore.LLMResponse{Message: polisherText("她倚窗而立。短句更有力，节奏明快。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error when polish checkpoint advanced, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != draft {
		t.Fatalf("draft must remain unchanged, got %q", snippet(saved))
	}
}

// ── P1-7（P0-3 遗留）：degraded/rejected 收敛路径的 digest 预检 ─────────
// 正常路径（CommitPolishCandidate）有 CAS 校验；degraded 路径（provider 失败 /
// 全部 edit 被丢弃 / 机械回归收敛）此前直接写绑定"模型调用开始时 digest"的
// checkpoint，草稿被并发修改时会留下陈旧绑定。修复后写 checkpoint 前预检
// digest，不匹配则跳过写入并返回 stale 错误（与正常路径语义一致）。

// TestPolishDraft_DegradedConvergenceStaleDraftNoBinding 覆盖 edit 全丢弃收敛
// （handleEditPlanRejected → writeDegradedPolishCheckpoint）：模型调用期间草稿
// 被并发修改 → 不写绑定旧 digest 的陈旧 degraded checkpoint、返回 stale 错误。
func TestPolishDraft_DegradedConvergenceStaleDraftNoBinding(t *testing.T) {
	draft := mechCleanDraft("她站在窗前，望着远处的灯火。晚风拂过她的发梢。")
	st := setupPolishStore(t, 1, draft)
	concurrent := draft + "\n\n并发修改。"
	// 整章覆盖 → 超过 50% 覆盖上限 → 全部丢弃 → 收敛路径（degraded）。
	badPlan := editListJSON([2]string{draft, "她倚窗而立，望向远方的灯火。晚风拂过她的发梢。"})
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		// 模拟模型调用期间草稿被并发修改（TOCTOU 窗口）。
		if err := st.Drafts.SaveDraft(1, concurrent); err != nil {
			return nil, err
		}
		return &agentcore.LLMResponse{Message: polisherText(badPlan)}, nil
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected stale error when draft changed during polish (收敛不写陈旧绑定)")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error message, got: %v", err)
	}
	// 草稿保持并发修改后的内容，绝不被覆盖。
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != concurrent {
		t.Fatalf("draft must not be overwritten, got %q", snippet(saved))
	}
	// 不写陈旧绑定的 degraded checkpoint。
	if n := polishCheckpointCount(t, st, 1); n != 0 {
		t.Errorf("no stale degraded checkpoint should be written, got %d", n)
	}
}

// TestPolishDraft_DegradedProviderFailureStaleDraftNoBinding 覆盖 provider 失败
// 降级路径（handlePolisherFailure）：同样执行 digest 预检，草稿被并发修改时
// 不写陈旧绑定。
func TestPolishDraft_DegradedProviderFailureStaleDraftNoBinding(t *testing.T) {
	const draft = "她站在窗前。这个句子很长很长，长到读起来非常累，一点都不顺口。"
	st := setupPolishStore(t, 1, draft)
	concurrent := draft + "\n\n并发修改。"
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		if err := st.Drafts.SaveDraft(1, concurrent); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("provider stream idle timeout: %w", agentcore.ErrProviderStreamIdle)
	})
	tool := newEnabledPolishTool(st, polisher)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected stale error when draft changed during polish (provider 降级不写陈旧绑定)")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error message, got: %v", err)
	}
	saved, _, _ := st.Drafts.LoadChapterContent(1)
	if saved != concurrent {
		t.Fatalf("draft must not be overwritten, got %q", snippet(saved))
	}
	if n := polishCheckpointCount(t, st, 1); n != 0 {
		t.Errorf("no stale degraded checkpoint should be written, got %d", n)
	}
}
