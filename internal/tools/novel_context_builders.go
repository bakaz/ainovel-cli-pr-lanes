package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/stylestat"
)

// writer_style_card 防御性上限。Writer 的 200KB 预算中仅分配 ~2% 给此卡片；
// 上限基于 compass 设计约束（long prose 3-5 条 ≤50 字，dialogue 角色 ≤10 个）
// 并留出合理余量以支持当前弧补充规则。
const (
	writerStyleCardMaxProseRules    = 20   // 合并 long+current 后的最多 prose 规则数
	writerStyleCardMaxDialogueChars = 20   // 合并后的最多对话角色数
	writerStyleCardMaxRulesPerChar  = 10   // 单个角色的最多对话规则条数
	writerStyleCardMaxRuleLen       = 200  // 单条规则的最大 UTF-8 字符数（约 4× long 设计上限 50）
	writerStyleCardMaxTotalBytes    = 4096 // 整个卡片序列化后的最大字节数（≈2% 的 200KB Writer 预算）
)

type contextBuildState struct {
	chapter             int
	profile             domain.ContextProfile
	progress            *domain.Progress
	runMeta             *domain.RunMeta
	currentEntry        *domain.OutlineEntry
	chapterPlan         *domain.ChapterPlan
	storyThreads        []domain.RecallItem
	foreshadow          []domain.ForeshadowEntry
	relationships       []domain.RelationshipEntry
	allStateChanges     []domain.StateChange
	styleRules          *domain.WritingStyleRules
	styleRulesCompass   *domain.WritingStyleRulesCompass // Compass 双层格式
	styleAnchorsManual  *domain.StyleAnchorsV1           // 归一化后的手动锚点（meta/style_anchors.json）
	styleAnchorsAuto    []string                         // 自动提取的风格锚点（仅当无 manual 且无 style_rules 时 legacy 回退，或 include_auto=true）
	manStatus           store.ManualFileStatus           // 手动文件状态
	hasStyleRules       bool                             // 缓存是否存在 style_rules
	styleReviewStatus   domain.StyleReviewStatus         // 当前章节风格评审状态（空字符串=未开始）
	styleReviewFeedback map[string]any                   // 精炼的风格评审反馈（仅 revision_open/final_pending/exhausted 时存在）
}
type chapterContextEnvelope struct {
	Working    map[string]any
	Episodic   map[string]any
	References map[string]any
	Selected   map[string]any
}

type architectContextEnvelope struct {
	Planning   map[string]any
	Foundation map[string]any
	References map[string]any
}

func newChapterContextEnvelope() chapterContextEnvelope {
	return chapterContextEnvelope{
		Working:    make(map[string]any),
		Episodic:   make(map[string]any),
		References: make(map[string]any),
		Selected:   make(map[string]any),
	}
}

func newArchitectContextEnvelope() architectContextEnvelope {
	return architectContextEnvelope{
		Planning:   make(map[string]any),
		Foundation: make(map[string]any),
		References: make(map[string]any),
	}
}

func (e chapterContextEnvelope) apply(result map[string]any) {
	// 合并而非替换：Execute 的章节路径会先后 apply 两个信封（seed + buildChapterContext），
	// 整体赋值会让第二次 apply 丢弃 seed 的容器内容。这里只维护 canonical
	// 容器，不再为历史 prompt 生成同名顶层镜像。
	mergeEnvelopeSection(result, "working_memory", e.Working)
	mergeEnvelopeSection(result, "episodic_memory", e.Episodic)
	mergeEnvelopeSection(result, "reference_pack", e.References)
	if len(e.Selected) > 0 {
		mergeEnvelopeSection(result, "selected_memory", e.Selected)
	}
}

// mergeEnvelopeSection 把 section 合并进 result[key] 的既有容器；容器不存在时直接挂载。
func mergeEnvelopeSection(result map[string]any, key string, section map[string]any) {
	if existing, ok := result[key].(map[string]any); ok {
		for k, v := range section {
			existing[k] = v
		}
		return
	}
	result[key] = section
}

func (e architectContextEnvelope) apply(result map[string]any) {
	result["planning_memory"] = e.Planning
	result["foundation_memory"] = e.Foundation
	result["reference_pack"] = e.References
}

// buildProgressStatus 在 Architect 不传 chapter 时返回进度摘要。
// Writer/Editor 的章节路径不需要这些信息，避免干扰写作。
func (t *ContextTool) buildProgressStatus(result map[string]any) {
	progress, err := t.store.Progress.Load()
	if err != nil || progress == nil {
		return
	}
	status := map[string]any{
		"phase":              string(progress.Phase),
		"flow":               string(progress.Flow),
		"completed_chapters": len(progress.CompletedChapters),
		"total_chapters":     progress.TotalChapters,
		"next_chapter":       progress.NextChapter(),
		"total_word_count":   progress.TotalWordCount,
	}
	if progress.InProgressChapter > 0 {
		status["in_progress_chapter"] = progress.InProgressChapter
	}
	if len(progress.PendingRewrites) > 0 {
		status["pending_rewrites"] = progress.PendingRewrites
		status["rewrite_reason"] = progress.RewriteReason
	}
	if progress.Layered {
		status["layered"] = true
		status["current_volume"] = progress.CurrentVolume
		status["current_arc"] = progress.CurrentArc
	}
	if progress.Phase == domain.PhaseComplete {
		status["finished"] = true
	}
	result["progress_status"] = status
}

// buildUserRules 把合并后的 Bundle 注入 working_memory.user_rules（canonical 路径）。
//
// 单点注入：writer / editor / architect 任一路径调用 novel_context
// 都能在 working_memory.user_rules 拿到一致的偏好。architect 路径原本没有 working_memory，
// 由本函数按需新建（仅装 user_rules）；chapter > 0 路径下 working_memory 已存在，直接嵌入。
//
// 即便 Bundle 为空也注入，保持字段稳定，避免 LLM 看到 user_rules=null 而走异常分支。
//
// 注入策略：只给 LLM 看 structured + preferences——这两项才是创作时需要遵循的偏好。
// sources / conflicts 是诊断信息（用户冲突排查），不进 LLM；由 CLI 启动诊断面板按需展示。
func (t *ContextTool) buildUserRules(result map[string]any) {
	snap, err := t.store.UserRules.Load()
	if err != nil || snap == nil {
		// 快照尚未初始化时使用代码内置默认，保证机械底线（字数/禁语/疲劳词）始终存在。
		def := rules.BuildSnapshot([]rules.Candidate{rules.SystemDefaults()})
		snap = &def
	}
	working, ok := result["working_memory"].(map[string]any)
	if !ok {
		working = map[string]any{}
		result["working_memory"] = working
	}
	working["user_rules"] = snap.PayloadForRole(t.role)
}

func (t *ContextTool) buildSimulationProfile(result map[string]any, sectionKey string, warn func(string, error)) {
	profile, err := t.store.Simulation.Load()
	if err != nil {
		warn("simulation_profile", err)
		return
	}
	compact := domain.CompactSimulationProfile(profile)
	if compact == nil {
		return
	}
	section, ok := result[sectionKey].(map[string]any)
	if !ok {
		section = map[string]any{}
		result[sectionKey] = section
	}
	section["simulation_profile"] = compact
	result["simulation_profile"] = true
}

