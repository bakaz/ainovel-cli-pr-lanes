package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range map[string]string{
		"outline.json":       `{"chapters":3}`,
		"chapters/01.md":     "# Ch1\nContent.",
		"chapters/02.md":     "# Ch2\nMore.",
		"meta/run.json":      `{}`,
		"meta/progress.json": `{"phase":"writing"}`,
	} {
		fp := filepath.Join(dir, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(fp), 0o755)
		os.WriteFile(fp, []byte(content), 0o644)
	}
	return dir
}

// ── Unique IDs ──

func TestSameTimestampGetsDistinctIDs(t *testing.T) {
	fixedTime := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return fixedTime }
	defer func() { timeNow = time.Now }()

	m1, err := Backup(t.TempDir(), "proj-test", KindVolume, 1, 0)
	if err == nil {
		_ = m1
	}
	// The backup may fail because the source is empty in a TempDir, but that's fine.
	// What matters is that SnapshotIDAt produces different IDs for the same time.
	id1 := SnapshotIDAt(KindArc, 1, 1, fixedTime)
	id2 := SnapshotIDAt(KindArc, 1, 1, fixedTime)
	if id1 == id2 {
		t.Fatal("same-timestamp IDs must be unique (crypto/rand)")
	}
}

// ── Backup ──

func TestBackupVolume(t *testing.T) {
	src := newTestSource(t)
	m, err := Backup(src, "proj-test", KindVolume, 1, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if m.Kind != KindVolume || m.Volume != 1 {
		t.Fatal("kind/volume mismatch")
	}
	if len(m.Files) != 5 {
		t.Fatalf("got %d files, want 5", len(m.Files))
	}
	root, _ := BackupRoot(src)
	sd := filepath.Join(root, "volume", m.SnapshotID)
	if _, err := os.Stat(filepath.Join(sd, "COMPLETE")); err != nil {
		t.Fatal("COMPLETE missing")
	}
	if _, err := os.Stat(filepath.Join(src, "outline.json")); err != nil {
		t.Fatal("source missing after backup")
	}
}

func TestBackupArc(t *testing.T) {
	src := newTestSource(t)
	m, err := Backup(src, "", KindArc, 2, 3)
	if err != nil {
		t.Fatalf("Backup arc: %v", err)
	}
	if m.Kind != KindArc || m.Volume != 2 || m.Arc != 3 {
		t.Fatal("volume/arc mismatch")
	}
}

func TestSameBoundaryDoubleBackup(t *testing.T) {
	src := newTestSource(t)
	m1, err := Backup(src, "", KindArc, 1, 1)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	m2, err := Backup(src, "", KindArc, 1, 1)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if m1.SnapshotID == m2.SnapshotID {
		t.Fatal("same-boundary backups must have unique IDs")
	}
}

func TestSourceSymlinkRejected(t *testing.T) {
	realDir := t.TempDir()
	linkDir := t.TempDir()
	linkPath := filepath.Join(linkDir, "project")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Skip("symlinks not supported")
	}
	if _, err := Backup(linkPath, "", KindVolume, 1, 0); err == nil {
		t.Fatal("should reject symlink source")
	}
}

func TestSourceUnchangedAfterSuccess(t *testing.T) {
	src := newTestSource(t)
	before := readAllFiles(t, src)
	m, err := Backup(src, "", KindVolume, 1, 0)
	if err != nil {
		t.Fatalf("Backup should succeed: %v", err)
	}
	if m == nil {
		t.Fatal("Backup returned nil manifest on success")
	}
	after := readAllFiles(t, src)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("source file changed after success: %s", k)
		}
	}
	if len(before) != len(after) {
		t.Fatal("source file count changed after success")
	}
}

func TestSourceUnchangedAfterFailure(t *testing.T) {
	src := newTestSource(t)
	before := readAllFiles(t, src)
	// Create a symlink to cause failure
	linkPath := filepath.Join(src, "badlink")
	if err := os.Symlink("nonexistent", linkPath); err != nil {
		t.Skip("symlinks not supported")
	}
	_, err := Backup(src, "", KindVolume, 1, 0)
	if err == nil {
		t.Fatal("Backup should fail with symlink in tree")
	}
	os.Remove(linkPath)
	after := readAllFiles(t, src)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("source file changed after failure: %s", k)
		}
	}
	if len(before) != len(after) {
		t.Fatal("source file count changed after failure")
	}
}

// ── List ──

func TestListEmpty(t *testing.T) {
	snaps, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatal("should be empty")
	}
}

func TestListAfterBackup(t *testing.T) {
	src := newTestSource(t)
	if _, err := Backup(src, "", KindVolume, 1, 0); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := Backup(src, "", KindArc, 1, 1); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	snaps, err := List(src)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d, want 2", len(snaps))
	}
}

func TestListOrder(t *testing.T) {
	src := newTestSource(t)
	if _, err := Backup(src, "", KindArc, 1, 1); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := Backup(src, "", KindArc, 1, 2); err != nil {
		t.Fatalf("second: %v", err)
	}
	snaps, err := List(src)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) < 2 {
		t.Fatal("need 2 snapshots")
	}
	ti, _ := time.Parse(time.RFC3339Nano, snaps[0].CreatedAt)
	tj, _ := time.Parse(time.RFC3339Nano, snaps[1].CreatedAt)
	if !ti.After(tj) {
		t.Fatal("expected newest first")
	}
}

