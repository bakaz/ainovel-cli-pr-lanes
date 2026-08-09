package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Novel 小说元信息。
type Novel struct {
	Name          string `json:"name"`
	TotalChapters int    `json:"total_chapters"`
}

// SceneBeat 一个场景节拍的结构化描述。
// 新模型输出完整对象；旧 string 格式兼容为仅 Action 有值，并用 fromString 标记来源。
type SceneBeat struct {
	Goal            string `json:"goal,omitempty"`
	Action          string `json:"action,omitempty"`
	Conflict        string `json:"conflict,omitempty"`
	Outcome         string `json:"outcome,omitempty"`
	SensoryAnchor   string `json:"sensory_anchor,omitempty"`
	BodyReaction    string `json:"body_reaction,omitempty"`
	EmotionReaction string `json:"emotion_reaction,omitempty"`
	EroticCharge    string `json:"erotic_charge,omitempty"`
	// fromString 为 true 表示 JSON 源是 string（遗留格式），不是残缺 object。
	// 不序列化；仅用于 IsLegacy / 校验 / 写出策略。
	fromString bool `json:"-"`
}

// IsLegacy 仅当 JSON 源为 string 时为 true。
// {"action":"..."} 残缺对象不是 legacy，必须走四字段校验。
func (s SceneBeat) IsLegacy() bool {
	return s.fromString
}

// Text 返回场景节拍的可读文本（供召回/过滤/渲染使用）。
func (s SceneBeat) Text() string {
	if s.IsLegacy() {
		return s.Action
	}
	var parts []string
	if s.Goal != "" {
		parts = append(parts, s.Goal)
	}
	if s.Action != "" {
		parts = append(parts, s.Action)
	}
	if s.Conflict != "" {
		parts = append(parts, s.Conflict)
	}
	if s.Outcome != "" {
		parts = append(parts, s.Outcome)
	}
	if s.SensoryAnchor != "" {
		parts = append(parts, s.SensoryAnchor)
	}
	if s.BodyReaction != "" {
		parts = append(parts, s.BodyReaction)
	}
	if s.EmotionReaction != "" {
		parts = append(parts, s.EmotionReaction)
	}
	if s.EroticCharge != "" {
		parts = append(parts, s.EroticCharge)
	}
	return strings.Join(parts, " ")
}

// ValidateRequired 校验结构化场景的四个必填字段是否非空。
// 仅对非 legacy 场景生效；legacy（源为 string）跳过校验。
func (s SceneBeat) ValidateRequired() error {
	if s.IsLegacy() {
		return nil
	}
	if strings.TrimSpace(s.Goal) == "" {
		return fmt.Errorf("goal: required")
	}
	if strings.TrimSpace(s.Action) == "" {
		return fmt.Errorf("action: required")
	}
	if strings.TrimSpace(s.Conflict) == "" {
		return fmt.Errorf("conflict: required")
	}
	if strings.TrimSpace(s.Outcome) == "" {
		return fmt.Errorf("outcome: required")
	}
	return nil
}

// SceneList 兼容新旧 scenes 格式的列表。
// JSON 反序列化支持 []string | []object | 混合；旧 string 仅填 Action 并标记 fromString。
// 序列化：legacy 仍写 string，完整对象写 object（避免隐式全书迁移成空字段 object）。
type SceneList []SceneBeat

// UnmarshalJSON 兼容 []string / []object / 混合格式 / null。
// null 元素（如 ["scene", null]）拒绝解析，不误标为 legacy。
func (s *SceneList) UnmarshalJSON(data []byte) error {
	// 处理 null
	if string(data) == "null" {
		*s = nil
		return nil
	}

	// 按元素逐个解析（支持混合格式：有些是 string，有些是 object）
	// 不设 []string 快速路径：避免 null → "" 误标 legacy。
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("scenes: expected []string or []object: %w", err)
	}
	*s = make([]SceneBeat, 0, len(raw))
	for _, item := range raw {
		trimmed := item
		// 跳过可能的空白
		for len(trimmed) > 0 && trimmed[0] <= ' ' {
			trimmed = trimmed[1:]
		}
		if len(trimmed) == 0 {
			return fmt.Errorf("scenes: empty element at index %d", len(*s))
		}
		switch trimmed[0] {
		case '"': // string
			var str string
			if err := json.Unmarshal(item, &str); err != nil {
				return err
			}
			*s = append(*s, SceneBeat{Action: str, fromString: true})
		case '{': // object — 即使仅 action 也不是 legacy
			var beat SceneBeat
			if err := json.Unmarshal(item, &beat); err != nil {
				return err
			}
			beat.fromString = false
			*s = append(*s, beat)
		case 'n': // null — 拒绝
			return fmt.Errorf("scenes: null element at index %d is not allowed", len(*s))
		default:
			return fmt.Errorf("scenes: unexpected element type at index %d: %s", len(*s), string(trimmed[:min(len(trimmed), 20)]))
		}
	}
	return nil
}

