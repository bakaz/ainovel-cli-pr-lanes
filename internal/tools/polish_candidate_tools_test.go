package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── 候选工具协议纯单元测试（不依赖 store/runner） ────────────────────────
// 覆盖 schema docs/polisher-candidate-tools-schema.md §13 要求 + oracle 审查修订。

const (
	testOpID     = "pol-1-1728000000000000-a1b2"
	testBaseline = "她站在窗前，看着窗外的梧桐树。2024年的秋天来得比往年更早，第3章的故事从这里开始。她数了数，窗外有5只麻雀。"
)

var testBaselineDigest = "sha256:" + strings.Repeat("ab", 32)

// ── 测试辅助 ───────────────────────────────────────────────────────────

func newPolishTestAcc(t *testing.T) *PolishAccumulator {
	t.Helper()
	return NewPolishAccumulator(testOpID, testBaselineDigest, testBaseline, 1)
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// testResult 是三种工具返回的通用解析结构。
type testResult struct {
	Accepted      int               `json:"accepted"`
	Rejected      int               `json:"rejected"`
	AcceptedTotal int               `json:"accepted_total"`
	Errors        []polishToolError `json:"errors"`
}

func parseResult(t *testing.T, raw json.RawMessage) testResult {
	t.Helper()
	var r testResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse result %s: %v", raw, err)
	}
	return r
}

func wantErr(t *testing.T, r testResult, code string) {
	t.Helper()
	if len(r.Errors) != 1 || r.Errors[0].Code != code {
		t.Fatalf("errors = %+v, want single %q", r.Errors, code)
	}
}

// ── 请求构造 ───────────────────────────────────────────────────────────

func polishPlanArgs(t *testing.T, op, digest string, issues []map[string]any) json.RawMessage {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"operation_id":       op,
		"baseline_digest":    digest,
		"overall_assessment": "全章节奏偏慢，短句不足。",
		"planned_edit_count": len(issues),
		"issues":             issues,
	})
}

func polishPlanIssue(id, action string) map[string]any {
	return map[string]any{
		"issue_id":  id,
		"origin":    "existing_finding",
		"category":  "rhythm",
		"priority":  "high",
		"anchor":    "锚点文本",
		"problem":   "问题描述",
		"edit_goal": "修改目标",
		"fact_risk": "none",
		"action":    action,
	}
}

func polishBatchArgs(t *testing.T, op, digest string, index int, edits []map[string]any) json.RawMessage {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"operation_id":    op,
		"baseline_digest": digest,
		"batch_index":     index,
		"edits":           edits,
	})
}

func polishBatchEdit(id, oldS, newS string) map[string]any {
	return map[string]any{
		"issue_id":   id,
		"old_string": oldS,
		"new_string": newS,
		"reason":     "修改理由",
		"category":   "rhythm",
		"fact_check": "unchanged",
	}
}

func polishFinishArgs(t *testing.T, op, digest, status string, count int, covered []string, unresolved []map[string]any, summary string) json.RawMessage {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"operation_id":         op,
		"baseline_digest":      digest,
		"status":               status,
		"submitted_edit_count": count,
		"covered_issue_ids":    covered,
		"unresolved":           unresolved,
		"summary":              summary,
	})
}

// submitPlan 提交一个含给定 issues 的 plan，断言成功。
func submitPlan(t *testing.T, acc *PolishAccumulator, issues []map[string]any) {
	t.Helper()
	tool := NewSubmitPolishPlanTool(acc)
	raw, err := tool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, issues))
	if err != nil {
		t.Fatalf("plan execute: %v", err)
	}
	r := parseResult(t, raw)
	if r.Accepted != 1 || r.Rejected != 0 || len(r.Errors) != 0 {
		t.Fatalf("plan result = %+v, want accepted=1", r)
	}
}

// ── 1. 正常流程：plan → batch×N → finish 全通过 ─────────────────────────

