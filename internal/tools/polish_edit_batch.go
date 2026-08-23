package tools

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
)

// ── submit_edit_batch 工具（schema docs/polisher-candidate-tools-schema.md §5） ──
//
// 提交一批候选 edit（每批 ≤8，累计 ≤32）。全部 old_string 必须原样来自 baseline。
// 确定性预检（§5.3 14 步，按序短路）：批级检查（1–5）整批拒绝（index=-1），
// 逐条检查（6–14）只拒绝该条（index=批内下标）。

// SubmitEditBatchTool 是 submit_edit_batch 工具。非 ReadOnly，非 ConcurrencySafe。
type SubmitEditBatchTool struct {
	acc *PolishAccumulator
}

// NewSubmitEditBatchTool 构造 batch 工具（共享外部注入的 accumulator）。
func NewSubmitEditBatchTool(acc *PolishAccumulator) *SubmitEditBatchTool {
	return &SubmitEditBatchTool{acc: acc}
}

func (t *SubmitEditBatchTool) Name() string { return "submit_edit_batch" }
func (t *SubmitEditBatchTool) Description() string {
	return "Submit a batch of candidate edits (max 8 per batch, max 4 batches, max 32 total). All old_string values must come verbatim from the original baseline draft."
}
func (t *SubmitEditBatchTool) Label() string { return "提交候选编辑批" }

func (t *SubmitEditBatchTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SubmitEditBatchTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *SubmitEditBatchTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("operation_id", schema.String("Host-generated run id, echo verbatim")).Required(),
		schema.Property("baseline_digest", schema.String("sha256: digest of the baseline draft, echo verbatim")).Required(),
		schema.Property("batch_index", schema.Int("Strictly increasing batch index, 1-4")).Required(),
		schema.Property("edits", schema.Array("Candidate edits (1-8)", schema.Object(
			schema.Property("issue_id", schema.String("p-XXX, must exist in plan with action=edit, not reused")).Required(),
			schema.Property("old_string", schema.String("Exact contiguous baseline text (1-2000 runes)")).Required(),
			schema.Property("new_string", schema.String("Candidate replacement (1-2000 runes)")).Required(),
			schema.Property("reason", schema.String("Edit reason (1-500 runes)")).Required(),
			schema.Property("category", schema.Enum("Issue category", "rhythm", "repetition", "voice", "density", "clarity", "imagery", "transition")).Required(),
			schema.Property("fact_check", schema.Enum("Fact check declaration", "unchanged")).Required(),
		))).Required(),
	)
}

// polishEditRequest 是 submit_edit_batch 的请求（严格模式，指针区分缺字段）。
type polishEditRequest struct {
	OperationID    *string `json:"operation_id"`
	BaselineDigest *string `json:"baseline_digest"`
	BatchIndex     *int    `json:"batch_index"`
	Edits          *[]struct {
		IssueID   *string `json:"issue_id"`
		OldString *string `json:"old_string"`
		NewString *string `json:"new_string"`
		Reason    *string `json:"reason"`
		Category  *string `json:"category"`
		FactCheck *string `json:"fact_check"`
	} `json:"edits"`
}