func (t *ContextTool) buildBaseContext(result map[string]any, warn func(string, error)) {
	if premise, err := t.store.Outline.LoadPremise(); err == nil && premise != "" {
		result["premise"] = premise
		if sections := parsePremiseSections(premise); len(sections) > 0 {
			result["premise_sections"] = sections
		}
		tier := domain.PlanningTier("")
		if meta, err := t.store.RunMeta.Load(); err == nil && meta != nil {
			tier = meta.PlanningTier
		}
		result["premise_structure"] = premiseStructure(premise, tier)
	} else {
		warn("premise", err)
	}
	if outline, err := t.store.Outline.LoadOutline(); err == nil && outline != nil {
		result["outline"] = outline
	} else {
		warn("outline", err)
	}
	if rules, err := t.store.World.LoadWorldRules(); err == nil && len(rules) > 0 {
		result["world_rules"] = rules
	} else {
		warn("world_rules", err)
	}
}

func (t *ContextTool) prepareChapterContext(chapter int, envelope *chapterContextEnvelope, warn func(string, error)) contextBuildState {
	state := contextBuildState{
		chapter: chapter,
		profile: domain.NewContextProfile(0),
	}

	progress, err := t.store.Progress.Load()
	warn("progress", err)
	runMeta, err := t.store.RunMeta.Load()
	warn("run_meta", err)
	state.progress = progress
	state.runMeta = runMeta

	if runMeta != nil && runMeta.PlanningTier != "" {
		envelope.Episodic["planning_tier"] = runMeta.PlanningTier
	}

	// 加载风格评审状态与精炼反馈（仅用于 Writer 分支感知，不暴露账本内部细节）
	if runMeta != nil && runMeta.StyleReviewMode.Enabled() {
		if ledger, lErr := t.store.StyleReview.Load(chapter); lErr == nil && ledger != nil {
			state.styleReviewStatus = ledger.CurrentStatus()
			state.styleReviewFeedback = buildCompactStyleReviewFeedback(ledger)
		} else if lErr != nil {
			warn("style_review_ledger", lErr)
		}
	}

	if progress != nil && progress.TotalChapters > 0 {
		state.profile = domain.NewContextProfile(progress.TotalChapters)
	}
	if progress == nil || !progress.Layered {
		state.profile.Layered = false
	}

	currentEntry, currentEntryErr := t.store.Outline.GetChapterOutline(chapter)
	if currentEntryErr == nil {
		envelope.Working["current_chapter_outline"] = currentEntry
	} else {
		warn("current_chapter_outline", currentEntryErr)
	}
	state.currentEntry = currentEntry

	chapterPlan, chapterPlanErr := t.store.Drafts.LoadChapterPlan(chapter)
	if chapterPlanErr == nil && chapterPlan != nil {
		envelope.Working["chapter_plan"] = chapterPlan
		if len(chapterPlan.Contract.RequiredBeats) > 0 ||
			len(chapterPlan.Contract.ForbiddenMoves) > 0 ||
			len(chapterPlan.Contract.ContinuityChecks) > 0 ||
			len(chapterPlan.Contract.EvaluationFocus) > 0 ||
			chapterPlan.Contract.EmotionTarget != "" ||
			len(chapterPlan.Contract.PayoffPoints) > 0 ||
			chapterPlan.Contract.HookGoal != "" {
			envelope.Working["chapter_contract"] = chapterPlan.Contract
		}
	} else {
		warn("chapter_plan", chapterPlanErr)
	}
	state.chapterPlan = chapterPlan

	// 是否正在重写本章：决定 novel_context 是否补"重写专用"事实。
	isRewrite := progress != nil && slices.Contains(progress.PendingRewrites, chapter)

	// 暴露 draft 是否已存在的事实：让 writer 被重派时能自行判断跳过重写还是覆盖。
	// 只暴露 exists + word_count，不注入正文（正文让 writer 按需用 read_chapter 拉）。
	if _, draftWords, draftErr := t.store.Drafts.LoadChapterContent(chapter); draftErr == nil && draftWords > 0 {
		envelope.Working["chapter_draft"] = map[string]any{
			"exists":     true,
			"word_count": draftWords,
		}
	} else if draftErr != nil {
		warn("chapter_draft", draftErr)
	}

	// 重写时把"为什么改 + 改哪里"交给 writer：理由来自返工队列，具体批评来自本章评审
	// （selectReviewLessons 只召回 chapter-1..chapter-3，恰好漏掉本章本身，writer 又无读评审的工具）。
	// 正文不在此注入——保持"正文按需 read_chapter 拉"的约定不破。
	if isRewrite {
		brief := map[string]any{"reason": progress.RewriteReason}
		if review, reviewErr := t.store.World.LoadReview(chapter); reviewErr == nil && review != nil {
			if review.Summary != "" {
				brief["review_summary"] = review.Summary
			}
			if len(review.Issues) > 0 {
				brief["issues"] = review.Issues
			}
			if len(review.ContractMisses) > 0 {
				brief["contract_misses"] = review.ContractMisses
			}
		} else if reviewErr != nil {
			warn("rewrite_review", reviewErr)
		}
		envelope.Working["rewrite_brief"] = brief
	}

	foreshadow, foreshadowErr := t.store.World.LoadActiveForeshadow()
	warn("foreshadow_ledger", foreshadowErr)
	state.foreshadow = foreshadow

	relationships, relErr := t.store.World.LoadRelationships()
	warn("relationship_state", relErr)
	if len(relationships) > 0 {
		envelope.Episodic["relationship_state"] = relationships
	}
	state.relationships = relationships

	allStateChanges, scErr := t.store.World.LoadStateChanges()
	warn("recent_state_changes", scErr)
	state.allStateChanges = allStateChanges
	if len(allStateChanges) > 0 {
		start := max(chapter-2, 1)
		var recent []domain.StateChange
		for _, c := range allStateChanges {
			if c.Chapter >= start && c.Chapter < chapter {
				recent = append(recent, c)
			}
		}
		if len(recent) > 0 {
			envelope.Episodic["recent_state_changes"] = recent
		}
	}

	styleRules, styleErr := t.store.World.LoadStyleRules()
	warn("style_rules", styleErr)
	state.styleRules = styleRules
	// 尝试加载 compass 双层格式（兼容旧单体）
	styleRulesCompass, compassErr := t.store.World.LoadStyleRulesCompass()
	if compassErr != nil {
		warn("style_rules_compass", compassErr)
	} else if styleRulesCompass != nil {
		state.styleRulesCompass = styleRulesCompass
	}

	// 加载手动风格锚点（含旧格式兼容）
	manRes := t.store.StyleAnchors.LoadManual()
	if len(manRes.Warnings) > 0 {
		for _, w := range manRes.Warnings {
			warn("style_anchors", fmt.Errorf("%s", w))
		}
	}

	// 仅正文笔法（prose/dialogue/taboos）算 style_rules；outline-only 不应阻断 anchor fallback
	state.hasStyleRules = state.styleRulesCompass != nil && state.styleRulesCompass.HasProseStyleContent()
	state.manStatus = manRes.Status

	switch manRes.Status {
	case store.StatusValid, store.StatusLegacyFormat:
		// 有效手动文件（含旧格式转换后）
		state.styleAnchorsManual = manRes.Anchors
		// 仅当 include_auto=true 时加载 auto 锚点
		if manRes.Anchors.IncludeAuto {
			var maxCompleted int
			if progress != nil {
				maxCompleted = maxCompletedChapter(progress.CompletedChapters)
			}
			if anchors := t.store.Drafts.ExtractStyleAnchors(3, maxCompleted); len(anchors) > 0 {
				state.styleAnchorsAuto = anchors
			}
		}

	case store.StatusEmptyValid:
		// 有效空文件：manual 存在但 anchors 为空 → 不触发任何 auto
		state.styleAnchorsManual = manRes.Anchors

	case store.StatusCorrupted:
		// 损坏 → fail closed：绝不注入 manual 也绝不注入任何 auto（即使无 style_rules）
		state.styleAnchorsManual = nil
		state.styleAnchorsAuto = nil

	case store.StatusNotExist:
		// 无手动文件
		state.styleAnchorsManual = nil
		// 仅当无 style_rules 时保留 legacy 自动回退
		if !state.hasStyleRules {
			var maxCompleted int
			if progress != nil {
				maxCompleted = maxCompletedChapter(progress.CompletedChapters)
			}
			if anchors := t.store.Drafts.ExtractStyleAnchors(3, maxCompleted); len(anchors) > 0 {
				state.styleAnchorsAuto = anchors
			}
		}
	}

	state.storyThreads = t.selectStoryThreads(state)
	if len(state.storyThreads) > 0 && len(state.storyThreads) < storyThreadRecallMinSelected {
		state.storyThreads = nil
	}

	return state
}

