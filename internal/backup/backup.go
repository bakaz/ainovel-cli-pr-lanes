package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const schemaVersion = 1

// timeNow can be overridden by tests for deterministic timestamps.
var timeNow = time.Now

// BackupHooks 提供测试用的备份注入点。生产代码不设置。
type BackupHooks struct {
	// BeforeWalkCopy 在 walk 目录并拷贝每个文件前调用；返回 error 中止备份。
	// 用于测试验证并发排他性（如阻止 startEngine 进入）。
	BeforeWalkCopy func() error
}

var backupHooks BackupHooks

// SetBackupHooks 设置测试钩子（非线程安全，仅测试使用）。
func SetBackupHooks(hooks BackupHooks) { backupHooks = hooks }

type SnapshotKind string

const (
	KindArc    SnapshotKind = "arc"
	KindVolume SnapshotKind = "volume"
)

func (k SnapshotKind) Valid() bool { return k == KindArc || k == KindVolume }

// SnapshotID returns a globally unique ID with crypto/rand suffix.
// For testing with fixed clocks, use SnapshotIDAt.
func SnapshotID(kind SnapshotKind, volume, arc int) string {
	return SnapshotIDAt(kind, volume, arc, time.Now())
}

// SnapshotIDAt returns a unique ID with injected time (for deterministic testing).
func SnapshotIDAt(kind SnapshotKind, volume, arc int, now time.Time) string {
	ts := now.UTC().Format("20060102T150405.000000000Z")
	var b [4]byte
	rand.Read(b[:])
	randSuffix := hex.EncodeToString(b[:])
	if kind == KindVolume {
		return fmt.Sprintf("vol_%d_%s_%s", volume, ts, randSuffix)
	}
	return fmt.Sprintf("arc_v%da%d_%s_%s", volume, arc, ts, randSuffix)
}

type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (fe FileEntry) validate() error {
	if fe.Path == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(fe.Path, "/") || strings.HasPrefix(fe.Path, "\\") {
		return fmt.Errorf("absolute path: %s", fe.Path)
	}
	if strings.HasPrefix(fe.Path, "..") {
		return fmt.Errorf("traversal path: %s", fe.Path)
	}
	if fe.Size < 0 {
		return fmt.Errorf("negative size: %d", fe.Size)
	}
	if len(fe.SHA256) != 64 {
		return fmt.Errorf("bad sha256 length: %s", fe.SHA256)
	}
	for _, c := range fe.SHA256 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("bad sha256 char: %c", c)
		}
	}
	return nil
}

type Manifest struct {
	SchemaVersion int          `json:"schema_version"`
	SnapshotID    string       `json:"snapshot_id"`
	ProjectID     string       `json:"project_id,omitempty"`
	Kind          SnapshotKind `json:"kind"`
	Volume        int          `json:"volume"`
	Arc           int          `json:"arc,omitempty"`
	CreatedAt     string       `json:"created_at"`
	Source        string       `json:"source"`
	Files         []FileEntry  `json:"files"`
}

func (m Manifest) Valid() bool {
	if m.SchemaVersion != schemaVersion || m.SnapshotID == "" || !m.Kind.Valid() {
		return false
	}
	if m.Volume <= 0 || (m.Kind == KindArc && m.Arc <= 0) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err != nil {
		return false
	}
	if m.Source == "" || len(m.Files) == 0 {
		return false
	}
	seen := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if err := f.validate(); err != nil {
			return false
		}
		if seen[f.Path] {
			return false
		}
		seen[f.Path] = true
	}
	return true
}

func BackupRoot(sourceDir string) (string, error) {
	abs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(abs), filepath.Base(abs)+".backups"), nil
}

// ── Verify ──

