package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStyleAnchorsTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

func writeStyleAnchorsFile(t *testing.T, s *Store, data string) {
	t.Helper()
	path := filepath.Join(s.Dir(), "meta", "style_anchors.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestStyleAnchorsStore_NotExists(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusNotExist {
		t.Fatalf("expected StatusNotExist, got %v", res.Status)
	}
	if res.Anchors != nil {
		t.Fatal("expected nil anchors")
	}
	if len(res.Warnings) > 0 {
		t.Fatalf("expected no warnings, got: %v", res.Warnings)
	}
}

func TestStyleAnchorsStore_Valid(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "include_auto": true,
		"anchors": [
			{"id": "a1", "excerpt": "Excerpt one.",
			 "applies_to": {"chapter_ranges": [[1,5]]},
			 "provenance": {"source_chapter": 3, "source_digest": "sha256:abc"}},
			{"id": "a2", "excerpt": "Excerpt two."}
		]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusValid {
		t.Fatalf("expected StatusValid, got %v", res.Status)
	}
	if res.Anchors == nil || len(res.Anchors.Anchors) != 2 {
		t.Fatal("expected 2 anchors")
	}
	if res.Anchors.Anchors[0].AppliesTo == nil || len(res.Anchors.Anchors[0].AppliesTo.ChapterRanges) != 1 {
		t.Fatal("expected applies_to with 1 range")
	}
	if res.Anchors.Anchors[0].Provenance == nil || res.Anchors.Anchors[0].Provenance.SourceChapter != 3 {
		t.Fatal("expected provenance")
	}
}

func TestStyleAnchorsStore_LegacyFormatIDPriority(t *testing.T) {
	// id 应优先于 label
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "purpose": "guide", "usage": "writer",
		"anchors": [
			{"id": "my-id", "label": "my-label", "text": "Text."},
			{"id": "", "label": "fallback-label", "text": "Fallback."},
			{"label": "label-only", "text": "Label only."}
		]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusLegacyFormat {
		t.Fatalf("expected StatusLegacyFormat, got %v", res.Status)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected migration warning")
	}
	if len(res.Anchors.Anchors) != 3 {
		t.Fatalf("expected 3 anchors, got %d", len(res.Anchors.Anchors))
	}
	// id 优先 → "my-id"
	if res.Anchors.Anchors[0].ID != "my-id" {
		t.Fatalf("expected id 'my-id', got %q", res.Anchors.Anchors[0].ID)
	}
	// id 为空 → 使用 label
	if res.Anchors.Anchors[1].ID != "fallback-label" {
		t.Fatalf("expected fallback to label, got %q", res.Anchors.Anchors[1].ID)
	}
	if res.Anchors.Anchors[2].ID != "label-only" {
		t.Fatalf("expected label-only, got %q", res.Anchors.Anchors[2].ID)
	}
	// text → excerpt
	if res.Anchors.Anchors[0].Excerpt != "Text." {
		t.Fatalf("expected excerpt 'Text.', got %q", res.Anchors.Anchors[0].Excerpt)
	}
}

func TestStyleAnchorsStore_LegacyUnknownFieldWarn(t *testing.T) {
	// 旧格式中未知顶层字段应 warning 但继续加载
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "purpose": "g", "usage": "u",
		"anchors": [{"id":"a","label":"l","text":"T."}],
		"unknown_top": true
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusLegacyFormat {
		t.Fatalf("expected StatusLegacyFormat despite unknown top field in legacy, got %v", res.Status)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected at least migration warning")
	}
	hasUnknown := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "未知") {
			hasUnknown = true
			break
		}
	}
	if !hasUnknown {
		t.Fatal("expected unknown field warning in legacy format")
	}
}

func TestStyleAnchorsStore_EmptyValid(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{"version": 1, "anchors": []}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusEmptyValid {
		t.Fatalf("expected StatusEmptyValid, got %v", res.Status)
	}
	if res.Anchors == nil || len(res.Anchors.Anchors) != 0 {
		t.Fatal("expected empty anchors")
	}
}

