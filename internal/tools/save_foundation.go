package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

// SaveFoundationTool 保存基础设定（premise/outline/characters），Architect 专用。
type SaveFoundationTool struct {
	store               *store.Store
	contract            *projectprofile.SceneBeatContract
	longApproval        *AskUserTool
	longApprovalTimeout time.Duration
}

const DefaultLongApprovalTimeout = 30 * time.Minute

func NewSaveFoundationTool(store *store.Store, contract *projectprofile.SceneBeatContract) *SaveFoundationTool {
	return &SaveFoundationTool{store: store, contract: contract, longApprovalTimeout: DefaultLongApprovalTimeout}
}

// SetLongApproval 注入 TUI 用户交互通道。未注入时，对既有 compass.long 的修改
// 会立即拒绝并以正常工具结果返回，不阻塞无人值守流程。
func (t *SaveFoundationTool) SetLongApproval(askUser *AskUserTool, timeout time.Duration) {
	t.longApproval = askUser
	if timeout > 0 {
		t.longApprovalTimeout = timeout
	}
}

func (t *SaveFoundationTool) Name() string { return "save_foundation" }
func (t *SaveFoundationTool) Description() string {
	return "保存小说基础设定（premise/outline/characters/world_rules/compass 等）。**这是唯一持久化入口**：未经此工具调用保存的内容不会进入 store，只在消息里输出 Markdown/JSON 等于丢失。参数固定为 {type, content, scale?, volume?, arc?, section?, reason?}。type 可选 premise / outline / layered_outline / characters / world_rules / expand_arc / append_volume / update_compass / complete_book。premise 时 content 必须是 Markdown 字符串；其他类型 content 优先直接传 JSON 数组或对象。expand_arc 校准并展开一个未写骨架弧（需 volume + arc，content 为 {title, goal, chapters}，可依据已完成正文修订原骨架目标）；append_volume 追加新卷（content 为完整 VolumeOutline JSON，含弧结构；顶层带 \"final\": true 即宣告收官卷——全书在该卷收束，所有章节写完后自动完结，无需再调 complete_book）；update_compass 采用 section=long/current 合并更新对应部分：current 直接保存；既有 long 的实质修改必须给 reason，并作为提案等待 TUI 用户批准，30 分钟未批准则拒绝 long 更新并继续；也可省略 section 并用 content={long:{...},current:{...}}。complete_book 宣告全书完结（content 传空对象 {}，直接推 Phase=Complete；工具会校验：大纲内章节已全部写完、无返工队列，否则拒绝——想提前收束用 append_volume 的 final 收官卷）。append_volume / complete_book 必须带 reason 参数（一句话判定理由，对照完结判定清单，记入裁定审计）。scale 可选，仅允许 short / mid / long。"
}
func (t *SaveFoundationTool) Label() string { return "保存设定" }

func (t *SaveFoundationTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SaveFoundationTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

// ── Validation helpers ──

func scenePropString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func scenePropInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func scenePropRequiredString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "minLength": 1}
}
func constEnum(v string) map[string]any {
	return map[string]any{"type": "string", "enum": []string{v}}
}

func sceneBeatSchema(v3 bool) map[string]any {
	props := map[string]any{
		"goal":             scenePropRequiredString("角色在本节拍中的目标"),
		"action":           scenePropRequiredString("角色采取的行动"),
		"conflict":         scenePropRequiredString("本节拍中的冲突/阻力"),
		"outcome":          scenePropRequiredString("本节拍的结果/转折"),
		"sensory_anchor":   scenePropString("感官锚点，一个具象画面/声音/气味/触感"),
		"body_reaction":    scenePropString("身体反应"),
		"emotion_reaction": scenePropString("心理反应"),
	}
	if v3 {
		props["erotic_charge"] = scenePropRequiredString("色气/性张力")
		for _, k := range []string{"body_reaction", "emotion_reaction"} {
			props[k].(map[string]any)["minLength"] = 1
		}
		return map[string]any{
			"type":                 "object",
			"properties":           props,
			"required":             []string{"goal", "action", "conflict", "outcome", "body_reaction", "emotion_reaction", "erotic_charge"},
			"additionalProperties": false,
		}
	}
	// Core4: object can include optional erotic_charge
	props["erotic_charge"] = scenePropString("色气/性张力（可选）")
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             []string{"goal", "action", "conflict", "outcome"},
		"additionalProperties": false,
	}
}

func chapterOutlineSchema(v3 bool, chapterReq bool) map[string]any {
	var scenesSchema map[string]any
	if v3 {
		scenesSchema = map[string]any{
			"type": "array", "description": "场景节拍数组（v3 七字段必填）",
			"items": sceneBeatSchema(true), "minItems": 1,
		}
	} else {
		// Core4: per-item mixed legacy string | object (optional erotic_charge)
		scenesSchema = map[string]any{
			"type": "array", "description": "场景节拍数组（混合 legacy string / Core4 对象）",
			"items": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "description": "遗留 string 格式场景"},
					sceneBeatSchema(false),
				},
			},
		}
	}
	reqd := []string{"title", "core_event"}
	if v3 {
		reqd = append(reqd, "scenes")
	}
	if chapterReq {
		reqd = append(reqd, "chapter")
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"chapter":    scenePropInt("章节序号（flat outline 必填；planning 路径可省略/0/显式值）"),
			"title":      scenePropString("章节标题"),
			"core_event": scenePropString("核心事件"),
			"hook":       scenePropString("章末钩子"),
			"scenes":     scenesSchema,
		},
		"required":             reqd,
		"additionalProperties": false,
	}
}

func arcOutlineSchema(v3 bool) map[string]any {
	chPlanning := chapterOutlineSchema(v3, false)
	if v3 {
		return map[string]any{
			"type": "object",
			"anyOf": []any{
				// detailed arc: must have non-empty chapters
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"index":              scenePropInt("卷内弧序号"),
						"title":              scenePropString("弧标题"),
						"goal":               scenePropString("弧目标（起承转合）"),
						"estimated_chapters": map[string]any{"type": "integer", "description": "预估章数（详细弧在转换前归零）"},
						"chapters":           map[string]any{"type": "array", "description": "章节数组", "items": chPlanning, "minItems": 1},
					},
					"required":             []string{"index", "title", "goal", "chapters"},
					"additionalProperties": false,
				},
				// skeleton arc: estimated_chapters >= 1; omit chapters or pass []
				// Note: do NOT use {"type":"null"} — DeepSeek/部分 OpenAI 兼容接口会拒绝 tool schema 中的 null type。
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"index":              scenePropInt("卷内弧序号"),
						"title":              scenePropString("弧标题"),
						"goal":               scenePropString("弧目标（起承转合）"),
						"estimated_chapters": map[string]any{"type": "integer", "description": "预估章数", "minimum": 1},
						"chapters":           skeletonChaptersSchema(),
					},
					"required":             []string{"index", "title", "goal", "estimated_chapters"},
					"additionalProperties": false,
				},
			},
		}
	}
	// Core4: also use anyOf for detailed/skeleton (not a single generic passage).
	// 外层必须有 type:object：DeepSeek 会拒绝无根 type 的 tool parameters（同 V3 根 schema）。
	return map[string]any{
		"type": "object",
		"anyOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index":    scenePropInt("卷内弧序号"),
					"title":    scenePropString("弧标题"),
					"goal":     scenePropString("弧目标（起承转合）"),
					"chapters": map[string]any{"type": "array", "description": "章节数组（含 non-empty chapters）", "items": chPlanning},
				},
				"required":             []string{"index", "title", "goal", "chapters"},
				"additionalProperties": false,
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index":              scenePropInt("卷内弧序号"),
					"title":              scenePropString("弧标题"),
					"goal":               scenePropString("弧目标（起承转合）"),
					"estimated_chapters": map[string]any{"type": "integer", "description": "预估章数", "minimum": 1},
					"chapters":           skeletonChaptersSchema(),
				},
				"required":             []string{"index", "title", "goal", "estimated_chapters"},
				"additionalProperties": false,
			},
		},
	}
}

// skeletonChaptersSchema 描述骨架弧可选的 chapters 字段。
// 对外 schema 只允许空数组（或不传该字段）；禁止 type:null，避免 DeepSeek 等拒收 tools。
// Execute 路径仍可容忍 JSON null（Go 解包后与省略等价）。
func skeletonChaptersSchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"maxItems":    0,
		"description": "骨架弧省略本字段或传空数组 []；不要传 null",
	}
}

func volumeOutlineSchema(v3 bool) map[string]any {
	arcs := map[string]any{"type": "array", "description": "弧数组", "items": arcOutlineSchema(v3)}
	if v3 {
		arcs["minItems"] = 1
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"index": map[string]any{"type": "integer", "description": "卷序号", "minimum": 1},
			"title": scenePropString("卷标题"),
			"theme": scenePropString("本卷核心冲突/主题"),
			"final": map[string]any{"type": "boolean", "description": "收官卷标记"},
			"arcs":  arcs,
		},
		"required":             []string{"index", "title", "theme", "arcs"},
		"additionalProperties": false,
	}
}

func characterSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "角色名"},
			"aliases":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "别名/称号"},
			"role":        map[string]any{"type": "string", "description": "角色身份"},
			"description": map[string]any{"type": "string", "description": "整体描述"},
			"arc":         map[string]any{"type": "string", "description": "角色弧线"},
			"traits":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "特质"},
			"tier":        map[string]any{"type": "string", "description": "角色层级"},
		},
		"additionalProperties": false,
	}
}

func worldRuleSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"category": map[string]any{"type": "string", "description": "规则类别（magic/technology/geography/society/other）"},
			"rule":     map[string]any{"type": "string", "description": "规则描述"},
			"boundary": map[string]any{"type": "string", "description": "不可违反的边界"},
		},
		"additionalProperties": false,
	}
}

// ── SCHEMA ──

func (t *SaveFoundationTool) Schema() map[string]any {
	if t.contract != nil && t.contract.GetContract() == projectprofile.ContractSceneBeatV3 {
		return t.v3Schema()
	}
	return t.core4Schema()
}

func (t *SaveFoundationTool) v3Branch(typeName string, contentSchema map[string]any, extraProps map[string]map[string]any) map[string]any {
	props := map[string]any{"type": constEnum(typeName), "content": contentSchema}
	for k, v := range extraProps {
		props[k] = v
	}
	return map[string]any{
		"type": "object", "properties": props,
		"required": []string{"type", "content"}, "additionalProperties": false,
	}
}

func (t *SaveFoundationTool) v3BranchReq(typeName string, contentSchema map[string]any, extraProps map[string]map[string]any, alsoReq ...string) map[string]any {
	props := map[string]any{"type": constEnum(typeName), "content": contentSchema}
	for k, v := range extraProps {
		props[k] = v
	}
	reqd := []string{"type", "content"}
	reqd = append(reqd, alsoReq...)
	return map[string]any{
		"type": "object", "properties": props,
		"required": reqd, "additionalProperties": false,
	}
}

func (t *SaveFoundationTool) v3Schema() map[string]any {
	chReq := chapterOutlineSchema(true, true)  // flat outline: chapter required
	chOpt := chapterOutlineSchema(true, false) // planning paths: chapter optional
	vol := volumeOutlineSchema(true)
	scaleEnum := map[string]any{"type": "string", "enum": []string{"short", "mid", "long"}}
	// 根必须是显式 object：DeepSeek 会拒绝无根 type 的 tool parameters
	// （报 got 'type: null'），例如 {"anyOf":[...]}。
	return map[string]any{
		"type": "object",
		"anyOf": []any{
			t.v3Branch("premise", map[string]any{"type": "string", "description": "premise 前提（Markdown 字符串）"}, map[string]map[string]any{"scale": scaleEnum}),
			t.v3Branch("outline", map[string]any{"type": "array", "description": "outline: Chapter 对象数组", "items": chReq, "minItems": 1}, map[string]map[string]any{"scale": scaleEnum}),
			t.v3Branch("layered_outline", map[string]any{"type": "array", "description": "layered_outline: Volume 对象数组", "items": vol, "minItems": 1}, map[string]map[string]any{"scale": scaleEnum}),
			t.v3BranchReq("expand_arc", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":    map[string]any{"type": "string", "description": "弧标题"},
					"goal":     map[string]any{"type": "string", "description": "弧目标"},
					"chapters": map[string]any{"type": "array", "description": "章节数组", "items": chOpt, "minItems": 1},
				},
				"required": []string{"title", "goal", "chapters"}, "additionalProperties": false,
			}, map[string]map[string]any{
				"volume": map[string]any{"type": "integer", "description": "目标卷序号"},
				"arc":    map[string]any{"type": "integer", "description": "目标弧序号"},
			}, "volume", "arc"),
			t.v3BranchReq("append_volume", vol, map[string]map[string]any{
				"reason": map[string]any{"type": "string", "description": "卷末判定理由"},
			}, "reason"),
			t.v3Branch("characters", map[string]any{"type": "array", "items": characterSchema()}, map[string]map[string]any{"scale": scaleEnum}),
			t.v3Branch("world_rules", map[string]any{"type": "array", "items": worldRuleSchema()}, map[string]map[string]any{"scale": scaleEnum}),
			t.v3Branch("update_compass", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ending_direction": map[string]any{"type": "string"},
					"direction":        map[string]any{"type": "string"},
					"open_threads":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"estimated_scale":  map[string]any{"type": "string"},
					"long":             map[string]any{"type": "object", "properties": map[string]any{"ending_direction": map[string]any{"type": "string"}, "open_threads": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "estimated_scale": map[string]any{"type": "string"}}, "additionalProperties": false},
					"current":          map[string]any{"type": "object", "properties": map[string]any{"direction": map[string]any{"type": "string"}, "open_threads": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
				},
				"additionalProperties": false,
			}, map[string]map[string]any{
				"section": map[string]any{"type": "string", "enum": []string{"long", "current"}},
				"reason":  map[string]any{"type": "string", "description": "long compass 更新原因"},
			}),
			t.v3BranchReq("complete_book", map[string]any{"type": "object", "description": "complete_book: 空对象 {}", "additionalProperties": false}, map[string]map[string]any{
				"reason": map[string]any{"type": "string", "description": "完结判定理由"},
			}, "reason"),
		},
	}
}

func (t *SaveFoundationTool) core4Schema() map[string]any {
	chReq := chapterOutlineSchema(false, true)  // flat outline: chapter required
	chOpt := chapterOutlineSchema(false, false) // planning paths: chapter optional
	_ = chOpt                                   // used implicitly through vol → arcOutlineSchema → chapterOutlineSchema(false, false)
	vol := volumeOutlineSchema(false)
	return schema.Object(
		schema.Property("type", schema.Enum("设定类型", "premise", "outline", "layered_outline", "characters", "world_rules", "expand_arc", "append_volume", "update_compass", "complete_book")).Required(),
		schema.Property("content", map[string]any{
			"anyOf": []any{
				map[string]any{"type": "string", "description": "premise 前提（Markdown 字符串）"},
				map[string]any{"type": "array", "items": chReq, "description": "outline: Chapter 对象数组"},
				map[string]any{"type": "array", "items": vol, "description": "layered_outline: Volume 对象数组"},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":    scenePropString("弧标题"),
						"goal":     scenePropString("弧目标"),
						"chapters": map[string]any{"type": "array", "items": chOpt, "description": "章节数组"},
					},
					"required": []string{"title", "goal"},
				},
				func() map[string]any { v := vol; v["description"] = "append_volume: Volume 对象"; return v }(),
				map[string]any{"type": "array", "items": map[string]any{"type": "object", "description": "角色或规则记录"}, "description": "characters 或 world_rules: 对象数组"},
				map[string]any{"type": "object", "description": "update_compass 补丁对象 / complete_book 空对象"},
			},
			"description": "内容。接受 string（premise）、array（outline/layered_outline/characters/world_rules）、object（expand_arc/append_volume/update_compass/complete_book），也可传 JSON 字符串框架自动解析。",
		}).Required(),
		schema.Property("scale", schema.Enum("规划级别", "short", "mid", "long")),
		schema.Property("volume", schema.Int("目标卷序号（仅 expand_arc 时必传）")),
		schema.Property("arc", schema.Int("目标弧序号（仅 expand_arc 时必传）")),
		schema.Property("section", schema.Enum("update_compass 可选更新分区", "long", "current")),
		schema.Property("reason", schema.String("卷末判定理由（append_volume / complete_book 时必填）或更新 long compass 的必要原因")),
	)
}

// ── Typed command and valid types ──

var validTypes = map[string]bool{
	"premise": true, "outline": true, "layered_outline": true,
	"characters": true, "world_rules": true,
	"expand_arc": true, "append_volume": true,
	"update_compass": true, "complete_book": true,
}

// completeBookContent 是 complete_book content 的严格空对象类型。
// completeBookPayload 是 Phase 1 校验后传给提交阶段的类型化负载。
type completeBookContent struct{}
type completeBookPayload struct{}

type appendVolumePayload struct {
	Volume      domain.VolumeOutline
	PriorFinale int
}

// typedCommand 携带已解码和验证的完整命令。Payload 由 Phase 1 设置，Phase 2 使用。
type typedCommand struct {
	Type    string
	Content string // 原始内容字符串（仅 Phase 1 解码/校验）
	Scale   string
	Volume  int
	Arc     int
	Section string
	Reason  string
	Payload any // 已解码的类型化负载，由 Phase 1 设置
	// VolumeEndFacts 在 Phase 1 从已校验可读的 progress 生成，
	// 避免提交阶段在首次写入后再读取前置状态。
	VolumeEndFacts json.RawMessage
}

