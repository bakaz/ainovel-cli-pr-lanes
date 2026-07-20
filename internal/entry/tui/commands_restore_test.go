package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/backup"
)

func TestRestoreCommandsAreRegistered(t *testing.T) {
	registry := commandRegistryInstance()
	for _, name := range []string{"snapshots", "restore"} {
		if _, ok := registry.Find(name); !ok {
			t.Fatalf("expected /%s command to be registered", name)
		}
		if !hasPaletteItem(builtinCommandItems(), name) {
			t.Fatalf("expected /%s in command palette", name)
		}
	}
}

func TestRestoreStagesAndCancelsWithoutWriting(t *testing.T) {
	m := Model{eventIndex: map[string]int{}}
	next, _ := m.handleRestoreCommand([]string{"arc-1"})
	staged := next.(Model)
	if staged.restoreStaged != "arc-1" {
		t.Fatalf("staged ID = %q, want arc-1", staged.restoreStaged)
	}
	if got := staged.events[0].Summary; !strings.Contains(got, "will overwrite existing files; extra files remain") || !strings.Contains(got, "permanent rescue is created first") {
		t.Fatalf("confirmation copy missing safety detail: %q", got)
	}
	next, _ = staged.handleRestoreCommand([]string{"cancel"})
	if got := next.(Model).restoreStaged; got != "" {
		t.Fatalf("cancel left staged ID %q", got)
	}
}

func TestRestoreRejectsStageWhileInFlight(t *testing.T) {
	m := Model{eventIndex: map[string]int{}, restoreInFlight: true}
	next, _ := m.handleRestoreCommand([]string{"arc-2"})
	if !strings.Contains(next.(Model).events[0].Summary, "already in progress") {
		t.Fatalf("expected in-flight rejection, got %q", next.(Model).events[0].Summary)
	}
	if next.(Model).restoreStaged != "" {
		t.Fatalf("expected restoreStaged to remain empty, got %q", next.(Model).restoreStaged)
	}
}

func TestRestoreRejectsConfirmWhileInFlight(t *testing.T) {
	m := Model{eventIndex: map[string]int{}, restoreStaged: "arc-1", restoreInFlight: true}
	next, _ := m.handleRestoreCommand([]string{"confirm"})
	if !strings.Contains(next.(Model).events[0].Summary, "already in progress") {
		t.Fatalf("expected in-flight rejection, got %q", next.(Model).events[0].Summary)
	}
	// restoreStaged must remain intact (not consumed)
	if next.(Model).restoreStaged != "arc-1" {
		t.Fatalf("expected restoreStaged to remain arc-1, got %q", next.(Model).restoreStaged)
	}
}

func TestRestoreRejectsCancelWhileInFlight(t *testing.T) {
	m := Model{eventIndex: map[string]int{}, restoreStaged: "arc-1", restoreInFlight: true}
	next, _ := m.handleRestoreCommand([]string{"cancel"})
	if !strings.Contains(next.(Model).events[0].Summary, "already in progress") {
		t.Fatalf("expected in-flight rejection, got %q", next.(Model).events[0].Summary)
	}
	if next.(Model).restoreStaged != "arc-1" {
		t.Fatalf("expected restoreStaged to remain arc-1, got %q", next.(Model).restoreStaged)
	}
}

func TestRestoreClearsInFlightOnResult(t *testing.T) {
	m := &Model{eventIndex: map[string]int{}, restoreInFlight: true}
	m.applyRestoreResult(restoreResultMsg{result: &backup.RestoreResult{
		SnapshotID: "arc-1",
		Attempted:  1, Succeeded: 1,
	}})
	if m.restoreInFlight {
		t.Fatal("expected restoreInFlight to be false after result")
	}
}

func TestRestoreClearsInFlightOnError(t *testing.T) {
	m := &Model{eventIndex: map[string]int{}, restoreInFlight: true}
	m.applyRestoreResult(restoreResultMsg{err: errors.New("snapshot not found")})
	if m.restoreInFlight {
		t.Fatal("expected restoreInFlight to be false after error")
	}
}

