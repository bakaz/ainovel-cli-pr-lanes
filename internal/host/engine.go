package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/llm"
	"github.com/voocel/agentcore/subagent"

	"github.com/voocel/ainovel-cli/internal/arbiter"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/notify"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// engine 是确定性执行引擎:读事实 → Route → 前置校验 → 直接运行 Worker →
// 检查推进 → 循环;语义场景按需咨询 Arbiter。它执行决定,不参与文学判断
// (docs/engine-rfc.md)。单 goroutine 串行,控制状态只在循环边界变更。
type engine struct {
	store   *storepkg.Store
	workers *subagent.Runner

	arbiterModel    agentcore.ChatModel
	failurePrompt   string
	planStartPrompt string // 启动裁定系统提示词:裁定从未完成时引擎据 StartPrompt 现场补裁
	style           string // 风格名,补裁时传给 DecidePlanStart
	// migrationCheck 在 run 循环开始时调用；非 nil 且返回非 nil 时引擎立即退出
	// （不执行任何 Worker/Provider 调用）。由 host 在构造时注入。
	migrationCheck func() error
	// contract 是当前项目的 SceneBeatContract，用于验证 outline entries 和 scenes。
	contract *projectprofile.SceneBeatContract
	// reconsult 把过期干预送回 host 的完整裁定路径(持久化/审计/全量动作应用),
	// 异步执行——engine 只丢弃过期派单,不自行做残缺的重新裁定。
	reconsult func(text string)

	observer  *observer
	budget    *BudgetSentinel
	gate      *ChapterAdvanceGate
	refresh   func() // 每次 writer 派发前刷新 RestorePack
	emitEvent func(Event)
	notify    func(kind, level, title, body string)
	onPause   func(summary string) // 引擎自主暂停(僵局/失败裁定 abort):走 host 统一暂停语义(lifecycle=paused)
	// onPauseStructured 是生产 Host 的结构化停机回调；保留 onPause 兼容现有
	// engine 单元测试与轻量构造器。结构化回调优先，禁止用 UI 摘要反推原因。
	onPauseStructured func(pauseRequest)
	onDone            func() // run 结束(任何原因);host 据 store 事实定终态

	// 弧/卷边界备份回调。引擎在检测到弧/卷完结章后同步调用。
	// 返回 error 时引擎必须停止循环（跳过 budget/gate 处理）。
	backupArc    func(volume, arc int) error
	backupVolume func(volume int) error

	// beforeRunWorker 仅测试使用：在 runWorker 前调用，用于验证 worker 从未被调用。
	beforeRunWorker func()

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	done    chan struct{}     // close in run() defer after onDone, for true completion signal
	pending []controlOp       // 干预的控制态动作,边界提交
	pause   *pauseRequest     // 请求在当前 Worker 完成后的安全边界暂停
	next    *flow.Instruction // 下一轮优先执行的指令(plan_start / arbiter dispatch)
	// deferGateForNext 只与 next 同生共灭：hold+dispatch 必须先运行配对的
	// editor/writer，让它建立返工队列，随后 Gate 才能判断 rewrites_drained。
	deferGateForNext bool

	// 僵局追踪:上一轮执行后 Route 仍产生同一指令键即累计。
	// Router 指令是任务后置条件的投影；真正完成会让下一指令改变。
	lastKey string
	repeats int
	// 失败重试:同指令键仅重试一次,再败问 Arbiter。
	failedKey string
	// maxTurnsRetries:max_turns 失败的有界反思重试计数(key=agent+task)。
	// 与 failedKey 互斥:max_turns 错误不走首败免费重试,直接进反思通道;
	// 成功即清零(与 failedKey 同位置),计数达 maxTurnsRetryLimit 后交
	// Arbiter 失败裁定。
	maxTurnsRetries map[string]int
	// pendingReflection:待注入下一轮派发的失败反思包(key=agent+task)。
	// runWorker 派发时拼进实际任务文本(一次性消费);不进 inst.Task,
	// 保持 trackDeadlock 的 key 稳定。
	pendingReflection map[string]string
	// fsmConfig 是与生产 Writer 工具集一致的章节流水线 FSM 配置(agents 包
	// BuildWorkers 注入六工具的同源配置,host 构造时经 ChapterFSMConfigFor 注入)。
	// 反思与失败裁定用它解析 stage/required——残缺配置(缺 PipelineEnabled/
	// ExpectedPolisherModel)会让反思报告的 stage 偏离真实工具拦截,产生
	// "提示直接提交"与"FSM 拒绝提交"的自相矛盾(P0-2)。
	fsmConfig tools.ChapterFSMConfig
	// noProgress 是章节无进展熔断器(P1-7/P1-9):统一观察入口 Guard 覆盖所有
	// 来源的 writer 派发(正常路由 / initial 指令 / Arbiter reroute / 干预派发),
	// 连续 3 轮同一章快照完全一致且无新 checkpoint/草稿/账本变化 → 不再自动
	// 重派该章(返回 nil)并标记 manual_recovery_required,熔断原因经
	// blockedWithNotify 输出到用户通道,等待人工。
	// 惰性初始化(run 循环首轮,用当时的 fsmConfig——测试可在构造后覆盖)。
	noProgress *flow.NoProgressBreaker
}

// pauseRequest 是 Engine 与 Host 之间的结构化停机请求。
type pauseRequest struct {
	generation uint64
	category   domain.StopCategory
	code       string
	summary    string
	level      string
	notBefore  string
}

// deadlockConsultAt / deadlockAbortAt:repeats 达到前者问 Arbiter,达到后者硬熔断。
// 确定性 Engine 必须对无进展循环给出明确上界(RFC §5)。
const (
	deadlockConsultAt = 3
	deadlockAbortAt   = 5
)

// maxTurnsRetryLimit:max_turns 失败后的有界反思重试次数(lib-1 调研:
// 业界标准为带失败摘要的有界重试 ≤2 次,再送 Arbiter 失败裁定)。
const maxTurnsRetryLimit = 2

// controlOp 是干预裁定中修改控制状态的动作(边界提交;RFC §3)。
// text/facts 保留原始咨询上下文:dispatch 对账失败时以新事实重询。
type controlOp struct {
	hold     *arbiter.AdvanceHoldOp
	reopen   *arbiter.ReopenOp
	dispatch *arbiter.DispatchOp
	text     string
	facts    arbiter.InterventionFacts
}

// start 启动引擎循环;已在运行则 no-op(返回 false)。
func (e *engine) start(initial *flow.Instruction) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = agentcore.WithToolProgress(ctx, e.observer.workerProgress)
	e.cancel = cancel
	e.running = true
	e.done = make(chan struct{})
	e.pause = nil
	// initial 为空时不覆盖 e.next——停机期干预可能已通过 applyControlOp 排入
	// 裁定派单(如 editor 返工),start(nil) 抹掉它会让 Route 派 writer 续写,
	// 与用户意图相反。
	if initial != nil {
		e.next = initial
		e.deferGateForNext = false
	}
	e.lastKey, e.repeats, e.failedKey = "", 0, ""
	e.maxTurnsRetries = map[string]int{}
	e.pendingReflection = map[string]string{}
	go e.run(ctx)
	return true
}