func (t *SubmitEditBatchTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	t.acc.mu.Lock()
	defer t.acc.mu.Unlock()

	rejectBatch := func(code string) (json.RawMessage, error) {
		return json.Marshal(polishBatchResult{Accepted: 0, Rejected: 0, AcceptedTotal: len(t.acc.accepted),
			Errors: []polishToolError{{Index: -1, Code: code}}})
	}

	var req polishEditRequest
	if err := parsePolishToolArgs(args, &req); err != nil {
		return rejectBatch(PolishErrMalformedJSON)
	}

	// §5.3 步骤 1：operation/baseline 一致（整批拒绝，不进入任何预检）。
	if req.OperationID == nil || *req.OperationID != t.acc.operationID {
		return rejectBatch(PolishErrOpMismatch)
	}
	if req.BaselineDigest == nil || *req.BaselineDigest != t.acc.baselineDigest {
		return rejectBatch(PolishErrBaselineMismatch)
	}

	// §5.3 步骤 2：状态必须为 planned。
	switch t.acc.state {
	case polishAccEmpty:
		return rejectBatch(PolishErrNotPlanned)
	case polishAccFinished:
		return rejectBatch(PolishErrAlreadyFinished)
	}

	// §5.3 步骤 3：batch_index == nextBatch（严格递增，防重复/乱序批次）。
	if req.BatchIndex == nil {
		return rejectBatch(PolishErrFieldRequired)
	}
	if *req.BatchIndex < 1 || *req.BatchIndex > 4 || *req.BatchIndex != t.acc.nextBatch {
		return rejectBatch(PolishErrBadBatchIndex)
	}

	// §5.3 步骤 4：批内 edits ≤ 8。
	if req.Edits == nil || len(*req.Edits) == 0 {
		return rejectBatch(PolishErrFieldRequired)
	}
	if len(*req.Edits) > 8 {
		return rejectBatch(PolishErrBatchLimit)
	}

	// §5.3 步骤 5：累计 accepted+rejected ≤ 32（rejected 计入上限是有意设计）。
	if len(t.acc.accepted)+len(t.acc.rejected)+len(*req.Edits) > maxPolishEdits {
		return rejectBatch(PolishErrTotalLimit)
	}

	// 逐条预检（§5.3 步骤 6–14，按序短路；同批内多条互相检查重叠）。
	normInput := normalizePolishAnchorMapped(t.acc.baselineContent)
	accepted := 0
	rejected := 0
	errors := []polishToolError{}
	batchAccepted := make([]PolishAcceptedEdit, 0, len(*req.Edits))

	for i, raw := range *req.Edits {
		rejectEdit := func(code string) {
			rejected++
			errors = append(errors, polishToolError{Index: i, Code: code})
			if raw.IssueID != nil {
				t.acc.rejected = append(t.acc.rejected, PolishRejectedEdit{IssueID: *raw.IssueID, Code: code, Index: i})
			}
		}

		// 步骤 6：issue 存在且 action=edit 且未被使用（issue_id_invalid 先于查找）。
		if raw.IssueID == nil {
			rejectEdit(PolishErrFieldRequired)
			continue
		}
		if !polishIssueIDRe.MatchString(*raw.IssueID) {
			rejectEdit(PolishErrIssueIDInvalid)
			continue
		}
		issue := t.acc.findPlanIssue(*raw.IssueID)
		if issue == nil {
			rejectEdit(PolishErrIssueUnknown)
			continue
		}
		if issue.Action != "edit" {
			rejectEdit(PolishErrIssueNotEditable)
			continue
		}
		if t.acc.usedIssueIDs[*raw.IssueID] {
			rejectEdit(PolishErrIssueReused)
			continue
		}

		// 步骤 7：old_string 非空、≤2000 runes。
		if raw.OldString == nil {
			rejectEdit(PolishErrFieldRequired)
			continue
		}
		if *raw.OldString == "" {
			rejectEdit(PolishErrEmptyOld)
			continue
		}
		if utf8.RuneCountInString(*raw.OldString) > maxPolishEditOldRunes {
			rejectEdit(PolishErrAnchorTooLong)
			continue
		}

		// 步骤 8：new_string 非空、≤2000 runes。
		if raw.NewString == nil {
			rejectEdit(PolishErrFieldRequired)
			continue
		}
		if *raw.NewString == "" {
			rejectEdit(PolishErrEmptyNew)
			continue
		}
		if utf8.RuneCountInString(*raw.NewString) > maxPolishEditOldRunes {
			rejectEdit(PolishErrNewTooLong)
			continue
		}

		// 步骤 9：old/new 归一化后不同（复用现有 no-op 判定）。
		if normalizePolishAnchor(*raw.OldString) == normalizePolishAnchor(*raw.NewString) {
			rejectEdit(PolishErrNoop)
			continue
		}

		// 步骤 10：old_string 唯一命中 baseline（exact → normalized 两级，复用 locatePolishEdit）。
		loc, reason, ok := locatePolishEdit(t.acc.baselineContent, &normInput,
			PolishEditItem{OldString: *raw.OldString, NewString: *raw.NewString}, i)
		if !ok {
			if reason == PolishEditDropAnchorAmbiguous {
				rejectEdit(PolishErrAnchorAmbiguous)
			} else {
				rejectEdit(PolishErrAnchorMissing)
			}
			continue
		}

		// 步骤 11：与已接受候选不重叠（byte range 判定；含本批已接受的）。
		if polishAcceptedOverlaps(loc.Start, loc.End, t.acc.accepted) ||
			polishAcceptedOverlaps(loc.Start, loc.End, batchAccepted) {
			rejectEdit(PolishErrAnchorOverlap)
			continue
		}

		// 步骤 12：数字保持 D(old) == D(new)。
		if !polishDigitsPreserved(*raw.OldString, *raw.NewString) {
			rejectEdit(PolishErrFactChanged)
			continue
		}

		// 步骤 13：fact_check == "unchanged"。
		if raw.FactCheck == nil {
			rejectEdit(PolishErrFieldRequired)
			continue
		}
		if *raw.FactCheck != "unchanged" {
			rejectEdit(PolishErrFactCheckInvalid)
			continue
		}

		// 步骤 14：枚举/必填字段合法。
		if raw.Reason == nil {
			rejectEdit(PolishErrFieldRequired)
			continue
		}
		if n := utf8.RuneCountInString(*raw.Reason); n < 1 || n > maxPolishReasonRunes {
			rejectEdit(PolishErrValueOutOfRange)
			continue
		}
		if raw.Category == nil || !containsString(polishPlanCategories, *raw.Category) {
			rejectEdit(PolishErrBadEnum)
			continue
		}

		// 通过：存入 accepted（含 byte range 与匹配模式），标记 usedIssueIDs。
		acc := PolishAcceptedEdit{
			IssueID:   *raw.IssueID,
			OldString: *raw.OldString,
			NewString: *raw.NewString,
			Reason:    *raw.Reason,
			Category:  *raw.Category,
			Start:     loc.Start,
			End:       loc.End,
			Mode:      loc.Mode,
		}
		t.acc.accepted = append(t.acc.accepted, acc)
		batchAccepted = append(batchAccepted, acc)
		t.acc.usedIssueIDs[*raw.IssueID] = true
		accepted++
	}

	// 批级检查通过后：本批 ≥1 条被接受才推进 nextBatch（严格递增；同批内多条
	// nextBatch 不变）。整批全拒（anchor 失准）不推进——模型可重试同一 batch_index，
	// 但 rejected 计入 32 上限（§5.3 步骤 5 注：anchor 失准消耗预算，激励提供准确锚点）。
	if accepted > 0 {
		t.acc.nextBatch++
	}

	return json.Marshal(polishBatchResult{
		Accepted:      accepted,
		Rejected:      rejected,
		AcceptedTotal: len(t.acc.accepted),
		Errors:        errors,
	})
}

// findPlanIssue 在 plan 中按 issue_id 查找 issue（调用方须持锁）。
func (a *PolishAccumulator) findPlanIssue(issueID string) *PolishPlanIssue {
	if a.plan == nil {
		return nil
	}
	for i := range a.plan.Issues {
		if a.plan.Issues[i].IssueID == issueID {
			return &a.plan.Issues[i]
		}
	}
	return nil
}

// polishAcceptedOverlaps 判定 [start,end) 与已接受候选是否重叠（byte range）。
func polishAcceptedOverlaps(start, end int, accepted []PolishAcceptedEdit) bool {
	for _, a := range accepted {
		if start < a.End && a.Start < end {
			return true
		}
	}
	return false
}
