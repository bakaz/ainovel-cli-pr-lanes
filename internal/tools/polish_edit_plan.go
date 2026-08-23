package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ── Polisher edit list 协议（ora-1 形态 2） ─────────────────────────────
// polisher 以结构化 edit 列表 JSON 替代整章重输出：polish_draft 在内存中基于
// 同一输入快照逐条验证 + 按优先级部分接受（ora-1 ④）→ 一次 SaveDraft →
// 一个最终 polish checkpoint。本文件只含纯函数（解析/校验/匹配/应用），
// 无 store/IO 依赖，可独立单测。

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

// ── 匹配模式与丢弃原因（审计） ─────────────────────────────────────────

// PolishEditMatchMode 是单条 edit 的定位匹配模式（审计用）。
type PolishEditMatchMode string

const (
	// PolishEditMatchExact 表示 old_string 在输入中逐字精确且唯一出现。
	PolishEditMatchExact PolishEditMatchMode = "exact"
	// PolishEditMatchNormalized 表示精确缺失后经白名单归一化唯一出现
	// （只允许确定性表示等价，禁近似匹配）。
	PolishEditMatchNormalized PolishEditMatchMode = "normalized"
)

// PolishEditDropReason 是单条 edit 被丢弃的原因分类（审计用）。
type PolishEditDropReason string

const (
	PolishEditDropAnchorMissing   PolishEditDropReason = "anchor_missing"
	PolishEditDropAnchorAmbiguous PolishEditDropReason = "anchor_ambiguous"
	PolishEditDropAnchorOverlap   PolishEditDropReason = "anchor_overlap"
	PolishEditDropOverlapLower    PolishEditDropReason = "overlap_lower_priority"
	PolishEditDropCoverageLimit   PolishEditDropReason = "coverage_limit"
	PolishEditDropOutputTooShort  PolishEditDropReason = "output_too_short"
	PolishEditDropOutputTooLong   PolishEditDropReason = "output_too_long"
	PolishEditDropCountLimit      PolishEditDropReason = "count_limit"
	PolishEditDropNoop            PolishEditDropReason = "noop"
	PolishEditDropEmptyOldString  PolishEditDropReason = "empty_old_string"
	PolishEditDropOldTooLong      PolishEditDropReason = "old_too_long"
	PolishEditDropMechanical      PolishEditDropReason = "mechanical"
	// ── 候选工具协议 per-edit 码（schema §9，包 2 扩展；只加常量，不改现有行为）──
	PolishEditDropFactChanged      PolishEditDropReason = "fact_changed"
	PolishEditDropFactCheckInvalid PolishEditDropReason = "fact_check_invalid"
	PolishEditDropIssueUnknown     PolishEditDropReason = "issue_unknown"
	PolishEditDropIssueNotEditable PolishEditDropReason = "issue_not_editable"
	PolishEditDropIssueReused      PolishEditDropReason = "issue_reused"
	PolishEditDropEmptyNewString   PolishEditDropReason = "empty_new"
	PolishEditDropNewTooLong       PolishEditDropReason = "new_too_long"
	PolishEditDropTotalLimit       PolishEditDropReason = "total_limit"
	PolishEditDropBatchLimit       PolishEditDropReason = "batch_limit"
)

// minPolishEditNormalizedAnchorRunes 是 normalized 回退定位的锚点最短非空白 rune
// 数（防仅凭引号/短词等弱锚点定位）。
const minPolishEditNormalizedAnchorRunes = 8

// PolishEditApplication 记录一条 edit 的验证/定位/应用结果（审计用）。
type PolishEditApplication struct {
	Idx        int // 原 plan.Edits 下标
	OldString  string
	NewString  string
	Start      int // 原始输入 byte range（已定位时有效）
	End        int
	Mode       PolishEditMatchMode  // 定位匹配模式
	DropReason PolishEditDropReason // 非空 = 被丢弃（不在 Applied 中）
}

// PolishEditPlanResult 是 ApplyPolishEditPlanDetailed 的完整结果（候选 + 审计）。
// 审计字段只含计数与原因分类，绝不含正文/old_string/new_string 内容。
type PolishEditPlanResult struct {
	Candidate            string
	Applied              []PolishEditApplication // 实际应用（优先级序）
	Dropped              []PolishEditApplication // 被丢弃（含原因）
	ProposedEditCount    int
	NormalizedMatchCount int  // 应用中 normalized 定位条数
	Partial              bool // 部分接受：≥1 应用且 ≥1 丢弃
	CoverageRunes        int  // 应用的 old range runes 总和
	InputRunes           int
}

