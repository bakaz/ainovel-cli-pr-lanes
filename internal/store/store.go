package store

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// errStoreReadOnly 是只读 Store 上写操作的统一拒绝原因（复核阻塞项 2 只读模式）。
var errStoreReadOnly = errors.New("store is read-only (diagnostic/export mode)")

// errStoreClosed 是 Close() 后写操作的统一拒绝原因（缺口 2）。
var errStoreClosed = errors.New("store is closed")

// storeState 是 Store 的统一可变写守卫状态（缺口 1/2）：构造期一次性创建，所有
// 子 IO 共享同一引用，写入口统一检查。四类拒绝场景全部经同一 guard：
//   - 未 ready：workspace 锁获取失败 / 本进程双写（readyErr）
//   - checkpoint 数据损坏（readyErr）——缺口 1：不再只 Init 报错、写仍放行
//   - 只读模式（readOnly）
//   - Close() 后（closed，atomic）——缺口 2：不再只释放锁、写仍放行
//
// 独立 IO（newIO 直接构造，如单测的 NewCheckpointStore(newIO(dir))）state 为
// nil → 不拦截（无 Store 状态可检查）。
type storeState struct {
	mu       sync.Mutex
	readyErr error       // 构造期一次性设置；非 nil = 未 ready（Ready()=false）
	readOnly bool        // 只读模式（Ready()=true，写被拒）
	closed   atomic.Bool // Close() 后（Ready()=false；并发 Close 安全）
	// lease 是写生命周期 lease（Close 与在途写竞争窗口修复）：物理写在完整 OS
	// 修改期间持读 lease（io.beginWrite/endWrite），Close() 先取独占 lease
	// 等待所有在途写结束后，再置 closed 并释放 workspace 锁——消除"旧写通过
	// guard 后、Close 释放锁、新 Store 并发写"窗口。
	// 锁序：写路径 io.mu → lease.RLock（Unlocked 方法在 io.mu 内获取）；
	// Close 的 lease.Lock 从不获取 io.mu → 无反向嵌套、无死锁环。
	lease sync.RWMutex
}

