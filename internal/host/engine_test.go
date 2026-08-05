package host

// Engine 端到端集成测试(engine-rfc.md §7 原型验收):
// 真实 store + 真实 Worker 工具 + 脚本化 ChatModel,验证
//  1. Route 驱动的完整写书链路:写第1章 → 写第2章 → 完本 → 引擎自然停机
//  2. Worker 失败路径:重试一次 → Arbiter worker_failure 裁定 abort → 暂停 + 审计落盘
//  3. 僵局路径:同指令无进展 ×3 → Arbiter deadlock 裁定 → 审计落盘 → abort 停机

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/arbiter"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// namedChatModel 实现 ModelNamer 的 scriptedChatModel 变体（P0 provenance 测试用）。
type namedChatModel struct {
	scriptedChatModel
	name string
}

func (m *namedChatModel) ModelName() string { return m.name }

// TestEngineRunWorker_RecordsWriterModel 验证 P0 provenance：Engine 在派发 writer 前
// 把当前生效的作者模型记录到 RunMeta.LastAuthorModel（真实"最近一次正文写入"模型）。
func TestEngineRunWorker_RecordsWriterModel(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatal(err)
	}
	model := &namedChatModel{name: "writer-ds-v1"}
	model.fn = func(msgs []agentcore.Message) agentcore.Message {
		return testTextMsg("done")
	}
	workers := subagent.NewRunner(subagent.Config{
		Name: "writer", Description: "test writer", Model: model,
		SystemPrompt: "test", MaxTurns: 1,
	})
	e, _, _ := newTestEngine(t, st, workers, nil)

	inst := &flow.Instruction{Agent: "writer", Chapter: 1, Task: "写第 1 章"}
	if err := e.runWorker(t.Context(), inst); err != nil {
		t.Fatalf("runWorker: %v", err)
	}
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.LastAuthorModel != "writer-ds-v1" {
		t.Fatalf("LastAuthorModel = %v, want writer-ds-v1（派发时记录的真实作者模型）", meta.LastAuthorModel)
	}
}

// scriptedChatModel 按回调产出响应的最小 ChatModel。
type scriptedChatModel struct {
	fn func(msgs []agentcore.Message) agentcore.Message
}

func (m *scriptedChatModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return &agentcore.LLMResponse{Message: m.fn(msgs)}, nil
}

func (m *scriptedChatModel) GenerateStream(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	resp, _ := m.Generate(ctx, msgs, tools, opts...)
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *scriptedChatModel) SupportsTools() bool { return true }

// editThenCancelModel 复现 #84：每次 Worker 都成功产生一个内容不同的
// edit checkpoint，随后在同一 run 内返回 context canceled，始终没有 commit。
type editThenCancelModel struct {
	edits atomic.Int32
}

func (m *editThenCancelModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == agentcore.RoleTool {
		return nil, context.Canceled
	}
	n := int(m.edits.Add(1))
	return &agentcore.LLMResponse{Message: testToolCallMsg("edit_chapter", map[string]any{
		"chapter":    1,
		"old_string": fmt.Sprintf("版本%d", n-1),
		"new_string": fmt.Sprintf("版本%d", n),
	})}, nil
}

func (m *editThenCancelModel) GenerateStream(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	resp, err := m.Generate(ctx, msgs, tools, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *editThenCancelModel) SupportsTools() bool { return true }

func testToolCallMsg(name string, args any) agentcore.Message {
	data, _ := json.Marshal(args)
	return agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.ToolCallBlock(agentcore.ToolCall{
			ID: "tc-" + name, Name: name, Args: data,
		})},
		StopReason: agentcore.StopReasonToolUse,
	}
}

func testTextMsg(text string) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(text)},
		StopReason: agentcore.StopReasonStop,
	}
}

var chapterRe = regexp.MustCompile(`写第 (\d+) 章`)

// scriptedWriterModel 按对话内已有的 tool 结果数决定下一步,
// 走完整 plan → draft → check → commit 序列(真实工具,真实落盘)。
func scriptedWriterModel() *scriptedChatModel {
	return &scriptedChatModel{fn: func(msgs []agentcore.Message) agentcore.Message {
		chapter := 0
		toolResults := 0
		for _, m := range msgs {
			if m.Role == agentcore.RoleUser {
				if match := chapterRe.FindStringSubmatch(m.TextContent()); match != nil {
					chapter, _ = strconv.Atoi(match[1])
				}
			}
			if m.Role == agentcore.RoleTool {
				toolResults++
			}
		}
		switch toolResults {
		case 0:
			return testToolCallMsg("plan_chapter", map[string]any{
				"chapter": chapter, "title": fmt.Sprintf("第%d章", chapter),
				"goal": "推进主线", "conflict": "主角遇阻", "hook": "悬念收尾",
			})
		case 1:
			return testToolCallMsg("draft_chapter", map[string]any{
				"chapter": chapter, "mode": "write",
				"content": strings.Repeat(fmt.Sprintf("第%d章的正文段落，主角在黑暗中摸索前行。她心里骂自己丢人，真不要脸。", chapter), 20),
			})
		case 2:
			return testToolCallMsg("check_consistency", map[string]any{"chapter": chapter})
		default:
			return testToolCallMsg("commit_chapter", map[string]any{
				"chapter": chapter, "summary": fmt.Sprintf("第%d章摘要", chapter),
				"characters": []string{"主角"}, "key_events": []string{"推进"},
				"hook_type": "crisis", "world_state_mode": "preserve",
			})
		}
	}}
}

