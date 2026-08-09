package ctxpack

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/voocel/agentcore"
	corecontext "github.com/voocel/agentcore/context"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

const defaultStoreSummaryBudgetTokens = 10000

type writerStoreSummaryState struct {
	progress          *domain.Progress
	chapter           int
	currentOutline    *domain.OutlineEntry
	chapterPlan       *domain.ChapterPlan
	recentSummaries   []domain.ChapterSummary
	currentArcSummary *domain.ArcSummary
	currentVolSummary *domain.VolumeSummary
	snapshots         []domain.CharacterSnapshot
	foreshadow        []domain.ForeshadowEntry
	characterState    []domain.CharacterStateEntry
	worldRules        []domain.WorldRule
	timeline          []domain.TimelineEvent
	styleRules        *domain.WritingStyleRules
	pendingReviews    []writerPendingReview
}

type writerPendingReview struct {
	Chapter        int                 `json:"chapter"`
	Scope          string              `json:"scope"`
	Verdict        string              `json:"verdict"`
	Summary        string              `json:"summary,omitempty"`
	ContractMisses []string            `json:"contract_misses,omitempty"`
	Issues         []writerReviewIssue `json:"issues,omitempty"`
}

type writerReviewIssue struct {
	Type        string `json:"type,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion,omitempty"`
}

func buildWriterStoreSummaryText(s *store.Store, budgetTokens int) (string, bool, error) {
	state, ok, err := loadWriterStoreSummaryState(s)
	if err != nil || !ok {
		return "", ok, err
	}
	if budgetTokens <= 0 {
		budgetTokens = defaultStoreSummaryBudgetTokens
	}
	sections := writerStoreSummarySections(state)
	// 渲染前预扣前缀与段间分隔符开销（正式估算器），保证最终完整文本落入预算
	overhead := corecontext.EstimateTokens(agentcore.UserMsg("以下内容来自小说持久化 store，用于在压缩后恢复写作上下文。\n\n"))
	if len(sections) > 1 {
		overhead += (len(sections) - 1) * 2 // 段间 "\n\n" 保守余量
	}
	wrap := func(parts []string) string {
		return "以下内容来自小说持久化 store，用于在压缩后恢复写作上下文。\n\n" + strings.Join(parts, "\n\n")
	}
	full, ok := renderStoreTextWithFinalCheck(state, budgetTokens, overhead, sections, wrap)
	return full, ok, nil
}

func buildWriterRestoreText(s *store.Store, budgetTokens int) (string, bool, error) {
	state, ok, err := loadWriterStoreSummaryState(s)
	if err != nil {
		return "", false, err
	}
	if !ok && s != nil {
		state, err = loadWriterRestoreState(s)
		if err != nil {
			return "", false, err
		}
	}
	if state == nil {
		return "", false, nil
	}
	if budgetTokens <= 0 {
		budgetTokens = restoreBudgetTokens
	}
	sections := writerRestoreSections(state)
	// 渲染前预扣 wrapper 与段间分隔符开销（正式估算器），保证最终完整文本
	// （含 <post-compact-context> wrapper）落入预算，避免 buildMessage 拒绝。
	overhead := corecontext.EstimateTokens(agentcore.UserMsg("<post-compact-context>\n\n</post-compact-context>"))
	if len(sections) > 1 {
		overhead += (len(sections) - 1) * 2 // 段间 "\n\n" 保守余量
	}
	wrap := func(parts []string) string {
		return "<post-compact-context>\n" + strings.Join(parts, "\n\n") + "\n</post-compact-context>"
	}
	full, ok := renderStoreTextWithFinalCheck(state, budgetTokens, overhead, sections, wrap)
	return full, ok, nil
}

// renderStoreTextWithFinalCheck 渲染 sections 并对最终完整文本做预算兜底：
// 分项估算非严格可加（估算器按完整文本重新判断 CJK/ASCII dominance），完整文本
// 可能超出预算——按超出比例收缩预算重渲染（最多 3 轮）；仍超则返回 false，
// 不把超预算结果交给 buildMessage（避免恢复包被整体拒绝）。
func renderStoreTextWithFinalCheck(state *writerStoreSummaryState, budgetTokens, overhead int, sections []writerStoreSection, wrap func([]string) string) (string, bool) {
	parts := renderWriterStoreSections(state, budgetTokens-overhead, sections)
	if len(parts) == 0 {
		return "", false
	}
	full := wrap(parts)
	for attempt := 0; attempt < 3 && corecontext.EstimateTokens(agentcore.UserMsg(full)) > budgetTokens; attempt++ {
		cur := corecontext.EstimateTokens(agentcore.UserMsg(full))
		ratio := float64(budgetTokens) / float64(cur)
		newBudget := int(float64(budgetTokens-overhead)*ratio*0.95) + overhead
		if newBudget <= overhead {
			return "", false
		}
		parts = renderWriterStoreSections(state, newBudget-overhead, sections)
		if len(parts) == 0 {
			return "", false
		}
		full = wrap(parts)
	}
	if corecontext.EstimateTokens(agentcore.UserMsg(full)) > budgetTokens {
		return "", false
	}
	return full, true
}