// writeBlocked 返回写操作被拒绝的稳定错误（nil = 允许）。
// 顺序：closed → readOnly → readyErr（与旧行为一致：只读 Store 恒返回
// errStoreReadOnly，即使同时存在 checkpoint 损坏）。
func (st *storeState) writeBlocked() error {
	if st == nil {
		return nil
	}
	if st.closed.Load() {
		return errStoreClosed
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.readOnly {
		return errStoreReadOnly
	}
	return st.readyErr
}

// Store 是状态管理的组合根，持有所有子存储。
type Store struct {
	dir string

	Progress        *ProgressStore
	Outline         *OutlineStore
	ChapterTitles   *ChapterTitleStore
	Drafts          *DraftStore
	Summaries       *SummaryStore
	RunMeta         *RunMetaStore
	UserRules       *UserRulesStore
	Signals         *SignalStore
	Runtime         *RuntimeStore
	Characters      *CharacterStore
	Cast            *CastStore
	World           *WorldStore
	Checkpoints     *CheckpointStore
	Sessions        *SessionStore
	Usage           *UsageStore
	PrefixManifest  *PrefixManifestStore
	Simulation      *SimulationStore
	StyleAnchors    *StyleAnchorsStore
	Decisions       *DecisionStore
	ProjectProfile  *ProjectProfileStore
	PlanningArchive *PlanningArchiveStore
	StyleReview     *StyleReviewStore
	AntiRefusal     *AntiRefusalStore

	crossMu sync.Mutex // 保护跨域原子操作

	// state 是统一写守卫状态（缺口 1/2）：所有子 IO 共享引用，写入口统一检查
	// （未 ready / 只读 / Close 后全部拒绝）。nil 理论不可达（所有构造路径都
	// 创建 state），防御性处理。
	state *storeState
	// workspaceRelease 释放 workspace 跨进程排他锁（进程退出/崩溃时 OS 自动
	// 释放；优雅退出可调用 Close）。nil = 未获取（锁失败时 Init 已报错）。
	workspaceRelease func()
}

// NewStore 创建可写状态管理器，dir 为小说输出根目录。
// 复核阻塞项 1：先 AcquireWorkspaceLock 再初始化任何持久化 Store/cache——
// 锁到手后才加载 checkpoint cache，避免"先加载旧 cache 再等锁、锁到手时 cache
// 已过期"；锁获取失败时不加载 cache（未 ready，写操作 fail-closed）。
// 复核阻塞项 2（方案 A）：同一进程内同一 workspace 只允许一个可写 Store。
func NewStore(dir string) *Store {
	release, lockErr := AcquireWorkspaceLock(dir)
	if lockErr != nil {
		// 锁失败（跨进程占用 / 本进程双写）：构造未 ready Store——不加载
		// checkpoint cache（避免陈旧数据），所有写操作 fail-closed，Init/Ready
		// 暴露明确错误。
		return newStoreInternal(dir, false, lockErr, false)
	}
	s := newStoreInternal(dir, false, nil, true)
	s.workspaceRelease = release
	return s
}

// NewReadOnlyStore 创建只读 Store（复核阻塞项 2 只读模式）：不获取 workspace
// 排他锁（不阻挡真实写入进程，诊断/导出/采集可安全并存），所有写操作
// fail-closed（errStoreReadOnly，经统一 guard）。checkpoint 数据损坏同样
// fail-closed（Ready false）。用于 diag 导出、migration 诊断 Host、离线采集、
// inspect 等只读用途。
func NewReadOnlyStore(dir string) *Store {
	return newStoreInternal(dir, true, nil, true)
}

// newStoreInternal 是 Store 构造共享实现。readyErr 非 nil 时所有子 IO 的写操作
// fail-closed（未 ready：锁失败）；loadCheckpoints=false 时跳过 checkpoint cache
// 加载（锁失败路径，避免加载陈旧 cache）；checkpoint 加载失败同样写入
// state.readyErr（缺口 1：checkpoint 损坏的 Store 所有写入口统一拒绝）。
func newStoreInternal(dir string, readOnly bool, readyErr error, loadCheckpoints bool) *Store {
	state := &storeState{readyErr: readyErr, readOnly: readOnly}
	mkIO := func() *IO {
		io := newIO(dir)
		io.state = state
		return io
	}
	io := mkIO()
	outline := NewOutlineStore(io)

	var checkpoints *CheckpointStore
	var cpErr error
	if loadCheckpoints {
		// P0-2：checkpoint 加载 fail-closed（重复 seq / 序号倒退 → 启动报错）。
		checkpoints, cpErr = NewCheckpointStore(io)
	}
	if checkpoints == nil {
		// 加载失败时 NewCheckpointStore 返回 nil——保留空 cache 的可用实例
		// （读不 panic；写仍被统一 guard 拒绝，缺口 1）。
		checkpoints = &CheckpointStore{io: io}
	}
	if cpErr != nil {
		// 缺口 1：checkpoint 损坏必须进入统一 guard（否则 Ready/Init 报错但
		// Drafts.SaveDraft 等仍可写盘）。
		state.mu.Lock()
		state.readyErr = errors.Join(state.readyErr, cpErr)
		state.mu.Unlock()
	}

	return &Store{
		dir:             dir,
		state:           state,
		Progress:        NewProgressStore(mkIO()),
		Outline:         outline,
		ChapterTitles:   NewChapterTitleStore(mkIO()),
		Drafts:          NewDraftStore(mkIO()),
		Summaries:       NewSummaryStore(mkIO(), outline),
		RunMeta:         NewRunMetaStore(mkIO()),
		UserRules:       NewUserRulesStore(mkIO()),
		Signals:         NewSignalStore(mkIO()),
		Runtime:         NewRuntimeStore(mkIO()),
		Characters:      NewCharacterStore(mkIO(), outline),
		Cast:            NewCastStore(mkIO()),
		World:           NewWorldStore(mkIO()),
		Checkpoints:     checkpoints,
		Sessions:        NewSessionStore(mkIO()),
		Usage:           NewUsageStore(mkIO()),
		PrefixManifest:  NewPrefixManifestStore(mkIO()),
		Simulation:      NewSimulationStore(mkIO()),
		StyleAnchors:    NewStyleAnchorsStore(mkIO()),
		Decisions:       NewDecisionStore(mkIO()),
		ProjectProfile:  NewProjectProfileStore(mkIO()),
		PlanningArchive: NewPlanningArchiveStore(mkIO()),
		StyleReview:     NewStyleReviewStore(mkIO()),
		AntiRefusal:     NewAntiRefusalStore(mkIO()),
	}
}

// Dir 返回输出根目录。
func (s *Store) Dir() string { return s.dir }

// ReadOnly 报告 Store 是否为只读模式（NewReadOnlyStore）。
func (s *Store) ReadOnly() bool {
	if s.state == nil {
		return false
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.readOnly
}

// Ready 报告 Store 是否可安全读写（复核阻塞项 4 + 缺口 2）：workspace 锁获取
// 成功（且不是本进程第二个可写实例）、checkpoint 数据校验通过、且未 Close。
// 未 ready 时所有写操作 fail-closed（经统一 guard 返回稳定错误）；调用方应在
// 使用前检查，或调用 Init（同样 fail-closed 暴露错误）。只读 Store 对读是
// ready 的（Ready()=true，写仍被 guard 拒绝）。
func (s *Store) Ready() bool {
	if s.state == nil {
		return true
	}
	if s.state.closed.Load() {
		return false // 缺口 2：Close 后 Ready()=false
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.readyErr == nil
}

// Close 释放 workspace 跨进程排他锁并标记 closed（缺口 2 + 在途写竞争窗口修复）：
//  1. 先取独占写生命周期 lease（lease.Lock）——等待所有在途写（已持读 lease）
//     完成；期间新写要么被独占 lease 阻塞、要么在 RLock 后经 closed guard 拒绝；
//  2. 然后置 closed（atomic，幂等）并释放 workspace 锁。
//
// 彻底消除"旧写通过 guard 后、Close 释放锁、新 Store 并发写"窗口：释放
// workspace 锁时保证没有任何本 Store 的物理写在途。并发 Close 安全
// （lease.Lock 串行 + closed.Swap 幂等，后到者等待独占 lease 后直接返回）。
// 不调用也安全：进程退出/崩溃时 OS 自动释放句柄锁。
func (s *Store) Close() {
	if s.state == nil {
		return
	}
	s.state.lease.Lock() // 等待所有在途写（读 lease）完成
	defer s.state.lease.Unlock()
	if s.state.closed.Swap(true) {
		return // 幂等：已关闭
	}
	if s.workspaceRelease != nil {
		s.workspaceRelease()
		s.workspaceRelease = nil
	}
}

// CheckConsistency 对事实层做一次浅层校验，用于启动/恢复时生成 warning。
// 纯只读：不修正数据，仅返回可读的问题描述。调用方决定如何展示（log / UI）。
// 为避免扫全目录带来的 IO 开销，只校验 Progress 的关键点：
//   - 最后一个完成章节必须在 chapters/ 下存在终稿
//   - Layered 模式下，当前 Volume/Arc 必须能在 layered_outline 中找到
func (s *Store) CheckConsistency() []string {
	var warnings []string
	progress, err := s.Progress.Load()
	if err != nil || progress == nil {
		return warnings
	}
	if n := len(progress.CompletedChapters); n > 0 {
		lastCh := progress.CompletedChapters[n-1]
		if text, err := s.Drafts.LoadChapterText(lastCh); err == nil && text == "" {
			warnings = append(warnings, fmt.Sprintf("progress 标记第 %d 章已完成，但 chapters/%02d.md 不存在或为空", lastCh, lastCh))
		}
	}
	if progress.Layered && progress.CurrentVolume > 0 && progress.CurrentArc > 0 {
		volumes, err := s.Outline.LoadLayeredOutline()
		if err == nil && len(volumes) > 0 {
			found := false
			for _, v := range volumes {
				if v.Index != progress.CurrentVolume {
					continue
				}
				for _, a := range v.Arcs {
					if a.Index == progress.CurrentArc {
						found = true
						break
					}
				}
				break
			}
			if !found {
				warnings = append(warnings, fmt.Sprintf("progress 当前 V%d A%d 在分层大纲中找不到对应条目", progress.CurrentVolume, progress.CurrentArc))
			}
		}
	}
	return warnings
}

// FoundationMissing 返回基础设定中尚缺的项，按用于 Prompt/Reminder 的稳定顺序排列。
// 长篇模式（已有 layered_outline）额外要求 compass。
func (s *Store) FoundationMissing() []string {
	var missing []string
	if p, _ := s.Outline.LoadPremise(); p == "" {
		missing = append(missing, "premise")
	}
	if o, _ := s.Outline.LoadOutline(); len(o) == 0 {
		missing = append(missing, "outline")
	}
	if c, _ := s.Characters.Load(); len(c) == 0 {
		missing = append(missing, "characters")
	}
	if r, _ := s.World.LoadWorldRules(); len(r) == 0 {
		missing = append(missing, "world_rules")
	}
	if layered, _ := s.Outline.LoadLayeredOutline(); len(layered) > 0 {
		if c, _ := s.Outline.LoadCompass(); c == nil {
			missing = append(missing, "compass")
		}
	}
	return missing
}

// Init 创建所需的子目录结构。
// P0-1/P0-2 fail-closed：workspace 锁获取失败（另一进程/本进程另一实例占用）、
// checkpoint 数据损坏或已 Close 时返回明确错误，不继续运行。等价于检查 Ready()。
func (s *Store) Init() error {
	if err := s.state.writeBlocked(); err != nil {
		return err
	}
	return s.Progress.io.EnsureDirs([]string{
		"chapters", "summaries", "drafts", "reviews", "meta", "meta/runtime", "meta/runtime/tasks", "meta/style_review", "meta/sessions", "meta/sessions/agents",
	})
}

// ── 跨域协调方法 ──

// LockPlanningAndOutline 以固定锁顺序（crossMu → Outline.io.mu → PlanningArchive.io.mu）
// 执行 fn。用于跨 PlanningArchive 和 Outline（compass）的原子操作。
// 调用方不可在 fn 内部再获取任何 mu（已持有）。
// 设计用于后续"最终 marker 校验 + Compass 保存"场景。
func (s *Store) LockPlanningAndOutline(fn func() error) error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()

	s.PlanningArchive.io.mu.Lock()
	defer s.PlanningArchive.io.mu.Unlock()

	return fn()
}

// SaveCompassWithMarkerCheck 在 LockPlanningAndOutline 临界区内执行 marker
// target 重校验 + SaveCompass，避免与 DeleteArchiveEntrySafe 的锁顺序冲突。
// markerCheck 在持有锁时对已加载的 open_threads 做 final validation。
// markerCheck 接收 compass + archive（均已加载，不再需要获取锁）。
// 仅当 markerCheck 通过后才写入。
func (s *Store) SaveCompassWithMarkerCheck(compass domain.StoryCompass, markerCheck func(c domain.StoryCompass, archive *domain.PlanningArchiveV1) error) error {
	return s.LockPlanningAndOutline(func() error {
		// 在锁内加载 archive（不再获取锁）；loadUnlocked 已含 Validate，
		// 加载失败时必须传播错误，不可忽略。
		archive, err := s.PlanningArchive.loadUnlocked()
		if err != nil {
			return fmt.Errorf("planning archive: load in SaveCompassWithMarkerCheck: %w", err)
		}
		if err := markerCheck(compass, archive); err != nil {
			return err
		}
		return s.Outline.SaveCompassUnlocked(compass)
	})
}

// ExpandArc 将骨架弧校准并展开为详细章节（Outline + Progress 联动）。
func (s *Store) ExpandArc(volumeIdx, arcIdx int, expansion domain.ArcExpansion) error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()

	volumes, err := s.Outline.expandArcUnlocked(volumeIdx, arcIdx, expansion)
	if err != nil {
		return err
	}

	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	p, err := s.Progress.loadUnlocked()
	if err != nil {
		return err
	}
	if p == nil {
		p = &domain.Progress{}
	}
	p.TotalChapters = domain.TotalChapters(volumes)
	return s.Progress.saveUnlocked(p)
}

// AppendVolume 追加新卷到分层大纲末尾（Outline + Progress 联动）。
func (s *Store) AppendVolume(vol domain.VolumeOutline) error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()

	volumes, err := s.Outline.appendVolumeUnlocked(vol)
	if err != nil {
		return err
	}

	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	p, err := s.Progress.loadUnlocked()
	if err != nil {
		return err
	}
	if p == nil {
		p = &domain.Progress{}
	}
	p.TotalChapters = domain.TotalChapters(volumes)
	return s.Progress.saveUnlocked(p)
}

