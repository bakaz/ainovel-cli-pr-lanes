package host

import (
	"fmt"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestSelectOutlineForSnapshotUsesCurrentVolume(t *testing.T) {
	outline := makeOutlineEntries(40)
	volumes := []domain.VolumeOutline{
		{Index: 1, Arcs: []domain.ArcOutline{{Chapters: outline[:25]}}},
		{Index: 2, Arcs: []domain.ArcOutline{{Chapters: outline[25:33]}}},
		{Index: 3, Arcs: []domain.ArcOutline{{Chapters: outline[33:]}}},
	}
	progress := &domain.Progress{Layered: true, CurrentVolume: 2, InProgressChapter: 28}
	got := selectOutlineForSnapshot(outline, progress, volumes)
	if len(got) != 8 {
		t.Fatalf("current volume window len=%d, want 8: %+v", len(got), got)
	}
	if got[0].Chapter != 26 || got[len(got)-1].Chapter != 33 {
		t.Fatalf("volume window chapters %d-%d, want 26-33", got[0].Chapter, got[len(got)-1].Chapter)
	}
}

func TestSelectOutlineForSnapshotCapsLongBook(t *testing.T) {
	outline := makeOutlineEntries(200)
	progress := &domain.Progress{InProgressChapter: 150}
	got := selectOutlineForSnapshot(outline, progress, nil)
	if len(got) != maxOutlineSnapshotChapters {
		t.Fatalf("len=%d, want %d", len(got), maxOutlineSnapshotChapters)
	}
	found := false
	for _, e := range got {
		if e.Chapter == 150 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("window %d-%d should include in-progress 150", got[0].Chapter, got[len(got)-1].Chapter)
	}
}

func TestWindowOutlineEntriesClampsToEnds(t *testing.T) {
	outline := makeOutlineEntries(30)
	got := windowOutlineEntries(outline, 1, 10)
	if len(got) != 10 || got[0].Chapter != 1 || got[9].Chapter != 10 {
		t.Fatalf("start window = %d-%d", got[0].Chapter, got[len(got)-1].Chapter)
	}
	got = windowOutlineEntries(outline, 30, 10)
	if len(got) != 10 || got[0].Chapter != 21 || got[9].Chapter != 30 {
		t.Fatalf("end window = %d-%d", got[0].Chapter, got[len(got)-1].Chapter)
	}
}

func makeOutlineEntries(n int) []domain.OutlineEntry {
	out := make([]domain.OutlineEntry, n)
	for i := 0; i < n; i++ {
		out[i] = domain.OutlineEntry{Chapter: i + 1, Title: fmt.Sprintf("第%d章", i+1)}
	}
	return out
}