func (t *ContextTool) buildChapterContext(result map[string]any, state contextBuildState, warn func(string, error)) {
	envelope := newChapterContextEnvelope()
	result["memory_policy"] = domain.NewChapterMemoryPolicy(state.progress, state.profile, state.currentEntry != nil)

	if state.profile.Layered {
		t.loadLayeredCharacters(envelope.Episodic, state.chapter, warn)
	} else {
		t.loadFilteredCharacters(envelope.Episodic, state.chapter, warn)
	}

	t.buildChapterEpisodicMemory(&envelope, state, warn)
	t.buildChapterWorkingMemory(&envelope, state, warn)
	t.buildChapterReferencePack(&envelope, state, warn)
	t.buildChapterSelectedMemory(&envelope, state, warn)
	t.buildStyleStats(&envelope, state)
	envelope.apply(result)
}

// buildStyleStats 对全部已完成章节做全书级风格统计，注入 episodic_memory.style_stats。
// 弧内评审窗口对"章均几十次的句式 tic、章末形态同构、跨章复读"天然失明，只有
// 全书统计能暴露——统计归代码（确定性），裁定归 LLM（editor 在 aesthetic 维度
// 按数字判分，writer 据此自避免）。章数不足时 stylestat 返回 nil，不注入。
func (t *ContextTool) buildStyleStats(envelope *chapterContextEnvelope, state contextBuildState) {
	if state.progress == nil || len(state.progress.CompletedChapters) == 0 {
		return
	}
	completed := slices.Clone(state.progress.CompletedChapters)
	slices.Sort(completed)
	chapters := make([]string, 0, len(completed))
	for _, ch := range completed {
		// 个别章读取失败跳过：统计是 best-effort 事实，不因单章缺失放弃全书视野
		if text, err := t.store.Drafts.LoadChapterText(ch); err == nil && text != "" {
			chapters = append(chapters, text)
		}
	}

	var titles []string
	if outline, err := t.store.Outline.LoadOutline(); err == nil {
		for _, entry := range outline {
			titles = append(titles, entry.Title)
		}
	}

	stats := stylestat.Compute(stylestat.Input{
		Chapters:  chapters,
		Titles:    titles,
		Stopwords: t.styleStopwords(),
	})
	if stats == nil {
		return
	}
	envelope.Episodic["style_stats"] = stats
}

// styleStopwords 收集角色名与别名供短语挖掘过滤——出场人名天然高频，不是文风问题。
func (t *ContextTool) styleStopwords() []string {
	var words []string
	if chars, err := t.store.Characters.Load(); err == nil {
		for _, c := range chars {
			words = append(words, c.Name)
			words = append(words, c.Aliases...)
		}
	}
	if cast, err := t.store.Cast.RecentActive(50); err == nil {
		for _, e := range cast {
			words = append(words, e.Name)
			words = append(words, e.Aliases...)
		}
	}
	return words
}

func (t *ContextTool) buildChapterWorkingMemory(envelope *chapterContextEnvelope, state contextBuildState, warn func(string, error)) {
	if next, err := t.store.Outline.GetChapterOutline(state.chapter + 1); err == nil && next != nil {
		envelope.Working["next_chapter_outline"] = next
	}

	if state.profile.Layered {
		t.loadLayeredSummaries(envelope.Working, state.chapter, state.profile.SummaryWindow, warn)
		// 收官纪律：本章属于已宣告的收官卷时注入，防 writer 在收官段临章再开新钩子
		//（收官卷写完即自动完结，此时新埋的伏笔永远没有机会回收）。
		if volumes, err := t.store.Outline.LoadLayeredOutline(); err == nil {
			if fv := domain.FinaleVolume(volumes); fv > 0 {
				if b, berr := t.store.Outline.CheckArcBoundary(state.chapter); berr == nil && b != nil && b.Volume == fv {
					envelope.Working["finale"] = "本卷为全书收官卷：不再新开长线或埋新伏笔，优先回收既有伏笔、收拢关系线，按大纲把故事推向终局。"
				}
			}
		}
	} else {
		if summaries, err := t.store.Summaries.LoadRecentSummaries(state.chapter, state.profile.SummaryWindow); err == nil && len(summaries) > 0 {
			envelope.Working["recent_summaries"] = summaries
		} else {
			warn("recent_summaries", err)
		}
	}

	if timeline, err := t.store.World.LoadRecentTimeline(state.chapter, state.profile.TimelineWindow); err == nil && len(timeline) > 0 {
		envelope.Working["timeline"] = timeline
	} else {
		warn("timeline", err)
	}

	if state.progress != nil {
		checkpoint := map[string]any{
			"in_progress_chapter": state.progress.InProgressChapter,
		}
		if len(state.progress.StrandHistory) > 0 {
			checkpoint["strand_history"] = state.progress.StrandHistory
		}
		if len(state.progress.HookHistory) > 0 {
			checkpoint["hook_history"] = state.progress.HookHistory
		}
		// Expose style review mode/status for Writer branching
		if state.runMeta != nil && state.runMeta.StyleReviewMode != "" {
			checkpoint["style_review_mode"] = string(state.runMeta.StyleReviewMode)
		}
		if state.styleReviewStatus != "" {
			checkpoint["style_review_status"] = string(state.styleReviewStatus)
		}
		// 精炼风格评审反馈：仅 Writer 在 revision_open/final_pending/exhausted 时注入
		if t.role == "writer" && state.styleReviewFeedback != nil {
			checkpoint["style_review_feedback"] = state.styleReviewFeedback
		}
		envelope.Working["checkpoint"] = checkpoint
	}

	if state.chapter > 1 {
		if prevText, err := t.store.Drafts.LoadChapterText(state.chapter - 1); err == nil && prevText != "" {
			runes := []rune(prevText)
			if len(runes) > 800 {
				runes = runes[len(runes)-800:]
			}
			envelope.Working["previous_tail"] = string(runes)
		}
	}
}

func (t *ContextTool) buildChapterSelectedMemory(envelope *chapterContextEnvelope, state contextBuildState, warn func(string, error)) {
	if len(state.storyThreads) > 0 {
		envelope.Selected["story_threads"] = state.storyThreads
	}
	if lessons := t.selectReviewLessons(state.chapter, warn); len(lessons) > 0 {
		envelope.Selected["review_lessons"] = lessons
	}
}

