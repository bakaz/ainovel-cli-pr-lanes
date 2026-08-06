package store

import (
	"fmt"
	"os"
	"sync"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// Store 是状态管理的组合根，持有所有子存储。
type Store struct {
	dir string

	Progress        *ProgressStore
	Outline         *OutlineStore
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

	crossMu sync.Mutex // 保护跨域原子操作
}

// NewStore 创建状态管理器，dir 为小说输出根目录。
func NewStore(dir string) *Store {
	io := newIO(dir)
	outline := NewOutlineStore(io)
	return &Store{
		dir:             dir,
		Progress:        NewProgressStore(newIO(dir)),
		Outline:         outline,
		Drafts:          NewDraftStore(newIO(dir)),
		Summaries:       NewSummaryStore(newIO(dir), outline),
		RunMeta:         NewRunMetaStore(newIO(dir)),
		UserRules:       NewUserRulesStore(newIO(dir)),
		Signals:         NewSignalStore(newIO(dir)),
		Runtime:         NewRuntimeStore(newIO(dir)),
		Characters:      NewCharacterStore(newIO(dir), outline),
		Cast:            NewCastStore(newIO(dir)),
		World:           NewWorldStore(newIO(dir)),
		Checkpoints:     NewCheckpointStore(io),
		Sessions:        NewSessionStore(newIO(dir)),
		Usage:           NewUsageStore(newIO(dir)),
		PrefixManifest:  NewPrefixManifestStore(newIO(dir)),
		Simulation:      NewSimulationStore(newIO(dir)),
		StyleAnchors:    NewStyleAnchorsStore(newIO(dir)),
		Decisions:       NewDecisionStore(newIO(dir)),
		ProjectProfile:  NewProjectProfileStore(newIO(dir)),
		PlanningArchive: NewPlanningArchiveStore(newIO(dir)),
		StyleReview:     NewStyleReviewStore(newIO(dir)),
	}
}

// Dir 返回输出根目录。
func (s *Store) Dir() string { return s.dir }

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
func (s *Store) Init() error {
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
