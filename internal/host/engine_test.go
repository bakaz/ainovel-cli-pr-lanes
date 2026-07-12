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
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

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
				"content": strings.Repeat(fmt.Sprintf("第%d章的正文段落，主角在黑暗中摸索前行。", chapter), 20),
			})
		case 2:
			return testToolCallMsg("check_consistency", map[string]any{"chapter": chapter})
		default:
			return testToolCallMsg("commit_chapter", map[string]any{
				"chapter": chapter, "summary": fmt.Sprintf("第%d章摘要", chapter),
				"characters": []string{"主角"}, "key_events": []string{"推进"},
				"hook_type": "crisis",
			})
		}
	}}
}

// newTestEngine 组装带真实 store/observer 的引擎;返回引擎、事件采集与完成信号。
func newTestEngine(t *testing.T, st *storepkg.Store, workers *subagent.Tool, arbiterModel agentcore.ChatModel) (*engine, *[]Event, chan struct{}) {
	t.Helper()
	var mu sync.Mutex
	events := &[]Event{}
	done := make(chan struct{}, 1)
	obs := newObserver(st, func(ev Event) {
		mu.Lock()
		*events = append(*events, ev)
		mu.Unlock()
	}, func(string) {}, func() {})
	e := &engine{
		store:         st,
		workers:       workers,
		arbiterModel:  arbiterModel,
		failurePrompt: "sys",
		observer:      obs,
		refresh:       func() {},
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
			tools.NewPlanChapterTool(st),
			tools.NewDraftChapterTool(st),
			tools.NewCheckConsistencyTool(st),
			tools.NewCommitChapterTool(st),
		},
		MaxTurns:       10,
		StopAfterTools: []string{"commit_chapter"},
	}
	e, events, done := newTestEngine(t, st, subagent.New(writer), nil)

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
	e, _, done := newTestEngine(t, st, subagent.New(writer), arb)

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

// TestEngine_DeadlockConsultsArbiter 僵局路径:规划补齐指令反复执行无 checkpoint
// 推进 → 第 3 次咨询 Arbiter → abort 停机 + deadlock 审计。
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

	// architect 无守卫、成功返回但不落任何盘 → checkpoint 恒无推进
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
	e, _, done := newTestEngine(t, st, subagent.New(architect), arb)

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
				"issues":  []map[string]any{{"severity": "major", "description": "语气", "evidence": "原文", "suggestion": "改冷"}},
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
			tools.NewPlanChapterTool(st),
			tools.NewDraftChapterTool(st),
			tools.NewCheckConsistencyTool(st),
			tools.NewCommitChapterTool(st),
		},
		MaxTurns: 10, StopAfterTools: []string{"commit_chapter"},
	}

	e, _, done := newTestEngine(t, st, subagent.New(editor, writer), nil)
	// 停靠点哨兵:消费即 abort(与 Host 接线同构)。
	e.pauser = NewPausePointSentinel(st, func(reason string) { e.abort() }, func(string, string) {})

	// 模拟 Arbiter 返工裁定:pause + dispatch editor(引擎未运行 → 立即应用)。
	e.applyControlOp(context.Background(), controlOp{
		pause:    &arbiter.PauseOp{Reason: "重写第1章语气,改完暂停验收"},
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
	if meta != nil && meta.PausePoint != nil {
		t.Fatalf("停靠点应已消费, got %+v", meta.PausePoint)
	}
}

// TestEngine_PauseOnlyDoesNotDispatchAnotherWorker 回归(评审阻断2的反面):
// 用户干预只裁定出停靠点(无派单/返工队列为空)时,引擎必须在当前边界立即
// 消费停靠点暂停,不得再多写一章。
func TestEngine_PauseOnlyDoesNotDispatchAnotherWorker(t *testing.T) {
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
			tools.NewPlanChapterTool(st),
			tools.NewDraftChapterTool(st),
			tools.NewCheckConsistencyTool(st),
			tools.NewCommitChapterTool(st),
		},
		MaxTurns: 10, StopAfterTools: []string{"commit_chapter"},
	}
	e, _, done := newTestEngine(t, st, subagent.New(writer), nil)
	e.pauser = NewPausePointSentinel(st, func(reason string) { e.abort() }, func(string, string) {})

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	// 第 1 章写作期间到达 pause-only 干预(与真实 Steer 时序一致)。
	e.enqueue(controlOp{
		pause: &arbiter.PauseOp{Reason: "先停一下我看看"},
		facts: arbiter.CollectInterventionFacts(st),
	})
	waitEngineDone(t, done)

	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		t.Fatalf("load progress: %v", err)
	}
	// 干预在第 1 章运行中到达 → 第 1 章写完;停靠点在边界立即消费 → 第 2 章不得开写。
	if n := len(progress.CompletedChapters); n > 1 {
		t.Fatalf("pause-only 后不得再多写一章, completed=%v", progress.CompletedChapters)
	}
	meta, _ := st.RunMeta.Load()
	if meta != nil && meta.PausePoint != nil {
		t.Fatalf("停靠点应已消费, got %+v", meta.PausePoint)
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
	e, _, done := newTestEngine(t, st, subagent.New(writer), nil)

	if !e.start(nil) {
		t.Fatal("engine start")
	}
	// worker 运行中:入队 pause+dispatch,随即 abort(动作永远等不到下个边界)。
	e.enqueue(controlOp{
		pause:    &arbiter.PauseOp{Reason: "验收"},
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
	if meta.PausePoint == nil {
		t.Fatal("pause 事实动作应在退出清理中补执行")
	}
}