func TestPolishCandidateTools_HappyPath(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	finishTool := NewFinishPolishTool(acc)

	issues := []map[string]any{
		polishPlanIssue("p-001", "edit"),
		polishPlanIssue("p-002", "edit"),
		polishPlanIssue("p-003", "no_op"),
		polishPlanIssue("p-004", "edit"),
	}
	submitPlan(t, acc, issues)

	if acc.StateName() != "planned" {
		t.Fatalf("state = %s, want planned", acc.StateName())
	}
	if acc.NextBatch() != 1 {
		t.Fatalf("nextBatch = %d, want 1", acc.NextBatch())
	}
	if acc.Plan() == nil || len(acc.Plan().Issues) != 4 {
		t.Fatalf("plan issues = %d, want 4", len(acc.Plan().Issues))
	}
	if len(acc.Plan().Digest) != 64 {
		t.Fatalf("plan digest = %q, want 64 hex chars", acc.Plan().Digest)
	}

	// batch 1：2 条通过。
	raw, err := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
		polishBatchEdit("p-002", "2024年的秋天来得比往年更早", "2024年的秋天来得比往年更早一些"),
	}))
	if err != nil {
		t.Fatalf("batch1 execute: %v", err)
	}
	r := parseResult(t, raw)
	if r.Accepted != 2 || r.Rejected != 0 || r.AcceptedTotal != 2 || len(r.Errors) != 0 {
		t.Fatalf("batch1 result = %+v, want accepted=2 total=2", r)
	}
	if acc.NextBatch() != 2 {
		t.Fatalf("nextBatch = %d, want 2", acc.NextBatch())
	}
	accd := acc.Accepted()
	if len(accd) != 2 || accd[0].Start != 0 || accd[0].End != 42 || accd[0].Mode != PolishEditMatchExact {
		t.Fatalf("accepted[0] = %+v, want byte range [0,42) exact", accd[0])
	}

	// batch 2：1 条通过。
	raw, err = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 2, []map[string]any{
		polishBatchEdit("p-004", "窗外有5只麻雀", "窗外有5只麻雀，它们很安静"),
	}))
	if err != nil {
		t.Fatalf("batch2 execute: %v", err)
	}
	r = parseResult(t, raw)
	if r.Accepted != 1 || r.AcceptedTotal != 3 {
		t.Fatalf("batch2 result = %+v, want accepted=1 total=3", r)
	}
	if acc.NextBatch() != 3 {
		t.Fatalf("nextBatch = %d, want 3", acc.NextBatch())
	}

	// finish：complete。
	raw, err = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
		"complete", 3, []string{"p-001", "p-002", "p-003", "p-004"}, nil, "本次精修完成"))
	if err != nil {
		t.Fatalf("finish execute: %v", err)
	}
	r = parseResult(t, raw)
	if r.Accepted != 1 || r.Rejected != 0 || len(r.Errors) != 0 {
		t.Fatalf("finish result = %+v, want accepted=1", r)
	}
	if acc.StateName() != "finished" {
		t.Fatalf("state = %s, want finished", acc.StateName())
	}
	f := acc.Finish()
	if f == nil || f.Status != "complete" || f.SubmittedEditCount != 3 || len(f.CoveredIssueIDs) != 4 {
		t.Fatalf("finish record = %+v", f)
	}
}

// ── 2. 调用顺序违规 ────────────────────────────────────────────────────

func TestPolishCandidateTools_OrderViolations(t *testing.T) {
	// batch/finish 先于 plan → not_planned。
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	finishTool := NewFinishPolishTool(acc)

	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前", "她倚窗而立"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrNotPlanned)

	raw, _ = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
		"complete", 0, nil, nil, "摘要"))
	wantErr(t, parseResult(t, raw), PolishErrNotPlanned)

	// 重复 plan → plan_exists。
	planTool := NewSubmitPolishPlanTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
	raw, _ = planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{
		polishPlanIssue("p-002", "edit"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrPlanExists)

	// finish 后任何调用 → already_finished。
	raw, _ = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
		"no_op", 0, []string{}, nil, "无需修改"))
	if r := parseResult(t, raw); r.Accepted != 1 {
		t.Fatalf("finish = %+v, want accepted=1", r)
	}
	raw, _ = planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, nil))
	wantErr(t, parseResult(t, raw), PolishErrAlreadyFinished)
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前", "她倚窗而立"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrAlreadyFinished)
	raw, _ = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
		"complete", 0, nil, nil, "摘要"))
	wantErr(t, parseResult(t, raw), PolishErrAlreadyFinished)
}

// ── 3. operation_id / baseline_digest 不一致 ───────────────────────────

func TestPolishCandidateTools_OpBaselineMismatch(t *testing.T) {
	acc := newPolishTestAcc(t)
	planTool := NewSubmitPolishPlanTool(acc)

	raw, _ := planTool.Execute(t.Context(), polishPlanArgs(t, "pol-wrong-1", testBaselineDigest, nil))
	wantErr(t, parseResult(t, raw), PolishErrOpMismatch)

	raw, _ = planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, "sha256:"+strings.Repeat("cd", 32), nil))
	wantErr(t, parseResult(t, raw), PolishErrBaselineMismatch)

	// batch 同样整批拒绝。
	batchTool := NewSubmitEditBatchTool(acc)
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, "pol-wrong-1", testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前", "她倚窗而立"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrOpMismatch)
}

