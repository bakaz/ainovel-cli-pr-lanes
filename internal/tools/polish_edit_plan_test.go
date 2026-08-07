package tools

import (
	"errors"
	"strings"
	"testing"
)

// ── ParsePolishEditPlan：解析与回退决策 ─────────────────────────────────

func TestPolishEditPlan_ParseValidSingleEdit(t *testing.T) {
	plan, fallback, err := ParsePolishEditPlan(`{"version":1,"edits":[{"old_string":"她站在窗前","new_string":"她倚窗而立"}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !fallback {
		t.Fatal("valid edit plan must be fallback=false (edit path)")
	}
	if plan.Version != 1 {
		t.Errorf("version = %d, want 1", plan.Version)
	}
	if len(plan.Edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(plan.Edits))
	}
	if plan.Edits[0].OldString != "她站在窗前" || plan.Edits[0].NewString != "她倚窗而立" {
		t.Errorf("edit = %+v", plan.Edits[0])
	}
}

func TestPolishEditPlan_ParseValidMultipleEdits(t *testing.T) {
	plan, _, err := ParsePolishEditPlan(`{"version":1,"edits":[{"old_string":"A","new_string":"a"},{"old_string":"B","new_string":"b"},{"old_string":"C","new_string":"c"}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plan.Edits) != 3 {
		t.Fatalf("edits = %d, want 3", len(plan.Edits))
	}
	// 顺序必须原样保留（应用时按 offset 排序，与输入顺序无关）。
	if plan.Edits[2].OldString != "C" || plan.Edits[0].NewString != "a" {
		t.Errorf("edits order corrupted: %+v", plan.Edits)
	}
}

func TestPolishEditPlan_ParseEmptyEdits(t *testing.T) {
	plan, fallback, err := ParsePolishEditPlan(`{"version":1,"edits":[]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !fallback || plan == nil {
		t.Fatal("empty edits must be a valid edit plan (no-op)")
	}
	if len(plan.Edits) != 0 {
		t.Errorf("edits = %d, want 0", len(plan.Edits))
	}
}

// 非 JSON（纯正文）→ 回退整章模式。
func TestPolishEditPlan_ParseNonJSONFallsBack(t *testing.T) {
	_, fallback, err := ParsePolishEditPlan("她倚窗而立。短句更有力，节奏明快。")
	if err == nil {
		t.Fatal("expected parse error for plain text")
	}
	if !fallback {
		t.Fatal("plain text must fall back to full-text mode")
	}
}

// 代码围栏包裹 → 回退整章模式（整章模式的围栏检查会拒绝）。
func TestPolishEditPlan_ParseFencedFallsBack(t *testing.T) {
	_, fallback, err := ParsePolishEditPlan("```json\n{\"version\":1,\"edits\":[]}\n```")
	if err == nil {
		t.Fatal("expected parse error for fenced output")
	}
	if !fallback {
		t.Fatal("fenced output must fall back to full-text mode")
	}
}

// 顶层未知字段 → 回退整章模式（规格 §2.3；整章模式的纯 JSON 检查会拒绝）。
func TestPolishEditPlan_ParseUnknownFieldFallsBack(t *testing.T) {
	for _, out := range []string{
		`{"version":1,"edits":[],"summary":"x"}`,
		`{"version":1,"edits":[{"old_string":"a","new_string":"b","extra":1}]}`,
		`{"result":"已完成精修","changed":true}`,
	} {
		_, fallback, err := ParsePolishEditPlan(out)
		if err == nil {
			t.Fatalf("expected parse error for %s", out)
		}
		if !fallback {
			t.Fatalf("%s must fall back to full-text mode", out)
		}
	}
}

// JSON 对象后的尾随内容（点评/第二个对象）→ 回退整章模式。
func TestPolishEditPlan_ParseTrailingContentFallsBack(t *testing.T) {
	for _, out := range []string{
		`{"version":1,"edits":[]} 已完成`,
		`{"version":1,"edits":[]}{"a":1}`,
	} {
		_, fallback, err := ParsePolishEditPlan(out)
		if err == nil {
			t.Fatalf("expected parse error for %s", out)
		}
		if !fallback {
			t.Fatalf("%s must fall back to full-text mode", out)
		}
	}
}

// JSON 数组（edit list 裸数组）→ 回退整章模式。
func TestPolishEditPlan_ParseArrayFallsBack(t *testing.T) {
	_, fallback, err := ParsePolishEditPlan(`[{"old_string":"a","new_string":"b"}]`)
	if err == nil {
		t.Fatal("expected parse error for bare array")
	}
	if !fallback {
		t.Fatal("bare array must fall back to full-text mode")
	}
}

// edits 不是数组（类型错误）→ 回退整章模式。
func TestPolishEditPlan_ParseEditsNotArrayFallsBack(t *testing.T) {
	_, fallback, err := ParsePolishEditPlan(`{"version":1,"edits":"x"}`)
	if err == nil {
		t.Fatal("expected parse error for non-array edits")
	}
	if !fallback {
		t.Fatal("non-array edits must fall back to full-text mode")
	}
}

// version 不受支持 → fail-closed 契约错误（不回退）。
func TestPolishEditPlan_ParseVersionMismatchContract(t *testing.T) {
	_, fallback, err := ParsePolishEditPlan(`{"version":2,"edits":[]}`)
	if err == nil {
		t.Fatal("expected contract error for version=2")
	}
	if fallback {
		t.Fatal("version mismatch must NOT fall back (fail-closed contract error)")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.Kind != PolishEditErrContract {
		t.Fatalf("err = %v, want PolishEditErrContract", err)
	}
}

// 缺少 version / 缺少 old_string → fail-closed 契约错误。
func TestPolishEditPlan_ParseMissingFieldContract(t *testing.T) {
	for _, out := range []string{
		`{"edits":[]}`,
		`{"version":1,"edits":[{"old_string":"a"}]}`,
		`{"version":1,"edits":[{"new_string":"b"}]}`,
	} {
		_, fallback, err := ParsePolishEditPlan(out)
		if err == nil {
			t.Fatalf("expected contract error for %s", out)
		}
		if fallback {
			t.Fatalf("%s must NOT fall back (fail-closed contract error)", out)
		}
	}
}

// ── ApplyPolishEditPlan：校验与应用 ────────────────────────────────────

// 多 edit 按 byte offset 倒序应用正确（互不重叠，乱序输入）。
// 输入含未修改填充段，保证 old 覆盖 ≤ 50%（协议上限）。
func TestPolishEditPlan_ApplyReverseOrder(t *testing.T) {
	input := "她站在窗前。他坐在桌边。猫趴在角落。此处为未修改的上下文。此处为保留原样的内容。"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "猫趴在角落。", NewString: "猫蜷在窗台。"},
		{OldString: "她站在窗前。", NewString: "她倚窗而立。"},
		{OldString: "他坐在桌边。", NewString: "他立在门前。"},
	}}
	got, err := ApplyPolishEditPlan(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := "她倚窗而立。他立在门前。猫蜷在窗台。此处为未修改的上下文。此处为保留原样的内容。"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// 相邻（首尾相接、不重叠）range 正确应用。
func TestPolishEditPlan_ApplyAdjacentRanges(t *testing.T) {
	input := "甲乙丙丁戊己庚辛"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "丙丁", NewString: "子丑"},
		{OldString: "甲乙", NewString: "寅卯"},
	}}
	got, err := ApplyPolishEditPlan(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "寅卯子丑戊己庚辛" {
		t.Errorf("got %q, want 寅卯子丑戊己庚辛", got)
	}
}

// 全部 edit 基于同一输入快照：后应用的 edit 不影响前一个 anchor 的定位
// （new_string 与其它 old_string 重叠是合法的）。
func TestPolishEditPlan_ApplyAllAnchorsOnInputSnapshot(t *testing.T) {
	input := "ABXY"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "B", NewString: "X"},
		{OldString: "A", NewString: "AB"},
	}}
	got, err := ApplyPolishEditPlan(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "ABXXY" {
		t.Errorf("got %q, want ABXXY", got)
	}
}

