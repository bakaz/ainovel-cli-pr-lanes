package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// CommitChapterTool 提交章节：加载正文 → 保存终稿 → 生成摘要 → 更新状态 → 更新进度。
type CommitChapterTool struct {
	store *store.Store
	// polishPipeline 是精修流水线的 commit 门控配置；nil = pipeline 关闭（不拦截）。
	// 由 BuildWorkers 按配置注入（SetPolishPipeline）。
	polishPipeline *PolishPipelineConfig
	// fsmConfig 是章节流水线强制状态机配置（BuildWorkers 注入）；Enabled 时
	// Execute 入口调用 RequireChapterAction 强制顺序（needs_commit 才允许提交）。
	fsmConfig ChapterFSMConfig
}

// PolishPipelineConfig 是精修流水线的 commit 门控配置。
type PolishPipelineConfig struct {
	// ExpectedModel 是 roles.polisher 显式配置时的当前模型名；空 = 未显式配置
	// （跳过 polish checkpoint 的模型一致性校验）。
	ExpectedModel string
}

func NewCommitChapterTool(store *store.Store) *CommitChapterTool {
	return &CommitChapterTool{store: store}
}

// SetPolishPipeline 启用精修流水线 commit 门控（cfg 为 nil 时关闭）。
func (t *CommitChapterTool) SetPolishPipeline(cfg *PolishPipelineConfig) { t.polishPipeline = cfg }

// SetChapterFSMConfig 注入章节流水线强制状态机配置（BuildWorkers 调用）。
func (t *CommitChapterTool) SetChapterFSMConfig(cfg ChapterFSMConfig) { t.fsmConfig = cfg }

// FSMConfig 返回注入的章节流水线配置（构建/测试诊断用）。
func (t *CommitChapterTool) FSMConfig() ChapterFSMConfig { return t.fsmConfig }

// commitOutput 在 domain.CommitResult 之上嵌入扩展字段，保持 domain 包不依赖 rules。
// 由于嵌入字段会被 JSON marshaler 提升（promoted），序列化结果等同于扁平结构。
type commitOutput struct {
	domain.CommitResult
	FinalTitle     string            `json:"final_title"`
	RuleViolations []rules.Violation `json:"rule_violations,omitempty"`
}

const maxFinalTitleRunes = 120

// normalizeFinalTitle 校验并规范化工具层传入的最终标题。
// 空字符串表示调用方未提供标题；仅空白的显式值则拒绝，避免把已有标题
// 意外清空。标题长度按 Unicode code point 计算，而不是 UTF-8 字节数。
func normalizeFinalTitle(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", fmt.Errorf("final_title 不能仅包含空白: %w", errs.ErrToolArgs)
	}
	if runes := utf8.RuneCountInString(title); runes > maxFinalTitleRunes {
		return "", fmt.Errorf("final_title 长度 %d 超过上限 %d 个 Unicode 字符: %w", runes, maxFinalTitleRunes, errs.ErrToolArgs)
	}
	return title, nil
}

func (t *CommitChapterTool) Name() string { return "commit_chapter" }
func (t *CommitChapterTool) Description() string {
	return "提交章节终稿。加载草稿正文保存为终稿，更新时间线、伏笔、关系、角色状态和进度。" +
		"返回结构化事实：next_chapter / review_required / arc_end / volume_end / needs_expansion / book_complete / flow 等"
}
func (t *CommitChapterTool) Label() string { return "提交章节" }

// 写工具（跨域原子操作：草稿→终稿→摘要→进度→checkpoint），禁止并发。
func (t *CommitChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *CommitChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *CommitChapterTool) Schema() map[string]any {
	timelineSchema := schema.Object(
		schema.Property("time", schema.String("故事内时间")).Required(),
		schema.Property("event", schema.String("事件描述")).Required(),
		schema.Property("characters", schema.Array("涉及角色", schema.String(""))),
	)
	foreshadowSchema := schema.Object(
		schema.Property("id", schema.String("伏笔 ID")).Required(),
		schema.Property("action", schema.Enum("操作", "plant", "advance", "resolve", "retire")).Required(),
		schema.Property("description", schema.String("伏笔描述（仅 plant 时必需）")),
		schema.Property("horizon", schema.Enum("伏笔跨度（仅 plant 时必填：跨弧长线或贯穿全书）", "cross_arc", "book")),
		schema.Property("evidence", schema.String("正文精确短引文（advance/resolve 必填，须逐字出现在本章草稿中）")),
		schema.Property("reason", schema.String("取消承诺原因（仅 retire 必填）")),
	)
	characterStateSchema := schema.Object(
		schema.Property("entity", schema.String("角色名或实体名")).Required(),
		schema.Property("field", schema.String("受控命名空间：body_device./health./location./capability./resource./inventory./status./knowledge.")).Required(),
		schema.Property("value", schema.String("当前状态值（≤800 字）。空字符串且提供 reason = 从当前账本移除该 field")).Required(),
		schema.Property("reason", schema.String("状态变化原因")),
		schema.Property("evidence", schema.String("正文引文（≤300 字）")),
	)
	relationshipSchema := schema.Object(
		schema.Property("character_a", schema.String("角色 A")).Required(),
		schema.Property("character_b", schema.String("角色 B")).Required(),
		schema.Property("relation", schema.String("当前关系描述")).Required(),
	)
	stateChangeSchema := schema.Object(
		schema.Property("entity", schema.String("角色名或实体名")).Required(),
		schema.Property("field", schema.String("变化属性")).Required(),
		schema.Property("old_value", schema.String("变化前的值")),
		schema.Property("new_value", schema.String("变化后的值")).Required(),
		schema.Property("reason", schema.String("变化原因")),
	)
	feedbackSchema := schema.Object(
		schema.Property("deviation", schema.String("偏离大纲的描述")).Required(),
		schema.Property("suggestion", schema.String("对后续大纲的调整建议")).Required(),
	)
	feedbackSchema["description"] = "对后续大纲的建议对象；必须直接传 JSON object，不要传字符串化 JSON"
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
		schema.Property("final_title", schema.String("正文完成后确定的读者章节标题（可选，≤120 个 Unicode 字符；省略或空字符串保持已有最终标题）")),
		schema.Property("summary", schema.String("本章内容摘要（200字以内）")).Required(),
		schema.Property("characters", schema.Array("本章出场角色名", schema.String(""))).Required(),
		schema.Property("key_events", schema.Array("本章关键事件", schema.String(""))).Required(),
		schema.Property("timeline_events", schema.Array("本章时间线事件", timelineSchema)),
		schema.Property("foreshadow_updates", schema.Array("伏笔操作", foreshadowSchema)),
		schema.Property("relationship_changes", schema.Array("关系变化", relationshipSchema)),
		schema.Property("state_changes", schema.Array("角色/实体状态变化", stateChangeSchema)),
		schema.Property("character_state_updates", schema.Array("角色/实体受控状态：value 非空为 upsert 当前值；value 空字符串且带 reason = 从当前账本移除该 field。与 state_changes 勿对同一 (entity,field) 双写", characterStateSchema)),
		schema.Property("cast_intros", schema.Array("本章首次引入且后续可能再出现的次要角色简介（不含主角及 characters.json 已有角色）", schema.Object(
			schema.Property("name", schema.String("角色名")).Required(),
			schema.Property("brief_role", schema.String("一句话定位（如：客栈老板/赌坊打手）")).Required(),
		))),
		schema.Property("hook_type", schema.Enum("章末钩子类型", "crisis", "mystery", "desire", "emotion", "choice")),
		schema.Property("dominant_strand", schema.Enum("本章主导叙事线", "quest", "fire", "constellation")),
		schema.Property("world_state_mode", schema.Enum(
			"重写/打磨已完成章节的提交必填：preserve=纯文风/节奏/色气重写（不改变剧情事实），不应用世界状态变更；replace=剧情变化重写，需世界状态重放支持（当前可能被拒绝，被拒绝时不得静默改剧情）。新章提交可省略。",
			worldStateModePreserve, worldStateModeReplace)),
		schema.Property("feedback", feedbackSchema),
	)
}