// ── 4. batch_index 乱序/重复 ───────────────────────────────────────────

func TestPolishCandidateTools_BadBatchIndex(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})

	// 首批就跳号（期望 1，提交 2）。
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 2, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrBadBatchIndex)

	// 正常提交批 1。
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	if r := parseResult(t, raw); r.Accepted != 1 {
		t.Fatalf("batch1 = %+v, want accepted=1", r)
	}

	// 重复批 1 → bad_batch_index（期望 2）。
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrBadBatchIndex)
}

// ── 5. 每批 8 条上限 / 累计 32 上限（rejected 计入） ────────────────────

func TestPolishCandidateTools_BatchLimit(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})

	edits := make([]map[string]any, 0, 9)
	for i := 0; i < 9; i++ {
		edits = append(edits, polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"))
	}
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, edits))
	r := parseResult(t, raw)
	if r.Accepted != 0 || len(r.Errors) != 1 || r.Errors[0].Index != -1 || r.Errors[0].Code != PolishErrBatchLimit {
		t.Fatalf("result = %+v, want batch_limit index=-1", r)
	}
}

func TestPolishCandidateTools_TotalLimit(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)

	// 8 个 edit issue，每批 8 条全部 anchor 失准被拒（rejected 计入 32 上限）。
	issues := make([]map[string]any, 0, 8)
	for i := 1; i <= 8; i++ {
		issues = append(issues, polishPlanIssue(sprintfP(i), "edit"))
	}
	submitPlan(t, acc, issues)

	badEdits := make([]map[string]any, 0, 8)
	for i := 1; i <= 8; i++ {
		badEdits = append(badEdits, polishBatchEdit(sprintfP(i), "不存在的文本", "替换文本"))
	}
	for batch := 0; batch < 4; batch++ {
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, badEdits))
		r := parseResult(t, raw)
		if r.Accepted != 0 || r.Rejected != 8 {
			t.Fatalf("batch %d = %+v, want 8 rejected", batch+1, r)
		}
	}
	if got := len(acc.Rejected()); got != 32 {
		t.Fatalf("rejected = %d, want 32", got)
	}
	// 全拒批次不推进 nextBatch（可重试同一 index），但预算耗尽后整批拒绝。
	if acc.NextBatch() != 1 {
		t.Fatalf("nextBatch = %d, want 1 (全拒不推进)", acc.NextBatch())
	}
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, badEdits))
	r := parseResult(t, raw)
	if r.Accepted != 0 || len(r.Errors) != 1 || r.Errors[0].Index != -1 || r.Errors[0].Code != PolishErrTotalLimit {
		t.Fatalf("result = %+v, want total_limit index=-1", r)
	}
}

// 预算被 rejected 消耗后，合法 edit 也整批拒绝（§5.3 步骤 5 注：rejected 计入 32 上限）。
func TestPolishCandidateTools_TotalLimitLegalEditRejected(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)

	issues := make([]map[string]any, 0, 8)
	for i := 1; i <= 8; i++ {
		issues = append(issues, polishPlanIssue(sprintfP(i), "edit"))
	}
	submitPlan(t, acc, issues)

	// 3 批 × 8 条 anchor 失准被拒 = 24 rejected（全拒不推进 nextBatch）。
	badEdits := make([]map[string]any, 0, 8)
	for i := 1; i <= 8; i++ {
		badEdits = append(badEdits, polishBatchEdit(sprintfP(i), "不存在的文本", "替换文本"))
	}
	for batch := 0; batch < 3; batch++ {
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, badEdits))
		if r := parseResult(t, raw); r.Accepted != 0 || r.Rejected != 8 {
			t.Fatalf("rejected batch %d = %+v, want 8 rejected", batch+1, r)
		}
	}
	// 再 1 条被拒 → 累计 25 rejected。
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "不存在的文本", "替换文本"),
	}))
	if r := parseResult(t, raw); r.Accepted != 0 || r.Rejected != 1 {
		t.Fatalf("rejected batch 4 = %+v, want 1 rejected", r)
	}
	if got := len(acc.Rejected()); got != 25 {
		t.Fatalf("rejected = %d, want 25", got)
	}

	// 合法 edit 批（8 条，锚点均有效）：25+8=33 > 32 → 整批 total_limit。
	// 批级检查（步骤 5）在逐条预检之前短路，edit 内容不影响断言。
	legalEdits := []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
		polishBatchEdit("p-002", "2024年的秋天来得比往年更早", "2024年的秋天来得比往年更早一些"),
		polishBatchEdit("p-003", "第3章的故事从这里开始", "第3章的故事从这里展开"),
		polishBatchEdit("p-004", "她数了数", "她仔细数了数"),
		polishBatchEdit("p-005", "窗外有5只麻雀", "窗外有5只麻雀，它们很安静"),
		polishBatchEdit("p-006", "看着窗外的梧桐树", "望着窗外的梧桐树"),
		polishBatchEdit("p-007", "来得比往年更早", "来得比往年更早一些"),
		polishBatchEdit("p-008", "的故事从这里开始", "的故事从这里展开"),
	}
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, legalEdits))
	r := parseResult(t, raw)
	if r.Accepted != 0 || len(r.Errors) != 1 || r.Errors[0].Index != -1 || r.Errors[0].Code != PolishErrTotalLimit {
		t.Fatalf("legal batch = %+v, want total_limit index=-1", r)
	}
}

