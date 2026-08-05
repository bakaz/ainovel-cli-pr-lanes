package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Test injection seam (minimal, per-file) ──

type RestoreHooks struct {
	BeforeFileWrite    func(path string) error // per-file failure injection
	BeforeRescueVerify func() error            // corrupt rescue before Verify
	BeforeFinalVerify  func() error            // corrupt file before final verification
}

var restoreHooks RestoreHooks

func SetRestoreHooks(h RestoreHooks) { restoreHooks = h }

// ── Result types ──

type RestoreResult struct {
	SnapshotID  string      `json:"snapshot_id"`
	RescueID    string      `json:"rescue_id"`
	RescuePath  string      `json:"rescue_path"`
	Attempted   int         `json:"attempted"`
	Succeeded   int         `json:"succeeded"`
	Failed      int         `json:"failed"`
	FileErrors  []FileError `json:"file_errors,omitempty"`
	FinalVerify bool        `json:"final_verify"`
}

type FileError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// ── Internal helpers ──

func rescueRoot(backupRoot string) string { return filepath.Join(backupRoot, ".rescue") }

// createRescue performs a full ordinary copy of sourceDir into a dedicated
// rescue directory under backupRoot/.rescue/, writes a manifest and COMPLETE
// marker, verifies, and publishes. The rescue is separate from Arc/Volume
// retention and is never rotated by the normal retention policy.
func createRescue(sourceDir, backupRoot string) (string, *Manifest, error) {
	absSrc, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve: %w", err)
	}

	rescueID := SnapshotID(KindVolume, 9999, 0)
	rr := rescueRoot(backupRoot)
	stageDir := filepath.Join(rr, "."+rescueID+".partial")

	if err := os.MkdirAll(rr, 0o755); err != nil {
		return "", nil, fmt.Errorf("rescue root: %w", err)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("stage: %w", err)
	}

	stageOK := false
	defer func() {
		if !stageOK {
			os.RemoveAll(stageDir)
		}
	}()

	dataDir := filepath.Join(stageDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("data dir: %w", err)
	}

	var files []FileEntry
	seen := make(map[string]bool)
	walkErr := filepath.WalkDir(absSrc, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		relPath, err := filepath.Rel(absSrc, path)
		if err != nil || relPath == "." {
			return nil
		}
		relForward := filepath.ToSlash(relPath)

		fi, le := os.Lstat(path)
		if le != nil {
			return le
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink: %s", relForward)
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return fmt.Errorf("non-regular: %s", relForward)
		}

		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dataDir, relForward), 0o755)
		}

		if seen[relForward] {
			return fmt.Errorf("duplicate: %s", relForward)
		}
		seen[relForward] = true

		hash, sz, ce := copyFile(path, filepath.Join(dataDir, relForward))
		if ce != nil {
			return fmt.Errorf("copy %s: %w", relForward, ce)
		}
		files = append(files, FileEntry{Path: relForward, Size: sz, SHA256: hash})
		return nil
	})
	if walkErr != nil {
		return "", nil, fmt.Errorf("rescue: %w", walkErr)
	}

	m := Manifest{
		SchemaVersion: schemaVersion,
		SnapshotID:    rescueID,
		Kind:          KindVolume,
		Volume:        9999,
		CreatedAt:     timeNow().UTC().Format(time.RFC3339Nano),
		Source:        absSrc,
		Files:         files,
	}
	mData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "snapshot.json"), mData, 0o644); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "COMPLETE"), []byte(rescueID), 0o644); err != nil {
		return "", nil, err
	}

	// Verify before publish
	if _, err := Verify(stageDir); err != nil {
		return "", nil, fmt.Errorf("rescue self-verify: %w", err)
	}

	rescueDir := filepath.Join(rr, rescueID)
	if _, err := os.Stat(rescueDir); err == nil {
		return "", nil, fmt.Errorf("rescue dir exists: %s", rescueID)
	}
	if err := os.Rename(stageDir, rescueDir); err != nil {
		return "", nil, fmt.Errorf("publish: %w", err)
	}

	stageOK = true
	return rescueDir, &m, nil
}