// newTestEngine 组装带真实 store/observer 的引擎;返回引擎、事件采集与完成信号。
func newTestEngine(t *testing.T, st *storepkg.Store, workers *subagent.Runner, arbiterModel agentcore.ChatModel) (*engine, *[]Event, chan struct{}) {
	t.Helper()
	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatalf("init run meta: %v", err)
	}
	var mu sync.Mutex
	events := &[]Event{}
	done := make(chan struct{}, 1)
	obs := newObserver(st, func(ev Event) {
		mu.Lock()
		*events = append(*events, ev)
		mu.Unlock()
	}, func(string) {}, func() {})
	e := &engine{
		store:           st,
		workers:         workers,
		arbiterModel:    arbiterModel,
		failurePrompt:   "sys",
		planStartPrompt: "sys",
		style:           "default",
		observer:        obs,
		refresh:         func() {},
		emitEvent: func(ev Event) {
			mu.Lock()
			*events = append(*events, ev)
			mu.Unlock()
		},
		notify: func(string, string, string, string) {},
		onDone: func() {
			select {
			case done <- struct{}{}:
			default:
			}
		},
	}
	e.gate = NewChapterAdvanceGate(st, func(string) { e.abort() }, func(string, string) {})
	return e, events, done
}

func waitEngineDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("引擎未在期限内停机")
	}
}

func TestEngine_ReviewPermitWritesExactlyOneNewChapter(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("逐章验收试书", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a"},
		{Chapter: 2, Title: "二", CoreEvent: "b"},
		{Chapter: 3, Title: "三", CoreEvent: "c"},
	}); err != nil {
		t.Fatal(err)
	}
	writer := subagent.Config{
		Name: "writer", Description: "test writer", Model: scriptedWriterModel(), SystemPrompt: "test",
		Tools: []agentcore.Tool{
			tools.NewPlanChapterTool(st, nil), tools.NewDraftChapterTool(st, nil),
			tools.NewCheckConsistencyTool(st), tools.NewCommitChapterTool(st),
		},
		MaxTurns: 10, StopAfterTools: []string{"commit_chapter"},
	}
	e, _, done := newTestEngine(t, st, subagent.NewRunner(writer), nil)
	if err := st.RunMeta.SetAdvanceMode(domain.ChapterAdvanceReview); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.GrantAdvancePermit(1); err != nil {
		t.Fatal(err)
	}
	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		t.Fatalf("load progress: %v", err)
	}
	if len(progress.CompletedChapters) != 1 || progress.CompletedChapters[0] != 1 {
		t.Fatalf("一个许可必须恰好只稳定一个新章: %v", progress.CompletedChapters)
	}
	meta, _ := st.RunMeta.Load()
	if meta.AdvancePermitChapter != 0 {
		t.Fatalf("稳定提交后许可必须消费: %+v", meta)
	}
}

func TestEngine_StalePairedDispatchDoesNotBypassHold(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("过期派单试书", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatal(err)
	}
	e, _, _ := newTestEngine(t, st, subagent.NewRunner(), nil)
	e.pending = []controlOp{{
		hold:     &arbiter.AdvanceHoldOp{After: domain.AdvanceHoldAtBoundary, Reason: "先停下"},
		dispatch: &arbiter.DispatchOp{Agent: "editor", Task: "过期任务"},
		facts:    arbiter.InterventionFacts{Phase: string(domain.PhaseOutline)},
	}}

	if e.applyPendingOps(context.Background()) {
		t.Fatal("事实过期的配对派单未落入 next 时不得绕过 Gate")
	}
	if e.next != nil || e.deferGateForNext {
		t.Fatalf("过期派单不得留下可执行指令: next=%+v defer=%v", e.next, e.deferGateForNext)
	}
	meta, _ := st.RunMeta.Load()
	if meta.AdvanceHold != nil {
		t.Fatalf("配对派单过期时不得留下孤立 hold: %+v", meta.AdvanceHold)
	}
	if e.gate.HandleBoundary() {
		t.Fatal("无孤立 hold 时 Gate 不应伪造暂停")
	}
}

// TestEngine_WritesBookToCompletion 完整链路:两章非分层书从 writing 写到 complete。
func TestEngine_WritesBookToCompletion(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("引擎试书", 2); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "第一章", CoreEvent: "开端"},
		{Chapter: 2, Title: "第二章", CoreEvent: "终局"},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}

	writer := subagent.Config{
		Name: "writer", Description: "test writer",
		Model:        scriptedWriterModel(),
		SystemPrompt: "test",
		Tools: []agentcore.Tool{
			tools.NewPlanChapterTool(st, nil),
			tools.NewDraftChapterTool(st, nil),
			tools.NewCheckConsistencyTool(st),
			tools.NewCommitChapterTool(st),
		},
		MaxTurns:       10,
		StopAfterTools: []string{"commit_chapter"},
	}
	e, events, done := newTestEngine(t, st, subagent.NewRunner(writer), nil)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		t.Fatalf("load progress: %v", err)
	}
	if progress.Phase != domain.PhaseComplete {
		t.Fatalf("两章写满应完本, got phase=%s completed=%v", progress.Phase, progress.CompletedChapters)
	}
	if len(progress.CompletedChapters) != 2 {
		t.Fatalf("应完成 2 章, got %v", progress.CompletedChapters)
	}
	// 事件形状:每章一条 DISPATCH(engine 发起),TOOL 行来自进度中继
	var dispatches, toolRows int
	for _, ev := range *events {
		switch ev.Category {
		case "DISPATCH":
			dispatches++
		case "TOOL":
			toolRows++
		}
	}
	if dispatches < 2 {
		t.Fatalf("应至少 2 条 DISPATCH 事件, got %d", dispatches)
	}
	if toolRows == 0 {
		t.Fatal("Worker 工具进度未经中继投影(TOOL 行缺失)")
	}
}

