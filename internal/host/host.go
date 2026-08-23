package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/agents"
	"github.com/voocel/ainovel-cli/internal/agents/ctxpack"
	"github.com/voocel/ainovel-cli/internal/arbiter"
	"github.com/voocel/ainovel-cli/internal/backup"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/host/exp"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	"github.com/voocel/ainovel-cli/internal/host/sim"
	modelreg "github.com/voocel/ainovel-cli/internal/models"
	"github.com/voocel/ainovel-cli/internal/notify"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/rules"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
	"github.com/voocel/ainovel-cli/internal/userrules"
)

// ── 测试钩子（零值时无操作，生产代码不设置） ──

var (
	// testBeforeArbiter 在 doIntervention 调用 Arbiter 前调用。
	// 返回 error 时会暂停引擎并跳过 Arbiter 调用。
	testBeforeArbiter func() error

	// testBeforeAsyncOp 在 trackSimOp/trackImpOp 读取异步操作通道前调用。
	testBeforeAsyncOp func() error

	// testBeforeImpRun 替换 ImportFrom 内部的 imp.Run 调用。
	// 非 nil 时跳过真实导入流程，返回提供的通道和 error。
	// 仅测试使用，生产代码不设置。
	testBeforeImpRun func(ctx context.Context) (<-chan imp.Event, error)

	// testBeforeSimRun 替换 Simulate/ImportSimulationProfile 内部的 sim.Run/sim.RunImport 调用。
	// 非 nil 时跳过真实仿写流程，返回提供的通道和 error。
	// 仅测试使用，生产代码不设置。
	testBeforeSimRun func(ctx context.Context) (<-chan sim.Event, error)
)

// Host 是运行时外壳:生命周期/干预入口/事件投影/模型管理。
// 调度与执行在 engine(确定性循环);语义裁定在 arbiter(LLM-as-function)。
type Host struct {
	cfg             bootstrap.Config
	bundle          assets.Bundle
	store           *storepkg.Store
	models          *bootstrap.ModelSet
	engine          *engine
	thinkingApplier agents.ApplyThinking // /model 调推理强度时联动各 Worker
	askUser         *tools.AskUserTool
	writerRestore   *ctxpack.WriterRestorePack
	userRules       *userrules.Service
	observer        *observer
	usage           *UsageTracker
	usageCtx        context.Context                   // autoSaveLoop 运行上下文（用于 stop-and-wait）
	usageCancel     context.CancelFunc                // 停掉 autoSaveLoop 并触发最后一次 flush
	budget          *BudgetSentinel                   // 预算政策；未启用为 nil（方法 nil 安全）
	gate            *ChapterAdvanceGate               // 章节许可与一次性暂停的统一政策组件
	notifier        *notify.Notifier                  // 无人值守告警；未启用为 nil（Send nil 安全）
	profile         projectprofile.ResolvedProfile    // 当前 workspace 项目档案（构造时解析一次）
	contract        *projectprofile.SceneBeatContract // 唯一不可变契约指针，原样传 workers/engine/import
	diagnosticOnly  bool                              // 只读诊断 Host（migration_required）；禁止所有 Provider 调用/文件写入

	events   chan Event
	streamCh chan string
	done     chan struct{}

	mu               sync.Mutex
	lifecycle        lifecycle
	lastStopReason   string // 最近一次停止原因；用于闲时调度避免重启故障停机
	lastStopCategory domain.StopCategory
	pendingStop      *pauseRequest
	stopRecorded     bool
	finalizing       bool // 正在把本代运行的终态写回 RunMeta；期间禁止新一代启动
	runGeneration    uint64
	cocreating       bool           // 阶段共创占用：paused 窗口内堵住 import/simulate/continue 的并发介入
	restoring        bool           // restore in progress; blocks startEngine/Resume/intervention
	activeOps        sync.WaitGroup // async import/simulation in-flight; Restore waits
	closeOnce        sync.Once

	interMu   sync.Mutex // 干预裁定 FIFO 串行(同一时刻至多一次在途咨询)
	runStopMu sync.Mutex // 串行化 RunMeta 终态落盘，避免 Close 与 onDone 重复收尾

	// runCtx 约束宿主侧的 LLM 裁定调用(启动裁定/干预分诊);Close 取消,
	// 避免退出时仍有裁定在途且无法中断。
	runCtx    context.Context
	runCancel context.CancelFunc
}

type lifecycle string

const (
	lifecycleIdle      lifecycle = "idle"
	lifecycleRunning   lifecycle = "running"
	lifecyclePaused    lifecycle = "paused"
	lifecycleCompleted lifecycle = "completed"
)

// New 创建 Host。
// 重要：
//  1. FillDefaults 后立即进行纯只读 profile preflight——即使 Provider config 损坏，
//     migration-required 的 workspace 也能得到 diagnostics-only Host。
//  2. ValidateBase 仅在 profile 不是 migration_required 时执行。
//  3. migration_required 时返回只读诊断 Host（零 Provider 调度、零状态写入、不初始化模型/Worker）。
func New(cfg bootstrap.Config, bundle assets.Bundle) (*Host, error) {
	cfg.FillDefaults()
	slog.Info("启动", "module", "boot", "provider", cfg.Provider, "model", cfg.ModelName, "output", cfg.OutputDir)

	// 第一步：纯只读 profile preflight —— 无 store.Init / model / goroutine / ValidateBase。
	// 即使 ValidateBase 因 Provider 损坏而失败，migration-required 也要得到诊断 Host。
	profile, profileErr := resolveProjectProfileEarly(cfg.OutputDir)
	if profileErr != nil {
		if IsMigrationRequired(profileErr) {
			// 注意：resolveProjectProfileEarly 在 migration_required 时同时返回 profile 和 error。
			// profile 此时已正确设置。
			return newDiagnosticHost(cfg, bundle, cfg.OutputDir, profile), nil
		}
		return nil, fmt.Errorf("resolve project profile: %w", profileErr)
	}
	slog.Info("项目档案已解析", "module", "boot", "contract", profile.Contract, "status", profile.Status)

	// 第二步：验证基础配置（只对非 migration-required 的普通项目执行）。
	if err := cfg.ValidateBase(); err != nil {
		return nil, err
	}

	// 第二步：正常初始化（此时 profile 保证不是 migration_required）
	modelreg.StartPricingRefresh(modelreg.DefaultRegistry(), bootstrap.DefaultConfigDir())

	store := storepkg.NewStore(cfg.OutputDir)
	if err := store.Init(); err != nil {
		// 复核阻塞项 3：构造失败路径释放已获取的 workspace 锁（Close 幂等；
		// 锁未获取时 Close 为空操作）。
		store.Close()
		return nil, fmt.Errorf("init store: %w", err)
	}
	// RunMeta 是所有控制语义的事实源，必须在构造模型/后台任务之前完成校验。
	// 未知 advance mode 直接返回结构化错误；禁止猜测降级后继续写盘。
	if err := store.RunMeta.Init(cfg.Style, cfg.Provider, cfg.ModelName); err != nil {
		store.Close()
		return nil, fmt.Errorf("init run meta: %w", err)
	}

	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("create models: %w", err)
	}
	slog.Info("模型就绪", "module", "boot", "summary", models.Summary())

	usage := NewUsageTracker(models, store)
	// 优先读 meta/usage.json；以下情况都走 sessions/*.jsonl 一次性回填：
	//   - 文件不存在（首次持久化前）
	//   - schema 版本不匹配（未来升级后丢弃旧格式）
	//   - 文件存在但损坏 / IO 错误（不能让坏数据让累计永久归零）
	// 回填完立即 SaveNow，把结果固化下来，下次启动直接 Load 命中。
	loaded, loadErr := usage.LoadFromStore()
	if loadErr != nil {
		slog.Warn("usage 加载失败，将尝试从 sessions 回填", "module", "usage", "err", loadErr)
	}
	if !loaded {
		if n, err := usage.ReplaySessions(cfg.OutputDir); err != nil {
			slog.Warn("usage replay 失败", "module", "usage", "err", err)
		} else if n > 0 {
			slog.Info("usage 从 session 回填完成", "module", "usage", "messages", n)
			if err := usage.SaveNow(); err != nil {
				slog.Warn("usage 回填后保存失败", "module", "usage", "err", err)
			}
		}
	}
	usageCtx, usageCancel := context.WithCancel(context.Background())
	usage.StartAutoSave(usageCtx)

	// onGuardBlock 前置声明:h 构造后才能挂事件浮出闭包。
	var onGuardBlock func(agent, reason string, consecutive int32)
	contract := projectprofile.ContractFor(profile.Contract)
	workers, askUser, restore, applyThinking := agents.BuildWorkers(cfg, store, models, bundle, usage.RecordRun,
		func(agent, reason string, consecutive int32) {
			if onGuardBlock != nil {
				onGuardBlock(agent, reason, consecutive)
			}
		}, contract)
	store.Signals.ClearStaleSignals()

	h := &Host{
		cfg:             cfg,
		bundle:          bundle,
		store:           store,
		models:          models,
		contract:        contract,
		thinkingApplier: applyThinking,
		askUser:         askUser,
		writerRestore:   restore,
		userRules:       userrules.NewService(store, agents.WithTrailingAntiRefusal(models.Default, store), rules.DefaultOptions()),
		usage:           usage,
		usageCtx:        usageCtx,
		usageCancel:     usageCancel,
		profile:         profile,
		events:          make(chan Event, 100),
		streamCh:        make(chan string, 256),
		done:            make(chan struct{}, 4),
		lifecycle:       lifecycleIdle,
	}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	h.observer = newObserver(store, h.emitEvent, h.emitDelta, h.emitClear)
	// 宿主侧 Arbiter 与 Worker 共用同一条 ToolProgress → observer → 工作台链路。
	h.runCtx = agentcore.WithToolProgress(h.runCtx, h.observer.workerProgress)
	if cfg.Notify.IsEnabled() {
		h.notifier = notify.New(cfg.Notify.Command, cfg.Notify.Events)
	}
	// 预算哨兵:Engine 在每轮循环边界直接调用 HandleBoundary(不再经事件订阅)。
	if sentinel := NewBudgetSentinel(cfg.Budget,
		func() float64 { c, _, _, _, _ := usage.Totals(); return c },
		func(reason string) { h.abortWithStop(domain.StopCategoryBudgetLimit, "budget_limit", reason, "error") },
		func(level, summary string) {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
			h.notifier.Send(notify.Notification{Kind: notify.KindBudget, Level: level, Title: "ainovel: 预算", Body: summary})
		},
	); sentinel != nil {
		h.budget = sentinel
		usage.SetOnCost(sentinel.OnCost)
		// 计费盲区告警：模型不报 usage 时成本恒 0，预算永不触发——保险丝没接上必须喊人。
		usage.SetOnMissingUsage(func() {
			const blind = "预算盲区: 模型未返回 usage 数据，成本统计为 0，预算上限不会触发（自定义模型请确认注册表价格或上游 include_usage）"
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: blind, Level: "warn"})
			h.notifier.Send(notify.Notification{Kind: notify.KindBudget, Level: "warn", Title: "ainovel: 预算", Body: blind})
		})
	}
	// 统一前进闸门：执行一次性 hold，并阻止 review 模式下无许可的新章。
	h.gate = NewChapterAdvanceGate(store,
		func(reason string) {
			h.abortWithStop(domain.StopCategoryAdvanceHold, "advance_hold", reason, "info")
			h.notifier.Send(notify.Notification{Kind: notify.KindAdvanceGate, Level: "info", Title: "ainovel: 等待验收", Body: reason})
		},
		func(level, summary string) {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
			h.notifier.Send(notify.Notification{Kind: notify.KindAdvanceGate, Level: level, Title: "ainovel: 章节推进", Body: summary})
		},
	)
	// StopGuard 拦截浮出：blocked 是高频自愈动作，只进屏内事件流（推送会刷屏）；
	// escalated / hard_stop 意味着本轮子任务报废，事件+notify 成对发出（架构 §2.3）。
	onGuardBlock = func(agent, reason string, n int32) {
		switch reason {
		case "escalated":
			body := fmt.Sprintf("%s 连续 %d 次空转未落盘必要产物，本轮任务终止，交回 Engine 处理", agent, n)
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Agent: agent, Summary: "StopGuard 升级: " + body, Level: "warn"})
			h.notifier.Send(notify.Notification{Kind: notify.KindStopGuard, Level: "warn", Title: "ainovel: StopGuard", Body: body})
		case "hard_stop":
			body := fmt.Sprintf("%s 遭 provider 拒答（safety/content_filter），本轮任务立即终止", agent)
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Agent: agent, Summary: "StopGuard 升级: " + body, Level: "warn"})
			h.notifier.Send(notify.Notification{Kind: notify.KindStopGuard, Level: "warn", Title: "ainovel: StopGuard", Body: body})
		default: // blocked
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Agent: agent,
				Summary: fmt.Sprintf("StopGuard: %s 未完成必要产物就试图结束，已拦截催促（连续第 %d 次）", agent, n), Level: "info"})
		}
	}
	// Engine:确定性执行引擎(docs/engine-rfc.md)。arbiter 用 Default 模型(过渡限制,
	// 见 engine-arbiter.md §4.2)。
	h.engine = &engine{
		store:           store,
		workers:         workers,
		contract:        contract,
		arbiterModel:    agents.WithTrailingAntiRefusal(newUsageTrackedModel(models.Default, usage.Record), store),
		failurePrompt:   bundle.Prompts.ArbiterFailure,
		planStartPrompt: bundle.Prompts.ArbiterPlanStart,
		style:           cfg.Style,
		// 与 BuildWorkers 注入六工具同源的完整 FSM 配置：反思/失败裁定解析的
		// stage/required 必须与真实工具拦截一致（P0-2）。
		fsmConfig:      agents.ChapterFSMConfigFor(cfg, models),
		migrationCheck: h.checkMigrationGate, // 引擎循环边界迁移门
		// 同步重询:阻塞引擎循环一次裁定(数秒),换取"干预先于后续创作生效"。
		reconsult: h.handleIntervention,
		observer:  h.observer,
		budget:    h.budget,
		gate:      h.gate,
		refresh:   h.refreshWriterRestore,
		emitEvent: h.emitEvent,
		notify: func(kind, level, title, body string) {
			h.notifier.Send(notify.Notification{Kind: kind, Level: level, Title: title, Body: body})
		},
		onPause:           func(summary string) { h.abortWithStop(domain.StopCategoryUnknown, "engine_pause", summary, "warn") },
		onPauseStructured: func(req pauseRequest) { h.handleEnginePause(req) },
		onDone:            h.runEnded,
		backupArc: func(volume, arc int) error {
			slog.Info("弧边界备份", "module", "host", "volume", volume, "arc", arc)
			source := h.store.Dir()
			_, berr := backup.Backup(source, h.projectID(), backup.KindArc, volume, arc)
			return berr
		},
		backupVolume: func(volume int) error {
			slog.Info("卷边界备份", "module", "host", "volume", volume)
			source := h.store.Dir()
			_, berr := backup.Backup(source, h.projectID(), backup.KindVolume, volume, 0)
			return berr
		},
	}

	return h, nil
}

