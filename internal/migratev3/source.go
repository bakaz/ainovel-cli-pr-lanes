package migratev3

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
)

type sourceBook struct {
	BookDir      string
	Outline      []domain.OutlineEntry
	Volumes      []domain.VolumeOutline
	Files        map[string][]byte
	Hashes       []FileHash
	Fingerprint  projectprofile.Fingerprint
	TotalScenes  int
	LegacyScenes int
	Structured   int
	FullSnapshot map[string]string
}

func loadSource(bookDir string, expected *projectprofile.Fingerprint) (*sourceBook, error) {
	abs, err := filepath.Abs(bookDir)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve book-dir: %w", err)
	}
	if info, err := os.Lstat(abs); err != nil || isReparse(info) || !samePath(abs, resolved) {
		return nil, fmt.Errorf("book-dir symlink/reparse paths are not allowed")
	}
	markerExists, err := plainEntryExistsWithin(resolved, "meta/project_profile.json")
	if err != nil {
		return nil, fmt.Errorf("inspect project marker: %w", err)
	}
	if markerExists {
		return nil, fmt.Errorf("project profile marker already exists; draft requires migration_required/no-marker state")
	}
	fullSnapshot, err := snapshotTree(resolved)
	if err != nil {
		return nil, fmt.Errorf("snapshot book-dir: %w", err)
	}

	paths := approvedSourcePaths()
	files := make(map[string][]byte, len(paths))
	var hashes []FileHash
	for _, rel := range paths {
		data, err := readPlainFileWithin(resolved, rel)
		if err != nil {
			return nil, fmt.Errorf("read approved source %s: %w", rel, err)
		}
		files[rel] = data
		hashes = append(hashes, FileHash{Path: rel, SHA256: hashBytes(data), Size: int64(len(data))})
	}

	if err := validateJSONLexically(files["outline.json"]); err != nil {
		return nil, fmt.Errorf("outline.json: %w", err)
	}
	if err := validateJSONLexically(files["layered_outline.json"]); err != nil {
		return nil, fmt.Errorf("layered_outline.json: %w", err)
	}
	if err := validateOutlineShape(files["outline.json"]); err != nil {
		return nil, fmt.Errorf("outline.json: %w", err)
	}
	if err := validateLayeredShape(files["layered_outline.json"]); err != nil {
		return nil, fmt.Errorf("layered_outline.json: %w", err)
	}

	var outline []domain.OutlineEntry
	if err := strictJSON(files["outline.json"], &outline); err != nil {
		return nil, fmt.Errorf("decode outline: %w", err)
	}
	var volumes []domain.VolumeOutline
	if err := strictJSON(files["layered_outline.json"], &volumes); err != nil {
		return nil, fmt.Errorf("decode layered outline: %w", err)
	}
	if len(outline) != 42 {
		return nil, fmt.Errorf("source chapter count = %d, want 42", len(outline))
	}
	for i, ch := range outline {
		if ch.Chapter != i+1 {
			return nil, fmt.Errorf("noncontiguous outline chapter at index %d: got %d want %d", i, ch.Chapter, i+1)
		}
		if len(ch.Scenes) == 0 {
			return nil, fmt.Errorf("chapter %d has no scenes", ch.Chapter)
		}
		for sceneIndex, scene := range ch.Scenes {
			if scene.IsLegacy() {
				if strings.TrimSpace(scene.Action) == "" {
					return nil, fmt.Errorf("chapter %d scene %d is an empty legacy string and cannot be preserved as non-empty v3 action", ch.Chapter, sceneIndex+1)
				}
				continue
			}
			if err := scene.ValidateRequired(); err != nil {
				return nil, fmt.Errorf("chapter %d scene %d is not a valid structured v2 scene: %w", ch.Chapter, sceneIndex+1, err)
			}
		}
	}
	flat := domain.FlattenOutline(volumes)
	equal, err := jsonSemanticallyEqual(outline, flat)
	if err != nil {
		return nil, fmt.Errorf("compare flat/layered outline: %w", err)
	}
	if !equal {
		return nil, fmt.Errorf("flat outline does not exactly match flattened layered outline")
	}

	total, legacy, structured := 0, 0, 0
	for _, ch := range outline {
		for _, scene := range ch.Scenes {
			total++
			if scene.IsLegacy() {
				legacy++
			} else {
				structured++
			}
		}
	}
	if total != 182 || legacy != 157 || structured != 25 {
		return nil, fmt.Errorf("unexpected scene counts: total=%d legacy=%d structured=%d; want 182/157/25", total, legacy, structured)
	}

	chapters := make(map[string]string, 34)
	for i := 1; i <= 34; i++ {
		name := fmt.Sprintf("%02d.md", i)
		chapters[name] = strings.TrimSpace(string(files["chapters/"+name]))
	}
	fp := projectprofile.NewFingerprint(strings.TrimSpace(string(files["premise.md"])), chapters)
	enrolled := fp.IsV3Enrolled()
	if expected != nil {
		enrolled = fp == *expected
	}
	if !enrolled {
		return nil, fmt.Errorf("book fingerprint is not the enrolled SceneBeat v3 project")
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].Path < hashes[j].Path })
	return &sourceBook{
		BookDir: resolved, Outline: outline, Volumes: volumes, Files: files, Hashes: hashes,
		Fingerprint: fp, TotalScenes: total, LegacyScenes: legacy, Structured: structured,
		FullSnapshot: fullSnapshot,
	}, nil
}