func sprintfP(i int) string {
	return "p-" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// ── 6. issue_id 跨批复用 ───────────────────────────────────────────────

func TestPolishCandidateTools_IssueReused(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})

	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	if r := parseResult(t, raw); r.Accepted != 1 {
		t.Fatalf("batch1 = %+v, want accepted=1", r)
	}
	// 批 2 复用 p-001 → issue_reused。
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 2, []map[string]any{
		polishBatchEdit("p-001", "2024年的秋天来得比往年更早", "2024年的秋天来得比往年更早一些"),
	}))
	r := parseResult(t, raw)
	if r.Accepted != 0 || r.Rejected != 1 || r.Errors[0].Code != PolishErrIssueReused || r.Errors[0].Index != 0 {
		t.Fatalf("result = %+v, want issue_reused index=0", r)
	}
}

// ── 7. anchor 缺失/歧义/重叠/超长/空 old/空 new/noop ───────────────────

func TestPolishCandidateTools_AnchorChecks(t *testing.T) {
	cases := []struct {
		name string
		old  string
		new  string
		code string
	}{
		{"anchor_missing", "不存在的文本", "替换文本", PolishErrAnchorMissing},
		{"weak_anchor", "；", "。", PolishErrAnchorMissing}, // 弱锚点（非空白 rune < 8）不做 normalized
		{"anchor_ambiguous", "她", "他", PolishErrAnchorAmbiguous},
		{"anchor_too_long", strings.Repeat("长", 2001), "替换", PolishErrAnchorTooLong},
		{"empty_old", "", "替换", PolishErrEmptyOld},
		{"empty_new", "她站在窗前", "", PolishErrEmptyNew},
		{"new_too_long", "她站在窗前", strings.Repeat("长", 2001), PolishErrNewTooLong},
		{"noop_raw", "她站在窗前", "她站在窗前", PolishErrNoop},
		{"noop_normalized", "她站在窗前", "她站在窗前 ", PolishErrNoop}, // 仅行尾空白差异
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newPolishTestAcc(t)
			batchTool := NewSubmitEditBatchTool(acc)
			submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
			raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
				polishBatchEdit("p-001", tc.old, tc.new),
			}))
			r := parseResult(t, raw)
			if r.Accepted != 0 || r.Rejected != 1 || r.Errors[0].Code != tc.code || r.Errors[0].Index != 0 {
				t.Fatalf("result = %+v, want %s index=0", r, tc.code)
			}
		})
	}
}

func TestPolishCandidateTools_AnchorOverlap(t *testing.T) {
	// 批内互相检查重叠：第二条与第一条的 byte range 重叠。
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{
		polishPlanIssue("p-001", "edit"),
		polishPlanIssue("p-002", "edit"),
	})
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"), // [0,45)
		polishBatchEdit("p-002", "看着窗外的梧桐树", "望着窗外的梧桐树"),             // [18,51) 重叠
	}))
	r := parseResult(t, raw)
	if r.Accepted != 1 || r.Rejected != 1 || r.Errors[0].Code != PolishErrAnchorOverlap || r.Errors[0].Index != 1 {
		t.Fatalf("result = %+v, want accepted=1 rejected=1 anchor_overlap index=1", r)
	}

	// 跨批重叠：批 2 与批 1 已接受候选重叠。
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 2, []map[string]any{
		polishBatchEdit("p-002", "看着窗外的梧桐树", "望着窗外的梧桐树"),
	}))
	r = parseResult(t, raw)
	if r.Accepted != 0 || r.Rejected != 1 || r.Errors[0].Code != PolishErrAnchorOverlap {
		t.Fatalf("cross-batch result = %+v, want anchor_overlap", r)
	}
}

