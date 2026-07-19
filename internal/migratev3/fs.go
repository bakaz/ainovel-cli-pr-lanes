package migratev3

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func strictJSON(data []byte, out any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return fmt.Errorf("trailing JSON document")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected trailing content: %w", err)
	}
	return nil
}

func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func jsonSemanticallyEqual(a, b any) (bool, error) {
	left, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(left) == string(right), nil
}

func atomicCreate(path string, data []byte) error {
	return atomicCreateArtifact(filepath.Dir(path), filepath.Base(path), data)
}

func atomicCreateArtifact(root, rel string, data []byte) error {
	rel, err := cleanArtifactPath(rel)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := ensurePlainDir(rootAbs, "artifact root"); err != nil {
		return err
	}
	parts := strings.Split(rel, "/")
	parent := rootAbs
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, statErr := os.Lstat(parent)
		switch {
		case os.IsNotExist(statErr):
			if err := os.Mkdir(parent, 0o755); err != nil {
				return err
			}
		case statErr != nil:
			return statErr
		case !info.IsDir() || isReparse(info):
			return fmt.Errorf("artifact directory is not a plain directory: %s", parent)
		}
		if err := ensurePlainDir(parent, "artifact directory"); err != nil {
			return err
		}
	}
	path := filepath.Join(parent, parts[len(parts)-1])
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing artifact %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensurePlainDir(parent, "artifact parent"); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := ensurePlainDir(parent, "artifact parent"); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || isReparse(info) {
		return fmt.Errorf("created artifact is not a plain regular file: %s", path)
	}
	return nil
}

func cleanArtifactPath(rel string) (string, error) {
	if rel == "" || strings.Contains(rel, "\\") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("unsafe artifact path %q", rel)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean != rel || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe artifact path %q", rel)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return "", fmt.Errorf("unsafe artifact path %q", rel)
		}
	}
	return clean, nil
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hashFile(path string) (FileHash, error) {
	data, err := readPlainFile(path)
	if err != nil {
		return FileHash{}, err
	}
	return FileHash{SHA256: hashBytes(data), Size: int64(len(data))}, nil
}

func hashFileWithin(root, rel string) (FileHash, error) {
	data, err := readPlainFileWithin(root, rel)
	if err != nil {
		return FileHash{}, err
	}
	return FileHash{SHA256: hashBytes(data), Size: int64(len(data))}, nil
}

func defaultNow() time.Time { return time.Now().UTC() }

func defaultRandom(p []byte) error {
	_, err := rand.Read(p)
	return err
}

func newRunDir(bookDir string, now func() time.Time, random func([]byte) error) (string, string, error) {
	bookAbs, err := filepath.Abs(bookDir)
	if err != nil {
		return "", "", err
	}
	bookResolved, err := filepath.EvalSymlinks(bookAbs)
	if err != nil {
		return "", "", fmt.Errorf("resolve book-dir: %w", err)
	}
	if !samePath(bookAbs, bookResolved) {
		return "", "", fmt.Errorf("book-dir reparse paths are not allowed")
	}
	if err := ensurePlainDir(bookAbs, "book-dir"); err != nil {
		return "", "", err
	}
	parent := filepath.Dir(bookAbs)
	migrations := filepath.Join(parent, "migrations")
	if err := os.Mkdir(migrations, 0o755); err != nil && !os.IsExist(err) {
		return "", "", err
	}
	if err := ensurePlainDir(migrations, "migrations directory"); err != nil {
		return "", "", err
	}
	rnd := make([]byte, 8)
	if err := random(rnd); err != nil {
		return "", "", err
	}
	runID := "scene-beat-v3-" + now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd)
	if strings.ContainsAny(runID, `/\\`) || runID == "." || runID == ".." {
		return "", "", fmt.Errorf("invalid generated run id")
	}
	runDir := filepath.Join(migrations, runID)
	if err := os.Mkdir(runDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create exclusive run directory: %w", err)
	}
	if err := ensurePlainDir(runDir, "new run directory"); err != nil {
		return "", "", err
	}
	return runDir, runID, nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