// restoreFile copies a single manifest file from the snapshot's data directory
// to the target root. The copy goes to a same-directory temporary file,
// is verified for size and SHA-256, then atomically replaces the target.
// If the target is a symlink, directory, or non-regular file the operation
// fails with an item error and does not delete or recreate the target.
func restoreFile(fe FileEntry, dataDir, targetRoot string) error {
	rel := filepath.FromSlash(fe.Path)

	// Path safety — reject absolute or traversal paths
	if filepath.IsAbs(rel) {
		return fmt.Errorf("absolute path")
	}
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path escapes target")
	}

	srcPath := filepath.Join(dataDir, rel)
	targetPath := filepath.Join(targetRoot, rel)
	targetParent := filepath.Dir(targetPath)

	// Check existing target — must not be symlink/dir/nonregular
	if fi, err := os.Lstat(targetPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("target is a symlink")
		}
		if fi.IsDir() {
			return fmt.Errorf("target is a directory")
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("target is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat target: %w", err)
	}

	// Create parent directories (only ordinary dirs — filepath.Dir is safe)
	if err := os.MkdirAll(targetParent, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Copy to a temporary file in the same directory
	tmpFile, err := os.CreateTemp(targetParent, ".restore-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmpFile.Name()
	closeAndRemove := true
	defer func() {
		tmpFile.Close()
		if closeAndRemove {
			os.Remove(tmpPath)
		}
	}()

	in, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, h), in)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	// Mark closed so the deferred Close is a no-op
	tmpFile = nil

	// Verify size and hash before replace
	if written != fe.Size {
		return fmt.Errorf("size: got %d, want %d", written, fe.Size)
	}
	gotHash := hex.EncodeToString(h.Sum(nil))
	if gotHash != fe.SHA256 {
		return fmt.Errorf("hash: got %s, want %s", gotHash, fe.SHA256)
	}

	// Atomically replace target
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("replace: %w", err)
	}

	closeAndRemove = false
	return nil
}