// TestEngine_WorkerFailureConsultsArbiterAndAborts 失败路径:
// 空转 writer 被 StopGuard 升级 → 重试一次 → Arbiter 裁定 abort → 暂停 + 审计。
func TestEngine_WorkerFailureConsultsArbiterAndAborts(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("失败试书", 2); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "一", CoreEvent: "s"}}); err != nil {
		t.Fatalf("outline: %v", err)
	}

	var runs atomic.Int32
	// writer 每轮只回文字不落盘 → guard.NewWriterStopGuard 连续拦截后升级 → Execute 报错
	idle := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		return testTextMsg("我写完了(其实什么都没做)")
	}}
	writer := subagent.Config{
		Name: "writer", Description: "idle writer",
		Model: idle, SystemPrompt: "test", MaxTurns: 20,
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			runs.Add(1)
			return failNTimesGuard()
		},
	}
	// Arbiter 裁定 abort
	arb := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		return testTextMsg(`{"action":"abort","reason":"writer 反复空转,建议人工检查模型配置"}`)
	}}
	e, _, done := newTestEngine(t, st, subagent.NewRunner(writer), arb)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	if got := runs.Load(); got != 2 {
		t.Fatalf("首败应重试一次(共 2 次 spawn), got %d", got)
	}
	recs, err := st.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.Kind == "worker_failure" && r.Decider == "arbiter" {
			found = true
			if !strings.Contains(string(r.Decision), "abort") {
				t.Fatalf("裁定内容应含 abort: %s", r.Decision)
			}
		}
	}
	if !found {
		t.Fatalf("worker_failure 裁定必须落盘: %+v", recs)
	}
}

// failNTimesGuard 立即升级的 StopGuard(模拟空转熔断)。
func failNTimesGuard() agentcore.StopGuard {
	return func(context.Context, agentcore.StopInfo) agentcore.StopDecision {
		return agentcore.StopDecision{Allow: false, Escalate: true}
	}
}

// TestEngine_RetriesUnfinishedPlanStart 启动裁定失败后的自愈路径:StartPrompt 已落盘、
// PlanStart 缺位(启动时模型故障)→ 引擎起动时现场补裁 → 固化 PlanStartRecord → 派发规划师。
// 规划师不落盘 → 走既有僵局路径停机,证明补裁后引擎回到正常轨道。
func TestEngine_RetriesUnfinishedPlanStart(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("", 0); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// 模拟 StartPrepared 失败现场:输入事实在,裁定事实缺位。
	if err := st.RunMeta.SetStartPrompt("凡人修仙"); err != nil {
		t.Fatalf("start prompt: %v", err)
	}

	// Arbiter:首次调用是补裁(plan_start),之后是僵局咨询(abort 收尾)。
	var arbCalls atomic.Int32
	arb := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		if arbCalls.Add(1) == 1 {
			return testTextMsg(`{"planner":"architect_long","task":"围绕凡人修仙规划三卷框架","reason":"长篇修仙题材"}`)
		}
		return testTextMsg(`{"action":"abort","reason":"规划师空转,停机"}`)
	}}
	// 规划师成功返回但不落任何盘 → Route 始终返回同一补齐指令 → 僵局。
	architect := subagent.Config{
		Name: "architect_long", Description: "idle planner",
		Model: &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
			return testTextMsg("已规划(其实没有落盘)")
		}},
		SystemPrompt: "test", MaxTurns: 3,
	}
	e, events, done := newTestEngine(t, st, subagent.NewRunner(architect), arb)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	meta, err := st.RunMeta.Load()
	if err != nil || meta == nil || meta.PlanStart == nil {
		t.Fatalf("补裁后 PlanStart 必须固化, meta=%+v err=%v", meta, err)
	}
	if meta.PlanStart.Planner != "architect_long" || meta.PlanStart.RawPrompt != "凡人修仙" || meta.PlanStart.DecisionID == "" {
		t.Fatalf("PlanStartRecord 字段不完整: %+v", meta.PlanStart)
	}
	recs, err := st.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	var planStartRec bool
	for _, r := range recs {
		if r.Kind == "plan_start" && strings.Contains(string(r.Decision), "architect_long") {
			planStartRec = true
		}
	}
	if !planStartRec {
		t.Fatalf("补裁必须留下 plan_start 审计: %+v", recs)
	}
	var dispatched, healed bool
	for _, ev := range *events {
		if ev.Category == "DISPATCH" {
			dispatched = true
		}
		if strings.Contains(ev.Summary, "启动裁定已补齐") {
			healed = true
		}
	}
	if !dispatched || !healed {
		t.Fatalf("补裁后应派发规划师并回显补齐事件, dispatched=%v healed=%v", dispatched, healed)
	}
}

// TestEngine_PlanStartRetryFailurePauses 补裁失败不允许无声停机:
// Arbiter 持续不可用 → 显式暂停回显 + plan_start 审计带 error + 零派发。
func TestEngine_PlanStartRetryFailurePauses(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("", 0); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.RunMeta.SetStartPrompt("凡人修仙"); err != nil {
		t.Fatalf("start prompt: %v", err)
	}

	arb := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		return testTextMsg("这不是 JSON")
	}}
	e, events, done := newTestEngine(t, st, subagent.NewRunner(), arb)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	for _, ev := range *events {
		if ev.Category == "DISPATCH" {
			t.Fatal("补裁失败不得派发任何 worker")
		}
	}
	var paused bool
	for _, ev := range *events {
		if strings.Contains(ev.Summary, "启动裁定失败") {
			paused = true
		}
	}
	if !paused {
		t.Fatalf("补裁失败必须显式回显暂停原因, events=%+v", *events)
	}
	recs, err := st.Decisions.Recent(5)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	var errRec bool
	for _, r := range recs {
		if r.Kind == "plan_start" && r.Error != "" && len(r.Decision) == 0 {
			errRec = true
		}
	}
	if !errRec {
		t.Fatalf("失败裁定必须带 error 落盘: %+v", recs)
	}
}