func (t *ContextTool) buildChapterEpisodicMemory(envelope *chapterContextEnvelope, state contextBuildState, warn func(string, error)) {
	if len(state.foreshadow) > 0 && len(state.storyThreads) == 0 {
		envelope.Episodic["foreshadow_ledger"] = state.foreshadow
	}

	// 配角名册：召回最近活跃的次要角色，让 Writer 在引入旧角色时能保持口吻/定位一致
	// 不召回所有条目（长篇会膨胀），只给最近活跃的前 N 个，按 LastSeenChapter 倒序
	if recentCast, err := t.store.Cast.RecentActive(15); err == nil && len(recentCast) > 0 {
		simplified := make([]map[string]any, 0, len(recentCast))
		for _, e := range recentCast {
			item := map[string]any{
				"name":             e.Name,
				"first_seen":       e.FirstSeenChapter,
				"last_seen":        e.LastSeenChapter,
				"appearance_count": e.AppearanceCount,
			}
			if e.BriefRole != "" {
				item["brief_role"] = e.BriefRole
			}
			if len(e.Aliases) > 0 {
				item["aliases"] = e.Aliases
			}
			simplified = append(simplified, item)
		}
		envelope.Episodic["recent_cast"] = simplified
	} else if err != nil {
		warn("recent_cast", err)
	}

	if state.progress != nil && state.progress.TotalChapters > 30 && state.currentEntry != nil {
		if related := t.buildRelatedChapters(
			state.chapter,
			state.currentEntry,
			state.foreshadow,
			state.relationships,
			state.allStateChanges,
		); len(related) > 0 {
			envelope.Episodic["related_chapters"] = related
		}
	}

	if state.profile.Layered && state.progress != nil {
		pos := map[string]any{
			"volume": state.progress.CurrentVolume,
			"arc":    state.progress.CurrentArc,
		}
		if volumes, err := t.store.Outline.LoadLayeredOutline(); err == nil {
			globalCh := 1
			for _, v := range volumes {
				if v.Index == state.progress.CurrentVolume {
					pos["volume_title"] = v.Title
					pos["volume_theme"] = v.Theme
				}
				for _, arc := range v.Arcs {
					if v.Index == state.progress.CurrentVolume && arc.Index == state.progress.CurrentArc {
						pos["arc_title"] = arc.Title
						pos["arc_goal"] = arc.Goal
						if n := len(arc.Chapters); n > 0 {
							pos["arc_total_chapters"] = n
							pos["arc_chapter_index"] = state.chapter - globalCh + 1
						}
					}
					globalCh += len(arc.Chapters)
				}
			}
		} else {
			warn("layered_outline", err)
		}
		envelope.Episodic["position"] = pos
	}
}

func (t *ContextTool) buildChapterReferencePack(envelope *chapterContextEnvelope, state contextBuildState, warn func(string, error)) {
	// ── 角色门控：所有 anchor 类字段（manual/auto/legacy）仅 Writer/Editor 可见 ──
	isWriterOrEditor := t.role == "writer" || t.role == "editor"
	isWriter := t.role == "writer"

	// ── 计算一次作用域过滤后的 compass，供 style_rules 和 writer_style_card 共用 ──
	compassForView := scopedCompassForChapter(t.store, state.chapter, warn)

	// 1. style_rules：仅 Writer/Editor 注入正文笔法（prose/dialogue/taboos）；
	//    Architect/Coordinator 即使 chapter>0 也不应注入 prose style_rules。
	//
	//    compass 路径阻断 legacy 降级：compass 存在（compassForView != nil）时
	//    无论是否注入 prose 内容，都不退回 legacy styleRules/anchors。
	if isWriterOrEditor && compassForView != nil && compassForView.HasProseStyleContent() {
		envelope.References["style_rules"] = buildCompassProseInjectionView(compassForView)
	} else if isWriterOrEditor && compassForView == nil && state.styleRules != nil && (len(state.styleRules.Prose) > 0 || len(state.styleRules.Dialogue) > 0 || len(state.styleRules.Taboos) > 0) {
		envelope.References["style_rules"] = state.styleRules
	} else if isWriterOrEditor && state.manStatus == store.StatusNotExist {
		// 无风格规则且无手动文件时回退到 legacy style_anchors / voice_samples
		// 注意：auto 锚点已在 prepareChapterContext 中一次性提取至 state.styleAnchorsAuto，
		// 此处不得再次调用 ExtractStyleAnchors。
		if len(state.styleAnchorsAuto) > 0 {
			envelope.References["style_anchors"] = state.styleAnchorsAuto
		}

		if state.currentEntry != nil {
			var voiceSamples []map[string]any
			chars, _ := t.store.Characters.Load()
			for _, c := range chars {
				if c.Tier == "secondary" || c.Tier == "decorative" {
					continue
				}
				samples := t.store.Drafts.ExtractDialogue(c.Name, c.Aliases, 3, safeMaxCompleted(state.progress))
				if len(samples) > 0 {
					voiceSamples = append(voiceSamples, map[string]any{
						"character": c.Name,
						"samples":   samples,
					})
				}
				if len(voiceSamples) >= 5 {
					break
				}
			}
			if len(voiceSamples) > 0 {
				envelope.References["voice_samples"] = voiceSamples
			}
		}
	}

	// 2. writer_style_card：Writer 专用正面的 prose+dialogue 风格卡片。
	//    跟随 compassForView 作用域结果，不含 taboos/user_rules/anchor 字段。
	//    ⌈bounded⌋ 诉求：不出现在 non-compass 路径，也不复制 user_rules 或 anchor excerpt。
	if isWriter && compassForView != nil {
		card := buildWriterStyleCard(compassForView)
		if card != nil {
			envelope.References["writer_style_card"] = card
		}
	}

	// 3. style_anchors_manual：仅 Writer/Editor，按当前章节过滤，只注入精简视图（id+excerpt）
	if isWriterOrEditor && state.styleAnchorsManual != nil && len(state.styleAnchorsManual.Anchors) > 0 {
		if injection := state.styleAnchorsManual.ToInjectionView(state.chapter); len(injection) > 0 {
			envelope.References["style_anchors_manual"] = injection
		}
	}

	// 3. style_anchors_auto：仅 Writer/Editor，仅当 include_auto=true 时低优先级注入
	if isWriterOrEditor && len(state.styleAnchorsAuto) > 0 &&
		state.styleAnchorsManual != nil && state.styleAnchorsManual.IncludeAuto {
		envelope.References["style_anchors_auto"] = state.styleAnchorsAuto
	}

	envelope.References["references"] = t.writerReferences(state.chapter)
}

// scopedCompassForChapter loads the compass and filters the current section
// based on the exact chapter-scope logic used by the Writer context.
// This ensures ReviewBasis uses the same scoped data as the Writer card.
// Returns nil when no compass with prose content exists or is loadable.
func scopedCompassForChapter(st *store.Store, chapter int, warn func(string, error)) *domain.WritingStyleRulesCompass {
	compass, err := st.World.LoadStyleRulesCompass()
	if err != nil || compass == nil || !compass.HasProseStyleContent() {
		return nil
	}
	if compass.Current == nil {
		return compass
	}

	// Same oracle gate as Writer context's buildChapterReferencePack:
	// progress unavailable → fail closed exclude current;
	// non-layered → always include current;
	// layered → only include when chapter belongs to matching volume+arc.
	progress, pErr := st.Progress.Load()
	if pErr != nil || progress == nil {
		if warn != nil {
			warn("style_rules_current_scope", fmt.Errorf(
				"进度状态不可用（progress=nil），style_rules.current (卷%d弧%d) 已降级为仅 long 规则",
				compass.Current.Volume, compass.Current.Arc,
			))
		}
		filtered := *compass
		filtered.Current = nil
		return &filtered
	}

	if !progress.Layered {
		return compass
	}

	volume, arc, locateErr := st.Outline.LocateChapter(chapter)
	if locateErr != nil {
		if warn != nil {
			warn("style_rules_current_scope", fmt.Errorf(
				"定位章节 %d 在分层大纲中失败: %w", chapter, locateErr,
			))
		}
		filtered := *compass
		filtered.Current = nil
		return &filtered
	}
	if volume != compass.Current.Volume || arc != compass.Current.Arc {
		if warn != nil {
			warn("style_rules_current_scope", fmt.Errorf(
				"章节 %d (卷%d弧%d) 不在 style_rules.current (卷%d弧%d) 范围内，降级为仅 long 规则",
				chapter, volume, arc,
				compass.Current.Volume, compass.Current.Arc,
			))
		}
		filtered := *compass
		filtered.Current = nil
		return &filtered
	}

	return compass
}