// ── 生命周期 ──

// PrepareUserRules 在新建模式下生成本书用户规则快照（启动侧确定性，不进主创作 Run）。
//
// 入参是用户的**原始**创作要求（未经 BuildStartPrompt 包装）——归一化要的是用户规则本身，
// 不是启动脚手架。入口须在 StartPrepared 之前调用一次（quick/cocreate 两条新建路径都走这里）。
//
// 归一化失败只降级不报错（增强路径）；只有快照无法落盘才返回 error 中止开书——
// 后续运行将没有稳定事实源（见设计 §失败与降级）。
func (h *Host) PrepareUserRules(rawPrompt string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return fmt.Errorf("恢复操作进行中")
	}
	svc := userrules.NewService(h.store, agents.WithTrailingAntiRefusal(h.models.Default, h.store), rules.DefaultOptions())
	snap, err := svc.Build(context.Background(), rawPrompt)
	if err != nil {
		return fmt.Errorf("用户规则快照落盘失败，无法继续: %w", err)
	}
	logUserRulesSnapshot(snap)
	return nil
}

// ensureUserRules 在恢复路径确保快照存在；缺失时按
// system_defaults + rules 文件生成。
func (h *Host) ensureUserRules() {
	svc := userrules.NewService(h.store, agents.WithTrailingAntiRefusal(h.models.Default, h.store), rules.DefaultOptions())
	snap, err := svc.GetOrBuild(context.Background())
	if err != nil {
		slog.Warn("用户规则快照读取/生成失败，运行时将退到内置默认", "module", "rules", "err", err)
		return
	}
	logUserRulesSnapshot(snap)
}

// logUserRulesSnapshot 启动回显：让用户看到系统把规则理解成了什么（复用日志，不新增机制）。
func logUserRulesSnapshot(snap *rules.Snapshot) {
	if snap == nil {
		return
	}
	slog.Info("用户规则快照",
		"module", "rules",
		"status", string(snap.Status),
		"来源", snap.Sources,
		"禁用短语", len(snap.Structured.ForbiddenPhrases),
		"疲劳词", len(snap.Structured.FatigueWords),
	)
	if snap.Status == rules.StatusDegraded {
		slog.Warn("部分规则未能解析，已按 raw preferences 运行（可重新生成快照）",
			"module", "rules", "uncertain", snap.Uncertain)
	}
}

// StartPrepared 用用户的**原始**创作要求开始创作:plan_start 裁定选规划师并扩充
// 需求，裁定结果先固化为
// 事实(PlanStartRecord)再启动 Engine——恢复永远依赖已落盘事实,不重做已有裁定。
// 输入事实(StartPrompt)在裁定之前落盘:裁定失败时它是引擎补裁的依据,
// 启动失败可从任何恢复入口(Resume/继续)自愈,不是死局。
func (h *Host) StartPrepared(rawRequirement string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	h.mu.Lock()
	if h.lifecycle == lifecycleRunning {
		h.mu.Unlock()
		return fmt.Errorf("already running")
	}
	if h.cocreating {
		h.mu.Unlock()
		return fmt.Errorf("阶段共创进行中，请先结束共创")
	}
	if h.restoring {
		h.mu.Unlock()
		return fmt.Errorf("恢复操作进行中")
	}
	h.mu.Unlock()

	rawRequirement = strings.TrimSpace(rawRequirement)
	if rawRequirement == "" {
		return fmt.Errorf("prompt is required")
	}
	if err := h.budget.Refuse(); err != nil {
		return err
	}
	if err := h.store.Checkpoints.Reset(); err != nil {
		return fmt.Errorf("reset checkpoints: %w", err)
	}
	if err := h.store.Progress.Init("", 0); err != nil {
		return fmt.Errorf("init progress: %w", err)
	}
	// 输入事实先于裁定落盘:裁定失败(模型故障等)后 StartPrompt 仍在,
	// 恢复/继续时引擎据此补裁(planStartFallback),启动失败不再是死局。
	if err := h.store.RunMeta.SetStartPrompt(rawRequirement); err != nil {
		return fmt.Errorf("记录创作需求: %w", err)
	}
	// 启动裁定:失败显式报错中止(启动期用户在场,报错优于猜测)。
	start := time.Now()
	decision, derr := runObservedDecision(h.observer, "启动裁定", func() (arbiter.PlanStartDecision, error) {
		return arbiter.DecidePlanStart(h.runCtx, h.arbiterModel(),
			h.bundle.Prompts.ArbiterPlanStart, rawRequirement, h.cfg.Style)
	})
	rec := storepkg.DecisionRecord{Kind: "plan_start", Decider: "arbiter", Input: rawRequirement,
		Reason: decision.Reason, DurationMs: time.Since(start).Milliseconds()}
	if derr == nil {
		if data, err := json.Marshal(decision); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	var recErr error
	if rec, recErr = h.store.Decisions.Append(rec); recErr != nil {
		slog.Warn("启动裁定审计落盘失败", "module", "host", "err", recErr)
	}
	if derr != nil {
		return fmt.Errorf("启动裁定失败: %w", derr)
	}
	if err := h.store.RunMeta.SetPlanStart(domain.PlanStartRecord{
		RawPrompt: rawRequirement, Planner: decision.Planner, PlannerTask: decision.Task, DecisionID: rec.ID,
	}); err != nil {
		return fmt.Errorf("记录启动裁定: %w", err)
	}

	slog.Info("开始创作", "module", "host", "planner", decision.Planner)
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
		Summary: fmt.Sprintf("开始创作（规划师: %s——%s）", decision.Planner, decision.Reason), Level: "info"})
	if !h.startEngineLocked(&flow.Instruction{Agent: decision.Planner, Task: decision.Task, Reason: decision.Reason}) {
		return fmt.Errorf("Engine 已在运行或正在停止，无法启动新书")
	}
	return nil
}

// startEngine 为未持 interMu 的调用方提供互斥保护后调用 startEngineLocked。
// Resume/Continue/StartPrepared/AdvanceOneChapter 等已持 interMu 的调用方
// 直接调 startEngineLocked。
func (h *Host) startEngine(initial *flow.Instruction, origins ...domain.RunOrigin) bool {
	h.interMu.Lock()
	defer h.interMu.Unlock()
	return h.startEngineLocked(initial, origins...)
}

// startEngineLocked 假设 interMu 已持有，直接检查恢复/运行/共创并启动引擎引擎。
func (h *Host) startEngineLocked(initial *flow.Instruction, origins ...domain.RunOrigin) bool {
	return h.startEngineLockedAs(initial, runOriginOrManual(origins...), nil)
}

func (h *Host) startEngineLockedAs(initial *flow.Instruction, origin domain.RunOrigin, permit *domain.ResumePermit) bool {
	if err := h.checkMigrationGate(); err != nil {
		slog.Warn("startEngine 被迁移门拦截", "module", "host", "err", err)
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restoring {
		slog.Warn("startEngine 被恢复操作拦截", "module", "host")
		return false
	}
	if h.cocreating {
		slog.Warn("startEngine 被共创占用拦截", "module", "host")
		return false
	}
	if h.finalizing {
		slog.Warn("startEngine 被运行终态收尾拦截", "module", "host")
		return false
	}
	if h.engine.isRunning() {
		return false
	}
	if h.lifecycle == lifecycleCompleted {
		return false
	}
	generation, err := h.store.RunMeta.BeginRun(origin, time.Now().Format(time.RFC3339), permit)
	if err != nil {
		slog.Warn("startEngine 运行控制记录失败", "module", "host", "err", err)
		return false
	}
	h.observer.setAborting(false)
	previous := h.lifecycle
	h.lifecycle = lifecycleRunning
	h.lastStopReason = ""
	h.lastStopCategory = ""
	h.pendingStop = nil
	h.stopRecorded = false
	h.runGeneration = generation
	if !h.engine.start(initial) {
		h.lifecycle = previous
		_ = h.store.RunMeta.FinishRun(domain.RunStopRecord{
			Generation: generation,
			Category:   domain.StopCategoryStartFailed,
			Code:       "engine_start",
			Summary:    "Engine 启动失败",
			StoppedAt:  time.Now().Format(time.RFC3339),
		}, nil)
		return false
	}
	return true
}

func runOriginOrManual(origins ...domain.RunOrigin) domain.RunOrigin {
	if len(origins) > 0 && origins[0].Valid() {
		return origins[0]
	}
	return domain.RunOriginManual
}

// Resume 恢复模式：从 checkpoint + progress 生成 resume prompt 并启动。
func (h *Host) Resume() (string, error) {
	if err := h.checkMigrationGate(); err != nil {
		return "", err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	return h.resumeLocked()
}

func (h *Host) resumeLocked() (string, error) {
	return h.resumeLockedAs(domain.RunOriginManual, nil)
}

// ResumeForTUI 是 TUI 启动时的自动恢复入口。只有持久化的、代次/来源匹配的
// ResumePermit 才能自动启动；旧项目、待裁决项目和未知状态均 fail closed，
// 返回工作台让用户人工确认。高峰窗口内的许可继续等待，不隐式绕过策略。
func (h *Host) ResumeForTUI(now time.Time) (label string, deferred bool, err error) {
	if err := h.checkMigrationGate(); err != nil {
		return "", false, err
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return "", false, err
	}
	label, err = resumeLabel(h.store)
	if err != nil || label == "" {
		return label, false, err
	}
	progress, progressErr := h.store.Progress.Load()
	if progressErr != nil {
		return label, false, progressErr
	}
	if startupResumeBlocked(meta, progress) {
		return label, true, nil
	}
	if meta == nil || meta.Control == nil || meta.Control.AutoResume == nil {
		if startupResumeDeferred(meta, now) {
			return label, true, nil
		}
		// 兼容旧项目及普通自然中断：旧版启动入口没有要求
		// RunControl.AutoResume，仍按原语义从已落盘事实自动续跑。
		h.interMu.Lock()
		defer h.interMu.Unlock()
		label, err = h.resumeLockedAs(domain.RunOriginManual, nil)
		return label, false, err
	}
	permit := *meta.Control.AutoResume
	if permit.Generation != meta.Control.Generation || permit.Origin == "" || !permit.Origin.Valid() {
		return label, true, nil
	}
	if meta.PendingSteer != "" {
		return label, true, nil
	}
	if !resumePermitDue(permit, now) {
		return label, true, nil
	}
	if permit.Origin == domain.RunOriginIdleScheduler && !meta.IdleWritingEnabled {
		_ = h.store.RunMeta.ClearIdleResumePermit()
		return label, true, nil
	}
	if !h.scheduledResumeAllowed(meta, permit, now) {
		return label, true, nil
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	label, err = h.resumeLockedAs(permit.Origin, &permit)
	return label, false, err
}

// resumeLockedAs 是 Resume 的实际实现；调用方必须持有 interMu。
func (h *Host) resumeLockedAs(origin domain.RunOrigin, permit *domain.ResumePermit) (string, error) {
	h.mu.Lock()
	if h.lifecycle == lifecycleRunning {
		h.mu.Unlock()
		return "", fmt.Errorf("already running")
	}
	if h.cocreating {
		h.mu.Unlock()
		return "", fmt.Errorf("阶段共创进行中，请先结束共创")
	}
	if h.restoring {
		h.mu.Unlock()
		return "", fmt.Errorf("恢复操作进行中，请稍后再试")
	}
	h.mu.Unlock()

	label, err := resumeLabel(h.store)
	if err != nil {
		return "", err
	}
	if label == "" {
		return "", nil // 新建模式，无恢复
	}
	if err := h.budget.Refuse(); err != nil {
		return "", err
	}

	slog.Info("恢复创作", "module", "host", "label", label)
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "恢复创作: " + label, Level: "info"})
	for _, w := range h.store.CheckConsistency() {
		slog.Warn("一致性告警", "module", "host", "detail", w)
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "一致性告警: " + w, Level: "warn"})
	}
	// 确保用户规则快照存在；已有则廉价读取。
	h.ensureUserRules()
	h.refreshWriterRestore()
	// 待处理干预(停机期留下的/裁定期崩溃残留的)必须先于引擎续跑裁定——
	// 否则引擎可能抢在裁定前继续写出与干预相悖的章节。但恢复入口不应被
	// Arbiter/网络阻塞：先返回项目界面，后台裁定；裁定完成后再按 restart=true
	// 拉起引擎。无待处理干预 → 直接续跑。
	meta, _ := h.store.RunMeta.Load()
	pendingSteer := ""
	if meta != nil {
		pendingSteer = meta.PendingSteer
	}
	// 纯“继续”只表达恢复意图，不包含需要 Arbiter 解释的事实。若它在退出/取消
	// 竞态中残留，启动时再次送给 Arbiter 会让 TUI 卡在欢迎页等待模型重试，甚至
	// 每次关闭后形成永久恢复循环。原子清掉它并直接恢复；任何有实际内容的干预
	// 仍严格保留“先裁定、后续跑”的原语义。
	if isPlainResumeSteer(pendingSteer) {
		if err := h.store.ClearHandledSteer(); err != nil {
			return label, fmt.Errorf("清除残留继续指令: %w", err)
		}
		pendingSteer = ""
		slog.Info("已清除残留的纯继续指令", "module", "host")
	}
	if permit != nil && pendingSteer != "" {
		return label, nil
	}
	if pendingSteer != "" {
		h.resumePendingIntervention(pendingSteer)
	} else {
		// 只恢复事实,不恢复会话(RFC §6):Engine 从 store 重算路由续跑。
		if !h.startEngineLockedAs(nil, origin, permit) {
			return label, fmt.Errorf("Engine 正在完成上一轮停止，请稍后重试恢复")
		}
	}
	// lifecycle 由 startEngine / runEnded 管理,此处不再覆写——
	// 引擎立即结束(完本等)时覆写会把终态改回 running。
	return label, nil
}