// TestEngine_DeadlockConsultsArbiter 僵局路径:规划补齐指令连续重现
// → 第 3 次咨询 Arbiter → abort 停机 + deadlock 审计。
func TestEngine_DeadlockConsultsArbiter(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("僵局试书", 3); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// 规划期 + tier 已知 + 缺项恒在 → Route 每轮产出同一补齐指令
	if err := st.RunMeta.SetPlanningTier(domain.PlanningTierLong); err != nil {
		t.Fatalf("tier: %v", err)
	}

	// architect 无守卫、成功返回但不落任何盘 → Route 指令恒定
	lazy := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		return testTextMsg("知道了(什么也不做)")
	}}
	architect := subagent.Config{
		Name: "architect_long", Description: "lazy architect",
		Model: lazy, SystemPrompt: "test", MaxTurns: 5,
	}
	arb := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		return testTextMsg(`{"action":"abort","reason":"规划师反复无产出"}`)
	}}
	e, _, done := newTestEngine(t, st, subagent.NewRunner(architect), arb)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	recs, err := st.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.Kind == "deadlock" && r.Decider == "arbiter" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deadlock 裁定必须落盘: %+v", recs)
	}
}

// TestEngine_IntermediateCheckpointsDoNotMaskDeadlock 锁定 #84：Writer 反复修改
// 草稿会产生新 digest 和新 edit checkpoint，但只要 Route 仍是同一个
// "写第 1 章"，就说明 Engine 级后置条件(commit)未完成，必须继续累计僵局。
func TestEngine_IntermediateCheckpointsDoNotMaskDeadlock(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("#84 回归", 1); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "第一章", CoreEvent: "开端"}}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, "版本0 正文初稿"); err != nil {
		t.Fatalf("draft: %v", err)
	}

	writerModel := &editThenCancelModel{}
	writer := subagent.Config{
		Name: "writer", Description: "edit then cancel writer",
		Model: writerModel, SystemPrompt: "test",
		Tools:    []agentcore.Tool{tools.NewEditChapterTool(st)},
		MaxTurns: 5,
	}
	// 即使 Arbiter 对 worker_failure / deadlock 一直要求 retry，现有第 5 次
	// 硬熔断也必须在派发前截停，不得被 edit checkpoint 重置。
	arb := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		return testTextMsg(`{"action":"retry","reason":"继续重试"}`)
	}}
	e, _, done := newTestEngine(t, st, subagent.NewRunner(writer), arb)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	if got := writerModel.edits.Load(); got != deadlockAbortAt-1 {
		t.Fatalf("deadlock 应在第 %d 次派发前硬熔断，实际 edit %d 次", deadlockAbortAt, got)
	}
	var edits int
	for _, cp := range st.Checkpoints.All() {
		if cp.Scope.Matches(domain.ChapterScope(1)) && cp.Step == "edit" {
			edits++
		}
	}
	if edits != deadlockAbortAt-1 {
		t.Fatalf("应保留 %d 条不同的 edit checkpoint，实际 %d", deadlockAbortAt-1, edits)
	}
	recs, err := st.Decisions.Recent(10)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	var hasWorkerFailure, hasDeadlock bool
	for _, rec := range recs {
		switch rec.Kind {
		case "worker_failure":
			hasWorkerFailure = true
		case "deadlock":
			hasDeadlock = true
		}
	}
	if !hasWorkerFailure || !hasDeadlock {
		t.Fatalf("应先记录 worker_failure 再记录 deadlock: %+v", recs)
	}
}

// TestEngine_PauseWithEditorDispatchWaitsForRewriteQueue 修复验证(评审阻断2):
// Arbiter 返工裁定 = 停靠点 + 派 editor 入队。停靠点必须等 editor 建立返工队列、
// writer 重写排空之后才消费——不能在 editor 执行前被"队列已排空"误判消费。
func TestEngine_PauseWithEditorDispatchWaitsForRewriteQueue(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("返工试书", 3); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a"},
		{Chapter: 2, Title: "二", CoreEvent: "b"},
		{Chapter: 3, Title: "三", CoreEvent: "c"},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	// 第 1 章已完成(将被返工);writer worker 会先重写它,然后停靠点消费。
	if err := st.Progress.StartChapter(1); err != nil {
		t.Fatalf("start ch1: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1200, "crisis", "quest"); err != nil {
		t.Fatalf("complete ch1: %v", err)
	}

	// editor:一次 save_review(verdict=rewrite, affected=[1]) 把第 1 章入队。
	editorModel := &scriptedChatModel{fn: func(msgs []agentcore.Message) agentcore.Message {
		toolResults := 0
		for _, m := range msgs {
			if m.Role == agentcore.RoleTool {
				toolResults++
			}
		}
		if toolResults == 0 {
			return testToolCallMsg("save_review", map[string]any{
				"chapter": 1, "scope": "chapter",
				"dimensions": []map[string]any{
					{"dimension": "consistency", "score": 85, "comment": "达标(引用:原文)"},
					{"dimension": "character", "score": 85, "comment": "达标(引用:原文)"},
					{"dimension": "pacing", "score": 85, "comment": "达标(引用:原文)"},
					{"dimension": "continuity", "score": 85, "comment": "达标(引用:原文)"},
					{"dimension": "foreshadow", "score": 85, "comment": "达标(引用:原文)"},
					{"dimension": "hook", "score": 85, "comment": "达标(引用:原文)"},
					{"dimension": "aesthetic", "score": 55, "comment": "语气不符(引用:原文第一段)"},
				},
				"issues":  []map[string]any{{"type": "aesthetic", "severity": "error", "description": "语气", "evidence": "原文", "suggestion": "改冷"}},
				"verdict": "rewrite", "summary": "第1章语气需重写",
				"affected_chapters": []int{1},
			})
		}
		return testTextMsg("done")
	}}
	editor := subagent.Config{
		Name: "editor", Description: "test editor", Model: editorModel,
		SystemPrompt: "test", MaxTurns: 6,
		Tools:          []agentcore.Tool{tools.NewSaveReviewTool(st)},
		StopAfterTools: []string{"save_review"},
	}
	writer := subagent.Config{
		Name: "writer", Description: "test writer", Model: scriptedWriterModel(),
		SystemPrompt: "test",
		Tools: []agentcore.Tool{
			tools.NewPlanChapterTool(st, nil),
			tools.NewDraftChapterTool(st, nil),
			tools.NewCheckConsistencyTool(st),
			tools.NewCommitChapterTool(st),
		},
		MaxTurns: 10, StopAfterTools: []string{"commit_chapter"},
	}

	e, _, done := newTestEngine(t, st, subagent.NewRunner(editor, writer), nil)
	// 模拟 Arbiter 返工裁定:hold + dispatch editor(引擎未运行 → 立即应用)。
	e.applyControlOp(context.Background(), controlOp{
		hold:     &arbiter.AdvanceHoldOp{After: domain.AdvanceHoldAfterRewritesDrained, Reason: "重写第1章语气,改完暂停验收"},
		dispatch: &arbiter.DispatchOp{Agent: "editor", Task: "复核第 1 章:语气改冷,save_review(verdict=rewrite, affected_chapters=[1])"},
		facts:    arbiter.CollectInterventionFacts(st),
	})
	if !e.start(nil) {
		t.Fatal("engine start")
	}
	waitEngineDone(t, done)

	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		t.Fatalf("load progress: %v", err)
	}
	// 核心断言①:停靠点没有在 editor 入队前消费——第 1 章确实经历了重写
	//(重写 commit 会把它从队列 drain 掉)。
	if len(progress.PendingRewrites) != 0 {
		t.Fatalf("返工队列应已排空, got %v", progress.PendingRewrites)
	}
	if progress.ChapterWordCounts[1] == 1200 {
		t.Fatal("第 1 章应被真实重写(字数应变化)")
	}
	// 核心断言②:排空后停靠点消费,引擎暂停——第 2 章不应被续写。
	if len(progress.CompletedChapters) != 1 {
		t.Fatalf("停靠点应在续写第 2 章前暂停, completed=%v", progress.CompletedChapters)
	}
	meta, _ := st.RunMeta.Load()
	if meta != nil && meta.AdvanceHold != nil {
		t.Fatalf("一次性暂停应已消费, got %+v", meta.AdvanceHold)
	}
}