// abort 取消当前循环(暂停语义;checkpoint 保证无损)。
func (e *engine) abort() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// requestPause 请求在当前 Worker 和本轮边界处理完成后暂停，不取消在途 Worker。
func (e *engine) requestPause(req pauseRequest) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || e.pause != nil {
		return false
	}
	e.pause = &req
	return true
}

// cancelPeakPolicyPause 撤销尚未被 Engine 消费的手动任务高峰暂停请求。
// 闲时来源的 idle_window_end 不在这里撤销：闲时任务在高峰必须停下。
func (e *engine) cancelPeakPolicyPause() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pause == nil || e.pause.category != domain.StopCategoryPeakPolicy {
		return false
	}
	e.pause = nil
	return true
}

func (e *engine) takePause() (pauseRequest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pause == nil {
		return pauseRequest{}, false
	}
	req := *e.pause
	e.pause = nil
	return req, true
}

func (e *engine) isRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// enqueue 把干预的控制态动作排入边界队列(引擎运行中);返回 false 表示未运行,
// 调用方应立即自行执行。
func (e *engine) enqueue(op controlOp) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return false
	}
	e.pending = append(e.pending, op)
	return true
}

func (e *engine) run(ctx context.Context) {
	defer func() {
		// 保持 running=true 贯穿所有清理工作及 onDone，
		// 确保 Host.runEnded 在读取生命周期时 engine 仍标记为运行中。
		e.mu.Lock()
		leftover := e.pending
		e.pending = nil
		e.mu.Unlock()

		for _, op := range leftover {
			if op.dispatch != nil {
				if op.text != "" {
					if err := e.store.RunMeta.SetPendingSteer(op.text); err != nil {
						slog.Warn("残留干预回存失败", "module", "engine", "err", err)
					}
				}
				e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
					Summary: "引擎已停,裁定派单未执行;干预已保留,继续创作时自动重新裁定"})
				op.dispatch = nil
			}
			if op.hold != nil || op.reopen != nil {
				_ = e.applyControlOp(context.Background(), op)
			}
		}
		// onDone 仍能看到 running=true
		e.onDone()

		// 原子清除 running + 通知等待方
		e.mu.Lock()
		e.running = false
		e.cancel = nil
		dch := e.done
		e.done = nil
		e.mu.Unlock()
		if dch != nil {
			close(dch)
		}
	}()

	// 迁移门：migration_required 状态下引擎不执行任何 Worker/Provider 调用。
	// 放在 defer 之后以确保退出时 running 正确复位。
	if e.migrationCheck != nil {
		if err := e.migrationCheck(); err != nil {
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
				Summary: "迁移门拦截引擎启动: " + err.Error(), Level: "warn"})
			return
		}
	}

	// P1-7：惰性初始化无进展熔断器（用当前 fsmConfig；测试可在构造后覆盖）。
	if e.noProgress == nil {
		e.noProgress = flow.NewNoProgressBreaker(e.fsmConfig)
	}

	for {
		if ctx.Err() != nil {
			return
		}
		// hold+dispatch 必须先让配对派单建立返工事实；其它情况在派发前统一检查
		// Gate，保证 boundary hold 和无许可 review 不会多跑一个 Worker。
		deferGate := e.applyPendingOps(ctx) || e.nextDefersGate()
		// 政策止损优先于高峰自动暂停：即使高峰请求先到，也不能把预算/验收
		// 闸门错误包装成可自动恢复的暂停。
		if e.budget.HandleBoundary() {
			return
		}
		if !deferGate {
			if e.gate.HandleBoundary() {
				return
			}
		}
		if req, ok := e.takePause(); ok {
			if e.onPauseStructured != nil {
				e.onPauseStructured(req)
			} else if e.onPause != nil {
				e.onPause(req.summary)
			}
			return
		}

		inst := e.takeNext()
		if inst == nil {
			// 路由（纯函数）：writer 派发的无进展熔断统一在下方 Guard 检查
			// （覆盖 takeNext/reroute/干预派发等所有来源，P1-9 阻塞项 9.3）。
			inst = flow.Route(flow.LoadState(e.store))
		}
		if inst == nil {
			inst = e.planStartFallback(ctx)
		}
		if inst == nil {
			// 语义场景或终态:完本 → 确定性收尾;其余(Steering 残留等)
			// → 自然停机,等用户 Continue / 干预。
			return
		}
		if replaced := e.precheck(inst); replaced != nil {
			inst = replaced
		}
		allowed, gateErr := e.gate.Allow(inst)
		if gateErr != nil {
			e.pauseWithNotify(notify.KindAdvanceGate, "章节推进控制错误，已暂停: "+gateErr.Error())
			return
		}
		if !allowed {
			return
		}
		// P1-9 阻塞项 9.3 + 复核缺口 2：统一熔断观察——只在 gate 通过、即将实际
		// 派发的最终指令上计数（覆盖正常路由 / initial 指令 / Arbiter reroute /
		// 干预派发等所有来源）。gate 拒绝的轮次 worker 未运行、不累计计数，
		// 避免多次 Continue 反复 gate 拒绝错误触发 manual_recovery_required。
		// 熔断 → 确定性原因输出到用户通道并停机等待人工；先于 trackDeadlock
		// 的 Arbiter 咨询（ch450 类场景咨询→retry 只会继续烧钱）。
		if e.noProgress != nil && e.noProgress.Guard(e.store, inst) == nil {
			e.blockedWithNotify(inst)
			return
		}
		if stop := e.trackDeadlock(ctx, &inst); stop {
			return
		}
		if inst == nil {
			continue // 僵局裁定要求重算路由
		}

		// 记录 Worker 启动前的最新已完成章；读错则暂停/停止，不执行 Worker。
		before, beforeErr := e.latestCompletedChapter()
		if beforeErr != nil {
			e.pauseWithNotify("backup", "进度读取失败，已暂停: "+beforeErr.Error())
			return
		}

		if e.beforeRunWorker != nil {
			e.beforeRunWorker()
		}
		// 复核缺口 2：实际派发点提交熔断计数——Guard 的预演在此生效。trackDeadlock
		// 咨询/reroute、Arbiter abort、进度读取失败、上下文取消等未派发分支不提交
		// （只计实际派发，咨询轮不计入停滞）。
		if e.noProgress != nil && inst.Chapter > 0 {
			e.noProgress.Commit(inst.Chapter)
		}
		err := e.runWorker(ctx, inst)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if stop := e.handleWorkerError(ctx, inst, err); stop {
				return
			}
		} else {
			// P1-9 阻塞项 9.2：worker 成功后清理该章错误码（避免熔断理由报告
			// 上次失败）；成功但无状态变化不重置无进展计数（停滞仍会累计熔断）。
			if e.noProgress != nil && inst.Chapter > 0 {
				e.noProgress.ClearError(inst.Chapter)
			}
			// Worker 成功——检测新完成的章节并检查弧/卷边界
			if stop := e.handleCompletedChapters(ctx, before); stop {
				return
			}
		}

		// 政策边界:预算止损优先于验收/推进暂停。
		if e.budget.HandleBoundary() {
			return
		}
		if e.gate.HandleBoundary() {
			return
		}
	}
}