// ── EXECUTE: pure validate-then-write ──

func (t *SaveFoundationTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	isV3 := t.contract != nil && t.contract.GetContract() == projectprofile.ContractSceneBeatV3
	var v3ArgKeys map[string]json.RawMessage
	args = normalizeIntegerStringFields(args, "volume", "arc")

	// ── Phase 0: decode raw args into typed command ──
	var raw struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Scale   string          `json:"scale"`
		Volume  int             `json:"volume"`
		Arc     int             `json:"arc"`
		Section string          `json:"section"`
		Reason  string          `json:"reason"`
	}
	if isV3 {
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
		}
		if err := json.Unmarshal(args, &v3ArgKeys); err != nil {
			return nil, fmt.Errorf("invalid args object: %w: %w", errs.ErrToolArgs, err)
		}
		var trailing json.RawMessage
		if err := dec.Decode(&trailing); err == nil {
			return nil, fmt.Errorf("invalid args: trailing JSON value: %w", errs.ErrToolArgs)
		} else if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("invalid args: trailing data: %w: %w", errs.ErrToolArgs, err)
		}
	} else if err := json.Unmarshal(args, &raw); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if raw.Type == "" {
		return nil, fmt.Errorf("type is required: %w", errs.ErrToolArgs)
	}
	if !validTypes[raw.Type] {
		return nil, fmt.Errorf("unknown type %q, expected premise/outline/layered_outline/characters/world_rules/expand_arc/append_volume/update_compass/complete_book: %w", raw.Type, errs.ErrToolArgs)
	}
	if isV3 {
		if err := validateV3ArgumentKeys(raw.Type, v3ArgKeys); err != nil {
			return nil, err
		}
	}
	contentStr, err := normalizeFoundationContent(raw.Content)
	if err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	if contentStr == "" {
		return nil, fmt.Errorf("content is required: %w", errs.ErrToolArgs)
	}

	cmd := typedCommand{
		Type: raw.Type, Content: contentStr, Scale: raw.Scale,
		Volume: raw.Volume, Arc: raw.Arc, Section: raw.Section, Reason: raw.Reason,
	}

	// ── Phase 1: pure validation + decode into typed payload — no writes ──

	if cmd.Type == "premise" {
		if !strings.HasPrefix(strings.TrimSpace(cmd.Content), "#") {
			return nil, fmt.Errorf("premise must be Markdown starting with '# 书名': %w", errs.ErrToolArgs)
		}
	}
	if cmd.Scale != "" {
		switch domain.PlanningTier(cmd.Scale) {
		case domain.PlanningTierShort, domain.PlanningTierMid, domain.PlanningTierLong:
		default:
			return nil, fmt.Errorf("invalid scale %q, expected short/mid/long: %w", cmd.Scale, errs.ErrToolArgs)
		}
	}
	if cmd.Type == "outline" || cmd.Type == "layered_outline" {
		progress, err := t.store.Progress.Load()
		if err != nil {
			return nil, fmt.Errorf("%s: load progress: %w: %w", cmd.Type, errs.ErrStoreRead, err)
		}
		if progress != nil && progress.Phase == domain.PhaseWriting {
			return nil, fmt.Errorf("writing phase forbids full %s; use expand_arc or append_volume: %w", cmd.Type, errs.ErrToolPrecondition)
		}
	}

	// Decode payload into concrete type and set cmd.Payload
	switch cmd.Type {
	case "premise":
		cmd.Payload = cmd.Content
	case "characters":
		var chars []domain.Character
		if err := decodeFoundationJSON("characters", cmd.Content, &chars); err != nil {
			return nil, err
		}
		cmd.Payload = chars
	case "world_rules":
		var rules []domain.WorldRule
		if err := decodeFoundationJSON("world_rules", cmd.Content, &rules); err != nil {
			return nil, err
		}
		cmd.Payload = rules
	case "update_compass":
		payload, err := t.mergeCompassUpdate(cmd.Content, cmd.Section, cmd.Reason)
		if err != nil {
			return nil, err
		}
		// final ending_direction 必须非空
		if strings.TrimSpace(payload.Compass.Long.EndingDirection) == "" {
			return nil, fmt.Errorf("update_compass: ending_direction is required: %w", errs.ErrToolArgs)
		}
		cmd.Payload = payload
	case "outline":
		var entries []domain.OutlineEntry
		if err := decodeFoundationJSON("outline", cmd.Content, &entries); err != nil {
			return nil, err
		}
		if isV3 && len(entries) == 0 {
			return nil, fmt.Errorf("outline: chapters array is empty: %w", errs.ErrToolArgs)
		}
		if isV3 {
			for i, e := range entries {
				if e.Chapter <= 0 {
					return nil, fmt.Errorf("outline: chapters[%d].chapter must be > 0: %w", i, errs.ErrToolArgs)
				}
			}
		}
		if err := t.validateChapterScenes(entries); err != nil {
			return nil, err
		}
		cmd.Payload = entries
	case "layered_outline":
		var planVolumes []PlanningVolumeInput
		if err := decodeFoundationJSON("layered_outline", cmd.Content, &planVolumes); err != nil {
			return nil, err
		}
		if isV3 && len(planVolumes) == 0 {
			return nil, fmt.Errorf("layered_outline: volumes array is empty: %w", errs.ErrToolArgs)
		}
		var semErrors planValidationErrors
		for vi, pv := range planVolumes {
			vPath := fmt.Sprintf("volumes[%d]", vi)
			if pv.Index <= 0 {
				semErrors.addErrorf(vPath, "INVALID_INDEX",
					"volume index %d must be positive", pv.Index)
			}
			if len(pv.Arcs) == 0 {
				if isV3 {
					return nil, fmt.Errorf("layered_outline: volume %d has no arcs: %w", pv.Index, errs.ErrToolArgs)
				}
				semErrors.addErrorf(vPath, "EMPTY_VOLUME",
					"volume %d has no arcs", pv.Index)
				continue
			}
			// 第一条全局弧（全书第一个弧）必须 detailed
			if vi == 0 && !pv.Arcs[0].IsDetailed() {
				return nil, fmt.Errorf("layered_outline: first volume's first arc must be detailed (non-empty chapters): %w", errs.ErrToolArgs)
			}
			for ai, a := range pv.Arcs {
				aPath := fmt.Sprintf("%s.arcs[%d]", vPath, ai)
				if a.Index <= 0 {
					semErrors.addErrorf(aPath, "INVALID_INDEX",
						"arc %d index must be positive", a.Index)
				}
				if a.IsSkeleton() {
					// skeleton arc: skip scene validation
					continue
				}
				if !a.IsDetailed() {
					semErrors.addErrorf(aPath, "EMPTY_ARC",
						"volume %d arc %d: must have non-empty chapters or estimated_chapters>0", pv.Index, a.Index)
					continue
				}
				// detailed arc: aggregate scene validation errors
				chs := make([]domain.OutlineEntry, len(a.Chapters))
				for ci, c := range a.Chapters {
					chs[ci] = c.toDomain()
				}
				for _, sceneErr := range t.validateChapterScenesAggregated(chs) {
					semErrors.addError(aPath+".chapters", "INVALID_SCENE", sceneErr.Error())
				}
			}
		}
		if len(semErrors) > 0 {
			return nil, fmt.Errorf("layered_outline: %w: %w", semErrors, errs.ErrToolArgs)
		}
		// 转换为 domain 模型
		volumes := make([]domain.VolumeOutline, len(planVolumes))
		for i, pv := range planVolumes {
			volumes[i] = pv.toDomain()
		}
		// 在任意写入前完成编号/拓扑校验
		if err := t.store.Outline.ValidateLayeredOutline(volumes); err != nil {
			return nil, fmt.Errorf("layered_outline: %w: %w", err, errs.ErrToolArgs)
		}
		cmd.Payload = volumes
	case "expand_arc":
		if cmd.Volume <= 0 || cmd.Arc <= 0 {
			return nil, fmt.Errorf("expand_arc requires volume and arc parameters: %w", errs.ErrToolArgs)
		}
		var expansion domain.ArcExpansion
		if err := decodeFoundationJSON("expand_arc", cmd.Content, &expansion); err != nil {
			return nil, err
		}
		if strings.TrimSpace(expansion.Title) == "" {
			return nil, fmt.Errorf("expand_arc: title is required: %w", errs.ErrToolArgs)
		}
		if strings.TrimSpace(expansion.Goal) == "" {
			return nil, fmt.Errorf("expand_arc: goal is required: %w", errs.ErrToolArgs)
		}
		if len(expansion.Chapters) == 0 {
			return nil, fmt.Errorf("expand_arc: chapters array is empty: %w", errs.ErrToolArgs)
		}
		if err := t.validateChapterScenes(expansion.Chapters); err != nil {
			return nil, err
		}
		// Store.ExpandArc 在写大纲后才读 progress；在这里先确认其可读，
		// 避免已修改 outline 后才因损坏的 progress 失败。
		if _, err := t.store.Progress.Load(); err != nil {
			return nil, fmt.Errorf("expand_arc: load progress: %w: %w", errs.ErrStoreRead, err)
		}
		priorVols, loadErr := t.store.Outline.LoadLayeredOutline()
		if loadErr != nil {
			return nil, fmt.Errorf("expand_arc: load layered outline: %w: %w", errs.ErrStoreRead, loadErr)
		}
		var target *domain.ArcOutline
		for vi := range priorVols {
			if priorVols[vi].Index != cmd.Volume {
				continue
			}
			for ai := range priorVols[vi].Arcs {
				if priorVols[vi].Arcs[ai].Index == cmd.Arc {
					target = &priorVols[vi].Arcs[ai]
					break
				}
			}
		}
		if target == nil {
			return nil, fmt.Errorf("expand_arc: volume %d arc %d not found in layered outline: %w", cmd.Volume, cmd.Arc, errs.ErrToolArgs)
		}
		if target.IsExpanded() {
			current := domain.ArcExpansion{Title: target.Title, Goal: target.Goal, Chapters: target.Chapters}
			if !reflect.DeepEqual(current, expansion) {
				return nil, fmt.Errorf("expand_arc: volume %d arc %d already expanded with different content: %w", cmd.Volume, cmd.Arc, errs.ErrToolPrecondition)
			}
		}
		// 在任意写入前做前瞻性拓扑/编号校验：深拷贝 priorVols、应用 expansion、校验
		prospective := deepCopyVolumes(priorVols)
		applied := false
		for vi := range prospective {
			if prospective[vi].Index != cmd.Volume {
				continue
			}
			for ai := range prospective[vi].Arcs {
				if prospective[vi].Arcs[ai].Index == cmd.Arc {
					prospective[vi].Arcs[ai].Title = expansion.Title
					prospective[vi].Arcs[ai].Goal = expansion.Goal
					prospective[vi].Arcs[ai].Chapters = expansion.Chapters
					prospective[vi].Arcs[ai].EstimatedChapters = 0
					applied = true
					break
				}
			}
			if applied {
				break
			}
		}
		if err := t.store.Outline.ValidateLayeredOutline(prospective); err != nil {
			return nil, fmt.Errorf("expand_arc: 展开后拓扑/编号非法: %w: %w", err, errs.ErrToolPrecondition)
		}
		cmd.Payload = expansion
	case "append_volume":
		if strings.TrimSpace(cmd.Reason) == "" {
			return nil, fmt.Errorf("append_volume 必须带 reason 参数: %w", errs.ErrToolArgs)
		}
		var planVol PlanningVolumeInput
		if err := decodeFoundationJSON("append_volume", cmd.Content, &planVol); err != nil {
			return nil, err
		}
		if planVol.Index <= 0 {
			return nil, fmt.Errorf("append_volume: volume index must be positive: %w", errs.ErrToolArgs)
		}
		if len(planVol.Arcs) == 0 {
			return nil, fmt.Errorf("append_volume: volume has no arcs: %w", errs.ErrToolArgs)
		}
		// 新卷首弧必须 detailed
		if !planVol.Arcs[0].IsDetailed() {
			return nil, fmt.Errorf("append_volume: first arc must contain expanded chapters: %w", errs.ErrToolArgs)
		}
		var semErrors planValidationErrors
		for ai, a := range planVol.Arcs {
			aPath := fmt.Sprintf("arcs[%d]", ai)
			if a.Index <= 0 {
				semErrors.addErrorf(aPath, "INVALID_INDEX",
					"arc %d index must be positive", a.Index)
			}
			if a.IsSkeleton() {
				continue
			}
			if !a.IsDetailed() {
				semErrors.addErrorf(aPath, "EMPTY_ARC",
					"arc %d: must have non-empty chapters or estimated_chapters>0", a.Index)
				continue
			}
			chs := make([]domain.OutlineEntry, len(a.Chapters))
			for ci, c := range a.Chapters {
				chs[ci] = c.toDomain()
			}
			for _, sceneErr := range t.validateChapterScenesAggregated(chs) {
				semErrors.addError(aPath+".chapters", "INVALID_SCENE", sceneErr.Error())
			}
		}
		if len(semErrors) > 0 {
			return nil, fmt.Errorf("append_volume: %w: %w", semErrors, errs.ErrToolArgs)
		}
		vol := planVol.toDomain()
		progress, err := t.store.Progress.Load()
		if err != nil {
			return nil, fmt.Errorf("append_volume: load progress: %w: %w", errs.ErrStoreRead, err)
		}
		if progress != nil && progress.Phase == domain.PhaseComplete {
			return nil, fmt.Errorf("全书已完结（phase=complete），不允许追加新卷: %w", errs.ErrToolPrecondition)
		}
		priorVols, err := t.store.Outline.LoadLayeredOutline()
		if err != nil {
			return nil, fmt.Errorf("append_volume: load layered outline: %w: %w", errs.ErrStoreRead, err)
		}
		// LoadLayeredOutline 对文件不存在返回 (nil, nil)，但 Store.AppendVolume
		// 的提交路径要求已有 layered_outline.json。必须在任何写入前
		// 显式预检，否则 scale 会先落盘、然后 append 才失败。
		if _, err := os.Stat(filepath.Join(t.store.Dir(), "layered_outline.json")); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("append_volume: layered outline is not initialized: %w", errs.ErrToolPrecondition)
			}
			return nil, fmt.Errorf("append_volume: stat layered outline: %w: %w", errs.ErrStoreRead, err)
		}
		if len(priorVols) > 0 && vol.Index <= priorVols[len(priorVols)-1].Index {
			return nil, fmt.Errorf("append_volume: volume index %d must be greater than last existing volume %d: %w",
				vol.Index, priorVols[len(priorVols)-1].Index, errs.ErrToolArgs)
		}
		// 在任意写入前做前瞻性拓扑/编号校验：构造完整 combined 卷集并校验
		prospective := append(deepCopyVolumes(priorVols), vol)
		if err := t.store.Outline.ValidateLayeredOutline(prospective); err != nil {
			return nil, fmt.Errorf("append_volume: 追加后拓扑/编号非法: %w: %w", err, errs.ErrToolPrecondition)
		}
		cmd.Payload = appendVolumePayload{Volume: vol, PriorFinale: domain.FinaleVolume(priorVols)}
		cmd.VolumeEndFacts = makeVolumeEndFacts(progress)
	case "complete_book":
		// Strict struct decoder: 只接收严格空对象 {} / { }，拒 "{}" 及任意属性
		if trimmed := bytes.TrimSpace(raw.Content); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("complete_book content must be an empty JSON object {}: %w", errs.ErrToolArgs)
		}
		dec := json.NewDecoder(bytes.NewReader(raw.Content))
		dec.DisallowUnknownFields()
		var cbContent completeBookContent
		if err := dec.Decode(&cbContent); err != nil {
			return nil, fmt.Errorf("complete_book content must be empty object {}, without any properties: %w: %w", errs.ErrToolArgs, err)
		}
		var trailing json.RawMessage
		if err := dec.Decode(&trailing); err == nil {
			return nil, fmt.Errorf("complete_book content: trailing data after empty object: %w", errs.ErrToolArgs)
		} else if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("complete_book content: unexpected content after JSON object: %w", errs.ErrToolArgs)
		}
		if strings.TrimSpace(cmd.Reason) == "" {
			return nil, fmt.Errorf("complete_book 必须带 reason 参数: %w", errs.ErrToolArgs)
		}
		progress, perr := t.store.Progress.Load()
		if perr != nil {
			return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, perr)
		}
		if progress == nil {
			return nil, fmt.Errorf("progress 未初始化: %w", errs.ErrToolPrecondition)
		}
		if progress.Phase != domain.PhaseWriting {
			return nil, fmt.Errorf("complete_book 仅在 writing 阶段可调用: %w", errs.ErrToolPrecondition)
		}
		if len(progress.PendingRewrites) > 0 {
			return nil, fmt.Errorf("有 %d 章在返工队列中: %w", len(progress.PendingRewrites), errs.ErrToolPrecondition)
		}
		if len(progress.CompletedChapters) == 0 {
			return nil, fmt.Errorf("一章未写不可完本: %w", errs.ErrToolPrecondition)
		}
		if next := progress.NextChapter(); progress.TotalChapters > 0 && next <= progress.TotalChapters {
			return nil, fmt.Errorf("大纲内还有未写章节（下一章 %d/共 %d）: %w", next, progress.TotalChapters, errs.ErrToolPrecondition)
		}
		cmd.Payload = completeBookPayload{}
		cmd.VolumeEndFacts = makeVolumeEndFacts(progress)
	}

	// ── Phase 2: writes — only reached if Phase 1 passed. No re-decoding; use cmd.Payload. ──
	// PlanningTier 写入仅对非卷末/完结类型的有效命令执行；complete_book 不写，
	// 其所有预条件已在 Phase 1 完成。

	result := map[string]any{"saved": true, "type": cmd.Type, "scale": cmd.Scale}

	if cmd.Scale != "" && cmd.Type != "complete_book" {
		if err := t.store.RunMeta.SetPlanningTier(domain.PlanningTier(cmd.Scale)); err != nil {
			return nil, fmt.Errorf("save planning tier: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	volumeEnd := cmd.Type == "append_volume" || cmd.Type == "complete_book"

	switch cmd.Type {
	case "premise":
		content := cmd.Payload.(string)
		name := domain.ExtractNovelNameFromPremise(content)
		if err := t.store.Outline.SavePremise(content); err != nil {
			return nil, fmt.Errorf("save premise: %w: %w", errs.ErrStoreWrite, err)
		}
		if name != "" {
			_ = t.store.Progress.SetNovelName(name)
			result["novel_name"] = name
		}
		_ = t.store.Progress.UpdatePhase(domain.PhasePremise)

	case "outline":
		entries := cmd.Payload.([]domain.OutlineEntry)
		if err := t.store.Outline.SaveOutline(entries); err != nil {
			return nil, fmt.Errorf("save outline: %w: %w", errs.ErrStoreWrite, err)
		}
		_ = t.store.Progress.UpdatePhase(domain.PhaseOutline)
		_ = t.store.Progress.SetTotalChapters(len(entries))
		if domain.PlanningTier(cmd.Scale) != domain.PlanningTierLong {
			_ = t.store.Progress.SetLayered(false)
			_ = t.store.Progress.UpdateVolumeArc(0, 0)
			_ = t.store.Outline.ClearLayeredOutline()
		}
		result["chapters"] = len(entries)

	case "layered_outline":
		volumes := cmd.Payload.([]domain.VolumeOutline)
		if err := t.store.Outline.SaveLayeredOutline(volumes); err != nil {
			return nil, fmt.Errorf("save layered_outline: %w: %w", errs.ErrStoreWrite, err)
		}
		flat := domain.FlattenOutline(volumes)
		if err := t.store.Outline.SaveOutline(flat); err != nil {
			return nil, fmt.Errorf("save flattened outline: %w: %w", errs.ErrStoreWrite, err)
		}
		total := domain.TotalChapters(volumes)
		_ = t.store.Progress.UpdatePhase(domain.PhaseOutline)
		_ = t.store.Progress.SetTotalChapters(total)
		_ = t.store.Progress.SetLayered(true)
		if len(volumes) > 0 && len(volumes[0].Arcs) > 0 {
			_ = t.store.Progress.UpdateVolumeArc(volumes[0].Index, volumes[0].Arcs[0].Index)
		}
		result["volumes"] = len(volumes)
		result["chapters"] = total

	case "characters":
		chars := cmd.Payload.([]domain.Character)
		if err := t.store.Characters.Save(chars); err != nil {
			return nil, fmt.Errorf("save characters: %w: %w", errs.ErrStoreWrite, err)
		}
		result["count"] = len(chars)

	case "world_rules":
		rules := cmd.Payload.([]domain.WorldRule)
		// 保存前量控校验：条数达到软上限即告警（提示合并/移除过期规则，允许保存）；
		// 总序列化字节超硬上限则拒绝保存（防止规则集膨胀挤占上下文预算，禁静默截断）。
		if len(rules) >= domain.MaxWorldRulesEntries {
			result["warning"] = fmt.Sprintf("world_rules 共 %d 条，达到/超过软上限 %d 条：建议合并或移除过期规则（已保存，仅提示）",
				len(rules), domain.MaxWorldRulesEntries)
		}
		// 与 store 落盘格式一致（MarshalIndent），量测实际持久化字节数。
		if data, err := json.MarshalIndent(rules, "", "  "); err != nil {
			return nil, fmt.Errorf("world_rules: serialize: %w: %w", errs.ErrStoreWrite, err)
		} else if len(data) > domain.MaxWorldRulesBytes {
			return nil, fmt.Errorf("world_rules 总序列化大小 %d 字节超过硬上限 %d 字节，拒绝保存：请合并/移除过期规则后重试: %w",
				len(data), domain.MaxWorldRulesBytes, errs.ErrToolArgs)
		}
		if err := t.store.World.SaveWorldRules(rules); err != nil {
			return nil, fmt.Errorf("save world_rules: %w: %w", errs.ErrStoreWrite, err)
		}
		result["count"] = len(rules)

	case "expand_arc":
		expansion := cmd.Payload.(domain.ArcExpansion)
		if err := t.store.ExpandArc(cmd.Volume, cmd.Arc, expansion); err != nil {
			return nil, fmt.Errorf("expand arc: %w: %w", errs.ErrStoreWrite, err)
		}
		result["volume"] = cmd.Volume
		result["arc"] = cmd.Arc
		result["title"] = expansion.Title
		result["goal"] = expansion.Goal
		result["chapters"] = len(expansion.Chapters)
		t.consumeWriterFeedback()

	case "append_volume":
		payload := cmd.Payload.(appendVolumePayload)
		vol := payload.Volume
		if err := t.store.AppendVolume(vol); err != nil {
			return nil, fmt.Errorf("append volume: %w: %w", errs.ErrStoreWrite, err)
		}
		result["volume"] = vol.Index
		if vol.Final {
			result["final_volume"] = true
		} else if payload.PriorFinale > 0 {
			result["finale_released"] = true
		}
		result["arcs"] = len(vol.Arcs)
		if chCount := countChapters(vol.Arcs); chCount > 0 {
			result["chapters"] = chCount
		}
		t.consumeWriterFeedback()

	case "complete_book":
		_ = cmd.Payload.(completeBookPayload) // phase 1 marker; consumed here, no re-decode
		if err := t.store.Progress.MarkComplete(); err != nil {
			return nil, fmt.Errorf("mark complete: %w: %w", errs.ErrStoreWrite, err)
		}
		result["book_complete"] = true
		result["phase"] = string(domain.PhaseComplete)

	case "update_compass":
		payload := cmd.Payload.(compassUpdatePayload)
		compass := payload.Compass
		if payload.Proposal != nil {
			outcome, err := t.requestLongCompassApproval(ctx, *payload.Proposal)
			if err != nil {
				return nil, err
			}
			if outcome == longApprovalApproved {
				latest, err := t.store.Outline.LoadCompass()
				if err != nil {
					return nil, fmt.Errorf("recheck compass before approved long update: %w: %w", errs.ErrStoreRead, err)
				}
				if latest == nil || !reflect.DeepEqual(latest.Long, payload.Proposal.Base) {
					outcome = longApprovalStale
				}
				if outcome == longApprovalApproved {
					// 审批等待期间可能已有 room 被删除，重新验证 marker targets
					if err := t.revalidateMarkerTargets(compass.Long.OpenThreads); err != nil {
						return nil, err
					}
				}
			}
			t.recordLongCompassApproval(*payload.Proposal, outcome)
			result["long_proposal_id"] = payload.Proposal.ID
			result["long_approval"] = string(outcome)
			if outcome != longApprovalApproved {
				// 审批拒绝/超时不是工具失败：保持磁盘上的 long，并让 Architect 继续。
				latest, err := t.store.Outline.LoadCompass()
				if err != nil {
					return nil, fmt.Errorf("reload compass after rejected long update: %w: %w", errs.ErrStoreRead, err)
				}
				if latest != nil {
					compass.Long = latest.Long
				} else {
					compass.Long = payload.Proposal.Base
				}
				result["long_saved"] = false
				result["continued"] = true
				if !payload.CurrentRequested {
					if _, err := t.store.Checkpoints.Append(domain.GlobalScope(), "update_compass_rejected", "", "proposal:"+payload.Proposal.ID); err != nil {
						return nil, fmt.Errorf("checkpoint rejected long proposal: %w: %w", errs.ErrStoreWrite, err)
					}
					result["saved"] = false
					return json.Marshal(result)
				}
			}
		}
		// 全部 long compass 保存都走临界区路径（LockPlanningAndOutline），确保
		// marker 校验与 compass 保存原子化，防止并发 DeleteArchiveEntrySafe 在
		// 校验与保存之间制造 dangling marker。锁顺序与 delete 一致。
		if err := t.store.SaveCompassWithMarkerCheck(compass, func(c domain.StoryCompass, archive *domain.PlanningArchiveV1) error {
			// 只对含有 [room:...] marker 的 long open_threads 做重校验
			hasMarkers := false
			for _, t := range c.Long.OpenThreads {
				if strings.Contains(t, "[room:") {
					hasMarkers = true
					break
				}
			}
			if !hasMarkers {
				return nil
			}
			return t.revalidateMarkerTargetsLocked(c.Long.OpenThreads, archive, c.Long.Reference)
		}); err != nil {
			return nil, fmt.Errorf("save compass: %w: %w", errs.ErrStoreWrite, err)
		}
		result["section"] = cmd.Section
		result["long"] = compass.Long
		result["current"] = compass.Current
		result["ending_direction"] = compass.Long.EndingDirection
		result["last_updated"] = compass.Long.LastUpdated
		if payload.Proposal != nil {
			if result["long_approval"] == string(longApprovalApproved) {
				result["long_saved"] = true
			}
			if payload.CurrentRequested {
				result["current_saved"] = true
			}
		}
		t.consumeWriterFeedback()
	}

	// checkpoint
	scope := domain.GlobalScope()
	if cmd.Type == "expand_arc" {
		scope = domain.ArcScope(cmd.Volume, cmd.Arc)
	}
	if _, err := t.store.Checkpoints.AppendArtifact(scope, cmd.Type, foundationArtifact(cmd.Type)); err != nil {
		return nil, fmt.Errorf("checkpoint foundation %s: %w: %w", cmd.Type, errs.ErrStoreWrite, err)
	}
	if volumeEnd {
		t.recordVolumeEndDecision(cmd.Type, cmd.Reason, cmd.VolumeEndFacts, result)
	}

	remaining := t.store.FoundationMissing()
	ready := len(remaining) == 0
	result["remaining"] = remaining
	result["foundation_ready"] = ready
	if ready {
		if p, _ := t.store.Progress.Load(); p != nil &&
			p.Phase != domain.PhaseWriting && p.Phase != domain.PhaseComplete {
			_ = t.store.Progress.UpdatePhase(domain.PhaseWriting)
			result["phase"] = string(domain.PhaseWriting)
		}
	}
	return json.Marshal(result)
}

func validateV3ArgumentKeys(typeName string, keys map[string]json.RawMessage) error {
	allowed := map[string]bool{"type": true, "content": true}
	switch typeName {
	case "premise", "outline", "layered_outline":
		allowed["scale"] = true
	case "expand_arc":
		allowed["volume"] = true
		allowed["arc"] = true
	case "append_volume", "complete_book":
		allowed["reason"] = true
	case "update_compass":
		allowed["section"] = true
		allowed["reason"] = true
	case "characters", "world_rules":
		allowed["scale"] = true
	}
	for key := range keys {
		if !allowed[key] {
			return fmt.Errorf("%s: field %q is not allowed for this command: %w", typeName, key, errs.ErrToolArgs)
		}
	}
	return nil
}

// ValidateOutlineEntry validates outline entry scenes against contract.
// Shared by plan/draft/engine.
// V3: entry must exist, scenes must be non-empty, every scene must pass contract.Validate.
// Core4: entry may be missing or have legacy scenes; non-legacy scenes are validated.
// Load failure always fails closed.
func ValidateOutlineEntry(st *store.Store, contract *projectprofile.SceneBeatContract, chapter int) error {
	if contract == nil {
		return nil
	}
	outline, err := st.Outline.LoadOutline()
	if err != nil {
		return fmt.Errorf("validate outline entry: load outline: %w", err)
	}
	isV3 := contract.GetContract() == projectprofile.ContractSceneBeatV3
	for _, entry := range outline {
		if entry.Chapter == chapter {
			if isV3 {
				if len(entry.Scenes) == 0 {
					return fmt.Errorf("validate outline entry: chapter %d has no scenes (v3 requires non-empty scenes)", chapter)
				}
			}
			for j, sc := range entry.Scenes {
				if err := contract.Validate(sc); err != nil {
					return fmt.Errorf("validate outline entry: chapter %d scene[%d]: %w", chapter, j, err)
				}
			}
			return nil
		}
	}
	if isV3 {
		return fmt.Errorf("validate outline entry: chapter %d not found in outline (v3 requires entry)", chapter)
	}
	return nil
}

// ── Compass / Helpers ──

type longCompassPatch struct {
	EndingDirection *string   `json:"ending_direction"`
	OpenThreads     *[]string `json:"open_threads"`
	EstimatedScale  *string   `json:"estimated_scale"`
}
type currentCompassPatch struct {
	Direction   *string   `json:"direction"`
	OpenThreads *[]string `json:"open_threads"`
}

type compassUpdatePayload struct {
	Compass          domain.StoryCompass
	Proposal         *longCompassProposal
	CurrentRequested bool
}

type longCompassProposal struct {
	ID       string
	Reason   string
	Base     domain.LongCompass
	Proposed domain.LongCompass
}

type longApprovalOutcome string

const (
	longApprovalApproved    longApprovalOutcome = "approved"
	longApprovalRejected    longApprovalOutcome = "rejected"
	longApprovalTimeout     longApprovalOutcome = "timeout"
	longApprovalUnavailable longApprovalOutcome = "unavailable"
	longApprovalFailed      longApprovalOutcome = "interaction_error"
	longApprovalStale       longApprovalOutcome = "stale"
)

func (t *SaveFoundationTool) mergeCompassUpdate(content, section, reason string) (compassUpdatePayload, error) {
	section = strings.ToLower(strings.TrimSpace(section))
	var longPatch *longCompassPatch
	var currentPatch *currentCompassPatch
	switch section {
	case "long":
		var patch longCompassPatch
		if err := decodeFoundationJSON("long compass", content, &patch); err != nil {
			return compassUpdatePayload{}, err
		}
		longPatch = &patch
	case "current":
		var patch currentCompassPatch
		if err := decodeFoundationJSON("current compass", content, &patch); err != nil {
			return compassUpdatePayload{}, err
		}
		currentPatch = &patch
	case "":
		var patch struct {
			Long    *longCompassPatch    `json:"long"`
			Current *currentCompassPatch `json:"current"`
		}
		if err := decodeFoundationJSON("compass", content, &patch); err != nil {
			return compassUpdatePayload{}, err
		}
		longPatch, currentPatch = patch.Long, patch.Current
		if longPatch == nil && currentPatch == nil {
			var legacy longCompassPatch
			if err := decodeFoundationJSON("long compass", content, &legacy); err != nil {
				return compassUpdatePayload{}, err
			}
			longPatch = &legacy
		}
	default:
		return compassUpdatePayload{}, fmt.Errorf("invalid compass section %q: %w", section, errs.ErrToolArgs)
	}
	if longPatch != nil && strings.TrimSpace(reason) == "" {
		return compassUpdatePayload{}, fmt.Errorf("更新 long compass 必须提供 reason: %w", errs.ErrToolArgs)
	}
	existing, err := t.store.Outline.LoadCompass()
	if err != nil {
		return compassUpdatePayload{}, fmt.Errorf("load compass: %w: %w", errs.ErrStoreRead, err)
	}
	var compass domain.StoryCompass
	if existing != nil {
		compass = *existing
	}
	baseLong := cloneLongCompass(compass.Long)
	lastUpdated := 0
	p, err := t.store.Progress.Load()
	if err != nil {
		return compassUpdatePayload{}, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if p != nil {
		lastUpdated = p.LatestCompleted()
	}
	if longPatch != nil {
		if longPatch.EndingDirection != nil {
			compass.Long.EndingDirection = strings.TrimSpace(*longPatch.EndingDirection)
		}
		if longPatch.OpenThreads != nil {
			newThreads := append([]string(nil), (*longPatch.OpenThreads)...)
			// 对新设 open_threads 做 marker 校验（只校验新增/改写线程）：
			//   - 带 [room:<id>] 的线程须有非空自然语言摘要
			//   - 无畸形/重复 marker（ParseOpenThreadMarkers 保证）
			//   - 目标 room 在 archive 或 legacy 中存在
			if err := t.validateOpenThreadMarkers(newThreads, baseLong.OpenThreads); err != nil {
				return compassUpdatePayload{}, err
			}
			compass.Long.OpenThreads = newThreads
		}
		if longPatch.EstimatedScale != nil {
			compass.Long.EstimatedScale = strings.TrimSpace(*longPatch.EstimatedScale)
		}
		compass.Long.LastUpdated = lastUpdated
	}
	if currentPatch != nil {
		if compass.Current == nil {
			compass.Current = &domain.Compass{}
		}
		if currentPatch.Direction != nil {
			compass.Current.Direction = strings.TrimSpace(*currentPatch.Direction)
		}
		if currentPatch.OpenThreads != nil {
			compass.Current.OpenThreads = append([]string(nil), (*currentPatch.OpenThreads)...)
		}
		compass.Current.LastUpdated = lastUpdated
	}
	payload := compassUpdatePayload{Compass: compass, CurrentRequested: currentPatch != nil}
	if longPatch != nil && longCompassEstablished(baseLong) && !longCompassContentEqual(baseLong, compass.Long) {
		proposal := longCompassProposal{
			Reason: reason, Base: baseLong, Proposed: cloneLongCompass(compass.Long),
		}
		proposal.ID = longCompassProposalID(proposal)
		payload.Proposal = &proposal
	}
	return payload, nil
}

// checkRoomExists 检查指定 roomID 在 planning archive 中存在，或在 archive
// 不存在时从 legacy compass.long.reference 中可解析（复用 domain 的 ExtractLegacyRoomsFromReference）。
func (t *SaveFoundationTool) checkRoomExists(roomID string) error {
	archive, err := t.store.PlanningArchive.Load()
	if err != nil {
		return fmt.Errorf("load planning archive: %w", err)
	}
	return t.roomExistsLockedFallback(roomID, archive)
}

// checkRoomExistsInArchiveOrLegacy 是 checkRoomExists 的非加锁变体，用于已持锁的临界区。
// 注意：在 LockPlanningAndOutline 内不能调用此方法（Outline 锁已持有会导致死锁），
// 应使用 revalidateMarkerTargetsLocked 代替。
func (t *SaveFoundationTool) checkRoomExistsInArchiveOrLegacy(roomID string, archive *domain.PlanningArchiveV1) error {
	return t.roomExistsLocked(roomID, archive, nil)
}

// roomExistsLockedFallback 辅助 checkRoomExists（非临界区版本）：若 archive 不存在，
// 加载 compass 获取 reference 并查询 legacy。
func (t *SaveFoundationTool) roomExistsLockedFallback(roomID string, archive *domain.PlanningArchiveV1) error {
	if archive != nil {
		for _, e := range archive.Entries {
			if e.Kind == "room" && e.ID == roomID {
				return nil
			}
		}
		return fmt.Errorf("在 archive 中未找到")
	}
	compass, cerr := t.store.Outline.LoadCompass()
	if cerr != nil {
		return fmt.Errorf("load compass: %w", cerr)
	}
	if compass == nil {
		return fmt.Errorf("archive 不存在且 legacy reference 不可用")
	}
	return t.roomExistsLocked(roomID, nil, compass.Long.Reference)
}

// validateOpenThreadMarkers 校验 compass.long.open_threads 中新增/改写的 marker。
//
// 只校验相对 baseThreads 有变化（新增或文本不同）的线程；旧 malformed 线程
// 原样保留时不阻断完整数组更新。
//
// 规则：
//   - 带 [room:<id>] 的线程必须能通过 ParseOpenThreadMarkers 严格解析
//   - 带 [room:<id>] 的线程必须有非空自然语言摘要
//   - marker 指向的 room 须在 planning archive 中存在；archive 不存在时
//     尝试 legacy compass.long.reference 回退；都不存在时拒绝。
//   - 无 marker 的普通线程永远允许。
func (t *SaveFoundationTool) validateOpenThreadMarkers(threads []string, baseThreads []string) error {
	// 建立 base 集合供快速比较
	baseSet := make(map[string]int, len(baseThreads))
	for _, bt := range baseThreads {
		baseSet[bt]++
	}
	isNewOrChanged := func(thread string) bool {
		if baseSet[thread] > 0 {
			baseSet[thread]--
			return false
		}
		return true
	}

	for _, thread := range threads {
		if !isNewOrChanged(thread) {
			continue // 未改写的旧线程，跳过校验（即使 malformed）
		}
		parsed, err := ParseOpenThreadMarkers(thread)
		if err != nil {
			return fmt.Errorf("open_threads 条目 %q 解析失败: %w: %w", thread, err, errs.ErrToolArgs)
		}
		if len(parsed.RoomIDs) == 0 {
			continue // 无 marker，永远允许
		}
		// 有 marker 的线程必须有非空摘要
		if strings.TrimSpace(parsed.NaturalSummary) == "" {
			return fmt.Errorf("open_threads 条目 %q 存在 room marker 但缺少非空自然语言摘要: %w", thread, errs.ErrToolArgs)
		}
		// 校验 marker 指向的 room 在 archive 或 legacy 中存在
		for _, roomID := range parsed.RoomIDs {
			if err := t.checkRoomExists(roomID); err != nil {
				return fmt.Errorf("open_threads 条目 %q 引用的 room %q 不存在: %w: %w", thread, roomID, err, errs.ErrToolArgs)
			}
		}
	}
	return nil
}

// revalidateMarkerTargets 在 long approval 批准后、写入 compass 前重新验证
// marker targets 仍然存在（防止审批等待期间 room 被删除导致 dangling marker）。
// 非临界区版本：加载 archive + compass。
func (t *SaveFoundationTool) revalidateMarkerTargets(openThreads []string) error {
	archive, _ := t.store.PlanningArchive.Load()
	compass, _ := t.store.Outline.LoadCompass()
	var ref json.RawMessage
	if compass != nil {
		ref = compass.Long.Reference
	}
	return t.revalidateMarkerTargetsLocked(openThreads, archive, ref)
}

// revalidateMarkerTargetsLocked 使用预加载的 archive + compassRef 做校验（不额外获取锁）。
// 在 LockPlanningAndOutline 临界区内调用。
func (t *SaveFoundationTool) revalidateMarkerTargetsLocked(openThreads []string, archive *domain.PlanningArchiveV1, compassRef json.RawMessage) error {
	for _, thread := range openThreads {
		parsed, err := ParseOpenThreadMarkers(thread)
		if err != nil {
			continue
		}
		for _, roomID := range parsed.RoomIDs {
			if err := t.roomExistsLocked(roomID, archive, compassRef); err != nil {
				return fmt.Errorf("marker 指向的 room %q 在审批期间已不存在，请重新规划: %w", roomID, errs.ErrToolPrecondition)
			}
		}
	}
	return nil
}

// roomExistsLocked 检查 room 在 archive 或 legacy Reference 中存在（不加锁）。
func (t *SaveFoundationTool) roomExistsLocked(roomID string, archive *domain.PlanningArchiveV1, compassRef json.RawMessage) error {
	if archive != nil {
		for _, e := range archive.Entries {
			if e.Kind == "room" && e.ID == roomID {
				return nil
			}
		}
		return fmt.Errorf("在 archive 中未找到")
	}
	// archive 不存在：走 legacy
	rooms, _, found, err := domain.ExtractLegacyRoomsFromReference(compassRef)
	if err != nil || !found || len(rooms) == 0 {
		return fmt.Errorf("archive 不存在且 legacy reference 中无 room 数据")
	}
	for _, r := range rooms {
		if r.ID == roomID {
			return nil
		}
	}
	return fmt.Errorf("archive 不存在且在 legacy reference 中也未找到")
}

func cloneLongCompass(in domain.LongCompass) domain.LongCompass {
	out := in
	out.OpenThreads = append([]string(nil), in.OpenThreads...)
	out.Reference = append(json.RawMessage(nil), in.Reference...)
	return out
}

func longCompassEstablished(long domain.LongCompass) bool {
	return strings.TrimSpace(long.EndingDirection) != "" || len(long.OpenThreads) > 0 || strings.TrimSpace(long.EstimatedScale) != ""
}

func longCompassContentEqual(a, b domain.LongCompass) bool {
	return strings.TrimSpace(a.EndingDirection) == strings.TrimSpace(b.EndingDirection) &&
		strings.TrimSpace(a.EstimatedScale) == strings.TrimSpace(b.EstimatedScale) &&
		reflect.DeepEqual(a.OpenThreads, b.OpenThreads)
}

func longCompassProposalID(p longCompassProposal) string {
	raw, _ := json.Marshal(struct {
		Reason   string             `json:"reason"`
		Base     domain.LongCompass `json:"base"`
		Proposed domain.LongCompass `json:"proposed"`
	}{p.Reason, p.Base, p.Proposed})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("long-%x", sum[:6])
}

func (t *SaveFoundationTool) requestLongCompassApproval(ctx context.Context, proposal longCompassProposal) (longApprovalOutcome, error) {
	if t.longApproval == nil {
		return longApprovalUnavailable, nil
	}
	timeout := t.longApprovalTimeout
	if timeout <= 0 {
		timeout = DefaultLongApprovalTimeout
	}
	question := Question{
		Header: "长线更新审批",
		Question: fmt.Sprintf(
			"Architect 请求修改 compass.long（提案 %s）。\n原因：%s\n%s\n\n请在 30 分钟内决定；未批准将自动拒绝本次 long 更新并继续创作。",
			proposal.ID, strings.TrimSpace(proposal.Reason), describeLongCompassChange(proposal.Base, proposal.Proposed),
		),
		Options: []Option{
			{Label: "批准", Description: "按提案写入 compass.long，并继续创作"},
			{Label: "拒绝", Description: "保持现有 compass.long 不变，并继续创作"},
		},
	}
	approvalCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := t.longApproval.RequestLongApproval(approvalCtx, []Question{question})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return longApprovalTimeout, nil
		}
		if errors.Is(err, ErrInteractiveUnavailable) {
			return longApprovalUnavailable, nil
		}
		return longApprovalFailed, nil
	}
	if resp != nil && resp.Answers[question.Question] == "批准" {
		return longApprovalApproved, nil
	}
	return longApprovalRejected, nil
}

func describeLongCompassChange(base, proposed domain.LongCompass) string {
	var changes []string
	if strings.TrimSpace(base.EndingDirection) != strings.TrimSpace(proposed.EndingDirection) {
		changes = append(changes, fmt.Sprintf("- 终局方向：%s → %s",
			truncateRunes(base.EndingDirection, 60), truncateRunes(proposed.EndingDirection, 60)))
	}
	if strings.TrimSpace(base.EstimatedScale) != strings.TrimSpace(proposed.EstimatedScale) {
		changes = append(changes, fmt.Sprintf("- 预估规模：%s → %s",
			truncateRunes(base.EstimatedScale, 30), truncateRunes(proposed.EstimatedScale, 30)))
	}
	added, removed := diffLongThreads(base.OpenThreads, proposed.OpenThreads)
	if len(added) > 0 {
		changes = append(changes, fmt.Sprintf("- 新增长线（%d）：%s", len(added), summarizeThreads(added)))
	}
	if len(removed) > 0 {
		changes = append(changes, fmt.Sprintf("- 删除/改写长线（%d）：%s", len(removed), summarizeThreads(removed)))
	}
	if len(changes) == 0 {
		return "- 未检测到长期内容变化"
	}
	return strings.Join(changes, "\n")
}

func diffLongThreads(base, proposed []string) (added, removed []string) {
	baseCounts := make(map[string]int, len(base))
	proposedCounts := make(map[string]int, len(proposed))
	for _, thread := range base {
		baseCounts[strings.TrimSpace(thread)]++
	}
	for _, thread := range proposed {
		proposedCounts[strings.TrimSpace(thread)]++
	}
	for _, thread := range proposed {
		key := strings.TrimSpace(thread)
		if baseCounts[key] > 0 {
			baseCounts[key]--
		} else {
			added = append(added, thread)
		}
	}
	for _, thread := range base {
		key := strings.TrimSpace(thread)
		if proposedCounts[key] > 0 {
			proposedCounts[key]--
		} else {
			removed = append(removed, thread)
		}
	}
	return added, removed
}

func summarizeThreads(threads []string) string {
	limit := min(len(threads), 4)
	parts := make([]string, 0, limit)
	for _, thread := range threads[:limit] {
		parts = append(parts, "“"+truncateRunes(strings.TrimSpace(thread), 36)+"”")
	}
	text := strings.Join(parts, "、")
	if len(threads) > limit {
		text += fmt.Sprintf(" 等 %d 条", len(threads))
	}
	return text
}

func (t *SaveFoundationTool) recordLongCompassApproval(proposal longCompassProposal, outcome longApprovalOutcome) {
	facts, _ := json.Marshal(map[string]any{
		"proposal_id": proposal.ID,
		"base":        proposal.Base,
		"proposed":    proposal.Proposed,
	})
	decision, _ := json.Marshal(map[string]any{
		"proposal_id": proposal.ID,
		"outcome":     outcome,
	})
	decider := "system"
	if outcome == longApprovalApproved || outcome == longApprovalRejected {
		decider = "user"
	}
	if _, err := t.store.Decisions.Append(store.DecisionRecord{
		Kind: "compass_long_approval", Decider: decider,
		Facts: facts, Decision: decision, Reason: proposal.Reason,
	}); err != nil {
		slog.Warn("compass.long 审批审计落盘失败", "module", "tools", "proposal", proposal.ID, "outcome", outcome, "err", err)
	}
}

func foundationArtifact(t string) string {
	switch t {
	case "premise":
		return "premise.md"
	case "outline":
		return "outline.json"
	case "layered_outline", "expand_arc", "append_volume":
		return "layered_outline.json"
	case "complete_book":
		return "meta/progress.json"
	case "characters":
		return "characters.json"
	case "world_rules":
		return "world_rules.json"
	case "update_compass":
		return "meta/compass.json"
	default:
		return ""
	}
}

func decodeFoundationJSON(typeName, content string, out any) error {
	err := json.Unmarshal([]byte(content), out)
	if err == nil {
		return nil
	}
	hint := `常见原因：字符串值中的双引号未转义为 \", 换行未转义为 \n, 或对象字段间漏了逗号。请整段重新生成一次。`
	if se, ok := err.(*json.SyntaxError); ok {
		line, col := offsetToLineCol(content, int(se.Offset))
		return fmt.Errorf("parse %s JSON (line %d col %d): %w — %s", typeName, line, col, err, hint)
	}
	return fmt.Errorf("parse %s JSON: %w — %s", typeName, err, hint)
}

func offsetToLineCol(s string, offset int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(s) {
		offset = len(s)
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if s[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func normalizeFoundationContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("content is required: %w", errs.ErrToolArgs)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("invalid content: expected Markdown string or valid JSON value: %w", errs.ErrToolArgs)
	}
	return string(raw), nil
}

func makeVolumeEndFacts(p *domain.Progress) json.RawMessage {
	completed, total := 0, 0
	if p != nil {
		completed = len(p.CompletedChapters)
		total = p.TotalChapters
	}
	facts, _ := json.Marshal(map[string]any{
		"completed_chapters": completed,
		"total_chapters":     total,
	})
	return facts
}

func (t *SaveFoundationTool) recordVolumeEndDecision(action, reason string, facts json.RawMessage, result map[string]any) {
	decision := map[string]any{"action": action}
	if v, ok := result["volume"]; ok {
		decision["volume"] = v
	}
	if _, ok := result["final_volume"]; ok {
		decision["final"] = true
	}
	raw, _ := json.Marshal(decision)
	if _, err := t.store.Decisions.Append(store.DecisionRecord{
		Kind: "volume_end", Decider: "architect",
		Facts: facts, Decision: raw, Reason: reason,
	}); err != nil {
		slog.Warn("卷末裁定审计落盘失败", "module", "tools", "action", action, "err", err)
	}
}

func (t *SaveFoundationTool) consumeWriterFeedback() {
	if err := t.store.Outline.ClearOutlineFeedback(); err != nil {
		slog.Warn("清空 writer 反馈池失败", "module", "tools", "err", err)
	}
}

func (t *SaveFoundationTool) validateChapterScenes(chapters []domain.OutlineEntry) error {
	isV3 := t.contract != nil && t.contract.GetContract() == projectprofile.ContractSceneBeatV3
	for i, ch := range chapters {
		if isV3 && len(ch.Scenes) == 0 {
			return fmt.Errorf("chapters[%d].scenes: v3 requires non-empty scenes array", i)
		}
		for j, sc := range ch.Scenes {
			if err := t.contract.Validate(sc); err != nil {
				return fmt.Errorf("chapters[%d].scenes[%d].%w", i, j, err)
			}
		}
	}
	return nil
}

// validateChapterScenesAggregated 遍历所有章节的所有场景，返回全部语义错误；nil 表示全部通过。
// 仅用于 layered_outline/append_volume 的 Phase 1 聚合；expand_arc 仍使用 fail-fast 的 validateChapterScenes。
func (t *SaveFoundationTool) validateChapterScenesAggregated(chapters []domain.OutlineEntry) []error {
	isV3 := t.contract != nil && t.contract.GetContract() == projectprofile.ContractSceneBeatV3
	var errs []error
	for i, ch := range chapters {
		if isV3 && len(ch.Scenes) == 0 {
			errs = append(errs, fmt.Errorf("chapters[%d].scenes: v3 requires non-empty scenes array", i))
			continue
		}
		for j, sc := range ch.Scenes {
			if err := t.contract.Validate(sc); err != nil {
				errs = append(errs, fmt.Errorf("chapters[%d].scenes[%d].%w", i, j, err))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func countChapters(arcs []domain.ArcOutline) int {
	n := 0
	for _, a := range arcs {
		n += len(a.Chapters)
	}
	return n
}

// deepCopyVolumes 通过 JSON 往返深拷贝 VolumeOutline 切片。
func deepCopyVolumes(src []domain.VolumeOutline) []domain.VolumeOutline {
	data, _ := json.Marshal(src)
	var dst []domain.VolumeOutline
	json.Unmarshal(data, &dst)
	return dst
}