// DroppedEditCount 返回被丢弃（未应用）的 edit 条数（审计用）。
func (r *PolishEditPlanResult) DroppedEditCount() int { return len(r.Dropped) }

// DropReasons 返回被丢弃 edit 的原因分类（按原 plan 下标序，审计用）。
func (r *PolishEditPlanResult) DropReasons() []string {
	order := make([]PolishEditApplication, len(r.Dropped))
	copy(order, r.Dropped)
	sort.Slice(order, func(i, j int) bool { return order[i].Idx < order[j].Idx })
	out := make([]string, 0, len(order))
	for _, d := range order {
		out = append(out, string(d.DropReason))
	}
	return out
}

// AppliedMatchModes 返回实际应用 edit 的匹配模式（按应用序，审计用）。
func (r *PolishEditPlanResult) AppliedMatchModes() []string {
	out := make([]string, 0, len(r.Applied))
	for _, a := range r.Applied {
		out = append(out, string(a.Mode))
	}
	return out
}

// DropApplied 将下标在 dropIdx 集合中的已应用 edit 移入 Dropped（reason=mechanical），
// 基于同一输入快照重建候选并更新审计字段（机械回归剔除责任 edit 用，④ 第 6 条）。
func (r *PolishEditPlanResult) DropApplied(input string, dropIdx map[int]bool) {
	var kept []PolishEditApplication
	coverage := 0
	for _, a := range r.Applied {
		if dropIdx[a.Idx] {
			a.DropReason = PolishEditDropMechanical
			r.Dropped = append(r.Dropped, a)
			if a.Mode == PolishEditMatchNormalized {
				r.NormalizedMatchCount--
			}
			continue
		}
		kept = append(kept, a)
		coverage += utf8.RuneCountInString(a.OldString)
	}
	r.Applied = kept
	r.Candidate = buildPolishCandidate(input, r.Applied)
	r.CoverageRunes = coverage
	r.Partial = len(r.Applied) > 0 && len(r.Dropped) > 0
}

// ApplySinglePolishEdit 基于输入快照单独应用一条已定位 edit（机械责任剔除测试用，
// 纯函数；Start/End 必须指向同一输入快照）。
func ApplySinglePolishEdit(input string, app PolishEditApplication) string {
	return input[:app.Start] + app.NewString + input[app.End:]
}

// ── 归一化匹配白名单（exact + normalized 两级，禁近似匹配） ─────────────
//
// 只允许确定性表示等价（与 agentcore fuzzyFind 同源但收紧）：
//  1. CRLF/CR → LF
//  2. Unicode 水平空格（U+00A0/U+2007/U+202F/U+2009）→ 普通空格
//  3. 行尾空白差异（逐行 TrimRight）
//  4. 智能单双引号 → ASCII 引号
//  5. dash 变体（U+2013/U+2014/U+2015/U+2212）→ '-'
//  6. 全角 ASCII 标点（，。：；！？（）／［］～）→ 半角对应标点
//
// 禁止：Levenshtein/编辑距离/相似度候选自动应用、首尾行锚定、删除全部空白定位、
// 全局 NFKC、全角数字字母归一化、大小写折叠。new_string 不做任何归一化。

// normalizePolishRune 是白名单 rune 归一化。全角数字/字母（U+FF10-FF19/
// U+FF21-FF3A/U+FF41-FF5A）不在白名单内——与半角数字/字母是不同 token，不得视为等价。
func normalizePolishRune(r rune) rune {
	switch r {
	case '\u2018', '\u2019', '\u201A', '\u201B':
		return '\''
	case '\u201C', '\u201D', '\u201E', '\u201F':
		return '"'
	case '\u2013', '\u2014', '\u2015', '\u2212':
		return '-'
	case '\u00A0', '\u2007', '\u202F', '\u2009':
		return ' '
	case '\uFF0C': // ，
		return ','
	case '\u3002': // 。
		return '.'
	case '\uFF1A': // ：
		return ':'
	case '\uFF1B': // ；
		return ';'
	case '\uFF01': // ！
		return '!'
	case '\uFF1F': // ？
		return '?'
	case '\uFF08': // （
		return '('
	case '\uFF09': // ）
		return ')'
	case '\uFF0F': // ／
		return '/'
	case '\uFF3B': // ［
		return '['
	case '\uFF3D': // ］
		return ']'
	case '\uFF5E': // ～
		return '~'
	}
	return r
}

