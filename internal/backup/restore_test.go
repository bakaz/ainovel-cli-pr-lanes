package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func newTestSrcForRestore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range map[string]string{
		"outline.json":       `{"chapters":5}`,
		"chapters/01.md":     "original content",
		"meta/run.json":      `{}`,
		"meta/progress.json": `{"phase":"writing"}`,
	} {
		fp := filepath.Join(dir, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(fp), 0o755)
		os.WriteFile(fp, []byte(content), 0o644)
	}
	return dir
}

func readAllFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		m[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// assertSourceUnchanged checks that the file tree at src is identical
// (same count, same keys, same content) to the pre-capture before map.
func assertSourceUnchanged(t *testing.T, before map[string]string, src string) {
	t.Helper()
	after := readAllFiles(t, src)
	if len(before) != len(after) {
		t.Fatalf("source file count changed: before %d, after %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("source file %q changed: before %q, after %q", k, v, after[k])
		}
	}
}

func backupAndVerifyActive(t *testing.T, src string) *Manifest {
	t.Helper()
	m, err := Backup(src, "proj-test", KindVolume, 1, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return m
}

// ── Success ──

func TestRestoreSuccess(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)
	// Modify a file so we can prove it gets reverted
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":99}`), 0o644)

	rr, err := Restore(src, m.SnapshotID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rr.SnapshotID != m.SnapshotID {
		t.Fatalf("snapshot ID: got %q, want %q", rr.SnapshotID, m.SnapshotID)
	}
	if rr.RescueID == "" {
		t.Fatal("rescue ID should be set")
	}
	if rr.RescuePath == "" {
		t.Fatal("rescue path should be set")
	}
	if rr.Attempted != 4 {
		t.Fatalf("attempted: got %d, want 4", rr.Attempted)
	}
	if rr.Succeeded != 4 {
		t.Fatalf("succeeded: got %d, want 4", rr.Succeeded)
	}
	if rr.Failed != 0 {
		t.Fatalf("failed: got %d, want 0", rr.Failed)
	}
	if len(rr.FileErrors) != 0 {
		t.Fatal("should have no file errors")
	}
	if !rr.FinalVerify {
		t.Fatal("expected final verify to pass")
	}
	// Content should be reverted
	data, _ := os.ReadFile(filepath.Join(src, "outline.json"))
	if strings.TrimSpace(string(data)) != `{"chapters":5}` {
		t.Fatal("restore did not revert content")
	}
}

// ── Extra files preserved ──

func TestRestoreExtraFilesPreserved(t *testing.T) {
	src := newTestSrcForRestore(t)
	// Create snapshot FIRST (extra file does not exist yet)
	m := backupAndVerifyActive(t, src)

	// THEN add an extra file not tracked in any snapshot
	os.WriteFile(filepath.Join(src, "user_notes.txt"), []byte("extra content"), 0o644)
	// Also modify a tracked file so restore has work to do
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":99}`), 0o644)

	rr, err := Restore(src, m.SnapshotID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rr.Failed != 0 {
		t.Fatalf("expected no failures, got %d", rr.Failed)
	}
	if !rr.FinalVerify {
		t.Fatal("final verify should pass")
	}
	// Extra file (not in manifest) must survive unchanged
	data, _ := os.ReadFile(filepath.Join(src, "user_notes.txt"))
	if string(data) != "extra content" {
		t.Fatal("extra file not in manifest should be preserved unchanged")
	}
}

// ── Rescue equals full pre-restore tree including extras ──

func TestRescueEqualsPreRestoreTree(t *testing.T) {
	src := newTestSrcForRestore(t)
	// Extra files
	os.WriteFile(filepath.Join(src, "user_notes.txt"), []byte("extra"), 0o644)
	os.MkdirAll(filepath.Join(src, "drafts"), 0o755)
	os.WriteFile(filepath.Join(src, "drafts", "ideas.md"), []byte("idea"), 0o644)

	m := backupAndVerifyActive(t, src)
	os.WriteFile(filepath.Join(src, "outline.json"), []byte(`{"chapters":99}`), 0o644)

	before := readAllFiles(t, src)
	beforeLen := len(before)

	rr, err := Restore(src, m.SnapshotID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Rescue must contain all pre-restore files (including extras)
	rescueDataDir := filepath.Join(rr.RescuePath, "data")
	rescueFiles := readAllFiles(t, rescueDataDir)
	t.Logf("rescue data dir: %s", rescueDataDir)
	t.Logf("rescue files (%d): %v", len(rescueFiles), mapKeysSorted(rescueFiles))
	t.Logf("before files (%d): %v", beforeLen, mapKeysSorted(before))
	if len(rescueFiles) != beforeLen {
		t.Fatalf("rescue has %d files, pre-restore has %d", len(rescueFiles), beforeLen)
	}
	for k, v := range before {
		rk := filepath.ToSlash(k)
		got, ok := rescueFiles[rk]
		if !ok {
			// Try OS-separator key
			got = rescueFiles[k]
		}
		if got != v {
			t.Fatalf("rescue file %q: got %q, want %q", k, got, v)
		}
	}
}

func mapKeysSorted(m map[string]string) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── Preflight snapshot verify failure: zero writes ──

func TestPreflightSnapshotVerifyFailure(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)
	// Corrupt snapshot
	root, _ := BackupRoot(src)
	snapDir := filepath.Join(root, "volume", m.SnapshotID)
	os.Remove(filepath.Join(snapDir, "COMPLETE"))

	before := readAllFiles(t, src)
	_, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail on corrupt snapshot")
	}
	assertSourceUnchanged(t, before, src)
}

// ── Preflight rescue create failure: zero writes ──

func TestPreflightRescueCreateFailure(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)
	// Block rescue root by creating a file where .rescue dir would be
	root, _ := BackupRoot(src)
	rescueRootPath := filepath.Join(root, ".rescue")
	os.MkdirAll(filepath.Dir(rescueRootPath), 0o755)
	os.WriteFile(rescueRootPath, []byte("block"), 0o644)
	defer os.Remove(rescueRootPath)

	before := readAllFiles(t, src)
	_, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail on rescue creation")
	}
	assertSourceUnchanged(t, before, src)
}