// ScheduleReconcileResult 是 TUI 调度 tick 的 Host 裁定结果。
type ScheduleReconcileResult struct {
	Started        bool
	PauseRequested bool
}

// ReconcileSchedule 统一处理高峰暂停和持久化恢复许可；TUI 只负责定时调用。
func (h *Host) ReconcileSchedule(now time.Time) (ScheduleReconcileResult, error) {
	if err := h.checkMigrationGate(); err != nil {
		return ScheduleReconcileResult{}, err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	var result ScheduleReconcileResult
	if IdleWritingStatusAt(now).InPeak {
		result.PauseRequested = h.requestPeakPauseLocked(now)
	}
	if result.PauseRequested {
		return result, nil
	}
	started, err := h.startScheduledRunLocked(now)
	result.Started = started
	return result, err
}

// StartIdleWriting 在北京时间非高峰时段尝试消费一张自动恢复许可。
// 许可可以属于 idle_scheduler，也可以属于被高峰暂停的 manual 任务；
// 但待裁决、闸门、预算和故障状态不会生成许可，因此不会被此入口绕过。
func (h *Host) StartIdleWriting(now time.Time) (started bool, err error) {
	if err := h.checkMigrationGate(); err != nil {
		return false, err
	}

	h.interMu.Lock()
	defer h.interMu.Unlock()
	return h.startScheduledRunLocked(now)
}

func (h *Host) startScheduledRunLocked(now time.Time) (started bool, err error) {
	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return false, err
	}
	if meta == nil || meta.PendingSteer != "" || meta.AdvanceHold != nil {
		return false, nil
	}
	progress, err := h.store.Progress.Load()
	if err != nil {
		return false, err
	}
	if startupResumeBlocked(meta, progress) {
		return false, nil
	}

	var (
		origin domain.RunOrigin
		permit *domain.ResumePermit
	)
	if meta.Control == nil || meta.Control.AutoResume == nil {
		// 兼容旧项目：高峰启动时由 ResumeForTUI 延迟，窗口打开后由
		// 闲时调度器接力；仍要求用户开启闲时写作和 auto 推进。
		if !meta.IdleWritingEnabled || meta.AdvanceMode != domain.ChapterAdvanceAuto || IdleWritingStatusAt(now).InPeak {
			return false, nil
		}
		origin = domain.RunOriginIdleScheduler
	} else {
		p := *meta.Control.AutoResume
		if p.Generation != meta.Control.Generation || !resumePermitDue(p, now) {
			return false, nil
		}
		if p.Origin == domain.RunOriginIdleScheduler &&
			(!meta.IdleWritingEnabled || meta.AdvanceMode != domain.ChapterAdvanceAuto) {
			return false, nil
		}
		if !h.scheduledResumeAllowed(meta, p, now) {
			return false, nil
		}
		origin = p.Origin
		permit = &p
	}

	h.mu.Lock()
	blocked := h.lifecycle == lifecycleRunning || h.lifecycle == lifecycleCompleted || h.cocreating || h.restoring
	h.mu.Unlock()
	if blocked {
		return false, nil
	}

	label, err := h.resumeLockedAs(origin, permit)
	if err != nil {
		return false, err
	}
	if label == "" {
		return false, nil
	}
	return h.engine.isRunning(), nil
}

func resumePermitDue(permit domain.ResumePermit, now time.Time) bool {
	if permit.NotBefore == "" {
		return true
	}
	notBefore, err := time.Parse(time.RFC3339, permit.NotBefore)
	return err == nil && !now.Before(notBefore)
}

func (h *Host) scheduledResumeAllowed(meta *domain.RunMeta, permit domain.ResumePermit, now time.Time) bool {
	if meta == nil || !resumePermitDue(permit, now) {
		return false
	}
	schedule := IdleWritingStatusAt(now)
	if permit.Origin == domain.RunOriginIdleScheduler {
		return !schedule.InPeak && meta.IdleWritingEnabled
	}
	if !schedule.InPeak || !meta.PeakAutoPauseEnabled {
		return true
	}
	return peakOverrideActive(meta.Control, now)
}

func peakOverrideActive(control *domain.RunControl, now time.Time) bool {
	if control == nil || control.PeakOverrideUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, control.PeakOverrideUntil)
	return err == nil && now.Before(until)
}

func (h *Host) requestPeakPauseLocked(now time.Time) bool {
	if !IdleWritingStatusAt(now).InPeak {
		return false
	}
	h.mu.Lock()
	running := h.lifecycle == lifecycleRunning
	h.mu.Unlock()
	if !running {
		return false
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil || meta == nil {
		return false
	}
	control := meta.Control
	origin := domain.RunOriginManual
	if control != nil && control.Origin.Valid() {
		origin = control.Origin
	}
	if origin == domain.RunOriginManual &&
		(!meta.PeakAutoPauseEnabled || peakOverrideActive(control, now)) {
		return false
	}
	category := domain.StopCategoryPeakPolicy
	if origin == domain.RunOriginIdleScheduler {
		category = domain.StopCategoryIdleWindowEnd
	}
	schedule := IdleWritingStatusAt(now)
	req := pauseRequest{
		category: category,
		code:     string(category),
		summary:  "高峰时段已到，已请求在当前任务边界暂停",
		level:    "info",
	}
	if !schedule.NextTransition.IsZero() {
		req.notBefore = schedule.NextTransition.Format(time.RFC3339)
	}
	if !h.engine.requestPause(req) {
		return false
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: req.summary, Level: "info"})
	return true
}

// PauseForPeak 在北京时间高峰时间到达时暂停当前创作。
func (h *Host) PauseForPeak() bool {
	return h.ReconcilePeakPause(time.Now())
}

// ReconcilePeakPause 请求在当前 Worker 完成后暂停当前创作。
func (h *Host) ReconcilePeakPause(now time.Time) bool {
	if err := h.checkMigrationGate(); err != nil {
		return false
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	return h.requestPeakPauseLocked(now)
}

// PauseIdleWriting 保留闲时写作调度器的专用入口；同样请求安全边界暂停。
func (h *Host) PauseIdleWriting() bool {
	return h.ReconcilePeakPause(time.Now())
}

// StopIdleWriting 关闭闲时写作时暂停当前由该调度器启动的引擎。
func (h *Host) StopIdleWriting() bool {
	return h.abortWithStop(domain.StopCategoryIdleDisabled, "idle_disabled", "闲时写作已关闭，当前自动创作已暂停", "info")
}

func (h *Host) resumePendingIntervention(text string) {
	go func() {
		h.doIntervention(text, true)
		// 裁定失败(已回显并清除 pending)时也要恢复续跑——书不能因一条
		// 无法理解或模型临时失败的旧干预卡死在恢复入口。若动作持久化失败
		// 保留了 pending，也仍沿用旧语义恢复，让用户进项目后能继续干预。
		if !h.engine.isRunning() {
			if err := h.budget.Refuse(); err == nil {
				h.refreshWriterRestore()
				if !h.startEngine(nil) {
					h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
						Summary: "Engine 正在完成上一轮停止；干预已保存，请稍后继续"})
				}
			} else {
				h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: err.Error(), Level: "warn"})
			}
		}
	}()
}

// isPlainResumeSteer 只匹配不携带任何创作约束的控制词。保持白名单极窄，避免
// 把“继续，但……”一类真实干预误当成无信息指令而丢弃。
func isPlainResumeSteer(text string) bool {
	return strings.TrimSpace(text) == "继续"
}

// handleIntervention 用户干预的统一裁定路径:Collect → Decide → 执行。
// FIFO 串行(同一时刻至多一次在途咨询);answer/rules 即时执行,控制态动作
// (hold/reopen/dispatch)引擎运行中排队边界提交、停机时立即执行。
// restart=true(Continue 语义)时干预处理完确保引擎运行。
func (h *Host) handleIntervention(text string) {
	h.doIntervention(text, false)
}

func (h *Host) doIntervention(text string, restart bool) InterventionOutcome {
	h.interMu.Lock()
	defer h.interMu.Unlock()

	// 崩溃保护:裁定前先持久化(PendingSteer),成功应用或已当面回显失败后原子清除
	// (ClearHandledSteer 同时复位 FlowSteering)。裁定期间崩溃 → 下次 Resume 重放。
	if err := h.store.RunMeta.SetPendingSteer(text); err != nil {
		slog.Warn("干预持久化失败(继续裁定,但崩溃保护失效)", "module", "host", "err", err)
	}
	clearPending := func() {
		if err := h.store.ClearHandledSteer(); err != nil {
			slog.Warn("清除已处理干预失败", "module", "host", "err", err)
		}
	}

	// 测试钩子：在 Arbiter 调用前阻塞，用于验证 restore 互斥
	if testBeforeArbiter != nil {
		if err := testBeforeArbiter(); err != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: "干预跳过 (test hook): " + err.Error()})
			clearPending()
			return InterventionOutcome{OK: false, Failure: err}
		}
	}

	facts := arbiter.CollectInterventionFacts(h.store)
	facts.Running = h.engine.isRunning()

	start := time.Now()
	decision, derr := runObservedDecision(h.observer, "用户干预裁定", func() (arbiter.InterventionDecision, error) {
		return arbiter.DecideIntervention(h.runCtx, h.arbiterModel(),
			h.bundle.Prompts.ArbiterIntervention, facts, text)
	})

	rec := storepkg.DecisionRecord{Kind: "intervention", Decider: "arbiter", Input: text,
		Reason: decision.Reason, DurationMs: time.Since(start).Milliseconds()}
	if cp := h.store.Checkpoints.LatestGlobal(); cp != nil {
		rec.CheckpointSeq = cp.Seq
	}
	if data, err := json.Marshal(facts); err == nil {
		rec.Facts = data
	}
	if derr == nil {
		if data, err := json.Marshal(decision); err == nil {
			rec.Decision = data
		}
	} else {
		rec.Error = derr.Error()
	}
	if _, err := h.store.Decisions.Append(rec); err != nil {
		slog.Warn("裁定审计落盘失败", "module", "host", "err", err)
	}

	if derr != nil {
		// 宁可不动,不可误动:不产生任何写入。调用错误与
		// 输出校验错误共用同一 error 通道,必须原样回显,不得统一伪装成"未能理解"。
		// 已当面告知 → 清除 pending(否则下次 Resume 会自动重放同一条失败干预)。
		h.emitEvent(newInterventionFailureEvent(derr))
		clearPending()
		return InterventionOutcome{OK: false, Failure: derr}
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "裁定: " + decision.Reason, Level: "info"})
	if decision.Answer != "" {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: decision.Answer, Level: "info"})
	}
	// 任一动作持久化失败 → 保留 PendingSteer(恢复时整条重放重新裁定;
	// hold/reopen 幂等、dispatch 经新事实重询,重放安全)。
	actionsFailed := false
	if decision.Rules != "" {
		if snap, _, err := h.userRules.AddRuntimeRule(h.runCtx, decision.Rules); err != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "ERROR", Summary: "写作规则落盘失败: " + err.Error(), Level: "error"})
			actionsFailed = true
		} else if snap != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "写作规则已更新并持久化", Level: "info"})
		}
	}

	if decision.Hold != nil || decision.Reopen != nil || decision.Dispatch != nil {
		op := controlOp{hold: decision.Hold, reopen: decision.Reopen, dispatch: decision.Dispatch, text: text, facts: facts}
		if !h.engine.enqueue(op) {
			// 引擎未运行:立即执行;持久化失败 → 保留 PendingSteer,恢复时重放整条干预。
			if err := h.engine.applyControlOp(context.Background(), op); err != nil {
				h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
					Summary: "干预动作执行失败,已保留;恢复/继续时将自动重试"})
				return InterventionOutcome{OK: false, Failure: fmt.Errorf("干预动作执行失败，已保留: %w", err)}
			}
			// reopen/dispatch 表达了继续创作的意图,拉起引擎。
			if decision.Reopen != nil || decision.Dispatch != nil {
				restart = true
			}
		}
	}
	if actionsFailed {
		// 保留 PendingSteer:恢复/继续时整条重放重新裁定。
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
			Summary: "部分干预动作未成功,干预已保留;恢复/继续时自动重试"})
		return InterventionOutcome{OK: false, Failure: fmt.Errorf("部分干预动作未成功，干预已保留；恢复/继续时自动重试")}
	}
	// 动作已成功应用/入队,清除崩溃保护(入队后引擎侧失败或退出竞态由 engine
	// 回存 PendingSteer 兜底)。
	clearPending()

	if restart && !h.engine.isRunning() {
		if err := h.budget.Refuse(); err != nil {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: err.Error(), Level: "warn"})
			return InterventionOutcome{OK: true, EngineRunning: false, Failure: err}
		}
		h.refreshWriterRestore()
		if !h.startEngineLocked(nil) {
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: "Engine 正在完成上一轮停止；干预已保存，请稍后继续"})
			return InterventionOutcome{OK: true, EngineRunning: false,
				Failure: fmt.Errorf("Engine 正在完成上一轮停止，干预已保存")}
		}
	}
	return InterventionOutcome{OK: true, EngineRunning: h.engine.isRunning()}
}

func newInterventionFailureEvent(err error) Event {
	detail := err.Error()
	return Event{
		Time:     time.Now(),
		Category: "ERROR",
		Agent:    "arbiter",
		Summary:  "干预裁定失败：" + detail + "（未做任何修改）",
		Detail:   detail,
		Kind:     errorKind(err, detail),
		Level:    "error",
	}
}

// arbiterModel 返回带用量追踪的裁定模型(token/成本进预算与 usage 系统)。
func (h *Host) arbiterModel() agentcore.ChatModel {
	return agents.WithTrailingAntiRefusal(newUsageTrackedModel(h.models.Default, h.usage.Record), h.store)
}

// InterventionOutcome 一次干预处理的结果（同步调用方用；TUI 异步路径忽略）。
// 调用方据此区分三种语义：engine_started（OK 且 EngineRunning）、
// intervention_failed（!OK）、no_run（OK 但 EngineRunning=false）。
type InterventionOutcome struct {
	// OK 为 true 表示裁定成功且动作（规则/控制态）已应用。
	OK bool
	// Failure 干预失败原因（OK=false 时非 nil）；OK=true 但引擎未启动时为未启动原因。
	Failure error
	// EngineRunning 干预处理结束后引擎是否处于运行状态。
	EngineRunning bool
}

