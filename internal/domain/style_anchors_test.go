package domain

import (
	"fmt"
	"strings"
	"testing"
)

func TestStyleAnchorsV1_Validate_Valid(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{
			{
				ID:      "a1",
				Excerpt: "夜色如墨，他在城头站了整夜。",
				AppliesTo: &StyleAnchorAppliesTo{
					ChapterRanges: [][2]int{{1, 5}, {10, 15}},
				},
				Provenance: &StyleAnchorProvenance{
					SourceChapter: 3,
					SourceDigest:  "sha256:abc123",
				},
			},
			{ID: "a2", Excerpt: "她笑了笑。"},
		},
		IncludeAuto: true,
	}
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_EmptyAnchors(t *testing.T) {
	s := StyleAnchorsV1{Version: 1}
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("empty anchors should be valid, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_WrongVersion(t *testing.T) {
	s := StyleAnchorsV1{Version: 2}
	errs := s.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "version") {
		t.Fatalf("expected version error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_TooManyAnchors(t *testing.T) {
	s := StyleAnchorsV1{Version: 1}
	for i := 0; i < 9; i++ {
		s.Anchors = append(s.Anchors, StyleAnchorItem{
			ID: fmt.Sprintf("a%d", i), Excerpt: "Test excerpt.",
		})
	}
	errs := s.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "最多 8 项") {
		t.Fatalf("expected max 8 items error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_DuplicateIDs(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{
			{ID: "dup", Excerpt: "First."},
			{ID: "dup", Excerpt: "Second."},
		},
	}
	errs := s.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "重复") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected duplicate id error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_EmptyAndWhitespaceID(t *testing.T) {
	for name, id := range map[string]string{"empty": "", "whitespace": "  ", "tab": "\t"} {
		t.Run(name, func(t *testing.T) {
			s := StyleAnchorsV1{
				Version: 1,
				Anchors: []StyleAnchorItem{{ID: id, Excerpt: "Ex."}},
			}
			if errs := s.Validate(); len(errs) == 0 {
				t.Fatal("expected empty id error")
			}
		})
	}
}

func TestStyleAnchorsV1_Validate_IDTooLong(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{{ID: strings.Repeat("x", 65), Excerpt: "Ex."}},
	}
	errs := s.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "64") {
		t.Fatalf("expected id too long error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_EmptyAndWhitespaceExcerpt(t *testing.T) {
	for name, ex := range map[string]string{"empty": "", "whitespace": "   ", "newline": "\n\n"} {
		t.Run(name, func(t *testing.T) {
			s := StyleAnchorsV1{
				Version: 1,
				Anchors: []StyleAnchorItem{{ID: "a1", Excerpt: ex}},
			}
			if errs := s.Validate(); len(errs) == 0 {
				t.Fatal("expected empty excerpt error")
			}
		})
	}
}

func TestStyleAnchorsV1_Validate_ExcerptTooLong(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{{ID: "a1", Excerpt: strings.Repeat("好", 1001)}},
	}
	errs := s.Validate()
	if len(errs) == 0 {
		t.Fatal("expected excerpt too long error")
	}
}

func TestStyleAnchorsV1_Validate_TotalExcerptTooLong(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{
			{ID: "a1", Excerpt: strings.Repeat("好", 4001)},
			{ID: "a2", Excerpt: strings.Repeat("好", 4001)},
		},
	}
	errs := s.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "总长度") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected total excerpt too long error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_TooManyChapterRanges(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{{
			ID: "a1", Excerpt: "Ex.",
			AppliesTo: &StyleAnchorAppliesTo{
				ChapterRanges: [][2]int{{1, 2}, {3, 4}, {5, 6}, {7, 8}, {9, 10}},
			},
		}},
	}
	errs := s.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "最多 4 个区间") {
		t.Fatalf("expected max 4 ranges error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_NegativeSourceChapter(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{{
			ID: "a1", Excerpt: "Ex.",
			Provenance: &StyleAnchorProvenance{SourceChapter: -1},
		}},
	}
	errs := s.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "负数") {
		t.Fatalf("expected negative source_chapter error, got: %v", errs)
	}
}

func TestStyleAnchorsV1_Validate_DigestTooLong(t *testing.T) {
	s := StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{{
			ID: "a1", Excerpt: "Ex.",
			Provenance: &StyleAnchorProvenance{SourceDigest: strings.Repeat("x", 65)},
		}},
	}
	errs := s.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "64") {
		t.Fatalf("expected digest too long error, got: %v", errs)
	}
}

func TestAnchorMatchesChapter_Global(t *testing.T) {
	a := StyleAnchorItem{ID: "g", Excerpt: "global"}
	if !a.AnchorMatchesChapter(0) || !a.AnchorMatchesChapter(100) {
		t.Fatal("global anchor should match any chapter")
	}
}

func TestAnchorMatchesChapter_EmptyAppliesTo(t *testing.T) {
	a := StyleAnchorItem{ID: "g", Excerpt: "g", AppliesTo: &StyleAnchorAppliesTo{}}
	if !a.AnchorMatchesChapter(5) {
		t.Fatal("empty AppliesTo should match any chapter")
	}
}

func TestAnchorMatchesChapter_InRange(t *testing.T) {
	a := StyleAnchorItem{ID: "r", Excerpt: "r",
		AppliesTo: &StyleAnchorAppliesTo{ChapterRanges: [][2]int{{3, 7}}},
	}
	if a.AnchorMatchesChapter(2) {
		t.Fatal("should not match before range")
	}
	if !a.AnchorMatchesChapter(3) || !a.AnchorMatchesChapter(5) || !a.AnchorMatchesChapter(7) {
		t.Fatal("should match inside range")
	}
	if a.AnchorMatchesChapter(8) {
		t.Fatal("should not match after range")
	}
}

func TestAnchorMatchesChapter_MultiRange(t *testing.T) {
	a := StyleAnchorItem{ID: "mr", Excerpt: "mr",
		AppliesTo: &StyleAnchorAppliesTo{ChapterRanges: [][2]int{{1, 2}, {10, 12}}},
	}
	if !a.AnchorMatchesChapter(1) || a.AnchorMatchesChapter(5) || !a.AnchorMatchesChapter(11) {
		t.Fatal("multi-range matching failed")
	}
}

func TestToInjectionView_FiltersAndStrips(t *testing.T) {
	s := &StyleAnchorsV1{
		Version: 1,
		Anchors: []StyleAnchorItem{
			{ID: "global", Excerpt: "Global.", AppliesTo: nil},
			{ID: "in", Excerpt: "In range.", AppliesTo: &StyleAnchorAppliesTo{ChapterRanges: [][2]int{{3, 7}}}},
			{ID: "out", Excerpt: "Out of range.", AppliesTo: &StyleAnchorAppliesTo{ChapterRanges: [][2]int{{10, 15}}}},
		},
	}
	// chapter 5: global + in should match, out should not
	view := s.ToInjectionView(5)
	if len(view) != 2 {
		t.Fatalf("expected 2 items for ch5, got %d", len(view))
	}
	if view[0].ID != "global" || view[1].ID != "in" {
		t.Fatalf("wrong items: %+v", view)
	}
	// Verify stripped: only id+excerpt
	if view[0].Excerpt == "" {
		t.Fatal("expected excerpt in injection view")
	}
}

func TestToInjectionView_Nil(t *testing.T) {
	var s *StyleAnchorsV1
	if v := s.ToInjectionView(1); v != nil {
		t.Fatal("expected nil for nil receiver")
	}
}
