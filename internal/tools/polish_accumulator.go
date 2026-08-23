package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ── Polisher 候选工具协议 accumulator（schema docs/polisher-candidate-tools-schema.md §3） ──
//
// 本文件实现三个候选提交工具（submit_polish_plan / submit_edit_batch / finish_polish）
// 共享的内存 accumulator：单次 polish run 的候选累积器（empty → planned → finished
// 状态机，§3.2）。纯内存态，无 store/IO 依赖，可独立单测。
//
// 注意：schema §3.3 的字段清单是审计映射所需的存储字段；baselineContent 是 batch
// 预检（§5.3 步骤 10 复用 locatePolishEdit）必需的内部工作字段，由 host 在构造时
// 注入（包 6 接线），不属于审计映射范围。

// polishAccState 是 accumulator 状态机状态（§3.2）。
type polishAccState int

const (
	polishAccEmpty polishAccState = iota
	polishAccPlanned
	polishAccFinished
)

// PolishAccumulator 是单次 polish run 的候选累积器（内存态）。
// 三个候选工具共享同一个 *PolishAccumulator（由外部注入）；polish_draft 非
// ConcurrencySafe，但 accumulator 内部仍加互斥锁（防御性，防未来误用）。
type PolishAccumulator struct {
	mu              sync.Mutex
	operationID     string
	baselineDigest  string
	baselineContent string // 输入快照全文（batch 预检定位用，非审计字段）
	chapter         int
	state           polishAccState

	plan         *PolishPlanRecord    // plan 校验通过后非 nil
	accepted     []PolishAcceptedEdit // 已通过预检的候选（含 byte range）
	rejected     []PolishRejectedEdit // 被预检拒绝的候选（审计计数用）
	usedIssueIDs map[string]bool      // 已接受 edit 用过的 issue_id（防跨批复用）
	nextBatch    int                  // 期望的下一个 batch_index（1 起）
	finish       *PolishFinishRecord  // finish 校验通过后非 nil
	// runRejectionCode 记录最近一次 run 级拒绝（整批/整 run，index=-1）的错误码
	//（schema §9 RunRejectionCode 审计来源；per-edit 拒绝不进此字段）。
	runRejectionCode string
}

// NewPolishAccumulator 构造 accumulator。operationID/baselineDigest 由 host 生成
// （polish_draft 启动 run 时），baselineContent 是本次 run 的输入快照全文。
func NewPolishAccumulator(operationID, baselineDigest, baselineContent string, chapter int) *PolishAccumulator {
	return &PolishAccumulator{
		operationID:     operationID,
		baselineDigest:  baselineDigest,
		baselineContent: baselineContent,
		chapter:         chapter,
		state:           polishAccEmpty,
		usedIssueIDs:    make(map[string]bool),
		nextBatch:       1,
	}
}

// ── 审计/诊断访问器（包 6 落盘映射用，§9） ─────────────────────────────

// OperationID 返回 run 的 operation_id。
func (a *PolishAccumulator) OperationID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.operationID
}

// BaselineDigest 返回 run 的 baseline_digest。
func (a *PolishAccumulator) BaselineDigest() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.baselineDigest
}

// Chapter 返回章节号。
func (a *PolishAccumulator) Chapter() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chapter
}

// StateName 返回状态机状态的稳定字符串（empty/planned/finished，诊断与审计用）。
func (a *PolishAccumulator) StateName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.state {
	case polishAccPlanned:
		return "planned"
	case polishAccFinished:
		return "finished"
	default:
		return "empty"
	}
}

// Plan 返回已通过的 plan 记录（未提交时为 nil）。
func (a *PolishAccumulator) Plan() *PolishPlanRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.plan
}

// Accepted 返回已接受候选（只读视图，审计用）。
func (a *PolishAccumulator) Accepted() []PolishAcceptedEdit {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accepted
}

// Rejected 返回被拒绝候选（只读视图，审计计数用）。
func (a *PolishAccumulator) Rejected() []PolishRejectedEdit {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rejected
}

// NextBatch 返回期望的下一个 batch_index。
func (a *PolishAccumulator) NextBatch() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nextBatch
}

// Finish 返回已通过的 finish 记录（未提交时为 nil）。
func (a *PolishAccumulator) Finish() *PolishFinishRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.finish
}