// Continue 停机后用户在输入框输入时调用:干预裁定 + 确保引擎重新运行。
func (h *Host) Continue(text string) error {
	if err := h.continueGuard(text); err != nil {
		return err
	}
	if isPlainResumeSteer(text) {
		// 精确“继续”是确定性的恢复控制词，不需要唤醒 Arbiter。
		// 这条路径仍会检查并拒绝未裁决的真实干预，避免把错误任务
		// 静默带入下一代运行。
		go h.continuePlain()
		return nil
	}
	go h.doIntervention(text, true)
	return nil
}

// ContinueAndWait 是 Continue 的同步变体：阻塞直到干预裁定、动作应用与引擎
// 启动尝试全部完成（不 fire-and-forget）。headless 自动化据此区分
// engine_started / intervention_failed / no_run。
func (h *Host) ContinueAndWait(text string) (InterventionOutcome, error) {
	if err := h.continueGuard(text); err != nil {
		return InterventionOutcome{}, err
	}
	if isPlainResumeSteer(text) {
		return h.continuePlain(), nil
	}
	return h.doIntervention(text, true), nil
}

// continuePlain 执行精确“继续”的确定性恢复。
// 调用方不需要再经过 Arbiter；但停机期留下的非纯继续干预必须先处理，
// 否则“继续”不能成为绕过待裁决错误的后门。调用方必须通过 Continue/ContinueAndWait
// 先完成 continueGuard，本函数内部负责干预互斥。
func (h *Host) continuePlain() InterventionOutcome {
	h.interMu.Lock()
	defer h.interMu.Unlock()

	fail := func(err error) InterventionOutcome {
		if err == nil {
			err = fmt.Errorf("继续恢复失败")
		}
		detail := err.Error()
		h.emitEvent(Event{
			Time:     time.Now(),
			Category: "ERROR",
			Agent:    "host",
			Summary:  "继续恢复失败：" + detail,
			Detail:   detail,
			Kind:     errorKind(err, detail),
			Level:    "error",
		})
		return InterventionOutcome{OK: false, Failure: err}
	}

	if h.engine.isRunning() {
		return fail(fmt.Errorf("引擎已在运行"))
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return fail(fmt.Errorf("读取运行元信息失败: %w", err))
	}
	if meta != nil && strings.TrimSpace(meta.PendingSteer) != "" && !isPlainResumeSteer(meta.PendingSteer) {
		return fail(fmt.Errorf("仍有未裁决干预 %q，请先处理该干预后再继续", meta.PendingSteer))
	}
	if meta != nil && meta.AdvanceHold != nil {
		if err := h.store.RunMeta.ClearAdvanceHold(*meta.AdvanceHold); err != nil {
			return fail(fmt.Errorf("取消一次性暂停失败: %w", err))
		}
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "已取消一次性暂停", Level: "info"})
	}
	// 恢复入口若在退出/取消竞态中留下纯“继续”，把它视为已处理，
	// 同时复位 FlowSteering；真实干预则在上面已被拒绝，不会被清掉。
	if meta != nil && meta.PendingSteer != "" {
		if err := h.store.ClearHandledSteer(); err != nil {
			return fail(fmt.Errorf("清除残留继续指令失败: %w", err))
		}
	}

	label, err := h.resumeLockedAs(domain.RunOriginManual, nil)
	if err != nil {
		return fail(err)
	}
	if label == "" {
		return InterventionOutcome{OK: true, EngineRunning: false}
	}
	return InterventionOutcome{OK: true, EngineRunning: h.engine.isRunning()}
}

// SetIdleWritingEnabled 切换闲时写作意图。它只持久化开关，不隐式启动或暂停
// 当前引擎；TUI 会在开关关闭且当前确由闲时调度运行时另行发起暂停。
func (h *Host) SetIdleWritingEnabled(enabled bool) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return fmt.Errorf("恢复操作进行中，无法切换闲时写作")
	}
	if err := h.store.RunMeta.SetIdleWritingEnabled(enabled); err != nil {
		return err
	}
	if !enabled {
		if err := h.store.RunMeta.ClearIdleResumePermit(); err != nil {
			return err
		}
	}
	label := "已关闭"
	if enabled {
		label = "已开启"
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "闲时写作" + label + "（北京时间高峰：09:00–12:00、14:00–18:00）", Level: "info"})
	return nil
}

// SetPeakAutoPauseEnabled 切换北京时间高峰时段的全局自动暂停意图。
// 它只持久化开关；TUI 调度器负责按时间检查并暂停当前 Engine。
func (h *Host) SetPeakAutoPauseEnabled(enabled bool) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return fmt.Errorf("恢复操作进行中，无法切换高峰自动暂停")
	}
	if err := h.store.RunMeta.SetPeakAutoPauseEnabled(enabled); err != nil {
		return err
	}
	if !enabled && h.engine.cancelPeakPolicyPause() {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "已取消尚未生效的高峰自动暂停请求", Level: "info"})
	}
	label := "已关闭"
	if enabled {
		label = "已开启"
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "高峰自动暂停" + label + "（北京时间：09:00–12:00、14:00–18:00）", Level: "info"})
	return nil
}

// SetPeakPauseSkip 跳过当前北京时间高峰窗口；窗口结束后自动失效。
// 它不关闭永久开关，也不隐式启动已经停止的 Engine。
func (h *Host) SetPeakPauseSkip(now time.Time) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	status := IdleWritingStatusAt(now)
	if !status.InPeak || status.NextTransition.IsZero() {
		return fmt.Errorf("当前不在北京时间高峰时段")
	}
	if meta, err := h.store.RunMeta.Load(); err == nil && meta != nil && meta.Control != nil && meta.Control.Origin == domain.RunOriginIdleScheduler {
		return fmt.Errorf("闲时调度任务在高峰必须暂停，/peak-pause skip 只对手动任务生效")
	}
	if err := h.store.RunMeta.SetPeakOverrideUntil(status.NextTransition.Format(time.RFC3339)); err != nil {
		return err
	}
	if h.engine.cancelPeakPolicyPause() {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "已取消尚未生效的本次高峰暂停请求", Level: "info"})
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: fmt.Sprintf("已跳过当前高峰自动暂停，本窗口至 %s", status.NextTransition.Format("15:04")), Level: "info"})
	return nil
}

// continueGuard 是 Continue / ContinueAndWait 共享的前置校验与事件回显。
func (h *Host) continueGuard(text string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("text is required")
	}
	h.mu.Lock()
	if h.cocreating {
		h.mu.Unlock()
		return fmt.Errorf("阶段共创进行中，请先结束共创")
	}
	if h.restoring {
		h.mu.Unlock()
		return fmt.Errorf("恢复操作进行中，请稍后再试")
	}
	h.mu.Unlock()
	if err := h.budget.Refuse(); err != nil {
		return err
	}
	if meta, err := h.store.RunMeta.Load(); err == nil && meta != nil && meta.PeakAutoPauseEnabled {
		status := IdleWritingStatusAt(time.Now())
		if status.InPeak && !peakOverrideActive(meta.Control, time.Now()) {
			until := status.NextTransition.Format("15:04")
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Level: "warn",
				Summary: fmt.Sprintf("当前为北京时间高峰（至 %s），高峰自动暂停已开启；本次继续允许启动，但会在当前任务安全边界暂停。持续运行请执行 /peak-pause skip，永久关闭请执行 /peak-pause off", until)})
		}
	}

	h.emitEvent(Event{Time: time.Now(), Category: "USER", Summary: "[继续] " + text, Level: "info"})
	return nil
}

// SetAdvanceMode 确定性切换章节推进模式。它只写入用户运行意图，
// 不调用 Arbiter，也不隐式启动已经暂停的 Engine。
func (h *Host) SetAdvanceMode(mode domain.ChapterAdvanceMode) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return fmt.Errorf("恢复操作进行中，无法切换推进模式")
	}
	if err := h.store.RunMeta.SetAdvanceMode(mode); err != nil {
		return err
	}
	label := "自动推进"
	if mode == domain.ChapterAdvanceReview {
		label = "逐章验收"
	}
	summary := "章节推进模式已切换为" + label
	h.mu.Lock()
	state := h.lifecycle
	h.mu.Unlock()
	if mode == domain.ChapterAdvanceAuto && state != lifecycleRunning && state != lifecycleCompleted {
		summary += "；当前仍暂停，输入继续指令后恢复运行"
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "info"})
	return nil
}

// AdvanceOneChapter 在逐章验收模式下授权一个精确章节并启动 Engine。
func (h *Host) AdvanceOneChapter() error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()

	h.mu.Lock()
	running, cocreating := h.lifecycle == lifecycleRunning, h.cocreating
	h.mu.Unlock()
	if running || h.engine.isRunning() {
		return fmt.Errorf("创作仍在运行或正在完成暂停，请稍后再执行 /next")
	}
	if cocreating {
		return fmt.Errorf("阶段共创进行中，请先结束共创")
	}
	if h.isRestoring() {
		return fmt.Errorf("恢复操作进行中，请稍后再执行 /next")
	}
	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("RunMeta 未初始化")
	}
	if meta.AdvanceMode != domain.ChapterAdvanceReview {
		return fmt.Errorf("/next 仅用于逐章验收模式，请先执行 /review on")
	}
	if meta.AdvanceHold != nil {
		return fmt.Errorf("仍有一次性暂停意图待处理（%s），请先恢复或完成当前干预", meta.AdvanceHold.Reason)
	}
	if err := h.budget.Refuse(); err != nil {
		return err
	}
	progress, err := h.store.Progress.Load()
	if err != nil {
		return err
	}
	if progress == nil || progress.Phase != domain.PhaseWriting {
		phase := "<nil>"
		if progress != nil {
			phase = string(progress.Phase)
		}
		return fmt.Errorf("当前阶段不能授权新章（phase=%s）", phase)
	}
	target := progress.NextChapter()
	if target <= 0 {
		return fmt.Errorf("无法从当前进度推导下一章")
	}
	if err := h.store.RunMeta.GrantAdvancePermit(target); err != nil {
		return err
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
		Summary: fmt.Sprintf("已放行第 %d 章；该章提交后会先完成必要的评审与弧/卷结构维护，再次等待放行", target), Level: "info"})
	h.refreshWriterRestore()
	if !h.startEngineLocked(nil) {
		// 许可按章节号持久化且同目标幂等，调用方稍后重试不会重复授权。
		return fmt.Errorf("章节许可已保存，但 Engine 仍在完成上一轮停止；请稍后重试 /next")
	}
	return nil
}

// SetStyleReviewMode 切换风格评审质量模式。它只写入用户运行意图，
// 不修改已有风格评审账本或草稿内容。off 关闭评审，critic 启用批评模式。
// 使用 h.store.RunMeta.SetStyleReviewMode 持久化，验证请求值有效，
// 发射相应状态事件。
func (h *Host) SetStyleReviewMode(mode domain.StyleQualityMode) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	if !mode.Valid() {
		return fmt.Errorf("不支持的风格评审模式 %q，可用值：off, critic", mode)
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return fmt.Errorf("恢复操作进行中，无法切换风格评审模式")
	}
	if err := h.store.RunMeta.SetStyleReviewMode(mode); err != nil {
		return err
	}
	label := "已关闭"
	if mode.Enabled() {
		label = "批评模式"
	}
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
		Summary: "风格评审模式已切换为" + label, Level: "info"})
	return nil
}

// StyleReviewOverride 覆盖已耗尽的风格评审账本，追加 overridden 条目并允许后续提交。
// chapter 必须 > 0，reason 必须非空。仅当账本当前状态为 exhausted 时允许操作。
// 使用当前草稿摘要和基础摘要执行 append-only 覆盖。
//
// 覆盖后评审处于 overridden terminal "快照权威"状态：后续基础配置变更不阻挡 commit。
// 操作成功后调用方应指示用户运行 /continue 以恢复 Writer 创作。
func (h *Host) StyleReviewOverride(chapter int, reason string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	if chapter <= 0 {
		return fmt.Errorf("章节号必须大于 0")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("覆盖原因不能为空")
	}

	// 1. 加载账本
	ledger, err := h.store.StyleReview.Load(chapter)
	if err != nil {
		return fmt.Errorf("加载风格评审账本: %w", err)
	}
	if ledger == nil || ledger.IsEmpty() {
		return fmt.Errorf("第 %d 章尚无风格评审账本", chapter)
	}
	if ledger.CurrentStatus() != domain.ReviewStatusExhausted {
		return fmt.Errorf("第 %d 章当前评审状态 %q，仅 exhausted 可被覆盖", chapter, ledger.CurrentStatus())
	}

	// 2. 加载草稿并计算摘要
	content, _, err := h.store.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return fmt.Errorf("加载草稿: %w", err)
	}
	if content == "" {
		return fmt.Errorf("第 %d 章无草稿内容", chapter)
	}
	draftDigest := domain.DigestDraft(content)

	// 3. 计算基础摘要（使用固定版本标识，与 commit gate 校验一致）
	const overrideVersion = "override-v1"
	basisDigest := tools.ComputeBasisDigest(h.store, chapter, overrideVersion)

	// 4. Append-only 覆盖
	//    C1-H3：overridden 条目保留 exhausted 条目的 Epoch 与 Request.PolishCheckpointSeq
	//    ——commit gate 以当前 terminal 条目为权威读取绑定 seq，若覆盖丢掉了绑定，
	//    gate 会回退 legacy 时间比较导致判定错误。
	now := time.Now().Format(time.RFC3339)
	if err := h.store.StyleReview.Update(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, fmt.Errorf("账本已消失")
		}
		nextCycle := len(cur.Cycles) + 1
		var boundSeq int64
		if prev := cur.CurrentCycle(); prev != nil && prev.Request != nil {
			boundSeq = prev.Request.PolishCheckpointSeq
		}
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle:       nextCycle,
			Status:      domain.ReviewStatusOverridden,
			CreatedAt:   now,
			AttemptID:   fmt.Sprintf("override-%d-%d", chapter, time.Now().UnixNano()),
			Request:     &domain.StyleReviewRequest{Prompt: overrideVersion, PolishCheckpointSeq: boundSeq},
			DraftDigest: draftDigest,
			BasisDigest: basisDigest,
			Epoch:       cur.MaxEpoch(),
			// user override 不消耗内容/技术预算（style budget，计划 §9）。
			EventKind: domain.ReviewEventOverride,
			// Override 记录用户干预的审计轨迹
			Override: &domain.StyleReviewOverride{
				Actor:        "user",
				Reason:       reason,
				DraftDigest:  draftDigest,
				BasisDigest:  basisDigest,
				OverriddenAt: now,
			},
		})
		return cur, nil
	}); err != nil {
		return fmt.Errorf("追加 overridden 条目: %w", err)
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
		Summary: fmt.Sprintf("已覆盖第 %d 章风格评审（原因：%s），现在可以提交。请运行 /continue 恢复 Writer 创作", chapter, reason), Level: "info"})
	return nil
}

