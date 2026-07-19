package projectprofile

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ── V3 内建 Profile 片段（字段语义嵌入资产，不从外部 config/rules 读取）──
// 所有五个消费者使用单一共享模板：七字段非空、sensory 可选、legacy 禁止。

// V3SceneBeatGuidance 是所有 v3 消费者共享的场景节拍字段模板。
// 每 scene 七字段均必填且不可为空；sensory_anchor 可选；legacy 禁止。
const V3SceneBeatGuidance = "## 场景节拍字段要求（v3 契约）\n" +
	"场景节拍（scene）必须使用结构化对象，每 scene 包含以下七个字段。遗留字符串格式（legacy）不允许使用。\n" +
	"- goal（必填，不可为空）：角色在本节拍中的目标\n" +
	"- action（必填，不可为空）：角色采取的行动\n" +
	"- conflict（必填，不可为空）：本节拍中的冲突/阻力\n" +
	"- outcome（必填，不可为空）：本节拍的结果/转折\n" +
	"- sensory_anchor（可选）：感官锚点，一个具象画面/声音/气味/触感\n" +
	"- body_reaction（必填，不可为空）：身体反应\n" +
	"- emotion_reaction（必填，不可为空）：情绪/心理反应\n" +
	"- erotic_charge（必填，不可为空）：色气/性张力"

// V3ArchitectShortGuidance 共享模板的别名。
const V3ArchitectShortGuidance = V3SceneBeatGuidance

// V3ArchitectLongGuidance 共享模板的别名。
const V3ArchitectLongGuidance = V3SceneBeatGuidance

// V3WriterGuidance 共享模板的别名。
const V3WriterGuidance = V3SceneBeatGuidance

// V3EditorGuidance 共享模板的别名。
const V3EditorGuidance = V3SceneBeatGuidance

// V3ImportGuidance 共享模板的别名。
const V3ImportGuidance = V3SceneBeatGuidance

// SceneBeatContract 提供契约感知的场景节拍校验。
// 所有字段不可变（构造后不可修改）。策略字段不公开暴露。
type SceneBeatContract struct {
	contract  Contract
	reqFields []string
	rejectLeg bool
	guidance  string
	fieldGets map[string]func(domain.SceneBeat) string
}

// GetContract 返回契约标识（不可变值类型）。
func (c *SceneBeatContract) GetContract() Contract { return c.contract }

// RequiredFields 返回必填字段名的副本（防御性拷贝）。
func (c *SceneBeatContract) RequiredFields() []string {
	out := make([]string, len(c.reqFields))
	copy(out, c.reqFields)
	return out
}

// RejectLegacy 返回是否拒绝 legacy string 格式。
func (c *SceneBeatContract) RejectLegacy() bool { return c.rejectLeg }

// NewCore4Contract 创建 Core4 契约校验器（仅 Goal/Action/Conflict/Outcome）。
func NewCore4Contract() *SceneBeatContract {
	return &SceneBeatContract{
		contract:  ContractCore4,
		reqFields: []string{"goal", "action", "conflict", "outcome"},
		rejectLeg: false,
		fieldGets: map[string]func(domain.SceneBeat) string{
			"goal":     func(s domain.SceneBeat) string { return s.Goal },
			"action":   func(s domain.SceneBeat) string { return s.Action },
			"conflict": func(s domain.SceneBeat) string { return s.Conflict },
			"outcome":  func(s domain.SceneBeat) string { return s.Outcome },
		},
	}
}

// NewSceneBeatV3Contract 创建 v3 契约校验器（全部七个字段）。
// 从内部 sceneBeatV3Fields 复制字段表——外部无法通过修改全局变量影响契约。
func NewSceneBeatV3Contract() *SceneBeatContract {
	fields := make([]string, len(sceneBeatV3Fields))
	copy(fields, sceneBeatV3Fields)
	return &SceneBeatContract{
		contract:  ContractSceneBeatV3,
		reqFields: fields,
		rejectLeg: true,
		fieldGets: map[string]func(domain.SceneBeat) string{
			"goal":             func(s domain.SceneBeat) string { return s.Goal },
			"action":           func(s domain.SceneBeat) string { return s.Action },
			"conflict":         func(s domain.SceneBeat) string { return s.Conflict },
			"outcome":          func(s domain.SceneBeat) string { return s.Outcome },
			"body_reaction":    func(s domain.SceneBeat) string { return s.BodyReaction },
			"emotion_reaction": func(s domain.SceneBeat) string { return s.EmotionReaction },
			"erotic_charge":    func(s domain.SceneBeat) string { return s.EroticCharge },
		},
	}
}

// Validate 校验单个 SceneBeat 是否符合本契约要求。
// Core4 契约接受 legacy（源为 string）场景；v3 契约拒绝 legacy。
func (c *SceneBeatContract) Validate(beat domain.SceneBeat) error {
	if beat.IsLegacy() {
		if c.rejectLeg {
			return fmt.Errorf("scene_beat: legacy string scene is not valid for v3 contract (missing body_reaction, emotion_reaction, erotic_charge)")
		}
		return nil
	}
	for _, field := range c.reqFields {
		getter, ok := c.fieldGets[field]
		if !ok {
			continue
		}
		if strings.TrimSpace(getter(beat)) == "" {
			return fmt.Errorf("scene_beat: %s is required", field)
		}
	}
	return nil
}

// ValidateAll 校验多个 SceneBeat 是否符合本契约。
// 返回所有缺失字段的汇总错误；nil 表示全部通过。
func (c *SceneBeatContract) ValidateAll(beats []domain.SceneBeat) error {
	var errs []string
	for i, beat := range beats {
		if err := c.Validate(beat); err != nil {
			errs = append(errs, fmt.Sprintf("[%d] %s", i, err.Error()))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scene_beat: %d validation error(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// GuidanceForRole 根据角色返回 v3 的字段指导片段。
// Core4 契约返回空字符串（不追加额外指导）。
func (c *SceneBeatContract) GuidanceForRole(role string) string {
	if c.contract != ContractSceneBeatV3 {
		return ""
	}
	switch role {
	case "architect_short", "architect":
		return V3ArchitectShortGuidance
	case "architect_long":
		return V3ArchitectLongGuidance
	case "writer":
		return V3WriterGuidance
	case "editor":
		return V3EditorGuidance
	default:
		return ""
	}
}

// ImportGuidance 返回 v3 导入字段指导。Core4 返回空。
func (c *SceneBeatContract) ImportGuidance() string {
	if c.contract != ContractSceneBeatV3 {
		return ""
	}
	return V3ImportGuidance
}

// ContractFor 根据 Contract 返回对应的 SceneBeatContract 校验器。
func ContractFor(c Contract) *SceneBeatContract {
	switch c {
	case ContractSceneBeatV3:
		return NewSceneBeatV3Contract()
	default:
		return NewCore4Contract()
	}
}