func loadWriterStoreSummaryState(s *store.Store) (*writerStoreSummaryState, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	progress, err := s.Progress.Load()
	if err != nil || progress == nil {
		return nil, false, err
	}

	chapter := progress.CurrentChapter
	if progress.InProgressChapter > 0 {
		chapter = progress.InProgressChapter
	}
	if chapter <= 1 {
		return nil, false, nil
	}

	profile := domain.NewContextProfile(progress.TotalChapters)
	if !progress.Layered {
		profile.Layered = false
	}

	state := &writerStoreSummaryState{
		progress: progress,
		chapter:  chapter,
	}

	state.chapterPlan, err = s.Drafts.LoadChapterPlan(chapter)
	if err != nil {
		return nil, false, err
	}
	state.currentOutline, err = s.Outline.GetChapterOutline(chapter)
	if err != nil {
		state.currentOutline = nil
	}
	if state.currentOutline == nil {
		if outline, layeredErr := s.Outline.GetChapterFromLayered(chapter); layeredErr == nil {
			state.currentOutline = outline
		}
	}

	state.recentSummaries, err = s.Summaries.LoadRecentSummaries(chapter, profile.SummaryWindow)
	if err != nil {
		return nil, false, err
	}
	state.snapshots, err = s.Characters.LoadLatestSnapshots()
	if err != nil {
		return nil, false, err
	}
	state.foreshadow, err = s.World.LoadActiveForeshadow()
	if err != nil {
		return nil, false, err
	}
	state.characterState, err = s.World.LoadCharacterState()
	if err != nil {
		return nil, false, err
	}
	state.worldRules, err = s.World.LoadWorldRules()
	if err != nil {
		return nil, false, err
	}
	state.timeline, err = s.World.LoadRecentTimeline(chapter, profile.TimelineWindow)
	if err != nil {
		return nil, false, err
	}
	state.styleRules, err = s.World.LoadStyleRules()
	if err != nil {
		return nil, false, err
	}
	state.pendingReviews, err = loadPendingReviewsForStoreState(s, chapter)
	if err != nil {
		return nil, false, err
	}

	loadLayeredSummariesForStoreState(s, progress, chapter, state)

	hasSummaries := len(state.recentSummaries) > 0 || state.currentArcSummary != nil || state.currentVolSummary != nil
	hasWorkingState := state.chapterPlan != nil || state.currentOutline != nil
	if !hasSummaries || !hasWorkingState {
		return nil, false, nil
	}
	return state, true, nil
}

func loadWriterRestoreState(s *store.Store) (*writerStoreSummaryState, error) {
	if s == nil {
		return nil, nil
	}
	progress, err := s.Progress.Load()
	if err != nil || progress == nil {
		return nil, err
	}

	chapter := progress.CurrentChapter
	if progress.InProgressChapter > 0 {
		chapter = progress.InProgressChapter
	}
	if chapter <= 0 {
		return nil, nil
	}

	profile := domain.NewContextProfile(progress.TotalChapters)
	if !progress.Layered {
		profile.Layered = false
	}

	state := &writerStoreSummaryState{
		progress: progress,
		chapter:  chapter,
	}
	state.chapterPlan, _ = s.Drafts.LoadChapterPlan(chapter)
	state.currentOutline, _ = s.Outline.GetChapterOutline(chapter)
	if state.currentOutline == nil {
		state.currentOutline, _ = s.Outline.GetChapterFromLayered(chapter)
	}
	state.snapshots, _ = s.Characters.LoadLatestSnapshots()
	state.foreshadow, _ = s.World.LoadActiveForeshadow()
	state.characterState, _ = s.World.LoadCharacterState()
	state.worldRules, _ = s.World.LoadWorldRules()
	state.pendingReviews, _ = loadPendingReviewsForStoreState(s, chapter)
	state.styleRules, _ = s.World.LoadStyleRules()
	state.timeline, _ = s.World.LoadRecentTimeline(chapter, profile.TimelineWindow)
	if chapter > 1 {
		state.recentSummaries, _ = s.Summaries.LoadRecentSummaries(chapter, min(profile.SummaryWindow, 2))
	}
	loadLayeredSummariesForStoreState(s, progress, chapter, state)
	if isEmptySummarySection(state.chapterPlan) &&
		isEmptySummarySection(state.currentOutline) &&
		isEmptySummarySection(state.snapshots) &&
		isEmptySummarySection(state.pendingReviews) &&
		isEmptySummarySection(state.recentSummaries) &&
		isEmptySummarySection(state.foreshadow) &&
		isEmptySummarySection(state.characterState) &&
		isEmptySummarySection(state.worldRules) {
		return nil, nil
	}
	return state, nil
}