func (e *engine) takeNext() *flow.Instruction {
	e.mu.Lock()
	defer e.mu.Unlock()
	inst := e.next
	e.next = nil
	e.deferGateForNext = false
	return inst
}

func (e *engine) nextDefersGate() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.next != nil && e.deferGateForNext
}

// planStartFallback 覆盖规划事实缺位、Route 无法推导规划师的两个窗口:
//  1. 裁定已落盘、首个 save_foundation 尚未发生 → 按固化的 PlanStartRecord 续跑,
//     不重新裁定(RFC §6);首个 foundation 落盘后 tier 就位,补齐分支接管。
//  2. 裁定从未完成(启动时模型故障)但输入事实 StartPrompt 在 → 现场补裁。
//     这是首次裁定的重试,不违反"恢复不依赖重新裁定"——那条纪律针对已存在的裁定。
//     补裁失败走显式暂停:启动失败不允许无声停机。
func (e *engine) planStartFallback(ctx context.Context) *flow.Instruction {
	progress, err := e.store.Progress.Load()
	if err != nil || progress == nil {
		return nil
	}
	if progress.Phase == domain.PhaseWriting || progress.Phase == domain.PhaseComplete {
		return nil
	}
	meta, err := e.store.RunMeta.Load()
	if err != nil || meta == nil || meta.PlanningTier != "" {
		return nil
	}
	if len(e.store.FoundationMissing()) == 0 {
		return nil
	}
	if meta.PlanStart != nil {
		return &flow.Instruction{
			Agent:  meta.PlanStart.Planner,
			Task:   meta.PlanStart.PlannerTask,
			Reason: "按已固化的启动裁定开始规划",
		}
	}
	if meta.StartPrompt == "" {
		return nil
	}
	return e.retryPlanStart(ctx, meta.StartPrompt)
}

// retryPlanStart 补裁启动决策并固化(裁定先落事实再执行,与 StartPrepared 同构)。
func (e *engine) retryPlanStart(ctx context.Context, prompt string) *flow.Instruction {
	start := time.Now()
	decision, derr := runObservedDecision(e.observer, "启动补裁", func() (arbiter.PlanStartDecision, error) {
		return arbiter.DecidePlanStart(ctx, e.arbiterModel, e.planStartPrompt, prompt, e.style)
	})
	rec := storepkg.DecisionRecord{Kind: "plan_start", Decider: "arbiter", Input: prompt,
		Reason: decision.Reason, DurationMs: time.Since(start).Milliseconds()}
	if derr == nil {
		if data, err := json.Marshal(decision); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	rec, recErr := e.store.Decisions.Append(rec)
	if recErr != nil {
		slog.Warn("启动补裁审计落盘失败", "module", "engine", "err", recErr)
	}
	if derr != nil {
		e.pauseWithNotify(notify.KindPlanStart, "启动裁定失败,已暂停(请检查模型/网络配置后继续): "+truncate(derr.Error(), 200))
		return nil
	}
	if err := e.store.RunMeta.SetPlanStart(domain.PlanStartRecord{
		RawPrompt: prompt, Planner: decision.Planner, PlannerTask: decision.Task, DecisionID: rec.ID,
	}); err != nil {
		e.pauseWithNotify(notify.KindPlanStart, "启动裁定无法落盘,已暂停: "+err.Error())
		return nil
	}
	e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "info",
		Summary: fmt.Sprintf("启动裁定已补齐(规划师: %s——%s)", decision.Planner, decision.Reason)})
	return &flow.Instruction{Agent: decision.Planner, Task: decision.Task, Reason: decision.Reason}
}

// precheck 是原 ToolGate 的确定性化身:不合法的派发直接改写,无需教学文案。
func (e *engine) precheck(inst *flow.Instruction) *flow.Instruction {
	progress, progressErr := e.store.Progress.Load()
	if progress != nil && progress.Phase == domain.PhaseComplete {
		slog.Warn("完本期派发被丢弃", "module", "engine", "agent", inst.Agent)
		return &flow.Instruction{}
	}
	isV3 := e.contract != nil && e.contract.GetContract() == projectprofile.ContractSceneBeatV3
	strictTargetAgent := inst.Agent == "writer" || inst.Agent == "architect_short" || inst.Agent == "architect_long"
	if isV3 && strictTargetAgent {
		if progressErr != nil {
			e.pauseWithNotify("engine", "引擎预检: progress 加载失败 ("+progressErr.Error()+")")
			return &flow.Instruction{}
		}
		if progress == nil {
			e.pauseWithNotify("engine", "引擎预检: progress 未初始化")
			return &flow.Instruction{}
		}
	}

	if inst.Agent == "writer" {
		var ch int
		if isV3 {
			if len(progress.PendingRewrites) > 0 {
				ch = progress.PendingRewrites[0]
			} else {
				ch = progress.NextChapter()
			}
		} else {
			ch = writerTargetChapter(e.store)
		}
		// 复核缺口 1：归一化最终 writer 目标章节——干预派发 / 非 writer→writer
		// reroute / 未绑定章节的原始 writer 指令在此统一写回 inst.Chapter
		// （否则 breaker.Guard 因 Chapter<=0 跳过观察，Chapter=0 的 writer 可
		// 绕过熔断）。
		if inst.Chapter <= 0 && ch > 0 {
			inst.Chapter = ch
		}
		if ch > 0 {
			// 检查 legacy exhausted：阻止 writer dispatch，避免工具调用风暴
			exhausted, loadErr := e.isStyleReviewExhausted(ch)
			if loadErr != nil {
				e.pauseWithNotify("engine", fmt.Sprintf("引擎预检 Writer: 第 %d 章评审状态检查失败: %v", ch, loadErr))
				return &flow.Instruction{}
			}
			if exhausted {
				e.pauseWithNotify("engine", fmt.Sprintf("引擎预检 Writer: 第 %d 章评审已耗尽（exhausted），无法继续创作。请使用 /style-override 覆盖评审结果后重试", ch))
				return &flow.Instruction{}
			}
			if isV3 {
				// V3: Writer 必须有一个有效展开的 outline entry。
				// 缺失、无效或加载错误一律 hard pause，不允许回退到 Architect。
				if err := tools.ValidateOutlineEntry(e.store, e.contract, ch); err != nil {
					e.pauseWithNotify("engine", "引擎预检 Writer: "+err.Error())
					return &flow.Instruction{}
				}
			} else {
				// Core4: 向后兼容 — 目标章未展开时改派 architect_long
				if err := tools.EnsureChapterExpanded(e.store, ch); err != nil {
					return &flow.Instruction{
						Agent:  "architect_long",
						Task:   fmt.Sprintf("下一弧为骨架(%s)。调用 save_foundation(type=expand_arc) 展开下一弧;若当前卷已写完,改用 type=append_volume 追加并展开下一卷。", err),
						Reason: "写作目标章未展开,先展开再续写",
					}
				}
				if e.contract != nil {
					if err := tools.ValidateOutlineEntry(e.store, e.contract, ch); err != nil {
						e.pauseWithNotify("engine", "引擎预检 Writer: "+err.Error())
						return &flow.Instruction{}
					}
				}
			}
		} else {
			// 复核缺口 1：无法推导 writer 目标章节 → 显式暂停（返回明确错误、
			// 停止派发），不静默放行一个未绑定章节、绕过熔断观察的 writer 指令。
			e.pauseWithNotify("engine", "引擎预检 Writer: 无法确定目标章节，已停止自动派发，等待人工处理")
			return &flow.Instruction{}
		}
		e.refresh()
	}
	if (inst.Agent == "architect_short" || inst.Agent == "architect_long") && e.contract != nil {
		var ch int
		if isV3 {
			ch = progress.NextChapter()
		} else {
			ch = architectTargetChapter(e.store)
		}
		if ch > 0 {
			// Architect: 仅在 outline 成功加载且目标确实 absent 时允许通过。
			// 加载失败或目标已存在但无效 → hard pause。
			outline, loadErr := e.store.Outline.LoadOutline()
			if loadErr != nil {
				e.pauseWithNotify("engine", "引擎预检 Architect: outline 加载失败 ("+loadErr.Error()+")")
				return &flow.Instruction{}
			}
			hasChapter := false
			for _, entry := range outline {
				if entry.Chapter == ch {
					hasChapter = true
					break
				}
			}
			if hasChapter {
				if err := tools.ValidateOutlineEntry(e.store, e.contract, ch); err != nil {
					e.pauseWithNotify("engine", "引擎预检 Architect: "+err.Error())
					return &flow.Instruction{}
				}
			}
			// hasChapter=false → target absent → 允许通过
		} else if isV3 {
			e.pauseWithNotify("engine", "引擎预检 Architect: 无法确定目标章节")
			return &flow.Instruction{}
		}
	}
	return nil
}

