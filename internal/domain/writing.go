package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ChapterPlan 章节写作构思，Writer 自主生成。
// 不再强制场景拆分，Agent 自己决定如何组织内容。
type ChapterPlan struct {
	Chapter    int             `json:"chapter"`
	Title      string          `json:"title"`
	Goal       string          `json:"goal"`
	Conflict   string          `json:"conflict"`
	Hook       string          `json:"hook"`
	EmotionArc string          `json:"emotion_arc,omitempty"`
	Notes      string          `json:"notes,omitempty"` // Agent 的自由备忘
	Contract   ChapterContract `json:"contract,omitempty"`
}

// ChapterContract 是 Writer 和 Editor 共享的章节验收契约。
// 它定义本章必须完成的推进项、禁止越界项以及审阅关注点。
type ChapterContract struct {
	RequiredBeats    []string `json:"required_beats,omitempty"`    // 本章必须落地的推进项
	ForbiddenMoves   []string `json:"forbidden_moves,omitempty"`   // 本章明确不能发生的推进
	ContinuityChecks []string `json:"continuity_checks,omitempty"` // 本章需特别核对的连续性点
	EvaluationFocus  []string `json:"evaluation_focus,omitempty"`  // Editor 需要重点检查的点
	EmotionTarget    string   `json:"emotion_target,omitempty"`    // 可选：本章希望读者主要感受到的情绪
	PayoffPoints     []string `json:"payoff_points,omitempty"`     // 可选：关键章希望回应的情节点/兑现点
	HookGoal         string   `json:"hook_goal,omitempty"`         // 可选：章末钩子希望驱动的追读欲望
}

// ChapterSummary 章节摘要，供后续章节的上下文窗口使用。
type ChapterSummary struct {
	Chapter    int      `json:"chapter"`
	Summary    string   `json:"summary"`
	Characters []string `json:"characters"`
	KeyEvents  []string `json:"key_events"`
}

// ArcSummary 弧级摘要，弧结束时由 Editor 生成。
type ArcSummary struct {
	Volume    int      `json:"volume"`
	Arc       int      `json:"arc"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	KeyEvents []string `json:"key_events"`
}

// VolumeSummary 卷级摘要，卷结束时生成。
type VolumeSummary struct {
	Volume    int      `json:"volume"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	KeyEvents []string `json:"key_events"`
}

