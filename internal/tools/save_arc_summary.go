package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// 短数字箭头账本：20→22、24->28（prose/dialogue 禁止；taboos 不检）
var styleRulesDigitArrow = regexp.MustCompile(`\d{1,3}\s*(?:→|->|➜)\s*\d{1,3}`)

// SaveArcSummaryTool 保存弧级摘要和角色快照，Editor 在弧结束时调用。
type SaveArcSummaryTool struct {
	store *store.Store
}

func NewSaveArcSummaryTool(store *store.Store) *SaveArcSummaryTool {
	return &SaveArcSummaryTool{store: store}
}

func (t *SaveArcSummaryTool) Name() string { return "save_arc_summary" }
func (t *SaveArcSummaryTool) Description() string {
	return "保存弧级摘要和角色状态快照（长篇模式，弧结束时调用）"
}
func (t *SaveArcSummaryTool) Label() string { return "保存弧摘要" }

// 写工具，禁止并发。
func (t *SaveArcSummaryTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SaveArcSummaryTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *SaveArcSummaryTool) Schema() map[string]any {
	snapshotSchema := schema.Object(
		schema.Property("name", schema.String("角色名")).Required(),
		schema.Property("status", schema.String("当前状态（存活/受伤/失踪等）")).Required(),
		schema.Property("power", schema.String("能力变化")),
		schema.Property("motivation", schema.String("当前动机")).Required(),
		schema.Property("relations", schema.String("关键关系变化")),
	)
	voiceSchema := schema.Object(
		schema.Property("name", schema.String("角色名")).Required(),
		schema.Property("rules", schema.Array("2-3 条语言特征规则（每条 ≤30 字）", schema.String(""))).Required(),
	)
	styleRulesSchema := schema.Object(
		schema.Property("prose", schema.Array("3-5 条正向（≤50字）：按黑暗情色小说写——主轴/焦点/节奏/词感；对齐 long.reason；禁止把说明文/观察报告腔升格成规范", schema.String(""))).Required(),
		schema.Property("dialogue", schema.Array("核心角色口吻/行为（话后接肉；非频率报表）", voiceSchema)).Required(),
		schema.Property("taboos", schema.Array("不要怎样写：说明文、观察报告、黏膜网格、脊髓解释、意味着、第N下、盆底等；本弧污染进这里", schema.String(""))),
	)
	return schema.Object(
		schema.Property("volume", schema.Int("卷号")).Required(),
		schema.Property("arc", schema.Int("弧号")).Required(),
		schema.Property("title", schema.String("弧标题")).Required(),
		schema.Property("summary", schema.String("弧摘要（500字以内）")).Required(),
		schema.Property("key_events", schema.Array("弧内关键事件", schema.String(""))).Required(),
		schema.Property("character_snapshots", schema.Array("角色状态快照", snapshotSchema)).Required(),
		schema.Property("style_rules", styleRulesSchema),
	)
}