// Steer 提交用户干预(运行中随时可用;停机时裁定后视动作决定是否拉起引擎)。
func (h *Host) Steer(text string) {
	if err := h.checkMigrationGate(); err != nil {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
			Summary: "迁移未完成，无法执行干预: " + err.Error(), Level: "warn"})
		return
	}
	if h.isRestoring() {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
			Summary: "恢复操作进行中，无法执行干预", Level: "warn"})
		return
	}
	h.emitEvent(Event{Time: time.Now(), Category: "USER", Summary: "[用户干预] " + text, Level: "info"})
	go h.handleIntervention(text)
}

// Abort 暂停当前引擎循环。
func (h *Host) Abort() bool {
	return h.abortWithEvent("用户手动暂停当前创作", "warn")
}

// abortWithEvent 以指定原因事件执行暂停。预算停机与手动暂停共用同一停机机制，
// 仅事件文案不同（预算停机=用户预先签署的 Abort 指令，语义等同手动暂停）。
// isRestoring 检查恢复操作是否正在进行中（受 h.mu 保护）。
func (h *Host) isRestoring() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.restoring
}

// waitEngineCompletion 等待引擎 goroutine 完全退出（包括 deferred cleanup/onDone）。
// 阻塞等待 engine.done channel 关闭；channel 不存在等价于已结束。
func (h *Host) waitEngineCompletion() {
	if done := h.engine.engineDone(); done != nil {
		<-done
	}
}

func (h *Host) abortWithEvent(summary, level string) bool {
	return h.abortWithStop(domain.StopCategoryManualPause, "manual_pause", summary, level)
}

func (h *Host) abortWithStop(category domain.StopCategory, code, summary, level string) bool {
	h.mu.Lock()
	running := h.lifecycle == lifecycleRunning
	if running {
		h.lifecycle = lifecyclePaused
		h.lastStopReason = summary
		h.lastStopCategory = category
		h.pendingStop = &pauseRequest{generation: h.runGeneration, category: category, code: code, summary: summary, level: level}
		h.stopRecorded = false
	}
	h.mu.Unlock()
	if !running {
		return false
	}
	// 置位必须在 engine.abort 之前：cancel 传播会立刻引发 stream init / worker
	// 失败事件，observer 凭此标志识别为 abort 衍生噪声并抑制。
	h.observer.setAborting(true)
	h.engine.abort()
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level})
	return true
}

// handleEnginePause 收敛 Engine 在安全边界产生的结构化暂停。
// 这里不再 Abort：Engine 已经完成当前 Worker，返回 run loop 即可安全停机。
func (h *Host) handleEnginePause(req pauseRequest) {
	h.mu.Lock()
	running := h.lifecycle == lifecycleRunning
	if running {
		req.generation = h.runGeneration
		h.lifecycle = lifecyclePaused
		h.lastStopReason = req.summary
		h.lastStopCategory = req.category
		h.pendingStop = &req
		h.stopRecorded = false
	}
	h.mu.Unlock()
	if running && req.category != domain.StopCategoryPeakPolicy && req.category != domain.StopCategoryIdleWindowEnd {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: req.summary, Level: req.level})
	}
}

func (h *Host) recordRunStop(req pauseRequest) error {
	h.runStopMu.Lock()
	defer h.runStopMu.Unlock()

	h.mu.Lock()
	if h.stopRecorded {
		h.finalizing = false
		h.mu.Unlock()
		return nil
	}
	expectedGeneration := req.generation
	if expectedGeneration == 0 {
		expectedGeneration = h.runGeneration
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.finalizing = false
		h.mu.Unlock()
	}()

	meta, err := h.store.RunMeta.Load()
	if err != nil {
		return err
	}
	if meta == nil || meta.Control == nil {
		return fmt.Errorf("run control 未初始化")
	}
	control := meta.Control
	if expectedGeneration != 0 && control.Generation != expectedGeneration {
		return fmt.Errorf("运行代次不匹配: expected=%d actual=%d", expectedGeneration, control.Generation)
	}
	category := req.category
	if category == "" || !category.Valid() {
		category = domain.StopCategoryUnknown
	}
	if req.summary == "" {
		req.summary = "创作已停止"
	}
	stop := domain.RunStopRecord{
		Generation: expectedGeneration,
		Category:   category,
		Code:       req.code,
		Summary:    req.summary,
		StoppedAt:  time.Now().Format(time.RFC3339),
	}
	if stop.Generation == 0 {
		stop.Generation = control.Generation
	}
	var permit *domain.ResumePermit
	if category == domain.StopCategoryPeakPolicy || category == domain.StopCategoryIdleWindowEnd {
		notBefore := req.notBefore
		if notBefore == "" {
			schedule := IdleWritingStatusAt(time.Now())
			if schedule.InPeak && !schedule.NextTransition.IsZero() {
				notBefore = schedule.NextTransition.Format(time.RFC3339)
			}
		}
		if notBefore != "" && control.Origin.Valid() {
			permit = &domain.ResumePermit{
				Generation: control.Generation,
				Trigger:    domain.ResumeTriggerAfterPeak,
				Origin:     control.Origin,
				NotBefore:  notBefore,
			}
		}
	} else if category == domain.StopCategorySessionExit && control.Origin.Valid() {
		permit = &domain.ResumePermit{
			Generation: control.Generation,
			Trigger:    domain.ResumeTriggerNextOpen,
			Origin:     control.Origin,
			NotBefore:  time.Now().Format(time.RFC3339),
		}
	}
	if err := h.store.RunMeta.FinishRun(stop, permit); err != nil {
		return err
	}
	h.mu.Lock()
	h.stopRecorded = true
	h.lastStopReason = stop.Summary
	h.lastStopCategory = stop.Category
	h.mu.Unlock()
	return nil
}

// Close 终止引擎并关闭事件通道。
//
// Usage 持久化语义：先取消 autoSaveLoop（它自行 flush 最后一次 dirty 状态），
// 再补一次同步 SaveNow 收尾。终止后 in-flight LLM 调用的最末几百 token
// 丢失由下次启动时 session jsonl replay 自动补回。
func (h *Host) Close() {
	if h.diagnosticOnly {
		// 只读诊断 Host：绝不写入任何文件、不持久化 usage、不调用 engine/observer。
		// store 为只读模式（NewReadOnlyStore，无锁）；Close 为空操作（对称清理）。
		if h.store != nil {
			h.store.Close()
		}
		h.closeOnce.Do(func() {
			close(h.done)
			close(h.events)
			close(h.streamCh)
		})
		return
	}
	h.prepareSessionExit()
	if h.observer != nil {
		h.observer.setAborting(true)
	}
	if h.runCancel != nil {
		h.runCancel() // 中断在途的宿主侧裁定调用
	}
	if h.engine != nil {
		h.engine.abort()
	}
	h.usage.StopAutoSave(h.usageCancel)
	h.usageCancel = nil
	h.usageCtx = nil
	if err := h.usage.SaveNow(); err != nil {
		slog.Warn("usage 退出前落盘失败", "module", "usage", "err", err)
	}
	// 复核阻塞项 3：释放 workspace 排他锁（Host 生命周期结束；进程退出时 OS
	// 也会自动释放，此处保证同进程后续可重新获取——如测试的"先铺状态再开
	// Host"顺序模式）。
	if h.store != nil {
		h.store.Close()
	}
	h.closeOnce.Do(func() {
		close(h.done)
		close(h.events)
		close(h.streamCh)
	})
}

// prepareSessionExit 在关闭前固化 next_open 许可，避免随后关闭 store 后
// runEnded 无法再写入恢复事实。退出属于会话生命周期，不等同于人工暂停。
func (h *Host) prepareSessionExit() {
	h.mu.Lock()
	if h.lifecycle != lifecycleRunning || h.stopRecorded {
		h.mu.Unlock()
		return
	}
	h.lifecycle = lifecyclePaused
	h.finalizing = true
	req := pauseRequest{
		generation: h.runGeneration,
		category:   domain.StopCategorySessionExit,
		code:       "session_exit",
		summary:    "会话关闭，已保存下次打开恢复许可",
		level:      "info",
	}
	h.lastStopReason = req.summary
	h.lastStopCategory = req.category
	h.pendingStop = &req
	h.mu.Unlock()
	if err := h.recordRunStop(req); err != nil {
		slog.Warn("关闭前记录会话恢复许可失败", "module", "host", "err", err)
	}
}

// runEnded 引擎循环结束(任何原因)时由 engine.onDone 回调:按 store 事实定终态。
//   - Phase=Complete  → 标记 completed，发"创作完成"事件
//   - 其它            → 标记 idle/paused，发"创作停止"事件
func (h *Host) runEnded() {
	// 退出期 Close() 可能已 close(h.done)，末尾发送会 panic;recover 兜住竞态。
	defer func() { recover() }()
	h.observer.finalize()

	h.mu.Lock()
	progress, _ := h.store.Progress.Load()
	if progress != nil && progress.Phase == domain.PhaseComplete {
		h.finalizing = true
		h.lifecycle = lifecycleCompleted
		h.lastStopReason = "创作完成"
		h.lastStopCategory = domain.StopCategoryCompleted
		req := pauseRequest{generation: h.runGeneration, category: domain.StopCategoryCompleted, code: "completed", summary: "创作完成", level: "success"}
		h.pendingStop = &req
		// 完本收尾:确定性生成(store 已有全部事实,不花 LLM 调用;RFC 末节)。
		summary := completionSummary(h.store)
		h.mu.Unlock()
		req.summary = summary
		if err := h.recordRunStop(req); err != nil {
			slog.Warn("记录创作完成状态失败", "module", "host", "err", err)
		}
		slog.Info(summary, "module", "host")
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "success"})
		h.notifier.Send(notify.Notification{
			Kind: notify.KindRunEnd, Level: "info", Title: "ainovel: 创作完成",
			Body: h.runEndBody(progress.NovelName, summary),
		})
	} else {
		wasRunning := h.lifecycle == lifecycleRunning
		var req pauseRequest
		if wasRunning {
			h.finalizing = true
			h.lifecycle = lifecycleIdle
			h.lastStopReason = "引擎自然停止"
			h.lastStopCategory = domain.StopCategoryNaturalStop
			req = pauseRequest{generation: h.runGeneration, category: domain.StopCategoryNaturalStop, code: "natural_stop", summary: "引擎自然停止", level: "warn"}
		} else if h.pendingStop != nil {
			h.finalizing = true
			req = *h.pendingStop
		} else {
			h.finalizing = true
			req = pauseRequest{generation: h.runGeneration, category: domain.StopCategoryUnknown, code: "unknown", summary: "引擎停止，停止原因未知", level: "warn"}
		}
		h.pendingStop = nil
		completed := 0
		name := ""
		if progress != nil {
			completed = len(progress.CompletedChapters)
			name = progress.NovelName
		}
		h.mu.Unlock()
		if err := h.recordRunStop(req); err != nil {
			slog.Warn("记录创作停止状态失败", "module", "host", "err", err)
		}
		if wasRunning {
			summary := fmt.Sprintf("引擎停止 (已完成 %d 章)", completed)
			slog.Warn(summary, "module", "host")
			h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "warn"})
			h.notifier.Send(notify.Notification{
				Kind: notify.KindRunEnd, Level: "warn", Title: "ainovel: 创作停止",
				Body: h.runEndBody(name, summary),
			})
		}
	}

	select {
	case h.done <- struct{}{}:
	default:
	}
}

// runEndBody 组装 run_end 通知正文：书名 + 进度摘要 + 累计花费。
func (h *Host) runEndBody(novelName, summary string) string {
	if name := strings.TrimSpace(novelName); name != "" {
		summary = "《" + name + "》" + summary
	}
	cost, _, _, _, _ := h.usage.Totals()
	if cost > 0 {
		summary += fmt.Sprintf(" · 花费 $%.2f", cost)
	}
	return summary
}

// ── 通道 ──

// StreamClearSentinel 通过 streamCh 单条发送以示意"清空当前流式 round"。
// 不再用独立 clearCh —— 双通道无序导致 ✻ header 时常落到上一个 round 末尾。
const StreamClearSentinel = "\x00\x00CLEAR\x00\x00"

func (h *Host) Events() <-chan Event  { return h.events }
func (h *Host) Stream() <-chan string { return h.streamCh }
func (h *Host) Done() <-chan struct{} { return h.done }
func (h *Host) Dir() string           { return h.store.Dir() }
func (h *Host) AskUser() *tools.AskUserTool {
	// 诊断 Host 也返回非 nil AskUserTool（无 handler = 安全降级），
	// 确保 TUI/CLI 入口的 SetHandler 调用不会 panic。
	return h.askUser
}

// IsDiagnosticOnly 返回 Host 是否为只读诊断模式（migration_required）。
// TUI/CLI 入口据此跳过文件日志创建等有副作用的初始化步骤。
func (h *Host) IsDiagnosticOnly() bool {
	return h.diagnosticOnly
}

// ── 事件发射 ──