// ── 8. 数字保持：同位数字替换必须拒绝；无数字改动通过 ───────────────────

func TestPolishCandidateTools_DigitPreservation(t *testing.T) {
	cases := []struct {
		name string
		old  string
		new  string
		code string // "" = 应通过
	}{
		{"digit_replaced_same_pos", "第3章的故事从这里开始", "第5章的故事从这里开始", PolishErrFactChanged},
		{"year_changed", "2024年的秋天来得比往年更早", "2025年的秋天来得比往年更早", PolishErrFactChanged},
		{"digit_removed", "窗外有5只麻雀", "窗外有五只麻雀", PolishErrFactChanged},
		{"digit_added", "窗外有5只麻雀", "窗外有5只麻雀和6只燕子", PolishErrFactChanged},
		{"digit_order_changed", "2024年的秋天来得比往年更早，第3章的故事从这里开始", "第3年的秋天来得比往年更早，2024章的故事从这里开始", PolishErrFactChanged}, // D(old)=["2024","3"] vs D(new)=["3","2024"]，顺序变化
		{"digits_unchanged", "窗外有5只麻雀", "窗外有5只麻雀，它们很安静", ""},
		{"no_digits", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newPolishTestAcc(t)
			batchTool := NewSubmitEditBatchTool(acc)
			submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
			raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
				polishBatchEdit("p-001", tc.old, tc.new),
			}))
			r := parseResult(t, raw)
			if tc.code == "" {
				if r.Accepted != 1 || len(r.Errors) != 0 {
					t.Fatalf("result = %+v, want accepted=1", r)
				}
			} else if r.Accepted != 0 || r.Rejected != 1 || r.Errors[0].Code != tc.code {
				t.Fatalf("result = %+v, want %s", r, tc.code)
			}
		})
	}
}

// polishDigitRuns 纯函数直接验证（§5.3 步骤 12 定义）。
func TestPolishDigitRuns(t *testing.T) {
	got := polishDigitRuns("第3章有5只猫，2024年")
	want := []string{"3", "5", "2024"}
	if len(got) != len(want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runs = %v, want %v", got, want)
		}
	}
	if !polishDigitsPreserved("2024年", "2024年") {
		t.Fatal("identical digits must be preserved")
	}
	if polishDigitsPreserved("2024年", "24年") {
		t.Fatal("run string differs (2024 vs 24) must not be preserved")
	}
	if polishDigitsPreserved("第3章", "第5章") {
		t.Fatal("3→5 must not be preserved")
	}
}

// ── 9. fact_check 非 unchanged 拒绝 ────────────────────────────────────

func TestPolishCandidateTools_FactCheckInvalid(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})

	edit := polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树")
	edit["fact_check"] = "changed"
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{edit}))
	r := parseResult(t, raw)
	if r.Accepted != 0 || r.Rejected != 1 || r.Errors[0].Code != PolishErrFactCheckInvalid {
		t.Fatalf("result = %+v, want fact_check_invalid", r)
	}
}

// ── 10. fact_risk=high + action=edit 拒绝 ──────────────────────────────

func TestPolishCandidateTools_FactRiskEditConflict(t *testing.T) {
	acc := newPolishTestAcc(t)
	planTool := NewSubmitPolishPlanTool(acc)

	iss := polishPlanIssue("p-001", "edit")
	iss["fact_risk"] = "high"
	raw, _ := planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{iss}))
	r := parseResult(t, raw)
	if r.Accepted != 0 || r.Rejected != 1 || r.Errors[0].Code != PolishErrFactRiskEditConflict {
		t.Fatalf("result = %+v, want fact_risk_edit_conflict", r)
	}

	// high + defer_to_writer 合法。
	iss2 := polishPlanIssue("p-002", "defer_to_writer")
	iss2["fact_risk"] = "high"
	raw, _ = planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{iss2}))
	if r := parseResult(t, raw); r.Accepted != 1 {
		t.Fatalf("high+defer = %+v, want accepted=1", r)
	}
}

