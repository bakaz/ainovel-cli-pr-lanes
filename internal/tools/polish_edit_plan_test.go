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
	got, err := ApplyPolishEditPlan(input, plan)
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
	got, err := ApplyPolishEditPlan(input, plan)
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
	got, err := ApplyPolishEditPlan(input, plan)
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
	got, err := ApplyPolishEditPlan(input, &PolishEditPlan{Version: 1})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != input {
		t.Errorf("no-op must return input unchanged, got %q", got)
	}
}

// ── 失败路径：均返回内容校验错误且无部分结果 ────────────────────────────

func TestPolishEditPlan_ApplyAnchorMissing(t *testing.T) {
	_, err := ApplyPolishEditPlan("她站在窗前。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "不存在的片段", NewString: "x"},
	}})
	if err == nil {
		t.Fatal("expected error for missing anchor")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.Kind != PolishEditErrContent || pe.Index != 0 {
		t.Fatalf("err = %v, want PolishEditErrContent index 0", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("expected missing-anchor message, got: %v", err)
	}
}

func TestPolishEditPlan_ApplyAnchorMultiple(t *testing.T) {
	_, err := ApplyPolishEditPlan("重复的片段，重复的片段。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "重复的片段", NewString: "x"},
	}})
	if err == nil {
		t.Fatal("expected error for non-unique anchor")
	}
	if !strings.Contains(err.Error(), "出现 2 次") {
		t.Errorf("expected multiplicity message, got: %v", err)
	}
}

func TestPolishEditPlan_ApplyOverlap(t *testing.T) {
	_, err := ApplyPolishEditPlan("她站在窗前。", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "她站在窗前", NewString: "a"},
		{OldString: "在窗前。", NewString: "b"},
	}})
	if err == nil {
		t.Fatal("expected error for overlapping ranges")
	}
	if !strings.Contains(err.Error(), "重叠") {
		t.Errorf("expected overlap message, got: %v", err)
	}
}

func TestPolishEditPlan_ApplyEmptyOld(t *testing.T) {
	_, err := ApplyPolishEditPlan("甲乙", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "", NewString: "x"},
	}})
	if err == nil {
		t.Fatal("expected error for empty old_string")
	}
}

func TestPolishEditPlan_ApplySameOldNew(t *testing.T) {
	_, err := ApplyPolishEditPlan("甲乙", &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "甲", NewString: "甲"},
	}})
	if err == nil {
		t.Fatal("expected error for identical old/new")
	}
}

func TestPolishEditPlan_ApplyTooManyEdits(t *testing.T) {
	edits := make([]PolishEditItem, maxPolishEdits+1)
	for i := range edits {
		edits[i] = PolishEditItem{OldString: "a", NewString: "b"}
	}
	_, err := ApplyPolishEditPlan("x", &PolishEditPlan{Version: 1, Edits: edits})
	if err == nil {
		t.Fatal("expected error for too many edits")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("expected count-limit message, got: %v", err)
	}
}

func TestPolishEditPlan_ApplyOldTooLong(t *testing.T) {
	old := strings.Repeat("长", maxPolishEditOldRunes+1)
	_, err := ApplyPolishEditPlan(old, &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: old, NewString: "短"},
	}})
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
	}})
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
	}})
	if err == nil {
		t.Fatal("expected error for overlong candidate")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("expected output-limit message, got: %v", err)
	}
}

// 第 N 条无效 → 整批失败（前 N-1 条合法也不产生部分结果）。
func TestPolishEditPlan_ApplyInvalidNthNoPartialResult(t *testing.T) {
	input := "她站在窗前。他坐在桌边。猫趴在角落。"
	plan := &PolishEditPlan{Version: 1, Edits: []PolishEditItem{
		{OldString: "她站在窗前。", NewString: "她倚窗而立。"},
		{OldString: "他坐在桌边。", NewString: "他立在门前。"},
		{OldString: "不存在的片段", NewString: "x"},
		{OldString: "猫趴在角落。", NewString: "猫蜷在窗台。"},
	}}
	_, err := ApplyPolishEditPlan(input, plan)
	if err == nil {
		t.Fatal("expected error for invalid edit #2 (0-based)")
	}
	var pe *PolishEditError
	if !errors.As(err, &pe) || pe.Index != 2 {
		t.Fatalf("err = %v, want error at index 2", err)
	}
}