// DeleteArchiveEntrySafe 跨域原子操作：在 crossMu → Outline.io.mu → PlanningArchive.io.mu
// 锁顺序下检查 compass.long.open_threads（通过 checkRef 回调）并删除 archive 条目。
// checkRef 接收 open_threads 切片，返回 nil 表示可删除，非 nil 表示拒绝。
// 工具层应传入使用 ParseOpenThreadMarkers 严格解析的检查函数。
func (s *Store) DeleteArchiveEntrySafe(kind, id string, checkRef func(threads []string) error) error {
	if kind == "" || id == "" {
		return fmt.Errorf("kind and id must not be empty")
	}
	return s.LockPlanningAndOutline(func() error {
		// 加载 compass 检查 open_threads（回调使用 tools.ParseOpenThreadMarkers）
		compass, err := s.Outline.loadCompassUnlocked()
		if err != nil {
			return fmt.Errorf("load compass: %w", err)
		}
		if compass != nil && len(compass.Long.OpenThreads) > 0 {
			if err := checkRef(compass.Long.OpenThreads); err != nil {
				return err
			}
		}
		// 删除 archive 条目
		return s.PlanningArchive.deleteEntryUnlocked(kind, id)
	})
}

// ClearHandledSteer 原子性清除 PendingSteer 并重置 FlowSteering 状态
// （RunMeta + Progress 联动）。
func (s *Store) ClearHandledSteer() error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.RunMeta.io.mu.Lock()
	defer s.RunMeta.io.mu.Unlock()

	meta, err := s.RunMeta.loadUnlocked()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if meta != nil && meta.PendingSteer != "" {
		meta.PendingSteer = ""
		if err := s.RunMeta.saveUnlocked(*meta); err != nil {
			return err
		}
	}

	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	p, err := s.Progress.loadUnlocked()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if p != nil && p.Flow == domain.FlowSteering {
		if err := domain.ValidateFlowTransition(p.Flow, domain.FlowWriting); err != nil {
			return err
		}
		p.Flow = domain.FlowWriting
		if err := s.Progress.saveUnlocked(p); err != nil {
			return err
		}
	}
	return nil
}