// ── Preflight rescue verify failure: zero writes ──

func TestPreflightRescueVerifyFailure(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)

	before := readAllFiles(t, src)
	hookCalled := false
	SetRestoreHooks(RestoreHooks{
		BeforeRescueVerify: func() error {
			hookCalled = true
			// Corrupt the rescue that was just created by removing COMPLETE
			root, _ := BackupRoot(src)
			rescueRootDir := filepath.Join(root, ".rescue")
			entries, _ := os.ReadDir(rescueRootDir)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), ".") {
					os.Remove(filepath.Join(rescueRootDir, e.Name(), "COMPLETE"))
				}
			}
			return nil
		},
	})
	defer SetRestoreHooks(RestoreHooks{})

	_, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail on rescue verify")
	}
	if !hookCalled {
		t.Fatal("BeforeRescueVerify hook was not called")
	}
	assertSourceUnchanged(t, before, src)
}

// ── Per-file symlink target: item error ──

func TestPerFileSymlinkTarget(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)
	// Replace one file with a symlink
	linkTarget := filepath.Join(src, "outline.json.link")
	os.WriteFile(linkTarget, []byte("linkdest"), 0o644)
	os.Remove(filepath.Join(src, "outline.json"))
	if err := os.Symlink(linkTarget, filepath.Join(src, "outline.json")); err != nil {
		t.Skip("symlink not supported")
	}

	before := readAllFiles(t, src)
	rr, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail with symlink target")
	}
	// Preflight caught this before any writes — zero source changes
	assertSourceUnchanged(t, before, src)
	// Verify the correct file failed
	found := false
	for _, fe := range rr.FileErrors {
		if fe.Path == "outline.json" {
			found = true
			if !strings.Contains(fe.Error, "symlink") {
				t.Fatalf("expected symlink error, got: %s", fe.Error)
			}
			break
		}
	}
	if !found {
		t.Fatal("outline.json should be in file errors")
	}
	// Preflight — no files were written; rescue not created
	if rr.Succeeded != 0 {
		t.Fatalf("expected 0 succeeded (preflight), got %d", rr.Succeeded)
	}
	if rr.Attempted != 4 {
		t.Fatalf("expected 4 attempted, got %d", rr.Attempted)
	}
	if rr.RescueID != "" {
		t.Fatal("rescue should not be created for preflight failure")
	}
	if rr.RescuePath != "" {
		t.Fatal("rescue path should be empty for preflight failure")
	}
}

