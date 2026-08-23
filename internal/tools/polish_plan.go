package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
)

// ── submit_polish_plan 工具（schema docs/polisher-candidate-tools-schema.md §4） ──
//
// 提交整章审阅计划（既有 findings + 自发现 issue），路由事实/结构 issue 到 writer。
// 必须恰好调用一次、最先调用。校验通过后 accumulator 状态 empty → planned。

// 枚举（§4.2 / §5.2 / §6.2）。
var (
	polishPlanOrigins    = []string{"existing_finding", "polisher_discovered"}
	polishPlanCategories = []string{"rhythm", "repetition", "voice", "density", "clarity", "imagery", "transition"}
	polishPlanPriorities = []string{"high", "medium", "low"}
	polishFactRisks      = []string{"none", "low", "high"}
	polishPlanActions    = []string{"edit", "defer_to_writer", "no_op"}
	polishFinishStatuses = []string{"complete", "partial", "no_op", "escalate"}
)

// 格式约束（§4.2 / §5.2 / §6.2）。
var (
	polishIssueIDRe      = regexp.MustCompile(`^p-[0-9]{3}$`)
	polishBaselineDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

const (
	// maxPolishPlanIssues 是 plan 的 issues 硬上限（§4.2）。
	maxPolishPlanIssues = 32
	// maxPolishSourceFindingIDs 是 source_finding_ids 项数上限（§4.2）。
	maxPolishSourceFindingIDs = 16
	// maxPolishSourceFindingIDRunes 是单条 source_finding_id 长度上限（§4.2）。
	maxPolishSourceFindingIDRunes = 32
	// maxPolishPlanAnchorRunes 是 plan issue anchor 长度上限（§4.2）。
	maxPolishPlanAnchorRunes = 2000
	// maxPolishProblemRunes 是 problem 长度上限（§4.2）。
	maxPolishProblemRunes = 1000
	// maxPolishEditGoalRunes 是 edit_goal 长度上限（§4.2）。
	maxPolishEditGoalRunes = 1000
	// maxPolishOverallAssessmentRunes 是 overall_assessment 长度上限（§4.2）。
	maxPolishOverallAssessmentRunes = 2000
	// maxPolishOperationIDRunes 是 operation_id 长度上限（§4.2）。
	maxPolishOperationIDRunes = 64
	// maxPolishReasonRunes 是 batch edit reason 长度上限（§5.2）。
	maxPolishReasonRunes = 500
	// maxPolishUnresolvedRunes 是 finish unresolved reason 长度上限（§6.2）。
	maxPolishUnresolvedRunes = 200
	// maxPolishSummaryRunes 是 finish summary 长度上限（§6.2）。
	maxPolishSummaryRunes = 1000
	// maxPolishUnresolvedItems 是 finish unresolved 项数上限（§6.2）。
	maxPolishUnresolvedItems = 32
)

// SubmitPolishPlanTool 是 submit_polish_plan 工具。非 ReadOnly，非 ConcurrencySafe。
type SubmitPolishPlanTool struct {
	acc *PolishAccumulator
}

// NewSubmitPolishPlanTool 构造 plan 工具（共享外部注入的 accumulator）。
func NewSubmitPolishPlanTool(acc *PolishAccumulator) *SubmitPolishPlanTool {
	return &SubmitPolishPlanTool{acc: acc}
}

func (t *SubmitPolishPlanTool) Name() string { return "submit_polish_plan" }
func (t *SubmitPolishPlanTool) Description() string {
	return "Submit the full-chapter review plan before any edit batches. Must be called exactly once, first. Declares issues (existing findings + self-discovered) and routes fact/structural issues to the writer."
}
func (t *SubmitPolishPlanTool) Label() string { return "提交精修计划" }

func (t *SubmitPolishPlanTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SubmitPolishPlanTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *SubmitPolishPlanTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("operation_id", schema.String("Host-generated run id, echo verbatim")).Required(),
		schema.Property("baseline_digest", schema.String("sha256: digest of the baseline draft, echo verbatim")).Required(),
		schema.Property("overall_assessment", schema.String("Full-chapter style assessment")).Required(),
		schema.Property("planned_edit_count", schema.Int("Soft target edit count, 0-32")).Required(),
		schema.Property("issues", schema.Array("Declared issues (0-32)", schema.Object(
			schema.Property("issue_id", schema.String("p-XXX")).Required(),
			schema.Property("source_finding_ids", schema.Array("Optional source finding ids (<=16)", schema.String("finding id"))),
			schema.Property("origin", schema.Enum("Issue origin", "existing_finding", "polisher_discovered")).Required(),
			schema.Property("category", schema.Enum("Issue category", "rhythm", "repetition", "voice", "density", "clarity", "imagery", "transition")).Required(),
			schema.Property("priority", schema.Enum("Issue priority", "high", "medium", "low")).Required(),
			schema.Property("anchor", schema.String("Exact contiguous baseline text (intent declaration)")).Required(),
			schema.Property("problem", schema.String("Problem description")).Required(),
			schema.Property("edit_goal", schema.String("Edit goal")).Required(),
			schema.Property("fact_risk", schema.Enum("Fact risk", "none", "low", "high")).Required(),
			schema.Property("action", schema.Enum("Action", "edit", "defer_to_writer", "no_op")).Required(),
		))).Required(),
	)
}