// 空 edits → no-op：原样返回输入。
func TestPolishEditPlan_ApplyEmptyEditsNoOp(t *testing.T) {
	input := "原样文本。"
	got, err := ApplyPolishEditPlan(input, &PolishEditPlan{Version: 1}, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != input {
		t.Errorf("no-op must return input unchanged, got %q", got)
	}
}

// ── 部分接受（ora-1 ④）：单条无效只丢弃该条，其余合法 edit 仍应用 ──────
//
// ApplyPolishEditPlanDetailed 返回完整结果（候选 + Applied/Dropped + 审计字段）。
// 仅当原始 edits 非空但全部被丢弃时才返回内容错误（无部分结果）。

// 第 3 条 anchor missing：前后合法 edit 仍应用（只丢弃第 3 条）。
func TestPolishEditPlan_PartialAnchorMissingMidList(t *testing.T) {
	input := "甲乙丙丁戊己庚辛壬癸"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲", NewString: "子"},
		{OldString: "乙", NewString: "丑"},
		{OldString: "不存在的片段", NewString: "x"},
		{OldString: "丙", NewString: "寅"},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("mid-list invalid edit must not fail the whole plan, got: %v", err)
	}
	if len(res.Applied) != 3 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 3/1", len(res.Applied), len(res.Dropped))
	}
	if res.Dropped[0].Idx != 2 || res.Dropped[0].DropReason != PolishEditDropAnchorMissing {
		t.Errorf("dropped = %+v, want edit#2 anchor_missing", res.Dropped[0])
	}
	if !res.Partial {
		t.Fatal("mid-list drop must mark result partial")
	}
	if res.Candidate != "子丑寅丁戊己庚辛壬癸" {
		t.Errorf("candidate = %q, want 子丑寅丁戊己庚辛壬癸", res.Candidate)
	}
	if res.ProposedEditCount != 4 || res.DroppedEditCount() != 1 {
		t.Errorf("audit proposed=%d dropped=%d, want 4/1", res.ProposedEditCount, res.DroppedEditCount())
	}
}

