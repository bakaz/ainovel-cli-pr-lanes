package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
)

// ── finish_polish 工具（schema docs/polisher-candidate-tools-schema.md §6） ──
//
// 结束 polish run：声明 status、covered issues 与 unresolved 项。调用成功后
// accumulator 状态 planned → finished；此后任何 plan/batch/finish 调用 →
// already_finished。finish_polish 是 runner 的停止工具（StopAfterTools，包 4 配置），
// 不直接保存正文（包 6 最终应用）。

// FinishPolishTool 是 finish_polish 工具。非 ReadOnly，非 ConcurrencySafe。
// accumulator 为可替换指针：工具在构建时注册（nil），运行时经 SetAccumulator
// 注入（包 4 holder / 包 6 polish_draft 每次 run 接线）；Execute 在未注入时
// 返回明确错误，不会静默空转。
type FinishPolishTool struct {
	mu  sync.Mutex
	acc *PolishAccumulator
}

// NewFinishPolishTool 构造 finish 工具（无参；accumulator 由 SetAccumulator 注入）。
func NewFinishPolishTool() *FinishPolishTool {
	return &FinishPolishTool{}
}

// SetAccumulator 注入/替换当前 run 的 accumulator（线程安全，运行时调用）。
func (t *FinishPolishTool) SetAccumulator(acc *PolishAccumulator) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.acc = acc
}

// currentAccumulator 返回当前注入的 accumulator（未注入时为 nil）。
func (t *FinishPolishTool) currentAccumulator() *PolishAccumulator {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acc
}

func (t *FinishPolishTool) Name() string { return "finish_polish" }
func (t *FinishPolishTool) Description() string {
	return "End the polish run: declare status, covered issues and unresolved items. After this call no further plan/batch calls are accepted."
}
func (t *FinishPolishTool) Label() string { return "结束精修" }

func (t *FinishPolishTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *FinishPolishTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *FinishPolishTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("operation_id", schema.String("Host-generated run id, echo verbatim")).Required(),
		schema.Property("baseline_digest", schema.String("sha256: digest of the baseline draft, echo verbatim")).Required(),
		schema.Property("status", schema.Enum("Finish status", "complete", "partial", "no_op", "escalate")).Required(),
		schema.Property("submitted_edit_count", schema.Int("Must equal the accepted total, 0-32")).Required(),
		schema.Property("covered_issue_ids", schema.Array("Issues covered by accepted edits or declared no_op", schema.String("p-XXX"))).Required(),
		schema.Property("unresolved", schema.Array("Unresolved items (<=32)", schema.Object(
			schema.Property("issue_id", schema.String("p-XXX, must exist in plan")).Required(),
			schema.Property("reason", schema.String("Reason (1-200 runes)")).Required(),
			schema.Property("recommended_owner", schema.Enum("Recommended owner", "writer")).Required(),
		))),
		schema.Property("summary", schema.String("Finish summary (1-1000 runes)")).Required(),
	)
}

// polishFinishRequest 是 finish_polish 的请求（严格模式，指针区分缺字段）。
type polishFinishRequest struct {
	OperationID        *string   `json:"operation_id"`
	BaselineDigest     *string   `json:"baseline_digest"`
	Status             *string   `json:"status"`
	SubmittedEditCount *int      `json:"submitted_edit_count"`
	CoveredIssueIDs    *[]string `json:"covered_issue_ids"`
	Unresolved         *[]struct {
		IssueID          *string `json:"issue_id"`
		Reason           *string `json:"reason"`
		RecommendedOwner *string `json:"recommended_owner"`
	} `json:"unresolved"`
	Summary *string `json:"summary"`
}