// ── 11. finish 一致性规则（§6.4） ──────────────────────────────────────

func TestPolishCandidateTools_FinishConsistency(t *testing.T) {
	t.Run("no_op_with_count", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"no_op", 2, []string{}, nil, "无需修改"))
		wantErr(t, parseResult(t, raw), PolishErrStatusCountConflict)
	})

	t.Run("complete_with_unresolved_edited", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{
			polishPlanIssue("p-001", "edit"),
			polishPlanIssue("p-002", "edit"),
		})
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
			polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
		}))
		if r := parseResult(t, raw); r.Accepted != 1 {
			t.Fatalf("batch = %+v", r)
		}
		raw, _ = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"complete", 1, []string{"p-001"}, []map[string]any{
				{"issue_id": "p-002", "reason": "需要结构调整", "recommended_owner": "writer"},
			}, "完成"))
		wantErr(t, parseResult(t, raw), PolishErrUnresolvedEdited)
	})

	t.Run("escalate_empty_unresolved", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"escalate", 0, []string{}, nil, "升级"))
		wantErr(t, parseResult(t, raw), PolishErrUnresolvedEmpty)
	})

	t.Run("covered_lie", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{
			polishPlanIssue("p-001", "edit"),
			polishPlanIssue("p-002", "edit"),
		})
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
			polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
		}))
		if r := parseResult(t, raw); r.Accepted != 1 {
			t.Fatalf("batch = %+v", r)
		}
		// p-002 未提交过 edit 且 action=edit → coverage_not_editable。
		raw, _ = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"complete", 1, []string{"p-001", "p-002"}, nil, "完成"))
		wantErr(t, parseResult(t, raw), PolishErrCoverageNotEditable)
	})

	t.Run("count_mismatch", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
			polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
		}))
		if r := parseResult(t, raw); r.Accepted != 1 {
			t.Fatalf("batch = %+v", r)
		}
		raw, _ = finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"complete", 2, []string{"p-001"}, nil, "完成"))
		wantErr(t, parseResult(t, raw), PolishErrCountMismatch)
	})

	t.Run("no_op_legit", func(t *testing.T) {
		// no_op 合法终态：0 计数、空 covered、空 unresolved。
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "no_op")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"no_op", 0, []string{}, nil, "完整审阅后无需修改"))
		if r := parseResult(t, raw); r.Accepted != 1 {
			t.Fatalf("no_op finish = %+v, want accepted=1", r)
		}
	})
}

// ── 12. JSON 容错：BOM / 单层 fence / 未知字段 / 尾随内容 ───────────────

func TestPolishCandidateTools_JSONTolerance(t *testing.T) {
	base := string(polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{polishPlanIssue("p-001", "edit")}))

	t.Run("bom_prefix", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		raw, _ := planTool.Execute(t.Context(), json.RawMessage("\uFEFF"+base))
		if r := parseResult(t, raw); r.Accepted != 1 {
			t.Fatalf("BOM plan = %+v, want accepted=1", r)
		}
	})

	t.Run("single_fence", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		raw, _ := planTool.Execute(t.Context(), json.RawMessage("```json\n"+base+"\n```"))
		if r := parseResult(t, raw); r.Accepted != 1 {
			t.Fatalf("fenced plan = %+v, want accepted=1", r)
		}
	})

	t.Run("unknown_field", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		var m map[string]any
		_ = json.Unmarshal([]byte(base), &m)
		m["extra"] = 1
		raw, _ := planTool.Execute(t.Context(), mustMarshal(t, m))
		wantErr(t, parseResult(t, raw), PolishErrMalformedJSON)
	})

	t.Run("trailing_content", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		raw, _ := planTool.Execute(t.Context(), json.RawMessage(base+" 已完成"))
		wantErr(t, parseResult(t, raw), PolishErrMalformedJSON)
	})

	t.Run("nested_unknown_field", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		edit := polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树")
		edit["bogus"] = true
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{edit}))
		wantErr(t, parseResult(t, raw), PolishErrMalformedJSON)
	})
}

// ── 13. 枚举非法 / 缺字段 / 长度越界 ───────────────────────────────────

