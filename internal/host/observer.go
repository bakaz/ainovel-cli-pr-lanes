package host

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errclass"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"sync/atomic"
)

// errorKind classifies a runtime error into a stable, short label for log
// filtering and alert routing. Returns "" when no special tag applies.
//
// err is the live error chain (may be nil after JSON serialization); msg is
// the rendered string fallback used when the chain has been flattened
// (e.g. inside sub-agent JSON results).
//
// Classification priority:
//  1. Exact error chain match (errors.Is) — but args-malformed overrides
//     ErrToolValidation when msg indicates a JSON parse failure.
//  2. Message pattern match (errclass.ClassifyMsg).
//
// Provider-level sentinel checks (ErrProviderStreamIdle) live here because they
// rely on agentcore types that errclass does not import.
func errorKind(err error, msg string) string {
	// ---- Exact error chain match (most reliable) ----
	if err != nil {
		switch {
		case errors.Is(err, agentcore.ErrMaxTurns):
			return errclass.CatMaxTurns
		case errors.Is(err, agentcore.ErrToolValidation):
			// ErrToolValidation wraps both schema violations and JSON parse
			// failures. When msg indicates args-malformed, prefer that category
			// so malformed args are never generalized as schema validation.
			if msg != "" {
				if cat := errclass.ClassifyMsg(msg); cat == errclass.CatToolArgsMalformed {
					return cat
				}
			}
			return errclass.CatToolSchemaValidation
		case errors.Is(err, agentcore.ErrProviderStreamIdle):
			return errclass.CatStreamIdle
		}
	}
	// ---- Fall back to shared message pattern matching ----
	if msg != "" {
		return errclass.ClassifyMsg(msg)
	}
	return ""
}

// 单调递增的事件 ID 计数器；配合时间戳生成稳定 ID。
var eventIDCounter uint64

func nextEventID() string {
	return fmt.Sprintf("e%d", atomic.AddUint64(&eventIDCounter, 1))
}

// activeCall 记录一次正在进行的调用（TOOL / DISPATCH）的 ID、起点时间与 summary。
// summary 在完成事件时回填进 finish Event，保证 replay（runtime queue）能还原行内容。
type activeCall struct {
	id      string
	start   time.Time
	summary string
	depth   int
}

// observer 把 Engine 派发与 Worker 进度投影到 Host 的输出通道。
// 它是纯观察者,不参与任何控制决策。
//
// 并发安全：agentMu 保护 agents 快照（TUI 侧栏），mapMu 保护以下所有 map 以及
// stream 原子状态。handleToolUpdate 从 engine goroutine 与 Arbiter goroutine
// 两处进入，因此所有 map 访问必须持 mapMu。
type observer struct {
	emitEv  func(Event)
	emitD   func(string)
	emitC   func()
	store   *storepkg.Store // 用于 runtime queue 持久化（ReplayQueue 消费）
	agents  map[string]*agentState
	agentMu sync.Mutex

	// mapMu 保护以下所有 map 字段及 stream 原子状态。
	mapMu sync.Mutex

	// aborting 由 Host 在 Abort()/Close() 入口置位、Start/Resume/Continue 清位。
	// 置位期间所有 context-cancel 衍生的错误事件被抑制（既是用户期望，也避免与
	// "用户手动暂停"事件重复）。真实异常（非 cancel）仍照常上报。
	aborting atomic.Bool

	streamThinking      bool
	lastThinkingByAgent map[string]string          // agent → 最近的累积 thinking 文本（用于提取增量 delta）
	dispatchStarts      map[string]*activeCall     // dispatched agent → 进行中的 DISPATCH 调用
	toolStarts          map[string]*activeCall     // agent → 进行中的 TOOL 调用
	streamExtractors    map[string]*agentExtractor // agent → 当前工具调用 JSON 参数的内容抽取器
	streamArgPrefixes   map[string]string          // agent/tool → 参数流前缀，用于提前识别轻量标签
	streamArgLabels     map[string]string          // agent/tool → 已从参数流提前识别出的展示名
	retryEvents         map[string]string          // retry scope → event ID，用同一行原地更新 (2/7)
	streamHasContent    bool                       // 当前 streamRound 是否已输出过内容（判断是否需要段落分隔）
	streamLastByte      byte                       // 最近一次流式输出的末字节（用于精确补齐换行）

	// streamOwner 记录当前 stream round 属于哪个 agent。
	// retry 只有 owner 才允许 CLEAR/reset 全局 stream，避免 Arbiter retry
	// 截断 Worker 正在输出的流式内容。
	streamOwner string
}