// 高/低优先级 edit 重叠 → 只保留高优先级（数组顺序 = 优先级，低优先级丢弃）。
func TestPolishEditPlan_PartialOverlapKeepsHighPriority(t *testing.T) {
	input := "她站在窗前，望着远处。"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "她站在窗前", NewString: "她倚窗而立"},
		{OldString: "在窗前，望着", NewString: "x"},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("partial acceptance must not fail the whole plan: %v", err)
	}
	if len(res.Applied) != 1 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 1/1", len(res.Applied), len(res.Dropped))
	}
	if !res.Partial {
		t.Fatal("overlap drop must mark result partial")
	}
	if res.Dropped[0].Idx != 1 || res.Dropped[0].DropReason != PolishEditDropOverlapLower {
		t.Errorf("dropped = %+v, want edit#1 overlap_lower_priority", res.Dropped[0])
	}
	if res.Candidate != "她倚窗而立，望着远处。" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}

// 第 5 条覆盖超限 → 丢弃该条，继续尝试后续较短 edit（第 6 条仍可接受）。
func TestPolishEditPlan_PartialCoverageDropsFifthKeepsSixth(t *testing.T) {
	input := "甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未申酉" // 20 runes，50% 上限 = 10
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲", NewString: "壹"},
		{OldString: "乙", NewString: "贰"},
		{OldString: "丙", NewString: "叁"},
		{OldString: "丁", NewString: "肆"},
		{OldString: "戊己庚辛壬癸子丑寅卯辰巳午", NewString: "X"}, // 13 runes → 4+13=17 > 10
		{OldString: "未", NewString: "伍"},             // 1 rune → 4+1=5 ≤ 10
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("coverage-exceeding edit must be dropped individually, got: %v", err)
	}
	if len(res.Applied) != 5 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 5/1", len(res.Applied), len(res.Dropped))
	}
	if res.Dropped[0].Idx != 4 || res.Dropped[0].DropReason != PolishEditDropCoverageLimit {
		t.Errorf("dropped = %+v, want edit#4 coverage_limit", res.Dropped[0])
	}
	if res.Candidate != "壹贰叁肆戊己庚辛壬癸子丑寅卯辰巳午伍申酉" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}