// writerTargetChapter 推导 writer 下一次派发实际会写的章节(重写队列头,否则下一章)。
func writerTargetChapter(st *storepkg.Store) int {
	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		return 0
	}
	if len(progress.PendingRewrites) > 0 {
		return progress.PendingRewrites[0]
	}
	return progress.NextChapter()
}

// trackDeadlock 维护僵局计数：连续出现同一 Agent+Task 说明上一轮
// 没有满足路由后置条件。Worker 内部的 plan/draft/edit 等中间 checkpoint
// 只用于恢复和观测，不能重置 Engine 级计数（issue #84）。
// repeats 达阈值时咨询 Arbiter，硬上限直接熔断。
// 返回 stop=true 表示本轮应结束循环;inst 可能被 Arbiter 改写(reroute)或置 nil(重算)。
func (e *engine) trackDeadlock(ctx context.Context, inst **flow.Instruction) (stop bool) {
	in := *inst
	if in == nil || in.Agent == "" {
		*inst = nil
		return false
	}
	key := in.Agent + "\x00" + in.Task
	if key == e.lastKey {
		e.repeats++
	} else {
		e.lastKey, e.repeats = key, 1
	}
	if e.repeats < deadlockConsultAt {
		return false
	}
	if e.repeats >= deadlockAbortAt {
		e.pauseWithNotify(notify.KindDeadlock, fmt.Sprintf("僵局熔断: 指令连续 %d 次无进展(%s),已暂停等待人工介入", e.repeats, in.Agent))
		return true
	}
	// Arbiter 僵局咨询(repeats ∈ [consultAt, abortAt))。裁定 retry 不清零计数。
	facts := e.failureFacts("deadlock", in, "")
	decision, err := runObservedDecision(e.observer, "僵局裁定", func() (arbiter.FailureDecision, error) {
		return arbiter.DecideFailure(ctx, e.arbiterModel, e.failurePrompt, facts)
	})
	e.recordFailureDecision("deadlock", in, facts, decision, err)
	if err != nil {
		e.pauseWithNotify(notify.KindDeadlock, "僵局裁定失败,已暂停等待人工介入: "+err.Error())
		return true
	}
	switch decision.Action {
	case "retry":
		return false
	case "reroute":
		// 复核缺口 1：deadlock reroute 不直接在当前循环派发——reroute 产生于
		// breaker.Guard 之后，当前循环直接执行会绕过熔断观察。排队到下一轮，
		// 重新完整经过 precheck → gate.Allow → breaker.Guard → trackDeadlock →
		// dispatch，保证所有最终 writer 派发都经过 Guard。
		// DispatchOp 无 chapter 字段：writer reroute 继承当前指令的章节号
		// （reroute 目标是当前卡住的章节），使 Guard 能按章观察。
		dispatch := &flow.Instruction{Agent: decision.Dispatch.Agent, Task: decision.Dispatch.Task, Reason: decision.Reason}
		if dispatch.Agent == "writer" {
			dispatch.Chapter = in.Chapter
		}
		e.mu.Lock()
		e.next = dispatch
		e.deferGateForNext = false
		e.mu.Unlock()
		*inst = nil // 触发 continue：下一轮 takeNext 消费排队指令
		return false
	default: // abort
		e.pauseWithNotify(notify.KindDeadlock, "僵局裁定: "+decision.Reason)
		return true
	}
}

// runWorker 直接运行一次子代理:DISPATCH 事件 + 进度中继 + 结果解析。
func (e *engine) runWorker(ctx context.Context, inst *flow.Instruction) error {
	slog.Info("engine 派发", "module", "engine", "agent", inst.Agent, "reason", inst.Reason)
	e.observer.dispatchStart(inst.Agent, inst.Task)
	// Writer 任务预标进行中(与旧 Dispatcher 一致:UI 大纲立即反映"▸ 进行中")。
	if inst.Agent == "writer" && inst.Chapter > 0 {
		if err := e.store.Progress.ValidateChapterWork(inst.Chapter); err != nil {
			e.observer.dispatchFinish(inst.Agent, true)
			return fmt.Errorf("%w: %w", errInvalidWriteTarget, err)
		}
		if err := e.store.Progress.StartChapter(inst.Chapter); err != nil {
			slog.Warn("预标进行中失败", "module", "engine", "chapter", inst.Chapter, "err", err)
		}
	}

	// P0 provenance：派发 writer 时把当前生效的作者模型记录到 RunMeta.LastAuthorModel
	// （真实"最近一次正文写入"模型，落盘在写入工具/引擎派发侧；rewrite_brief 的
	// candidate.author_model 据此取值，不再从 StyleReview 反推 critic 模型）。
	if inst.Agent == "writer" {
		e.recordWriterModel()
	}

	// Worker 进度经 ctx ToolProgress 中继到 observer。
	runCtx := agentcore.WithToolProgress(ctx, func(p agentcore.ProgressPayload) {
		e.observer.workerProgress(p)
	})
	// 反思重试注入:max_turns 失败后,把失败反思包拼进本次实际派发的任务文本
	// (不进 inst.Task,保持 trackDeadlock 的 key 稳定;一次性消费)。
	// Runner.Run 只接收 agent+task,Reason 不进 worker 提示,故必须在此合并。
	task := inst.Task
	if refl := e.takePendingReflection(inst.Agent, inst.Task); refl != "" {
		task = task + "\n\n[上一次执行反思] " + refl
	}
	_, err := e.workers.Run(runCtx, inst.Agent, task)
	if err == nil {
		// 成功即清失败追踪:同键的下一次失败重新享有"先重试一次"额度;
		// max_turns 反思计数与待注入包同步清零,下次失败重新享有全额额度。
		e.failedKey = ""
		e.clearMaxTurnsState(inst.Agent + "\x00" + inst.Task)
	}
	e.observer.dispatchFinish(inst.Agent, err != nil)
	return err
}