// polishPlanRequest 是 submit_polish_plan 的请求（严格模式，指针区分缺字段）。
type polishPlanRequest struct {
	OperationID       *string `json:"operation_id"`
	BaselineDigest    *string `json:"baseline_digest"`
	OverallAssessment *string `json:"overall_assessment"`
	PlannedEditCount  *int    `json:"planned_edit_count"`
	Issues            *[]struct {
		IssueID          *string  `json:"issue_id"`
		SourceFindingIDs []string `json:"source_finding_ids"`
		Origin           *string  `json:"origin"`
		Category         *string  `json:"category"`
		Priority         *string  `json:"priority"`
		Anchor           *string  `json:"anchor"`
		Problem          *string  `json:"problem"`
		EditGoal         *string  `json:"edit_goal"`
		FactRisk         *string  `json:"fact_risk"`
		Action           *string  `json:"action"`
	} `json:"issues"`
}

func (t *SubmitPolishPlanTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	t.acc.mu.Lock()
	defer t.acc.mu.Unlock()

	reject := func(code string) (json.RawMessage, error) {
		return json.Marshal(polishPlanResult{Accepted: 0, Rejected: 1,
			Errors: []polishToolError{{Index: -1, Code: code}}})
	}

	var req polishPlanRequest
	if err := parsePolishToolArgs(args, &req); err != nil {
		return reject(PolishErrMalformedJSON)
	}

	// §3.1：operation_id / baseline_digest 必须与 accumulator 一致（整批拒绝，不进入预检）。
	if req.OperationID == nil || *req.OperationID != t.acc.operationID {
		return reject(PolishErrOpMismatch)
	}
	if req.BaselineDigest == nil || *req.BaselineDigest != t.acc.baselineDigest {
		return reject(PolishErrBaselineMismatch)
	}

	// §3.2 状态机：empty → planned；重复 plan → plan_exists；finish 后 → already_finished。
	switch t.acc.state {
	case polishAccPlanned:
		return reject(PolishErrPlanExists)
	case polishAccFinished:
		return reject(PolishErrAlreadyFinished)
	}

	// §4.2 字段约束。
	if req.OverallAssessment == nil {
		return reject(PolishErrFieldRequired)
	}
	if n := utf8.RuneCountInString(*req.OverallAssessment); n < 1 || n > maxPolishOverallAssessmentRunes {
		return reject(PolishErrValueOutOfRange)
	}
	if req.PlannedEditCount == nil {
		return reject(PolishErrFieldRequired)
	}
	if *req.PlannedEditCount < 0 || *req.PlannedEditCount > maxPolishEdits {
		return reject(PolishErrValueOutOfRange)
	}
	if req.Issues == nil {
		return reject(PolishErrFieldRequired)
	}
	if len(*req.Issues) > maxPolishPlanIssues {
		return reject(PolishErrPlanLimit)
	}

	// 逐条 issue 校验（§4.2 + §4.3 一致性）。
	seen := make(map[string]bool, len(*req.Issues))
	issues := make([]PolishPlanIssue, 0, len(*req.Issues))
	for _, raw := range *req.Issues {
		if raw.IssueID == nil {
			return reject(PolishErrFieldRequired)
		}
		if !polishIssueIDRe.MatchString(*raw.IssueID) || seen[*raw.IssueID] {
			return reject(PolishErrIssueIDInvalid)
		}
		seen[*raw.IssueID] = true

		if len(raw.SourceFindingIDs) > maxPolishSourceFindingIDs {
			return reject(PolishErrValueOutOfRange)
		}
		for _, fid := range raw.SourceFindingIDs {
			if n := utf8.RuneCountInString(fid); n < 1 || n > maxPolishSourceFindingIDRunes {
				return reject(PolishErrValueOutOfRange)
			}
		}

		if raw.Origin == nil || !containsString(polishPlanOrigins, *raw.Origin) {
			return reject(PolishErrBadEnum)
		}
		if raw.Category == nil || !containsString(polishPlanCategories, *raw.Category) {
			return reject(PolishErrBadEnum)
		}
		if raw.Priority == nil || !containsString(polishPlanPriorities, *raw.Priority) {
			return reject(PolishErrBadEnum)
		}
		if raw.Anchor == nil {
			return reject(PolishErrFieldRequired)
		}
		if n := utf8.RuneCountInString(*raw.Anchor); n < 1 || n > maxPolishPlanAnchorRunes {
			return reject(PolishErrValueOutOfRange)
		}
		if raw.Problem == nil {
			return reject(PolishErrFieldRequired)
		}
		if n := utf8.RuneCountInString(*raw.Problem); n < 1 || n > maxPolishProblemRunes {
			return reject(PolishErrValueOutOfRange)
		}
		if raw.EditGoal == nil {
			return reject(PolishErrFieldRequired)
		}
		if n := utf8.RuneCountInString(*raw.EditGoal); n < 1 || n > maxPolishEditGoalRunes {
			return reject(PolishErrValueOutOfRange)
		}
		if raw.FactRisk == nil || !containsString(polishFactRisks, *raw.FactRisk) {
			return reject(PolishErrBadEnum)
		}
		if raw.Action == nil || !containsString(polishPlanActions, *raw.Action) {
			return reject(PolishErrBadEnum)
		}

		// §4.3：fact_risk=high ⇒ action 必须为 defer_to_writer。
		if *raw.FactRisk == "high" && *raw.Action != "defer_to_writer" {
			return reject(PolishErrFactRiskEditConflict)
		}

		issues = append(issues, PolishPlanIssue{
			IssueID:          *raw.IssueID,
			SourceFindingIDs: raw.SourceFindingIDs,
			Origin:           *raw.Origin,
			Category:         *raw.Category,
			Priority:         *raw.Priority,
			Anchor:           *raw.Anchor,
			Problem:          *raw.Problem,
			EditGoal:         *raw.EditGoal,
			FactRisk:         *raw.FactRisk,
			Action:           *raw.Action,
		})
	}

	// 通过：存 plan（Digest=sha256(规范化 plan JSON)），状态 → planned。
	plan := &PolishPlanRecord{
		OverallAssessment: *req.OverallAssessment,
		PlannedEditCount:  *req.PlannedEditCount,
		Issues:            issues,
	}
	plan.Digest = polishPlanDigest(t.acc.operationID, t.acc.baselineDigest, plan)
	t.acc.plan = plan
	t.acc.state = polishAccPlanned
	t.acc.nextBatch = 1

	return json.Marshal(polishPlanResult{Accepted: 1, Rejected: 0, Errors: []polishToolError{}})
}