// ── P0-3 / P0-4：跨域原子落盘（CAS 防 TOCTOU）────────────────────────
//
// 历史死锁根因：polish 模型调用期间另一在途流程覆盖草稿、critic 接受旧候选
// （digest 不匹配）、checkpoint 重复 seq 导致顺序绑定错乱。Commit* 方法在统一
// 锁序（crossMu → RunMeta → Drafts → StyleReview → Checkpoints → Progress，
// 复核阻塞项 5 的 mutation 判定需要 RunMeta/Progress）的临界区内完成
// "重载 + CAS 校验 + 写入"，校验与写入之间无窗口。
// 锁序说明：所有跨域操作都先取 crossMu（与 LockPlanningAndOutline/ExpandArc/
// ClearHandledSteer 同约定），其余各域 IO 锁为独立锁且无反向嵌套路径，无死锁环。

// ErrPolishCandidateStale 表示 polish 候选已过期：模型调用期间草稿/账本/
// polish checkpoint 被并发修改。候选被丢弃（草稿未被覆盖），调用方应返回
// 明确错误让 writer 重新走 polish 流程。
var ErrPolishCandidateStale = errors.New("polish candidate stale: draft changed during polish, discard and retry")

// ErrReviewStale 表示评审候选已过期：critic 调用期间草稿/账本/polish
// checkpoint 被并发修改。accepted_* 结果不落盘，该 attempt 已在账本中标记为
// degraded（stale），调用方返回明确错误/警告让 writer 重新 review。
var ErrReviewStale = errors.New("review candidate stale: draft changed during review")

// LedgerBaseline 是模型调用前捕获的 style review 账本基线（P0-3 CAS #2 用）。
// 单写者模型下模型调用期间账本不应有任何变化；任何变化（新 pending/terminal/
// degraded/override 周期）都意味着并发干扰 → 候选过期。
type LedgerBaseline struct {
	Status     domain.StyleReviewStatus
	CycleCount int
}

// ledgerBaselineMatches 判断账本当前状态与基线一致（P0-3 CAS #2）。
func ledgerBaselineMatches(ledger *domain.StyleReviewLedger, baseline LedgerBaseline) bool {
	var status domain.StyleReviewStatus
	count := 0
	if ledger != nil {
		status = ledger.CurrentStatus()
		count = len(ledger.Cycles)
	}
	return status == baseline.Status && count == baseline.CycleCount
}