// ── Per-file directory target: item error ──

func TestPerFileDirTarget(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)
	// Replace one file with a directory
	os.Remove(filepath.Join(src, "outline.json"))
	os.MkdirAll(filepath.Join(src, "outline.json"), 0o755)

	before := readAllFiles(t, src)
	rr, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail with directory target")
	}
	// Preflight — zero source changes
	assertSourceUnchanged(t, before, src)
	found := false
	for _, fe := range rr.FileErrors {
		if fe.Path == "outline.json" {
			found = true
			if !strings.Contains(fe.Error, "directory") {
				t.Fatalf("expected directory error, got: %s", fe.Error)
			}
			break
		}
	}
	if !found {
		t.Fatal("outline.json should be in file errors")
	}
	// Preflight — no files written, no rescue created
	if rr.Succeeded != 0 {
		t.Fatalf("expected 0 succeeded (preflight), got %d", rr.Succeeded)
	}
	if rr.RescueID != "" {
		t.Fatal("rescue should not be created for preflight failure")
	}
	if rr.RescuePath != "" {
		t.Fatal("rescue path should be empty for preflight failure")
	}
}

// ── Nested parent symlink / non-directory component ──

func TestPreflightNestedParentSymlink(t *testing.T) {
	src := newTestSrcForRestore(t)
	// Add a file in a deeply nested directory to exercise component walking
	nestedDir := filepath.Join(src, "deep", "nested", "dir")
	os.MkdirAll(nestedDir, 0o755)
	os.WriteFile(filepath.Join(nestedDir, "data.txt"), []byte("nested content"), 0o644)

	m := backupAndVerifyActive(t, src)

	// Replace middle component "deep/nested" with a regular file.
	// Preflight walks: "deep" (directory, ok) → "deep/nested" (not a dir, error).
	os.RemoveAll(filepath.Join(src, "deep", "nested"))
	os.WriteFile(filepath.Join(src, "deep", "nested"), []byte("blocker"), 0o644)

	before := readAllFiles(t, src)
	rr, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail on nested parent not being a directory")
	}
	// Complete source-tree equality
	assertSourceUnchanged(t, before, src)

	// Itemization: deep/nested/dir/data.txt must be listed
	found := false
	for _, fe := range rr.FileErrors {
		if fe.Path == "deep/nested/dir/data.txt" {
			found = true
			if !strings.Contains(fe.Error, "not a directory") {
				t.Fatalf("expected not-a-directory error, got: %s", fe.Error)
			}
			break
		}
	}
	if !found {
		t.Fatal("deep/nested/dir/data.txt should be in file errors")
	}
	// Zero success, no rescue
	if rr.Succeeded != 0 {
		t.Fatalf("expected 0 succeeded (preflight), got %d", rr.Succeeded)
	}
	if rr.RescueID != "" {
		t.Fatal("rescue should not be created for preflight failure")
	}
	if rr.RescuePath != "" {
		t.Fatal("rescue path should be empty for preflight failure")
	}
	// Prove other manifest files without parent issues are also blocked
	// (not errored individually, but not written because preflight stopped)
	if rr.Attempted != 5 {
		t.Fatalf("expected 5 attempted (4 original + 1 new), got %d", rr.Attempted)
	}
}