// normalizePolishAnchor 按白名单归一化文本：CRLF/CR→LF、逐行去除行尾空白、
// normalizePolishRune 逐 rune 映射。new_string 不做任何归一化（原样应用）。
func normalizePolishAnchor(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		ln = strings.TrimRightFunc(ln, unicode.IsSpace)
		lines[i] = strings.Map(normalizePolishRune, ln)
	}
	return strings.Join(lines, "\n")
}

// polishNormMap 保存归一化文本与原始字节偏移映射（normalized 定位回映用）。
type polishNormMap struct {
	text       string
	byteMap    []int // byteMap[i] = 归一化文本第 i 个 rune 在原始输入的起始字节偏移；末位哨兵 = len(input)
	byteToRune []int // byteToRune[b] = 归一化文本字节 b 所在 rune 的下标
}

type polishNormUnit struct {
	r rune
	b int // 该 rune 在原始输入中的起始字节偏移
}

// normalizePolishAnchorMapped 同 normalizePolishAnchor，另产出 rune→原始字节偏移映射
// （逐 rune 映射是单调的：归一化后连续范围唯一对应原始连续字节范围）。
func normalizePolishAnchorMapped(s string) polishNormMap {
	var units []polishNormUnit
	for b := 0; b < len(s); {
		r, size := utf8.DecodeRuneInString(s[b:])
		if r == '\r' {
			// CRLF/CR → LF；CRLF 时 CR 与其后的 LF 合并为一个 '\n'。
			if b+size < len(s) {
				if nxt, _ := utf8.DecodeRuneInString(s[b+size:]); nxt == '\n' {
					b += size
				}
			}
			units = append(units, polishNormUnit{r: '\n', b: b})
			b += size
			continue
		}
		units = append(units, polishNormUnit{r: normalizePolishRune(r), b: b})
		b += size
	}
	// 逐行去除行尾空白：'\n' 是行终止符（保留），每行内末尾空白 rune 丢弃。
	kept := make([]bool, len(units))
	for i := range kept {
		kept[i] = true
	}
	lineStart := 0
	for i, u := range units {
		if u.r == '\n' {
			trimPolishLineEnd(units, kept, lineStart, i)
			lineStart = i + 1
		}
	}
	trimPolishLineEnd(units, kept, lineStart, len(units))

	var sb strings.Builder
	byteMap := make([]int, 0, len(units)+1)
	for i, u := range units {
		if !kept[i] {
			continue
		}
		sb.WriteRune(u.r)
		byteMap = append(byteMap, u.b)
	}
	byteMap = append(byteMap, len(s))

	text := sb.String()
	byteToRune := make([]int, len(text)+1)
	ri := 0
	for b := 0; b < len(text); {
		_, size := utf8.DecodeRuneInString(text[b:])
		for i := 0; i < size; i++ {
			byteToRune[b+i] = ri
		}
		byteToRune[b+size] = ri + 1
		b += size
		ri++
	}
	return polishNormMap{text: text, byteMap: byteMap, byteToRune: byteToRune}
}

// trimPolishLineEnd 将 [start, end) 行内末尾的空白 rune 标记为丢弃。
func trimPolishLineEnd(units []polishNormUnit, kept []bool, start, end int) {
	last := end
	for last > start && unicode.IsSpace(units[last-1].r) {
		last--
	}
	for i := last; i < end; i++ {
		kept[i] = false
	}
}

