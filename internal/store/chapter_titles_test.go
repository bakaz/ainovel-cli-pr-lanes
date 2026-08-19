package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestChapterTitleStoreMissingFileIsEmptyFact(t *testing.T) {
	store := NewChapterTitleStore(newIO(t.TempDir()))

	if got, err := store.Load(1); err != nil {
		t.Fatalf("Load missing title: %v", err)
	} else if got != "" {
		t.Fatalf("Load missing title = %q, want empty", got)
	}
	if got, err := store.LoadAll(); err != nil {
		t.Fatalf("LoadAll missing titles: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("LoadAll missing titles = %#v, want empty map", got)
	}
}

func TestChapterTitleStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewChapterTitleStore(newIO(dir))
	want := map[int]string{1: "雨夜归人", 2: "破晓之后"}

	for chapter, title := range want {
		if err := store.Save(chapter, title); err != nil {
			t.Fatalf("Save(%d): %v", chapter, err)
		}
	}

	for chapter, title := range want {
		got, err := store.Load(chapter)
		if err != nil {
			t.Fatalf("Load(%d): %v", chapter, err)
		}
		if got != title {
			t.Errorf("Load(%d) = %q, want %q", chapter, got, title)
		}
	}
	got, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadAll = %#v, want %#v", got, want)
	}

	// The file is a single atomically rewritten fact map, not per-chapter files.
	if _, err := os.Stat(filepath.Join(dir, chapterTitlesPath)); err != nil {
		t.Fatalf("chapter title file missing: %v", err)
	}
}

func TestStoreInitializesChapterTitleStore(t *testing.T) {
	store := NewStore(t.TempDir())
	t.Cleanup(store.Close)
	if store.ChapterTitles == nil {
		t.Fatal("Store.ChapterTitles must be initialized")
	}
}
