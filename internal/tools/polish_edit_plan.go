package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// ── Polisher edit list 协议（ora-1 形态 2） ─────────────────────────────
// polisher 以结构化 edit 列表 JSON 替代整章重输出：polish_draft 在内存中基于
// 同一输入快照原子应用全部 edit → 一次 SaveDraft → 一个最终 polish checkpoint。
// 本文件只含纯函数（解析/校验/应用），无 store/IO 依赖，可独立单测。

const (
	// polishEditPlanVersion 是当前 edit 列表协议版本。
	polishEditPlanVersion = 1
	// maxPolishEdits 是单次 edit 列表的条数上限。
	maxPolishEdits = 32
	// maxPolishEditOldRunes 是单条 old_string 的长度上限（runes）。
	maxPolishEditOldRunes = 2000
	// maxPolishEditCoverageRatio 是所有 old range 总和占输入的比例上限（普通 draft 场景）。
	maxPolishEditCoverageRatio = 0.50
	// maxPolishEditCoverageRatioRewrite 是重写/打磨队列（stage=rewrite）场景的
	// 覆盖上限（P1-6）：重写场景允许更大改动面（70%），但超过 70% 仍拒绝——
	// 要求走显式整章 rewrite 路径，edit list 不隐式放开（见 polishCoverageLimitForChapter）。
	maxPolishEditCoverageRatioRewrite = 0.70
	// minPolishEditOutputRatio 是应用后候选占输入的比例下限（与整章模式 40% 一致）。
	minPolishEditOutputRatio = 0.40
)

// PolishEditItem 是一条精修 edit：old_string 必须在输入快照中精确且唯一出现。
type PolishEditItem struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// PolishEditPlan 是 polisher 输出的完整 edit 列表（严格 JSON 协议，禁止额外字段）。
// 注意：必须用 ParsePolishEditPlan 解析（严格模式），不要直接 json.Unmarshal。
type PolishEditPlan struct {
	Version int              `json:"version"`
	Edits   []PolishEditItem `json:"edits"`
}

// PolishEditErrKind 区分 edit plan 失败类别（fail-closed 错误分类用）。
type PolishEditErrKind int

const (
	// PolishEditErrContract 是 JSON 契约错误：输出是 edit plan 形状但契约不合法
	// （version 不受支持 / 缺少字段）。fail-closed，不回退整章模式。
	PolishEditErrContract PolishEditErrKind = iota
	// PolishEditErrContent 是内容校验错误：anchor 缺失/多次/重叠/超限/产物过短等。
	PolishEditErrContent
)

// PolishEditError 是 edit plan 校验失败的结构化错误。
type PolishEditError struct {
	Kind  PolishEditErrKind
	Index int // 出错 edit 下标；-1 表示整体
	Msg   string
	// 覆盖比例超限错误（Kind=Content）时填充：全部 old range 的 runes 总和、
	// 输入 runes 总数、当前场景覆盖上限比例。P0-4 内部纠错反馈与审计分类
	// （coverage_exceeded vs edit_plan_invalid）依赖这些字段。
	CoverageRunes int
	InputRunes    int
	CoverageLimit float64 // 0 表示非覆盖超限错误
}

func (e *PolishEditError) Error() string {
	if e.Index >= 0 {
		return fmt.Sprintf("edit[%d]: %s", e.Index, e.Msg)
	}
	return e.Msg
}