type writerStoreSection struct {
	heading string
	data    any
	// canon 表示关键状态段（计划/大纲/CharacterState/伏笔/WorldRules）：
	// 渲染分两轮——canon 段先各自按独立预算份额渲染，某段超份额时独立
	// 截断，不阻止后续 canon 段；optional 段（进度/摘要/快照/时间线/风格
	// 规则等）第二轮消费剩余预算。
	canon bool
}

// progressCompletedTail 进度段仅保留最近 N 章 completed_chapters 明细。
// 长书（数千章）的完整明细会独占上下文预算，且对续写无增量信息——
// completed_count 已给出总量，明细只需覆盖最近章节用于衔接。
const progressCompletedTail = 5

func writerStoreProgressSection(state *writerStoreSummaryState) map[string]any {
	if state == nil || state.progress == nil {
		return nil
	}
	completed := state.progress.CompletedChapters
	if len(completed) > progressCompletedTail {
		completed = completed[len(completed)-progressCompletedTail:]
	}
	return map[string]any{
		"phase":               state.progress.Phase,
		"flow":                state.progress.Flow,
		"current_chapter":     state.chapter,
		"completed_chapters":  completed,
		"completed_count":     len(state.progress.CompletedChapters),
		"current_volume":      state.progress.CurrentVolume,
		"current_arc":         state.progress.CurrentArc,
		"in_progress_chapter": state.progress.InProgressChapter,
	}
}

// writerStoreSummarySections 列出全部输出段。渲染分两轮（见
// renderWriterStoreSections）：canon 段（当前章节计划/当前章节大纲/角色
// 受控状态/活跃伏笔/世界规则）各自保留预算份额，任意单个段（如 24KiB
// 的 WorldRules）不能独占余量导致其他 canon 段丢失；optional 段（进度/
// 待修问题/摘要/快照/时间线/风格规则）第二轮消费剩余预算。
func writerStoreSummarySections(state *writerStoreSummaryState) []writerStoreSection {
	return []writerStoreSection{
		{heading: "当前进度", data: writerStoreProgressSection(state)},
		{heading: "当前章节计划", data: state.chapterPlan, canon: true},
		{heading: "当前章节大纲", data: state.currentOutline, canon: true},
		{heading: "角色受控状态", data: state.characterState, canon: true},
		{heading: "活跃伏笔", data: state.foreshadow, canon: true},
		{heading: "世界规则", data: state.worldRules, canon: true},
		{heading: "待修审稿问题", data: state.pendingReviews},
		{heading: "最近章节摘要", data: state.recentSummaries},
		{heading: "当前弧摘要", data: state.currentArcSummary},
		{heading: "当前卷摘要", data: state.currentVolSummary},
		{heading: "角色快照", data: state.snapshots},
		{heading: "最近时间线", data: state.timeline},
		{heading: "风格规则", data: state.styleRules},
	}
}

func writerRestoreSections(state *writerStoreSummaryState) []writerStoreSection {
	return []writerStoreSection{
		{heading: "当前进度", data: writerStoreProgressSection(state)},
		{heading: "当前章节计划", data: state.chapterPlan, canon: true},
		{heading: "当前章节大纲", data: state.currentOutline, canon: true},
		{heading: "角色受控状态", data: state.characterState, canon: true},
		{heading: "活跃伏笔", data: state.foreshadow, canon: true},
		{heading: "世界规则", data: state.worldRules, canon: true},
		{heading: "待修审稿问题", data: state.pendingReviews},
		{heading: "最近章节摘要", data: state.recentSummaries},
		{heading: "角色快照", data: state.snapshots},
		{heading: "当前弧摘要", data: state.currentArcSummary},
		{heading: "当前卷摘要", data: state.currentVolSummary},
		{heading: "最近时间线", data: state.timeline},
		{heading: "风格规则", data: state.styleRules},
	}
}

