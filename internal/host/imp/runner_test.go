package imp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// scriptedLLM 按调用顺序返回不同响应：第一次 foundation envelope，之后每次 analyzer envelope。
type scriptedLLM struct {
	responses []string
	calls     atomic.Int32
}

func (s *scriptedLLM) Generate(_ context.Context, _ []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	idx := int(s.calls.Add(1)) - 1
	if idx >= len(s.responses) {
		return nil, fmt.Errorf("scriptedLLM exhausted at call %d", idx+1)
	}
	return &agentcore.LLMResponse{
		Message: agentcore.Message{
			Role:      agentcore.RoleAssistant,
			Content:   []agentcore.ContentBlock{agentcore.TextBlock(s.responses[idx])},
			Timestamp: time.Now(),
		},
	}, nil
}

// capturingLLM 按调用顺序返回固定响应，并记录每次调用的 user prompt。
type capturingLLM struct {
	responses   []string
	userPrompts []string
	calls       atomic.Int32
}

func (s *capturingLLM) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	idx := int(s.calls.Add(1)) - 1
	for _, m := range msgs {
		if m.Role == agentcore.RoleUser {
			s.userPrompts = append(s.userPrompts, m.TextContent())
		}
	}
	if idx >= len(s.responses) {
		return nil, fmt.Errorf("capturingLLM exhausted at call %d", idx+1)
	}
	return &agentcore.LLMResponse{
		Message: agentcore.Message{
			Role:      agentcore.RoleAssistant,
			Content:   []agentcore.ContentBlock{agentcore.TextBlock(s.responses[idx])},
			Timestamp: time.Now(),
		},
	}, nil
}

// 顺序导入基线传递：第 N 章提交的 character_state 必须成为第 N+1 章分析的
// "开章基线"（runner 每章分析前 LoadCharacterState 并注入 analyzer prompt）。
func TestRunner_SequentialImportPassesCharacterStateBaseline(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "novel.txt")
	body := strings.Repeat("正文段落，足够字数以通过 LoadChapterContent 校验。她心里骂自己丢人，真不要脸。\n", 30)
	content := "第一章 初遇\n" + body + "\n第二章 循迹\n" + body
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.NewStore(filepath.Join(dir, "out"))
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("baseline-test", 0); err != nil {
		t.Fatal(err)
	}

	// 第一章的 CHARACTER_STATE（林晚 location.place=陈氏祖宅）经 commit 落库后，
	// 第二章分析必须收到该开章基线。
	llm := &capturingLLM{responses: []string{
		validEnvelope,
		validAnalyzerEnvelope,
		validAnalyzerEnvelope,
	}}
	deps := Deps{
		Store:      st,
		CommitTool: tools.NewCommitChapterTool(st),
		LLM:        llm,
		Prompts: Prompts{
			Foundation: "foundation prompt with ${chapter_count}",
			Analyzer:   "analyzer prompt",
		},
	}
	events, err := Run(context.Background(), deps, Options{SourcePath: src})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for ev := range events {
		if ev.Err != nil {
			t.Fatalf("import errored: %v", ev.Err)
		}
	}

	// 基线已落库
	states, err := st.World.LoadCharacterState()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cs := range states {
		if cs.Entity == "林晚" && cs.Field == "location.place" && cs.Value == "陈氏祖宅" {
			found = true
		}
	}
	if !found {
		t.Fatalf("chapter-1 character_state not persisted: %+v", states)
	}

	// 第二章分析 prompt 含开章基线
	var ch2Prompt string
	for _, p := range llm.userPrompts {
		if strings.Contains(p, "请分析第 2 章") {
			ch2Prompt = p
		}
	}
	if ch2Prompt == "" {
		t.Fatalf("chapter-2 analyzer prompt not captured; prompts=%d", len(llm.userPrompts))
	}
	if !strings.Contains(ch2Prompt, "## 已知角色状态（开章基线）") {
		t.Fatalf("chapter-2 prompt missing baseline section: %s", ch2Prompt)
	}
	if !strings.Contains(ch2Prompt, "`林晚` `location.place`：陈氏祖宅") {
		t.Fatalf("chapter-2 prompt missing chapter-1 baseline entry: %s", ch2Prompt)
	}
}

func TestRunner_FullImport(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "novel.txt")
	body := strings.Repeat("正文段落，足够字数以通过 LoadChapterContent 校验。她心里骂自己丢人，真不要脸。\n", 30)
	content := "第一章 初遇\n" + body + "\n第二章 循迹\n" + body
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.NewStore(filepath.Join(dir, "out"))
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("runner-test", 0); err != nil {
		t.Fatal(err)
	}

	llm := &scriptedLLM{responses: []string{
		validEnvelope,
		validAnalyzerEnvelope,
		validAnalyzerEnvelope,
	}}
	deps := Deps{
		Store:      st,
		CommitTool: tools.NewCommitChapterTool(st),
		LLM:        llm,
		Prompts: Prompts{
			Foundation: "foundation prompt with ${chapter_count}",
			Analyzer:   "analyzer prompt",
		},
	}

	events, err := Run(context.Background(), deps, Options{SourcePath: src})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var stages []Stage
	var lastErr error
	for ev := range events {
		stages = append(stages, ev.Stage)
		if ev.Err != nil {
			lastErr = ev.Err
		}
	}
	if lastErr != nil {
		t.Fatalf("import errored: %v", lastErr)
	}
	if stages[len(stages)-1] != StageDone {
		t.Errorf("last stage: %v, want done; full: %v", stages[len(stages)-1], stages)
	}

	prog, _ := st.Progress.Load()
	if len(prog.CompletedChapters) != 2 {
		t.Errorf("completed chapters: %v", prog.CompletedChapters)
	}
	// 回归：导入不得把书自动判完结（否则"继续创作"撞上已完结的书无法续写），
	// 且必须是分层模式（续写靠 append_volume/expand_arc，非分层无路可扩）。
	if prog.Phase == domain.PhaseComplete {
		t.Errorf("import must NOT auto-complete the book, phase=%q", prog.Phase)
	}
	if !prog.Layered {
		t.Errorf("imported book must be layered")
	}
	if llm.calls.Load() != 3 {
		t.Errorf("expected 3 LLM calls (1 foundation + 2 chapters), got %d", llm.calls.Load())
	}
}