// 删除性 edit 把输出推到 40% 保底以下 → 只丢弃该条（rewrite 场景，70% 覆盖上限下
// 40% 保底才可能被触发：覆盖 ≤ 70% 时输出可低至 30%）。
func TestPolishEditPlan_PartialDeletionKeeps40Percent(t *testing.T) {
	input := "甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未申酉" // 20 runes，40% = 8
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲乙丙", NewString: "甲"},        // 删除 2 → 18 ≥ 8
		{OldString: "丁戊己庚辛壬癸子丑寅卯", NewString: ""}, // 删除 11 → 7 < 8
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatioRewrite)
	if err != nil {
		t.Fatalf("below-40%% deletion must be dropped individually, got: %v", err)
	}
	if len(res.Applied) != 1 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 1/1", len(res.Applied), len(res.Dropped))
	}
	if res.Dropped[0].Idx != 1 || res.Dropped[0].DropReason != PolishEditDropOutputTooShort {
		t.Errorf("dropped = %+v, want edit#1 output_too_short", res.Dropped[0])
	}
	if res.Candidate != "甲丁戊己庚辛壬癸子丑寅卯辰巳午未申酉" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}

// 膨胀 edit 超输出上限 → 只丢弃该条，其余 edit 正常应用。
func TestPolishEditPlan_PartialOutputTooLongDropInflated(t *testing.T) {
	input := "甲乙丙丁戊己庚辛壬癸"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲", NewString: "甲改"},
		{OldString: "乙", NewString: strings.Repeat("长", maxPolishOutputRunes)},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("inflated edit must be dropped individually, got: %v", err)
	}
	if len(res.Applied) != 1 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 1/1", len(res.Applied), len(res.Dropped))
	}
	if res.Dropped[0].DropReason != PolishEditDropOutputTooLong {
		t.Errorf("dropped reason = %s, want output_too_long", res.Dropped[0].DropReason)
	}
	if res.Candidate != "甲改乙丙丁戊己庚辛壬癸" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}

// 超过 maxPolishEdits 条的有效 edit：按优先级保留前 maxPolishEdits 条，
// 其余丢弃（count_limit），不整批拒绝。
func TestPolishEditPlan_ApplyTooManyEdits(t *testing.T) {
	chars := strings.Split("abcdefghijklmnopqrstuvwxyz0123456", "") // 33 个唯一锚点
	input := strings.Join(chars, " ") + " 填充填充"                     // 70 runes，覆盖预算 35
	edits := make([]PolishEditItem, 0, len(chars))
	for _, c := range chars {
		edits = append(edits, PolishEditItem{OldString: c, NewString: ""})
	}
	res, err := ApplyPolishEditPlanDetailed(input, &PolishEditPlan{Version: 1, Edits: edits}, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("count-limit must be partial, got: %v", err)
	}
	if len(res.Applied) != maxPolishEdits {
		t.Errorf("applied = %d, want %d", len(res.Applied), maxPolishEdits)
	}
	if len(res.Dropped) != 1 || res.Dropped[0].DropReason != PolishEditDropCountLimit {
		t.Errorf("dropped = %+v, want single count_limit", res.Dropped)
	}
}

// ── 失败路径：全部被丢弃（原始非空且 0 应用）→ 聚合内容错误 ──────────────

func TestPolishEditPlan_ApplyAnchorMissing(t *testing.T) {
	_, err := ApplyPolishEditPlan("她站在窗前。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "不存在的片段", NewString: "x"},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for missing anchor")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.Kind != PolishEditErrContent {
		t.Fatalf("err = %v, want PolishEditErrContent", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("expected missing-anchor message, got: %v", err)
	}
}

func TestPolishEditPlan_ApplyAnchorMultiple(t *testing.T) {
	_, err := ApplyPolishEditPlan("重复的片段，重复的片段。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "重复的片段", NewString: "x"},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for non-unique anchor")
	}
	if !strings.Contains(err.Error(), "多次") {
		t.Errorf("expected multiplicity message, got: %v", err)
	}
}

func TestPolishEditPlan_ApplyEmptyOld(t *testing.T) {
	_, err := ApplyPolishEditPlan("甲乙", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "", NewString: "x"},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for empty old_string")
	}
}

func TestPolishEditPlan_ApplySameOldNew(t *testing.T) {
	_, err := ApplyPolishEditPlan("甲乙", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲", NewString: "甲"},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for identical old/new")
	}
}