// renderWriterStoreSections 分两轮渲染：
//   - 第一轮：canon 段。每个 canon 段按"剩余预算/剩余段数"取独立份额，
//     段实际占用小于份额时余量回池；超份额时独立截断到份额内（标记
//     [已截断]）并继续渲染后续 canon 段——任何单个段都不能独占全部余量。
//   - 第二轮：optional 段按列表顺序消费剩余预算，首个超限段截断并停止
//     （保持原"截断即止"语义）。
func renderWriterStoreSections(state *writerStoreSummaryState, budgetTokens int, sections []writerStoreSection) []string {
	if state == nil || len(sections) == 0 || budgetTokens <= 0 {
		return nil
	}
	var canon, optional []writerStoreSection
	for _, sec := range sections {
		if isEmptySummarySection(sec.data) {
			continue
		}
		if sec.canon {
			canon = append(canon, sec)
		} else {
			optional = append(optional, sec)
		}
	}

	parts := make([]string, 0, len(sections))
	remaining := budgetTokens

	// 第一轮：canon 段，各自独立份额。
	for i, sec := range canon {
		if remaining <= 0 {
			break
		}
		share := remaining / (len(canon) - i)
		used := appendCanonSection(&parts, sec.heading, sec.data, share)
		remaining -= used
	}

	// 第二轮：optional 段消费剩余预算。
	for _, sec := range optional {
		if remaining <= 0 {
			break
		}
		stop := appendOptionalSection(&parts, sec.heading, sec.data, &remaining)
		if stop {
			break
		}
	}
	return parts
}

// appendCanonSection 以独立份额渲染 canon 段：tokens ≤ share 时完整输出；
// 超份额时按正式估算器截断到份额内并标记 [已截断]。返回该段实际消耗的
// token 数（真实估算，保证不侵占后续 canon 段份额）。
func appendCanonSection(parts *[]string, heading string, data any, share int) int {
	if parts == nil || share <= 0 {
		return 0
	}
	b, err := json.Marshal(data)
	if err != nil {
		return 0
	}
	text := string(b)
	tokens := estimateCompactSectionTokens(heading, text)
	if tokens <= share {
		*parts = append(*parts, fmt.Sprintf("## %s\n%s", heading, text))
		return tokens
	}
	truncated, used := truncateSectionToBudget(heading, text, " [已截断]", share)
	if truncated == "" {
		return 0
	}
	*parts = append(*parts, truncated)
	return used
}

// appendOptionalSection 渲染 optional 段：超剩余预算时截断并停止（返回 true），
// 与旧 appendJSONSection 的非 canon 语义一致。
func appendOptionalSection(parts *[]string, heading string, data any, remaining *int) bool {
	if parts == nil || remaining == nil || *remaining <= 0 {
		return true
	}
	b, err := json.Marshal(data)
	if err != nil {
		return false
	}
	text := string(b)
	tokens := estimateCompactSectionTokens(heading, text)
	if tokens > *remaining {
		if *remaining <= 100 {
			return true
		}
		truncated, used := truncateSectionToBudget(heading, text, " [已截断]", *remaining)
		if truncated == "" {
			return true
		}
		*parts = append(*parts, truncated)
		*remaining -= used
		return true
	}
	*parts = append(*parts, fmt.Sprintf("## %s\n%s", heading, text))
	*remaining -= tokens
	return false
}