func Verify(snapDir string) (*Manifest, error) {
	if _, err := os.Stat(filepath.Join(snapDir, "COMPLETE")); err != nil {
		return nil, fmt.Errorf("missing COMPLETE")
	}
	raw, err := os.ReadFile(filepath.Join(snapDir, "snapshot.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if !m.Valid() {
		return nil, fmt.Errorf("manifest invalid")
	}

	dataDir := filepath.Join(snapDir, "data")
	for _, fe := range m.Files {
		rel := filepath.FromSlash(fe.Path)
		fp := filepath.Join(dataDir, rel)
		// Rel containment via filepath.Rel
		fpRel, fpErr := filepath.Rel(dataDir, fp)
		if fpErr != nil || strings.HasPrefix(fpRel, "..") {
			return nil, fmt.Errorf("path containment: %s", fe.Path)
		}
		fi, err := os.Stat(fp)
		if err != nil {
			return nil, fmt.Errorf("missing: %s", fe.Path)
		}
		if fi.Size() != fe.Size {
			return nil, fmt.Errorf("size: %s", fe.Path)
		}
		hash, err := hashFile(fp)
		if err != nil {
			return nil, fmt.Errorf("hash read: %s: %w", fe.Path, err)
		}
		if hash != fe.SHA256 {
			return nil, fmt.Errorf("hash: %s", fe.Path)
		}
	}
	return &m, nil
}

// ── Backup ──

func Backup(sourceDir, projectID string, kind SnapshotKind, volume, arc int) (*Manifest, error) {
	absSrc, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	// Lstat source: must be a non-symlink directory
	srcFi, err := os.Lstat(absSrc)
	if err != nil {
		return nil, fmt.Errorf("lstat source: %w", err)
	}
	if srcFi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("source is a symlink")
	}
	if !srcFi.IsDir() {
		return nil, fmt.Errorf("source is not a directory")
	}

	if !kind.Valid() {
		return nil, fmt.Errorf("invalid kind %q", kind)
	}
	if volume <= 0 || (kind == KindArc && arc <= 0) {
		return nil, fmt.Errorf("invalid volume/arc")
	}

	backupRoot, _ := BackupRoot(absSrc)
	backupRoot, _ = filepath.Abs(backupRoot)
	if strings.EqualFold(absSrc, backupRoot) {
		return nil, fmt.Errorf("source equals backup root")
	}
	rel, _ := filepath.Rel(absSrc, backupRoot)
	if rel != "" && !strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("backup root inside source")
	}

	kindDir := filepath.Join(backupRoot, string(kind))
	if err := os.MkdirAll(kindDir, 0o755); err != nil {
		return nil, fmt.Errorf("kind dir: %w", err)
	}

	snapID := SnapshotID(kind, volume, arc)
	backupTime := timeNow()
	stageParent := filepath.Join(kindDir, "."+snapID+".partial")
	if err := os.Mkdir(stageParent, 0o755); err != nil {
		return nil, fmt.Errorf("staging: %w", err)
	}
	dataDir := filepath.Join(stageParent, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		os.RemoveAll(stageParent)
		return nil, fmt.Errorf("data dir: %w", err)
	}

	if backupHooks.BeforeWalkCopy != nil {
		if err := backupHooks.BeforeWalkCopy(); err != nil {
			os.RemoveAll(stageParent)
			return nil, fmt.Errorf("BeforeWalkCopy: %w", err)
		}
	}

	var files []FileEntry
	seen := make(map[string]bool)
	walkErr := filepath.WalkDir(absSrc, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		relPath, _ := filepath.Rel(absSrc, path)
		if relPath == "." {
			return nil
		}
		// Exclude backup root
		pa, _ := filepath.Abs(path)
		if strings.HasPrefix(strings.ToLower(pa), strings.ToLower(backupRoot)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, le := os.Lstat(path)
		if le != nil {
			return le
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink: %s", relPath)
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return fmt.Errorf("non-regular: %s", relPath)
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dataDir, relPath), 0o755)
		}
		relForward := filepath.ToSlash(relPath)
		if seen[relForward] {
			return fmt.Errorf("duplicate: %s", relPath)
		}
		seen[relForward] = true
		hash, sz, ce := copyFile(path, filepath.Join(dataDir, relPath))
		if ce != nil {
			return fmt.Errorf("copy %s: %w", relPath, ce)
		}
		files = append(files, FileEntry{Path: relForward, Size: sz, SHA256: hash})
		return nil
	})
	if walkErr != nil {
		os.RemoveAll(stageParent)
		return nil, fmt.Errorf("walk: %w", walkErr)
	}

	m := Manifest{
		SchemaVersion: schemaVersion, SnapshotID: snapID, ProjectID: projectID,
		Kind: kind, Volume: volume, Arc: arc,
		CreatedAt: backupTime.UTC().Format(time.RFC3339Nano), Source: absSrc, Files: files,
	}
	mData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		os.RemoveAll(stageParent)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stageParent, "snapshot.json"), mData, 0o644); err != nil {
		os.RemoveAll(stageParent)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stageParent, "COMPLETE"), []byte(snapID), 0o644); err != nil {
		os.RemoveAll(stageParent)
		return nil, err
	}
	// Verify before publish
	if _, err := Verify(stageParent); err != nil {
		os.RemoveAll(stageParent)
		return nil, fmt.Errorf("verify: %w", err)
	}

	finalDir := filepath.Join(kindDir, snapID)
	if _, err := os.Stat(finalDir); err == nil {
		os.RemoveAll(stageParent)
		return nil, fmt.Errorf("final dir exists: %s", snapID)
	}
	if err := os.Rename(stageParent, finalDir); err != nil {
		os.RemoveAll(stageParent)
		return nil, fmt.Errorf("rename: %w", err)
	}

	if kind == KindArc {
		if rerr := enforceRetention(backupRoot); rerr != nil {
			// Retention error never invalidates new snapshot; keep extras
			_ = rerr
		}
	}
	return &m, nil
}

