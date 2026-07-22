package tools

import (
	"strings"
	"testing"
)

// ── ParseOpenThreadMarkers ──

func TestParseOpenThreadMarkers_NoMarker(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索遗迹")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索遗迹" {
		t.Fatalf("expected '探索遗迹', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 0 {
		t.Fatalf("expected 0 room IDs, got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_SingleMarker(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索遗迹[room:ancient_temple]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索遗迹" {
		t.Fatalf("expected '探索遗迹', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 1 || p.RoomIDs[0] != "ancient_temple" {
		t.Fatalf("expected [ancient_temple], got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_MultipleMarkers(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索遗迹[room:temple][room:treasure_room]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索遗迹" {
		t.Fatalf("expected '探索遗迹', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 2 || p.RoomIDs[0] != "temple" || p.RoomIDs[1] != "treasure_room" {
		t.Fatalf("expected [temple treasure_room], got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_MarkerOnly(t *testing.T) {
	p, err := ParseOpenThreadMarkers("[room:only_marker]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "" {
		t.Fatalf("expected empty summary, got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 1 || p.RoomIDs[0] != "only_marker" {
		t.Fatalf("expected [only_marker], got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_EmptyString(t *testing.T) {
	p, err := ParseOpenThreadMarkers("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "" {
		t.Fatalf("expected empty summary, got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 0 {
		t.Fatalf("expected 0 room IDs, got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_RejectsMiddleMarker(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索 [room:bad] 遗迹")
	if err == nil {
		t.Fatal("expected error for middle marker")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set on error")
	}
	if p.NaturalSummary != "探索 [room:bad] 遗迹" {
		t.Fatalf("expected NaturalSummary to preserve original text, got %q", p.NaturalSummary)
	}
}

func TestParseOpenThreadMarkers_RejectsEmptyID(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索 [room:]")
	if err == nil {
		t.Fatal("expected error for empty room id")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set on error")
	}
	if !strings.Contains(p.ParseError, "空 room id") {
		t.Fatalf("expected ParseError mentioning empty id, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_RejectsWhitespaceID(t *testing.T) {
	_, err := ParseOpenThreadMarkers("探索 [room:  ]")
	if err == nil {
		t.Fatal("expected error for whitespace room id")
	}
}

func TestParseOpenThreadMarkers_RejectsDuplicateID(t *testing.T) {
	_, err := ParseOpenThreadMarkers("探索[room:same][room:same]")
	if err == nil {
		t.Fatal("expected error for duplicate room id")
	}
}

func TestParseOpenThreadMarkers_CaseSensitive(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索[room:Room_A]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.RoomIDs) != 1 || p.RoomIDs[0] != "Room_A" {
		t.Fatalf("expected [Room_A], got %v", p.RoomIDs)
	}
	p2, err2 := ParseOpenThreadMarkers("探索[room:room_a]")
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if len(p2.RoomIDs) != 1 || p2.RoomIDs[0] != "room_a" {
		t.Fatalf("expected [room_a], got %v", p2.RoomIDs)
	}
}

func TestParseOpenThreadMarkers_MarkersWithTextAfter(t *testing.T) {
	// marker 不在末尾
	p, err := ParseOpenThreadMarkers("探索 [room:mid] 探索")
	if err == nil {
		t.Fatal("expected error for marker not at end")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
}

func TestParseOpenThreadMarkers_TrailingWhitespace(t *testing.T) {
	// 末尾空白现被严格拒绝（marker 必须精确位于末尾）
	p, err := ParseOpenThreadMarkers("探索 [room:temple]  ")
	if err == nil {
		t.Fatal("expected error for trailing whitespace after marker")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
	if p.NaturalSummary != "探索 [room:temple]  " {
		t.Fatalf("expected NaturalSummary to preserve original, got %q", p.NaturalSummary)
	}
}

func TestParseOpenThreadMarkers_TrailingWhitespaceOnly(t *testing.T) {
	// 纯末尾空白无 marker 应返回原文，不 trim
	p, err := ParseOpenThreadMarkers("探索  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索  " {
		t.Fatalf("expected NaturalSummary to preserve trailing whitespace, got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 0 {
		t.Fatalf("expected 0 room IDs, got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_NoSpaceBeforeMarker(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索[room:no_space]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索" {
		t.Fatalf("expected '探索', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 1 || p.RoomIDs[0] != "no_space" {
		t.Fatalf("expected [no_space], got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_SpaceBeforeMarkerPreservesSummaryWhitespace(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索 [room:ancient_temple]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索 " {
		t.Fatalf("expected '探索 ' (trailing space preserved), got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 1 || p.RoomIDs[0] != "ancient_temple" {
		t.Fatalf("expected [ancient_temple], got %v", p.RoomIDs)
	}
}

func TestParseOpenThreadMarkers_AdjacentMarkers(t *testing.T) {
	// marker 间不能有空格，保留原文 NaturalSummary（"探索 " 含尾部空格来自 "探索 [room:a]"）
	p, err := ParseOpenThreadMarkers("探索[room:a][room:b]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索" {
		t.Fatalf("expected '探索', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 2 || p.RoomIDs[0] != "a" || p.RoomIDs[1] != "b" {
		t.Fatalf("expected [a b], got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_AdjacentMarkersPreservesTrailingSummaryWhitespace(t *testing.T) {
	// "探索 " 在 marker 之前，NaturalSummary 保留原文（含尾部空格）
	p, err := ParseOpenThreadMarkers("探索 [room:a][room:b]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.NaturalSummary != "探索 " {
		t.Fatalf("expected '探索 ' (trailing space preserved), got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 2 || p.RoomIDs[0] != "a" || p.RoomIDs[1] != "b" {
		t.Fatalf("expected [a b], got %v", p.RoomIDs)
	}
}

// ── New stricter validation tests ──

func TestParseOpenThreadMarkers_RejectsUnclosedBracket(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索 [room:missing_bracket")
	if err == nil {
		t.Fatal("expected error for unclosed marker bracket")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
	if !strings.Contains(p.ParseError, "未闭合") && !strings.Contains(p.ParseError, "未闭合") {
		t.Fatalf("ParseError should mention unclosed, got %q", p.ParseError)
	}
	// NaturalSummary 必须保留原始文本
	if p.NaturalSummary != "探索 [room:missing_bracket" {
		t.Fatalf("expected NaturalSummary to preserve original, got %q", p.NaturalSummary)
	}
}

func TestParseOpenThreadMarkers_RejectsUnicodeWhitespaceInID(t *testing.T) {
	// 中文全角空格（U+3000）也是 Unicode 空白
	p, err := ParseOpenThreadMarkers("探索 [room:bad\u3000id]")
	if err == nil {
		t.Fatal("expected error for Unicode whitespace in ID")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
	if !strings.Contains(p.ParseError, "空白") {
		t.Fatalf("ParseError should mention whitespace, got %q", p.ParseError)
	}
	if p.NaturalSummary != "探索 [room:bad\u3000id]" {
		t.Fatalf("expected NaturalSummary to preserve original, got %q", p.NaturalSummary)
	}
}

func TestParseOpenThreadMarkers_RejectsTabInID(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索 [room:tab\tid]")
	if err == nil {
		t.Fatal("expected error for tab in room ID")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
	if p.NaturalSummary != "探索 [room:tab\tid]" {
		t.Fatalf("expected NaturalSummary to preserve original, got %q", p.NaturalSummary)
	}
}

func TestParseOpenThreadMarkers_RejectsControlCharInID(t *testing.T) {
	// \x00 空字符
	p, err := ParseOpenThreadMarkers("探索 [room:bad\x00id]")
	if err == nil {
		t.Fatal("expected error for control character in ID")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
	if p.NaturalSummary != "探索 [room:bad\x00id]" {
		t.Fatalf("expected NaturalSummary to preserve original, got %q", p.NaturalSummary)
	}
}

func TestParseOpenThreadMarkers_RejectsNewlineInID(t *testing.T) {
	p, err := ParseOpenThreadMarkers("探索 [room:bad\nid]")
	if err == nil {
		t.Fatal("expected error for newline in ID")
	}
	if p.ParseError == "" {
		t.Fatal("expected ParseError to be set")
	}
}

func TestParseOpenThreadMarkers_IDNotTrimmed(t *testing.T) {
	// ID 本身不 trim/不规范化，前导/后缀空白视为畸形
	// 但 trailing whitespace before ] — 实际上 [room:foo ] → id="foo "
	p, err := ParseOpenThreadMarkers("探索 [room:foo ]")
	if err == nil {
		t.Fatalf("expected error for trailing space in ID, got parsed: %+v", p)
	}
	if err != nil {
		if !strings.Contains(err.Error(), "空白") {
			t.Fatalf("expected error about whitespace, got %v", err)
		}
	}
}

func TestParseOpenThreadMarkers_ParseErrorOnSuccessIsEmpty(t *testing.T) {
	p, err := ParseOpenThreadMarkers("有效线程 [room:valid_id]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ParseError != "" {
		t.Fatalf("expected ParseError empty, got %q", p.ParseError)
	}
	if len(p.RoomIDs) != 1 || p.RoomIDs[0] != "valid_id" {
		t.Fatalf("unexpected room IDs: %v", p.RoomIDs)
	}
}

// TestParseOpenThreadMarkers_SuspectPatternNotAccepted 确保任何 [room:...] 模式
// 如果不满足严格的末尾闭合格式，都会被拒绝而不是静默当作普通文本。
func TestParseOpenThreadMarkers_SpaceSeparatedMultiMarkers_ParseCorrectly(t *testing.T) {
	p, err := ParseOpenThreadMarkers("线索 [room:a] [room:b]")
	if err != nil {
		t.Fatalf("unexpected error for space-separated markers: %v", err)
	}
	if p.NaturalSummary != "线索 " {
		t.Fatalf("expected '线索 ', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 2 || p.RoomIDs[0] != "a" || p.RoomIDs[1] != "b" {
		t.Fatalf("expected [a b], got %v", p.RoomIDs)
	}
	if p.ParseError != "" {
		t.Fatalf("expected empty ParseError, got %q", p.ParseError)
	}
}

func TestParseOpenThreadMarkers_SpaceSeparatedMultiMarkers_MultipleSpaces(t *testing.T) {
	p, err := ParseOpenThreadMarkers("线索[room:a]  [room:b]")
	if err != nil {
		t.Fatalf("unexpected error for multi-space between markers: %v", err)
	}
	if p.NaturalSummary != "线索" {
		t.Fatalf("expected '线索', got %q", p.NaturalSummary)
	}
	if len(p.RoomIDs) != 2 || p.RoomIDs[0] != "a" || p.RoomIDs[1] != "b" {
		t.Fatalf("expected [a b], got %v", p.RoomIDs)
	}
}

func TestParseOpenThreadMarkers_SpaceSeparatedMultiMarkers_RejectsTrailingWhitespaceAfterLast(t *testing.T) {
	_, err := ParseOpenThreadMarkers("线索 [room:a] [room:b]  ")
	if err == nil {
		t.Fatal("expected error for trailing whitespace after last marker")
	}
}

func TestParseOpenThreadMarkers_SpaceSeparatedMultiMarkers_RejectsTextAfterLast(t *testing.T) {
	_, err := ParseOpenThreadMarkers("线索 [room:a] [room:b] 后")
	if err == nil {
		t.Fatal("expected error for text after last marker")
	}
}

func TestParseOpenThreadMarkers_SpaceSeparatedMultiMarkers_RejectsMiddleMarkerByText(t *testing.T) {
	_, err := ParseOpenThreadMarkers("线索 [room:a] 中间 [room:b]")
	if err == nil {
		t.Fatal("expected error for middle marker")
	}
}

func TestParseOpenThreadMarkers_SuspectPatternNotAccepted(t *testing.T) {
	cases := []string{
		"[room:no_summary] 文本在后面",
		"文本 [room:middle] 在中间",
		"[room:unclosed",
		"前 [room:middle] 后",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			p, err := ParseOpenThreadMarkers(tc)
			if err == nil {
				// 如果无 error，则必须确保没有 RoomIDs（拒绝静默解析）
				if len(p.RoomIDs) > 0 {
					t.Fatalf("suspected marker pattern parsed RoomIDs=%v, expected rejection", p.RoomIDs)
				}
			}
			// 有 error 是正常的
		})
	}
}

// ── ExtractPlanningRefs ──

func TestExtractPlanningRefs(t *testing.T) {
	refs, parsed := ExtractPlanningRefs([]string{
		"探索遗迹[room:ancient_temple]",
		"寻找神器[room:artifact_hall]",
		"普通线程（无 marker）",
	})
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].Kind != "room" || refs[0].ID != "ancient_temple" {
		t.Fatalf("ref[0] unexpected: %+v", refs[0])
	}
	if refs[1].Kind != "room" || refs[1].ID != "artifact_hall" {
		t.Fatalf("ref[1] unexpected: %+v", refs[1])
	}
	if len(parsed) != 3 {
		t.Fatalf("expected 3 parsed entries, got %d", len(parsed))
	}
	if parsed[0].NaturalSummary != "探索遗迹" {
		t.Fatalf("parsed[0] summary: %q", parsed[0].NaturalSummary)
	}
	if parsed[1].NaturalSummary != "寻找神器" {
		t.Fatalf("parsed[1] summary: %q", parsed[1].NaturalSummary)
	}
	if parsed[2].NaturalSummary != "普通线程（无 marker）" {
		t.Fatalf("parsed[2] summary: %q", parsed[2].NaturalSummary)
	}
	// 成功解析的条目 ParseError 必须为空
	if parsed[0].ParseError != "" || parsed[1].ParseError != "" || parsed[2].ParseError != "" {
		t.Fatalf("successful entries should have empty ParseError: %+v", parsed)
	}
}

func TestExtractPlanningRefsWithSpaces(t *testing.T) {
	// 空格在摘要和 marker 之间是合法的——marker 本身必须精确在末尾
	refs, parsed := ExtractPlanningRefs([]string{
		"探索遗迹 [room:ancient_temple]",
	})
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if len(parsed) != 1 {
		t.Fatalf("expected 1 parsed, got %d", len(parsed))
	}
	// NaturalSummary 保留原文（含空格）
	if parsed[0].NaturalSummary != "探索遗迹 " {
		t.Fatalf("expected NaturalSummary '探索遗迹 ' (space preserved), got %q", parsed[0].NaturalSummary)
	}
	if len(parsed[0].RoomIDs) != 1 {
		t.Fatalf("expected 1 room ID, got %v", parsed[0].RoomIDs)
	}
}

func TestExtractPlanningRefs_Dedup(t *testing.T) {
	refs, _ := ExtractPlanningRefs([]string{
		"探索[room:same_room]",
		"寻宝[room:same_room]",
		"其他[room:other_room]",
	})
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs (deduped), got %d", len(refs))
	}
	if refs[0].ID != "same_room" || refs[1].ID != "other_room" {
		t.Fatalf("unexpected refs: %+v", refs)
	}
}

func TestExtractPlanningRefs_RejectsInvalidEntry(t *testing.T) {
	refs, parsed := ExtractPlanningRefs([]string{
		"有效线程[room:valid]",
		"中间有 [room:bad] 标记",
	})
	if len(refs) != 1 || refs[0].ID != "valid" {
		t.Fatalf("expected 1 valid ref, got %+v", refs)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 parsed entries, got %d", len(parsed))
	}
	if parsed[1].ParseError == "" {
		t.Fatal("invalid entry should have non-empty ParseError")
	}
	if parsed[1].NaturalSummary != "中间有 [room:bad] 标记" {
		t.Fatalf("invalid entry should keep original text: %q", parsed[1].NaturalSummary)
	}
}

func TestExtractPlanningRefs_EmptyInput(t *testing.T) {
	refs, parsed := ExtractPlanningRefs(nil)
	if len(refs) != 0 {
		t.Fatalf("expected 0 refs, got %d", len(refs))
	}
	if len(parsed) != 0 {
		t.Fatalf("expected 0 parsed, got %d", len(parsed))
	}

	refs2, parsed2 := ExtractPlanningRefs([]string{})
	if len(refs2) != 0 || len(parsed2) != 0 {
		t.Fatalf("expected empty results for empty input")
	}
}

// ── ExtractPlanningRefsWithErrors ──

func TestExtractPlanningRefsWithErrors(t *testing.T) {
	refs, parsed, errs := ExtractPlanningRefsWithErrors([]string{
		"有效线程 [room:valid]",
		"中间有 [room:bad] 标记",
		"另一有效 [room:other]",
	})
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if len(parsed) != 3 {
		t.Fatalf("expected 3 parsed, got %d", len(parsed))
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if errs[0].Index != 1 {
		t.Fatalf("expected error at index 1, got %d", errs[0].Index)
	}
	if errs[0].Text != "中间有 [room:bad] 标记" {
		t.Fatalf("unexpected error text: %q", errs[0].Text)
	}
	if parsed[0].ParseError != "" {
		t.Fatalf("valid entry should have empty ParseError, got %q", parsed[0].ParseError)
	}
	if parsed[1].ParseError == "" {
		t.Fatal("invalid entry should have non-empty ParseError")
	}
}

func TestExtractPlanningRefsWithErrors_AllValid(t *testing.T) {
	refs, parsed, errs := ExtractPlanningRefsWithErrors([]string{
		"探索 [room:a]",
		"寻宝 [room:b]",
	})
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors for all valid, got %d", len(errs))
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 parsed, got %d", len(parsed))
	}
	for i, p := range parsed {
		if p.ParseError != "" {
			t.Fatalf("parsed[%d] should have empty ParseError, got %q", i, p.ParseError)
		}
	}
}

func TestExtractPlanningRefsWithErrors_Empty(t *testing.T) {
	refs, parsed, errs := ExtractPlanningRefsWithErrors(nil)
	if len(errs) != 0 || len(refs) != 0 || len(parsed) != 0 {
		t.Fatalf("expected all empty for nil input")
	}

	refs2, parsed2, errs2 := ExtractPlanningRefsWithErrors([]string{})
	if len(errs2) != 0 || len(refs2) != 0 || len(parsed2) != 0 {
		t.Fatalf("expected all empty for empty input")
	}
}