// TestEngine_BoundaryHoldDoesNotDispatchAnotherWorker 回归：
// 用户干预只裁定出 boundary hold（无派单）时，引擎必须在当前边界立即
// 消费 hold 并暂停，不得再多写一章。
func TestEngine_BoundaryHoldDoesNotDispatchAnotherWorker(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("暂停试书", 3); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a"},
		{Chapter: 2, Title: "二", CoreEvent: "b"},
		{Chapter: 3, Title: "三", CoreEvent: "c"},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}

	writer := subagent.Config{
		Name: "writer", Description: "test writer", Model: scriptedWriterModel(),
		SystemPrompt: "test",
		Tools: []agentcore.Tool{
			tools.NewPlanChapterTool(st, nil),
			tools.NewDraftChapterTool(st, nil),
			tools.NewCheckConsistencyTool(st),
			tools.NewCommitChapterTool(st),
		},
		MaxTurns: 10, StopAfterTools: []string{"commit_chapter"},
	}
	e, _, done := newTestEngine(t, st, subagent.NewRunner(writer), nil)
	if !e.start(nil) {
		t.Fatal("engine start")
	}
	// 第 1 章写作期间到达 hold-only 干预（与真实 Steer 时序一致）。
	e.enqueue(controlOp{
		hold:  &arbiter.AdvanceHoldOp{After: domain.AdvanceHoldAtBoundary, Reason: "先停一下我看看"},
		facts: arbiter.CollectInterventionFacts(st),
	})
	waitEngineDone(t, done)

	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		t.Fatalf("load progress: %v", err)
	}
	// 干预在第 1 章运行中到达 → 第 1 章写完;停靠点在边界立即消费 → 第 2 章不得开写。
	if n := len(progress.CompletedChapters); n > 1 {
		t.Fatalf("boundary hold 后不得再多写一章, completed=%v", progress.CompletedChapters)
	}
	meta, _ := st.RunMeta.Load()
	if meta != nil && meta.AdvanceHold != nil {
		t.Fatalf("一次性暂停应已消费, got %+v", meta.AdvanceHold)
	}
}

// TestEngine_ExitRaceRestoresPendingDispatch 回归(评审阻断3):
// 干预入队与引擎退出竞态时,残留的裁定派单不得无声丢弃——PendingSteer 必须回存,
// pause 类事实动作必须补执行。
func TestEngine_ExitRaceRestoresPendingDispatch(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("竞态试书", 2); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}

	// worker 挂起直到 ctx 取消:制造"入队后引擎被 abort"的窗口。
	blocked := &scriptedChatModel{fn: func([]agentcore.Message) agentcore.Message {
		time.Sleep(50 * time.Millisecond)
		return testTextMsg("...")
	}}
	writer := subagent.Config{Name: "writer", Description: "slow", Model: blocked, SystemPrompt: "t", MaxTurns: 100}
	// 需要 outline 让 Route 派 writer
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "一", CoreEvent: "a"}, {Chapter: 2, Title: "二", CoreEvent: "b"}}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	e, _, done := newTestEngine(t, st, subagent.NewRunner(writer), nil)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	// worker 运行中:入队 pause+dispatch,随即 abort(动作永远等不到下个边界)。
	e.enqueue(controlOp{
		hold:     &arbiter.AdvanceHoldOp{After: domain.AdvanceHoldAfterRewritesDrained, Reason: "验收"},
		dispatch: &arbiter.DispatchOp{Agent: "writer", Task: "重写第 1 章"},
		text:     "重写第1章然后停下来",
		facts:    arbiter.CollectInterventionFacts(st),
	})
	e.abort()
	waitEngineDone(t, done)

	meta, err := st.RunMeta.Load()
	if err != nil || meta == nil {
		t.Fatalf("load meta: %v", err)
	}
	if meta.PendingSteer != "重写第1章然后停下来" {
		t.Fatalf("残留派单必须回存 PendingSteer 供恢复重放, got %q", meta.PendingSteer)
	}
	if meta.AdvanceHold == nil {
		t.Fatal("hold 事实动作应在退出清理中补执行")
	}
}