func (t *CommitChapterTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	args = normalizeIntegerStringFields(args, "chapter")
	var a struct {
		Chapter               int                           `json:"chapter"`
		FinalTitle            string                        `json:"final_title"`
		Summary               string                        `json:"summary"`
		Characters            []string                      `json:"characters"`
		KeyEvents             []string                      `json:"key_events"`
		TimelineEvents        []domain.TimelineEvent        `json:"timeline_events"`
		ForeshadowUpdates     []domain.ForeshadowUpdate     `json:"foreshadow_updates"`
		RelationshipChanges   []domain.RelationshipEntry    `json:"relationship_changes"`
		StateChanges          []domain.StateChange          `json:"state_changes"`
		CharacterStateUpdates []domain.CharacterStateUpdate `json:"character_state_updates"`
		CastIntros            []domain.CastIntro            `json:"cast_intros"`
		HookType              string                        `json:"hook_type"`
		DominantStrand        string                        `json:"dominant_strand"`
		WorldStateMode        string                        `json:"world_state_mode"`
		Feedback              *domain.OutlineFeedback       `json:"feedback"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}
	finalTitle, err := normalizeFinalTitle(a.FinalTitle)
	if err != nil {
		return nil, err
	}
	storedFinalTitle, err := t.store.ChapterTitles.Load(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter final title: %w: %w", errs.ErrStoreRead, err)
	}
	effectiveFinalTitle := storedFinalTitle
	if finalTitle != "" {
		effectiveFinalTitle = finalTitle
	}

	// 章节流水线强制状态机（Enabled 时）：needs_commit 才允许提交；被拒时
	// 不创建 pending commit、不写 final。现有 commit gates 保留（纵深防御）。
	if err := RequireChapterAction(t.store, a.Chapter, ChapterActionCommit, t.fsmConfig); err != nil {
		// 崩溃恢复窄路径：MarkChapterComplete 之后、ClearPendingCommit 之前崩溃时，
		// 本章已完成且残留匹配 PendingCommit——FSM 判 complete 拒绝一切动作，
		// 重试永远到不了下方 171-178 的既有恢复代码。此处先恢复（追加 commit
		// checkpoint + 清除残留信号），再返回与既有 skip 路径一致的事实。
		// 不写任何新正文（原提交已成功），恢复即终点；未命中该条件时保持原拒绝。
		if t.store.Progress.IsChapterCompleted(a.Chapter) {
			if pending, _ := t.store.Signals.LoadPendingCommit(); pending != nil && pending.Chapter == a.Chapter {
				if cErr := t.appendCommitCheckpoint(a.Chapter); cErr != nil {
					return nil, fmt.Errorf("checkpoint commit: %w: %w", errs.ErrStoreWrite, cErr)
				}
				if cErr := t.store.Signals.ClearPendingCommit(); cErr != nil {
					return nil, fmt.Errorf("clear pending commit: %w: %w", errs.ErrStoreWrite, cErr)
				}
				progress, _ := t.store.Progress.Load()
				return t.buildSkipResult(a.Chapter, progress, storedFinalTitle)
			}
		}
		return nil, fmt.Errorf("commit_chapter: %w", err)
	}

	// 批次 4 安全闸门：重写提交必须显式声明世界状态处理模式。识别
	// completed + rewrite queue（重写路径入口）后立即校验——早于 literary gate
	// （命中会落盘 rule_violations）与残留 PendingCommit 清理（会追加 checkpoint
	// 并清除恢复信号），缺失 mode 的失败保证零副作用（无部分写入）。
	// 校验通过后 executeRewriteCommit 不再重复校验。
	if isCompletedAndInRewriteQueue(t.store, a.Chapter) {
		if err := validateRewriteWorldStateMode(a.Chapter, a.WorldStateMode); err != nil {
			return nil, err
		}
	}

	// critic 模式 commit 闸门：校验一致性、账本状态、摘要匹配
	if err := CheckCommitStyleGate(t.store, a.Chapter); err != nil {
		return nil, fmt.Errorf("commit_chapter: %w", err)
	}

	// 精修流水线 commit 闸门：fresh polish checkpoint + 模型一致性 + 时序（pipeline 启用时）
	if t.polishPipeline != nil {
		if err := CheckPolishPipelineGate(t.store, a.Chapter, t.polishPipeline.ExpectedModel); err != nil {
			return nil, fmt.Errorf("commit_chapter: %w", err)
		}
	}

	// 文学腔句式硬闸（commit 级打回）：只拦"真正提交新正文"的路径——
	// 新章提交与重写/打磨提交都算新正文；已完成章节的重复提交（skip 结果）跳过。
	// 硬闸在一切写操作之前执行，违例即中止，终稿/摘要/进度都不会被改动。
	if !t.store.Progress.IsChapterCompleted(a.Chapter) || isCompletedAndInRewriteQueue(t.store, a.Chapter) {
		if err := CheckLiteraryProseGate(t.store, a.Chapter); err != nil {
			return nil, fmt.Errorf("commit_chapter: %w", err)
		}
	}

	if t.store.Progress.IsChapterCompleted(a.Chapter) {
		// 清理可能残留的 PendingCommit（崩溃发生在 ProgressMarked 之后、ClearPendingCommit 之前）
		if pending, _ := t.store.Signals.LoadPendingCommit(); pending != nil && pending.Chapter == a.Chapter {
			if err := t.appendCommitCheckpoint(a.Chapter); err != nil {
				return nil, fmt.Errorf("checkpoint commit: %w: %w", errs.ErrStoreWrite, err)
			}
			_ = t.store.Signals.ClearPendingCommit()
		}
		// 打磨/重写路径：章节虽已完成，但仍在 pending_rewrites 中，允许覆盖并 drain 队列
		// （world_state_mode 已在 Execute 入口校验，见上方批次 4 安全闸门）。
		progress, _ := t.store.Progress.Load()
		inRewriteQueue := progress != nil && slices.Contains(progress.PendingRewrites, a.Chapter)
		if inRewriteQueue {
			return t.executeRewriteCommit(a.Chapter, a.Summary, a.Characters, a.KeyEvents,
				finalTitle, storedFinalTitle, a.HookType, a.DominantStrand, progress)
		}
		return t.buildSkipResult(a.Chapter, progress, storedFinalTitle)
	}
	existingPending, err := t.store.Signals.LoadPendingCommit()
	if err != nil {
		return nil, fmt.Errorf("load pending commit: %w: %w", errs.ErrStoreRead, err)
	}
	if existingPending != nil && existingPending.Chapter != a.Chapter {
		return nil, fmt.Errorf("存在未恢复的章节提交：第 %d 章（阶段 %s），请先恢复或重新提交该章: %w", existingPending.Chapter, existingPending.Stage, errs.ErrToolConflict)
	}
	if err := t.store.Progress.ValidateChapterWork(a.Chapter); err != nil {
		// 队列冲突保持原样（已带 ErrToolConflict 分类）；其他 IO 错误归 Precondition。
		if errors.Is(err, errs.ErrToolConflict) {
			return nil, err
		}
		return nil, fmt.Errorf("章节当前不允许提交: %w: %w", errs.ErrToolPrecondition, err)
	}

	// 分层模式越界拦截：必须先于任何写操作，否则越界 commit 会把章节文件、摘要、
	// Progress 都改坏。boundary 复用给下方第 6b 步算弧/卷信号。
	var boundary *store.ArcBoundary
	if progress, perr := t.store.Progress.Load(); perr == nil && progress != nil && progress.Layered {
		b, bErr := t.store.Outline.CheckArcBoundary(a.Chapter)
		if bErr != nil {
			return nil, fmt.Errorf("弧边界检测失败 chapter=%d: %w: %w", a.Chapter, errs.ErrStoreRead, bErr)
		}
		if b == nil {
			return nil, fmt.Errorf(
				"第 %d 章不在分层大纲范围内：写作必须先 expand_arc 扩展弧或 append_volume 追加卷；若全书已完结请调 save_foundation type=complete_book: %w",
				a.Chapter, errs.ErrToolPrecondition)
		}
		boundary = b
	}

	// 1. 加载章节正文
	content, wordCount, err := t.store.Drafts.LoadChapterContent(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}

	// 1b. preflight（批次 4 新增能力）：foreshadow/character_state 参数语义校验 +
	// 双写冲突检测。置于一切写入（pending/终稿/摘要/账本）之前：任一失败整体
	// 拒绝，所有文件零变化。只读（草稿正文 + 伏笔账本），不产生任何写入。
	if err := t.preflightCommitArgs(a.Chapter, content, a.ForeshadowUpdates, a.StateChanges, a.CharacterStateUpdates, a.Feedback); err != nil {
		return nil, err
	}

	now := time.Now().Format(time.RFC3339)
	pending := domain.PendingCommit{
		Chapter:        a.Chapter,
		Stage:          domain.CommitStageStarted,
		Summary:        a.Summary,
		HookType:       a.HookType,
		DominantStrand: a.DominantStrand,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("save pending commit: %w: %w", errs.ErrStoreWrite, err)
	}

	// 2. 保存终稿
	if err := t.store.Drafts.SaveFinalChapter(a.Chapter, content); err != nil {
		return nil, fmt.Errorf("save final chapter: %w: %w", errs.ErrStoreWrite, err)
	}
	if finalTitle != "" {
		if err := t.store.ChapterTitles.Save(a.Chapter, finalTitle); err != nil {
			return nil, fmt.Errorf("save chapter final title: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 3. 保存摘要
	summary := domain.ChapterSummary{
		Chapter:    a.Chapter,
		Summary:    a.Summary,
		Characters: a.Characters,
		KeyEvents:  a.KeyEvents,
	}
	if err := t.store.Summaries.SaveSummary(summary); err != nil {
		return nil, fmt.Errorf("save summary: %w: %w", errs.ErrStoreWrite, err)
	}

	// 4. 更新状态增量
	if len(a.TimelineEvents) > 0 {
		for i := range a.TimelineEvents {
			a.TimelineEvents[i].Chapter = a.Chapter
		}
		if err := t.store.World.AppendTimelineEvents(a.TimelineEvents); err != nil {
			return nil, fmt.Errorf("append timeline: %w: %w", errs.ErrStoreWrite, err)
		}
	}
	if len(a.ForeshadowUpdates) > 0 {
		if err := t.store.World.UpdateForeshadow(a.Chapter, a.ForeshadowUpdates); err != nil {
			return nil, fmt.Errorf("update foreshadow: %w: %w", errs.ErrStoreWrite, err)
		}
	}
	if len(a.CharacterStateUpdates) > 0 {
		if err := t.store.World.UpsertCharacterState(a.Chapter, a.CharacterStateUpdates); err != nil {
			return nil, fmt.Errorf("upsert character state: %w: %w", errs.ErrStoreWrite, err)
		}
	}
	if len(a.RelationshipChanges) > 0 {
		for i := range a.RelationshipChanges {
			a.RelationshipChanges[i].Chapter = a.Chapter
		}
		if err := t.store.World.UpdateRelationships(a.RelationshipChanges); err != nil {
			return nil, fmt.Errorf("update relationships: %w: %w", errs.ErrStoreWrite, err)
		}
	}
	if len(a.StateChanges) > 0 {
		for i := range a.StateChanges {
			a.StateChanges[i].Chapter = a.Chapter
		}
		if err := t.store.World.AppendStateChanges(a.StateChanges); err != nil {
			return nil, fmt.Errorf("append state changes: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 4b. 累加配角名册：本章出场的非核心角色进 cast_ledger，供 novel_context 召回。
	// 失败时只 warn 不阻断 commit——名册是次要数据，可通过下一章 commit 自愈。
	if len(a.Characters) > 0 {
		coreNames := loadCoreCharacterNameSet(t.store)
		if err := t.store.Cast.MergeAppearances(a.Chapter, a.Characters, a.CastIntros, coreNames); err != nil {
			slog.Warn("配角名册累加失败，跳过", "module", "commit", "chapter", a.Chapter, "err", err)
		}
	}

	pending.Stage = domain.CommitStageStateApplied
	// 不做 mutation payload（已决策简化，见 docs/four-layer-state-design.md §已接受偏差 B3）：
	// 恢复完全依赖 store 幂等（UpsertCharacterState 同值跳过派生、stateChangeKey 去重）
	// 与调整后的写入顺序（先流水后状态，见 character_state.go），无需标记字段。
	pending.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("update pending commit stage: %w: %w", errs.ErrStoreWrite, err)
	}

	// 5. 更新进度
	if err := t.store.Progress.MarkChapterComplete(a.Chapter, wordCount, a.HookType, a.DominantStrand); err != nil {
		return nil, fmt.Errorf("mark chapter complete: %w: %w", errs.ErrStoreWrite, err)
	}

	// 6. 判断是否需要审阅
	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	completedCount := 0
	if progress != nil {
		completedCount = len(progress.CompletedChapters)
	}

	// 6b. 长篇模式弧/卷信号：boundary 已在入口前置校验，Layered 时保证非 nil
	var arcEnd, volumeEnd, needsExpansion, needsNewVolume bool
	var vol, arc, nextVol, nextArc int
	if progress != nil && progress.Layered && boundary != nil {
		arcEnd = boundary.IsArcEnd
		volumeEnd = boundary.IsVolumeEnd
		vol = boundary.Volume
		arc = boundary.Arc
		needsExpansion = boundary.NeedsExpansion
		needsNewVolume = boundary.NeedsNewVolume
		nextVol = boundary.NextVolume
		nextArc = boundary.NextArc
		_ = t.store.Progress.UpdateVolumeArc(vol, arc)
	}

	var reviewRequired bool
	var reviewReason string
	if progress != nil && progress.Layered {
		reviewRequired, reviewReason = domain.ShouldArcReview(arcEnd, volumeEnd, vol, arc)
	} else {
		reviewRequired, reviewReason = domain.ShouldReview(completedCount)
	}

	// 7. 构造结构化信号
	result := domain.CommitResult{
		Chapter:        a.Chapter,
		Committed:      true,
		WordCount:      wordCount,
		NextChapter:    a.Chapter + 1,
		ReviewRequired: reviewRequired,
		ReviewReason:   reviewReason,
		HookType:       a.HookType,
		DominantStrand: a.DominantStrand,
		Feedback:       a.Feedback,
		// (feedback 同时持久化到反馈池,见下方 persistFeedback——返回值只是镜像,
		// architect 经 novel_context 消费的是 store 事实)
		ArcEnd:         arcEnd,
		VolumeEnd:      volumeEnd,
		Volume:         vol,
		Arc:            arc,
		NeedsExpansion: needsExpansion,
		NeedsNewVolume: needsNewVolume,
		NextVolume:     nextVol,
		NextArc:        nextArc,
	}

	// 8. 完成态判定：非分层写完最后一章 / 分层最终卷最后一章 → MarkComplete
	if t.applyCompletion(&result, progress) {
		result.BookComplete = true
	}
	if p, _ := t.store.Progress.Load(); p != nil {
		result.Flow = string(p.Flow)
	}

	// 8.5 反馈池:writer 对大纲的反馈落盘,architect 下次结构操作经 novel_context
	// 消费(仅返回值会随 run 结束丢失)。附属事实 best-effort,不阻断提交。
	// 仅分层书持久化:非分层书没有结构操作,落盘只会制造永远无消费者的垃圾事实
	// (返回值镜像仍保留,诊断可见)。
	layered := progress != nil && progress.Layered
	if layered && a.Feedback != nil && (strings.TrimSpace(a.Feedback.Deviation) != "" || strings.TrimSpace(a.Feedback.Suggestion) != "") {
		if err := t.store.Outline.AppendOutlineFeedback(store.ChapterFeedback{
			Chapter: a.Chapter, Deviation: a.Feedback.Deviation, Suggestion: a.Feedback.Suggestion,
		}); err != nil {
			slog.Warn("大纲反馈落盘失败", "module", "tools", "chapter", a.Chapter, "err", err)
		}
	}

	pending.Stage = domain.CommitStageProgressMarked
	pending.Result = &result
	pending.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := t.store.Signals.SavePendingCommit(pending); err != nil {
		return nil, fmt.Errorf("update pending commit result: %w: %w", errs.ErrStoreWrite, err)
	}

	// 9. 追加 checkpoint。必须先于清除 pending_commit，确保重启后可见的
	// pending_commit 总能驱动重跑补齐缺失 checkpoint。
	if err := t.appendCommitCheckpoint(a.Chapter); err != nil {
		return nil, fmt.Errorf("checkpoint commit: %w: %w", errs.ErrStoreWrite, err)
	}

	// 10. 清除进度中间状态
	if err := t.store.Progress.ClearInProgress(); err != nil {
		return nil, fmt.Errorf("clear in-progress: %w: %w", errs.ErrStoreWrite, err)
	}
	if err := t.store.Signals.ClearPendingCommit(); err != nil {
		return nil, fmt.Errorf("clear pending commit: %w: %w", errs.ErrStoreWrite, err)
	}

	// 11. 机械规则检查（仅返事实，不阻断）
	violations := t.checkRules(content)
	// 持久化违规事实:editor 评审经 novel_context 消费(返回值只是镜像——
	// writer 在 commit 后立即硬停,返回值无人可读)。best-effort。
	if err := t.store.World.SaveRuleViolations(a.Chapter, violations); err != nil {
		slog.Warn("机械违规落盘失败", "module", "tools", "chapter", a.Chapter, "err", err)
	}
	return json.Marshal(commitOutput{
		CommitResult:   result,
		FinalTitle:     effectiveFinalTitle,
		RuleViolations: violations,
	})
}

func (t *CommitChapterTool) appendCommitCheckpoint(chapter int) error {
	_, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(chapter), "commit",
		fmt.Sprintf("chapters/%02d.md", chapter),
	)
	return err
}

// preflightCommitArgs 对提交参数做写入前的整体校验（一切写操作之前执行，只读）：
//   - foreshadow_updates：逐条 domain.ForeshadowUpdate.Validate（含 evidence 必须
//     逐字出现在本章草稿正文）；advance/resolve/retire 的 ID 必须已存在于伏笔账本；
//     每条按当前状态经 domain.ForeshadowTransitionAllowed 校验转换合法性（§4.1 表，
//     与 store.UpdateForeshadow 共用同一纯函数——非法转换在写入前拒绝，零副作用）；
//     同一 ID 在同一提交内不得出现冲突 action（如 advance 与 resolve 并存）。
//   - character_state_updates：逐条 domain.ValidateCharacterStateUpdate；批内
//     (entity,field) 重复且 value 不同 → 拒绝；单实体字段数容量预检（现有条数 +
//     批内新增 ≤ MaxFieldsPerEntity，与 store.UpsertCharacterState 判定一致）。
//   - 双写冲突：同一 (entity,field) 不得同时出现在 character_state_updates 与
//     state_changes——受控状态走 character_state_updates 单一通道，避免账本分叉。
//   - feedback.suggestion：以 [world_rule] 前缀开头的规则提案必须携带规则描述
//     （去除前缀后非空且 ≥10 字符），防止空壳提案混入世界规则修订流程。
//
// 任一失败 → ErrToolArgs 错误整体拒绝；本函数不产生任何写入。
func (t *CommitChapterTool) preflightCommitArgs(
	chapter int,
	draftText string,
	foreshadow []domain.ForeshadowUpdate,
	stateChanges []domain.StateChange,
	charStates []domain.CharacterStateUpdate,
	feedback *domain.OutlineFeedback,
) error {
	evidenceInDraft := func(evidence string) bool {
		return draftText != "" && strings.Contains(draftText, evidence)
	}
	// 加载完整伏笔账本：记录各 ID 的当前状态，供转换合法性校验。
	// 读取失败时整体拒绝（fail closed）——宁可拒绝提交也不在状态未知时冒险写入。
	ledger, err := t.store.World.LoadForeshadowLedger()
	if err != nil {
		return fmt.Errorf("preflight: 加载伏笔账本失败: %w: %w", errs.ErrStoreRead, err)
	}
	statusByID := make(map[string]string, len(ledger))
	for _, e := range ledger {
		status := e.Status
		if status == "" {
			// 遗留空状态与 store 的 transitionStatus 归一化一致（视为 planted），避免 preflight 与 store 口径分裂。
			status = "planted"
		}
		statusByID[e.ID] = status
	}
	actionByID := make(map[string]string, len(foreshadow))
	for i, u := range foreshadow {
		if u.Action != "plant" {
			status, known := statusByID[u.ID]
			if !known {
				return fmt.Errorf("foreshadow_updates[%d]: 伏笔 %q 不存在（action=%s 只能作用于已埋设的伏笔）: %w",
					i, u.ID, u.Action, errs.ErrToolArgs)
			}
			// 非法状态转换（resolved 上 advance / retired 上 resolve 等）在写入前拒绝
			if err := domain.ForeshadowTransitionAllowed(status, u); err != nil {
				return fmt.Errorf("foreshadow_updates[%d]: %v: %w", i, err, errs.ErrToolArgs)
			}
		} else if status, known := statusByID[u.ID]; known {
			// plant 已存在的 ID：仅 planted（或遗留空状态）可幂等补空；
			// advanced/resolved/retired 上重复 plant 拒绝。
			if err := domain.ForeshadowTransitionAllowed(status, u); err != nil {
				return fmt.Errorf("foreshadow_updates[%d]: %v: %w", i, err, errs.ErrToolArgs)
			}
		}
		if err := u.Validate(chapter, evidenceInDraft); err != nil {
			return fmt.Errorf("foreshadow_updates[%d]: %v: %w", i, err, errs.ErrToolArgs)
		}
		if prev, ok := actionByID[u.ID]; ok && prev != u.Action {
			return fmt.Errorf("foreshadow_updates: 伏笔 %q 存在冲突操作 %q 与 %q（同一提交内一个 ID 只能有一种操作）: %w",
				u.ID, prev, u.Action, errs.ErrToolArgs)
		}
		actionByID[u.ID] = u.Action
	}
	// 单实体字段数容量预检：现有条数 + 批内新增 ≤ MaxFieldsPerEntity
	// （与 store.UpsertCharacterState 的判定一致；读取失败 fail closed）。
	stateEntries, err := t.store.World.LoadCharacterState()
	if err != nil {
		return fmt.Errorf("preflight: 加载角色状态失败: %w: %w", errs.ErrStoreRead, err)
	}
	fieldCount := make(map[string]int, len(stateEntries))
	existingKeys := make(map[string]struct{}, len(stateEntries))
	for _, e := range stateEntries {
		fieldCount[e.Entity]++
		existingKeys[e.Entity+"\x00"+e.Field] = struct{}{}
	}
	seenCharKey := make(map[string]string, len(charStates))
	for i, u := range charStates {
		if err := domain.ValidateCharacterStateUpdate(u); err != nil {
			return fmt.Errorf("character_state_updates[%d]: %v: %w", i, err, errs.ErrToolArgs)
		}
		key := u.Entity + "\x00" + u.Field
		if prev, dup := seenCharKey[key]; dup {
			if prev != u.Value {
				return fmt.Errorf("character_state_updates: %s.%s 在同一提交内重复且 value 不同（%q 与 %q），请合并为一条: %w",
					u.Entity, u.Field, prev, u.Value, errs.ErrToolArgs)
			}
			continue
		}
		seenCharKey[key] = u.Value
	}
	for _, u := range charStates {
		if !u.Clears() {
			continue
		}
		key := u.Entity + "\x00" + u.Field
		if _, exists := existingKeys[key]; exists {
			fieldCount[u.Entity]--
			delete(existingKeys, key)
		}
	}
	newStatus := 0
	for i, u := range charStates {
		if u.Clears() {
			continue
		}
		key := u.Entity + "\x00" + u.Field
		if _, exists := existingKeys[key]; exists {
			continue
		}
		if fieldCount[u.Entity] >= domain.MaxFieldsPerEntity {
			return fmt.Errorf("character_state_updates[%d]: %s 字段数已达上限 %d，拒绝新增 %s: %w",
				i, u.Entity, domain.MaxFieldsPerEntity, u.Field, errs.ErrToolArgs)
		}
		fieldCount[u.Entity]++
		existingKeys[key] = struct{}{}
		if strings.HasPrefix(u.Field, "status.") {
			newStatus++
			if newStatus > domain.MaxNewStatusFieldsPerCommit {
				return fmt.Errorf("character_state_updates[%d]: 单次提交新增 status.* 不得超过 %d 条（status 只报仍约束下一章的状态，章节进度请用 timeline_events）: %w",
					i, domain.MaxNewStatusFieldsPerCommit, errs.ErrToolArgs)
			}
		}
	}
	// 双写冲突：(entity,field) 同时出现在 character_state_updates 与 state_changes
	if len(charStates) > 0 && len(stateChanges) > 0 {
		keys := make(map[string]bool, len(charStates))
		for _, u := range charStates {
			keys[u.Entity+"\x00"+u.Field] = true
		}
		for _, sc := range stateChanges {
			if keys[sc.Entity+"\x00"+sc.Field] {
				return fmt.Errorf("双写冲突：%s.%s 同时出现在 character_state_updates 与 state_changes，请改用 character_state_updates 一个通道: %w",
					sc.Entity, sc.Field, errs.ErrToolArgs)
			}
		}
	}
	// [world_rule] 提案格式校验：feedback.suggestion 以 [world_rule] 前缀开头时，
	// 去除前缀后必须包含非空且 ≥10 字符的规则描述（防止空壳提案进入规则修订流程）。
	if feedback != nil {
		suggestion := strings.TrimSpace(feedback.Suggestion)
		if strings.HasPrefix(suggestion, "[world_rule]") {
			desc := strings.TrimSpace(strings.TrimPrefix(suggestion, "[world_rule]"))
			if desc == "" || utf8.RuneCountInString(desc) < 10 {
				return fmt.Errorf("feedback.suggestion: [world_rule] 提案须包含规则描述（去除前缀后至少 10 字符）: %w", errs.ErrToolArgs)
			}
		}
	}
	return nil
}

// checkRules 对章节正文做机械检查：内置产品底线 Lint（机制残留，始终执行）
// + 用户规则 Check（读本书快照的 structured；快照缺失退到内置默认，保证机械底线始终在）
// + 文学腔句式硬闸事实（硬闸已在 Execute 前置阶段拦截；此处同样跑一遍，
// 把命中句随完整违例集落盘，供 editor 经 novel_context 审计）。
func (t *CommitChapterTool) checkRules(text string) []rules.Violation {
	violations := rules.Lint(text)
	structured := rules.SystemDefaults().Structured
	if snap, err := t.store.UserRules.Load(); err == nil && snap != nil {
		structured = snap.Structured
	}
	return append(violations, rules.CheckLiteraryGate(text, utf8.RuneCountInString(text), structured)...)
}

// 重写提交的世界状态处理模式（批次 4 安全闸门）。
// 不能靠"数组为空"猜测语义：纯文风重写空数组=保持，剧情重写空数组=清除，语义相反，
// 因此重写提交必须显式声明。缺失或非法值一律拒绝（Precondition），不静默提交。
const (
	worldStateModePreserve = "preserve"
	worldStateModeReplace  = "replace"
)

// validateRewriteWorldStateMode 强制重写提交显式声明世界状态处理模式：
//   - 缺失（""）或非法值 → ErrToolPrecondition 错误（不写终稿、不 drain PendingRewrites）。
//   - "preserve" → 放行：executeRewriteCommit 现有行为，5 组世界状态变更
//     （TimelineEvents/ForeshadowUpdates/RelationshipChanges/StateChanges/
//     CharacterStateUpdates）一律不应用。
//   - "replace" → 一律显式拒绝（批次 4 范围内无可安全处理的章节）：
//     按章替换时间线/状态并重放后续章节的世界状态重放能力（world-delta/baseline，
//     批次 5）尚未就绪；Relationships/Foreshadow 是聚合快照（无发生章、删除后无法恢复），
//     无可重放历史的章节 replace 无法安全执行。宁肯拒绝也不静默错误应用。
//
// 错误消息始终包含 chapter、world_state_mode 值、原因与恢复建议。
func validateRewriteWorldStateMode(chapter int, mode string) error {
	switch mode {
	case "":
		return fmt.Errorf(
			"第 %d 章重写提交缺失 world_state_mode：纯文风重写必须传 %q，剧情变化重写必须传 %q。"+
				"原因：不显式声明会静默丢弃世界状态变更（timeline/foreshadow/relationships/state/character_state 与正文失配且无报错）。"+
				"恢复建议：补充 world_state_mode 后重试: %w",
			chapter, worldStateModePreserve, worldStateModeReplace, errs.ErrToolPrecondition)
	case worldStateModePreserve:
		return nil
	case worldStateModeReplace:
		return fmt.Errorf(
			"第 %d 章重写提交 world_state_mode=%q 被拒绝：剧情级重写需要世界状态重放能力（按章替换时间线/状态并重放后续章节），"+
				"该能力尚未就绪（世界账本重放未实现；Relationships/Foreshadow 为聚合快照、无可重放历史，删除后无法恢复），"+
				"直接替换会让世界账本与正文永久失配。恢复建议：改用 world_state_mode=%q 仅做纯文风重写（不改变剧情事实），"+
				"或停下请人工处理世界账本后再提交；不要用空数组蒙混（preserve 下空数组=保持、replace 下空数组=清除，语义相反）: %w",
			chapter, worldStateModeReplace, worldStateModePreserve, errs.ErrToolPrecondition)
	default:
		return fmt.Errorf(
			"第 %d 章重写提交 world_state_mode=%q 非法：仅支持 %q 或 %q。恢复建议：修正 world_state_mode 后重试: %w",
			chapter, mode, worldStateModePreserve, worldStateModeReplace, errs.ErrToolPrecondition)
	}
}

// executeRewriteCommit 处理打磨/重写章节的提交：覆盖终稿与摘要、更新字数、drain 队列。
// 调用方（Execute）已通过 validateRewriteWorldStateMode 强制 world_state_mode：
//   - preserve：跳过所有世界状态追加（timeline / foreshadow / relationship / state_changes /
//     character_state_updates），这些已在章节原始提交时应用——纯文风重写不改变剧情事实，
//     账本无需变更。
//   - replace：被安全闸门显式拒绝，不会走到本函数（世界状态重放能力未就绪）。
//
// 已知风险（登记，批次 5 范围）：本路径也不更新 CastIntros/配角名册
// （主路径 4b 步 MergeAppearances 会累加 cast_ledger）——重写新增的次要角色
// 不会进入配角账本；当前仅记录风险，不做实现。
func (t *CommitChapterTool) executeRewriteCommit(
	chapter int,
	summary string,
	characters, keyEvents []string,
	finalTitle, storedFinalTitle string,
	hookType, dominantStrand string,
	progress *domain.Progress,
) (json.RawMessage, error) {
	// 1. 加载打磨后的正文
	content, wordCount, err := t.store.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return nil, fmt.Errorf("rewrite: load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", chapter, errs.ErrToolPrecondition)
	}

	// 2. 硬校验：drafts 与现终稿完全相同 → 判定为未真正打磨/重写（writer 跳过了 draft_chapter）
	// 拒绝 commit，强制 writer 先调 draft_chapter(mode=write) 写入新版本。
	existingFinal, _ := t.store.Drafts.LoadChapterText(chapter)
	if existingFinal != "" && existingFinal == content {
		mode := "重写"
		if progress != nil && progress.Flow == domain.FlowPolishing {
			mode = "打磨"
		}
		return nil, fmt.Errorf("第 %d 章 drafts 与 chapters 内容完全相同，未检测到%s改动。请先调 draft_chapter(mode=write, chapter=%d) 写入%s后的新正文，再 commit_chapter: %w",
			chapter, mode, chapter, mode, errs.ErrToolPrecondition)
	}

	// 3. 覆盖终稿
	if err := t.store.Drafts.SaveFinalChapter(chapter, content); err != nil {
		return nil, fmt.Errorf("rewrite: save final chapter: %w: %w", errs.ErrStoreWrite, err)
	}
	if finalTitle != "" {
		if err := t.store.ChapterTitles.Save(chapter, finalTitle); err != nil {
			return nil, fmt.Errorf("rewrite: save chapter final title: %w: %w", errs.ErrStoreWrite, err)
		}
		storedFinalTitle = finalTitle
	}

	// 3. 覆盖摘要
	if err := t.store.Summaries.SaveSummary(domain.ChapterSummary{
		Chapter:    chapter,
		Summary:    summary,
		Characters: characters,
		KeyEvents:  keyEvents,
	}); err != nil {
		return nil, fmt.Errorf("rewrite: save summary: %w: %w", errs.ErrStoreWrite, err)
	}

	// 4. 更新字数（MarkChapterComplete 对已完成章节是幂等的：replaces word count, slice.Contains 防止重复入队）
	if err := t.store.Progress.MarkChapterComplete(chapter, wordCount, hookType, dominantStrand); err != nil {
		return nil, fmt.Errorf("rewrite: update word count: %w: %w", errs.ErrStoreWrite, err)
	}

	// 5. Drain 待处理队列；队列空时 CompleteRewrite 会自动把 flow 切回 writing
	if err := t.store.Progress.CompleteRewrite(chapter); err != nil {
		return nil, fmt.Errorf("rewrite: complete rewrite: %w: %w", errs.ErrStoreWrite, err)
	}

	// 6. Checkpoint
	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(chapter), "commit",
		fmt.Sprintf("chapters/%02d.md", chapter),
	); err != nil {
		return nil, fmt.Errorf("rewrite: checkpoint commit: %w: %w", errs.ErrStoreWrite, err)
	}

	// 7. 读取 drain 后的 Progress 快照，作为事实返回
	mode := "rewrite"
	if progress.Flow == domain.FlowPolishing {
		mode = "polish"
	}
	latest, _ := t.store.Progress.Load()
	remaining := []int{}
	nextChapter := chapter + 1
	flow := string(domain.FlowWriting)
	if latest != nil {
		remaining = append(remaining, latest.PendingRewrites...)
		nextChapter = latest.NextChapter()
		flow = string(latest.Flow)
	}
	drained := len(remaining) == 0

	// 队列清空后再判完结：返工提交不经过主路径 applyCompletion，完结只能在此触发。
	//   - 分层 + 正向写作：layeredComplete 总判定（收官卷结构写完 / 未宣告走质量级）。
	//   - 分层 + reopen 返工（ReopenedFromComplete）：返工只改已有章、不增减结构，按结构完整
	//     即重新完结——若因返工扰动了某条线索就卡在 writing，终卷末会落到越界续写死循环。
	//   - 非分层：写满 TotalChapters 即完结（返工不增减章数，原本就满）。
	bookComplete := false
	if drained && latest != nil {
		reComplete := false
		switch {
		case latest.Layered && latest.ReopenedFromComplete:
			reComplete = layeredStructurallyComplete(t.store, latest)
		case latest.Layered:
			reComplete = layeredComplete(t.store, latest)
		default:
			reComplete = latest.TotalChapters > 0 && len(latest.CompletedChapters) >= latest.TotalChapters
		}
		if reComplete {
			if cerr := t.store.Progress.MarkComplete(); cerr == nil {
				bookComplete = true
				if p, _ := t.store.Progress.Load(); p != nil {
					flow = string(p.Flow)
				}
			}
		}
	}

	// 同主路径：rewrite/polish 也做机械检查并持久化(重写后落新记录,旧违规视为已清)
	violations := t.checkRules(content)
	if err := t.store.World.SaveRuleViolations(chapter, violations); err != nil {
		slog.Warn("机械违规落盘失败", "module", "tools", "chapter", chapter, "err", err)
	}
	return json.Marshal(map[string]any{
		"chapter":         chapter,
		"final_title":     storedFinalTitle,
		"rewritten":       true,
		"mode":            mode,
		"word_count":      wordCount,
		"remaining_queue": remaining,
		"queue_drained":   drained,
		"next_chapter":    nextChapter,
		"flow":            flow,
		"book_complete":   bookComplete,
		"rule_violations": violations,
	})
}

// buildSkipResult 为"章节已完成的重复提交"构造与正常 commit 对齐的事实返回。
// 协调者据此做后续决策（writer/editor/architect 派发），而不会因为拿到 prose 提示而幻觉。
func (t *CommitChapterTool) buildSkipResult(chapter int, progress *domain.Progress, finalTitle string) (json.RawMessage, error) {
	_, wordCount, _ := t.store.Drafts.LoadChapterContent(chapter)

	result := domain.CommitResult{
		Chapter:     chapter,
		Committed:   true,
		WordCount:   wordCount,
		NextChapter: chapter + 1,
	}

	if progress != nil && progress.Layered {
		if boundary, _ := t.store.Outline.CheckArcBoundary(chapter); boundary != nil {
			result.ArcEnd = boundary.IsArcEnd
			result.VolumeEnd = boundary.IsVolumeEnd
			result.Volume = boundary.Volume
			result.Arc = boundary.Arc
			result.NeedsExpansion = boundary.NeedsExpansion
			result.NeedsNewVolume = boundary.NeedsNewVolume
			result.NextVolume = boundary.NextVolume
			result.NextArc = boundary.NextArc
		}
		result.ReviewRequired, result.ReviewReason = domain.ShouldArcReview(result.ArcEnd, result.VolumeEnd, result.Volume, result.Arc)
	} else if progress != nil {
		result.ReviewRequired, result.ReviewReason = domain.ShouldReview(len(progress.CompletedChapters))
	}

	if progress != nil {
		if progress.Phase == domain.PhaseComplete {
			result.BookComplete = true
		}
		result.Flow = string(progress.Flow)
	}

	return json.Marshal(commitOutput{CommitResult: result, FinalTitle: finalTitle})
}

// loadCoreCharacterNameSet 加载 characters.json 中已有的角色名集合（含别名）。
// 用作 cast_ledger 的"已知核心"过滤集——核心角色不进次要名册。
// 加载失败时返回 nil（merge 时所有 characters 都进 ledger，可接受）。
func loadCoreCharacterNameSet(s *store.Store) map[string]bool {
	chars, err := s.Characters.Load()
	if err != nil || len(chars) == 0 {
		return nil
	}
	set := make(map[string]bool, len(chars)*2)
	for _, c := range chars {
		if c.Name != "" {
			set[c.Name] = true
		}
		for _, alias := range c.Aliases {
			if alias != "" {
				set[alias] = true
			}
		}
	}
	return set
}

// applyCompletion 判断本次 commit 是否使全书完结，若是则 MarkComplete 并返回 true。
//   - 非分层：写完约定总章数即完结。
//   - 分层：架构师显式 save_foundation type=complete_book 是主路径；这里再加一道
//     确定性兜底（见 layeredComplete）——防止模型在终点既不 append_volume 也不
//     complete_book，导致"写手裸跑越界章节 → 越界守卫拦截 → 反复重试"的 livelock
//     （《凡骨》ch204..347 案例的根因）。
func (t *CommitChapterTool) applyCompletion(result *domain.CommitResult, progress *domain.Progress) bool {
	if progress == nil {
		return false
	}
	if progress.Layered {
		if layeredComplete(t.store, progress) {
			_ = t.store.Progress.MarkComplete()
			return true
		}
		return false
	}
	if progress.TotalChapters > 0 && result.NextChapter > progress.TotalChapters {
		_ = t.store.Progress.MarkComplete()
		return true
	}
	return false
}

// ── 分层完结判定（包级：commit_chapter 与 save_volume_summary 两个触发点共用）──
//
// 完结检查永远发生在"最后一块事实落地"的工具里：
//   - 未宣告收官：末章 commit（layeredBookComplete 质量级）
//   - 已宣告收官：正向主路径的最后一块拼图是卷末收尾三连（评审→弧摘要→卷摘要），
//     故触发点在 save_volume_summary；返工 drain 后三连已齐时由 commit 触发。

// layeredStructurallyComplete 判定分层长篇是否"结构上写完"：返工队列空 + 无骨架弧待展开
// + 所有已展开章节都已写。这是确定性的终态事实，不含伏笔/长线等语义判断——用作"防终态
// 死循环"的安全网（返工排空后据此重新完结）。
func layeredStructurallyComplete(st *store.Store, progress *domain.Progress) bool {
	// 1. 返工队列必须清空
	if len(progress.PendingRewrites) > 0 {
		return false
	}
	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil || len(volumes) == 0 {
		return false
	}
	// 2. 不能还有骨架弧待展开（计划内仍有内容要写）
	for i := range volumes {
		for j := range volumes[i].Arcs {
			if !volumes[i].Arcs[j].IsExpanded() {
				return false
			}
		}
	}
	// 3. 已展开章节必须全部写完
	expanded := len(domain.FlattenOutline(volumes))
	return expanded > 0 && len(progress.CompletedChapters) >= expanded
}

// finaleWrapped 收官卷的卷末收尾三连（弧评审/弧摘要/卷摘要）是否齐备。
// 收官完结不要求伏笔/长线归零，但必须等末弧过完编辑质量闸——结局是全书最要紧的部分，
// 完结不能抢在 editor 评审（可能入队返工）与摘要落盘之前。
func finaleWrapped(st *store.Store, progress *domain.Progress) bool {
	last := progress.LatestCompleted()
	if last <= 0 {
		return false
	}
	b, err := st.Outline.CheckArcBoundary(last)
	if err != nil || b == nil || !b.IsArcEnd {
		return false
	}
	return st.World.HasArcReview(last) &&
		st.Summaries.HasArcSummary(b.Volume, b.Arc) &&
		st.Summaries.HasVolumeSummary(b.Volume)
}

// layeredComplete 分层正向写作的完结总判定：
//   - 已宣告收官卷（layered_outline 最后一卷带 final）→ 结构写完 + 卷末收尾三连齐备
//     即完结，不再要求伏笔/长线归零。收官卷整卷以收线为目标（架构师规划时已把长线/
//     伏笔分配进各弧），个别遗漏属编辑质量问题，不该把全书卡在终态之外——否则
//     estimated_scale 高估的书永远无法合法完本（140 章 stop guard 熔断案例的根因侧）。
//   - 未宣告 → 质量级 layeredBookComplete，防模型既不收官也不完本时在大纲耗尽处
//     过早收尾。
func layeredComplete(st *store.Store, progress *domain.Progress) bool {
	if volumes, err := st.Outline.LoadLayeredOutline(); err == nil && domain.FinaleVolume(volumes) > 0 {
		return layeredStructurallyComplete(st, progress) && finaleWrapped(st, progress)
	}
	return layeredBookComplete(st, progress)
}

// layeredBookComplete 用客观事实判断分层长篇是否真正写完，对照 architect-long.md 完结判定
// 清单里可量化的几项 + 结构性事实。结构完整之上再要求伏笔归零、长线收束——任一不满足都
// 让位给架构师继续 expand_arc / append_volume，绝不抢在故事没写完时收尾。无 compass 时保守
// 判为未完结。这是未宣告收官卷时的"质量级"完结判定，比 layeredStructurallyComplete 更严。
func layeredBookComplete(st *store.Store, progress *domain.Progress) bool {
	if !layeredStructurallyComplete(st, progress) {
		return false
	}
	// 4. 活跃伏笔必须归零（承诺已兑现）
	if active, aerr := st.World.LoadActiveForeshadow(); aerr != nil || len(active) > 0 {
		return false
	}
	// 5. 指南针活跃长线必须收束（无 compass / 长线未清都交回架构师裁定）
	compass, cerr := st.Outline.LoadCompass()
	if cerr != nil || compass == nil || len(compass.Long.OpenThreads) > 0 {
		return false
	}
	return true
}