func ensureRunLocation(bookDir, runDir string) error {
	bookAbs, err := filepath.Abs(bookDir)
	if err != nil {
		return err
	}
	bookResolved, err := filepath.EvalSymlinks(bookAbs)
	if err != nil {
		return err
	}
	if !samePath(bookAbs, bookResolved) {
		return fmt.Errorf("book-dir is a reparse escape")
	}
	if err := ensurePlainDir(bookAbs, "book-dir"); err != nil {
		return err
	}
	runAbs, err := filepath.Abs(runDir)
	if err != nil {
		return err
	}
	runResolved, err := filepath.EvalSymlinks(runAbs)
	if err != nil {
		return err
	}
	wantParent := filepath.Join(filepath.Dir(bookResolved), "migrations")
	wantResolved, err := filepath.EvalSymlinks(wantParent)
	if err != nil {
		return err
	}
	if !samePath(wantParent, wantResolved) {
		return fmt.Errorf("migrations directory is a reparse escape")
	}
	if err := ensurePlainDir(wantParent, "migrations directory"); err != nil {
		return err
	}
	if !samePath(filepath.Dir(runResolved), wantResolved) {
		return fmt.Errorf("run directory is not an immediate child of %s", wantResolved)
	}
	if !samePath(runAbs, runResolved) {
		return fmt.Errorf("run directory is a symlink/reparse escape")
	}
	return ensurePlainDir(runAbs, "run directory")
}

func ensurePlainDir(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || isReparse(info) {
		return fmt.Errorf("%s is not a plain directory: %s", label, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !samePath(path, resolved) {
		return fmt.Errorf("%s is a reparse escape: %s", label, path)
	}
	return nil
}

func readPlainFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || isReparse(info) {
		return nil, fmt.Errorf("refusing non-regular/reparse file: %s", path)
	}
	return os.ReadFile(path)
}

func ensureNoReparseComponents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	root := volume + string(os.PathSeparator)
	rel := strings.TrimPrefix(abs, root)
	current := root
	for _, component := range strings.Split(filepath.Clean(rel), string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if isReparse(info) {
			return fmt.Errorf("path crosses a symlink/reparse component: %s", current)
		}
	}
	return nil
}

func readPlainFileWithin(root, rel string) ([]byte, error) {
	path, exists, err := plainEntryWithin(root, rel)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	return readPlainFile(path)
}

func plainEntryExistsWithin(root, rel string) (bool, error) {
	_, exists, err := plainEntryWithin(root, rel)
	return exists, err
}

func plainEntryWithin(root, rel string) (string, bool, error) {
	rel, err := cleanArtifactPath(rel)
	if err != nil {
		return "", false, err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	if err := ensurePlainDir(rootAbs, "source root"); err != nil {
		return "", false, err
	}
	parts := strings.Split(rel, "/")
	parent := rootAbs
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, statErr := os.Lstat(parent)
		if os.IsNotExist(statErr) {
			return filepath.Join(rootAbs, filepath.FromSlash(rel)), false, nil
		}
		if statErr != nil {
			return "", false, statErr
		}
		if !info.IsDir() || isReparse(info) {
			return "", false, fmt.Errorf("source path crosses non-directory/reparse component: %s", parent)
		}
	}
	path := filepath.Join(parent, parts[len(parts)-1])
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return path, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if isReparse(info) {
		return "", false, fmt.Errorf("source entry is a reparse point: %s", path)
	}
	return path, true, nil
}

func listRegularFiles(root string) ([]string, error) {
	files, _, err := listArtifactTree(root)
	return files, err
}

func listArtifactTree(root string) ([]string, []string, error) {
	var files []string
	var dirs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if isReparse(info) {
			return fmt.Errorf("symlink/reparse path rejected: %s", path)
		}
		if info.IsDir() {
			if !samePath(path, root) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				dirs = append(dirs, filepath.ToSlash(rel))
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs, err
}