// agentExtractor 记录某个 agent 当前正在抽取的工具名与抽取器实例。
// 工具名用于检测"新的工具调用开始了"，避免缓存被上一轮残留污染。
type agentExtractor struct {
	tool       string
	ext        *jsonFieldExtractor
	emittedAny bool // 本 extractor 是否已经产出过内容；用于首次输出前补段落分隔
}

type agentState struct {
	name           string
	state          string
	tool           string
	summary        string
	turn           int
	taskKind       string
	context        AgentContextSnapshot
	updated        time.Time
	lastLogPercent float64
}

func newObserver(s *storepkg.Store, emitEv func(Event), emitD func(string), emitC func()) *observer {
	return &observer{
		emitEv:              emitEv,
		emitD:               emitD,
		emitC:               emitC,
		store:               s,
		agents:              make(map[string]*agentState),
		lastThinkingByAgent: make(map[string]string),
		dispatchStarts:      make(map[string]*activeCall),
		toolStarts:          make(map[string]*activeCall),
		streamExtractors:    make(map[string]*agentExtractor),
		streamArgPrefixes:   make(map[string]string),
		streamArgLabels:     make(map[string]string),
		retryEvents:         make(map[string]string),
	}
}

// ── Engine 直驱入口 ──
//
// Engine 直接运行 Worker，事件来源分为两条:
//  1. dispatchStart/dispatchFinish —— Engine 在派发边界直接调用(DISPATCH 行)
//  2. workerProgress —— Worker 的进度中继(ctx ToolProgress)，
//     由 handleToolUpdate 统一处理 TOOL/流式正文/thinking/retry/context
//     (TOOL 行/流式正文/thinking/retry/context)。

// dispatchStart 记录一次 Worker 派发开始并发 DISPATCH 行。
func (o *observer) dispatchStart(agent, task string) {
	summary := dispatchSummary(agent, task)
	o.updateAgent(agent, func(a *agentState) {
		a.state = "working"
		a.tool = ""
		a.taskKind = o.taskKindForDispatch(agent)
		a.summary = fmt.Sprintf("engine → %s", summary)
	})
	id := nextEventID()
	o.mapMu.Lock()
	o.dispatchStarts[agent] = &activeCall{id: id, start: time.Now(), summary: summary}
	o.streamOwner = agent
	o.mapMu.Unlock()
	o.emitAndLog(Event{
		ID:       id,
		Time:     time.Now(),
		Category: "DISPATCH",
		Agent:    "engine",
		Summary:  summary,
		Level:    "info",
	})
}

// taskKindForDispatch 按 agent + 当前进度 flow 映射 AgentSnapshot.TaskKind
// （B4）。flow 来源为 store.Progress.Load() 的 .Flow；store 为 nil 或读取
// 失败时按空 flow 处理（非 writer/editor 的 agent 不依赖 flow）。
func (o *observer) taskKindForDispatch(agent string) string {
	flow := ""
	if o.store != nil && o.store.Progress != nil {
		if p, err := o.store.Progress.Load(); err == nil && p != nil {
			flow = string(p.Flow)
		}
	}
	return taskKindForAgentFlow(agent, flow)
}

// taskKindForAgentFlow 按 agent + flow 映射 TaskKind 展示标签（B4）。
func taskKindForAgentFlow(agent, flow string) string {
	switch agent {
	case "architect_short", "architect_long":
		return "foundation_plan"
	case "writer":
		switch flow {
		case "writing":
			return "chapter_write"
		case "rewriting":
			return "chapter_rewrite"
		case "polishing":
			return "chapter_polish"
		}
	case "editor":
		switch flow {
		case "reviewing":
			return "chapter_review"
		case "rewriting":
			return "chapter_rewrite"
		}
	case "polisher":
		return "chapter_polish"
	}
	return ""
}

// dispatchFinish 把 DISPATCH 行落成完成态并复位 Worker 状态;
// 清理该 Worker 名下的孤儿 TOOL 行(abort/错误路径 ProgressToolEnd 可能缺席)。
// 放弃 streamOwner（dispatch 结束后不再属于任何 agent）。
func (o *observer) dispatchFinish(agent string, failed bool) {
	o.updateAgent(agent, func(a *agentState) {
		a.state = "idle"
		a.tool = ""
	})
	o.mapMu.Lock()
	delete(o.lastThinkingByAgent, agent)
	call, hasTool := o.toolStarts[agent]
	if hasTool {
		delete(o.toolStarts, agent)
		delete(o.streamExtractors, agent)
	}
	dispatchCall, hasDispatch := o.dispatchStarts[agent]
	if hasDispatch {
		delete(o.dispatchStarts, agent)
	}
	if o.streamOwner == agent {
		o.streamOwner = ""
	}
	o.mapMu.Unlock()
	if hasTool {
		o.emitCallFinish(call, "TOOL", agent, failed)
	}
	if hasDispatch {
		o.emitCallFinish(dispatchCall, "DISPATCH", agent, failed)
	}
	o.streamClear()
}