func TestRestoreResultWithoutResultShowsGenericFailed(t *testing.T) {
	m := Model{eventIndex: map[string]int{}}
	m.applyRestoreResult(restoreResultMsg{err: errors.New("snapshot not found")})
	joined := make([]string, 0, len(m.events))
	for _, event := range m.events {
		joined = append(joined, event.Summary)
	}
	got := strings.Join(joined, "\n")
	if !strings.Contains(got, "Restore failed: snapshot not found") {
		t.Fatalf("restore error output %q missing 'Restore failed: snapshot not found'", got)
	}
	// Ensure NOT the paused wording
	if strings.Contains(got, "Restore applied; finalization paused") {
		t.Fatalf("restore error output %q should not contain paused wording when result is nil", got)
	}
}

func TestFormatSnapshotIncludesRequiredFields(t *testing.T) {
	got := formatSnapshot(backup.Manifest{SnapshotID: "arc-1", Kind: backup.KindArc, Volume: 2, Arc: 3, CreatedAt: "2026-07-20T12:00:00Z"})
	for _, want := range []string{"arc-1", "arc", "Volume 2 Arc 3", "2026-07-20T12:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapshot row %q missing %q", got, want)
		}
	}
}

func TestRestoreResultWithEmptyRescuePath(t *testing.T) {
	m := Model{eventIndex: map[string]int{}}
	m.applyRestoreResult(restoreResultMsg{result: &backup.RestoreResult{
		SnapshotID: "arc-1",
		RescuePath: "",
		Attempted:  2,
		Succeeded:  2,
		Failed:     0,
	}})

	joined := make([]string, 0, len(m.events))
	for _, event := range m.events {
		joined = append(joined, event.Summary)
	}
	got := strings.Join(joined, "\n")
	for _, want := range []string{"arc-1", "no rescue created", "attempted 2, succeeded 2, failed 0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("restore result output %q missing %q", got, want)
		}
	}
	// Ensure the awkward "rescue no rescue created" is NOT rendered
	if strings.Contains(got, "rescue no rescue created") {
		t.Fatalf("restore result output %q should not contain 'rescue no rescue created'", got)
	}
}

func TestRestoreResultPreflightConflict(t *testing.T) {
	m := Model{eventIndex: map[string]int{}}
	m.applyRestoreResult(restoreResultMsg{result: &backup.RestoreResult{
		SnapshotID: "arc-1",
		RescuePath: "",
		Attempted:  4,
		Succeeded:  0,
		Failed:     4,
		FileErrors: []backup.FileError{
			{Path: "draft.md", Error: "parent component \"draft.md\" is a symlink"},
			{Path: "outline.json", Error: "target is a directory"},
		},
	}, err: errors.New("preflight: 2 file(s) have target conflicts")})

	joined := make([]string, 0, len(m.events))
	for _, event := range m.events {
		joined = append(joined, event.Summary)
	}
	got := strings.Join(joined, "\n")

	// Summary line: no "rescue " prefix before "no rescue created"
	for _, want := range []string{"arc-1", "no rescue created", "attempted 4, succeeded 0, failed 4"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight result output %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "rescue no rescue created") {
		t.Fatalf("preflight result output %q should not contain 'rescue no rescue created'", got)
	}

	// File errors rendered normally
	for _, want := range []string{"parent component \"draft.md\" is a symlink", "target is a directory"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight result output %q missing file error %q", got, want)
		}
	}

	// Error line uses preflight wording, NOT "applied; finalization paused"
	if !strings.Contains(got, "Restore failed before applying changes: preflight: 2 file(s) have target conflicts") {
		t.Fatalf("preflight result output %q missing preflight error wording", got)
	}
	if strings.Contains(got, "Restore applied; finalization paused") {
		t.Fatalf("preflight result output %q should not contain paused wording", got)
	}
}

func TestRestoreResultShowsRescueCountsAndFileErrors(t *testing.T) {
	m := Model{eventIndex: map[string]int{}}
	m.applyRestoreResult(restoreResultMsg{result: &backup.RestoreResult{
		SnapshotID: "arc-1",
		RescuePath: "/project.backups/.rescue/rescue-1",
		Attempted:  4,
		Succeeded:  3,
		Failed:     1,
		FileErrors: []backup.FileError{{Path: "draft.md", Error: "permission denied"}},
	}, err: errors.New("final verification failed")})

	joined := make([]string, 0, len(m.events))
	for _, event := range m.events {
		joined = append(joined, event.Summary)
	}
	got := strings.Join(joined, "\n")
	for _, want := range []string{"rescue /project.backups/.rescue/rescue-1", "attempted 4, succeeded 3, failed 1", "draft.md: permission denied", "Restore applied; finalization paused: final verification failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("restore result output %q missing %q", got, want)
		}
	}
}