func (t *SaveArcSummaryTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	args = normalizeIntegerStringFields(args, "volume", "arc")
	var a struct {
		Volume             int                        `json:"volume"`
		Arc                int                        `json:"arc"`
		Title              string                     `json:"title"`
		Summary            string                     `json:"summary"`
		KeyEvents          []string                   `json:"key_events"`
		CharacterSnapshots []domain.CharacterSnapshot `json:"character_snapshots"`
		StyleRules         *arcSummaryStyleRules      `json:"style_rules"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		if strings.Contains(err.Error(), "style_rules.dialogue") {
			return nil, fmt.Errorf("invalid args: style_rules.dialogue must be an array of objects {name, rules}, not strings: %w: %w", errs.ErrToolArgs, err)
		}
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Volume <= 0 || a.Arc <= 0 {
		return nil, fmt.Errorf("volume and arc must be > 0: %w", errs.ErrToolArgs)
	}
	if err := validateArcSummaryStyleRules(a.StyleRules); err != nil {
		return nil, err
	}

	arcSummary := domain.ArcSummary{
		Volume:    a.Volume,
		Arc:       a.Arc,
		Title:     a.Title,
		Summary:   a.Summary,
		KeyEvents: a.KeyEvents,
	}
	if err := t.store.Summaries.SaveArcSummary(arcSummary); err != nil {
		return nil, fmt.Errorf("save arc summary: %w: %w", errs.ErrStoreWrite, err)
	}

	if len(a.CharacterSnapshots) > 0 {
		for i := range a.CharacterSnapshots {
			a.CharacterSnapshots[i].Volume = a.Volume
			a.CharacterSnapshots[i].Arc = a.Arc
		}
		if err := t.store.Characters.SaveSnapshots(a.Volume, a.Arc, a.CharacterSnapshots); err != nil {
			return nil, fmt.Errorf("save character snapshots: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	styleRulesSaved := false
	if a.StyleRules != nil && len(a.StyleRules.Prose) > 0 {
		rules := domain.WritingStyleRules{
			Volume:    a.Volume,
			Arc:       a.Arc,
			Prose:     a.StyleRules.Prose,
			Dialogue:  a.StyleRules.Dialogue,
			Taboos:    a.StyleRules.Taboos,
			UpdatedAt: time.Now().Format(time.RFC3339),
		}
		if err := t.store.World.SaveStyleRules(rules); err != nil {
			return nil, fmt.Errorf("save style rules: %w: %w", errs.ErrStoreWrite, err)
		}
		styleRulesSaved = true
	}

	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ArcScope(a.Volume, a.Arc), "arc_summary",
		fmt.Sprintf("summaries/arc-v%02da%02d.json", a.Volume, a.Arc),
	); err != nil {
		return nil, fmt.Errorf("checkpoint arc summary: %w: %w", errs.ErrStoreWrite, err)
	}

	result := map[string]any{
		"saved": true, "type": "arc_summary",
		"volume": a.Volume, "arc": a.Arc,
		"snapshots":         len(a.CharacterSnapshots),
		"style_rules_saved": styleRulesSaved,
	}

	// 快照派生基底：加载 character_state 当前值（按 entity 分组），提示 LLM
	// 快照应与之兼容——快照是派生摘要，不覆盖 current state。
	if basis := buildCurrentStateBasis(t.store); len(basis) > 0 {
		result["current_state_basis"] = basis
		result["snapshot_hint"] = "以上为 meta/character_state.json 当前值（权威层）。快照应与之兼容：快照是弧末派生摘要，不得与 current state 明显矛盾（如当前存在某装置却写已移除）。"
		if conflicts := checkSnapshotStateConflicts(a.CharacterSnapshots, basis); len(conflicts) > 0 {
			result["snapshot_conflict_warnings"] = conflicts
		}
	}

	return json.Marshal(result)
}

// buildCurrentStateBasis 按 entity 分组返回 character_state 当前值摘要。
// 无状态文件/空状态返回 nil。
func buildCurrentStateBasis(st *store.Store) map[string]map[string]string {
	entries, err := st.World.LoadCharacterState()
	if err != nil || len(entries) == 0 {
		return nil
	}
	grouped := make(map[string]map[string]string, len(entries))
	for _, e := range entries {
		m := grouped[e.Entity]
		if m == nil {
			m = make(map[string]string)
			grouped[e.Entity] = m
		}
		m[e.Field] = e.Value
	}
	return grouped
}

// snapshotPresenceContradictors 宽松冲突检查关键词：current state 中"装置/状态存在且
// 持续"的字段（见 currentStateImpliesPresence），快照文本出现这些词视为潜在矛盾
// （仅告警，不拒绝）。覆盖迁移数据形态：除"完好/痊愈/消失"外，还有"双腿笔直/
// 手脚自由/已摘除"等表达"装置移除或身体复原"的短语。
var snapshotPresenceContradictors = []string{
	"完好", "痊愈", "消失", "不存在", "移除", "去除", "截肢", "已断",
	"双腿笔直", "手脚自由", "已摘除", "已拆除", "恢复正常", "不受影响",
}

// currentStateImpliesPresence 判断 current state 字段是否暗示"装置/状态存在且持续"：
// body_device.* 前缀一律视为装置在位（迁移数据多为安装/持续描述，value 无"存在"字样），
// 其余字段按 value 关键词兜底判断（"存在/安装/固定/永久/融合/持续"）。
func currentStateImpliesPresence(field, value string) bool {
	if strings.HasPrefix(field, "body_device.") {
		return true
	}
	for _, kw := range []string{"存在", "安装", "固定", "永久", "融合", "持续"} {
		if strings.Contains(value, kw) {
			return true
		}
	}
	return false
}

// checkSnapshotStateConflicts 宽松冲突检查：current state 中暗示"装置/状态存在且持续"
// 的字段（body_device.* 前缀或 value 含"存在/安装/固定"等词），若同名角色快照文本
// （status/power/motivation/relations）出现矛盾关键词 → 返回警告。
// 启发式实现：只覆盖最明确的"装置/伤势存在 ↔ 完好/消失"矛盾，不做语义级判断。
func checkSnapshotStateConflicts(snapshots []domain.CharacterSnapshot, basis map[string]map[string]string) []string {
	if len(snapshots) == 0 || len(basis) == 0 {
		return nil
	}
	var warnings []string
	for _, snap := range snapshots {
		fields, ok := basis[snap.Name]
		if !ok {
			continue
		}
		text := snap.Status + " " + snap.Power + " " + snap.Motivation + " " + snap.Relations
		for field, value := range fields {
			if !currentStateImpliesPresence(field, value) {
				continue
			}
			for _, kw := range snapshotPresenceContradictors {
				if strings.Contains(text, kw) {
					warnings = append(warnings, fmt.Sprintf("快照 %s 疑似与 current state 矛盾：current %s.%s=%s，快照文本含“%s”",
						snap.Name, snap.Name, field, value, kw))
					break
				}
			}
		}
	}
	return warnings
}

type arcSummaryStyleRules struct {
	Prose    []string                `json:"prose"`
	Dialogue []domain.CharacterVoice `json:"dialogue"`
	Taboos   []string                `json:"taboos"`
}

func validateArcSummaryStyleRules(rules *arcSummaryStyleRules) error {
	if rules == nil {
		return nil
	}
	if len(rules.Prose) == 0 {
		return fmt.Errorf("style_rules.prose is required when style_rules is provided: %w", errs.ErrToolArgs)
	}
	if len(rules.Dialogue) == 0 {
		return fmt.Errorf("style_rules.dialogue is required when style_rules is provided; expected array of objects {name, rules}: %w", errs.ErrToolArgs)
	}
	for i, line := range rules.Prose {
		if strings.TrimSpace(line) == "" {
			return fmt.Errorf("style_rules.prose[%d] is empty: %w", i, errs.ErrToolArgs)
		}
		if hit := styleRulesPollutionHit(line); hit != "" {
			return fmt.Errorf("style_rules.prose[%d] looks like metering/observation pollution (%s); put bans in taboos, rewrite prose as body-process how-to (≤50 chars, tactile outcomes): %w", i, hit, errs.ErrToolArgs)
		}
	}
	for i, voice := range rules.Dialogue {
		if strings.TrimSpace(voice.Name) == "" {
			return fmt.Errorf("style_rules.dialogue[%d].name is required: %w", i, errs.ErrToolArgs)
		}
		if len(voice.Rules) == 0 {
			return fmt.Errorf("style_rules.dialogue[%d].rules is required: %w", i, errs.ErrToolArgs)
		}
		for j, rule := range voice.Rules {
			if strings.TrimSpace(rule) == "" {
				return fmt.Errorf("style_rules.dialogue[%d].rules[%d] is empty: %w", i, j, errs.ErrToolArgs)
			}
			if hit := styleRulesPollutionHit(rule); hit != "" {
				return fmt.Errorf("style_rules.dialogue[%d].rules[%d] looks like metering/observation pollution (%s); put bans in taboos, keep dialogue as voice/behavior only: %w", i, j, hit, errs.ErrToolArgs)
			}
		}
	}
	// taboos 允许出现「避免次/分…」等禁令字样，不做污染拦截
	return nil
}

// styleRulesPollutionHit 检测 prose/dialogue 是否把近章账本/观察腔升格成「本弧要这样写」。
// 命中则返回短标签；taboos 侧不调用本函数。
func styleRulesPollutionHit(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 频率/次分账本（正文污染主因）
	markers := []struct {
		sub string
		tag string
	}{
		{"次/分", "次/分"},
		{"次每分", "次每分"},
		{"次每分钟", "次每分钟"},
		{"差额", "次数差额"},
		{"跳至", "跳至N"},
		{"骤升至", "骤升至N"},
		{"基线从", "基线从N"},
		{"基线高", "基线高N次"},
		{"双栏", "双栏读数"},
		{"开尔文", "色温开尔文"},
		{"+500K", "色温K"},
		{"声景为节拍", "声景节拍器"},
		{"传导链", "神经传导链"},
		{"意味着", "意味着解释链"},
		{"不是A是B", "否定绕弯"},
		{"不是…而是", "否定绕弯"},
		{"蠕动×", "蠕动乘次"},
		{"蠕动 x", "蠕动乘次"},
		{"第一下", "数字动作串"},
		{"第二下", "数字动作串"},
		{"第三下", "数字动作串"},
		{"第N颗", "序号串戏"},
		{"四次收缩", "数字动作串"},
		{"赫兹", "工程频率腔"},
		{"整数比", "工程频率腔"},
		{"谐波", "工程频率腔"},
	}
	lower := s
	for _, m := range markers {
		if strings.Contains(lower, m.sub) {
			// 「避免/禁止 + 污染词」视为 ban 句，允许出现在 prose 时仍危险——prose 应写正向怎么落
			// 若整句以 避免/禁止 开头，更像误放进 prose 的 taboo：仍拒，要求挪到 taboos
			return m.tag
		}
	}
	// 纯数字跳变账本：20→22、24→28 等（箭头两侧短数字）
	if styleRulesDigitArrow.MatchString(s) {
		return "数字箭头账本"
	}
	return ""
}