func TestPolishCandidateTools_EnumFieldLength(t *testing.T) {
	t.Run("plan_bad_enum", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		iss := polishPlanIssue("p-001", "edit")
		iss["origin"] = "bogus"
		raw, _ := planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{iss}))
		wantErr(t, parseResult(t, raw), PolishErrBadEnum)
	})

	t.Run("plan_field_required", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		m := map[string]any{
			"operation_id":       testOpID,
			"baseline_digest":    testBaselineDigest,
			"planned_edit_count": 1,
			"issues":             []map[string]any{polishPlanIssue("p-001", "edit")},
		} // 缺 overall_assessment
		raw, _ := planTool.Execute(t.Context(), mustMarshal(t, m))
		wantErr(t, parseResult(t, raw), PolishErrFieldRequired)
	})

	t.Run("plan_value_out_of_range", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		m := map[string]any{
			"operation_id":       testOpID,
			"baseline_digest":    testBaselineDigest,
			"overall_assessment": "判断",
			"planned_edit_count": 33,
			"issues":             []map[string]any{},
		}
		raw, _ := planTool.Execute(t.Context(), mustMarshal(t, m))
		wantErr(t, parseResult(t, raw), PolishErrValueOutOfRange)
	})

	t.Run("plan_limit", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		issues := make([]map[string]any, 0, 33)
		for i := 1; i <= 33; i++ {
			issues = append(issues, polishPlanIssue(sprintfP(i), "edit"))
		}
		// planned_edit_count 保持合法（0），只触发 issues 硬上限。
		m := map[string]any{
			"operation_id":       testOpID,
			"baseline_digest":    testBaselineDigest,
			"overall_assessment": "判断",
			"planned_edit_count": 0,
			"issues":             issues,
		}
		raw, _ := planTool.Execute(t.Context(), mustMarshal(t, m))
		wantErr(t, parseResult(t, raw), PolishErrPlanLimit)
	})

	t.Run("plan_issue_id_invalid", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		iss := polishPlanIssue("x-001", "edit")
		raw, _ := planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{iss}))
		wantErr(t, parseResult(t, raw), PolishErrIssueIDInvalid)
	})

	t.Run("plan_issue_id_duplicate", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		planTool := NewSubmitPolishPlanTool(acc)
		raw, _ := planTool.Execute(t.Context(), polishPlanArgs(t, testOpID, testBaselineDigest, []map[string]any{
			polishPlanIssue("p-001", "edit"),
			polishPlanIssue("p-001", "edit"),
		}))
		wantErr(t, parseResult(t, raw), PolishErrIssueIDInvalid)
	})

	t.Run("batch_bad_enum", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		edit := polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树")
		edit["category"] = "bogus"
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{edit}))
		wantErr(t, parseResult(t, raw), PolishErrBadEnum)
	})

	t.Run("batch_field_required", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		edit := polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树")
		delete(edit, "issue_id")
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{edit}))
		wantErr(t, parseResult(t, raw), PolishErrFieldRequired)
	})

	t.Run("batch_reason_too_long", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		batchTool := NewSubmitEditBatchTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		edit := polishBatchEdit("p-001", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树")
		edit["reason"] = strings.Repeat("理", 501)
		raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{edit}))
		wantErr(t, parseResult(t, raw), PolishErrValueOutOfRange)
	})

	t.Run("finish_status_invalid", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"bogus", 0, []string{}, nil, "摘要"))
		wantErr(t, parseResult(t, raw), PolishErrStatusInvalid)
	})

	t.Run("finish_owner_invalid", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"escalate", 0, []string{}, []map[string]any{
				{"issue_id": "p-001", "reason": "需要结构调整", "recommended_owner": "critic"},
			}, "升级"))
		wantErr(t, parseResult(t, raw), PolishErrOwnerInvalid)
	})

	t.Run("finish_summary_required", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"complete", 0, []string{}, nil, ""))
		wantErr(t, parseResult(t, raw), PolishErrSummaryRequired)
	})

	t.Run("finish_covered_unknown", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"complete", 0, []string{"p-999"}, nil, "摘要"))
		wantErr(t, parseResult(t, raw), PolishErrIssueUnknown)
	})

	t.Run("finish_unresolved_unknown", func(t *testing.T) {
		acc := newPolishTestAcc(t)
		finishTool := NewFinishPolishTool(acc)
		submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})
		raw, _ := finishTool.Execute(t.Context(), polishFinishArgs(t, testOpID, testBaselineDigest,
			"escalate", 0, []string{}, []map[string]any{
				{"issue_id": "p-999", "reason": "需要结构调整", "recommended_owner": "writer"},
			}, "升级"))
		wantErr(t, parseResult(t, raw), PolishErrIssueUnknown)
	})
}