func copyFile(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".copy-*.tmp")
	if err != nil {
		return "", 0, err
	}
	tp := out.Name()
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(tp)
		}
	}()
	h := sha256.New()
	mw := io.MultiWriter(out, h)
	sz, err := io.Copy(mw, in)
	if err != nil {
		return "", 0, err
	}
	if err := out.Sync(); err != nil {
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tp, dst); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), sz, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ── List ──

func List(sourceDir string) ([]Manifest, error) {
	backupRoot, err := BackupRoot(sourceDir)
	if err != nil {
		return nil, err
	}
	var result []Manifest
	for _, kind := range []string{"arc", "volume"} {
		kd := filepath.Join(backupRoot, kind)
		entries, err := os.ReadDir(kd)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sd := filepath.Join(kd, e.Name())
			m, verr := Verify(sd)
			if verr != nil {
				continue
			}
			result = append(result, *m)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339Nano, result[i].CreatedAt)
		tj, _ := time.Parse(time.RFC3339Nano, result[j].CreatedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return result[i].SnapshotID < result[j].SnapshotID
	})
	return result, nil
}

// ── Global Arc Retention ──

func enforceRetention(backupRoot string) error {
	arcDir := filepath.Join(backupRoot, "arc")
	entries, err := os.ReadDir(arcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type snap struct {
		dir string
		m   Manifest
	}
	var snaps []snap
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sd := filepath.Join(arcDir, e.Name())
		m, verr := Verify(sd)
		if verr != nil {
			continue
		}
		snaps = append(snaps, snap{sd, *m})
	}
	// Sort by CreatedAt desc (newest first), then SnapshotID for determinism
	sort.Slice(snaps, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339Nano, snaps[i].m.CreatedAt)
		tj, _ := time.Parse(time.RFC3339Nano, snaps[j].m.CreatedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return snaps[i].m.SnapshotID < snaps[j].m.SnapshotID
	})
	if len(snaps) <= 3 {
		return nil
	}
	var removeErr error
	for _, s := range snaps[3:] {
		if rerr := os.RemoveAll(s.dir); rerr != nil {
			removeErr = rerr
		}
	}
	return removeErr
}