// recordWriterModel 在派发 writer 前把当前生效的作者模型名记录到 RunMeta。
// 模型名从注册的 writer agent 配置读取（SwappableModel 反映运行时 /model 热切换）。
// 记录失败只告警不阻断派发——provenance 是审计事实，不是执行前置条件。
func (e *engine) recordWriterModel() {
	cfg, ok := e.workers.AgentConfig("writer")
	if !ok || cfg.Model == nil {
		return
	}
	if name := modelNameOf(cfg.Model); name != "" {
		if err := e.store.RunMeta.SetLastAuthorModel(name); err != nil {
			slog.Warn("记录 writer 模型失败", "module", "engine", "err", err)
		}
	}
}

// modelNameOf 提取 ChatModel 的当前模型名：优先 ModelNamer，回退 Info()。
// failoverModel（配置了 fallbacks 的角色）只实现 Info()，两者都覆盖。
func modelNameOf(m agentcore.ChatModel) string {
	if mn, ok := m.(agentcore.ModelNamer); ok {
		if name := mn.ModelName(); name != "" {
			return name
		}
	}
	if info, ok := m.(interface{ Info() llm.ModelInfo }); ok {
		return info.Info().Name
	}
	return ""
}

// handleWorkerError 错误分类(RFC §4):确定性错误直接暂停(重试与裁定都无意义);
// 其余同指令重试一次 → Arbiter → 最保守暂停。
func (e *engine) handleWorkerError(ctx context.Context, inst *flow.Instruction, werr error) (stop bool) {
	msg := werr.Error()
	// P1-7：把本次失败的稳定错误码记入无进展熔断快照（相同错误码保持计数，
	// 不同错误码视为状态变化重置计数）。
	if e.noProgress != nil && inst.Chapter > 0 {
		e.noProgress.RecordError(inst.Chapter, workerErrorCode(werr))
	}
	e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Agent: inst.Agent,
		Summary: truncate(fmt.Sprintf("%s 失败: %s", inst.Agent, msg), 120), Detail: msg, Level: "error"})

	// 确定性分类先行:参数/配置类错误是代码或配置 bug,重试必然同错,
	// 送 Arbiter 也给不出出路——直接暂停等人工。
	if isDeterministicWorkerError(werr) {
		e.pauseWithNotify(notify.KindWorkerFailure, "确定性错误(重试无意义),已暂停等待人工介入: "+truncate(msg, 200))
		return true
	}

	// max_turns 不消耗 failedKey 首败免费重试:失败原因已知(轮次耗尽),
	// 纯重放无意义——直接走带反思的有界重试通道(≤2 次 → Arbiter),
	// 避免 "polish 超时 / FSM 循环" 型失败无限重烧预算。
	if errors.Is(werr, agentcore.ErrMaxTurns) {
		return e.handleMaxTurnsRetry(ctx, inst, werr)
	}

	key := inst.Agent + "\x00" + inst.Task
	if e.failedKey != key {
		// 首败:原指令重试一次(下一轮 Route 重算,事实驱动天然幂等)。
		e.failedKey = key
		return false
	}
	e.failedKey = ""
	return e.arbitrateWorkerFailure(ctx, inst, werr)
}

// arbitrateWorkerFailure 走 Arbiter worker_failure 裁定(首败免费重试与
// max_turns 反思额度均耗尽后的共同出口):retry 返回 false 继续循环,
// reroute 改派新指令,abort 暂停。返回 stop=true 表示应停止循环。
func (e *engine) arbitrateWorkerFailure(ctx context.Context, inst *flow.Instruction, werr error) (stop bool) {
	msg := werr.Error()
	facts := e.failureFacts("worker_failure", inst, msg)
	decision, err := runObservedDecision(e.observer, "失败裁定", func() (arbiter.FailureDecision, error) {
		return arbiter.DecideFailure(ctx, e.arbiterModel, e.failurePrompt, facts)
	})
	e.recordFailureDecision("worker_failure", inst, facts, decision, err)
	if err != nil {
		e.pauseWithNotify(notify.KindWorkerFailure, "失败裁定不可用,已暂停等待人工介入: "+msg+contentFilterAdvice(werr))
		return true
	}
	switch decision.Action {
	case "retry":
		return false
	case "reroute":
		// 与 trackDeadlock 的 reroute 同一语义（复核缺口 1）：writer reroute
		// 继承当前指令的章节号——排队指令下一轮经 takeNext → precheck →
		// gate.Allow → breaker.Guard 完整管线后派发，Guard 按章观察。
		dispatch := &flow.Instruction{Agent: decision.Dispatch.Agent, Task: decision.Dispatch.Task, Reason: decision.Reason}
		if dispatch.Agent == "writer" {
			dispatch.Chapter = inst.Chapter
		}
		e.mu.Lock()
		e.next = dispatch
		e.deferGateForNext = false
		e.mu.Unlock()
		return false
	default: // abort
		e.pauseWithNotify(notify.KindWorkerFailure, "失败裁定: "+decision.Reason+contentFilterAdvice(werr))
		return true
	}
}