// truncateSectionToBudget 对完整 section 文本（heading+正文+标记）用正式估算器
// （corecontext.EstimateTokens，CJK runes*1.5）做 rune 安全二分截断，返回截断后
// 文本与真实估算 token 数。heading 本身超预算时降级截断 heading；完全放不下返回空。
func truncateSectionToBudget(heading, body, marker string, budgetTokens int) (string, int) {
	if budgetTokens <= 0 {
		return "", 0
	}
	prefix := "## " + heading + "\n"
	full := prefix + body
	if corecontext.EstimateTokens(agentcore.UserMsg(full)) <= budgetTokens {
		return full, corecontext.EstimateTokens(agentcore.UserMsg(full))
	}
	// heading 本身是否放得下
	if corecontext.EstimateTokens(agentcore.UserMsg(prefix)) > budgetTokens {
		rs := []rune(prefix)
		lo, hi := 0, len(rs)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if corecontext.EstimateTokens(agentcore.UserMsg(string(rs[:mid]))) <= budgetTokens {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo == 0 {
			return "", 0
		}
		out := string(rs[:lo])
		return out, corecontext.EstimateTokens(agentcore.UserMsg(out))
	}
	// 最小候选（无正文 + 截断标记）检查：heading 可容纳但 heading+marker 不可容纳时，
	// 正文二分无合法候选（best=0 也超预算）——必须降级 heading-only，否则返回超预算文本。
	minimum := prefix + marker
	if corecontext.EstimateTokens(agentcore.UserMsg(minimum)) > budgetTokens {
		return prefix, corecontext.EstimateTokens(agentcore.UserMsg(prefix))
	}
	// 二分正文 rune 数：prefix + body[:n] + marker ≤ budgetTokens
	bodyRunes := []rune(body)
	best := 0
	lo, hi := 0, len(bodyRunes)
	for lo <= hi {
		mid := (lo + hi) / 2
		cand := prefix + string(bodyRunes[:mid]) + marker
		if corecontext.EstimateTokens(agentcore.UserMsg(cand)) <= budgetTokens {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	out := prefix + string(bodyRunes[:best]) + marker
	used := corecontext.EstimateTokens(agentcore.UserMsg(out))
	if used > budgetTokens {
		// 最终保险：估算器非严格可加，极端情况下仍可能超——降级 heading-only
		return prefix, corecontext.EstimateTokens(agentcore.UserMsg(prefix))
	}
	return out, used
}

func loadPendingReviewsForStoreState(s *store.Store, chapter int) ([]writerPendingReview, error) {
	if s == nil || chapter <= 1 {
		return nil, nil
	}
	start := max(chapter-3, 1)
	pending := make([]writerPendingReview, 0, 4)
	for ch := chapter - 1; ch >= start; ch-- {
		review, err := s.World.LoadReview(ch)
		if err != nil {
			return nil, err
		}
		if compact, ok := compactPendingReview(review); ok {
			pending = append(pending, compact)
		}
	}
	global, err := s.World.LoadLastReview(chapter - 1)
	if err != nil {
		return nil, err
	}
	if compact, ok := compactPendingReview(global); ok {
		alreadyIncluded := false
		for _, item := range pending {
			if item.Chapter == compact.Chapter && item.Scope == compact.Scope {
				alreadyIncluded = true
				break
			}
		}
		if !alreadyIncluded {
			pending = append(pending, compact)
		}
	}
	return pending, nil
}

func compactPendingReview(review *domain.ReviewEntry) (writerPendingReview, bool) {
	if review == nil {
		return writerPendingReview{}, false
	}
	if review.Verdict == "accept" && len(review.Issues) == 0 && len(review.ContractMisses) == 0 {
		return writerPendingReview{}, false
	}
	item := writerPendingReview{
		Chapter: review.Chapter,
		Scope:   review.Scope,
		Verdict: review.Verdict,
		Summary: review.Summary,
	}
	if len(review.ContractMisses) > 0 {
		item.ContractMisses = append([]string(nil), review.ContractMisses[:min(len(review.ContractMisses), 5)]...)
	}
	if len(review.Issues) > 0 {
		limit := min(len(review.Issues), 5)
		item.Issues = make([]writerReviewIssue, 0, limit)
		for _, issue := range review.Issues[:limit] {
			item.Issues = append(item.Issues, writerReviewIssue{
				Type:        issue.Type,
				Severity:    issue.Severity,
				Description: issue.Description,
				Suggestion:  issue.Suggestion,
			})
		}
	}
	return item, true
}

func loadLayeredSummariesForStoreState(s *store.Store, progress *domain.Progress, chapter int, state *writerStoreSummaryState) {
	if s == nil || progress == nil || state == nil {
		return
	}
	volume, arc := progress.CurrentVolume, progress.CurrentArc
	if volume <= 0 || arc <= 0 {
		if v, a, err := s.Outline.LocateChapter(chapter); err == nil {
			volume, arc = v, a
		} else if v, a, err := s.Outline.LocateChapter(max(chapter-1, 1)); err == nil {
			volume, arc = v, a
		}
	}
	if volume <= 0 {
		return
	}
	if sum, err := s.Summaries.LoadVolumeSummary(volume); err == nil {
		state.currentVolSummary = sum
	}
	if arc > 0 {
		if sum, err := s.Summaries.LoadArcSummary(volume, arc); err == nil {
			state.currentArcSummary = sum
		}
	}
}

func estimateCompactSectionTokens(heading, body string) int {
	section := fmt.Sprintf("## %s\n%s", heading, body)
	return corecontext.EstimateTokens(agentcore.UserMsg(section))
}

func isEmptySummarySection(data any) bool {
	if data == nil {
		return true
	}
	rv := reflect.ValueOf(data)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	case reflect.Slice, reflect.Map, reflect.Array, reflect.String:
		return rv.Len() == 0
	default:
		return false
	}
}