func (s *sourceBook) copySnapshot(runDir string) error {
	for _, file := range s.Hashes {
		if err := atomicCreateArtifact(runDir, "source_snapshot/"+file.Path, s.Files[file.Path]); err != nil {
			return err
		}
	}
	manifest := SourceManifest{Protocol: ProtocolVersion, BookDir: s.BookDir, Fingerprint: s.Fingerprint, Files: s.Hashes}
	data, err := marshalJSON(manifest)
	if err != nil {
		return err
	}
	return atomicCreateArtifact(runDir, "source_manifest.json", data)
}

func (s *sourceBook) verifyUnchanged() error {
	for _, want := range s.Hashes {
		got, err := hashFileWithin(s.BookDir, want.Path)
		if err != nil {
			return fmt.Errorf("source stale: %s: %w", want.Path, err)
		}
		if got.SHA256 != want.SHA256 || got.Size != want.Size {
			return fmt.Errorf("source stale: %s changed during draft", want.Path)
		}
	}
	current, err := snapshotTree(s.BookDir)
	if err != nil {
		return err
	}
	if len(current) != len(s.FullSnapshot) {
		return fmt.Errorf("source stale: book-dir file set changed")
	}
	for path, want := range s.FullSnapshot {
		if current[path] != want {
			return fmt.Errorf("source stale: book-dir file changed: %s", path)
		}
	}
	return nil
}

func snapshotTree(root string) (map[string]string, error) {
	files, err := listRegularFiles(root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(files))
	for _, rel := range files {
		digest, err := hashPlainFileStreaming(filepath.Join(root, filepath.FromSlash(rel)), sha256.New())
		if err != nil {
			return nil, err
		}
		out[rel] = digest
	}
	return out, nil
}

// hashPlainFileStreaming is integrity-only: it never exposes non-whitelisted
// book bytes to source parsing, snapshots, prompts, generators, or artifacts.
func hashPlainFileStreaming(path string, digest hash.Hash) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || isReparse(info) {
		return "", fmt.Errorf("integrity snapshot rejects non-regular/reparse file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(digest, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func validateJSONLexically(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err == nil {
		return fmt.Errorf("trailing JSON value")
	} else if err != io.EOF {
		return fmt.Errorf("trailing JSON content: %w", err)
	}
	return nil
}

func scanJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = true
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("unterminated object")
		}
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", d)
	}
	return nil
}

func validateOutlineShape(data []byte) error {
	var chapters []json.RawMessage
	if err := strictJSON(data, &chapters); err != nil {
		return err
	}
	for i, raw := range chapters {
		if err := validateChapterShape(raw); err != nil {
			return fmt.Errorf("chapter[%d]: %w", i, err)
		}
	}
	return nil
}

func validateLayeredShape(data []byte) error {
	var volumes []map[string]json.RawMessage
	if err := strictJSON(data, &volumes); err != nil {
		return err
	}
	for vi, volume := range volumes {
		if err := onlyKeys(volume, "index", "title", "theme", "final", "arcs"); err != nil {
			return fmt.Errorf("volume[%d]: %w", vi, err)
		}
		var arcs []map[string]json.RawMessage
		if err := strictJSON(volume["arcs"], &arcs); err != nil {
			return fmt.Errorf("volume[%d].arcs: %w", vi, err)
		}
		for ai, arc := range arcs {
			if err := onlyKeys(arc, "index", "title", "goal", "estimated_chapters", "chapters"); err != nil {
				return fmt.Errorf("volume[%d].arc[%d]: %w", vi, ai, err)
			}
			if bytes.Equal(bytes.TrimSpace(arc["chapters"]), []byte("null")) {
				continue
			}
			var chapters []json.RawMessage
			if err := strictJSON(arc["chapters"], &chapters); err != nil {
				return fmt.Errorf("volume[%d].arc[%d].chapters: %w", vi, ai, err)
			}
			for ci, chapter := range chapters {
				if err := validateChapterShape(chapter); err != nil {
					return fmt.Errorf("volume[%d].arc[%d].chapter[%d]: %w", vi, ai, ci, err)
				}
			}
		}
	}
	return nil
}

func validateChapterShape(raw json.RawMessage) error {
	var chapter map[string]json.RawMessage
	if err := strictJSON(raw, &chapter); err != nil {
		return err
	}
	if err := onlyKeys(chapter, "chapter", "title", "core_event", "hook", "scenes"); err != nil {
		return err
	}
	var scenes []json.RawMessage
	if err := strictJSON(chapter["scenes"], &scenes); err != nil {
		return fmt.Errorf("scenes: %w", err)
	}
	for i, scene := range scenes {
		trimmed := bytes.TrimSpace(scene)
		if len(trimmed) == 0 {
			return fmt.Errorf("scene[%d] empty", i)
		}
		if trimmed[0] == '"' {
			var text string
			if err := strictJSON(scene, &text); err != nil {
				return err
			}
			continue
		}
		var fields map[string]json.RawMessage
		if err := strictJSON(scene, &fields); err != nil {
			return fmt.Errorf("scene[%d]: %w", i, err)
		}
		if err := onlyKeys(fields, "goal", "action", "conflict", "outcome", "sensory_anchor", "body_reaction", "emotion_reaction", "erotic_charge"); err != nil {
			return fmt.Errorf("scene[%d]: %w", i, err)
		}
	}
	return nil
}

func onlyKeys(m map[string]json.RawMessage, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	for key := range m {
		if !set[key] {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}