// PolishBaseline 是 polish 输入基线（复核阻塞项 5）：同一锁序下原子读取的
// 草稿快照 + 账本状态 + polish checkpoint 绑定 + mutation 许可判定。
// MutationAllowed=false 表示账本当前状态锁定草稿修改（pending/terminal/
// exhausted 锁定；重写队列豁免与 tools.CheckStyleReviewMutationGuard 同语义），
// 调用方不得启动 polisher。
type PolishBaseline struct {
	Content     string // 草稿全文（与 InputDigest 同快照，可直接用于 polisher 任务）
	InputDigest string
	Ledger      LedgerBaseline
	PolishSeq   int64 // 模型输入时的最新 polish checkpoint seq（0 = 无）

	MutationAllowed       bool
	MutationBlockedReason string // MutationAllowed=false 时的原因（供错误消息）
}

// CapturePolishBaseline 在统一锁序（crossMu → RunMeta → Drafts → StyleReview →
// Checkpoints → Progress）下原子捕获 polish 输入基线（复核阻塞项 5）：草稿
// digest + 账本状态 + 最新 polish seq，并在同一临界区内判定账本是否允许修改
// 草稿——拒绝"baseline 捕获时 ledger 已 terminal"的窗口（guard 判定与捕获之间
// 无间隙，另一流程在 guard 通过后、捕获前追加 terminal 周期会被捕获阶段直接
// 拒绝）。polish_draft 用它替换"分别读取 content/guard/seq/ledger"。
func (s *Store) CapturePolishBaseline(chapter int) (*PolishBaseline, error) {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()
	s.RunMeta.io.mu.Lock()
	defer s.RunMeta.io.mu.Unlock()
	s.Drafts.io.mu.Lock()
	defer s.Drafts.io.mu.Unlock()
	s.StyleReview.io.mu.Lock()
	defer s.StyleReview.io.mu.Unlock()
	s.Checkpoints.io.mu.Lock()
	defer s.Checkpoints.io.mu.Unlock()
	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	content, err := s.Drafts.io.ReadFileUnlocked(fmt.Sprintf("drafts/%02d.draft.md", chapter))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load chapter content: %w", err)
	}
	ledger, err := s.StyleReview.loadUnlocked(chapter)
	if err != nil {
		return nil, fmt.Errorf("load style review ledger: %w", err)
	}
	allowed, reason, err := s.styleReviewAllowsMutationLocked(chapter)
	if err != nil {
		return nil, err
	}
	var polishSeq int64
	if cp := s.Checkpoints.latestByStepUnlocked(domain.ChapterScope(chapter), "polish"); cp != nil {
		polishSeq = cp.Seq
	}

	b := &PolishBaseline{
		Content:               string(content),
		InputDigest:           domain.DigestDraft(string(content)),
		Ledger:                ledgerBaselineOf(ledger),
		PolishSeq:             polishSeq,
		MutationAllowed:       allowed,
		MutationBlockedReason: reason,
	}
	return b, nil
}

func ledgerBaselineOf(ledger *domain.StyleReviewLedger) LedgerBaseline {
	if ledger == nil {
		return LedgerBaseline{}
	}
	return LedgerBaseline{Status: ledger.CurrentStatus(), CycleCount: len(ledger.Cycles)}
}