func verifyRestored(dir string, m *Manifest) error {
	for _, fe := range m.Files {
		fp := filepath.Join(dir, filepath.FromSlash(fe.Path))
		fi, err := os.Stat(fp)
		if err != nil {
			return fmt.Errorf("missing: %s", fe.Path)
		}
		if fi.Size() != fe.Size {
			return fmt.Errorf("size: %s", fe.Path)
		}
		hash, err := hashFile(fp)
		if err != nil {
			return err
		}
		if hash != fe.SHA256 {
			return fmt.Errorf("hash: %s", fe.Path)
		}
	}
	return nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func findSnapshotDir(backupRoot, snapshotID string) string {
	for _, kind := range []string{"arc", "volume"} {
		p := filepath.Join(backupRoot, kind, snapshotID)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ── Preflight target-path validation ──

// preflightCheck walks each existing parent component from targetRoot toward
// each manifest file's target path, Lstats every component, and rejects a
// symlink or non-directory with a structured FileError.  Component walking
// stops at the first nonexistent component (MkdirAll will create the rest).
// Target-file checking follows the same rules as restoreFile.
// If any conflict is found the restore is blocked with zero active-tree
// writes and no rescue creation.
func preflightCheck(files []FileEntry, targetRoot string) []FileError {
	var errors []FileError
	for _, fe := range files {
		rel := filepath.FromSlash(fe.Path)
		parts := strings.Split(rel, string(filepath.Separator))

		// Walk each existing parent component from root toward target.
		// Stop at the first component that does not exist.
		current := targetRoot
		pathFailed := false

		for i := 0; i < len(parts)-1; i++ {
			if parts[i] == "" {
				continue
			}
			current = filepath.Join(current, parts[i])

			fi, err := os.Lstat(current)
			if err != nil {
				if os.IsNotExist(err) {
					break // first nonexistent — MkdirAll creates the rest
				}
				errors = append(errors, FileError{
					Path:  fe.Path,
					Error: fmt.Sprintf("lstat parent: %s", err),
				})
				pathFailed = true
				break
			}

			if fi.Mode()&os.ModeSymlink != 0 {
				errors = append(errors, FileError{
					Path: fe.Path,
					Error: fmt.Sprintf("parent component %q is a symlink",
						strings.Join(parts[:i+1], "/")),
				})
				pathFailed = true
				break
			}
			if !fi.IsDir() {
				errors = append(errors, FileError{
					Path: fe.Path,
					Error: fmt.Sprintf("parent component %q is not a directory",
						strings.Join(parts[:i+1], "/")),
				})
				pathFailed = true
				break
			}
		}
		if pathFailed {
			continue
		}

		// Check existing target — must be absent or an ordinary regular file
		targetPath := filepath.Join(targetRoot, rel)
		if fi, err := os.Lstat(targetPath); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				errors = append(errors, FileError{Path: fe.Path, Error: "target is a symlink"})
				continue
			}
			if fi.IsDir() {
				errors = append(errors, FileError{Path: fe.Path, Error: "target is a directory"})
				continue
			}
			if !fi.Mode().IsRegular() {
				errors = append(errors, FileError{Path: fe.Path, Error: "target is not a regular file"})
				continue
			}
		} else if !os.IsNotExist(err) {
			errors = append(errors, FileError{
				Path:  fe.Path,
				Error: fmt.Sprintf("lstat target: %v", err),
			})
			continue
		}
	}
	return errors
}

// ── Restore (lightweight copy-over) ──

// Restore verifies a selected complete snapshot, runs a target-path
// preflight check, creates and verifies a dedicated rescue backup of the
// active source tree, then copies every manifest-listed file over the active
// project using per-file temp replacement. Files not present in the manifest
// are never deleted. Returns a structured result with per-file error detail.
//
// Preflight (target-path check / snapshot verify / rescue create / rescue
// verify) failure guarantees zero writes to the active ordinary tree.
func Restore(sourceDir, snapshotID string) (*RestoreResult, error) {
	absSrc, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	backupRoot, err := BackupRoot(sourceDir)
	if err != nil {
		return nil, err
	}

	// ── Preflight: zero writes on failure ──

	snapDir := findSnapshotDir(backupRoot, snapshotID)
	if snapDir == "" {
		return nil, fmt.Errorf("snapshot %s not found", snapshotID)
	}
	snapManifest, err := Verify(snapDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot verify: %w", err)
	}

	// Target-path preflight check — before rescue, so per-file
	// symlink/dir/nonregular conflicts block with zero writes and no rescue.
	if errs := preflightCheck(snapManifest.Files, absSrc); len(errs) > 0 {
		return &RestoreResult{
			SnapshotID: snapshotID,
			Attempted:  len(snapManifest.Files),
			Failed:     len(errs),
			FileErrors: errs,
		}, fmt.Errorf("preflight: %d file(s) have target conflicts", len(errs))
	}

	rescueDir, rescueManifest, err := createRescue(absSrc, backupRoot)
	if err != nil {
		return nil, fmt.Errorf("rescue: %w", err)
	}

	if restoreHooks.BeforeRescueVerify != nil {
		if err := restoreHooks.BeforeRescueVerify(); err != nil {
			os.RemoveAll(rescueDir)
			return nil, fmt.Errorf("rescue hook: %w", err)
		}
	}
	if _, err := Verify(rescueDir); err != nil {
		os.RemoveAll(rescueDir)
		return nil, fmt.Errorf("rescue verify: %w", err)
	}

	// ── Copy-over phase ──

	result := &RestoreResult{
		SnapshotID: snapshotID,
		RescueID:   rescueManifest.SnapshotID,
		RescuePath: rescueDir,
	}
	dataDir := filepath.Join(snapDir, "data")

	for _, fe := range snapManifest.Files {
		result.Attempted++

		// Hook: per-file failure injection
		if restoreHooks.BeforeFileWrite != nil {
			if hookErr := restoreHooks.BeforeFileWrite(fe.Path); hookErr != nil {
				result.Failed++
				result.FileErrors = append(result.FileErrors, FileError{
					Path: fe.Path, Error: hookErr.Error(),
				})
				continue
			}
		}

		if err := restoreFile(fe, dataDir, absSrc); err != nil {
			result.Failed++
			result.FileErrors = append(result.FileErrors, FileError{
				Path: fe.Path, Error: err.Error(),
			})
		} else {
			result.Succeeded++
		}
	}

	// ── Final verification ──

	if restoreHooks.BeforeFinalVerify != nil {
		if err := restoreHooks.BeforeFinalVerify(); err != nil {
			result.FinalVerify = false
			return result, fmt.Errorf("final verify hook: %w", err)
		}
	}

	// Verify files that were successfully restored
	if result.Succeeded > 0 {
		verifyManifest := &Manifest{Files: make([]FileEntry, 0, result.Succeeded)}
		failedSet := make(map[string]bool, len(result.FileErrors))
		for _, fe := range result.FileErrors {
			failedSet[fe.Path] = true
		}
		for _, fe := range snapManifest.Files {
			if !failedSet[fe.Path] {
				verifyManifest.Files = append(verifyManifest.Files, fe)
			}
		}
		if err := verifyRestored(absSrc, verifyManifest); err != nil {
			result.FinalVerify = false
			if result.Failed > 0 {
				return result, fmt.Errorf("final verify: %w (and %d file(s) failed)", err, result.Failed)
			}
			return result, fmt.Errorf("final verify: %w", err)
		}
	}
	result.FinalVerify = true

	if result.Failed > 0 {
		return result, fmt.Errorf("%d file(s) failed", result.Failed)
	}
	return result, nil
}