// handleMaxTurnsRetry 带反思的有界重试通道(max_turns 专用):
//   - 跳过 failedKey 首败免费重试——失败原因已知(轮次耗尽),纯重放无意义;
//   - 计数 < maxTurnsRetryLimit:构造失败反思包(FSM stage / required action /
//     草稿状态 / 策略提示),经 Reason 与 pendingReflection 注入下一轮重派;
//   - 计数 >= maxTurnsRetryLimit:清计数,交 Arbiter 失败裁定(retry/reroute/abort)。
//
// 反思通道自身有上界(≤2 次),不会无限重烧预算;每次注入同时重置僵局计数——
// 反思重试视作新的尝试窗口,deadlock 咨询(repeats>=3)不会在通道内提前介入,
// 对其它无进展循环的熔断照常工作。返回 stop=true 表示应停止循环。
func (e *engine) handleMaxTurnsRetry(ctx context.Context, inst *flow.Instruction, werr error) (stop bool) {
	key := inst.Agent + "\x00" + inst.Task
	if e.maxTurnsRetries == nil {
		e.maxTurnsRetries = map[string]int{}
		e.pendingReflection = map[string]string{}
	}
	if e.maxTurnsRetries[key] >= maxTurnsRetryLimit {
		// 反思额度耗尽:清计数,交 Arbiter 失败裁定。
		e.clearMaxTurnsState(key)
		return e.arbitrateWorkerFailure(ctx, inst, werr)
	}
	e.maxTurnsRetries[key]++
	refl := e.buildMaxTurnsReflection(inst, e.maxTurnsRetries[key], werr)
	e.pendingReflection[key] = refl
	// 反思重试视作新的尝试窗口:重置僵局计数(通道 ≤2 次自身上界,
	// 不需要 repeats 兜底)。
	e.lastKey, e.repeats = "", 0
	// 下一轮优先重派同一任务;反思摘要进 Reason(事件/日志可见),不进 Task,
	// 保持 trackDeadlock 的 key 稳定;反思包同时经 pendingReflection 拼进
	// 实际任务文本,writer 才能看到(Runner.Run 只接收 agent+task)。
	e.mu.Lock()
	e.next = &flow.Instruction{
		Agent: inst.Agent, Task: inst.Task, Chapter: inst.Chapter,
		Reason: fmt.Sprintf("反思重试(%d/%d): %s", e.maxTurnsRetries[key], maxTurnsRetryLimit, refl),
	}
	e.mu.Unlock()
	return false
}

// buildMaxTurnsReflection 组装带反思的失败摘要,注入 max_turns 反思重试轮:
// 上次失败原因 / FSM stage 与 required action / 草稿状态 / 按 Required action
// 生成的唯一动作提示 / 预算提示。全部字段 best-effort:store 读失败即省略,
// 不阻断重派。stage/required 用与生产一致的 e.fsmConfig 解析。
func (e *engine) buildMaxTurnsReflection(inst *flow.Instruction, retry int, werr error) string {
	var b strings.Builder
	b.WriteString("上次执行未完成,原因: 达到轮次上限(max turns reached)")
	if inst.Chapter > 0 {
		fmt.Fprintf(&b, "; 目标章节: 第 %d 章", inst.Chapter)
		var stage tools.ChapterStage
		var required tools.ChapterAction
		if d, err := tools.ResolveChapterStage(e.store, inst.Chapter, e.fsmConfig); err == nil {
			stage, required = d.Stage, d.Required
			if d.Reason != "" {
				fmt.Fprintf(&b, "; 流水线判定: %s", d.Reason)
			}
		}
		if draft, err := e.store.Drafts.LoadDraft(inst.Chapter); err == nil {
			if draft == "" {
				b.WriteString("; 草稿尚未生成")
			} else {
				b.WriteString("; 草稿已存在")
			}
		}
		if stage != "" {
			fmt.Fprintf(&b, "; 流水线阶段: %s", stage)
			if required != "" {
				fmt.Fprintf(&b, ", 当前要求的动作: %s", required)
			}
		}
		// 按 Required action 生成唯一动作提示,与 FSM 拦截的 required 严格一致,
		// 消除"提示直接提交"与"FSM 拒绝提交"的自相矛盾(P0-1)。
		b.WriteString("; 策略提示: ")
		switch required {
		case tools.ChapterActionPolish:
			fmt.Fprintf(&b, "当前唯一动作：调用 polish_draft(chapter=%d)，成功后调用一次 check_consistency，严格执行其 required_next_action。禁止 edit_chapter/commit_chapter。", inst.Chapter)
		case tools.ChapterActionCheck:
			fmt.Fprintf(&b, "当前唯一动作：调用 check_consistency(chapter=%d)，然后严格执行其 required_next_action。", inst.Chapter)
		case tools.ChapterActionReview:
			fmt.Fprintf(&b, "当前唯一动作：调用 review_style(chapter=%d)，按评审结果继续。禁止直接提交。", inst.Chapter)
		case tools.ChapterActionEdit:
			b.WriteString("当前唯一动作：按当前 findings 用 edit_chapter 修改，修改后调用 check_consistency。")
		case tools.ChapterActionDraft:
			fmt.Fprintf(&b, "当前唯一动作：调用 draft_chapter(chapter=%d, mode=write) 提供完整正文。", inst.Chapter)
		case tools.ChapterActionCommit:
			fmt.Fprintf(&b, "可直接提交：调用 commit_chapter(chapter=%d)（含必要参数）。", inst.Chapter)
		default:
			// required 为空:blocked/disabled/complete 等无下一步动作的阶段。
			if stage == tools.ChapterStageBlocked {
				b.WriteString("当前状态 blocked：停止自动重试并升级人工。")
			} else {
				b.WriteString("直接收敛到提交，不要重复已完成的步骤")
			}
		}
	}
	// 预算提示:仍以配置的轮次上限执行,提示尽早收敛到提交。
	limit := 45
	var mte *agentcore.MaxTurnsError
	if errors.As(werr, &mte) && mte.Limit > 0 {
		limit = mte.Limit
	}
	fmt.Fprintf(&b, "; 预算提示: 本轮仍以 %d 轮为上限(反思重试第 %d/%d 次),请在轮次耗尽前收敛到提交",
		limit, retry, maxTurnsRetryLimit)
	return b.String()
}

// clearMaxTurnsState 清空指定 key 的反思计数与待注入反思包。
func (e *engine) clearMaxTurnsState(key string) {
	if e.maxTurnsRetries != nil {
		delete(e.maxTurnsRetries, key)
	}
	if e.pendingReflection != nil {
		delete(e.pendingReflection, key)
	}
}

// takePendingReflection 取出并消费待注入的失败反思包(不存在返回空串)。
func (e *engine) takePendingReflection(agent, task string) string {
	if e.pendingReflection == nil {
		return ""
	}
	key := agent + "\x00" + task
	refl := e.pendingReflection[key]
	if refl != "" {
		delete(e.pendingReflection, key)
	}
	return refl
}

// contentFilterAdvice 给内容审核拦截的暂停附上用户可执行的出路。
// 审核是服务商黑盒,预检/规避都不可行,能做的只有把决策递到用户手上;
// 拦截本身不提前熔断——换上下文重派对它有真实自愈率(ch21-24 实测),
// 走完"免费重试→仲裁"再暂停。
func contentFilterAdvice(werr error) string {
	if !errors.Is(werr, agentcore.ErrProviderContentFilter) {
		return ""
	}
	return "。这是服务商内容审核拦截(非本地错误),可选: /model 切到无审核层的服务商后输入「继续」;或修改本章草稿(drafts/)措辞后再继续;原样重试大概率仍被拦"
}

// architectTargetChapter 推导 architect 可能操作的章节（当前写作中的下一章）。
func architectTargetChapter(st *storepkg.Store) int {
	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		return 0
	}
	return progress.NextChapter()
}

// errInvalidWriteTarget 标记 runWorker 前置校验拦下的非法写作目标——引擎自身
// 产生的确定性错误,与 subagent.ErrUnknownAgent 同属"重试必然同错"类。
var errInvalidWriteTarget = errors.New("非法写作目标")