// styleReviewAllowsMutationLocked 判定 critic 模式下章节草稿当前是否允许修改
// （与 tools.CheckStyleReviewMutationGuard 完全同语义——重写队列 digest/status
// 感知豁免；复核阻塞项 5 在锁内复用同一判定，杜绝两条判定路径漂移）。
// 调用方必须已持有 RunMeta/Drafts/StyleReview/Checkpoints/Progress 的 io.mu
// （CapturePolishBaseline / CommitPolishCandidate 的锁链）。
// 返回 (allowed, 拒绝原因, err)。
func (s *Store) styleReviewAllowsMutationLocked(chapter int) (bool, string, error) {
	meta, err := s.RunMeta.loadUnlocked()
	if err != nil {
		return false, "", fmt.Errorf("load run meta: %w", err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return true, "", nil // off 模式不拦截
	}
	ledger, err := s.StyleReview.loadUnlocked(chapter)
	if err != nil {
		return false, "", fmt.Errorf("load style review ledger: %w", err)
	}

	// 已完成 + 重写队列 → digest/status 感知豁免（与 chapter_guard.go 的
	// CheckStyleReviewMutationGuard 同语义）。
	inRewriteQueue, err := s.completedAndInRewriteQueueLocked(chapter)
	if err != nil {
		return false, "", err
	}
	if inRewriteQueue && ledger != nil && !ledger.IsEmpty() {
		status := ledger.CurrentStatus()
		cycle := ledger.CurrentCycle()
		if status == domain.ReviewStatusInitialPending || status == domain.ReviewStatusFinalPending {
			return false, fmt.Sprintf("critic 模式：章节 %d 有未完成评审（%s），不能修改", chapter, status), nil
		}
		if status == domain.ReviewStatusExhausted {
			return false, fmt.Sprintf("critic 模式：章节 %d 评审已耗尽，必须先 /style-override，不能修改", chapter), nil
		}
		draft, dErr := s.Drafts.io.ReadFileUnlocked(fmt.Sprintf("drafts/%02d.draft.md", chapter))
		if dErr != nil && !os.IsNotExist(dErr) {
			return false, "", fmt.Errorf("load draft: %w", dErr)
		}
		final, fErr := s.Drafts.io.ReadFileUnlocked(fmt.Sprintf("chapters/%02d.md", chapter))
		if fErr != nil && !os.IsNotExist(fErr) {
			return false, "", fmt.Errorf("load final chapter: %w", fErr)
		}
		draftExists, finalExists := len(draft) > 0, len(final) > 0
		draftDigest := ""
		if draftExists {
			draftDigest = domain.DigestDraft(string(draft))
		}
		rewriteNotStarted := !draftExists || (finalExists && draftDigest == domain.DigestDraft(string(final)))

		switch {
		case rewriteNotStarted && status.IsTerminal():
			return true, "", nil // 原终稿未开始重写：允许开始修改
		case status == domain.ReviewStatusRevisionOpen:
			return true, "", nil
		case status == domain.ReviewStatusDegraded:
			return true, "", nil
		case status.IsTerminal() && cycle != nil &&
			domain.IsValidDigest(cycle.DraftDigest) && cycle.DraftDigest == draftDigest:
			return false, fmt.Sprintf("critic 模式：章节 %d 当前重写候选已获终态评审（%s）且摘要匹配，正文已锁定；只能 commit_chapter", chapter, status), nil
		case status.IsTerminal():
			return true, "", nil // 旧 terminal digest 不匹配当前返工草稿：返工进行中
		default:
			return false, fmt.Sprintf("critic 模式：章节 %d 评审状态 %s 不允许修改", chapter, status), nil
		}
	}

	if ledger == nil || ledger.IsEmpty() {
		return true, "", nil // 尚无风格评审，允许首次起草
	}
	switch ledger.CurrentStatus() {
	case domain.ReviewStatusRevisionOpen, domain.ReviewStatusDegraded:
		return true, "", nil
	case domain.ReviewStatusInitialPending, domain.ReviewStatusFinalPending:
		return false, fmt.Sprintf("critic 模式：章节 %d 有未完成的评审（%s），不能修改", chapter, ledger.CurrentStatus()), nil
	default:
		// terminal（accepted_initial/accepted_revised/overridden）+ exhausted 拒绝修改
		return false, fmt.Sprintf("critic 模式：章节 %d 评审状态 %s 不允许修改", chapter, ledger.CurrentStatus()), nil
	}
}

// completedAndInRewriteQueueLocked 判断章节是否已完成且在重写/打磨队列中
// （tools.isCompletedAndInRewriteQueue 的锁内版本；调用方须持有 Progress.io.mu）。
func (s *Store) completedAndInRewriteQueueLocked(chapter int) (bool, error) {
	p, err := s.Progress.loadUnlocked()
	if err != nil {
		return false, fmt.Errorf("load progress: %w", err)
	}
	if p == nil || !slices.Contains(p.CompletedChapters, chapter) {
		return false, nil
	}
	return slices.Contains(p.PendingRewrites, chapter), nil
}

// CommitPolishCandidate 在统一写锁内原子完成 polish 候选的 CAS 校验与落盘（P0-3
// + 复核阻塞项 5）。校验（任一不满足 → 丢弃候选、不覆盖草稿、不写 checkpoint，
// 返回 ErrPolishCandidateStale）：
//  1. 当前草稿 digest 仍等于基线 InputDigest
//  2. style review 账本状态未在模型调用期间变化（仍等于基线 Ledger）
//     2b. 账本当前状态仍允许修改草稿（复核阻塞项 5：terminal 锁定的基线在捕获时
//     已被拒绝；这里对提交时刻的账本再判一次——即使调用方误传非可修改基线，
//     也不会覆盖已锁定草稿）
//  3. 没有更新的 polish checkpoint 抢先建立（LatestByStep polish 的 seq 仍等于
//     基线 PolishSeq）
//
// 校验通过后在同一临界区内：保存草稿 → draft checkpoint → polish checkpoint
// （meta 附加精修元数据），校验与写入之间无窗口。返回新写入的 polish checkpoint。
func (s *Store) CommitPolishCandidate(chapter int, candidate string, baseline *PolishBaseline, meta domain.PolishCheckpointMeta) (*domain.Checkpoint, error) {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()
	s.RunMeta.io.mu.Lock()
	defer s.RunMeta.io.mu.Unlock()
	s.Drafts.io.mu.Lock()
	defer s.Drafts.io.mu.Unlock()
	s.StyleReview.io.mu.Lock()
	defer s.StyleReview.io.mu.Unlock()
	s.Checkpoints.io.mu.Lock()
	defer s.Checkpoints.io.mu.Unlock()
	s.Progress.io.mu.Lock()
	defer s.Progress.io.mu.Unlock()

	// 1. 当前草稿 digest 仍等于模型输入 digest（防在途覆盖丢失）。
	draftRel := fmt.Sprintf("drafts/%02d.draft.md", chapter)
	cur, err := s.Drafts.io.ReadFileUnlocked(draftRel)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reload draft for polish CAS: %w", err)
	}
	if domain.DigestDraft(string(cur)) != baseline.InputDigest {
		return nil, ErrPolishCandidateStale
	}

	// 2. style review 账本状态未变化。
	ledger, err := s.StyleReview.loadUnlocked(chapter)
	if err != nil {
		return nil, fmt.Errorf("reload style review ledger for polish CAS: %w", err)
	}
	if !ledgerBaselineMatches(ledger, baseline.Ledger) {
		return nil, ErrPolishCandidateStale
	}

	// 2b. 复核阻塞项 5：账本当前状态仍允许修改（terminal 锁定 → 丢弃候选）。
	allowed, _, mErr := s.styleReviewAllowsMutationLocked(chapter)
	if mErr != nil {
		return nil, fmt.Errorf("mutation check for polish CAS: %w", mErr)
	}
	if !allowed {
		return nil, ErrPolishCandidateStale
	}

	// 3. 没有更新的 polish checkpoint 抢先建立。
	latest := s.Checkpoints.latestByStepUnlocked(domain.ChapterScope(chapter), "polish")
	latestSeq := int64(0)
	if latest != nil {
		latestSeq = latest.Seq
	}
	if latestSeq != baseline.PolishSeq {
		return nil, ErrPolishCandidateStale
	}

	// 4. 校验通过：同一临界区内原子落盘（保存草稿 → draft checkpoint → polish checkpoint）。
	if err := s.Drafts.io.WriteFileUnlocked(draftRel, []byte(candidate)); err != nil {
		return nil, fmt.Errorf("save polished draft: %w", err)
	}
	if _, err := s.Checkpoints.appendUnlocked(
		domain.ChapterScope(chapter), "draft", draftRel, domain.DigestDraft(candidate), true, nil,
	); err != nil {
		return nil, fmt.Errorf("checkpoint draft after polish: %w", err)
	}
	cp, err := s.Checkpoints.appendUnlocked(
		domain.ChapterScope(chapter), "polish", draftRel, domain.DigestDraft(candidate), false, &meta,
	)
	if err != nil {
		return nil, fmt.Errorf("checkpoint polish: %w", err)
	}
	return cp, nil
}