// ── Architect / Writer precheck tests (Phase 2 acceptance blocker 2) ──

// newMinimalEngineForPrecheck 创建一个带真实 store 和 V3 contract 的最小 engine，
// 专用于 precheck 测试。observer 全 mock，验证零 provider/worker 调用。
func newMinimalEngineForPrecheck(t *testing.T, st *storepkg.Store) *engine {
	t.Helper()
	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatalf("run meta init: %v", err)
	}
	contract := projectprofile.NewSceneBeatV3Contract()
	var events []Event
	e := &engine{
		store:     st,
		contract:  contract,
		observer:  newObserver(st, func(ev Event) { events = append(events, ev) }, func(string) {}, func() {}),
		emitEvent: func(ev Event) { events = append(events, ev) },
		notify:    func(string, string, string, string) {},
		onPause:   func(string) {},
		onDone:    func() {},
		refresh:   func() {},
	}
	e.gate = NewChapterAdvanceGate(st, func(string) { e.abort() }, func(string, string) {})
	return e
}

// TestEnginePrecheck_Architect_AbsentTargetAllowed 验证 Architect preflight 在目标章
// 不在 outline 中时允许通过（零 observer/worker/provider 调用）。
// 构造方式：前 3 章已写完，NextChapter=4，但 outline 只有 1-3 → 4 不存在。
func TestEnginePrecheck_Architect_AbsentTargetAllowed(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("test", 4); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// 只展开 1-3 章，第 4 章不在 outline 中（骨架弧）
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
		{Chapter: 2, Title: "二", CoreEvent: "b", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
		{Chapter: 3, Title: "三", CoreEvent: "c", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	// 标记 1-3 已完成 → NextChapter=4
	for ch := 1; ch <= 3; ch++ {
		if err := st.Progress.MarkChapterComplete(ch, 1000, "", ""); err != nil {
			t.Fatalf("mark ch%d: %v", ch, err)
		}
	}

	e := newMinimalEngineForPrecheck(t, st)
	inst := &flow.Instruction{Agent: "architect_long", Task: "展开第4章", Reason: "下一章骨架展开"}

	replaced := e.precheck(inst)

	// 第 4 章不在 outline 中 → Architect 应通过（允许创建）
	if replaced != nil {
		t.Fatalf("architect with absent target should pass precheck, got replaced=%+v", replaced)
	}
}

// TestEnginePrecheck_Architect_ExistingInvalidTargetPauses 验证 Architect preflight
// 在目标章已存在但场景不符合 V3 契约时暂停（零 provider/worker 调用）。
func TestEnginePrecheck_Architect_ExistingInvalidTargetPauses(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// 用 scenes 为空模拟无效目标章（V3 拒绝空 scenes）
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a"},
		{Chapter: 2, Title: "二", CoreEvent: "b", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
		{Chapter: 3, Title: "三", CoreEvent: "c", Scenes: []domain.SceneBeat{}}, // V3: empty scenes
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	// 标记第 2 章已完成，NextChapter 回到 3
	if err := st.Progress.MarkChapterComplete(1, 1000, "", ""); err != nil {
		t.Fatalf("mark ch1: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(2, 1000, "", ""); err != nil {
		t.Fatalf("mark ch2: %v", err)
	}

	e := newMinimalEngineForPrecheck(t, st)
	inst := &flow.Instruction{Agent: "architect_long", Task: "处理第3章", Reason: "重写"}

	replaced := e.precheck(inst)

	// 应返回空 instruction（pause），因为第 3 章无 scenes
	if replaced == nil {
		t.Fatal("architect with existing invalid target should pause, got nil")
	}
	if replaced.Agent != "" || replaced.Task != "" {
		t.Fatalf("pause should return empty instruction, got %+v", replaced)
	}
}

// TestEnginePrecheck_Writer_AbsentTargetPauses 验证 Writer preflight 在目标章
// 不在 outline 中时硬暂停（V3 要求所有目标章必须存在于 outline 中；
// 零 provider/worker 调用）。
func TestEnginePrecheck_Writer_AbsentTargetPauses(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("test", 2); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	// Outline 无第 2 章（只有骨架弧预计 2 章但只展开 1）
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1000, "", ""); err != nil {
		t.Fatalf("mark ch1: %v", err)
	}

	e := newMinimalEngineForPrecheck(t, st)
	inst := &flow.Instruction{Agent: "writer", Task: "写第2章", Reason: "续写"}

	replaced := e.precheck(inst)

	// Writer 目标章不在 outline → V3 硬暂停（空 instr = pause）
	if replaced == nil {
		t.Fatal("writer with absent target in V3 should pause (got nil)")
	}
	if replaced.Agent != "" || replaced.Task != "" {
		t.Fatalf("writer absent target should pause (empty instr), got %+v", replaced)
	}
	// 不消耗 observer/worker/provider
}

// TestEnginePrecheck_Writer_ExistingInvalidTargetPauses 验证 Writer preflight
// 在目标章场景违反 V3 契约时暂停（零 provider/worker 调用）。
func TestEnginePrecheck_Writer_ExistingInvalidTargetPauses(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// 第 1 章已写完，NextChapter=2；第 2 章只有空 scenes
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
		{Chapter: 2, Title: "二", CoreEvent: "b", Scenes: []domain.SceneBeat{}}, // V3: empty scenes
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1000, "", ""); err != nil {
		t.Fatalf("mark ch1: %v", err)
	}

	e := newMinimalEngineForPrecheck(t, st)
	inst := &flow.Instruction{Agent: "writer", Task: "写第2章", Reason: "续写"}

	replaced := e.precheck(inst)

	if replaced == nil {
		t.Fatal("writer with existing invalid target should pause, got nil")
	}
	if replaced.Agent != "" || replaced.Task != "" {
		t.Fatalf("pause should return empty instruction, got %+v", replaced)
	}
}

// TestEngineV3Writer_MissingTargetPausesNoDispatch 真实 run-loop 测试：
// V3 契约下 Writer 目标章不在 outline 中时引擎立即暂停，零 dispatch/零 provider/零写。
func TestEngineV3Writer_MissingTargetPausesNoDispatch(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("测试书", 2); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("phase: %v", err)
	}
	// Only chapter 1 in outline; chapter 2 is missing → Writer target absent
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1000, "", ""); err != nil {
		t.Fatalf("mark ch1: %v", err)
	}

	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatalf("run meta: %v", err)
	}

	// V3 contract
	contract := projectprofile.NewSceneBeatV3Contract()

	// 无实际 worker（不应被调用）
	noopWorker := subagent.Config{
		Name: "writer", Description: "noop",
		Model: &scriptedChatModel{fn: func(msgs []agentcore.Message) agentcore.Message {
			t.Error("worker 不应被调用")
			return testTextMsg("")
		}},
		SystemPrompt: "test", MaxTurns: 1,
	}

	var mu sync.Mutex
	var events []Event
	done := make(chan struct{}, 1)
	abortFn := func() {}
	e := &engine{
		store:    st,
		workers:  subagent.NewRunner(noopWorker),
		contract: contract,
		observer: newObserver(st, func(ev Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}, func(string) {}, func() {}),
		emitEvent: func(ev Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
		notify:  func(string, string, string, string) {},
		onPause: func(string) { abortFn() },
		onDone: func() {
			select {
			case done <- struct{}{}:
			default:
			}
		},
		refresh: func() {},
	}
	abortFn = func() { e.abort() }
	e.gate = NewChapterAdvanceGate(st, func(string) { e.abort() }, func(string, string) {})

	// Start engine
	if !e.start(nil) {
		t.Fatal("engine start")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not pause in time")
	}

	mu.Lock()
	defer mu.Unlock()

	// 断言零 DISPATCH 事件（无 worker 调用）
	for _, ev := range events {
		if ev.Category == "DISPATCH" {
			t.Errorf("engine should not dispatch any worker when writer target missing, got DISPATCH: %+v", ev)
		}
	}

	// 断言零新 checkpoint
	if cps := st.Checkpoints.All(); len(cps) > 0 {
		t.Errorf("zero checkpoints expected, got %d", len(cps))
	}
}