// workerProgress 把 Worker 进度中继适配为既有的 ToolExecUpdate 处理。
func (o *observer) workerProgress(p agentcore.ProgressPayload) {
	payload := p
	o.handleToolUpdate(agentcore.Event{Type: agentcore.EventToolExecUpdate, Progress: &payload})
}

// finalize 运行结束（完成/出错停止/abort）时收尾：
//  1. agents 快照全部置 idle（agentMu 保护）；
//  2. 整体清空 mapMu 保护的全部 per-run 状态。
//
// abort/cancel 路径不会逐 agent 走 dispatchFinish，残留的 lastThinkingByAgent
// 会持有整章 thinking 全文、toolStarts/dispatchStarts 持有 activeCall、
// streamExtractors 持有抽取器实例、streamArgPrefixes/Labels 持有参数前缀缓存；
// 长会话多次 abort 后累积可达 GB 级 —— 这里统一重置为零值。
// aborting 不在此复位：它由 Host 在 Start/Resume/Continue 入口经 setAborting 显式管理。
func (o *observer) finalize() {
	o.agentMu.Lock()
	for _, a := range o.agents {
		a.state = "idle"
		a.tool = ""
	}
	o.agentMu.Unlock()

	o.mapMu.Lock()
	o.lastThinkingByAgent = make(map[string]string)
	o.dispatchStarts = make(map[string]*activeCall)
	o.toolStarts = make(map[string]*activeCall)
	o.streamExtractors = make(map[string]*agentExtractor)
	o.streamArgPrefixes = make(map[string]string)
	o.streamArgLabels = make(map[string]string)
	o.retryEvents = make(map[string]string)
	o.streamOwner = ""
	o.streamHasContent = false
	o.streamLastByte = 0
	o.streamThinking = false
	o.mapMu.Unlock()
}

// setAborting 由 Host 在 Abort/Close/Start 等生命周期切换处调用，控制
// "context canceled" 类衍生事件是否需要抑制（避免与"用户手动暂停"重复）。
func (o *observer) setAborting(v bool) { o.aborting.Store(v) }

func (o *observer) retryEventID(scope string, attempt int) string {
	if strings.TrimSpace(scope) == "" {
		scope = "engine"
	}
	o.mapMu.Lock()
	defer o.mapMu.Unlock()
	if o.retryEvents == nil {
		o.retryEvents = make(map[string]string)
	}
	if attempt <= 1 || o.retryEvents[scope] == "" {
		o.retryEvents[scope] = nextEventID()
	}
	return o.retryEvents[scope]
}

// emitAndLog 用于调用类事件的"开始"态：发给 TUI 但不写入 runtime queue，
// 避免 replay 时"开始一行、完成又一行"重复。slog 由 host.emitEvent 统一记录。
func (o *observer) emitAndLog(ev Event) {
	o.emitEv(ev)
}

// persistEvent 把事件写入 runtime queue（slog 由 host.emitEvent 统一记录）。
func (o *observer) persistEvent(ev Event) {
	if o.store == nil || o.store.Runtime == nil {
		return
	}
	priority := domain.RuntimePriorityBackground
	switch ev.Category {
	case "SYSTEM", "ERROR":
		priority = domain.RuntimePriorityControl
	}
	_, _ = o.store.Runtime.AppendQueue(domain.RuntimeQueueItem{
		Time:     ev.Time,
		Kind:     domain.RuntimeQueueUIEvent,
		Priority: priority,
		Category: ev.Category,
		Summary:  ev.Summary,
		Payload:  ev,
	})
}

func (o *observer) updateAgent(name string, fn func(*agentState)) {
	if name == "" {
		return
	}
	o.agentMu.Lock()
	defer o.agentMu.Unlock()
	a, ok := o.agents[name]
	if !ok {
		a = &agentState{name: name, state: "idle"}
		o.agents[name] = a
	}
	fn(a)
	a.updated = time.Now()
}

func (o *observer) agentSnapshots() []AgentSnapshot {
	if o == nil {
		return nil
	}
	o.agentMu.Lock()
	defer o.agentMu.Unlock()
	snaps := make([]AgentSnapshot, 0, len(o.agents))
	for _, a := range o.agents {
		snaps = append(snaps, AgentSnapshot{
			Name:      a.name,
			State:     a.state,
			TaskKind:  a.taskKind,
			Summary:   a.summary,
			Tool:      a.tool,
			Turn:      a.turn,
			Context:   a.context,
			UpdatedAt: a.updated,
		})
		// LastChanged 只亮一帧：TUI 读走后清掉，避免压缩策略一直挂在侧栏。
		a.context.LastChanged = false
	}
	return snaps
}