func TestPartialStagingHidden(t *testing.T) {
	src := newTestSource(t)
	root, _ := BackupRoot(src)
	os.MkdirAll(filepath.Join(root, "arc", ".partial.partial", "data"), 0o755)
	snaps, err := List(src)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range snaps {
		if strings.HasPrefix(s.SnapshotID, ".") {
			t.Fatal("partial should not appear")
		}
	}
}

// ── Retention ──

func TestRetentionArcNewest3(t *testing.T) {
	src := newTestSource(t)
	for i := 0; i < 5; i++ {
		if _, err := Backup(src, "", KindArc, 1, i+1); err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
	}
	snaps, err := List(src)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var kept []string
	for _, s := range snaps {
		if s.Kind == KindArc {
			kept = append(kept, s.SnapshotID)
		}
	}
	if len(kept) > 3 {
		t.Fatalf("expected ≤3, got %d", len(kept))
	}
	if len(kept) == 0 {
		t.Fatal("some arcs should remain")
	}
}

func TestRetentionExactIDsAcrossVolumes(t *testing.T) {
	src := newTestSource(t)
	var createdIDs []string
	// 3 volumes × 2 arcs = 6 snapshots
	for v := 1; v <= 3; v++ {
		for a := 1; a <= 2; a++ {
			m, err := Backup(src, "", KindArc, v, a)
			if err != nil {
				t.Fatalf("Backup v%d a%d: %v", v, a, err)
			}
			createdIDs = append(createdIDs, m.SnapshotID)
		}
	}
	snaps, err := List(src)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var kept []string
	for _, s := range snaps {
		if s.Kind == KindArc {
			kept = append(kept, s.SnapshotID)
		}
	}
	if len(kept) != 3 {
		t.Fatalf("expected exactly 3 retained, got %d: %v", len(kept), kept)
	}
	// The retained set must equal the last 3 created (newest 3)
	last3 := createdIDs[len(createdIDs)-3:]
	for _, id := range kept {
		found := false
		for _, lid := range last3 {
			if id == lid {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("retained ID %q is not among last 3 created %v", id, last3)
		}
	}
}

func TestRetentionVolumePermanent(t *testing.T) {
	src := newTestSource(t)
	if _, err := Backup(src, "", KindVolume, 1, 0); err != nil {
		t.Fatalf("Backup volume: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := Backup(src, "", KindArc, 1, i+1); err != nil {
			t.Fatalf("Backup arc %d: %v", i, err)
		}
	}
	snaps, err := List(src)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	vols := 0
	for _, s := range snaps {
		if s.Kind == KindVolume {
			vols++
		}
	}
	if vols != 1 {
		t.Fatalf("expected 1 volume, got %d", vols)
	}
}

// ── Verify ──

func TestVerifyValid(t *testing.T) {
	src := newTestSource(t)
	m, err := Backup(src, "", KindVolume, 1, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	root, _ := BackupRoot(src)
	sd := filepath.Join(root, "volume", m.SnapshotID)
	vm, err := Verify(sd)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vm.SnapshotID != m.SnapshotID {
		t.Fatal("Verify returned wrong snapshot")
	}
}

func TestVerifyMissingCOMPLETE(t *testing.T) {
	src := newTestSource(t)
	m, _ := Backup(src, "", KindVolume, 1, 0)
	root, _ := BackupRoot(src)
	sd := filepath.Join(root, "volume", m.SnapshotID)
	os.Remove(filepath.Join(sd, "COMPLETE"))
	if _, err := Verify(sd); err == nil {
		t.Fatal("should reject missing COMPLETE")
	}
}

func TestVerifyTamperedHash(t *testing.T) {
	src := newTestSource(t)
	m, _ := Backup(src, "", KindVolume, 1, 0)
	root, _ := BackupRoot(src)
	sd := filepath.Join(root, "volume", m.SnapshotID)
	os.WriteFile(filepath.Join(sd, "data", "outline.json"), []byte("tampered"), 0o644)
	if _, err := Verify(sd); err == nil {
		t.Fatal("should detect tampered file")
	}
}

func TestCopyFailureNoPublished(t *testing.T) {
	src := newTestSource(t)
	linkPath := filepath.Join(src, "bad")
	if err := os.Symlink(t.TempDir(), linkPath); err != nil {
		t.Skip("symlinks not supported")
	}
	_, err := Backup(src, "", KindVolume, 1, 0)
	if err == nil {
		t.Fatal("Backup should fail with symlink in tree")
	}
	root, _ := BackupRoot(src)
	entries, _ := os.ReadDir(filepath.Join(root, "volume"))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			t.Fatal("no snapshot should publish after failure")
		}
	}
}

func TestSourceNotFound(t *testing.T) {
	if _, err := Backup(t.TempDir()+"_nonexistent", "", KindVolume, 1, 0); err == nil {
		t.Fatal("should reject nonexistent source")
	}
}