func TestPolishEditPlan_ApplyOldTooLong(t *testing.T) {
	old := strings.Repeat("长", maxPolishEditOldRunes+1)
	_, err := ApplyPolishEditPlan(old, &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: old, NewString: "短"},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for overlong old_string")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("expected old-length message, got: %v", err)
	}
}

// 覆盖比例：所有 old ranges 总和 ≤ 输入 50%（6/10 = 60% → 拒绝）。
func TestPolishEditPlan_ApplyCoverageTooLarge(t *testing.T) {
	_, err := ApplyPolishEditPlan("一二三四五六七八九十", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "一二三四五六", NewString: ""},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for coverage > 50%")
	}
	if !strings.Contains(err.Error(), "50%") {
		t.Errorf("expected coverage message, got: %v", err)
	}
}

// 应用后超过 maxPolishOutputRunes 上限 → 拒绝。
func TestPolishEditPlan_ApplyOutputTooLong(t *testing.T) {
	_, err := ApplyPolishEditPlan("甲乙", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲", NewString: strings.Repeat("长", maxPolishOutputRunes+1)},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected error for overlong candidate")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("expected output-limit message, got: %v", err)
	}
}

// 全部 edit 无效（原始非空且 0 应用）→ 聚合内容错误（调用方据此写 degraded
// checkpoint，不再触发第二次模型纠错）。
func TestPolishEditPlan_ApplyAllRejectedAggregateError(t *testing.T) {
	input := "甲乙丙丁戊己庚辛壬癸"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "不存在的片段", NewString: "x"},
		{OldString: "甲", NewString: "甲"},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected aggregate error when all edits are dropped")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.Kind != PolishEditErrContent {
		t.Fatalf("err = %v, want PolishEditErrContent", err)
	}
	if res == nil || len(res.Applied) != 0 || len(res.Dropped) != 2 {
		t.Fatalf("result must carry full audit: applied=%d dropped=%d", len(res.Applied), len(res.Dropped))
	}
	if !strings.Contains(err.Error(), "均被丢弃") {
		t.Errorf("expected all-dropped message, got: %v", err)
	}
}

// ── 覆盖阈值按场景区分（P1-6） ─────────────────────────────────────────
//
// 普通 draft 阶段保持 50% 上限；stage=rewrite（重写/打磨队列）放宽到 70%；
// 超过 70% 仍拒绝（要求显式整章 rewrite 路径）。覆盖错误携带结构化数字
// （CoverageRunes/InputRunes/CoverageLimit），供审计分类使用。

// 普通 draft（50% 上限）：等于 50 接受、超过拒绝。
func TestPolishEditPlan_CoverageDraftBoundary(t *testing.T) {
	input := "一二三四五六七八九十" // 10 runes，每个字符唯一
	// 恰好 50%（5/10）→ 接受。
	got, err := ApplyPolishEditPlan(input, &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "一二三四五", NewString: "x"},
	}}, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("coverage == 50%% must be accepted, got: %v", err)
	}
	if got == input {
		t.Error("plan must be applied")
	}
	// 超过 50%（6/10）→ 拒绝，且错误携带结构化覆盖数字。
	_, err = ApplyPolishEditPlan(input, &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "一二三四五六", NewString: "x"},
	}}, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("coverage > 50%% must be rejected")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.CoverageLimit != maxPolishEditCoverageRatio {
		t.Fatalf("err = %v, want coverage error with limit 0.50", err)
	}
	if pe.CoverageRunes != 6 || pe.InputRunes != 10 {
		t.Errorf("coverage fields = %d/%d, want 6/10", pe.CoverageRunes, pe.InputRunes)
	}
	if !strings.Contains(err.Error(), "50%") {
		t.Errorf("coverage error message must mention 50%%, got: %v", err)
	}
}