// nonSpaceRunes 统计字符串中的非空白 rune 数（normalized 回退的锚点强度门槛）。
func nonSpaceRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// locatePolishEdit 在输入快照上定位单条 edit（exact + normalized 两级，禁近似匹配）：
//  1. exact 出现 1 次 → 直接用（现有语义）。
//  2. exact 出现 0 次 → normalized unique：归一化后出现 1 次才允许定位；
//     0 次 → anchor_missing，多次 → anchor_ambiguous，均丢弃该 edit。
//  3. exact 出现多次 → 立即判歧义丢弃，不尝试 normalized 猜测（归一化次数 ≥
//     精确次数，猜测不可能挽救；exact 多次 ⇒ normalized 必多次）。
//  4. normalized 回退要求 old_string 至少含 minPolishEditNormalizedAnchorRunes 个
//     非空白 rune（防仅凭引号/短词定位）。
func locatePolishEdit(input string, normInput *polishNormMap, e PolishEditItem, idx int) (app PolishEditApplication, reason PolishEditDropReason, ok bool) {
	app = PolishEditApplication{Idx: idx, OldString: e.OldString, NewString: e.NewString}
	if n := strings.Count(input, e.OldString); n == 1 {
		start := strings.Index(input, e.OldString)
		app.Start, app.End, app.Mode = start, start+len(e.OldString), PolishEditMatchExact
		return app, "", true
	} else if n > 1 {
		app.Mode = PolishEditMatchExact
		return app, PolishEditDropAnchorAmbiguous, false // 精确多次 → 不做 normalized 猜测
	}
	// 精确 0 次：normalized 回退（锚点强度门槛）。
	app.Mode = PolishEditMatchNormalized
	if nonSpaceRunes(e.OldString) < minPolishEditNormalizedAnchorRunes {
		return app, PolishEditDropAnchorMissing, false // 弱锚点不做 normalized
	}
	normOld := normalizePolishAnchor(e.OldString)
	if normOld == "" {
		return app, PolishEditDropAnchorMissing, false
	}
	if n := strings.Count(normInput.text, normOld); n != 1 {
		if n == 0 {
			return app, PolishEditDropAnchorMissing, false
		}
		return app, PolishEditDropAnchorAmbiguous, false
	}
	ns := strings.Index(normInput.text, normOld)
	ne := ns + len(normOld)
	app.Start = normInput.byteMap[normInput.byteToRune[ns]]
	app.End = normInput.byteMap[normInput.byteToRune[ne]]
	return app, "", true
}

// buildPolishCandidate 按 byte offset 升序应用 selected 到输入快照（互不重叠）。
// 全部 anchor 基于同一输入快照定位，替换互不干扰。
func buildPolishCandidate(input string, sel []PolishEditApplication) string {
	order := make([]PolishEditApplication, len(sel))
	copy(order, sel)
	sort.Slice(order, func(i, j int) bool { return order[i].Start < order[j].Start })
	var sb strings.Builder
	sb.Grow(len(input))
	cursor := 0
	for _, a := range order {
		sb.WriteString(input[cursor:a.Start])
		sb.WriteString(a.NewString)
		cursor = a.End
	}
	sb.WriteString(input[cursor:])
	return sb.String()
}

// overlapsAny 判断 a 与已选范围是否重叠。
func overlapsAny(a PolishEditApplication, sel []PolishEditApplication) bool {
	for _, s := range sel {
		if a.Start < s.End && s.Start < a.End {
			return true
		}
	}
	return false
}