// CharacterSnapshot 角色状态快照，弧边界时记录。
type CharacterSnapshot struct {
	Volume     int    `json:"volume"`
	Arc        int    `json:"arc"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Power      string `json:"power,omitempty"`
	Motivation string `json:"motivation"`
	Relations  string `json:"relations,omitempty"`
}

// OutlineFeedback Writer 对大纲的反馈，提交章节时可选。
type OutlineFeedback struct {
	Deviation  string `json:"deviation"`  // 偏离描述
	Suggestion string `json:"suggestion"` // 调整建议
}

// WritingStyleRules 从已写章节中提炼的写作规则，弧边界时由 Editor 生成。
// 取代原文片段（style_anchors / voice_samples），用规则替代搬运原文。
type WritingStyleRules struct {
	Volume    int              `json:"volume"`
	Arc       int              `json:"arc"`
	Prose     []string         `json:"prose"`      // 3-5 条叙述风格规则，每条 ≤50 字
	Dialogue  []CharacterVoice `json:"dialogue"`   // 角色对话风格规则
	Taboos    []string         `json:"taboos"`     // 禁忌清单
	UpdatedAt string           `json:"updated_at"` // ISO8601 时间戳
}

// CharacterVoice 单个角色的对话风格规则。
type CharacterVoice struct {
	Name  string   `json:"name"`
	Rules []string `json:"rules"` // 2-3 条语言特征规则，每条 ≤30 字
}

// ── 双层 StyleRules Compass（轻量模式） ──

// StyleRulesLong 跨弧基线风格规则（Compass 长期层）。
// 长期规则是全书统一的风格基准，不包含局部装置名、循环次数、积分算法、具体参数。
// 更新必须显式给出 reason 且做字段级合并。
type StyleRulesLong struct {
	Prose       []string         `json:"prose,omitempty"`    // 3-5 条叙述风格规则，每条 ≤50 字
	Dialogue    []CharacterVoice `json:"dialogue,omitempty"` // 角色对话风格规则，跨弧稳定
	Taboos      []string         `json:"taboos,omitempty"`   // 全书通用禁忌
	Reason      string           `json:"reason,omitempty"`   // 上次更新原因（审计元数据）
	LastUpdated string           `json:"last_updated,omitempty"` // ISO8601 更新时间
}

// Validate 检查 long 规则是否含有禁止的局部实现细节。
// 禁止项：循环次数、积分算法、具体参数阈值等不应放入长期基线的内容。
// 验证所有文本（含 prose、taboos、dialogue 规则）。
// 返回可读的拒绝理由；nil 表示合法。
func (s *StyleRulesLong) Validate() error {
	// 只限制确属"局部实现细节"的指标性关键词，避免误杀通用写作术语。
	// 例如"武器""装备""法器""循环"在小说语境下过于常见，不拦截。
	forbidden := []struct {
		pattern string
		reason  string
	}{
		{"第.*轮", "长期规则不应指定轮次/次数（属于当前弧章节细节）"},
		{"循环次数", "长期规则不应指定循环次数（属于当前弧章节细节）"},
		{"积分算法", "长期规则不应包含积分/分数算法（属于当前弧章节细节）"},
		{"阈值", "长期规则不应包含具体参数阈值（属于当前弧细节）"},
		{"参数配置", "长期规则不应包含具体参数配置（属于当前弧细节）"},
	}
	// 收集所有文本（含 dialogue 规则）
	var allText string
	allText += strings.Join(s.Prose, "\n") + "\n"
	allText += strings.Join(s.Taboos, "\n") + "\n"
	for _, d := range s.Dialogue {
		allText += strings.Join(d.Rules, "\n") + "\n"
	}
	for _, f := range forbidden {
		if matched, _ := regexp.MatchString(f.pattern, allText); matched {
			return fmt.Errorf("style_rules.long 包含禁止内容: %s", f.reason)
		}
	}
	return nil
}

// StyleRulesCurrent 当前弧风格规则（Compass 当前层）。
// 由 Editor 在弧边界时生成，仅作用于当前弧。
// 同字段在 long 和 current 都有定义时，long 优先（由上下文注入保证）。
type StyleRulesCurrent struct {
	Volume      int              `json:"volume"`
	Arc         int              `json:"arc"`
	Prose       []string         `json:"prose,omitempty"`
	Dialogue    []CharacterVoice `json:"dialogue,omitempty"`
	Taboos      []string         `json:"taboos,omitempty"`
	LastUpdated string           `json:"last_updated,omitempty"` // ISO8601
}

// WritingStyleRulesCompass 双层风格规则罗盘（持久化到 meta/style_rules.json）。
// long（跨弧基线）+ current（当前弧含 volume/arc/last_updated）。
// 兼容旧单体 WritingStyleRules 格式：加载时自动迁入 current。
type WritingStyleRulesCompass struct {
	Long    *StyleRulesLong    `json:"long,omitempty"`
	Current *StyleRulesCurrent `json:"current,omitempty"`
}

// UnmarshalJSON 兼容旧单体 WritingStyleRules（有 volume/arc/prose 顶层字段）
// 自动迁移到 current 层。新格式直接解析。
func (c *WritingStyleRulesCompass) UnmarshalJSON(data []byte) error {
	// 先尝试新格式
	type compassShape WritingStyleRulesCompass
	var current compassShape
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	if current.Long != nil || current.Current != nil {
		*c = WritingStyleRulesCompass(current)
		return nil
	}

	// 旧格式：迁移到 current
	var legacy WritingStyleRules
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err // 既不是新格式也不是旧格式
	}
	if legacy.Prose == nil && legacy.Dialogue == nil && legacy.Taboos == nil {
		return nil // 空的旧对象 → 空 compass
	}
	c.Current = &StyleRulesCurrent{
		Volume:      legacy.Volume,
		Arc:         legacy.Arc,
		Prose:       legacy.Prose,
		Dialogue:    legacy.Dialogue,
		Taboos:      legacy.Taboos,
		LastUpdated: legacy.UpdatedAt,
	}
	return nil
}

// HasContent 检查 compass 是否包含任何有效规则。
func (c *WritingStyleRulesCompass) HasContent() bool {
	if c == nil {
		return false
	}
	return (c.Long != nil && (len(c.Long.Prose) > 0 || len(c.Long.Dialogue) > 0 || len(c.Long.Taboos) > 0)) ||
		(c.Current != nil && (len(c.Current.Prose) > 0 || len(c.Current.Dialogue) > 0 || len(c.Current.Taboos) > 0))
}

// ConflictsWithLong 保留供外部调用，但不再做文本级冲突拒绝。
// 不同层级的增量规则不属于冲突——long 与 current 的差异由上下文注入的
// long 优先策略保证一致性。返回 nil 表示无硬冲突。
func ConflictsWithLong(current *StyleRulesCurrent, long *StyleRulesLong) error {
	// 当前不做语义级文本矛盾检测。long 与 current 的差异视为
	// 正常迭代，由 long 优先的上下文合并保证风格一致性。
	return nil
}

// HasContent 检查 long 层是否包含任何有效规则。
func (s *StyleRulesLong) HasContent() bool {
	return s != nil && (len(s.Prose) > 0 || len(s.Dialogue) > 0 || len(s.Taboos) > 0)
}

// StyleRulesCurrentsEqual 比较两个 current 指针的等效性（用于条件恢复判定）。
func StyleRulesCurrentsEqual(a, b *StyleRulesCurrent) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Volume != b.Volume || a.Arc != b.Arc || a.LastUpdated != b.LastUpdated {
		return false
	}
	if !stringSlicesEqual(a.Prose, b.Prose) {
		return false
	}
	if !stringSlicesEqual(a.Taboos, b.Taboos) {
		return false
	}
	if len(a.Dialogue) != len(b.Dialogue) {
		return false
	}
	for i := range a.Dialogue {
		if a.Dialogue[i].Name != b.Dialogue[i].Name {
			return false
		}
		if !stringSlicesEqual(a.Dialogue[i].Rules, b.Dialogue[i].Rules) {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RelatedChapter 推荐回读的相关章节。
type RelatedChapter struct {
	Chapter int    `json:"chapter"`
	Reason  string `json:"reason"`
}

// RecallItem 是按当前任务选择性召回的长期信息。
// 它不替代正式工件，只负责把当前轮真正相关的少量历史信息回注给模型。
type RecallItem struct {
	Kind    string `json:"kind"`
	Key     string `json:"key,omitempty"`
	Chapter int    `json:"chapter,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ── StyleAnchors V1 ──

// StyleAnchorAppliesTo 定义锚点适用的章节范围。
type StyleAnchorAppliesTo struct {
	ChapterRanges [][2]int `json:"chapter_ranges,omitempty"` // 目标章范围 [start,end]，OR 语义（任一匹配即适用）
}

// StyleAnchorProvenance 记录锚点的来源元数据。
// 验证：source_chapter ≥ 0，source_digest ≤ 64 字符。
// 注入 LLM 上下文时，provenance 被剔除（审计元数据不由模型消费）。
type StyleAnchorProvenance struct {
	SourceChapter int    `json:"source_chapter,omitempty"` // 来源章节号，≥ 0（0=手动）
	SourceDigest  string `json:"source_digest,omitempty"`  // 来源摘要/指纹，≤ 64 字符
}

// StyleAnchorItem 是 style_anchors.json 中的一条风格锚点。
type StyleAnchorItem struct {
	ID         string                `json:"id"`                   // 唯一标识，TrimSpace 非空，长度 ≤ 64 Unicode 字符
	Excerpt    string                `json:"excerpt"`              // TrimSpace 非空，长度 ≤ 1000 Unicode 字符
	AppliesTo  *StyleAnchorAppliesTo `json:"applies_to,omitempty"` // 可选，目标章范围约束
	Provenance *StyleAnchorProvenance `json:"provenance,omitempty"`// 可选，来源元数据
}

// StyleAnchorsV1 是 v1 格式的风格锚点文件（meta/style_anchors.json）。
type StyleAnchorsV1 struct {
	Version     int               `json:"version"`                // 必须为 1
	Anchors     []StyleAnchorItem `json:"anchors"`               // 锚点列表
	IncludeAuto bool              `json:"include_auto,omitempty"` // true=额外注入 auto 提取锚点（低优先级）
}

// AnchorInjectionItem 是注入 LLM 上下文的精简锚点视图。
// 剔除 provenance（审计元数据）和 applies_to（已用做过滤，模型无需知范围）。
type AnchorInjectionItem struct {
	ID      string `json:"id"`
	Excerpt string `json:"excerpt"`
}

// ToInjectionView 将完整锚点转换为注入视图（仅 id+excerpt），并过滤不匹配当前 chapter 的项。
func (s *StyleAnchorsV1) ToInjectionView(chapter int) []AnchorInjectionItem {
	if s == nil {
		return nil
	}
	var out []AnchorInjectionItem
	for _, a := range s.Anchors {
		if !a.AnchorMatchesChapter(chapter) {
			continue
		}
		out = append(out, AnchorInjectionItem{
			ID:      a.ID,
			Excerpt: a.Excerpt,
		})
	}
	return out
}

// AnchorMatchesChapter 检查锚点是否匹配给定章节号。
// nil AppliesTo 或空 chapter_ranges 视为全局适用（匹配所有章节）。
func (a *StyleAnchorItem) AnchorMatchesChapter(chapter int) bool {
	if a.AppliesTo == nil || len(a.AppliesTo.ChapterRanges) == 0 {
		return true // 无范围约束 = 全局适用
	}
	for _, r := range a.AppliesTo.ChapterRanges {
		if chapter >= r[0] && chapter <= r[1] {
			return true
		}
	}
	return false
}

// Validate 校验 style_anchors v1 文件的完整约束。
// 返回所有发现的校验错误（而非第一个就停），便于调用方一次性展示全部问题。
func (s *StyleAnchorsV1) Validate() []error {
	var errs []error
	add := func(msg string) {
		errs = append(errs, fmt.Errorf("style_anchors: %s", msg))
	}

	if s.Version != 1 {
		add(fmt.Sprintf("version 必须为 1，当前为 %d", s.Version))
	}

	if len(s.Anchors) > 8 {
		add(fmt.Sprintf("anchors 最多 8 项，当前 %d 项", len(s.Anchors)))
	}

	seenIDs := make(map[string]int)
	totalExcerptRunes := 0
	for i, a := range s.Anchors {
		prefix := fmt.Sprintf("anchors[%d]", i)

		// ID: TrimSpace 非空
		id := strings.TrimSpace(a.ID)
		idRunes := utf8.RuneCountInString(id)
		if idRunes == 0 {
			add(fmt.Sprintf("%s.id 不能为空", prefix))
		} else if idRunes > 64 {
			add(fmt.Sprintf("%s.id 长度 %d 超过上限 64 个字符", prefix, idRunes))
		}
		if prev, dup := seenIDs[id]; dup {
			add(fmt.Sprintf("%s.id %q 与 anchors[%d] 重复", prefix, id, prev))
		} else if id != "" {
			seenIDs[id] = i
		}

		// Excerpt: TrimSpace 非空
		excerpt := strings.TrimSpace(a.Excerpt)
		excerptRunes := utf8.RuneCountInString(excerpt)
		if excerptRunes == 0 {
			add(fmt.Sprintf("%s.excerpt 不能为空", prefix))
		} else if excerptRunes > 1000 {
			add(fmt.Sprintf("%s.excerpt 长度 %d 超过上限 1000 个字符", prefix, excerptRunes))
		}
		totalExcerptRunes += excerptRunes

		// AppliesTo.chapter_ranges 校验
		if a.AppliesTo != nil {
			if len(a.AppliesTo.ChapterRanges) > 4 {
				add(fmt.Sprintf("%s.applies_to.chapter_ranges 最多 4 个区间，当前 %d 个", prefix, len(a.AppliesTo.ChapterRanges)))
			}
			for j, r := range a.AppliesTo.ChapterRanges {
				if r[0] <= 0 || r[1] <= 0 || r[0] > r[1] {
					add(fmt.Sprintf("%s.applies_to.chapter_ranges[%d] 无效区间 [%d,%d]", prefix, j, r[0], r[1]))
				}
			}
		}

		// Provenance 校验
		if a.Provenance != nil {
			if a.Provenance.SourceChapter < 0 {
				add(fmt.Sprintf("%s.provenance.source_chapter 不能为负数", prefix))
			}
			if utf8.RuneCountInString(a.Provenance.SourceDigest) > 64 {
				add(fmt.Sprintf("%s.provenance.source_digest 长度 %d 超过上限 64 个字符", prefix, utf8.RuneCountInString(a.Provenance.SourceDigest)))
			}
		}
	}

	if totalExcerptRunes > 8000 {
		add(fmt.Sprintf("所有 anchors.excerpt 总长度 %d 超过上限 8000 个字符", totalExcerptRunes))
	}

	return errs
}

// CommitResult 是 commit_chapter 工具的结构化返回值。
// 只包含事实字段；"下一步做什么"由 Reminder 通道基于当前 Progress 自行生成。
type CommitResult struct {
	Chapter        int              `json:"chapter"`
	Committed      bool             `json:"committed"`
	WordCount      int              `json:"word_count"`
	NextChapter    int              `json:"next_chapter"`
	ReviewRequired bool             `json:"review_required"`
	ReviewReason   string           `json:"review_reason,omitempty"`
	HookType       string           `json:"hook_type,omitempty"`
	DominantStrand string           `json:"dominant_strand,omitempty"`
	Feedback       *OutlineFeedback `json:"feedback,omitempty"`
	// 长篇分层信号
	ArcEnd         bool `json:"arc_end,omitempty"`
	VolumeEnd      bool `json:"volume_end,omitempty"`
	Volume         int  `json:"volume,omitempty"`
	Arc            int  `json:"arc,omitempty"`
	NeedsExpansion bool `json:"needs_expansion,omitempty"`  // 下一弧是骨架，需要展开章节
	NeedsNewVolume bool `json:"needs_new_volume,omitempty"` // 需要 Architect 创建下一卷
	NextVolume     int  `json:"next_volume,omitempty"`      // 下一弧/卷序号
	NextArc        int  `json:"next_arc,omitempty"`         // 下一弧序号
	// 完成态事实：本次 commit 后是否整本书已完成
	BookComplete bool `json:"book_complete,omitempty"`
	// 当前 Progress.Flow 快照（writing / reviewing / rewriting / polishing）
	Flow string `json:"flow,omitempty"`
}