// buildCompassProseInjectionView Writer/Editor：仅 prose/dialogue/taboos（无 outline）。
func buildCompassProseInjectionView(compass *domain.WritingStyleRulesCompass) map[string]any {
	view := map[string]any{
		"_compass": "双层正文风格规则。long（长期基线）优先于 current（当前弧定制）。不含规划 outline。",
	}
	hasLong := compass.Long != nil && (len(compass.Long.Prose) > 0 || len(compass.Long.Dialogue) > 0 || len(compass.Long.Taboos) > 0)
	hasCurrent := compass.Current != nil && (len(compass.Current.Prose) > 0 || len(compass.Current.Dialogue) > 0 || len(compass.Current.Taboos) > 0)
	if hasLong {
		view["long"] = map[string]any{
			"prose":    compass.Long.Prose,
			"dialogue": compass.Long.Dialogue,
			"taboos":   compass.Long.Taboos,
		}
	}
	if hasCurrent {
		cur := map[string]any{
			"volume": compass.Current.Volume,
			"arc":    compass.Current.Arc,
		}
		if len(compass.Current.Prose) > 0 {
			cur["prose"] = compass.Current.Prose
		}
		if len(compass.Current.Dialogue) > 0 {
			cur["dialogue"] = compass.Current.Dialogue
		}
		if len(compass.Current.Taboos) > 0 {
			cur["taboos"] = compass.Current.Taboos
		}
		if compass.Current.LastUpdated != "" {
			cur["last_updated"] = compass.Current.LastUpdated
		}
		view["current"] = cur
	}
	longHasProse := hasLong && len(compass.Long.Prose) > 0
	longHasTaboos := hasLong && len(compass.Long.Taboos) > 0
	longHasDialogue := hasLong && len(compass.Long.Dialogue) > 0
	if longHasProse {
		view["prose"] = compass.Long.Prose
	}
	if longHasTaboos {
		view["taboos"] = compass.Long.Taboos
	}
	if longHasDialogue {
		view["dialogue"] = compass.Long.Dialogue
	}
	if hasCurrent {
		if !longHasProse && len(compass.Current.Prose) > 0 {
			view["prose"] = compass.Current.Prose
		}
		if !longHasTaboos && len(compass.Current.Taboos) > 0 {
			view["taboos"] = compass.Current.Taboos
		}
		if len(compass.Current.Dialogue) > 0 {
			if longHasDialogue {
				existingVoices := compass.Long.Dialogue
				existingNames := make(map[string]bool, len(existingVoices))
				for _, v := range existingVoices {
					existingNames[v.Name] = true
				}
				for _, v := range compass.Current.Dialogue {
					if !existingNames[v.Name] {
						existingVoices = append(existingVoices, v)
					}
				}
				view["dialogue"] = existingVoices
			} else {
				view["dialogue"] = compass.Current.Dialogue
			}
		}
		view["volume"] = compass.Current.Volume
		view["arc"] = compass.Current.Arc
		if compass.Current.LastUpdated != "" {
			view["last_updated"] = compass.Current.LastUpdated
		}
	}
	return view
}

// buildCompassOutlineInjectionView Architect：仅 outline 规划规则（无 prose/dialogue/taboos）。
func buildCompassOutlineInjectionView(compass *domain.WritingStyleRulesCompass) map[string]any {
	if compass == nil || !compass.HasOutlineContent() {
		return nil
	}
	view := map[string]any{
		"_compass": "规划层 outline 规则。约束 core_event/scenes 的 beat 形态，不是正文笔法。long 优先，current 追加。",
	}
	var longO, curO *domain.OutlinePlanningRules
	if compass.Long != nil && compass.Long.Outline.HasContent() {
		longO = compass.Long.Outline
		view["long"] = map[string]any{"outline": longO}
	}
	if compass.Current != nil && compass.Current.Outline.HasContent() {
		curO = compass.Current.Outline
		cur := map[string]any{
			"volume":  compass.Current.Volume,
			"arc":     compass.Current.Arc,
			"outline": curO,
		}
		if compass.Current.LastUpdated != "" {
			cur["last_updated"] = compass.Current.LastUpdated
		}
		view["current"] = cur
		view["volume"] = compass.Current.Volume
		view["arc"] = compass.Current.Arc
	}
	if merged := mergeOutlinePlanning(longO, curO); merged != nil {
		view["outline"] = merged
	}
	return view
}

// buildCompassInjectionView 完整视图（测试/诊断用；运行时按角色拆分注入）。
func buildCompassInjectionView(compass *domain.WritingStyleRulesCompass) map[string]any {
	view := buildCompassProseInjectionView(compass)
	if outlineView := buildCompassOutlineInjectionView(compass); outlineView != nil {
		if o, ok := outlineView["outline"]; ok {
			view["outline"] = o
		}
		if long, ok := view["long"].(map[string]any); ok {
			if ol, ok := outlineView["long"].(map[string]any); ok {
				if o, ok := ol["outline"]; ok {
					long["outline"] = o
				}
			}
		} else if ol, ok := outlineView["long"]; ok {
			view["long"] = ol
		}
		if cur, ok := view["current"].(map[string]any); ok {
			if oc, ok := outlineView["current"].(map[string]any); ok {
				if o, ok := oc["outline"]; ok {
					cur["outline"] = o
				}
			}
		} else if oc, ok := outlineView["current"]; ok {
			view["current"] = oc
		}
	}
	view["_compass"] = "双层风格规则。long（长期基线）优先于 current（当前弧定制）。"
	return view
}

// buildCompactStyleReviewFeedback 从账本中提取精炼的风格评审反馈视图。
// 仅当最近完成的周期有 result 且当前状态为 revision_open/final_pending/exhausted
// 时返回非 nil 结果。返回结构含 strength（正面评价）和 findings（最多 3 条精简条目）。
// 绝不泄漏完整账本内部细节（attempt_id、digest、request metadata 等）。
func buildCompactStyleReviewFeedback(ledger *domain.StyleReviewLedger) map[string]any {
	if ledger == nil || ledger.IsEmpty() {
		return nil
	}
	status := ledger.CurrentStatus()
	// 仅 revision_open/final_pending/exhausted 需要反馈
	switch status {
	case domain.ReviewStatusRevisionOpen, domain.ReviewStatusFinalPending, domain.ReviewStatusExhausted:
		// 继续
	default:
		return nil
	}

	// 找到最后一个有 result 的周期
	var lastResult *domain.StyleReviewEntry
	for i := len(ledger.Cycles) - 1; i >= 0; i-- {
		if ledger.Cycles[i].Result != nil {
			lastResult = &ledger.Cycles[i]
			break
		}
	}
	if lastResult == nil {
		return nil
	}

	r := lastResult.Result
	feedback := map[string]any{
		"verdict": string(r.Verdict),
	}
	// strength: 正面评价
	if r.Evidence != "" {
		feedback["strength"] = map[string]any{
			"evidence": truncateRunes(r.Evidence, 60),
		}
	}
	// findings: 最多 3 条精简条目
	if len(r.Findings) > 0 {
		n := len(r.Findings)
		if n > 3 {
			n = 3
		}
		compact := make([]map[string]any, 0, n)
		for i, f := range r.Findings {
			if i >= 3 {
				break
			}
			compact = append(compact, map[string]any{
				"dimension":  f.Dimension,
				"severity":   f.Severity,
				"category":   f.Category,
				"evidence":   truncateRunes(f.Evidence, 60),
				"suggestion": f.Suggestion,
			})
		}
		feedback["findings"] = compact
	}
	return feedback
}