func TestRunner_SkipsAlreadyCompletedChapters(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "novel.txt")
	body := strings.Repeat("正文段落。她心里骂自己丢人，真不要脸。\n", 30)
	content := "第一章 a\n" + body + "\n第二章 b\n" + body
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.NewStore(filepath.Join(dir, "out"))
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("skip-test", 0); err != nil {
		t.Fatal(err)
	}

	// 第一次导入：3 次 LLM 调用 (foundation + 2 chapters)
	llm := &scriptedLLM{responses: []string{
		validEnvelope,
		validAnalyzerEnvelope,
		validAnalyzerEnvelope,
	}}
	deps := Deps{
		Store:      st,
		CommitTool: tools.NewCommitChapterTool(st),
		LLM:        llm,
		Prompts:    Prompts{Foundation: "x", Analyzer: "x"},
	}
	events, err := Run(context.Background(), deps, Options{SourcePath: src})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if llm.calls.Load() != 3 {
		t.Fatalf("first import: want 3 calls, got %d", llm.calls.Load())
	}

	// 第二次导入相同文件：foundation 已存在 → 0 次 LLM；章节已完成 → 0 次 LLM
	llm2 := &scriptedLLM{responses: []string{}} // 任何 LLM 调用都会失败
	deps.LLM = llm2
	events2, err := Run(context.Background(), deps, Options{SourcePath: src})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events2 {
		if ev.Err != nil {
			t.Fatalf("re-import errored: %v", ev.Err)
		}
	}
	if llm2.calls.Load() != 0 {
		t.Errorf("re-import should make 0 LLM calls, got %d", llm2.calls.Load())
	}
}

func TestRunner_ResumeFromSkipsFoundation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "novel.txt")
	body := strings.Repeat("正文。她心里骂自己丢人，真不要脸。\n", 30)
	content := "第一章 a\n" + body + "\n第二章 b\n" + body
	_ = os.WriteFile(src, []byte(content), 0o644)

	st := store.NewStore(filepath.Join(dir, "out"))
	_ = st.Init()
	_ = st.Progress.Init("resume-test", 0)
	// 预置 foundation
	fr, _ := parseFoundationOutput(validEnvelope, 2)
	if err := PersistFoundation(context.Background(), st, "short", fr, nil); err != nil {
		t.Fatal(err)
	}

	llm := &scriptedLLM{responses: []string{validAnalyzerEnvelope, validAnalyzerEnvelope}}
	deps := Deps{
		Store:      st,
		CommitTool: tools.NewCommitChapterTool(st),
		LLM:        llm,
		Prompts:    Prompts{Foundation: "x", Analyzer: "x"},
	}
	events, err := Run(context.Background(), deps, Options{SourcePath: src, ResumeFrom: 1})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Err != nil {
			t.Fatalf("err: %v", ev.Err)
		}
	}
	if llm.calls.Load() != 2 {
		t.Errorf("want 2 chapter LLM calls (foundation skipped), got %d", llm.calls.Load())
	}
}

func TestRunner_UsesStageSpecificModels(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "novel.txt")
	body := strings.Repeat("正文段落，足够字数以通过 LoadChapterContent 校验。她心里骂自己丢人，真不要脸。\n", 30)
	first := "第一章 初遇\n" + body
	secondStart := strings.Count(first, "\n") + 1
	if err := os.WriteFile(src, []byte(first+"第二章 循迹\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}

	st := store.NewStore(filepath.Join(dir, "out"))
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("stage-model-test", 0); err != nil {
		t.Fatal(err)
	}
	segment := &segmentTestLLM{response: fmt.Sprintf("=== CHAPTERS ===\n[{\"title\":\"初遇\",\"start_line\":1},{\"title\":\"循迹\",\"start_line\":%d}]", secondStart)}
	synthesize := &scriptedLLM{responses: []string{validEnvelope}}
	analyze := &scriptedLLM{responses: []string{validAnalyzerEnvelope, validAnalyzerEnvelope}}
	events, err := Run(context.Background(), Deps{
		Store:         st,
		CommitTool:    tools.NewCommitChapterTool(st),
		SegmentLLM:    segment,
		AnalyzeLLM:    analyze,
		SynthesizeLLM: synthesize,
		Prompts:       Prompts{Segment: "segment", Synthesize: "synthesize", Analyze: "analyze"},
	}, Options{SourcePath: src})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Err != nil {
			t.Fatalf("import errored: %v", ev.Err)
		}
	}
	if segment.calls != 1 || synthesize.calls.Load() != 1 || analyze.calls.Load() != 2 {
		t.Fatalf("stage model calls: segment=%d synthesize=%d analyze=%d", segment.calls, synthesize.calls.Load(), analyze.calls.Load())
	}
}