// ── 14. 返回格式：不回显 old/new 内容 ──────────────────────────────────

func TestPolishCandidateTools_NoEcho(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})

	oldS := "她站在窗前，看着窗外的梧桐树"
	newS := "她倚窗而立，望着窗外的梧桐树"
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", oldS, newS),
	}))
	out := string(raw)
	if strings.Contains(out, oldS) || strings.Contains(out, newS) {
		t.Fatalf("batch result echoes edit content: %s", out)
	}

	// 拒绝路径同样不回显。
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 2, []map[string]any{
		polishBatchEdit("p-001", "不存在的文本", "替换文本"),
	}))
	out = string(raw)
	if strings.Contains(out, "不存在的文本") || strings.Contains(out, "替换文本") {
		t.Fatalf("rejected result echoes edit content: %s", out)
	}
}

// ── 15. normalized 定位（exact 缺失 → 白名单归一化唯一命中） ─────────────

func TestPolishCandidateTools_NormalizedMatch(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{polishPlanIssue("p-001", "edit")})

	// 半角逗号 → 归一化后唯一命中 baseline 的全角逗号。
	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-001", "她站在窗前,看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	r := parseResult(t, raw)
	if r.Accepted != 1 || len(r.Errors) != 0 {
		t.Fatalf("result = %+v, want accepted=1", r)
	}
	accd := acc.Accepted()
	if len(accd) != 1 || accd[0].Mode != PolishEditMatchNormalized || accd[0].Start != 0 || accd[0].End != 42 {
		t.Fatalf("accepted[0] = %+v, want normalized [0,42)", accd[0])
	}
}

// ── 16. plan digest 确定性 ─────────────────────────────────────────────

func TestPolishCandidateTools_PlanDigestDeterministic(t *testing.T) {
	acc1 := newPolishTestAcc(t)
	acc2 := newPolishTestAcc(t)
	issues := []map[string]any{polishPlanIssue("p-001", "edit")}
	submitPlan(t, acc1, issues)
	submitPlan(t, acc2, issues)
	if acc1.Plan().Digest != acc2.Plan().Digest {
		t.Fatalf("identical plans must have identical digests: %q vs %q",
			acc1.Plan().Digest, acc2.Plan().Digest)
	}
	if len(acc1.Plan().Digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", acc1.Plan().Digest)
	}
}

// ── 17. batch 中 issue 不存在 / action 非 edit ─────────────────────────

func TestPolishCandidateTools_IssueUnknownNotEditable(t *testing.T) {
	acc := newPolishTestAcc(t)
	batchTool := NewSubmitEditBatchTool(acc)
	submitPlan(t, acc, []map[string]any{
		polishPlanIssue("p-001", "edit"),
		polishPlanIssue("p-002", "defer_to_writer"),
	})

	raw, _ := batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-999", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrIssueUnknown)

	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-002", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrIssueNotEditable)

	// 格式非法 → issue_id_invalid（先于查找）。
	raw, _ = batchTool.Execute(t.Context(), polishBatchArgs(t, testOpID, testBaselineDigest, 1, []map[string]any{
		polishBatchEdit("p-01", "她站在窗前，看着窗外的梧桐树", "她倚窗而立，望着窗外的梧桐树"),
	}))
	wantErr(t, parseResult(t, raw), PolishErrIssueIDInvalid)
}

// ── 18. 工具接口元数据 ─────────────────────────────────────────────────

func TestPolishCandidateTools_Metadata(t *testing.T) {
	acc := newPolishTestAcc(t)
	planTool := NewSubmitPolishPlanTool(acc)
	batchTool := NewSubmitEditBatchTool(acc)
	finishTool := NewFinishPolishTool(acc)

	if planTool.Name() != "submit_polish_plan" || batchTool.Name() != "submit_edit_batch" || finishTool.Name() != "finish_polish" {
		t.Fatal("tool names mismatch")
	}
	for _, tool := range []interface {
		ReadOnly(json.RawMessage) bool
		ConcurrencySafe(json.RawMessage) bool
		Schema() map[string]any
	}{planTool, batchTool, finishTool} {
		if tool.ReadOnly(nil) {
			t.Fatal("candidate tools must not be ReadOnly")
		}
		if tool.ConcurrencySafe(nil) {
			t.Fatal("candidate tools must not be ConcurrencySafe")
		}
		if tool.Schema() == nil {
			t.Fatal("schema must be non-nil")
		}
	}
}
