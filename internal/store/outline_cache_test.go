package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestLoadOutlineCachesUntilSave(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	first := []domain.OutlineEntry{{Chapter: 1, Title: "一", CoreEvent: "开", Hook: "下"}}
	if err := s.Outline.SaveOutline(first); err != nil {
		t.Fatal(err)
	}
	got, err := s.Outline.LoadOutline()
	if err != nil || len(got) != 1 || got[0].Title != "一" {
		t.Fatalf("first load = %+v err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outline.json"), []byte(`[{"chapter":1,"title":"磁盘被改"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	cached, err := s.Outline.LoadOutline()
	if err != nil || cached[0].Title != "一" {
		t.Fatalf("cache should hide raw rewrite, got %+v err=%v", cached, err)
	}
	second := []domain.OutlineEntry{{Chapter: 1, Title: "二", CoreEvent: "开", Hook: "下"}}
	if err := s.Outline.SaveOutline(second); err != nil {
		t.Fatal(err)
	}
	after, err := s.Outline.LoadOutline()
	if err != nil || after[0].Title != "二" {
		t.Fatalf("save must invalidate cache, got %+v err=%v", after, err)
	}
}