// isDeterministicWorkerError 识别重试必然同错的错误。全部走类型化匹配:
// agent 未注册(subagent.ErrUnknownAgent)与引擎前置校验失败——不再依赖错误文案。
func isDeterministicWorkerError(err error) bool {
	return errors.Is(err, subagent.ErrUnknownAgent) || errors.Is(err, errInvalidWriteTarget)
}

// workerErrorCode 返回 worker 失败错误的稳定分类码（P1-7 无进展熔断快照用）。
// best-effort：未分类错误统一 "worker_error"（同一错误码保持熔断计数，
// 不同错误码视为状态变化重置计数）。
func workerErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, agentcore.ErrMaxTurns) {
		return "max_turns"
	}
	var te *tools.ChapterTransitionError
	if errors.As(err, &te) {
		if te.Stage == tools.ChapterStageBlocked {
			return "chapter_fsm_blocked"
		}
		return "chapter_fsm_denied"
	}
	if errors.Is(err, storepkg.ErrPolishCandidateStale) || errors.Is(err, storepkg.ErrReviewStale) {
		return "candidate_stale"
	}
	if errors.Is(err, agentcore.ErrProviderContentFilter) {
		return "content_filter"
	}
	return "worker_error"
}

func (e *engine) failureFacts(kind string, inst *flow.Instruction, errMsg string) arbiter.FailureFacts {
	f := arbiter.FailureFacts{Kind: kind, Agent: inst.Agent, Task: inst.Task, Error: errMsg, Repeats: e.repeats}
	f.FoundationGap = e.store.FoundationMissing()
	if p, err := e.store.Progress.Load(); err == nil && p != nil {
		f.Phase = string(p.Phase)
		f.NextChapter = p.NextChapter()
		f.PendingQueue = p.PendingRewrites
	}
	// writer 目标章的 FSM 阶段与要求动作(ResolveChapterStage 现场解析,用与
	// 生产一致的 e.fsmConfig;读失败或非 writer 任务时留空,best-effort 不阻断裁定)。
	if inst.Chapter > 0 {
		if d, err := tools.ResolveChapterStage(e.store, inst.Chapter, e.fsmConfig); err == nil {
			f.Stage = string(d.Stage)
			f.RequiredAction = string(d.Required)
		}
	}
	return f
}

func (e *engine) recordFailureDecision(kind string, inst *flow.Instruction, facts arbiter.FailureFacts, d arbiter.FailureDecision, derr error) {
	rec := storepkg.DecisionRecord{Kind: kind, Decider: "arbiter", Input: inst.Agent + ": " + inst.Task, Reason: d.Reason}
	if data, err := json.Marshal(facts); err == nil {
		rec.Facts = data
	}
	if derr == nil {
		if data, err := json.Marshal(d); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	if _, err := e.store.Decisions.Append(rec); err != nil {
		slog.Warn("裁定审计落盘失败", "module", "engine", "kind", kind, "err", err)
	}
}

// applyPendingOps 在循环边界提交干预的控制态动作;循环排空——同步重询
// (reconsult)会在应用过程中追加新动作,必须在本边界内消化完,否则中间会
// 多派一个 worker(干预必须先于后续创作生效)。
// 返回是否有 hold+dispatch 必须先执行配对派单；该情况下调用方暂缓 Gate 检查。
func (e *engine) applyPendingOps(ctx context.Context) (deferGate bool) {
	for {
		e.mu.Lock()
		ops := e.pending
		e.pending = nil
		e.mu.Unlock()
		if len(ops) == 0 {
			return deferGate
		}
		for _, op := range ops {
			pairedHoldDispatch := op.hold != nil && !op.hold.Cancel && op.dispatch != nil
			err := e.applyControlOp(ctx, op)
			if err != nil {
				// 动作持久化失败:host 已按"入队成功"清除 PendingSteer,
				// 这里回存整条干预,恢复/继续时重新裁定重试(动作幂等 + 重询按新事实)。
				if op.text != "" {
					if serr := e.store.RunMeta.SetPendingSteer(op.text); serr != nil {
						slog.Warn("干预回存失败", "module", "engine", "err", serr)
					}
				}
				e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
					Summary: "干预动作执行失败,已保留;恢复/继续时自动重试"})
			} else if pairedHoldDispatch && e.nextDefersGate() {
				// 只有 hold 与配对派单都成功落地，才允许绕过本次 Gate。
				// hold 写入失败或派单因事实过期被丢弃时继续绕过，都会让
				// 未受保护的 Worker 前进。
				deferGate = true
			}
		}
	}
}

// applyControlOp 执行单个控制态动作(hold 直写 RunMeta、reopen 调工具内核、dispatch 先对账)。
// 引擎未运行时由 host 在干预路径直接调用;返回首个持久化失败(调用方据此决定是否
// 保留 PendingSteer 供恢复重放)。
func (e *engine) applyControlOp(ctx context.Context, op controlOp) error {
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	if op.dispatch != nil {
		// Expect 必须在 hold 等配对动作落盘前核对。否则派单过期后旧 hold
		// 会残留，并与按新事实重新裁定出的 hold 冲突，最终只暂停却漏做修改。
		fresh := arbiter.CollectInterventionFacts(e.store)
		if fresh.Phase != op.facts.Phase || fresh.Flow != op.facts.Flow ||
			fresh.QueueHead() != op.facts.QueueHead() {
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: "裁定派单已过时(事实推进),以最新事实重新裁定"})
			e.recordStale(op)
			if op.text != "" && e.reconsult != nil {
				// 同步重询:干预必须先于后续创作生效——异步会让引擎在新裁定
				// 落地前又派一个 worker。新动作由 applyPendingOps 在本边界排空。
				e.reconsult(op.text)
			}
			return nil
		}
	}
	if op.hold != nil {
		if op.hold.Cancel {
			meta, err := e.store.RunMeta.Load()
			if err != nil {
				e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "读取一次性暂停失败: " + err.Error(), Level: "error"})
				return err
			}
			if meta != nil && meta.AdvanceHold != nil {
				if err := e.store.RunMeta.ClearAdvanceHold(*meta.AdvanceHold); err != nil {
					e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "取消一次性暂停失败: " + err.Error(), Level: "error"})
					return err
				}
			}
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "已取消一次性暂停", Level: "info"})
		} else {
			hold := domain.AdvanceHold{After: op.hold.After, Reason: op.hold.Reason}
			if err := e.store.RunMeta.SetAdvanceHold(hold); err != nil {
				e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "设置一次性暂停失败: " + err.Error(), Level: "error"})
				return err // hold 未落盘时关联 dispatch 不得执行
			}
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "已设置一次性暂停: " + op.hold.Reason, Level: "info"})
		}
	}
	if op.reopen != nil {
		args, _ := json.Marshal(map[string]any{"chapters": op.reopen.Chapters, "reason": op.reopen.Reason})
		if _, err := tools.NewReopenBookTool(e.store).Execute(ctx, args); err != nil {
			e.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "重开返工失败: " + err.Error(), Level: "error"})
			fail(err)
		} else {
			e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
				Summary: fmt.Sprintf("已重开全书返工: 第 %v 章入队", op.reopen.Chapters), Level: "info"})
		}
	}
	if op.dispatch != nil {
		// Expect 已在任何配对状态写入前核对。CheckpointSeq 只留审计不参与
		// 对账：干预到达时 worker 多半正在跑，seq 必然推进。
		e.mu.Lock()
		// 已知窗口(best-effort 边界,见 engine-arbiter.md 澄清③):派单自此存于内存,
		// worker 启动前被硬杀(kill -9,defer 不执行)会丢失本次派单意图——
		// 正常退出/Abort 由 run 的 defer 回存 PendingSteer 兜底。
		e.next = &flow.Instruction{Agent: op.dispatch.Agent, Task: op.dispatch.Task, Reason: "用户干预裁定"}
		e.deferGateForNext = op.hold != nil && !op.hold.Cancel
		e.mu.Unlock()
	}
	return firstErr
}