func (h *Host) emitEvent(ev Event) {
	defer func() { recover() }()
	// 所有事件的唯一 slog 入口。observer 翻译的 agentcore 事件和 Host 自发的
	// SYSTEM 事件（Start/Abort/Resume…）都在这里落日志，避免 ESC abort 与外部
	// 终止在 tui.log 上无法区分。
	if ev.Summary != "" || ev.Detail != "" {
		level := slog.LevelInfo
		switch ev.Level {
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
		// 日志记完整 Detail（排查用，不截断）；Detail 为空才回退到 Summary。
		msg := ev.Detail
		if msg == "" {
			msg = ev.Summary
		}
		attrs := []any{"module", "event", "category", ev.Category, "agent", ev.Agent}
		if ev.Kind != "" {
			attrs = append(attrs, "kind", ev.Kind)
		}
		slog.Log(context.Background(), level, msg, attrs...)
	}
	select {
	case h.events <- ev:
	default:
		select {
		case <-h.events:
		default:
		}
		select {
		case h.events <- ev:
		default:
		}
	}
}

func (h *Host) emitDelta(delta string) {
	defer func() { recover() }()
	select {
	case h.streamCh <- delta:
	default:
		select {
		case <-h.streamCh:
		default:
		}
		select {
		case h.streamCh <- delta:
		default:
		}
	}
}

func (h *Host) emitClear() {
	// 通过 streamCh 走"sentinel"，保证与 emitDelta 在同一条通道里有序送达 TUI。
	h.emitDelta(StreamClearSentinel)
}

// ── Snapshot (TUI 状态聚合) ──

func (h *Host) Snapshot() UISnapshot {
	h.mu.Lock()
	state := h.lifecycle
	lastStopReason := h.lastStopReason
	lastStopCategory := h.lastStopCategory
	provider, model := "", ""
	if h.models != nil {
		provider, model, _ = h.models.CurrentSelection("default")
	} else {
		provider = h.cfg.Provider
		model = h.cfg.ModelName
	}
	h.mu.Unlock()

	// 动态解析当前模型的上下文窗口，/model 切换后下一次 Snapshot 自动反映
	modelWindow, _ := h.cfg.ResolveContextWindow(model)
	idleSchedule := IdleWritingStatusAt(time.Now())
	cost, tokIn, tokOut, cacheRead, cacheWrite := h.usage.Totals()
	saved := h.usage.SavedUSD()
	overallCapable := h.usage.OverallCacheCapable()
	recentRead, recentInput, recentSamples := h.usage.OverallRecent()
	perAgent := h.usage.PerAgent()
	cacheStats := make([]AgentCacheStat, 0, len(perAgent))
	for _, a := range perAgent {
		cacheStats = append(cacheStats, AgentCacheStat{
			Role:            a.Role,
			Input:           a.Input,
			Output:          a.Output,
			CacheRead:       a.CacheRead,
			CacheWrite:      a.CacheWrite,
			Cost:            a.Cost,
			Saved:           a.Saved,
			CacheCapable:    a.CacheCapable,
			RecentCacheRead: a.RecentCacheRead,
			RecentInput:     a.RecentInput,
			RecentSamples:   a.RecentSamples,
		})
	}
	perModel := h.usage.PerModel()
	modelStats := make([]AgentCacheStat, 0, len(perModel))
	for _, a := range perModel {
		modelStats = append(modelStats, AgentCacheStat{
			Model:        a.Model,
			Input:        a.Input,
			Output:       a.Output,
			CacheRead:    a.CacheRead,
			CacheWrite:   a.CacheWrite,
			Cost:         a.Cost,
			Saved:        a.Saved,
			CacheCapable: a.CacheCapable,
		})
	}

	snap := UISnapshot{
		Provider:                  provider,
		ModelName:                 model,
		ModelContextWindow:        modelWindow,
		ThinkingLevel:             h.cfg.ResolveReasoningEffort("default"),
		Style:                     h.cfg.Style,
		RuntimeState:              string(state),
		IsRunning:                 state == lifecycleRunning,
		LastStopReason:            lastStopReason,
		LastStopCategory:          string(lastStopCategory),
		IdleWritingInPeak:         idleSchedule.InPeak,
		IdleWritingNextTransition: idleSchedule.NextTransition,
		TotalInputTokens:          tokIn,
		TotalOutputTokens:         tokOut,
		TotalCacheReadTokens:      cacheRead,
		TotalCacheWriteTokens:     cacheWrite,
		TotalCostUSD:              cost,
		TotalSavedUSD:             saved,
		BudgetLimitUSD: func() float64 {
			if h.budget != nil {
				return h.budget.Limit()
			}
			return 0
		}(),
		OverallCacheCapable:    overallCapable,
		OverallRecentCacheRead: recentRead,
		OverallRecentInput:     recentInput,
		OverallRecentSamples:   recentSamples,
		TotalCacheBreaks:       h.usage.OverallCacheBreaks(),
		CachePerAgent:          cacheStats,
		CachePerModel:          modelStats,
		MissingAssistantUsage:  h.usage.MissingAssistantUsage(),
	}

	progress, _ := h.store.Progress.Load()
	if progress != nil {
		snap.NovelName = strings.TrimSpace(progress.NovelName)
		snap.Phase = string(progress.Phase)
		snap.Flow = string(progress.Flow)
		snap.TotalChapters = progress.TotalChapters
		snap.CompletedCount = len(progress.CompletedChapters)
		snap.TotalWordCount = progress.TotalWordCount
		snap.InProgressChapter = progress.InProgressChapter
		snap.PendingRewrites = progress.PendingRewrites
		snap.RewriteReason = progress.RewriteReason
		snap.Layered = progress.Layered
		if progress.CurrentVolume > 0 {
			snap.CurrentVolumeArc = fmt.Sprintf("第%d卷·第%d弧", progress.CurrentVolume, progress.CurrentArc)
		}
	}
	if snap.NovelName == "" {
		if premise, _ := h.store.Outline.LoadPremise(); premise != "" {
			snap.NovelName = domain.ExtractNovelNameFromPremise(premise)
		}
	}
	if meta, _ := h.store.RunMeta.Load(); meta != nil {
		snap.PendingSteer = meta.PendingSteer
		snap.AdvanceMode = string(meta.AdvanceMode)
		snap.IdleWritingEnabled = meta.IdleWritingEnabled
		snap.PeakAutoPauseEnabled = meta.PeakAutoPauseEnabled
		if meta.Control != nil {
			snap.RunOrigin = string(meta.Control.Origin)
			if meta.Control.LastStop != nil {
				snap.LastStopCategory = string(meta.Control.LastStop.Category)
				snap.LastStopReason = meta.Control.LastStop.Summary
			}
			if meta.Control.AutoResume != nil {
				snap.AutoResumePending = true
				snap.AutoResumeNotBefore = parseRunControlTime(meta.Control.AutoResume.NotBefore)
			}
			snap.PeakOverrideUntil = parseRunControlTime(meta.Control.PeakOverrideUntil)
		}
		snap.AdvancePermitChapter = meta.AdvancePermitChapter
		if meta.AdvanceHold != nil {
			snap.HasAdvanceHold = true
			snap.AdvanceHoldReason = meta.AdvanceHold.Reason
		}
	}

	if h.observer != nil {
		snap.Agents = h.observer.agentSnapshots()
	}
	snap.StatusLabel = deriveStatusLabel(snap)

	// 恢复标签
	// 恢复标签
	if label, err := resumeLabel(h.store); err == nil && label != "" {
		snap.RecoveryLabel = label
	}

	h.fillDetails(&snap, progress)

	return snap
}

// fillDetails 填充详情区:设定、角色、最近 commit/review/摘要。
func (h *Host) fillDetails(snap *UISnapshot, progress *domain.Progress) {
	if premise, _ := h.store.Outline.LoadPremise(); premise != "" {
		snap.Premise = truncate(premise, 80)
	}
	var volumes []domain.VolumeOutline
	if progress != nil && progress.Layered {
		volumes, _ = h.store.Outline.LoadLayeredOutline()
		if compass, _ := h.store.Outline.LoadCompass(); compass != nil {
			snap.CompassDirection = compass.Long.EndingDirection
			snap.CompassScale = compass.Long.EstimatedScale
		}
		for _, v := range volumes {
			if v.Index > progress.CurrentVolume {
				snap.NextVolumeTitle = v.Title
				break
			}
		}
	}
	outline, _ := h.store.Outline.LoadOutline()
	if len(outline) == 0 && len(volumes) > 0 {
		outline = domain.FlattenOutline(volumes)
	}
	if len(outline) > 0 {
		snap.OutlinePlanned = len(outline)
		snap.Outline = selectOutlineForSnapshot(outline, progress, volumes)
	}
	if chars, _ := h.store.Characters.Load(); len(chars) > 0 {
		for _, c := range chars {
			label := c.Name
			if c.Role != "" {
				label += "（" + c.Role + "）"
			}
			snap.Characters = append(snap.Characters, label)
		}
	}
	if ledger, _ := h.store.Cast.Load(); len(ledger) > 0 {
		snap.SupportingCount = len(ledger)
		recent, _ := h.store.Cast.RecentActive(5)
		for _, e := range recent {
			label := e.Name
			if e.BriefRole != "" {
				label += "（" + e.BriefRole + "）"
			}
			snap.RecentSupporting = append(snap.RecentSupporting, label)
		}
	}
	if progress != nil && len(progress.CompletedChapters) > 0 {
		lastCh := progress.CompletedChapters[len(progress.CompletedChapters)-1]
		wc := progress.ChapterWordCounts[lastCh]
		snap.LastCommitSummary = fmt.Sprintf("第%d章 %d字", lastCh, wc)
	}
	currentCh := 1
	if progress != nil && len(progress.CompletedChapters) > 0 {
		currentCh = progress.CompletedChapters[len(progress.CompletedChapters)-1]
	}
	if review, err := h.store.World.LoadLastReview(currentCh); err == nil && review != nil {
		snap.LastReviewSummary = fmt.Sprintf("verdict=%s %d个问题", review.Verdict, len(review.Issues))
		if len(review.AffectedChapters) > 0 {
			snap.LastReviewSummary += fmt.Sprintf(" 影响%v", review.AffectedChapters)
		}
	}
	if cp := h.store.Checkpoints.LatestGlobal(); cp != nil {
		snap.LastCheckpointName = fmt.Sprintf("%s.%s", cp.Scope, cp.Step)
	}
	if progress != nil {
		for i := len(progress.CompletedChapters) - 1; i >= 0 && len(snap.RecentSummaries) < 2; i-- {
			ch := progress.CompletedChapters[i]
			if summary, err := h.store.Summaries.LoadSummary(ch); err == nil && summary != nil {
				snap.RecentSummaries = append(snap.RecentSummaries,
					fmt.Sprintf("第%d章: %s", ch, truncate(summary.Summary, 50)))
			}
		}
	}

	// 世界状态:时间线 / 伏笔 / 关系。
	// 单项读错误吞掉不阻塞 snapshot;仅当全部装载失败才关掉 WorldLoaded(空守卫)。
	loadedWorld := 0
	if timeline, err := h.store.World.LoadTimeline(); err == nil {
		loadedWorld++
		// 只展示已完成章节以内的时间线,过滤旧版残留的高章号条目。
		// maxCompleted 必须遍历求最大值:CompletedChapters 只 append 不排序
		// (见 store/progress.go MarkChapterComplete),取 [len-1] 在乱序时算错。
		// progress 为 nil 或零完成章时不显示时间线(不回退成全量)。
		maxCompleted := 0
		if progress != nil {
			for _, ch := range progress.CompletedChapters {
				if ch > maxCompleted {
					maxCompleted = ch
				}
			}
		}
		if maxCompleted > 0 {
			filtered := timeline[:0]
			for _, e := range timeline {
				if e.Chapter <= maxCompleted {
					filtered = append(filtered, e)
				}
			}
			// 按章号倒序取最近 5 条(同章按事件时间倒序)
			sort.Slice(filtered, func(i, j int) bool {
				if filtered[i].Chapter != filtered[j].Chapter {
					return filtered[i].Chapter > filtered[j].Chapter
				}
				return filtered[i].Time > filtered[j].Time
			})
			for i := 0; i < len(filtered) && len(snap.RecentTimeline) < 5; i++ {
				snap.RecentTimeline = append(snap.RecentTimeline,
					fmt.Sprintf("第%d章: %s", filtered[i].Chapter, truncate(filtered[i].Event, 30)))
			}
		}
	}
	if active, err := h.store.World.LoadActiveForeshadow(); err == nil {
		loadedWorld++
		// ForeshadowEntry 无 Stale/停滞状态字段,只填打开数,停滞数保持 0
		snap.ForeshadowOpen = len(active)
	}
	if rel, err := h.store.World.LoadRelationships(); err == nil {
		loadedWorld++
		snap.RelationshipCount = len(rel)
	}
	snap.WorldLoaded = loadedWorld > 0
}

func deriveStatusLabel(s UISnapshot) string {
	switch {
	case s.Phase == string(domain.PhaseComplete):
		return "COMPLETE"
	case s.Flow == string(domain.FlowReviewing):
		return "REVIEW"
	case s.Flow == string(domain.FlowRewriting) || s.Flow == string(domain.FlowPolishing):
		return "REWRITE"
	case s.RuntimeState == "running":
		return "RUNNING"
	default:
		return "READY"
	}
}

// ── 模型管理 ──

func (h *Host) ConfiguredProviders() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	providers := make([]string, 0, len(h.cfg.Providers))
	for name := range h.cfg.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	return providers
}

func (h *Host) ConfiguredModels(provider string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.CandidateModels(provider)
}

func (h *Host) CurrentModelSelection(role string) (provider, model string, ok bool) {
	provider = h.cfg.Provider
	model = h.cfg.ModelName
	if h.models != nil {
		provider, model, ok = h.models.CurrentSelection(role)
	}
	return
}

func (h *Host) SwitchModel(role, provider, model string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if provider == "" || model == "" {
		return fmt.Errorf("provider and model are required")
	}
	if err := h.models.Swap(role, provider, model); err != nil {
		return err
	}
	if role == "" || role == "default" {
		h.cfg.Provider = provider
		h.cfg.ModelName = model
	} else {
		if h.cfg.Roles == nil {
			h.cfg.Roles = make(map[string]bootstrap.RoleConfig)
		}
		rc := h.cfg.Roles[role]
		rc.Provider = provider
		rc.Model = model
		h.cfg.Roles[role] = rc
	}
	h.normalizeThinkingLocked(role)
	if path := bootstrap.DefaultConfigPath(); path != "" {
		if err := bootstrap.SaveConfig(path, h.cfg); err != nil {
			slog.Warn("保存配置失败", "module", "host", "err", err)
		}
	}
	h.applyThinkingLocked(role)
	// 切到未登记模型时打一行 warn，提示用户走了 128k 兜底——长篇容易被提前压缩。
	logRole := role
	if logRole == "" {
		logRole = "default"
	}
	window, source := h.cfg.ResolveContextWindow(model)
	bootstrap.LogContextWindowChoice(logRole, model, window, source)

	// 无常驻上下文需要联动:writer/architect/editor 的 ContextManager 走
	// ContextManagerFactory,下次 spawn 自动按新模型窗口重建。

	h.emitEvent(Event{
		Time:     time.Now(),
		Category: "SYSTEM",
		Summary:  fmt.Sprintf("模型已切换：%s → %s/%s", role, provider, model),
		Level:    "info",
	})
	return nil
}