// RunRejectionCode 返回最近一次 run 级拒绝的错误码（无则空串；schema §9 审计用）。
func (a *PolishAccumulator) RunRejectionCode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runRejectionCode
}

// recordRunRejectionLocked 记录 run 级拒绝错误码（调用方须已持有 mu；审计用）。
// 由三个候选工具的整批/整 run 拒绝路径调用（index=-1 的拒绝）。
func (a *PolishAccumulator) recordRunRejectionLocked(code string) {
	a.runRejectionCode = code
}

// BatchCount 返回实际推进协议的批次数（nextBatch-1；全拒批次不推进，schema §9 审计用）。
func (a *PolishAccumulator) BatchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nextBatch - 1
}

// ── 存储类型（schema §3.3） ────────────────────────────────────────────

// PolishPlanIssue 是 plan 中的单条 issue（§4.1）。
type PolishPlanIssue struct {
	IssueID          string   `json:"issue_id"`
	SourceFindingIDs []string `json:"source_finding_ids,omitempty"`
	Origin           string   `json:"origin"`
	Category         string   `json:"category"`
	Priority         string   `json:"priority"`
	Anchor           string   `json:"anchor"`
	Problem          string   `json:"problem"`
	EditGoal         string   `json:"edit_goal"`
	FactRisk         string   `json:"fact_risk"`
	Action           string   `json:"action"`
}

// PolishPlanRecord 是已通过的 plan（§3.3）。
type PolishPlanRecord struct {
	OverallAssessment string            `json:"overall_assessment"`
	PlannedEditCount  int               `json:"planned_edit_count"`
	Issues            []PolishPlanIssue `json:"issues"` // 按提交顺序
	Digest            string            `json:"-"`      // sha256(规范化 plan JSON)，审计用
}

// PolishAcceptedEdit 是已通过预检的候选（§3.3）。
type PolishAcceptedEdit struct {
	IssueID   string
	OldString string
	NewString string
	Reason    string
	Category  string
	Start     int // baseline 中的 byte range（定位时填充）
	End       int
	Mode      PolishEditMatchMode // exact | normalized（复用现有枚举）
}

// PolishRejectedEdit 是被预检拒绝的候选（审计计数用，§3.3）。
type PolishRejectedEdit struct {
	IssueID string
	Code    string // 错误码（§8）
	Index   int    // 批内下标
}

// PolishUnresolved 是 finish 中的未解决项（§6.1）。
type PolishUnresolved struct {
	IssueID          string `json:"issue_id"`
	Reason           string `json:"reason"`
	RecommendedOwner string `json:"recommended_owner"`
}

// PolishFinishRecord 是已通过的 finish（§3.3）。
type PolishFinishRecord struct {
	Status             string             `json:"status"`
	SubmittedEditCount int                `json:"submitted_edit_count"`
	CoveredIssueIDs    []string           `json:"covered_issue_ids"`
	Unresolved         []PolishUnresolved `json:"unresolved,omitempty"`
	Summary            string             `json:"summary"`
}

// ── 错误码（schema §8，稳定枚举，string 常量） ─────────────────────────

const (
	PolishErrOpMismatch           = "op_mismatch"
	PolishErrBaselineMismatch     = "baseline_mismatch"
	PolishErrNotPlanned           = "not_planned"
	PolishErrPlanExists           = "plan_exists"
	PolishErrAlreadyFinished      = "already_finished"
	PolishErrBadBatchIndex        = "bad_batch_index"
	PolishErrBatchLimit           = "batch_limit"
	PolishErrTotalLimit           = "total_limit"
	PolishErrIssueUnknown         = "issue_unknown"
	PolishErrIssueNotEditable     = "issue_not_editable"
	PolishErrFactRiskEditConflict = "fact_risk_edit_conflict"
	PolishErrIssueReused          = "issue_reused"
	PolishErrCoverageNotEditable  = "coverage_not_editable"
	PolishErrStatusCountConflict  = "status_count_conflict"
	PolishErrUnresolvedEdited     = "unresolved_edited"
	PolishErrUnresolvedEmpty      = "unresolved_empty"
	PolishErrValueOutOfRange      = "value_out_of_range"
	PolishErrAnchorMissing        = "anchor_missing"
	PolishErrAnchorAmbiguous      = "anchor_ambiguous"
	PolishErrAnchorOverlap        = "anchor_overlap"
	PolishErrAnchorTooLong        = "anchor_too_long"
	PolishErrEmptyOld             = "empty_old"
	PolishErrEmptyNew             = "empty_new"
	PolishErrNewTooLong           = "new_too_long"
	PolishErrNoop                 = "noop"
	PolishErrFactChanged          = "fact_changed"
	PolishErrFactCheckInvalid     = "fact_check_invalid"
	PolishErrBadEnum              = "bad_enum"
	PolishErrFieldRequired        = "field_required"
	PolishErrMalformedJSON        = "malformed_json"
	PolishErrPlanLimit            = "plan_limit"
	PolishErrIssueIDInvalid       = "issue_id_invalid"
	PolishErrStatusInvalid        = "status_invalid"
	PolishErrCountMismatch        = "count_mismatch"
	PolishErrOwnerInvalid         = "owner_invalid"
	PolishErrSummaryRequired      = "summary_required"
)