// rewrite（70% 上限）：63% 接受、超过 70 拒绝；同一 63% 计划在普通 draft
// （50%）场景必须被拒绝——阈值按场景区分。new_string 保持与 old 等长，
// 避免候选低于 40% 输出下限干扰覆盖断言。
func TestPolishEditPlan_CoverageRewriteBoundary(t *testing.T) {
	input := strings.Repeat("一二三四五六七八九十", 10) // 100 runes
	plan63 := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: strings.Repeat("一二三四五六七八九十", 6) + "一二三", NewString: strings.Repeat("改", 63)}, // 63 runes = 63%
	}}
	// 63% ≤ 70% → rewrite 场景接受。
	if _, err := ApplyPolishEditPlan(input, plan63, maxPolishEditCoverageRatioRewrite); err != nil {
		t.Fatalf("63%% coverage must be accepted in rewrite (70%% limit), got: %v", err)
	}
	// 同一 63% 计划在普通 draft（50%）场景必须被拒绝。
	if _, err := ApplyPolishEditPlan(input, plan63, maxPolishEditCoverageRatio); err == nil {
		t.Fatal("63%% coverage must be rejected in draft mode (50%% limit)")
	}
	// 超过 70%（71/100）→ rewrite 场景仍拒绝。
	plan71 := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: strings.Repeat("一二三四五六七八九十", 7) + "一", NewString: strings.Repeat("改", 71)}, // 71 runes = 71%
	}}
	_, err := ApplyPolishEditPlan(input, plan71, maxPolishEditCoverageRatioRewrite)
	if err == nil {
		t.Fatal("coverage 71%% must be rejected even in rewrite mode (> 70%%)")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.CoverageLimit != maxPolishEditCoverageRatioRewrite {
		t.Fatalf("err = %v, want coverage error with limit 0.70", err)
	}
	if !strings.Contains(err.Error(), "70%") {
		t.Errorf("coverage error message must mention 70%%, got: %v", err)
	}
}

// 非法 limit 参数（≤0 或 >1）按默认 0.50 兜底（纵深防御）。
func TestPolishEditPlan_CoverageInvalidLimitDefaults(t *testing.T) {
	input := "一二三四五六七八九十"
	for _, limit := range []float64{0, -1, 1.5} {
		// 6/10 = 60%：默认 50% 下拒绝；若 limit 被错误当作"无上限"则会接受。
		if _, err := ApplyPolishEditPlan(input, &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
			{OldString: "一二三四五六", NewString: "x"},
		}}, limit); err == nil {
			t.Fatalf("invalid limit %v must fall back to 0.50 and reject 60%% coverage", limit)
		}
	}
}

// ── exact + normalized 两级匹配链（禁近似匹配） ─────────────────────────
//
// 1. exact unique → 直接用；2. exact 0 次 → normalized unique 才允许定位；
// 3. exact 多次 → 立即判歧义（不做 normalized 猜测）；4. 白名单归一化只做确定性
// 表示等价；5. 禁止 Levenshtein/编辑距离/相似度候选/NFKC/全角数字字母/大小写折叠；
// 6. normalized 回退要求 anchor ≥ 8 个非空白 rune；7. new_string 不做任何归一化。

// 精确缺失、归一化唯一 → normalized 定位成功应用（白名单表示等价），审计记录
// MatchMode=normalized 且 NormalizedMatchCount=1；new_string 原样应用（不归一化）。
func TestPolishEditPlan_NormalizedUniqueApply(t *testing.T) {
	input := "她说道：“你好，世界。”他说道：“再见。”这是一段保留原样的上下文，用于满足覆盖比例上限要求。"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		// old 用 ASCII 引号 + 半角标点表示（输入是智能引号 + 全角标点），仅表示等价。
		{OldString: `她说道:"你好,世界."`, NewString: `她说："你好呀。"`},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("normalized-unique must be applied, got: %v", err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("applied = %d, want 1", len(res.Applied))
	}
	if res.Applied[0].Mode != PolishEditMatchNormalized {
		t.Errorf("match mode = %s, want normalized", res.Applied[0].Mode)
	}
	if res.NormalizedMatchCount != 1 {
		t.Errorf("normalized_match_count = %d, want 1", res.NormalizedMatchCount)
	}
	// new_string 不做任何归一化：原样应用（保留 ASCII 引号写法）。
	want := `她说："你好呀。"他说道：“再见。”这是一段保留原样的上下文，用于满足覆盖比例上限要求。`
	if res.Candidate != want {
		t.Errorf("candidate = %q, want %q", res.Candidate, want)
	}
}

