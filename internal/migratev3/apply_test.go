package migratev3

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
)

func TestApplyReviewedKeeps34AndRetractsLaterPlanning(t *testing.T) {
	book, expected := makeFixtureBook(t)
	addApplyMetadata(t, book)
	beforeOutline, _ := os.ReadFile(filepath.Join(book, "outline.json"))
	review, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyReviewed(ApplyOptions{
		BookDir: book, RunDir: review.RunDir, ApprovedManifestSHA256: review.ManifestSHA256, KeepThrough: 34,
		ExpectedEnrolled: &expected, Now: fixedNow, RandomBytes: fixedRandom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.KeptChapters != 34 || result.KeptScenes != 157 || len(result.Removed) != 8 || result.Removed[0] != 35 || result.Removed[7] != 42 || result.ReceiptSHA256 == "" {
		t.Fatalf("bad apply result: %+v", result)
	}
	verifiedApply, err := VerifyApplied(book, result.ReceiptDir, &expected)
	if err != nil || verifiedApply.ReceiptSHA256 != result.ReceiptSHA256 || verifiedApply.KeptChapters != 34 || verifiedApply.KeptScenes != 157 {
		t.Fatalf("independent apply verification failed: result=%+v err=%v", verifiedApply, err)
	}
	var outline []domain.OutlineEntry
	if err := readStrictFile(filepath.Join(book, "outline.json"), &outline); err != nil {
		t.Fatal(err)
	}
	if len(outline) != 34 {
		t.Fatalf("outline chapters=%d want 34", len(outline))
	}
	contract := newV3Contract()
	for _, chapter := range outline {
		for _, scene := range chapter.Scenes {
			if scene.IsLegacy() || contract.Validate(scene) != nil {
				t.Fatalf("chapter %d retained an invalid/legacy scene", chapter.Chapter)
			}
		}
	}
	var volumes []domain.VolumeOutline
	if err := readStrictFile(filepath.Join(book, "layered_outline.json"), &volumes); err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 || len(volumes[0].Arcs) != 1 || len(domain.FlattenOutline(volumes)) != 34 {
		t.Fatalf("later volume/skeleton planning was retained: %+v", volumes)
	}
	var progress domain.Progress
	if err := readStrictFile(filepath.Join(book, "meta", "progress.json"), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.TotalChapters != 34 || progress.CurrentChapter != 35 || progress.LatestCompleted() != 34 {
		t.Fatalf("bad post-apply progress: %+v", progress)
	}
	var compass domain.StoryCompass
	if err := readStrictFile(filepath.Join(book, "meta", "compass.json"), &compass); err != nil {
		t.Fatal(err)
	}
	if compass.Current == nil || compass.Current.LastUpdated != 34 || len(compass.Current.OpenThreads) != 1 {
		t.Fatalf("short compass was not reset: %+v", compass.Current)
	}
	var marker projectprofile.ProfileMarker
	if err := readStrictFile(filepath.Join(book, "meta", "project_profile.json"), &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Status != "active" || marker.Contract != "scene_beat_v3" || marker.ProfileID != projectprofile.SceneBeatV3ProfileID || marker.ApprovedManifestSHA256 != review.ManifestSHA256 {
		t.Fatalf("bad active marker: %+v", marker)
	}
	registry := projectprofile.NewRegistry(func() (*projectprofile.ProfileMarker, error) { return &marker, nil }, projectprofile.NewStoreFingerprinter(book), &expected)
	resolved, err := registry.Resolve()
	if err != nil || resolved.Status != projectprofile.StatusActive {
		t.Fatalf("active marker audit failed: profile=%+v err=%v", resolved, err)
	}
	backupOutline, err := os.ReadFile(filepath.Join(result.ReceiptDir, "before", "outline.json"))
	if err != nil || string(backupOutline) != string(beforeOutline) {
		t.Fatal("apply receipt did not preserve the exact prior outline")
	}
	if _, err := Verify(VerifyOptions{BookDir: book, RunDir: review.RunDir, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("pre-apply review verifier unexpectedly accepted the intentionally changed canonical state")
	}
	if _, err := ApplyReviewed(ApplyOptions{BookDir: book, RunDir: review.RunDir, ApprovedManifestSHA256: review.ManifestSHA256, KeepThrough: 34, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("reapply with an existing active marker was accepted")
	}
}

func TestApplyReviewedWrongApprovalHashWritesNothing(t *testing.T) {
	book, expected := makeFixtureBook(t)
	addApplyMetadata(t, book)
	review, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected})
	if err != nil {
		t.Fatal(err)
	}
	before := mustSnapshot(t, book)
	wrong := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := ApplyReviewed(ApplyOptions{BookDir: book, RunDir: review.RunDir, ApprovedManifestSHA256: wrong, KeepThrough: 34, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("wrong explicit approval hash was accepted")
	}
	assertSnapshot(t, book, before)
}

func addApplyMetadata(t *testing.T, book string) {
	t.Helper()
	completed := make([]int, 34)
	for i := range completed {
		completed[i] = i + 1
	}
	progress, _ := marshalJSON(domain.Progress{
		NovelName: "测试迁移书库", Phase: domain.PhaseWriting, CurrentChapter: 35, TotalChapters: 64,
		CompletedChapters: completed, Flow: domain.FlowWriting, CurrentVolume: 1, CurrentArc: 1, Layered: true,
	})
	compass, _ := marshalJSON(domain.StoryCompass{
		Long:    domain.LongCompass{EndingDirection: "长期终局不变", LastUpdated: 34},
		Current: &domain.Compass{Direction: "旧卷2规划", OpenThreads: []string{"旧第35章计划"}, LastUpdated: 34},
	})
	decision := map[string]any{
		"schema_version": 1, "id": "dec-old", "at": "2026-07-17T15:49:18+08:00", "kind": "volume_end", "decider": "architect",
		"decision": map[string]any{"action": "append_volume", "volume": 2},
	}
	decisionData, _ := json.Marshal(decision)
	mustWrite(t, filepath.Join(book, "meta", "progress.json"), progress)
	mustWrite(t, filepath.Join(book, "meta", "compass.json"), compass)
	mustWrite(t, filepath.Join(book, "meta", "decisions.jsonl"), append(decisionData, '\n'))
}