// mergeOutlinePlanning 合并 long/current 规划规则：long 在前，current 追加未重复条目。
func mergeOutlinePlanning(long, current *domain.OutlinePlanningRules) *domain.OutlinePlanningRules {
	out := &domain.OutlinePlanningRules{}
	seenBeat := map[string]bool{}
	seenAnti := map[string]bool{}
	appendUnique := func(dst *[]string, seen map[string]bool, items []string) {
		for _, s := range items {
			s = strings.TrimSpace(s)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			*dst = append(*dst, s)
		}
	}
	if long != nil {
		appendUnique(&out.BeatRules, seenBeat, long.BeatRules)
		appendUnique(&out.AntiPatterns, seenAnti, long.AntiPatterns)
	}
	if current != nil {
		appendUnique(&out.BeatRules, seenBeat, current.BeatRules)
		appendUnique(&out.AntiPatterns, seenAnti, current.AntiPatterns)
	}
	if !out.HasContent() {
		return nil
	}
	return out
}

// charEntry is a lighter inline representation for writer_style_card dialogue entries.
// Using a named package-level type so estimateWriterStyleCardBytes can reference it.
type charEntry struct {
	Name  string   `json:"name"`
	Rules []string `json:"rules"`
}

// buildWriterStyleCard 从已作用域过滤的 compass 构建 Writer 风格卡片。
// 只含 prose + dialogue（不含 taboos、volume、arc、last_updated）。
// long 优先，current 补充；五项防御性上限确保不超预算。
// 达到上限时截断末尾条目/字符；card 为空时返回 nil。
func buildWriterStyleCard(compass *domain.WritingStyleRulesCompass) map[string]any {
	if compass == nil {
		return nil
	}

	// ── 收集 prose（long 在前，current 补充不重复） ──
	var prose []string
	if compass.Long != nil {
		for _, p := range compass.Long.Prose {
			if utf8.RuneCountInString(p) > writerStyleCardMaxRuleLen {
				p = string([]rune(p)[:writerStyleCardMaxRuleLen])
			}
			if p != "" {
				prose = append(prose, p)
			}
			if len(prose) >= writerStyleCardMaxProseRules {
				break
			}
		}
	}
	if compass.Current != nil && len(prose) < writerStyleCardMaxProseRules {
		for _, p := range compass.Current.Prose {
			if slices.Contains(prose, p) {
				continue
			}
			if utf8.RuneCountInString(p) > writerStyleCardMaxRuleLen {
				p = string([]rune(p)[:writerStyleCardMaxRuleLen])
			}
			if p != "" {
				prose = append(prose, p)
			}
			if len(prose) >= writerStyleCardMaxProseRules {
				break
			}
		}
	}

	// ── 收集 dialogue（long 优先同名，current 补充新角色） ──
	var dialogue []charEntry
	seenNames := make(map[string]bool)

	appendChar := func(name string, rules []string) {
		if name == "" || seenNames[name] {
			return
		}
		if len(dialogue) >= writerStyleCardMaxDialogueChars {
			return
		}
		var cappedRules []string
		for _, r := range rules {
			if utf8.RuneCountInString(r) > writerStyleCardMaxRuleLen {
				r = string([]rune(r)[:writerStyleCardMaxRuleLen])
			}
			if r != "" {
				cappedRules = append(cappedRules, r)
			}
			if len(cappedRules) >= writerStyleCardMaxRulesPerChar {
				break
			}
		}
		if len(cappedRules) > 0 {
			seenNames[name] = true
			dialogue = append(dialogue, charEntry{Name: name, Rules: cappedRules})
		}
	}

	if compass.Long != nil {
		for _, d := range compass.Long.Dialogue {
			appendChar(d.Name, d.Rules)
		}
	}
	if compass.Current != nil {
		for _, d := range compass.Current.Dialogue {
			appendChar(d.Name, d.Rules)
		}
	}

	// ── 组装卡片，序列化后检查总字节上限 ──
	if len(prose) == 0 && len(dialogue) == 0 {
		return nil
	}
	card := make(map[string]any)
	if len(prose) > 0 {
		card["prose"] = prose
	}
	if len(dialogue) > 0 {
		card["dialogue"] = dialogue
	}

	// 检查总字节上限：直接 JSON 序列化后量测
	raw, _ := json.Marshal(card)
	for len(raw) > writerStyleCardMaxTotalBytes && len(dialogue) > 0 {
		dialogue = dialogue[:len(dialogue)-1]
		if len(dialogue) > 0 {
			card["dialogue"] = dialogue
		} else {
			delete(card, "dialogue")
		}
		raw, _ = json.Marshal(card)
	}
	for len(raw) > writerStyleCardMaxTotalBytes && len(prose) > 0 {
		prose = prose[:len(prose)-1]
		if len(prose) > 0 {
			card["prose"] = prose
		} else {
			delete(card, "prose")
		}
		raw, _ = json.Marshal(card)
	}
	if len(card) == 0 {
		return nil
	}
	return card
}

func (t *ContextTool) buildArchitectContext(result map[string]any, warn func(string, error)) {
	envelope := newArchitectContextEnvelope()
	result["memory_policy"] = domain.NewArchitectMemoryPolicy()
	t.buildArchitectPlanning(&envelope, warn)
	t.buildArchitectFoundation(&envelope, warn)
	t.buildArchitectReferences(&envelope, warn)
	envelope.apply(result)
}