// concreteThinkingRoles 是可应用推理强度的具体角色（与 agents.ApplyThinking 路由一致）。
// 调 default 时按各角色 ResolveReasoningEffort 逐个重新应用。
var concreteThinkingRoles = []string{"architect", "writer", "editor", "polisher"}

// CurrentThinking 返回某角色当前生效的推理强度原始串（供 /model 面板同步当前值）。
func (h *Host) CurrentThinking(role string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.ResolveReasoningEffort(strings.ToLower(strings.TrimSpace(role)))
}

func (h *Host) AvailableThinking(role string) []agentcore.ThinkingLevel {
	if h.models == nil {
		return nil
	}
	h.mu.Lock()
	model := h.models.ForRole(strings.ToLower(strings.TrimSpace(role)))
	h.mu.Unlock()
	return agents.AvailableThinkingForModel(model)
}

func (h *Host) normalizeThinkingLocked(role string) agentcore.ThinkingLevel {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" || role == "default" {
		parsed, _ := agents.ParseThinkingLevel(h.cfg.ReasoningEffort)
		for _, r := range concreteThinkingRoles {
			resolved, ok := agents.ResolveThinkingForModel(h.models.ForRole(r), parsed)
			if !ok || resolved != parsed {
				h.cfg.ReasoningEffort = string(resolved)
				return resolved
			}
		}
		h.cfg.ReasoningEffort = string(parsed)
		return parsed
	}

	_, hasRoleThinking := h.cfg.Roles[role]
	hasRoleThinking = hasRoleThinking && h.cfg.Roles[role].ReasoningEffort != ""
	parsed, _ := agents.ParseThinkingLevel(h.cfg.ResolveReasoningEffort(role))
	resolved, _ := agents.ResolveThinkingForModel(h.models.ForRole(role), parsed)
	if !hasRoleThinking {
		if resolved != parsed {
			h.cfg.ReasoningEffort = string(resolved)
		}
		return resolved
	}
	if h.cfg.Roles == nil {
		h.cfg.Roles = make(map[string]bootstrap.RoleConfig)
	}
	rc := h.cfg.Roles[role]
	rc.ReasoningEffort = string(resolved)
	h.cfg.Roles[role] = rc
	return resolved
}

func (h *Host) applyThinkingLocked(role string) {
	if h.thinkingApplier == nil {
		return
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" || role == "default" {
		for _, r := range concreteThinkingRoles {
			lv, _ := agents.ParseThinkingLevel(h.cfg.ResolveReasoningEffort(r))
			h.thinkingApplier(r, lv)
		}
		return
	}
	lv, _ := agents.ParseThinkingLevel(h.cfg.ResolveReasoningEffort(role))
	h.thinkingApplier(role, lv)
}

// SetRoleThinking 设置某角色（或 default）的推理强度：校验→持久化→联动 live agent→事件。
// 镜像 SwitchModel 的结构；与模型选择正交，可单独调整。level 为空 = 不覆盖（继承）。
func (h *Host) SetRoleThinking(role, level string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	parsed, err := agents.ParseThinkingLevel(level)
	if err != nil {
		return err
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" || role == "default" {
		for _, r := range concreteThinkingRoles {
			if resolved, ok := agents.ResolveThinkingForModel(h.models.ForRole(r), parsed); !ok || resolved != parsed {
				parsed = resolved
				break
			}
		}
	} else {
		parsed, _ = agents.ResolveThinkingForModel(h.models.ForRole(role), parsed)
	}
	// 持久化：具体角色写 Roles[role].ReasoningEffort，default/"" 写顶层 ReasoningEffort。
	if role == "" || role == "default" {
		h.cfg.ReasoningEffort = string(parsed)
	} else {
		if h.cfg.Roles == nil {
			h.cfg.Roles = make(map[string]bootstrap.RoleConfig)
		}
		rc := h.cfg.Roles[role]
		rc.ReasoningEffort = string(parsed)
		h.cfg.Roles[role] = rc
	}
	if path := bootstrap.DefaultConfigPath(); path != "" {
		if err := bootstrap.SaveConfig(path, h.cfg); err != nil {
			slog.Warn("保存配置失败", "module", "host", "err", err)
		}
	}

	// 联动 live：具体角色直接应用；default 则遍历各具体角色按 ResolveReasoningEffort 重新应用
	// （已被角色级覆盖的保留自身，未覆盖的吃上新默认）。
	h.applyThinkingLocked(role)

	logRole := role
	if logRole == "" {
		logRole = "default"
	}
	shown := string(parsed)
	if shown == "" {
		shown = "默认(继承)"
	}
	h.emitEvent(Event{
		Time:     time.Now(),
		Category: "SYSTEM",
		Summary:  fmt.Sprintf("推理强度已切换：%s → %s", logRole, shown),
		Level:    "info",
	})
	return nil
}

// ── 事件回放 ──

func (h *Host) ReplayQueue(afterSeq int64) ([]domain.RuntimeQueueItem, error) {
	if h.store == nil || h.store.Runtime == nil {
		return nil, nil
	}
	return h.store.Runtime.LoadQueueAfter(afterSeq)
}

func (h *Host) ReplayStreamQueue(afterSeq int64) ([]domain.RuntimeQueueItem, error) {
	if h.store == nil || h.store.Runtime == nil {
		return nil, nil
	}
	return h.store.Runtime.LoadQueueKindsAfter(afterSeq, domain.RuntimeQueueStreamDelta, domain.RuntimeQueueStreamClear)
}

// ── 共创 ──

// CoCreateStream 冷启动共创：从零澄清需求，产出整本书的创作指令。
func (h *Host) CoCreateStream(ctx context.Context, history []CoCreateMessage, onProgress func(kind, text string)) (CoCreateReply, error) {
	if err := h.checkMigrationGate(); err != nil {
		return CoCreateReply{}, err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return CoCreateReply{}, fmt.Errorf("恢复操作进行中")
	}
	return coCreateStream(ctx, h.models, h.store, coCreateSystemPrompt, history, onProgress)
}

// StageCoCreateStream 阶段共创：在已写内容的基础上规划后续方向。
// 系统提示 = 阶段 prompt + 当前故事状态摘要，让助手知道"已经写了什么"。
func (h *Host) StageCoCreateStream(ctx context.Context, history []CoCreateMessage, onProgress func(kind, text string)) (CoCreateReply, error) {
	if err := h.checkMigrationGate(); err != nil {
		return CoCreateReply{}, err
	}
	h.interMu.Lock()
	defer h.interMu.Unlock()
	if h.isRestoring() {
		return CoCreateReply{}, fmt.Errorf("恢复操作进行中")
	}
	return coCreateStream(ctx, h.models, h.store, stageSystemPrompt(h.store), history, onProgress)
}

// stagePlanPrefix 把共创产出的"后续方向 brief"包装成一条阶段规划干预，交 Arbiter 裁定。
// 只贴 [阶段规划] 事实标记 + 中性陈述，不写死"怎么落地"——具体路由（compass / architect /
// user_rules）交给 arbiter-intervention.md 的「阶段规划」判据，避免与 prompt 形成第二真相源、
// 也不堵死风格类要求走 user_rules（守"分类裁定归 LLM"）。Continue 再叠加 [用户干预] 前缀。
const stagePlanPrefix = "[阶段规划] 我暂停创作，和共创助手一起梳理了下面的后续方向，请按你的干预分类裁定如何落地，然后继续创作。后续方向如下：\n\n"

// PauseForCoCreate 进入阶段共创：置共创占用标记，运行中则一并暂停 Engine。
// 返回 false 表示无法进入（全书已完成或已在共创中），调用方忽略即可。
// 占用标记在共创窗口内堵住 import/simulate/start/resume/continue 的并发介入——
// 运行中暂停后 lifecycle=paused，现有 ==running 互斥失效，靠该标记补缺；
// 已停止（idle/paused）也允许进入，规划完经 Continue 续跑。
func (h *Host) PauseForCoCreate() bool {
	if err := h.checkMigrationGate(); err != nil {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
			Summary: "迁移未完成，无法进入阶段共创: " + err.Error(), Level: "warn"})
		return false
	}
	h.mu.Lock()
	if h.cocreating || h.lifecycle == lifecycleCompleted {
		h.mu.Unlock()
		return false
	}
	if h.restoring {
		h.mu.Unlock()
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM",
			Summary: "恢复操作进行中，无法进入阶段共创", Level: "warn"})
		return false
	}
	h.cocreating = true
	running := h.lifecycle == lifecycleRunning
	h.mu.Unlock()

	// 运行中复用 abortWithEvent 停机（running→paused + setAborting + Abort + 事件），与手动
	// 暂停同序、不另抄一遍；已停止（idle/paused）只置标记，规划完经 Continue 续跑。
	if running {
		h.abortWithStop(domain.StopCategoryCoCreate, "cocreate", "进入阶段共创，创作已暂停", "info")
	} else {
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "进入阶段共创", Level: "info"})
	}
	return true
}

// ResumeFromCoCreate 结束阶段共创：把共创产出的后续方向作为干预注入并恢复创作。
// 清占用标记后复用 Continue 的停机注入路径（受预算前置约束）。
// 注：draft 为空时提前返回、不清标记是有意的（共创尚未结束）；TUI 侧 canStart() 守卫
// 与此处用同一"非空"判据，保证该路径不可达，cocreating 不会因此泄漏。
func (h *Host) ResumeFromCoCreate(draft string) error {
	if err := h.checkMigrationGate(); err != nil {
		return err
	}
	draft = strings.TrimSpace(draft)
	if draft == "" {
		return fmt.Errorf("draft is required")
	}
	h.mu.Lock()
	if !h.cocreating {
		h.mu.Unlock()
		return fmt.Errorf("not in co-create")
	}
	h.cocreating = false
	h.mu.Unlock()

	// PauseForCoCreate 的 abort 是异步的:等引擎循环真正收敛再继续,回到与手动
	// 暂停后 Continue 一致的"真停机"前提。共创窗口是人机交互时间尺度,短轮询无感。
	for h.engine.isRunning() {
		time.Sleep(20 * time.Millisecond)
	}

	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "阶段共创完成，已注入后续方向并恢复创作", Level: "info"})
	return h.Continue(stagePlanPrefix + draft)
}

// CancelCoCreate 放弃阶段共创：清占用标记，保持暂停态（用户可在输入框继续或重启 Resume）。
func (h *Host) CancelCoCreate() {
	h.mu.Lock()
	if !h.cocreating {
		h.mu.Unlock()
		return
	}
	h.cocreating = false
	h.mu.Unlock()
	h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: "已退出阶段共创，创作保持暂停（可在输入框继续）", Level: "info"})
}

// ── 工具 ──

func (h *Host) refreshWriterRestore() {
	if h.writerRestore != nil {
		h.writerRestore.Refresh(h.store)
	}
}

func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func parseRunControlTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

// importRoleModel 返回导入语义函数使用的模型。
// 未显式配置 import_analyze/import_synthesize 时，按上游语义回落到 architect；
// import_segment 由调用方显式启用。其余导入运行时逻辑仍由本地 imp 包负责，
// 避免改变当前的 Store/互斥/checkpoint 行为。
func (h *Host) importRoleModel(role string) agentcore.ChatModel {
	modelRole := role
	if _, _, explicit := h.models.CurrentSelection(role); !explicit {
		modelRole = "architect"
	}
	model := h.models.ForRoleWithFailover(modelRole, func(ev bootstrap.FailoverEvent) {
		slog.Warn("导入角色 provider 切换",
			"module", "import",
			"role", role,
			"model_role", modelRole,
			"reason", ev.Reason,
			"from", fmt.Sprintf("%s/%s", ev.FromProvider, ev.FromModel),
			"to", fmt.Sprintf("%s/%s", ev.ToProvider, ev.ToModel),
			"err", ev.Err,
		)
	})
	var record func(string, string, agentcore.AgentMessage)
	if h.usage != nil {
		record = h.usage.Record
	}
	// 记录实际绑定的模型角色：未配置 import_* 时是 architect，避免 usage
	// 统计把回落到 architect 的调用误算成默认模型。
	return agents.WithTrailingAntiRefusal(newRoleUsageTrackedModel(model, modelRole, record), h.store)
}

// ImportFrom 启动一次外部小说反推导入：切分 → 反推 foundation → 逐章分析落盘。
// 与 Engine 运行互斥；导入完成后调用方可立即 Resume() 续写。
// 返回的事件通道由 imp.Run 关闭，调用方负责消费（满则丢弃以防阻塞分析协程）。
func (h *Host) ImportFrom(ctx context.Context, opts imp.Options) (<-chan imp.Event, error) {
	if err := h.checkMigrationGate(); err != nil {
		return nil, err
	}
	h.interMu.Lock()
	if err := h.guardExclusive("导入"); err != nil {
		h.interMu.Unlock()
		return nil, err
	}
	h.activeOps.Add(1)
	h.interMu.Unlock()

	if testBeforeImpRun != nil {
		ch, err := testBeforeImpRun(ctx)
		if err != nil {
			h.activeOps.Done()
			return nil, err
		}
		return h.trackImpOp(ch), nil
	}

	var segmentLLM agentcore.ChatModel
	if _, _, explicit := h.models.CurrentSelection("import_segment"); explicit {
		segmentLLM = h.importRoleModel("import_segment")
	}
	synthesizeLLM := h.importRoleModel("import_synthesize")
	analyzeLLM := h.importRoleModel("import_analyze")
	deps := imp.Deps{
		Store:      h.store,
		CommitTool: tools.NewCommitChapterTool(h.store),
		// LLM 保留为兼容字段；新流程明确按阶段绑定三个模型。
		LLM:           synthesizeLLM,
		SegmentLLM:    segmentLLM,
		AnalyzeLLM:    analyzeLLM,
		SynthesizeLLM: synthesizeLLM,
		Prompts: imp.Prompts{
			Segment:    h.bundle.Prompts.ImportSegment,
			Synthesize: h.bundle.Prompts.ImportSynthesize,
			Analyze:    h.bundle.Prompts.ImportAnalyze,
			// 保留旧字段，便于外部/测试构造的 Bundle 继续工作。
			Foundation: h.bundle.Prompts.ImportFoundation,
			Analyzer:   h.bundle.Prompts.ImportAnalyzer,
		},
		Contract: h.contract,
	}
	ch, err := imp.Run(ctx, deps, opts)
	if err != nil {
		h.activeOps.Done()
		return nil, err
	}
	return h.trackImpOp(ch), nil
}