func TestStyleAnchorsStore_CorruptedInvalidJSON(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{invalid`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_CorruptedExcerptTooLong(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	data, _ := json.Marshal(map[string]any{
		"version": 1,
		"anchors": []map[string]any{
			{"id": "a1", "excerpt": strings.Repeat("好", 1001)},
		},
	})
	writeStyleAnchorsFile(t, s, string(data))
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_CorruptedDuplicateIDs(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1,
		"anchors": [
			{"id": "dup", "excerpt": "First."},
			{"id": "dup", "excerpt": "Second."}
		]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for duplicates, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_UnknownTopFieldCorrupted(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "anchors": [{"id":"a","excerpt":"Ex."}],
		"bogus": 1
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for unknown top field, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_UnknownAnchorFieldCorrupted(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "anchors": [{"id":"a","excerpt":"Ex.","features":["bad"]}]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for unknown anchor field, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_UnknownNestedFieldCorrupted(t *testing.T) {
	// applies_to 未知子字段 → corrupted
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "anchors": [{"id":"a","excerpt":"Ex.","applies_to":{"chapter_ranges":[[1,5]],"foo":"bar"}}]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for unknown applies_to field, got %v", res.Status)
	}
	// provenance 未知子字段 → corrupted
	s2 := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s2, `{
		"version": 1, "anchors": [{"id":"a","excerpt":"Ex.","provenance":{"source_chapter":1,"bogus":true}}]
	}`)
	res = s2.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for unknown provenance field, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_ChapterRangesStrict(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"three elements", `[[1,2,3]]`},
		{"one element", `[[1]]`},
		{"float first", `[[1.5,2]]`},
		{"float second", `[[1,2.5]]`},
		{"string first", `[["1",2]]`},
		{"string second", `[[1,"2"]]`},
		{"null element", `[[1,null]]`},
		{"nested array", `[[[1],2]]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStyleAnchorsTestStore(t)
			writeStyleAnchorsFile(t, s, `{
				"version": 1,
				"anchors": [{"id":"a","excerpt":"Ex.","applies_to":{"chapter_ranges":`+tc.json+`}}]
			}`)
			res := s.StyleAnchors.LoadManual()
			if res.Status != StatusCorrupted {
				t.Fatalf("expected StatusCorrupted for %s, got %v", tc.name, res.Status)
			}
		})
	}
}

func TestStyleAnchorsStore_ProvenanceLimits(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	// source_chapter < 0 → corrupt
	writeStyleAnchorsFile(t, s, `{
		"version": 1, "anchors": [{"id":"a","excerpt":"Ex.","provenance":{"source_chapter": -1}}]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for negative source_chapter, got %v", res.Status)
	}

	// source_digest > 64 chars → corrupt
	s2 := newStyleAnchorsTestStore(t)
	digest := strings.Repeat("x", 65)
	writeStyleAnchorsFile(t, s2, `{
		"version": 1, "anchors": [{"id":"a","excerpt":"Ex.","provenance":{"source_digest":"`+digest+`"}}]
	}`)
	res = s2.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for long digest, got %v", res.Status)
	}
}

func TestStyleAnchorsStore_ChapterRangesLimit(t *testing.T) {
	s := newStyleAnchorsTestStore(t)
	writeStyleAnchorsFile(t, s, `{
		"version": 1,
		"anchors": [{
			"id": "a", "excerpt": "Ex.",
			"applies_to": {"chapter_ranges": [[1,2],[3,4],[5,6],[7,8],[9,10]]}
		}]
	}`)
	res := s.StyleAnchors.LoadManual()
	if res.Status != StatusCorrupted {
		t.Fatalf("expected StatusCorrupted for 5 ranges, got %v", res.Status)
	}
}