// sceneBeatJSON 完整对象写出形状（不含 fromString）。
type sceneBeatJSON struct {
	Goal            string `json:"goal"`
	Action          string `json:"action"`
	Conflict        string `json:"conflict"`
	Outcome         string `json:"outcome"`
	SensoryAnchor   string `json:"sensory_anchor,omitempty"`
	BodyReaction    string `json:"body_reaction,omitempty"`
	EmotionReaction string `json:"emotion_reaction,omitempty"`
	EroticCharge    string `json:"erotic_charge,omitempty"`
}

// MarshalJSON：legacy 写 string，完整对象写 object。
func (s SceneList) MarshalJSON() ([]byte, error) {
	if s == nil {
		return json.Marshal(nil)
	}
	out := make([]json.RawMessage, len(s))
	for i, beat := range s {
		var raw []byte
		var err error
		if beat.IsLegacy() {
			raw, err = json.Marshal(beat.Action)
		} else {
			raw, err = json.Marshal(sceneBeatJSON{
				Goal:            beat.Goal,
				Action:          beat.Action,
				Conflict:        beat.Conflict,
				Outcome:         beat.Outcome,
				SensoryAnchor:   beat.SensoryAnchor,
				BodyReaction:    beat.BodyReaction,
				EmotionReaction: beat.EmotionReaction,
				EroticCharge:    beat.EroticCharge,
			})
		}
		if err != nil {
			return nil, err
		}
		out[i] = raw
	}
	return json.Marshal(out)
}

// FlattenScenes 将所有场景文本拼接为单字符串（供召回/过滤使用）。
func FlattenScenes(scenes SceneList) string {
	var parts []string
	for _, sc := range scenes {
		parts = append(parts, sc.Text())
	}
	return strings.Join(parts, " ")
}

// OutlineEntry 大纲条目，对应一章。
type OutlineEntry struct {
	Chapter   int       `json:"chapter"`
	Title     string    `json:"title"`
	CoreEvent string    `json:"core_event"`
	Hook      string    `json:"hook"`
	Scenes    SceneList `json:"scenes"`
}

// Character 角色档案。
type Character struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"` // 别名/称号/绰号（如"废物少年"、"炎哥"）
	Role        string   `json:"role"`
	Description string   `json:"description"`
	Arc         string   `json:"arc"`
	Traits      []string `json:"traits"`
	Tier        string   `json:"tier,omitempty"` // core / important / secondary / decorative（默认 important）
}

// VolumeOutline 卷级大纲（长篇分层模式）。
type VolumeOutline struct {
	Index int          `json:"index"`
	Title string       `json:"title"`
	Theme string       `json:"theme"`           // 本卷核心冲突/主题
	Final bool         `json:"final,omitempty"` // 收官卷：全书在本卷收束（架构师 append_volume 时宣告）
	Arcs  []ArcOutline `json:"arcs"`
}

// IsExpanded 判断卷是否已展开（有弧级结构）。
func (v *VolumeOutline) IsExpanded() bool { return len(v.Arcs) > 0 }

// FinaleVolume 返回已宣告的收官卷序号，未宣告返回 0。
// 收官事实 = "最后一卷带 Final 标记"：宣告后全书进入收束态（规划收线、终卷结构
// 写完即完结）；若此后又追加了未标记的新卷，新卷成为最后一卷，收束态自然解除——
// 因此无需撤销工具，状态永远可从大纲数据推导。
func FinaleVolume(volumes []VolumeOutline) int {
	if n := len(volumes); n > 0 && volumes[n-1].Final {
		return volumes[n-1].Index
	}
	return 0
}

// StoryCompass 把稳定的全书终局方向与可自由调整的近期方向分开。
type StoryCompass struct {
	Long    LongCompass `json:"long"`
	Current *Compass    `json:"current,omitempty"`
}