// TestEnginePrecheck_Architect_ExistingExpandedOutlineValid 验证 Architect preflight
// 在目标章已存在且场景符合 V3 契约时允许通过（零 pause）。
func TestEnginePrecheck_Architect_ExistingExpandedOutlineValid(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "一", CoreEvent: "a"},
		{Chapter: 2, Title: "二", CoreEvent: "b", Scenes: []domain.SceneBeat{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o", BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec"},
		}},
		{Chapter: 3, Title: "三", CoreEvent: "c", Scenes: []domain.SceneBeat{
			{Goal: "g3", Action: "a3", Conflict: "c3", Outcome: "o3", BodyReaction: "b3", EmotionReaction: "e3", EroticCharge: "ec3"},
		}},
	}); err != nil {
		t.Fatalf("outline: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 1000, "", ""); err != nil {
		t.Fatalf("mark ch1: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(2, 1000, "", ""); err != nil {
		t.Fatalf("mark ch2: %v", err)
	}

	e := newMinimalEngineForPrecheck(t, st)
	// NextChapter=3, outline 有第3章且 scenes 符合 V3 → 应通过
	inst := &flow.Instruction{Agent: "architect_long", Task: "展开弧", Reason: "测试"}

	replaced := e.precheck(inst)

	// 第3章在 outline 中且有合法 scenes → V3 应通过（nil = 不改写）
	if replaced != nil {
		t.Fatalf("architect with valid existing entry should pass precheck, got replaced=%+v", replaced)
	}
}

func TestEnginePrecheckV3_TargetAgentsFailClosedOnMissingOrCorruptProgress(t *testing.T) {
	for _, agent := range []string{"writer", "architect_long"} {
		for _, state := range []string{"missing", "corrupt"} {
			t.Run(agent+"/"+state, func(t *testing.T) {
				dir := t.TempDir()
				st := storepkg.NewStore(dir)
				if err := st.Init(); err != nil {
					t.Fatalf("init: %v", err)
				}
				e := newMinimalEngineForPrecheck(t, st)
				if state == "corrupt" {
					if err := os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{broken`), 0o644); err != nil {
						t.Fatalf("write corrupt progress: %v", err)
					}
				}
				before := takeStoreSnapshot(dir)

				replaced := e.precheck(&flow.Instruction{Agent: agent, Task: "test"})
				if replaced == nil || replaced.Agent != "" || replaced.Task != "" {
					t.Fatalf("v3 %s with %s progress must hard pause, got %+v", agent, state, replaced)
				}
				assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
			})
		}
	}
}

// ── Exhausted style review ledger blocks writer dispatch ───────────

// exhaustedTestStore creates a store where chapter is in critic mode with
// the given ledger (exhausted or overridden).  The chapter is active (not
// completed) so that precheck's non-V3 writerTargetChapter resolves to it.
func exhaustedTestStore(t *testing.T, chapter int, ledger *domain.StyleReviewLedger) *storepkg.Store {
	t.Helper()
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: chapter, Title: "测试章", CoreEvent: "a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.StartChapter(chapter); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.StyleReview.Save(*ledger); err != nil {
		t.Fatal(err)
	}
	return st
}

// exhaustedLedger builds a V1-style exhausted ledger for testing.
func exhaustedLedger(chapter int) *domain.StyleReviewLedger {
	d := domain.DigestDraft("content")
	find := []domain.StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Evidence: "e"}}
	return &domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: chapter, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
				AttemptID: "a1", Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"}, DraftDigest: d, BasisDigest: d},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"},
				Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e", Findings: find},
				DraftDigest: d, BasisDigest: d},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &domain.StyleReviewRequest{Prompt: "final", Model: "m"}, DraftDigest: d, BasisDigest: d},
			{Cycle: 4, Status: domain.ReviewStatusExhausted, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &domain.StyleReviewRequest{Prompt: "final", Model: "m"},
				Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "e", Findings: find},
				DraftDigest: d, BasisDigest: d},
		},
	}
}

func TestEngine_ExhaustedLedgerNoWriterDispatch(t *testing.T) {
	const ch = 1
	st := exhaustedTestStore(t, ch, exhaustedLedger(ch))

	var workerCalls int32
	e := &engine{
		store:           st,
		workers:         subagent.NewRunner(),
		notify:          func(_, _, _, _ string) {},
		emitEvent:       func(Event) {},
		onPause:         func(string) {},
		onDone:          func() {},
		beforeRunWorker: func() { atomic.AddInt32(&workerCalls, 1) },
	}
	e.gate = NewChapterAdvanceGate(st, func(string) {}, func(string, string) {})
	// The writer will target ch via writerTargetChapter (NextChapter).
	e.next = nil

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.done = done
	e.running = true
	e.cancel = cancel
	go e.run(ctx)
	<-done

	if n := atomic.LoadInt32(&workerCalls); n != 0 {
		t.Fatalf("runWorker called %d times, expected 0 (exhausted should block dispatch)", n)
	}
}

func TestEngine_ExhaustedLedgerLoadFailurePauses(t *testing.T) {
	const ch = 1
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: ch, Title: "测试章", CoreEvent: "a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.StartChapter(ch); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	// Write corrupt ledger directly
	ledgerDir := filepath.Join(st.Dir(), "meta", "style_review")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ledgerDir, "01.json"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}

	var paused atomic.Int32
	e := &engine{
		store:     st,
		workers:   subagent.NewRunner(),
		notify:    func(_, _, _, _ string) {},
		emitEvent: func(Event) {},
		onDone:    func() {},
	}
	e.gate = NewChapterAdvanceGate(st, func(string) {}, func(string, string) {})
	// Explicitly set next to writer for chapter 1 — bypass Route.
	e.next = &flow.Instruction{Agent: "writer", Task: "写第 1 章", Chapter: 1}
	e.onPause = func(string) { paused.Add(1); e.abort() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.done = done
	e.running = true
	e.cancel = cancel
	go e.run(ctx)
	<-done

	if n := paused.Load(); n == 0 {
		t.Fatalf("expected engine to pause on corrupt style review ledger, but it did not (paused=%d)", n)
	}
}

func TestEngine_ExhaustedLedgerOverrideRecovers(t *testing.T) {
	const ch = 1
	st := exhaustedTestStore(t, ch, exhaustedLedger(ch))

	// Override the exhausted ledger
	d := domain.DigestDraft("content")
	now := time.Now().Format(time.RFC3339)
	if err := st.StyleReview.Update(ch, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle:       5,
			Status:      domain.ReviewStatusOverridden,
			CreatedAt:   now,
			DraftDigest: d,
			BasisDigest: d,
			Override: &domain.StyleReviewOverride{
				Actor: "user", Reason: "I confirm this draft", DraftDigest: d, BasisDigest: d, OverriddenAt: now,
			},
		})
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Also need a consistency checkpoint for the writer to make progress
	if _, err := st.Checkpoints.Append(domain.ChapterScope(ch), "consistency_check", "a", d); err != nil {
		t.Fatal(err)
	}
	// Initialize RunMeta so AdvanceMode is auto (not empty/invalid)
	if err := st.RunMeta.Init("default", "test", "test"); err != nil {
		t.Fatal(err)
	}
	// Re-set style review mode (Init preserves advance mode but clears style mode)
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}

	var workerCalls int32
	var paused atomic.Int32
	// Use a Runner with a single writer agent that returns success
	idleWriter := &scriptedChatModel{fn: func(msgs []agentcore.Message) agentcore.Message {
		return testTextMsg("done")
	}}
	writerCfg := subagent.Config{
		Name: "writer", Description: "idle", Model: idleWriter,
		SystemPrompt: "test", MaxTurns: 1,
	}
	obs := newObserver(st, func(Event) {}, func(string) {}, func() {})
	e := &engine{
		store:           st,
		workers:         subagent.NewRunner(writerCfg),
		observer:        obs,
		notify:          func(_, _, _, _ string) {},
		emitEvent:       func(Event) {},
		onDone:          func() {},
		refresh:         func() {},
		beforeRunWorker: func() { atomic.AddInt32(&workerCalls, 1) },
	}
	e.gate = NewChapterAdvanceGate(st, func(string) {}, func(string, string) {})
	// Explicitly set next to writer for chapter 1 — bypass Route.
	e.next = &flow.Instruction{Agent: "writer", Task: "写第 1 章", Chapter: 1}
	e.onPause = func(string) { paused.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	// Stop engine after a short delay to check workerCalls
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan struct{})
	e.done = done
	e.running = true
	e.cancel = cancel
	go e.run(ctx)
	<-done

	if n := atomic.LoadInt32(&workerCalls); n == 0 {
		t.Fatal("expected writer to be dispatched after override, but it was not")
	}
}