// CommitDegradedPolishCheckpoint 在统一锁序内原子完成 degraded/rejected polish
// checkpoint 的 CAS 校验与追加（复核阻塞项 6：正文未变、不写草稿；替换读后写
// 的 draftDigestUnchanged 预检——校验与追加之间无窗口）。
// 校验（任一不满足 → 不追加陈旧绑定，返回 ErrPolishCandidateStale）：
//  1. 当前草稿 digest 仍等于基线 InputDigest（模型调用期间草稿被并发修改时不写
//     绑定旧 digest 的 degraded 记录）
//  2. style review 账本状态未变化（基线 Ledger）
//  3. 最新 polish checkpoint seq 仍等于基线 PolishSeq
//
// 校验通过后追加一条 degraded/rejected checkpoint（Digest/InputDigest 强制绑定
// 当前草稿 digest=基线 InputDigest，防止调用方误传）。
func (s *Store) CommitDegradedPolishCheckpoint(chapter int, baseline *PolishBaseline, meta domain.PolishCheckpointMeta) (*domain.Checkpoint, error) {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()
	s.Drafts.io.mu.Lock()
	defer s.Drafts.io.mu.Unlock()
	s.StyleReview.io.mu.Lock()
	defer s.StyleReview.io.mu.Unlock()
	s.Checkpoints.io.mu.Lock()
	defer s.Checkpoints.io.mu.Unlock()

	// 1. 当前草稿 digest 仍等于模型输入 digest（不写陈旧绑定）。
	draftRel := fmt.Sprintf("drafts/%02d.draft.md", chapter)
	cur, err := s.Drafts.io.ReadFileUnlocked(draftRel)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reload draft for degraded polish CAS: %w", err)
	}
	if domain.DigestDraft(string(cur)) != baseline.InputDigest {
		return nil, ErrPolishCandidateStale
	}

	// 2. style review 账本状态未变化。
	ledger, err := s.StyleReview.loadUnlocked(chapter)
	if err != nil {
		return nil, fmt.Errorf("reload style review ledger for degraded polish CAS: %w", err)
	}
	if !ledgerBaselineMatches(ledger, baseline.Ledger) {
		return nil, ErrPolishCandidateStale
	}

	// 3. 最新 polish checkpoint 未变化。
	latest := s.Checkpoints.latestByStepUnlocked(domain.ChapterScope(chapter), "polish")
	latestSeq := int64(0)
	if latest != nil {
		latestSeq = latest.Seq
	}
	if latestSeq != baseline.PolishSeq {
		return nil, ErrPolishCandidateStale
	}

	// 4. 校验通过：追加 degraded/rejected checkpoint（Digest 强制绑定当前草稿
	//    digest=基线 InputDigest，防止调用方误传陈旧绑定）。
	meta.InputDigest = baseline.InputDigest
	return s.Checkpoints.appendUnlocked(
		domain.ChapterScope(chapter), "polish", draftRel, baseline.InputDigest, false, &meta,
	)
}