// ParsePolishEditPlan 严格解析 polisher 输出为 edit plan。
//
// 返回三元组：
//   - (plan, true, nil)：合法 edit plan → 调用方走 edit 路径（本任务主体）。
//   - (nil, true, err)：输出不像 edit plan（非 JSON/代码围栏/未知字段/尾随内容/
//     纯正文）→ 调用方回退现有整章模式（渐进切换，旧协议模型仍可用；整章模式
//     的纯 JSON/围栏检查会拒绝 JSON 形态的异常输出，行为与旧版一致）。
//   - (nil, false, err)：输出是 edit plan 形状但契约不合法（version 不受支持 /
//     缺少必填字段）→ fail-closed 契约错误，草稿原样、不写 checkpoint。
//
// 未知字段（含嵌套）按"解析失败"处理 → 回退整章模式（规格 §2.3）。
func ParsePolishEditPlan(output string) (*PolishEditPlan, bool, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, true, &PolishEditError{Kind: PolishEditErrContract, Index: -1, Msg: "空输出不是 edit plan"}
	}
	if strings.HasPrefix(trimmed, "```") {
		// 代码围栏整体包裹 → 非裸 edit plan（回退；整章模式的围栏检查会拒绝）。
		return nil, true, &PolishEditError{Kind: PolishEditErrContract, Index: -1, Msg: "输出被代码围栏包裹，不是裸 edit plan JSON"}
	}
	if !strings.HasPrefix(trimmed, "{") {
		// 不以 { 开头（纯正文/点评/列表等）→ 非 edit plan（回退整章模式）。
		return nil, true, &PolishEditError{Kind: PolishEditErrContract, Index: -1, Msg: "输出不以 { 开头，不是 edit plan JSON"}
	}

	// 严格解码：顶层与嵌套均拒绝未知字段（json.Decoder 递归生效）。
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var raw struct {
		Version *int `json:"version"`
		Edits   []struct {
			OldString *string `json:"old_string"`
			NewString *string `json:"new_string"`
		} `json:"edits"`
	}
	if err := dec.Decode(&raw); err != nil {
		// 解析失败（语法错误/未知字段/类型错误）→ 回退整章模式。
		return nil, true, &PolishEditError{Kind: PolishEditErrContract, Index: -1, Msg: fmt.Sprintf("JSON 解析失败: %v", err)}
	}
	// JSON 对象之后必须只剩空白：任何尾随内容（点评/第二个对象/散文本）都视为
	// 非干净 edit plan（回退整章模式）。
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, true, &PolishEditError{Kind: PolishEditErrContract, Index: -1, Msg: "edit plan JSON 后存在尾随内容"}
	}

	if raw.Version == nil {
		return nil, false, &PolishEditError{Kind: PolishEditErrContract, Index: -1, Msg: "缺少 version 字段"}
	}
	if *raw.Version != polishEditPlanVersion {
		return nil, false, &PolishEditError{Kind: PolishEditErrContract, Index: -1,
			Msg: fmt.Sprintf("version=%d 不受支持（当前协议 v%d）", *raw.Version, polishEditPlanVersion)}
	}

	plan := &PolishEditPlan{Version: *raw.Version}
	for i, e := range raw.Edits {
		if e.OldString == nil || e.NewString == nil {
			return nil, false, &PolishEditError{Kind: PolishEditErrContract, Index: i,
				Msg: "edit 缺少 old_string 或 new_string"}
		}
		plan.Edits = append(plan.Edits, PolishEditItem{OldString: *e.OldString, NewString: *e.NewString})
	}
	return plan, true, nil
}