// ApplyPolishEditPlanDetailed 是 edit plan 的验证 + 按优先级部分接受（ora-1 ④）。
//
// 流程：
//  1. 逐条局部验证 + 定位（基于同一输入快照），失败只丢弃该条（anchor 缺失/不唯一/
//     重叠/超长/old==new/归一化后 no-op/空 old_string）。
//  2. 按优先级（edits 数组顺序，高→低）贪心选择：
//     - 加入后覆盖超限 → 丢弃该条，继续尝试后续较短 edit；
//     - 加入后低于 40% 保底 → 丢弃导致越线的删除性 edit；
//     - 加入后超输出上限 → 丢弃膨胀 edit；
//     - 应用条数达 maxPolishEdits → 丢弃后续（count_limit）。
//
// 返回 err != nil 仅当原始 edits 非空但没有任何一条可安全应用（全无效）——调用方
// 据此写 degraded checkpoint，不再触发第二次模型纠错（④ 取代 P0-4 模型纠错）。
// 原始 edits=[] → 合法 no-op（err=nil、Candidate=input、非 degraded 语义）。
// 需要审计字段（proposed/dropped/partial/normalized 计数与匹配模式）时请用本函数；
// ApplyPolishEditPlan 是其精简包装。
func ApplyPolishEditPlanDetailed(input string, plan *PolishEditPlan, maxCoverageRatio float64) (*PolishEditPlanResult, error) {
	if maxCoverageRatio <= 0 || maxCoverageRatio > 1 {
		maxCoverageRatio = maxPolishEditCoverageRatio
	}
	res := &PolishEditPlanResult{ProposedEditCount: len(plan.Edits), InputRunes: utf8.RuneCountInString(input)}
	inputRunes := res.InputRunes
	normInput := normalizePolishAnchorMapped(input)

	// 1. 逐条局部验证 + 定位：失败只丢弃该条（同一输入快照，前后合法 edit 不受影响）。
	type locatedEdit struct {
		app      PolishEditApplication
		oldRunes int
	}
	var located []locatedEdit
	locatedCoverageRunes := 0
	for i, e := range plan.Edits {
		app := PolishEditApplication{Idx: i, OldString: e.OldString, NewString: e.NewString}
		if e.OldString == "" {
			app.DropReason = PolishEditDropEmptyOldString
			res.Dropped = append(res.Dropped, app)
			continue
		}
		if e.OldString == e.NewString {
			app.DropReason = PolishEditDropNoop
			res.Dropped = append(res.Dropped, app)
			continue
		}
		oldRunes := utf8.RuneCountInString(e.OldString)
		if oldRunes > maxPolishEditOldRunes {
			app.DropReason = PolishEditDropOldTooLong
			res.Dropped = append(res.Dropped, app)
			continue
		}
		if normalizePolishAnchor(e.OldString) == normalizePolishAnchor(e.NewString) {
			app.DropReason = PolishEditDropNoop // 归一化后 no-op（如仅行尾/引号差异）
			res.Dropped = append(res.Dropped, app)
			continue
		}
		loc, reason, ok := locatePolishEdit(input, &normInput, e, i)
		if !ok {
			loc.DropReason = reason
			res.Dropped = append(res.Dropped, loc)
			continue
		}
		located = append(located, locatedEdit{app: loc, oldRunes: oldRunes})
		locatedCoverageRunes += oldRunes
	}

	// 2. 按优先级（数组顺序）贪心选择（只选不重叠、覆盖与输出均在限内的安全 edit）。
	coverageRunes := 0
	for _, c := range located {
		if len(res.Applied) >= maxPolishEdits {
			c.app.DropReason = PolishEditDropCountLimit
			res.Dropped = append(res.Dropped, c.app)
			continue
		}
		if overlapsAny(c.app, res.Applied) {
			c.app.DropReason = PolishEditDropOverlapLower
			res.Dropped = append(res.Dropped, c.app)
			continue
		}
		if inputRunes > 0 && coverageRunes+c.oldRunes > int(float64(inputRunes)*maxCoverageRatio) {
			c.app.DropReason = PolishEditDropCoverageLimit
			res.Dropped = append(res.Dropped, c.app)
			continue
		}
		// 模拟应用（含本条）：40% 保底 / 输出上限。
		cand := buildPolishCandidate(input, append(res.Applied, c.app))
		if !utf8.ValidString(cand) {
			// 不可达（JSON 解析已保证合法 UTF-8）；防御性硬错误。
			return nil, &PolishEditError{Kind: PolishEditErrContent, Index: -1, Msg: "应用后文本含非法 UTF-8 序列"}
		}
		candRunes := utf8.RuneCountInString(cand)
		if candRunes < int(float64(inputRunes)*minPolishEditOutputRatio) {
			c.app.DropReason = PolishEditDropOutputTooShort
			res.Dropped = append(res.Dropped, c.app)
			continue
		}
		if candRunes > maxPolishOutputRunes {
			c.app.DropReason = PolishEditDropOutputTooLong
			res.Dropped = append(res.Dropped, c.app)
			continue
		}
		if c.app.Mode == PolishEditMatchNormalized {
			res.NormalizedMatchCount++
		}
		res.Applied = append(res.Applied, c.app)
		coverageRunes += c.oldRunes
	}

	res.Candidate = buildPolishCandidate(input, res.Applied)
	res.CoverageRunes = coverageRunes
	res.Partial = len(res.Applied) > 0 && len(res.Dropped) > 0

	// 3. 全无效（原始非空且 0 应用）→ 聚合内容错误（调用方据此写 degraded checkpoint）。
	if len(plan.Edits) > 0 && len(res.Applied) == 0 {
		return res, polishEditPlanRejectedError(res, locatedCoverageRunes, inputRunes, maxCoverageRatio)
	}
	return res, nil
}