func (e *engine) recordStale(op controlOp) {
	rec := storepkg.DecisionRecord{Kind: "decision_stale", Decider: "engine", Input: op.text}
	if data, err := json.Marshal(op.facts); err == nil {
		rec.Facts = data
	}
	if _, err := e.store.Decisions.Append(rec); err != nil {
		slog.Warn("stale 记录失败", "module", "engine", "err", err)
	}
}

// pauseWithNotify 引擎自主暂停(僵局熔断/失败裁定):离屏通知 + 走 Host 统一
// 的结构化停止原因；生产路径在当前边界结束后停机，旧测试回调仍可使用 abort 语义。
func (e *engine) pauseWithNotify(kind, body string) {
	e.notify(kind, "warn", "ainovel: 引擎暂停", body)
	req := pauseRequest{category: pauseCategoryForKind(kind), code: kind, summary: body, level: "warn"}
	if e.onPauseStructured != nil {
		e.onPauseStructured(req)
		return
	}
	if e.onPause != nil {
		e.onPause(body)
		return
	}
	e.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: body, Level: "warn"})
	e.abort()
}

func pauseCategoryForKind(kind string) domain.StopCategory {
	switch kind {
	case notify.KindAdvanceGate:
		return domain.StopCategoryReviewGate
	case notify.KindDeadlock:
		return domain.StopCategoryFailureBreaker
	case notify.KindWorkerFailure, notify.KindPlanStart:
		return domain.StopCategoryDecisionFailed
	case "backup":
		return domain.StopCategoryDeterministicErr
	case "engine":
		return domain.StopCategoryStateError
	default:
		return domain.StopCategoryUnknown
	}
}

// blockedWithNotify 章节无进展熔断停机（P1-9 阻塞项 9.1）：把确定性熔断原因
// （含 chapter/stage/required/errorCode/ledger，manual_recovery_required 标记）
// 输出到用户通道——复用 pauseWithNotify 的既有通知通道（SYSTEM event + 离屏
// 通知 + host 统一暂停语义），headless 下同样可见明确失败原因。
func (e *engine) blockedWithNotify(inst *flow.Instruction) {
	msg := "章节无进展熔断（manual_recovery_required）: "
	if e.noProgress != nil {
		if reason := e.noProgress.BlockedReason(inst.Chapter); reason != "" {
			msg += reason
		} else {
			msg += fmt.Sprintf("chapter=%d 连续多轮同一状态无任何 checkpoint/草稿/账本变化，已停止自动重派", inst.Chapter)
		}
	} else {
		msg += fmt.Sprintf("chapter=%d 无进展，已停止自动重派", inst.Chapter)
	}
	msg += "。请人工修复 ledger/草稿/候选状态后继续创作"
	e.pauseWithNotify(notify.KindDeadlock, msg)
}

// latestCompletedChapter 返回进度中最新完成的章节号。
// err 表示进度脏读（pause/stop）, 返回 0。
func (e *engine) latestCompletedChapter() (int, error) {
	p, err := e.store.Progress.Load()
	if err != nil {
		return 0, err
	}
	if p == nil || len(p.CompletedChapters) == 0 {
		return 0, nil
	}
	return p.CompletedChapters[len(p.CompletedChapters)-1], nil
}

// handleCompletedChapters 在 Worker 成功后检测是否有新完成的章节。
// 只在 after > before 时调用 CheckArcBoundary(after)；
// 卷末仅创建卷快照（弧快照由卷涵盖）。边界检查或备份出错时
// pauseWithNotify 并返回 true（跳过 budget/gate 处理）。
func (e *engine) handleCompletedChapters(_ context.Context, before int) bool {
	after, err := e.latestCompletedChapter()
	if err != nil {
		e.pauseWithNotify("backup", "进度读取失败，已暂停: "+err.Error())
		return true
	}
	if after <= before || after == 0 {
		return false
	}

	boundary, berr := e.store.Outline.CheckArcBoundary(after)
	if berr != nil {
		e.pauseWithNotify("backup", "弧边界检查失败，已暂停: "+berr.Error())
		return true
	}
	if boundary == nil {
		return false // 不在大纲中或不是弧末/卷末
	}

	if boundary.IsVolumeEnd {
		if e.backupVolume != nil {
			if berr := e.backupVolume(boundary.Volume); berr != nil {
				e.pauseWithNotify("backup", "卷边界备份失败，已暂停: "+berr.Error())
				return true
			}
		}
		return false
	}
	if boundary.IsArcEnd {
		if e.backupArc != nil {
			if berr := e.backupArc(boundary.Volume, boundary.Arc); berr != nil {
				e.pauseWithNotify("backup", "弧边界备份失败，已暂停: "+berr.Error())
				return true
			}
		}
		return false
	}
	return false
}

// isStyleReviewExhausted 检查指定章节的 style review 账本是否处于 legacy exhausted 状态。
// 当加载/校验失败时返回 error，调用方必须 fail-closed pause。
func (e *engine) isStyleReviewExhausted(chapter int) (bool, error) {
	meta, err := e.store.RunMeta.Load()
	if err != nil {
		return false, fmt.Errorf("load RunMeta: %w", err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return false, nil
	}
	ledger, err := e.store.StyleReview.Load(chapter)
	if err != nil {
		return false, fmt.Errorf("load style review ledger: %w", err)
	}
	if ledger == nil || ledger.IsEmpty() {
		return false, nil
	}
	return ledger.CurrentStatus() == domain.ReviewStatusExhausted, nil
}

// engineDone 返回引擎完成通知通道（nil 表示引擎未运行或已结束）。
func (e *engine) engineDone() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// completionSummary 完本的确定性收尾报告(store 已有全部事实,不花 LLM 调用;RFC 末节)。
func completionSummary(st *storepkg.Store) string {
	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		return "创作完成"
	}
	var b strings.Builder
	name := progress.NovelName
	if name == "" {
		name = "本书"
	}
	fmt.Fprintf(&b, "《%s》创作完成: 共 %d 章 %d 字", name, len(progress.CompletedChapters), progress.TotalWordCount)
	return b.String()
}