type LongCompass struct {
	EndingDirection string   `json:"ending_direction"`
	OpenThreads     []string `json:"open_threads,omitempty"`
	EstimatedScale  string   `json:"estimated_scale,omitempty"`
	LastUpdated     int      `json:"last_updated,omitempty"`
	// Reference 保存书籍自定义的长期规划资料。它不参与完成判定，也不会被
	// update_compass 的常规字段更新覆盖；仅由 Architect 按需工具读取。
	Reference json.RawMessage `json:"reference,omitempty"`
}

// Compass 是 Architect 可随弧推进自由编辑的短罗盘；不重复大纲的卷/弧字段。
type Compass struct {
	Direction   string   `json:"direction,omitempty"`
	OpenThreads []string `json:"open_threads,omitempty"`
	LastUpdated int      `json:"last_updated,omitempty"`
}

// UnmarshalJSON 兼容 v1 在根部保存 ending_direction/open_threads/estimated_scale 的文件。
func (s *StoryCompass) UnmarshalJSON(data []byte) error {
	type currentShape StoryCompass
	var current currentShape
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	if current.Long.EndingDirection != "" || current.Current != nil {
		*s = StoryCompass(current)
		return nil
	}
	var legacy struct {
		EndingDirection string   `json:"ending_direction"`
		OpenThreads     []string `json:"open_threads"`
		EstimatedScale  string   `json:"estimated_scale"`
		LastUpdated     int      `json:"last_updated"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	s.Long = LongCompass{
		EndingDirection: legacy.EndingDirection,
		OpenThreads:     legacy.OpenThreads,
		EstimatedScale:  legacy.EstimatedScale,
		LastUpdated:     legacy.LastUpdated,
	}
	return nil
}

func (s StoryCompass) LatestUpdated() int {
	if s.Current != nil && s.Current.LastUpdated > s.Long.LastUpdated {
		return s.Current.LastUpdated
	}
	return s.Long.LastUpdated
}

// ArcOutline 弧级大纲。
type ArcOutline struct {
	Index             int            `json:"index"` // 卷内弧序号
	Title             string         `json:"title"`
	Goal              string         `json:"goal"`                         // 弧目标（起承转合）
	EstimatedChapters int            `json:"estimated_chapters,omitempty"` // 骨架弧的预估章数（展开后清零）
	Chapters          []OutlineEntry `json:"chapters"`
}

// IsExpanded 判断弧是否已展开（有详细章节）。
func (a *ArcOutline) IsExpanded() bool { return len(a.Chapters) > 0 }

// ArcExpansion 是 Architect 在结构边界对一个未写弧作出的完整规划。
// Title/Goal 不是骨架的机械副本：模型可依据已完成正文修订尚未发生的计划。
type ArcExpansion struct {
	Title    string         `json:"title"`
	Goal     string         `json:"goal"`
	Chapters []OutlineEntry `json:"chapters"`
}

// TotalChapters 计算分层大纲的当前规划总章数。
// 已展开弧按真实章节数计，骨架弧按 EstimatedChapters 计。
// Progress.TotalChapters 用它判断长篇上下文策略；真正可写章节仍来自 FlattenOutline。
func TotalChapters(volumes []VolumeOutline) int {
	n := 0
	for _, v := range volumes {
		for _, a := range v.Arcs {
			if a.IsExpanded() {
				n += len(a.Chapters)
			} else {
				n += a.EstimatedChapters
			}
		}
	}
	return n
}

// FlattenOutline 将分层大纲展开为扁平章节列表，保持全局章节号连续。
func FlattenOutline(volumes []VolumeOutline) []OutlineEntry {
	var result []OutlineEntry
	ch := 1
	for _, v := range volumes {
		for _, a := range v.Arcs {
			for _, e := range a.Chapters {
				e.Chapter = ch
				result = append(result, e)
				ch++
			}
		}
	}
	return result
}

// WorldRule 世界观规则条目。
type WorldRule struct {
	Category string `json:"category"` // magic / technology / geography / society / other
	Rule     string `json:"rule"`     // 规则描述
	Boundary string `json:"boundary"` // 不可违反的边界
}

// world_rules 量控上限（四层状态机制）：
//   - MaxWorldRulesEntries 为条数软上限：超出仅告警（提示合并/移除过期规则），允许保存；
//   - MaxWorldRulesBytes 为总序列化字节硬上限：超出拒绝保存（防止规则集膨胀挤占上下文预算）。
const (
	MaxWorldRulesEntries = 30
	MaxWorldRulesBytes   = 24576
)