// Simulate 读取 simulate 目录并生成或增量更新仿写画像。
func (h *Host) Simulate(ctx context.Context) (<-chan sim.Event, error) {
	if err := h.checkMigrationGate(); err != nil {
		return nil, err
	}
	h.interMu.Lock()
	if err := h.guardExclusive("生成仿写画像"); err != nil {
		h.interMu.Unlock()
		return nil, err
	}
	h.activeOps.Add(1)
	h.interMu.Unlock()

	if testBeforeSimRun != nil {
		ch, err := testBeforeSimRun(ctx)
		if err != nil {
			h.activeOps.Done()
			return nil, err
		}
		return h.trackSimOp(ch), nil
	}

	wd, err := os.Getwd()
	if err != nil {
		h.activeOps.Done()
		return nil, fmt.Errorf("get working dir: %w", err)
	}
	deps := sim.Deps{
		Store: h.store,
		LLM:   agents.WithTrailingAntiRefusal(h.models.ForRole("architect"), h.store),
		Prompts: sim.Prompts{
			Source: h.bundle.Prompts.SimulationSource,
			Merge:  h.bundle.Prompts.SimulationMerge,
		},
	}
	ch, err := sim.Run(ctx, deps, sim.Options{SourceDir: filepath.Join(wd, "simulate")})
	if err != nil {
		h.activeOps.Done()
		return nil, err
	}
	return h.trackSimOp(ch), nil
}

// ImportSimulationProfile 导入此前生成的仿写画像。
func (h *Host) ImportSimulationProfile(ctx context.Context, path string) (<-chan sim.Event, error) {
	if err := h.checkMigrationGate(); err != nil {
		return nil, err
	}
	h.interMu.Lock()
	if err := h.guardExclusive("导入仿写画像"); err != nil {
		h.interMu.Unlock()
		return nil, err
	}
	h.activeOps.Add(1)
	h.interMu.Unlock()

	if testBeforeSimRun != nil {
		ch, err := testBeforeSimRun(ctx)
		if err != nil {
			h.activeOps.Done()
			return nil, err
		}
		return h.trackSimOp(ch), nil
	}

	ch, err := sim.RunImport(ctx, h.store, path)
	if err != nil {
		h.activeOps.Done()
		return nil, err
	}
	return h.trackSimOp(ch), nil
}

// trackSimOp wraps a sim.Event channel to track the operation's lifetime
// via h.activeOps. Add(1) must already have been called under interMu.
func (h *Host) trackSimOp(in <-chan sim.Event) <-chan sim.Event {
	out := make(chan sim.Event)
	go func() {
		defer h.activeOps.Done()
		if testBeforeAsyncOp != nil {
			testBeforeAsyncOp()
		}
		for ev := range in {
			out <- ev
		}
		close(out)
	}()
	return out
}

// trackImpOp wraps an imp.Event channel (same pattern as trackSimOp).
func (h *Host) trackImpOp(in <-chan imp.Event) <-chan imp.Event {
	out := make(chan imp.Event)
	go func() {
		defer h.activeOps.Done()
		if testBeforeAsyncOp != nil {
			testBeforeAsyncOp()
		}
		for ev := range in {
			out <- ev
		}
		close(out)
	}()
	return out
}

// guardExclusive 检查独占占用：Engine 运行中或阶段共创窗口内时拒绝会改写状态的入口
// （import/simulate）。补上 paused 期间只查 ==running 的并发缺口。
func (h *Host) guardExclusive(action string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.lifecycle == lifecycleRunning:
		return fmt.Errorf("创作引擎运行中，请先暂停后再%s", action)
	case h.cocreating:
		return fmt.Errorf("阶段共创进行中，请先结束共创后再%s", action)
	case h.restoring:
		return fmt.Errorf("恢复操作进行中，请稍后再%s", action)
	}
	return nil
}

// Export 导出已完成章节为外部文件（当前仅支持 TXT）。
//
// 与 ImportFrom 不同：导出是只读操作（不动 Progress / Checkpoint），
// 因此**不要求 Engine 停机**——写作中途也可以随时导出"现阶段成品"。
// 只读到 Progress.CompletedChapters + 章节终稿 + 大纲 + premise 的一致快照。
func (h *Host) Export(ctx context.Context, opts exp.Options) (*exp.Result, error) {
	return exp.Run(ctx, exp.Deps{Store: h.store}, opts)
}

// ── 快照与恢复 API（供 TUI/CLI 调用） ──

// projectID 返回用于备份元数据的项目标识。
func (h *Host) projectID() string {
	return "proj-" + filepath.Base(h.store.Dir())
}

// ListSnapshots 返回所有正常（非救援）快照，按创建时间最新优先排列。
// 隐藏的救援备份（.rescue/）不会被返回。
func (h *Host) ListSnapshots() ([]backup.Manifest, error) {
	if h.diagnosticOnly {
		return nil, nil
	}
	snaps, err := backup.List(h.store.Dir())
	if err != nil {
		return nil, err
	}
	return snaps, nil
}

// IsEngineQuiescent 返回引擎是否已停止（无写入）。恢复操作要求引擎停稳。
// 诊断 Host（migration_required）没有引擎，始终返回 true。
func (h *Host) IsEngineQuiescent() bool {
	if h.diagnosticOnly || h.engine == nil {
		return true
	}
	return !h.engine.isRunning()
}

// backupBoundary 加载进度并校验最新完成章是否与请求的 V/A 匹配。
// Arc 必须 !IsVolumeEnd；Volume 必须 IsVolumeEnd。严格模式：progress/chapter/boundary
// 任何读取错误均 fail closed。
func (h *Host) backupBoundary(kind backup.SnapshotKind, volume, arc int) error {
	p, err := h.store.Progress.Load()
	if err != nil {
		return fmt.Errorf("progress load: %w", err)
	}
	if p == nil || len(p.CompletedChapters) == 0 {
		return fmt.Errorf("no completed chapters; cannot validate boundary")
	}
	lastCh := p.CompletedChapters[len(p.CompletedChapters)-1]
	b, berr := h.store.Outline.CheckArcBoundary(lastCh)
	if berr != nil {
		return fmt.Errorf("boundary check: %w", berr)
	}
	if b == nil {
		return fmt.Errorf("chapter %d is not a boundary", lastCh)
	}
	if b.Volume != volume {
		return fmt.Errorf("chapter %d volume %d != requested volume %d", lastCh, b.Volume, volume)
	}
	switch kind {
	case backup.KindArc:
		if b.IsVolumeEnd {
			return fmt.Errorf("arc %d of volume %d is a volume end; use BackupVolume instead", arc, volume)
		}
		if !b.IsArcEnd {
			return fmt.Errorf("chapter %d is not an arc end", lastCh)
		}
		if b.Arc != arc {
			return fmt.Errorf("chapter %d arc %d != requested arc %d", lastCh, b.Arc, arc)
		}
	case backup.KindVolume:
		if !b.IsVolumeEnd {
			return fmt.Errorf("chapter %d is not a volume end", lastCh)
		}
	}
	return nil
}

// BackupArc 在当前弧边界创建一个弧快照。
// 持有 interMu→h.mu 独占 Host 预约：迁移门 → 诊断 → 恢复中/engine.isRunning
// → backupBoundary → backup.Backup 均在一个预约内完成。边界验证严格 fail closed。
func (h *Host) BackupArc(volume, arc int) (*backup.Manifest, error) {
	h.interMu.Lock()
	defer h.interMu.Unlock()

	if err := h.checkMigrationGate(); err != nil {
		return nil, err
	}
	if h.diagnosticOnly {
		return nil, fmt.Errorf("diagnostic host does not support backup")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restoring {
		return nil, fmt.Errorf("restore in progress")
	}
	if h.engine.isRunning() {
		return nil, fmt.Errorf("engine is running; pause before backup")
	}
	if err := h.backupBoundary(backup.KindArc, volume, arc); err != nil {
		return nil, err
	}
	source := h.store.Dir()
	m, err := backup.Backup(source, h.projectID(), backup.KindArc, volume, arc)
	if err != nil {
		return nil, fmt.Errorf("arc backup: %w", err)
	}
	return m, nil
}

// BackupVolume 在当前卷边界创建一个卷快照。
// 持有 interMu→h.mu 独占 Host 预约。边界验证要求最新完成章必须在卷末。
func (h *Host) BackupVolume(volume int) (*backup.Manifest, error) {
	h.interMu.Lock()
	defer h.interMu.Unlock()

	if err := h.checkMigrationGate(); err != nil {
		return nil, err
	}
	if h.diagnosticOnly {
		return nil, fmt.Errorf("diagnostic host does not support backup")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restoring {
		return nil, fmt.Errorf("restore in progress")
	}
	if h.engine.isRunning() {
		return nil, fmt.Errorf("engine is running; pause before backup")
	}
	if err := h.backupBoundary(backup.KindVolume, volume, 0); err != nil {
		return nil, err
	}
	source := h.store.Dir()
	m, err := backup.Backup(source, h.projectID(), backup.KindVolume, volume, 0)
	if err != nil {
		return nil, fmt.Errorf("volume backup: %w", err)
	}
	return m, nil
}

// RestoreSnapshot 从指定快照恢复到项目目录。
//
// 调用者必须传递 confirmed=true 以确认该操作。执行前生命周期无条件暂停；
// 恢复期间阻止所有引擎/provider/tool 写入以及 mutating 入口。
//
// 成功（RestoreResult.FinalVerify=true）后刷新 WriterRestorePack 并保持暂停态。
// 部分失败（非零 FileErrors）同样保持暂停态，返回结构化结果。
func (h *Host) RestoreSnapshot(snapshotID string, confirmed bool) (rr *backup.RestoreResult, rerr error) {
	if !confirmed {
		return nil, fmt.Errorf("restore requires explicit confirmation")
	}
	if err := h.checkMigrationGate(); err != nil {
		return nil, err
	}
	if h.diagnosticOnly {
		return nil, fmt.Errorf("diagnostic host does not support restore")
	}

	// ── 独占门：互斥恢复 + 检查共创 ──
	h.interMu.Lock()
	defer h.interMu.Unlock()

	h.mu.Lock()
	if h.cocreating {
		h.mu.Unlock()
		return nil, fmt.Errorf("阶段共创进行中，无法执行恢复")
	}
	if h.restoring {
		h.mu.Unlock()
		return nil, fmt.Errorf("restore already in progress")
	}
	h.restoring = true
	// 生命周期无条件暂停，并在引擎完成后再次确认暂停态
	h.lifecycle = lifecyclePaused
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		h.restoring = false
		h.mu.Unlock()
	}()

	// ── 等待异步操作（import/simulation）完成 ──
	h.activeOps.Wait()

	// ── 停引擎（如运行中）并等待 deferred cleanup + onDone ──
	if h.engine.isRunning() {
		h.observer.setAborting(true)
		h.engine.abort()
		h.waitEngineCompletion()
		// 引擎完成后再次确认 paused（runEnded 可能改为 idle/completed）
		h.mu.Lock()
		h.lifecycle = lifecyclePaused
		h.mu.Unlock()
	}

	// ── quiesce UsageTracker 自动保存（stop-and-wait + 持久化） ──
	// 在 StopAutoSave 之后立即 defer 重启 autosave，确保所有路径均会重启。
	h.usage.StopAutoSave(h.usageCancel)
	h.usageCancel = nil
	h.usageCtx = nil
	defer func() {
		uc, ucancel := context.WithCancel(context.Background())
		h.usageCtx = uc
		h.usageCancel = ucancel
		h.usage.StartAutoSave(uc)
	}()

	if err := h.usage.SaveNow(); err != nil {
		// 预存失败 —— 中止恢复，不写 active tree
		return nil, fmt.Errorf("usage save before restore failed: %w", err)
	}

	// ── 执行恢复 ──
	rr, rerr = backup.Restore(h.store.Dir(), snapshotID)

	// ── 恢复后：persist 当前内存 usage（保留单调累计） ──
	// post SaveNow 失败时不发正常成功事件，但保留 WRP 刷新（creative 恢复已生效）。
	postErr := h.usage.SaveNow()
	if postErr != nil {
		slog.Warn("restore: usage save after restore failed", "module", "host", "err", postErr)
	}

	// ── 处理结果 ──
	creativeOK := rerr == nil && rr != nil && rr.FinalVerify && rr.Failed == 0
	if creativeOK {
		h.refreshWriterRestore()
	}

	switch {
	case postErr != nil && creativeOK:
		// creative 成功但 usage 持久化失败 —— 仍发 paused 事件
		summary := fmt.Sprintf("从快照 %s 恢复成功（%d/%d 文件），usage 落盘失败",
			snapshotID, rr.Succeeded, rr.Attempted)
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "warn"})
		slog.Warn(summary, "module", "host")
		return rr, fmt.Errorf("usage save after restore failed: %v", postErr)

	case creativeOK:
		summary := fmt.Sprintf("从快照 %s 恢复成功（%d/%d 文件，最终验证通过）",
			snapshotID, rr.Succeeded, rr.Attempted)
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "success"})
		slog.Info(summary, "module", "host", "rescue_id", rr.RescueID)

	case rerr != nil:
		summary := "从快照恢复失败"
		if rr != nil {
			summary = fmt.Sprintf("从快照 %s 恢复失败（%d/%d 文件成功）", snapshotID, rr.Succeeded, rr.Attempted)
		}
		h.emitEvent(Event{Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "error"})
		slog.Warn(summary, "module", "host")
	}

	if rerr != nil {
		return rr, rerr
	}
	return rr, nil
}