// polishPlanDigest 计算规范化 plan JSON 的 sha256（审计用，§3.3/§9 PlanDigest）。
// 规范化 = 固定字段顺序重新序列化（含 operation_id/baseline_digest，保证跨 run 唯一）。
func polishPlanDigest(operationID, baselineDigest string, plan *PolishPlanRecord) string {
	canonical := struct {
		OperationID       string            `json:"operation_id"`
		BaselineDigest    string            `json:"baseline_digest"`
		OverallAssessment string            `json:"overall_assessment"`
		PlannedEditCount  int               `json:"planned_edit_count"`
		Issues            []PolishPlanIssue `json:"issues"`
	}{
		OperationID:       operationID,
		BaselineDigest:    baselineDigest,
		OverallAssessment: plan.OverallAssessment,
		PlannedEditCount:  plan.PlannedEditCount,
		Issues:            plan.Issues,
	}
	sum := sha256.Sum256(mustMarshalCanonicalPlan(canonical))
	return hex.EncodeToString(sum[:])
}

// mustMarshalCanonicalPlan 序列化规范化 plan（字段顺序固定，marshal 不会失败）。
func mustMarshalCanonicalPlan(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// 不可达：全部字段为基本类型。
		panic(fmt.Sprintf("canonical plan marshal: %v", err))
	}
	return b
}

// containsString 报告 s 是否在枚举列表中。
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