func (t *ContextTool) buildArchitectPlanning(envelope *architectContextEnvelope, warn func(string, error)) {
	runMeta, err := t.store.RunMeta.Load()
	warn("run_meta", err)
	if runMeta != nil && runMeta.PlanningTier != "" {
		envelope.Planning["planning_tier"] = runMeta.PlanningTier
	}

	// ── archive_refs 投影 ──
	// 从 compass.long.open_threads 解析末尾 [room:<id>] marker，生成轻量索引，
	// 供 ArchitectLong 判断何时调用 read_planning_archive 工具。
	// 所有异常只写 warning，不使 novel_context 失败。
	t.buildArchiveRefsProjection(envelope, warn)

	var layered []domain.VolumeOutline
	if l, err := t.store.Outline.LoadLayeredOutline(); err == nil && len(l) > 0 {
		layered = l
		progress, progressErr := t.store.Progress.Load()
		warn("progress", progressErr)
		includeNearbyChapterDetail := t.role == "architect"
		envelope.Planning["layered_outline"] = planningLayeredOutlineView(layered, progress, includeNearbyChapterDetail)
		if includeNearbyChapterDetail {
			envelope.Planning["outline_detail_policy"] = planningOutlineDetailPolicy(layered, progress)
		}
		var skeletonArcs []map[string]any
		for _, v := range layered {
			for _, a := range v.Arcs {
				if !a.IsExpanded() {
					skeletonArcs = append(skeletonArcs, map[string]any{
						"volume":             v.Index,
						"arc":                a.Index,
						"title":              a.Title,
						"goal":               a.Goal,
						"estimated_chapters": a.EstimatedChapters,
					})
				}
			}
		}
		if len(skeletonArcs) > 0 {
			envelope.Planning["skeleton_arcs"] = skeletonArcs
		}
	} else {
		warn("layered_outline", err)
	}

	var compass *domain.StoryCompass
	if c, err := t.store.Outline.LoadCompass(); err == nil && c != nil {
		compass = c
		// 首轮只装 long/current 的稳定字段。详细 Long Reference 即便对 Architect
		// 也改由 read_planning_reference 按需读取，避免每轮固定支付整包成本。
		envelope.Planning["compass"] = compassContextView(compass, false)
	} else {
		warn("compass", err)
	}
	if volSummaries, err := t.store.Summaries.LoadAllVolumeSummaries(); err == nil && len(volSummaries) > 0 {
		envelope.Planning["volume_summaries"] = planningVolumeSummaryView(volSummaries)
	} else {
		warn("volume_summaries", err)
	}
	// 卷摘要承接已完成卷；当前卷的弧摘要承接最近实际剧情。扩弧时两者与
	// 骨架目标同时交给 Architect，让模型自行决定保留还是修订未写计划。
	if progress, err := t.store.Progress.Load(); err == nil && progress != nil && progress.CurrentVolume > 0 {
		if arcSummaries, err := t.store.Summaries.LoadArcSummaries(progress.CurrentVolume); err == nil && len(arcSummaries) > 0 {
			envelope.Planning["arc_summaries"] = arcSummaries
		} else {
			warn("arc_summaries", err)
		}
	} else {
		warn("progress_for_arc_summaries", err)
	}

	// completion_signals 把"全书是否该结尾"的关键事实集中呈现，
	// 让架构师在裁定 complete_book / append_volume 时一眼看到对照面。
	// 散落在 progress / compass / foreshadow / layered_outline 里靠 LLM 脑算容易漏。
	envelope.Planning["completion_signals"] = t.completionSignals(layered, compass)
}

// compassContextView 返回按角色裁剪的只读视图。Store 中仍持久化完整 Long
// Reference，常规 long/current 字段对所有获准读取 Compass 的角色保持一致。
func compassContextView(compass *domain.StoryCompass, includeLongReference bool) *domain.StoryCompass {
	if compass == nil {
		return nil
	}
	view := *compass
	if !includeLongReference {
		view.Long.Reference = nil
	}
	return &view
}

// planningLayeredOutlineView 始终完整保留所有卷纲与弧纲；只有弧下逐章的
// chapters[] 按视野裁剪为当前卷及物理相邻的上一卷、下一卷。更远卷可由
// read_planning_reference 批量补读完整章节细纲。Store 原始大纲不变。
func planningLayeredOutlineView(layered []domain.VolumeOutline, progress *domain.Progress, includeNearbyChapterDetail bool) []map[string]any {
	currentIndex := -1
	flatArcCount := 0
	currentVolumeIndex := 0
	for _, volume := range layered {
		for _, arc := range volume.Arcs {
			if progress != nil && volume.Index == progress.CurrentVolume && arc.Index == progress.CurrentArc {
				currentIndex = flatArcCount
			}
			flatArcCount++
		}
	}
	if currentIndex < 0 && flatArcCount > 0 {
		currentIndex = 0
	}
	if progress != nil {
		for i, volume := range layered {
			if volume.Index == progress.CurrentVolume {
				currentVolumeIndex = i
				break
			}
		}
	}

	flatIndex := 0
	view := make([]map[string]any, 0, len(layered))
	for volumeIndex, volume := range layered {
		keepChapterDetail := includeNearbyChapterDetail && volumeIndex >= currentVolumeIndex-1 && volumeIndex <= currentVolumeIndex+1
		volumeView := map[string]any{
			"index": volume.Index,
			"title": volume.Title,
			"theme": volume.Theme,
		}
		if volume.Final {
			volumeView["final"] = true
		}
		arcs := make([]map[string]any, 0, len(volume.Arcs))
		for _, arc := range volume.Arcs {
			arcView := map[string]any{
				"index": arc.Index,
				"title": arc.Title,
				"goal":  arc.Goal,
			}
			if !arc.IsExpanded() {
				arcView["status"] = "skeleton"
				arcView["estimated_chapters"] = arc.EstimatedChapters
			} else {
				arcView["status"] = "planned"
				if flatIndex < currentIndex {
					arcView["status"] = "completed"
				} else if flatIndex == currentIndex {
					arcView["status"] = "active"
				}
				arcView["chapter_count"] = len(arc.Chapters)
				arcView["chapter_start"] = arc.Chapters[0].Chapter
				arcView["chapter_end"] = arc.Chapters[len(arc.Chapters)-1].Chapter
				arcView["chapter_detail"] = keepChapterDetail
				if keepChapterDetail {
					arcView["chapters"] = arc.Chapters
				}
			}
			arcs = append(arcs, arcView)
			flatIndex++
		}
		volumeView["arcs"] = arcs
		view = append(view, volumeView)
	}
	return view
}

// planningOutlineDetailPolicy 显式告诉 Architect 哪些卷已经包含 chapters[]，
// 防止它把远处卷的完整弧纲误判为信息缺失并逐卷查询。
func planningOutlineDetailPolicy(layered []domain.VolumeOutline, progress *domain.Progress) map[string]any {
	currentVolumeIndex := 0
	if progress != nil {
		for i, volume := range layered {
			if volume.Index == progress.CurrentVolume {
				currentVolumeIndex = i
				break
			}
		}
	}
	loaded := make([]int, 0, 3)
	for i := currentVolumeIndex - 1; i <= currentVolumeIndex+1; i++ {
		if i >= 0 && i < len(layered) {
			loaded = append(loaded, layered[i].Index)
		}
	}
	return map[string]any{
		"all_volume_outlines":    true,
		"all_arc_outlines":       true,
		"chapter_detail_volumes": loaded,
		"remote_reader":          "read_planning_reference",
		"max_reader_calls":       2,
		"reader_hint":            "只有确需远处卷 chapters[] 或 Long Reference 时才调用；多个卷号一次批量请求",
	}
}

// planningVolumeSummaryView 明确保留所有卷摘要的完整结构。调用方使用
// canonical planning_memory，不再靠顶层副本消耗第二份预算。
func planningVolumeSummaryView(summaries []domain.VolumeSummary) []domain.VolumeSummary {
	return slices.Clone(summaries)
}

func (t *ContextTool) completionSignals(layered []domain.VolumeOutline, compass *domain.StoryCompass) map[string]any {
	signals := map[string]any{}
	if progress, _ := t.store.Progress.Load(); progress != nil {
		signals["completed_chapters"] = len(progress.CompletedChapters)
		signals["total_word_count"] = progress.TotalWordCount
		signals["phase"] = string(progress.Phase)
	}
	if len(layered) > 0 {
		signals["planned_chapters"] = len(domain.FlattenOutline(layered))
		signals["volumes_total"] = len(layered)
		if fv := domain.FinaleVolume(layered); fv > 0 {
			signals["final_volume"] = fv
		}
	}
	if compass != nil {
		if compass.Long.EstimatedScale != "" {
			signals["compass_estimated_scale"] = compass.Long.EstimatedScale
		}
		signals["long_open_threads_count"] = len(compass.Long.OpenThreads)
		if compass.Current != nil {
			signals["current_open_threads_count"] = len(compass.Current.OpenThreads)
		}
	}
	if active, err := t.store.World.LoadActiveForeshadow(); err == nil {
		signals["active_foreshadow_count"] = len(active)
	}
	return signals
}