// ApplyPolishEditPlan 校验并原子应用 edit plan 到输入快照，返回完整候选文本。
//
// maxCoverageRatio 是当前场景的覆盖比例上限（所有 old range runes 总和 ÷ 输入
// runes），由调用方按场景传入（P1-6：普通 draft 0.50、rewrite 0.70）。调用方
// 必须传入非零合法值（0 < maxCoverageRatio <= 1），否则按默认 0.50 兜底。
//
// 所有校验基于原始输入快照：任一失败返回 *PolishEditError（Kind=Content），
// 不产生任何部分结果（纯函数，调用方据此保证草稿原子性——第 N 条无效时
// 前 N-1 条也绝不落盘）。
//
// 应用算法：每条 old_string 必须在输入中精确且唯一出现；按 byte offset 倒序
// 替换（先替换靠后的 range，再替换靠前的 range），保证互不重叠的 edit 互不干扰。
func ApplyPolishEditPlan(input string, plan *PolishEditPlan, maxCoverageRatio float64) (string, error) {
	if maxCoverageRatio <= 0 || maxCoverageRatio > 1 {
		maxCoverageRatio = maxPolishEditCoverageRatio
	}
	// 1. 条数上限。
	if len(plan.Edits) > maxPolishEdits {
		return "", &PolishEditError{Kind: PolishEditErrContent, Index: -1,
			Msg: fmt.Sprintf("edit 条数 %d 超过上限 %d", len(plan.Edits), maxPolishEdits)}
	}
	inputRunes := utf8.RuneCountInString(input)

	// 2. 逐条校验 + 收集 byte range（基于同一输入快照）。
	type editRange struct {
		start, end int
		idx        int // 原 plan.Edits 下标（错误信息定位用）
		item       PolishEditItem
	}
	ranges := make([]editRange, 0, len(plan.Edits))
	coverageRunes := 0
	for i, e := range plan.Edits {
		if e.OldString == "" {
			return "", &PolishEditError{Kind: PolishEditErrContent, Index: i, Msg: "old_string 为空"}
		}
		if e.OldString == e.NewString {
			return "", &PolishEditError{Kind: PolishEditErrContent, Index: i, Msg: "old_string 与 new_string 相同（无意义 edit）"}
		}
		oldRunes := utf8.RuneCountInString(e.OldString)
		if oldRunes > maxPolishEditOldRunes {
			return "", &PolishEditError{Kind: PolishEditErrContent, Index: i,
				Msg: fmt.Sprintf("old_string %d runes 超过上限 %d", oldRunes, maxPolishEditOldRunes)}
		}
		// 精确且唯一：strings.Count 是逐字子串匹配（含标点/空白），
		// 与"从原文精确复制"的协议语义一致。
		if n := strings.Count(input, e.OldString); n != 1 {
			if n == 0 {
				return "", &PolishEditError{Kind: PolishEditErrContent, Index: i,
					Msg: "old_string 在草稿中不存在（必须从输入草稿原文精确复制，含标点与空白）"}
			}
			return "", &PolishEditError{Kind: PolishEditErrContent, Index: i,
				Msg: fmt.Sprintf("old_string 在草稿中出现 %d 次（必须唯一）", n)}
		}
		start := strings.Index(input, e.OldString)
		ranges = append(ranges, editRange{start: start, end: start + len(e.OldString), idx: i, item: e})
		coverageRunes += oldRunes
	}

	// 3. 重叠检查：按 start 排序后，后一个 range 的 start 必须 ≥ 前一个的 end。
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	for i := 1; i < len(ranges); i++ {
		if ranges[i].start < ranges[i-1].end {
			return "", &PolishEditError{Kind: PolishEditErrContent, Index: -1,
				Msg: fmt.Sprintf("edit %d 与 edit %d 的 old_string 范围重叠", ranges[i-1].idx, ranges[i].idx)}
		}
	}

	// 4. 覆盖比例上限：所有 old ranges 总和 ≤ 输入 × 当前场景上限（P1-6）。
	if inputRunes > 0 && coverageRunes > int(float64(inputRunes)*maxCoverageRatio) {
		return "", &PolishEditError{Kind: PolishEditErrContent, Index: -1,
			CoverageRunes: coverageRunes, InputRunes: inputRunes, CoverageLimit: maxCoverageRatio,
			Msg: fmt.Sprintf("所有 old_string 覆盖 %d/%d runes（%.0f%%），超过上限 %.0f%%",
				coverageRunes, inputRunes, 100*float64(coverageRunes)/float64(inputRunes), maxCoverageRatio*100)}
	}

	// 5. 按 start 升序拼接（ranges 已在重叠检查时排序，且互不重叠）：
	//    input[:start0] + new0 + input[end0:start1] + new1 + ... + input[endN:]。
	//    全部 anchor 基于同一输入快照定位，替换互不干扰。
	var sb strings.Builder
	sb.Grow(len(input))
	cursor := 0
	for _, r := range ranges {
		sb.WriteString(input[cursor:r.start])
		sb.WriteString(r.item.NewString)
		cursor = r.end
	}
	sb.WriteString(input[cursor:])
	candidate := sb.String()

	// 6. 候选文本校验：UTF-8（防御）/ 长度下限 40% / 上限 maxPolishOutputRunes
	//    （与整章模式同一上限，防 edit 拼接放大）。
	if !utf8.ValidString(candidate) {
		return "", &PolishEditError{Kind: PolishEditErrContent, Index: -1, Msg: "应用后文本含非法 UTF-8 序列"}
	}
	candRunes := utf8.RuneCountInString(candidate)
	if candRunes < int(float64(inputRunes)*minPolishEditOutputRatio) {
		return "", &PolishEditError{Kind: PolishEditErrContent, Index: -1,
			Msg: fmt.Sprintf("应用后 %d runes 不足输入草稿（%d runes）的 40%%", candRunes, inputRunes)}
	}
	if candRunes > maxPolishOutputRunes {
		return "", &PolishEditError{Kind: PolishEditErrContent, Index: -1,
			Msg: fmt.Sprintf("应用后 %d runes 超过上限 %d", candRunes, maxPolishOutputRunes)}
	}
	return candidate, nil
}