// ── 工具返回结构（§4.4 / §5.4 / §6.5） ─────────────────────────────────

// polishToolError 是单条拒绝记录：index 为批内下标（整批拒绝时为 -1）。
type polishToolError struct {
	Index int    `json:"index"`
	Code  string `json:"code"`
}

// polishPlanResult 是 submit_polish_plan / finish_polish 的返回（无 accepted_total）。
type polishPlanResult struct {
	Accepted int               `json:"accepted"`
	Rejected int               `json:"rejected"`
	Errors   []polishToolError `json:"errors"`
}

// polishBatchResult 是 submit_edit_batch 的返回（含 accepted_total）。
type polishBatchResult struct {
	Accepted      int               `json:"accepted"`
	Rejected      int               `json:"rejected"`
	AcceptedTotal int               `json:"accepted_total"`
	Errors        []polishToolError `json:"errors"`
}

// ── 数字保持检查（§5.3 步骤 12，纯函数） ───────────────────────────────

// polishDigitRuns 提取 s 中所有"极大连续 ASCII 数字 run"按出现顺序组成的字符串序列。
// 例："第3章有5只猫，2024年" → ["3","5","2024"]。
func polishDigitRuns(s string) []string {
	var runs []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			runs = append(runs, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return runs
}

// polishDigitsPreserved 判定 D(old) == D(new)：序列长度、逐项 run 字符串、顺序
// 全部一致才返回 true（按 run 字符串逐项比较，非按 run 长度比较）。
func polishDigitsPreserved(oldS, newS string) bool {
	a, b := polishDigitRuns(oldS), polishDigitRuns(newS)
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

// ── JSON 解析容错（schema §7，三个工具共用） ───────────────────────────

// parsePolishToolArgs 严格解析工具参数：
//   - 容忍 BOM 前缀；
//   - 容忍单层 Markdown fence 包裹（```json ... ``` 剥掉后解析）；
//   - json.Decoder + DisallowUnknownFields（顶层与嵌套均拒绝未知字段）；
//   - JSON 对象之后必须只剩空白（尾随内容拒绝）。
//
// 任何失败 → 调用方整批拒绝，错误码 malformed_json（不触发模型重试）。
func parsePolishToolArgs(raw json.RawMessage, v any) error {
	text := string(raw)
	// 若参数本身是 JSON 字符串（模型把 JSON 包在引号里），先解包一层。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		text = s
	}
	text = strings.TrimPrefix(text, "\uFEFF") // BOM
	text = strings.TrimSpace(text)
	// 单层 fence 剥除（只剥一次，不循环）。
	if strings.HasPrefix(text, "```") {
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[idx+1:]
		} else {
			// 单行 fence：剥掉开头的 ```lang 与结尾的 ```。
			text = strings.TrimPrefix(text, "```")
			if i := strings.IndexAny(text, " \t"); i >= 0 {
				text = text[i+1:]
			}
		}
		text = strings.TrimSpace(text)
		if strings.HasSuffix(text, "```") {
			if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
				text = text[:idx]
			} else {
				text = strings.TrimSuffix(text, "```")
			}
		}
		text = strings.TrimSpace(text)
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("JSON 后存在尾随内容")
	}
	return nil
}