// normalized 多次出现（0 次精确）→ 只丢弃该 edit，其余合法 edit 仍应用。
func TestPolishEditPlan_NormalizedAmbiguousDrop(t *testing.T) {
	// 两行归一化后相同（智能引号 vs ASCII 引号各一行）→ normalized 2 次 → 丢弃。
	input := "他说：“你好。”\n他说:\"你好。\"\n再见"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: `他说：“你好。"`, NewString: "x"}, // 精确 0 次，归一化 2 次
		{OldString: "再见", NewString: "拜拜"},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("ambiguous normalized edit must be dropped individually, got: %v", err)
	}
	if len(res.Applied) != 1 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 1/1", len(res.Applied), len(res.Dropped))
	}
	if res.Dropped[0].Idx != 0 || res.Dropped[0].DropReason != PolishEditDropAnchorAmbiguous {
		t.Errorf("dropped = %+v, want edit#0 anchor_ambiguous", res.Dropped[0])
	}
	if res.Candidate != "他说：“你好。”\n他说:\"你好。\"\n拜拜" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}

// exact 出现多次 → 立即判歧义丢弃，不尝试 normalized 猜测（归一化次数 ≥ 精确次数，
// 猜测不可能挽救；丢弃记录的 MatchMode 保持 exact，证明从未走 normalized 路径）。
func TestPolishEditPlan_ExactAmbiguousNoNormalizedGuess(t *testing.T) {
	input := "他说：“你好。”\n他说：“你好。”"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "他说：“你好。”", NewString: "x"},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("expected aggregate error for exact-ambiguous anchor (all dropped)")
	}
	if len(res.Dropped) != 1 || res.Dropped[0].DropReason != PolishEditDropAnchorAmbiguous {
		t.Fatalf("dropped = %+v, want single anchor_ambiguous", res.Dropped)
	}
	if res.Dropped[0].Mode != PolishEditMatchExact {
		t.Errorf("mode = %s, want exact (normalized guessing must not be attempted)", res.Dropped[0].Mode)
	}
}

// 白名单之外的差异（编辑距离 1 的错别字）不得被"模糊"匹配：锚点视为缺失。
// 证明 normalized 不是 Levenshtein/相似度候选自动应用（禁近似匹配）。
func TestPolishEditPlan_NormalizedNoLevenshtein(t *testing.T) {
	input := "她站在窗前，望着远处。"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "她站再窗前", NewString: "x"}, // 在→再（错别字，编辑距离 1）
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("typo must NOT fuzzy-match: expected aggregate error")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("expected missing-anchor message, got: %v", err)
	}
	if res.Dropped[0].DropReason != PolishEditDropAnchorMissing {
		t.Errorf("dropped reason = %s, want anchor_missing", res.Dropped[0].DropReason)
	}
}

// 全角数字、字母不参与归一化：与半角数字/字母是不同 token，不得视为等价。
func TestPolishEditPlan_NormalizedNoFullwidthAlnum(t *testing.T) {
	// 全角字母 ＡＢＣ vs 半角 ABC。
	if _, err := ApplyPolishEditPlan("ＡＢＣ１２３号房。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "ABC123号房。", NewString: "x"},
	}}, maxPolishEditCoverageRatio); err == nil {
		t.Fatal("fullwidth letters/digits must NOT normalize to halfwidth")
	}
	// 全角数字 １ vs 半角 1。
	if _, err := ApplyPolishEditPlan("房间１号。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "房间1号。", NewString: "x"},
	}}, maxPolishEditCoverageRatio); err == nil {
		t.Fatal("fullwidth digit must NOT normalize to halfwidth digit")
	}
}