func (t *ContextTool) buildArchitectFoundation(envelope *architectContextEnvelope, warn func(string, error)) {
	if premise, err := t.store.Outline.LoadPremise(); err == nil && premise != "" {
		if sections := parsePremiseSections(premise); len(sections) > 0 {
			envelope.Foundation["premise_sections"] = sections
		}
		tier := domain.PlanningTier("")
		if meta, err := t.store.RunMeta.Load(); err == nil && meta != nil {
			tier = meta.PlanningTier
		}
		envelope.Foundation["premise_structure"] = premiseStructure(premise, tier)
	} else {
		warn("premise", err)
	}

	if chars, err := t.store.Characters.Load(); err == nil && chars != nil {
		envelope.Foundation["characters"] = chars
	} else {
		warn("characters", err)
	}

	if snapshots, err := t.store.Characters.LoadLatestSnapshots(); err == nil && len(snapshots) > 0 {
		envelope.Foundation["character_snapshots"] = snapshots
	} else {
		warn("character_snapshots", err)
	}
	if rules, err := t.store.World.LoadWorldRules(); err == nil && len(rules) > 0 {
		envelope.Foundation["world_rules"] = rules
	} else {
		warn("world_rules", err)
	}
	if foreshadow, err := t.store.World.LoadActiveForeshadow(); err == nil && len(foreshadow) > 0 {
		envelope.Foundation["foreshadow_ledger"] = foreshadow
	} else {
		warn("foreshadow_ledger", err)
	}
	envelope.Foundation["foundation_status"] = t.foundationStatus()
	// Writer 反馈池:commit_chapter 落盘的大纲偏离/建议,规划下一弧/卷时必须参考;
	// expand_arc / append_volume / update_compass 成功后自动清空(已消费)。
	if fbs := t.store.Outline.LoadPendingOutlineFeedback(); len(fbs) > 0 {
		envelope.Foundation["writer_feedback"] = fbs
	}
}

func (t *ContextTool) buildArchitectReferences(envelope *architectContextEnvelope, warn func(string, error)) {
	// Architect 只注入规划 outline 规则，不注入 prose/dialogue/taboos（避免半个 Writer）
	// 非 architect 角色走 chapter 路径时不注入 outline 规则。
	if t.role == "architect" {
		if compass, err := t.store.World.LoadStyleRulesCompass(); err == nil && compass != nil && compass.HasOutlineContent() {
			if view := buildCompassOutlineInjectionView(compass); view != nil {
				envelope.References["style_rules"] = view
			}
		} else if err != nil {
			warn("style_rules_compass", err)
		}
	}

	envelope.References["references"] = t.architectReferences()
}

// safeMaxCompleted 是 maxCompletedChapter 的 nil-safe 封装。
// progress 为 nil 时返回 0，不 panic。
func safeMaxCompleted(progress *domain.Progress) int {
	if progress == nil {
		return 0
	}
	return maxCompletedChapter(progress.CompletedChapters)
}

// buildArchiveRefsProjection 解析 compass.long.open_threads 中的 [room:<id>] markers，
// 在 planning_memory.archive_refs 中生成轻量投影，供 ArchitectLong 判断何时调用
// read_planning_archive 工具。所有异常只写 warning，不使上下文失败。
func (t *ContextTool) buildArchiveRefsProjection(envelope *architectContextEnvelope, warn func(string, error)) {
	if t.role != "architect" {
		return
	}

	compass, err := t.store.Outline.LoadCompass()
	if err != nil {
		warn("compass_for_archive_refs", err)
		return
	}
	if compass == nil || len(compass.Long.OpenThreads) == 0 {
		return
	}

	refs, parsed, parseErrs := ExtractPlanningRefsWithErrors(compass.Long.OpenThreads)
	if len(refs) == 0 && len(parseErrs) > 0 {
		// 全部解析失败：写 warning，投影仍在（含原始线程摘要）
		for _, pe := range parseErrs {
			warn("malformed_open_thread", fmt.Errorf("thread[%d]: %s (text: %q)", pe.Index, pe.Err, pe.Text))
		}
	}
	if len(refs) == 0 && len(parseErrs) == 0 {
		return
	}

	// 收集 warning：为每个解析失败的条目写一条
	var warnings []string
	for _, pe := range parseErrs {
		warnings = append(warnings, fmt.Sprintf("thread[%d] 解析失败: %s", pe.Index, pe.Err))
	}

	// 检查 archive 是否存在（不做深度校验——archive 不可用时仅影响可用性标记）
	archiveStatus := "unknown"
	archive, loadErr := t.store.PlanningArchive.Load()
	if loadErr != nil {
		archiveStatus = "error"
		warn("planning_archive_for_refs", loadErr)
	} else if archive == nil {
		archiveStatus = "absent"
	} else if archive.Schema != "ainovel.planning-archive" || archive.Version != 1 {
		archiveStatus = "unsupported"
	} else if err := archive.Validate(); err != nil {
		archiveStatus = "invalid"
		warn("planning_archive_validation", fmt.Errorf("archive 校验失败: %w", err))
	} else {
		archiveStatus = "available"
	}

	// 检查已请求的 ref 在 archive 中的可用性（仅标记存在性，不注入 entry data）
	// 使用 archiveEntryKey（NUL 分隔）防止 (room/foo,bar) 与 (room,foo/bar) 碰撞
	var availableRefs, missingRefs []PlanningRef
	if archiveStatus == "available" && archive != nil {
		index := make(map[string]bool, len(archive.Entries))
		for _, e := range archive.Entries {
			index[archiveEntryKey(e.Kind, e.ID)] = true
		}
		for _, r := range refs {
			if index[archiveEntryKey(r.Kind, r.ID)] {
				availableRefs = append(availableRefs, r)
			} else {
				missingRefs = append(missingRefs, r)
			}
		}
	}

	projection := map[string]any{
		"archive_status": archiveStatus,
		"marked_threads": buildMarkedThreadsView(parsed),
	}
	if len(refs) > 0 {
		projection["parsed_refs"] = refs
	}
	if len(availableRefs) > 0 {
		projection["available_refs"] = availableRefs
	}
	if len(missingRefs) > 0 {
		projection["missing_refs"] = missingRefs
	}
	if len(warnings) > 0 {
		projection["_warnings"] = warnings
	}
	projection["reader_hint"] = "使用 read_planning_archive 工具按 kind+id 读取上述 refs 的存档数据；一次最多请求 8 条"

	envelope.Planning["archive_refs"] = projection
}

// buildMarkedThreadsView 将已解析的 open_threads 转为可读的线程摘要列表。
// 绝不注入 archive entry data（仅展示摘要与 room refs）。
func buildMarkedThreadsView(parsed []ParsedOpenThread) []map[string]any {
	view := make([]map[string]any, 0, len(parsed))
	for _, p := range parsed {
		item := map[string]any{
			"summary": p.NaturalSummary,
		}
		if p.ParseError != "" {
			item["parse_error"] = p.ParseError
		}
		if len(p.RoomIDs) > 0 {
			item["room_refs"] = p.RoomIDs
		}
		view = append(view, item)
	}
	return view
}