// CommitReviewResult 在统一写锁内原子完成评审结果的 CAS 校验与落盘（P0-4
// + 复核阻塞项 7）。校验（任一不满足 → 不写 accepted 结果：账本中追加 degraded
// 周期标记该 attempt 已过期，并返回 ErrReviewStale）：
//  1. 当前草稿 digest 仍等于 pending entry 的 digest（expectedDraftDigest）
//  2. pending attempt（expectedAttemptID）仍是账本当前权威 attempt（未被更新的
//     attempt 覆盖，仍处于 pending 状态）
//  3. 绑定的 polish checkpoint 仍是当前 polish（LatestByStep polish 的 seq 仍等于
//     pending entry 绑定的 expectedPolishSeq；0 = 无绑定，当前也必须无 polish）
//
// 复核阻塞项 7：机械门禁（gate 回调）在 CAS 身份校验通过后、结果追加前执行，
// 使用与 digest 校验同一草稿快照——critic 期间草稿被并发修改时先走 stale 检测
// 标记 degraded，不会被门禁提前返回错误而遗留 stranded pending。gate 失败 →
// 同样标记 stale（不追加结果）。gate 为 nil 时跳过（revise 结果路径）。
// 校验通过后调用 loader 追加结果周期（append-only 语义与 StyleReview.Update 相同）。
func (s *Store) CommitReviewResult(chapter int, expectedAttemptID, expectedDraftDigest string, expectedPolishSeq int64, gate func(draft string) error, loader func(ledger *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error)) error {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()
	s.Drafts.io.mu.Lock()
	defer s.Drafts.io.mu.Unlock()
	s.StyleReview.io.mu.Lock()
	defer s.StyleReview.io.mu.Unlock()
	s.Checkpoints.io.mu.Lock()
	defer s.Checkpoints.io.mu.Unlock()

	// 1. 重载账本（当前权威 attempt；stale 标记需要复用其 request/digest 才能通过
	//    append-only 校验——ValidateLedger 的 attempt-id 绑定要求结果周期与 pending
	//    周期的 attempt_id/draft_digest/basis_digest/request 完全一致）。
	ledger, err := s.StyleReview.loadUnlocked(chapter)
	if err != nil {
		return fmt.Errorf("reload style review ledger for review CAS: %w", err)
	}
	var curCycle *domain.StyleReviewEntry
	if ledger != nil && !ledger.IsEmpty() {
		curCycle = ledger.CurrentCycle()
	}

	// 2. 当前草稿 digest 仍等于 pending entry 的 digest。
	cur, err := s.Drafts.io.ReadFileUnlocked(fmt.Sprintf("drafts/%02d.draft.md", chapter))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reload draft for review CAS: %w", err)
	}
	if domain.DigestDraft(string(cur)) != expectedDraftDigest {
		return s.markReviewStale(chapter, expectedAttemptID, expectedDraftDigest, curCycle,
			"草稿在评审期间被修改")
	}

	// 3. pending attempt 仍是当前权威 attempt（未被更新的 attempt 覆盖）。
	if curCycle == nil {
		return s.markReviewStale(chapter, expectedAttemptID, expectedDraftDigest, nil,
			"评审账本在评审期间消失")
	}
	if curCycle.AttemptID != expectedAttemptID ||
		(curCycle.Status != domain.ReviewStatusInitialPending && curCycle.Status != domain.ReviewStatusFinalPending) {
		return s.markReviewStale(chapter, expectedAttemptID, expectedDraftDigest, curCycle,
			"评审 attempt 在评审期间被更新的 attempt 覆盖")
	}

	// 4. 绑定的 polish checkpoint 仍是当前 polish。
	latestPolish := s.Checkpoints.latestByStepUnlocked(domain.ChapterScope(chapter), "polish")
	latestSeq := int64(0)
	if latestPolish != nil {
		latestSeq = latestPolish.Seq
	}
	if latestSeq != expectedPolishSeq {
		return s.markReviewStale(chapter, expectedAttemptID, expectedDraftDigest, curCycle,
			"polish checkpoint 在评审期间被更新")
	}

	// 5. 复核阻塞项 7：机械门禁在 CAS 身份确认后执行（同一草稿快照）。
	//    门禁失败 → 标记 stale（degraded），不遗留 stranded pending。
	if gate != nil {
		if gErr := gate(string(cur)); gErr != nil {
			return s.markReviewStale(chapter, expectedAttemptID, expectedDraftDigest, curCycle,
				fmt.Sprintf("评审结果被机械门禁拒绝：%v", gErr))
		}
	}

	// 6. 校验通过：append-only 追加结果周期。
	return s.StyleReview.updateUnlocked(chapter, loader)
}

// markReviewStale 在账本中追加 degraded 周期标记该 attempt 已过期（不写 accepted
// 结果），并返回 ErrReviewStale。pendingCycle 提供标记周期必须复用的 request 与
// basis_digest（ValidateLedger 的 attempt-id 绑定校验要求）。标记写入是
// best-effort：若 append-only/状态机校验拒绝（如当前状态不允许 degraded 转移），
// 仍返回 ErrReviewStale——"不写过期 accepted 结果"是硬保证，标记只是给 writer
// 的提示。
func (s *Store) markReviewStale(chapter int, attemptID, draftDigest string, pendingCycle *domain.StyleReviewEntry, cause string) error {
	now := time.Now().Format(time.RFC3339)
	var request *domain.StyleReviewRequest
	basisDigest := ""
	if pendingCycle != nil {
		request = pendingCycle.Request
		basisDigest = pendingCycle.BasisDigest
	}
	if request == nil {
		request = &domain.StyleReviewRequest{}
	}
	entry := domain.StyleReviewEntry{
		Status:      domain.ReviewStatusDegraded,
		CreatedAt:   now,
		AttemptID:   attemptID,
		Request:     request,
		DraftDigest: draftDigest,
		BasisDigest: basisDigest,
		Error:       cause,
	}
	_ = s.StyleReview.updateUnlocked(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, nil
		}
		entry.Cycle = len(cur.Cycles) + 1
		entry.Epoch = cur.MaxEpoch()
		cur.Cycles = append(cur.Cycles, entry)
		return cur, nil
	})
	return ErrReviewStale
}