// 白名单归一化逐类验证：只做确定性表示等价（CRLF/CR→LF、Unicode 水平空格、
// 行尾空白、智能引号、dash 变体、全角 ASCII 标点）。输入追加保留段保证
// old 覆盖 ≤ 50%（满足覆盖上限）。
func TestPolishEditPlan_NormalizedWhitelist(t *testing.T) {
	const filler = "这是一段保留原样的上下文，用于满足覆盖比例上限要求。"
	cases := []struct {
		name, input, old string
	}{
		{"CRLF", "第一行句子内容很长\r\n第二行内容" + filler, "第一行句子内容很长\n第二行内容"},
		{"CR", "第一行句子内容很长\r第二行内容" + filler, "第一行句子内容很长\n第二行内容"},
		{"NBSP", "他说\u00A0话很慢。你听清楚了吗。" + filler, "他说 话很慢。你听清楚了吗。"},
		{"figSpace", "他说\u2009话很慢。你听清楚了吗。" + filler, "他说 话很慢。你听清楚了吗。"},
		{"smartQuotes", "他说道：“这样很好。”" + filler, "他说道：\"这样很好。\""},
		{"smartSingleQuotes", "他说道：‘这样很好。’" + filler, "他说道：'这样很好。'"},
		{"enDash", "一九九三–一九九四年间" + filler, "一九九三-一九九四年间"},
		{"emDash", "一九九三—一九九四年间" + filler, "一九九三-一九九四年间"},
		{"minus", "温度−5度，下降很快。" + filler, "温度-5度，下降很快。"},
		{"fullwidthPunct", "她问：“你好，世界！”" + filler, "她问：\"你好,世界!\""},
		{"trailingWS", "第一行句子内容很长  \n第二行内容" + filler, "第一行句子内容很长\n第二行内容"},
	}
	for _, c := range cases {
		res, err := ApplyPolishEditPlanDetailed(c.input,
			&PolishEditPlan{Version: 1, Edits: []PolishEditItem{{OldString: c.old, NewString: "X"}}},
			maxPolishEditCoverageRatio)
		if err != nil {
			t.Errorf("%s: apply: %v", c.name, err)
			continue
		}
		if len(res.Applied) != 1 || res.Applied[0].Mode != PolishEditMatchNormalized {
			t.Errorf("%s: not normalized-applied: %+v", c.name, res.Applied)
		}
	}
}

// 归一化回退要求 anchor 至少含 8 个非空白 rune：仅引号/短词差异不定位（弱锚点）。
func TestPolishEditPlan_NormalizedShortAnchorRejected(t *testing.T) {
	input := "他说“好”"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: `他说"好"`, NewString: "x"}, // 5 个非空白 rune < 8
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err == nil {
		t.Fatal("short anchor must NOT normalized-match: expected aggregate error")
	}
	if res.Dropped[0].DropReason != PolishEditDropAnchorMissing {
		t.Errorf("dropped reason = %s, want anchor_missing (weak anchor)", res.Dropped[0].DropReason)
	}
}

// 归一化后 old==new（仅行尾/引号表示差异）→ noop 丢弃，其余 edit 不受影响。
func TestPolishEditPlan_NormalizedNoopDrop(t *testing.T) {
	input := "第一行内容很长\r\n第二行内容"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "第一行内容很长\r\n第二行内容", NewString: "第一行内容很长\n第二行内容"},
		{OldString: "第一行", NewString: "起首"},
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("normalized no-op must be dropped individually, got: %v", err)
	}
	if len(res.Applied) != 1 || len(res.Dropped) != 1 {
		t.Fatalf("applied=%d dropped=%d, want 1/1", len(res.Applied), len(res.Dropped))
	}
	if res.Dropped[0].Idx != 0 || res.Dropped[0].DropReason != PolishEditDropNoop {
		t.Errorf("dropped = %+v, want edit#0 noop", res.Dropped[0])
	}
	if res.Candidate != "起首内容很长\r\n第二行内容" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}

// normalized 定位的 byte range 必须精确回映原始输入（CRLF→LF 归一化后长度变化，
// 替换必须覆盖原始 \r\n 而非只覆盖 \n）。
func TestPolishEditPlan_NormalizedCRLFRangeMapping(t *testing.T) {
	input := "第一行内容\r\n第二行内容很长啊\r\n第三行内容"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "第二行内容很长啊\n", NewString: "第二行改"}, // old 用 LF，输入是 CRLF
	}}
	res, err := ApplyPolishEditPlanDetailed(input, plan, maxPolishEditCoverageRatio)
	if err != nil {
		t.Fatalf("CRLF normalized-unique must be applied, got: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Mode != PolishEditMatchNormalized {
		t.Fatalf("applied = %+v, want single normalized edit", res.Applied)
	}
	wantStart := strings.Index(input, "第二行内容很长啊")
	if res.Applied[0].Start != wantStart {
		t.Errorf("start = %d, want %d (must map back to original CRLF bytes)", res.Applied[0].Start, wantStart)
	}
	if res.Candidate != "第一行内容\r\n第二行改第三行内容" {
		t.Errorf("candidate = %q", res.Candidate)
	}
}