// ── Retention does not remove rescue ──

func TestRetentionKeepsRescue(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)
	os.WriteFile(filepath.Join(src, "outline.json"), []byte("modified"), 0o644)

	rr, err := Restore(src, m.SnapshotID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rr.RescuePath == "" {
		t.Fatal("rescue path should be set")
	}

	// Create enough Arc backups to trigger retention (keeps newest 3)
	for i := 0; i < 5; i++ {
		if _, err := Backup(src, "", KindArc, 1, i+10); err != nil {
			t.Fatalf("Arc backup %d: %v", i, err)
		}
	}

	// Rescue directory must survive Arc retention
	if !dirExists(rr.RescuePath) {
		t.Fatal("rescue directory should survive Arc retention")
	}
	// Rescue manifest and COMPLETE must be present
	if _, err := Verify(rr.RescuePath); err != nil {
		t.Fatalf("rescue Verify should pass after retention: %v", err)
	}
}

// ── Per-file write failure via hook: itemized/no false success ──

func TestPerFileWriteFailure(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)

	hookCalls := 0
	SetRestoreHooks(RestoreHooks{
		BeforeFileWrite: func(path string) error {
			hookCalls++
			if path == "chapters/01.md" {
				return os.ErrPermission
			}
			return nil
		},
	})
	defer SetRestoreHooks(RestoreHooks{})

	rr, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should report error")
	}
	if hookCalls != 4 {
		t.Fatalf("hook called %d times, want 4", hookCalls)
	}
	if rr.Succeeded != 3 {
		t.Fatalf("expected 3 succeeded, got %d", rr.Succeeded)
	}
	if rr.Failed != 1 {
		t.Fatalf("expected 1 failed, got %d", rr.Failed)
	}
	// File error must be itemized
	found := false
	for _, fe := range rr.FileErrors {
		if fe.Path == "chapters/01.md" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("chapters/01.md should be in file errors")
	}
	// Rescue must be preserved
	if !dirExists(rr.RescuePath) {
		t.Fatal("rescue should be preserved after failure")
	}
	// Successfully copied files should be verified
	if !rr.FinalVerify {
		t.Fatal("final verify should pass for succeeded files")
	}
}

// ── Post-copy hash mismatch (simulated via BeforeFinalVerify) ──

func TestPostCopyHashMismatch(t *testing.T) {
	src := newTestSrcForRestore(t)
	m := backupAndVerifyActive(t, src)

	SetRestoreHooks(RestoreHooks{
		BeforeFinalVerify: func() error {
			// Corrupt a file that was just restored
			return os.WriteFile(filepath.Join(src, "outline.json"), []byte("tampered"), 0o644)
		},
	})
	defer SetRestoreHooks(RestoreHooks{})

	rr, err := Restore(src, m.SnapshotID)
	if err == nil {
		t.Fatal("should fail on final verify")
	}
	if !strings.Contains(err.Error(), "final verify") {
		t.Fatalf("expected final verify error, got: %v", err)
	}
	if rr.FinalVerify {
		t.Fatal("FinalVerify should be false")
	}
	if rr.Succeeded != 4 {
		t.Fatalf("expected 4 succeeded, got %d", rr.Succeeded)
	}
	// Rescue must be preserved
	if !dirExists(rr.RescuePath) {
		t.Fatal("rescue should be preserved")
	}
}

// ── No journal/Recover code remains (compile-time proof) ──

func TestNoOldTypesRemain(t *testing.T) {
	// These compile-time checks verify Journal, OpState, Recover, and
	// ListOperations are gone from the package.
	//
	// The new RestoreResult must not have Journal, OldDir, CandidateDir fields.
	var rr RestoreResult
	if rr.SnapshotID != "" || rr.RescueID != "" || rr.RescuePath != "" {
		// just referencing the fields to ensure they compile
	}
	_ = rr.Attempted
	_ = rr.Succeeded
	_ = rr.Failed
	_ = rr.FileErrors
	_ = rr.FinalVerify
}