// polishEditPlanRejectedError 构造"全无效"聚合错误：附首条丢弃详情；drop reasons 含
// coverage_limit 时填充覆盖结构化数字（coverage_exceeded 分类与审计依赖）。
func polishEditPlanRejectedError(res *PolishEditPlanResult, locatedCoverage, inputRunes int, maxCoverageRatio float64) *PolishEditError {
	detail := "无"
	if len(res.Dropped) > 0 {
		d := res.Dropped[0]
		detail = fmt.Sprintf("edit[%d] %s：%s", d.Idx, d.DropReason, polishEditDropDetail(d, inputRunes, maxCoverageRatio, locatedCoverage))
	}
	pe := &PolishEditError{Kind: PolishEditErrContent, Index: -1,
		Msg: fmt.Sprintf("edit plan 全部 %d 条 edit 均被丢弃，无安全 edit 可应用；首条丢弃原因：%s",
			len(res.Dropped), detail)}
	for _, d := range res.Dropped {
		if d.DropReason == PolishEditDropCoverageLimit {
			pe.CoverageRunes, pe.InputRunes, pe.CoverageLimit = locatedCoverage, inputRunes, maxCoverageRatio
			break
		}
	}
	return pe
}

// polishEditDropDetail 构造单条丢弃原因的人类可读详情（聚合错误信息用）。
func polishEditDropDetail(d PolishEditApplication, inputRunes int, maxCoverageRatio float64, locatedCoverage int) string {
	switch d.DropReason {
	case PolishEditDropEmptyOldString:
		return "old_string 为空"
	case PolishEditDropNoop:
		return "old_string 与 new_string 相同或归一化后相同（无意义 edit）"
	case PolishEditDropOldTooLong:
		return fmt.Sprintf("old_string %d runes 超过上限 %d", utf8.RuneCountInString(d.OldString), maxPolishEditOldRunes)
	case PolishEditDropAnchorMissing:
		return "old_string 在草稿中不存在（必须从输入草稿原文精确复制，含标点与空白）"
	case PolishEditDropAnchorAmbiguous:
		return "old_string 在草稿中出现多次（必须唯一）"
	case PolishEditDropOverlapLower:
		return "与更高优先级 edit 的 old_string 范围重叠"
	case PolishEditDropCoverageLimit:
		return fmt.Sprintf("所有 old_string 覆盖 %d/%d runes（%.0f%%），超过上限 %.0f%%",
			locatedCoverage, inputRunes, 100*float64(locatedCoverage)/float64(inputRunes), maxCoverageRatio*100)
	case PolishEditDropOutputTooShort:
		return fmt.Sprintf("应用后不足输入草稿（%d runes）的 40%%", inputRunes)
	case PolishEditDropOutputTooLong:
		return fmt.Sprintf("应用后超过上限 %d", maxPolishOutputRunes)
	case PolishEditDropCountLimit:
		return fmt.Sprintf("edit 条数超过上限 %d", maxPolishEdits)
	case PolishEditDropMechanical:
		return "引入 error 级机械违规"
	}
	return string(d.DropReason)
}

// ApplyPolishEditPlan 校验并应用 edit plan 到输入快照，返回完整候选文本。
//
// 语义（ora-1 ④）：逐条局部验证 + 按优先级部分接受——单条无效只丢弃该条（anchor
// 缺失/不唯一/重叠/超长/old==new/归一化后 no-op 等），其余合法 edit 仍应用；
// 仅当原始 edits 非空但全部被丢弃时返回 *PolishEditError（Kind=Content，无部分
// 结果）。匹配为 exact + normalized 两级（白名单等价，禁近似匹配，见
// ApplyPolishEditPlanDetailed）。需要审计字段时请用 ApplyPolishEditPlanDetailed。
func ApplyPolishEditPlan(input string, plan *PolishEditPlan, maxCoverageRatio float64) (string, error) {
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxCoverageRatio)
	if err != nil {
		return "", err
	}
	return res.Candidate, nil
}