func (t *FinishPolishTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	acc := t.currentAccumulator()
	if acc == nil {
		return nil, fmt.Errorf("polish accumulator not initialized")
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()

	reject := func(code string) (json.RawMessage, error) {
		// run 级拒绝（整批 index=-1）：记录错误码供审计（schema §9 RunRejectionCode）。
		acc.recordRunRejectionLocked(code)
		return json.Marshal(polishPlanResult{Accepted: 0, Rejected: 1,
			Errors: []polishToolError{{Index: -1, Code: code}}})
	}

	var req polishFinishRequest
	if err := parsePolishToolArgs(args, &req); err != nil {
		return reject(PolishErrMalformedJSON)
	}

	// §3.1：operation_id / baseline_digest 一致。
	if req.OperationID == nil || *req.OperationID != acc.operationID {
		return reject(PolishErrOpMismatch)
	}
	if req.BaselineDigest == nil || *req.BaselineDigest != acc.baselineDigest {
		return reject(PolishErrBaselineMismatch)
	}

	// §3.2 状态机：planned → finished；未 plan → not_planned；已 finish → already_finished。
	switch acc.state {
	case polishAccEmpty:
		return reject(PolishErrNotPlanned)
	case polishAccFinished:
		return reject(PolishErrAlreadyFinished)
	}

	// §6.2 字段约束。
	if req.Status == nil {
		return reject(PolishErrFieldRequired)
	}
	if !containsString(polishFinishStatuses, *req.Status) {
		return reject(PolishErrStatusInvalid)
	}
	if req.SubmittedEditCount == nil {
		return reject(PolishErrFieldRequired)
	}
	if *req.SubmittedEditCount < 0 || *req.SubmittedEditCount > maxPolishEdits {
		return reject(PolishErrValueOutOfRange)
	}
	if req.CoveredIssueIDs == nil {
		return reject(PolishErrFieldRequired)
	}
	for _, id := range *req.CoveredIssueIDs {
		if !polishIssueIDRe.MatchString(id) {
			return reject(PolishErrIssueIDInvalid)
		}
		if acc.findPlanIssue(id) == nil {
			return reject(PolishErrIssueUnknown)
		}
	}
	if req.Unresolved != nil && len(*req.Unresolved) > maxPolishUnresolvedItems {
		return reject(PolishErrValueOutOfRange)
	}
	unresolved := make([]PolishUnresolved, 0)
	if req.Unresolved != nil {
		for _, u := range *req.Unresolved {
			if u.IssueID == nil {
				return reject(PolishErrFieldRequired)
			}
			if !polishIssueIDRe.MatchString(*u.IssueID) {
				return reject(PolishErrIssueIDInvalid)
			}
			if acc.findPlanIssue(*u.IssueID) == nil {
				return reject(PolishErrIssueUnknown)
			}
			if u.Reason == nil {
				return reject(PolishErrFieldRequired)
			}
			if n := utf8.RuneCountInString(*u.Reason); n < 1 || n > maxPolishUnresolvedRunes {
				return reject(PolishErrValueOutOfRange)
			}
			if u.RecommendedOwner == nil {
				return reject(PolishErrFieldRequired)
			}
			if *u.RecommendedOwner != "writer" {
				return reject(PolishErrOwnerInvalid)
			}
			unresolved = append(unresolved, PolishUnresolved{
				IssueID:          *u.IssueID,
				Reason:           *u.Reason,
				RecommendedOwner: *u.RecommendedOwner,
			})
		}
	}
	if req.Summary == nil {
		return reject(PolishErrFieldRequired)
	}
	if *req.Summary == "" {
		return reject(PolishErrSummaryRequired)
	}
	if n := utf8.RuneCountInString(*req.Summary); n > maxPolishSummaryRunes {
		return reject(PolishErrValueOutOfRange)
	}

	// §6.4 一致性规则（确定性，按 §6.4 顺序短路）。
	// - status=no_op ⇒ submitted_edit_count=0、covered 为空、unresolved 为空。
	if *req.Status == "no_op" &&
		(*req.SubmittedEditCount != 0 || len(*req.CoveredIssueIDs) != 0 || len(unresolved) != 0) {
		return reject(PolishErrStatusCountConflict)
	}
	// - status=complete ⇒ unresolved 不得含 plan 中 action=edit 的 issue。
	if *req.Status == "complete" {
		for _, u := range unresolved {
			if iss := acc.findPlanIssue(u.IssueID); iss != nil && iss.Action == "edit" {
				return reject(PolishErrUnresolvedEdited)
			}
		}
	}
	// - status=escalate ⇒ unresolved 必须非空。
	if *req.Status == "escalate" && len(unresolved) == 0 {
		return reject(PolishErrUnresolvedEmpty)
	}
	// - covered_issue_ids 每个 issue 必须满足可覆盖条件：(a) 已提交过 edit，或 (b) action=no_op。
	for _, id := range *req.CoveredIssueIDs {
		if !acc.issueHasAcceptedEdit(id) {
			if iss := acc.findPlanIssue(id); iss == nil || iss.Action != "no_op" {
				return reject(PolishErrCoverageNotEditable)
			}
		}
	}
	// - submitted_edit_count 必须等于已接受总数。
	if *req.SubmittedEditCount != len(acc.accepted) {
		return reject(PolishErrCountMismatch)
	}

	// 通过：存 finish，状态 → finished。
	acc.finish = &PolishFinishRecord{
		Status:             *req.Status,
		SubmittedEditCount: *req.SubmittedEditCount,
		CoveredIssueIDs:    *req.CoveredIssueIDs,
		Unresolved:         unresolved,
		Summary:            *req.Summary,
	}
	acc.state = polishAccFinished

	return json.Marshal(polishPlanResult{Accepted: 1, Rejected: 0, Errors: []polishToolError{}})
}

// issueHasAcceptedEdit 报告 issue_id 是否在某已接受 batch 中提交过 edit（调用方须持锁）。
func (a *PolishAccumulator) issueHasAcceptedEdit(issueID string) bool {
	for _, e := range a.accepted {
		if e.IssueID == issueID {
			return true
		}
	}
	return false
}
