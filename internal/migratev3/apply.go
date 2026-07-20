package migratev3

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

const ApplyProtocolVersion = "scene-beat-v3-apply/v1"

type ApplyOptions struct {
	BookDir                string
	RunDir                 string
	ApprovedManifestSHA256 string
	KeepThrough            int
	ExpectedEnrolled       *projectprofile.Fingerprint
	Now                    func() time.Time
	RandomBytes            func([]byte) error
}

type ApplyResult struct {
	ReceiptDir    string
	ReceiptSHA256 string
	KeptChapters  int
	KeptScenes    int
	Removed       []int
}

type VerifyApplyResult struct {
	ReceiptSHA256 string
	KeptChapters  int
	KeptScenes    int
}

type applyReceipt struct {
	Protocol               string     `json:"protocol"`
	Status                 string     `json:"status"`
	AppliedAt              time.Time  `json:"applied_at"`
	BookDir                string     `json:"book_dir"`
	ReviewedRun            string     `json:"reviewed_run"`
	ApprovedManifestSHA256 string     `json:"approved_manifest_sha256"`
	KeepThrough            int        `json:"keep_through"`
	RemovedChapters        []int      `json:"removed_chapters"`
	Before                 []FileHash `json:"before"`
	After                  []FileHash `json:"after"`
}

var canonicalApplyFiles = []string{
	"outline.json", "outline.md", "layered_outline.json", "layered_outline.md",
	"meta/progress.json", "meta/compass.json", "meta/decisions.jsonl",
}

// ApplyReviewed is the intentionally narrow post-review transition for this
// enrolled workspace. It applies the verified v7 proposal only through chapter
// 34 and retracts every later planned chapter so Architect can plan chapter 35
// again. A recoverable before/candidate receipt is completed before canonical
// writes; the active marker is always written last.
func ApplyReviewed(opts ApplyOptions) (ApplyResult, error) {
	if opts.KeepThrough != 34 {
		return ApplyResult{}, fmt.Errorf("apply requires --keep-through 34")
	}
	if len(opts.ApprovedManifestSHA256) != 64 || strings.ToLower(opts.ApprovedManifestSHA256) != opts.ApprovedManifestSHA256 {
		return ApplyResult{}, fmt.Errorf("approved manifest SHA-256 must be 64 lowercase hex characters")
	}
	if _, err := hex.DecodeString(opts.ApprovedManifestSHA256); err != nil {
		return ApplyResult{}, fmt.Errorf("approved manifest SHA-256 is invalid")
	}
	if opts.Now == nil {
		opts.Now = defaultNow
	}
	if opts.RandomBytes == nil {
		opts.RandomBytes = defaultRandom
	}
	verified, err := Verify(VerifyOptions{BookDir: opts.BookDir, RunDir: opts.RunDir, ExpectedEnrolled: opts.ExpectedEnrolled})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("review run verification failed: %w", err)
	}
	if verified.ManifestSHA256 != opts.ApprovedManifestSHA256 {
		return ApplyResult{}, fmt.Errorf("explicit approval hash does not match verified review manifest")
	}
	source, err := loadSource(opts.BookDir, opts.ExpectedEnrolled)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(source.Outline) != 42 {
		return ApplyResult{}, fmt.Errorf("apply source must still contain the reviewed 42 chapters")
	}
	markerExists, err := plainEntryExistsWithin(source.BookDir, "meta/project_profile.json")
	if err != nil {
		return ApplyResult{}, err
	}
	if markerExists {
		return ApplyResult{}, fmt.Errorf("project profile marker already exists; refusing reapply")
	}

	var proposed []domain.OutlineEntry
	if err := readStrictArtifact(opts.RunDir, "proposed/outline.json", &proposed); err != nil {
		return ApplyResult{}, err
	}
	var proposedVolumes []domain.VolumeOutline
	if err := readStrictArtifact(opts.RunDir, "proposed/layered_outline.json", &proposedVolumes); err != nil {
		return ApplyResult{}, err
	}
	outline, volumes, removed, scenes, err := truncateReviewedProposal(source, proposed, proposedVolumes, opts.KeepThrough)
	if err != nil {
		return ApplyResult{}, err
	}

	progressData, err := readPlainFileWithin(source.BookDir, "meta/progress.json")
	if err != nil {
		return ApplyResult{}, err
	}
	var progress domain.Progress
	if err := strictJSON(progressData, &progress); err != nil {
		return ApplyResult{}, fmt.Errorf("strict progress decode: %w", err)
	}
	if progress.LatestCompleted() != 34 || progress.CurrentChapter != 35 || progress.InProgressChapter != 0 {
		return ApplyResult{}, fmt.Errorf("progress is not at the reviewed chapter-34 boundary")
	}
	progress.TotalChapters = 34
	progress.CurrentVolume = volumes[len(volumes)-1].Index
	progress.CurrentArc = volumes[len(volumes)-1].Arcs[len(volumes[len(volumes)-1].Arcs)-1].Index

	compassData, err := readPlainFileWithin(source.BookDir, "meta/compass.json")
	if err != nil {
		return ApplyResult{}, err
	}
	var compass domain.StoryCompass
	if err := strictJSON(compassData, &compass); err != nil {
		return ApplyResult{}, fmt.Errorf("strict compass decode: %w", err)
	}
	compass.Current = &domain.Compass{
		Direction:   "第1–34章已完成；第35章及以后未规划。下一步由 Architect 重新执行完结判定并 append_volume，生成新的第二卷与首个展开弧。",
		OpenThreads: []string{"第35章及以后卷、弧和章节细纲已按用户要求撤回，等待 Architect 重新生成"},
		LastUpdated: 34,
	}

	decisions, err := readPlainFileWithin(source.BookDir, "meta/decisions.jsonl")
	if err != nil {
		return ApplyResult{}, err
	}
	decisionLine, err := userOverrideDecision(opts.Now().UTC(), opts.ApprovedManifestSHA256, removed)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(decisions) > 0 && decisions[len(decisions)-1] != '\n' {
		decisions = append(decisions, '\n')
	}
	decisions = append(decisions, decisionLine...)

	marker := projectprofile.ProfileMarker{
		Version: projectprofile.ProfileVersion, Contract: projectprofile.ContractSceneBeatV3.String(), Status: projectprofile.StatusActive.String(),
		ProfileID: projectprofile.SceneBeatV3ProfileID, EnrollmentFingerprint: &source.Fingerprint,
		ApprovedManifestSHA256: opts.ApprovedManifestSHA256,
	}
	candidate := map[string][]byte{}
	if candidate["outline.json"], err = marshalJSON(outline); err != nil {
		return ApplyResult{}, err
	}
	candidate["outline.md"] = []byte(renderOutline(outline))
	if candidate["layered_outline.json"], err = marshalJSON(volumes); err != nil {
		return ApplyResult{}, err
	}
	candidate["layered_outline.md"] = []byte(renderLayeredOutline(volumes))
	if candidate["meta/progress.json"], err = marshalJSON(progress); err != nil {
		return ApplyResult{}, err
	}
	if candidate["meta/compass.json"], err = marshalJSON(compass); err != nil {
		return ApplyResult{}, err
	}
	candidate["meta/decisions.jsonl"] = decisions
	markerData, err := marshalJSON(marker)
	if err != nil {
		return ApplyResult{}, err
	}
	candidate["meta/project_profile.json"] = markerData

	if err := source.verifyUnchanged(); err != nil {
		return ApplyResult{}, err
	}
	receiptDir, err := newApplyReceiptDir(source.BookDir, opts.Now, opts.RandomBytes)
	if err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{ReceiptDir: receiptDir, KeptChapters: len(outline), KeptScenes: scenes, Removed: removed}

	before := make(map[string][]byte, len(canonicalApplyFiles))
	var beforeHashes, afterHashes []FileHash
	for _, rel := range canonicalApplyFiles {
		data, err := readPlainFileWithin(source.BookDir, rel)
		if err != nil {
			return result, err
		}
		before[rel] = data
		beforeHashes = append(beforeHashes, fileHashForBytes(rel, data))
		if err := atomicCreateArtifact(receiptDir, "before/"+rel, data); err != nil {
			return result, err
		}
	}
	for rel, data := range candidate {
		afterHashes = append(afterHashes, fileHashForBytes(rel, data))
		if err := atomicCreateArtifact(receiptDir, "candidate/"+rel, data); err != nil {
			return result, err
		}
	}
	sort.Slice(beforeHashes, func(i, j int) bool { return beforeHashes[i].Path < beforeHashes[j].Path })
	sort.Slice(afterHashes, func(i, j int) bool { return afterHashes[i].Path < afterHashes[j].Path })
	plan := applyReceipt{
		Protocol: ApplyProtocolVersion, Status: "prepared", AppliedAt: opts.Now().UTC(), BookDir: source.BookDir,
		ReviewedRun: opts.RunDir, ApprovedManifestSHA256: opts.ApprovedManifestSHA256, KeepThrough: 34,
		RemovedChapters: removed, Before: beforeHashes, After: afterHashes,
	}
	planData, _ := marshalJSON(plan)
	if err := atomicCreateArtifact(receiptDir, "apply_plan.json", planData); err != nil {
		return result, err
	}

	written := make([]string, 0, len(canonicalApplyFiles))
	rollback := func(cause error) (ApplyResult, error) {
		var rollbackErrors []string
		for i := len(written) - 1; i >= 0; i-- {
			rel := written[i]
			if restoreErr := atomicReplaceCanonical(source.BookDir, rel, before[rel]); restoreErr != nil {
				rollbackErrors = append(rollbackErrors, restoreErr.Error())
			}
		}
		if len(rollbackErrors) != 0 {
			return result, fmt.Errorf("apply failed: %v; rollback failed: %s; recover from %s", cause, strings.Join(rollbackErrors, "; "), receiptDir)
		}
		return result, fmt.Errorf("apply failed and canonical files were restored: %w", cause)
	}
	for _, rel := range canonicalApplyFiles {
		if err := atomicReplaceCanonical(source.BookDir, rel, candidate[rel]); err != nil {
			return rollback(err)
		}
		written = append(written, rel)
	}
	if err := atomicCreateCanonical(source.BookDir, "meta/project_profile.json", markerData); err != nil {
		return rollback(err)
	}
	for rel, want := range candidate {
		got, err := readPlainFileWithin(source.BookDir, rel)
		if err != nil || !bytes.Equal(got, want) {
			_ = os.Remove(filepath.Join(source.BookDir, "meta", "project_profile.json"))
			return rollback(fmt.Errorf("post-apply verification failed for %s", rel))
		}
	}
	plan.Status = "complete"
	receiptData, err := marshalJSON(plan)
	if err != nil {
		return result, err
	}
	if err := atomicCreateArtifact(receiptDir, "receipt.json", receiptData); err != nil {
		return result, err
	}
	result.ReceiptSHA256 = hashBytes(receiptData)
	if err := atomicCreateArtifact(receiptDir, "receipt.sha256", []byte(result.ReceiptSHA256+"  receipt.json\n")); err != nil {
		return result, err
	}
	return result, nil
}

func VerifyApplied(bookDir, receiptDir string, expectedEnrolled ...*projectprofile.Fingerprint) (VerifyApplyResult, error) {
	bookAbs, err := filepath.Abs(bookDir)
	if err != nil {
		return VerifyApplyResult{}, err
	}
	receiptAbs, err := filepath.Abs(receiptDir)
	if err != nil {
		return VerifyApplyResult{}, err
	}
	if err := ensurePlainDir(bookAbs, "book-dir"); err != nil {
		return VerifyApplyResult{}, err
	}
	if err := ensurePlainDir(receiptAbs, "apply receipt directory"); err != nil {
		return VerifyApplyResult{}, err
	}
	if !samePath(filepath.Dir(receiptAbs), filepath.Join(filepath.Dir(bookAbs), "migrations")) || !strings.HasPrefix(filepath.Base(receiptAbs), "scene-beat-v3-apply-") {
		return VerifyApplyResult{}, fmt.Errorf("apply receipt is not an immediate migrations sibling")
	}
	var receipt applyReceipt
	if err := readStrictArtifact(receiptAbs, "receipt.json", &receipt); err != nil {
		return VerifyApplyResult{}, err
	}
	if receipt.Protocol != ApplyProtocolVersion || receipt.Status != "complete" || receipt.KeepThrough != 34 || !samePath(receipt.BookDir, bookAbs) {
		return VerifyApplyResult{}, fmt.Errorf("apply receipt metadata is invalid")
	}
	beforeSet := make(map[string]bool, len(canonicalApplyFiles))
	afterSet := make(map[string]bool, len(canonicalApplyFiles)+1)
	for _, rel := range canonicalApplyFiles {
		beforeSet[rel] = true
		afterSet[rel] = true
	}
	afterSet["meta/project_profile.json"] = true
	if !exactReceiptHashPaths(receipt.Before, beforeSet) || !exactReceiptHashPaths(receipt.After, afterSet) {
		return VerifyApplyResult{}, fmt.Errorf("apply receipt before/after path sets are not closed")
	}
	receiptData, err := readArtifactFile(receiptAbs, "receipt.json")
	if err != nil {
		return VerifyApplyResult{}, err
	}
	receiptHash := hashBytes(receiptData)
	hashLine, err := readArtifactFile(receiptAbs, "receipt.sha256")
	if err != nil || string(hashLine) != receiptHash+"  receipt.json\n" {
		return VerifyApplyResult{}, fmt.Errorf("apply receipt detached hash mismatch")
	}

	files, dirs, err := listArtifactTree(receiptAbs)
	if err != nil {
		return VerifyApplyResult{}, err
	}
	wantFiles := map[string]bool{"apply_plan.json": true, "receipt.json": true, "receipt.sha256": true}
	for _, rel := range canonicalApplyFiles {
		wantFiles["before/"+rel] = true
	}
	for _, hash := range receipt.After {
		wantFiles["candidate/"+hash.Path] = true
	}
	if len(files) != len(wantFiles) {
		return VerifyApplyResult{}, fmt.Errorf("apply receipt file set mismatch")
	}
	for _, rel := range files {
		if !wantFiles[rel] {
			return VerifyApplyResult{}, fmt.Errorf("unexpected apply receipt artifact %s", rel)
		}
	}
	wantDirs := []string{"before", "before/meta", "candidate", "candidate/meta"}
	if len(dirs) != len(wantDirs) {
		return VerifyApplyResult{}, fmt.Errorf("apply receipt directory set mismatch")
	}
	for i := range wantDirs {
		if dirs[i] != wantDirs[i] {
			return VerifyApplyResult{}, fmt.Errorf("unexpected apply receipt directory %s", dirs[i])
		}
	}
	var plan applyReceipt
	if err := readStrictArtifact(receiptAbs, "apply_plan.json", &plan); err != nil {
		return VerifyApplyResult{}, err
	}
	if plan.Status != "prepared" {
		return VerifyApplyResult{}, fmt.Errorf("apply plan is not the prepared receipt")
	}
	plan.Status = "complete"
	equal, err := jsonSemanticallyEqual(plan, receipt)
	if err != nil || !equal {
		return VerifyApplyResult{}, fmt.Errorf("prepared plan and completed receipt differ")
	}
	if err := verifyReceiptHashes(receiptAbs, "before", receipt.Before); err != nil {
		return VerifyApplyResult{}, err
	}
	if err := verifyReceiptHashes(receiptAbs, "candidate", receipt.After); err != nil {
		return VerifyApplyResult{}, err
	}
	for _, hash := range receipt.After {
		canonical, err := readPlainFileWithin(bookAbs, hash.Path)
		if err != nil || hashBytes(canonical) != hash.SHA256 || int64(len(canonical)) != hash.Size {
			return VerifyApplyResult{}, fmt.Errorf("canonical file differs from applied receipt: %s", hash.Path)
		}
	}

	var outline []domain.OutlineEntry
	if data, err := readPlainFileWithin(bookAbs, "outline.json"); err != nil || strictJSON(data, &outline) != nil || len(outline) != 34 {
		return VerifyApplyResult{}, fmt.Errorf("applied flat outline is not the strict 34-chapter candidate")
	}
	var volumes []domain.VolumeOutline
	if data, err := readPlainFileWithin(bookAbs, "layered_outline.json"); err != nil || strictJSON(data, &volumes) != nil || len(volumes) != 1 {
		return VerifyApplyResult{}, fmt.Errorf("applied layered outline is invalid")
	}
	if err := store.ValidateVolumesLayeredOutline(volumes); err != nil {
		return VerifyApplyResult{}, fmt.Errorf("applied volumes: %w", err)
	}
	flat := domain.FlattenOutline(volumes)
	equal, err = jsonSemanticallyEqual(outline, flat)
	if err != nil || !equal {
		return VerifyApplyResult{}, fmt.Errorf("applied flat/layered outlines differ")
	}
	outlineMD, err := readPlainFileWithin(bookAbs, "outline.md")
	if err != nil || string(outlineMD) != renderOutline(outline) {
		return VerifyApplyResult{}, fmt.Errorf("applied flat Markdown is not deterministic")
	}
	layeredMD, err := readPlainFileWithin(bookAbs, "layered_outline.md")
	if err != nil || string(layeredMD) != renderLayeredOutline(volumes) {
		return VerifyApplyResult{}, fmt.Errorf("applied layered Markdown is not deterministic")
	}
	contract := newV3Contract()
	scenes := 0
	for i, chapter := range outline {
		if chapter.Chapter != i+1 || len(chapter.Scenes) == 0 {
			return VerifyApplyResult{}, fmt.Errorf("applied chapter sequence/scenes invalid at %d", i+1)
		}
		for j, scene := range chapter.Scenes {
			if scene.IsLegacy() || contract.Validate(scene) != nil {
				return VerifyApplyResult{}, fmt.Errorf("applied scene invalid at ch-%02d/s-%02d", i+1, j+1)
			}
			scenes++
		}
	}
	if scenes != 157 {
		return VerifyApplyResult{}, fmt.Errorf("applied scene count = %d, want 157", scenes)
	}
	var progress domain.Progress
	if data, err := readPlainFileWithin(bookAbs, "meta/progress.json"); err != nil || strictJSON(data, &progress) != nil || progress.TotalChapters != 34 || progress.CurrentChapter != 35 || progress.LatestCompleted() != 34 {
		return VerifyApplyResult{}, fmt.Errorf("applied progress boundary is invalid")
	}
	var compass domain.StoryCompass
	if data, err := readPlainFileWithin(bookAbs, "meta/compass.json"); err != nil || strictJSON(data, &compass) != nil || compass.Current == nil || compass.Current.LastUpdated != 34 || !strings.Contains(compass.Current.Direction, "第35章及以后未规划") {
		return VerifyApplyResult{}, fmt.Errorf("applied short compass does not require chapter-35 replanning")
	}
	var marker projectprofile.ProfileMarker
	if data, err := readPlainFileWithin(bookAbs, "meta/project_profile.json"); err != nil || strictJSON(data, &marker) != nil {
		return VerifyApplyResult{}, fmt.Errorf("applied marker is invalid")
	}
	fp, err := projectprofile.NewStoreFingerprinter(bookAbs)()
	if err != nil || marker.Version != projectprofile.ProfileVersion || marker.Contract != projectprofile.ContractSceneBeatV3.String() || marker.Status != projectprofile.StatusActive.String() || marker.ProfileID != projectprofile.SceneBeatV3ProfileID || marker.EnrollmentFingerprint == nil || !applyFingerprintEqual(*marker.EnrollmentFingerprint, fp) || marker.ApprovedManifestSHA256 != receipt.ApprovedManifestSHA256 {
		return VerifyApplyResult{}, fmt.Errorf("active marker audit does not match the applied enrolled source")
	}
	var expected *projectprofile.Fingerprint
	if len(expectedEnrolled) > 0 {
		expected = expectedEnrolled[0]
	}
	registry := projectprofile.NewRegistry(
		func() (*projectprofile.ProfileMarker, error) { return &marker, nil },
		projectprofile.NewStoreFingerprinter(bookAbs), expected,
	)
	resolved, err := registry.Resolve()
	if err != nil || resolved.Contract != projectprofile.ContractSceneBeatV3 || resolved.Status != projectprofile.StatusActive {
		return VerifyApplyResult{}, fmt.Errorf("production profile registry rejected applied active marker: %v", err)
	}
	return VerifyApplyResult{ReceiptSHA256: receiptHash, KeptChapters: 34, KeptScenes: scenes}, nil
}

func exactReceiptHashPaths(hashes []FileHash, expected map[string]bool) bool {
	if len(hashes) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, hash := range hashes {
		if !expected[hash.Path] || seen[hash.Path] {
			return false
		}
		seen[hash.Path] = true
	}
	return true
}

func applyFingerprintEqual(a, b projectprofile.Fingerprint) bool {
	return a.PremiseHash == b.PremiseHash && a.ChaptersHash == b.ChaptersHash && a.CompletedThrough == b.CompletedThrough
}

func verifyReceiptHashes(root, prefix string, hashes []FileHash) error {
	seen := map[string]bool{}
	for _, want := range hashes {
		if seen[want.Path] || want.Path == "" {
			return fmt.Errorf("duplicate/empty apply receipt hash path")
		}
		seen[want.Path] = true
		got, err := hashFileWithin(root, prefix+"/"+want.Path)
		if err != nil || got.SHA256 != want.SHA256 || got.Size != want.Size {
			return fmt.Errorf("apply receipt hash mismatch for %s", want.Path)
		}
	}
	return nil
}

func truncateReviewedProposal(source *sourceBook, proposed []domain.OutlineEntry, proposedVolumes []domain.VolumeOutline, keep int) ([]domain.OutlineEntry, []domain.VolumeOutline, []int, int, error) {
	if len(proposed) != len(source.Outline) || keep > len(proposed) {
		return nil, nil, nil, 0, fmt.Errorf("reviewed proposal/source chapter count mismatch")
	}
	outline := append([]domain.OutlineEntry(nil), proposed[:keep]...)
	contract := newV3Contract()
	scenes := 0
	for i, chapter := range outline {
		src := source.Outline[i]
		if chapter.Chapter != i+1 || chapter.Title != src.Title || chapter.CoreEvent != src.CoreEvent || chapter.Hook != src.Hook || len(chapter.Scenes) != len(src.Scenes) {
			return nil, nil, nil, 0, fmt.Errorf("reviewed proposal changed chapter metadata/count at chapter %d", i+1)
		}
		for j, scene := range chapter.Scenes {
			if scene.IsLegacy() || contract.Validate(scene) != nil {
				return nil, nil, nil, 0, fmt.Errorf("chapter %d scene %d is not valid v3", i+1, j+1)
			}
			scenes++
		}
	}
	if scenes != 157 {
		return nil, nil, nil, 0, fmt.Errorf("kept scene count = %d, want 157", scenes)
	}
	var volumes []domain.VolumeOutline
	for _, volume := range proposedVolumes {
		copyVolume := volume
		copyVolume.Arcs = nil
		for _, arc := range volume.Arcs {
			var chapters []domain.OutlineEntry
			for _, chapter := range arc.Chapters {
				if chapter.Chapter <= keep {
					chapters = append(chapters, chapter)
				}
			}
			if len(chapters) == 0 {
				continue
			}
			copyArc := arc
			copyArc.Chapters = chapters
			copyArc.EstimatedChapters = 0
			copyVolume.Arcs = append(copyVolume.Arcs, copyArc)
		}
		if len(copyVolume.Arcs) > 0 {
			copyVolume.Final = false
			volumes = append(volumes, copyVolume)
		}
	}
	if err := store.ValidateVolumesLayeredOutline(volumes); err != nil {
		return nil, nil, nil, 0, fmt.Errorf("truncated volumes: %w", err)
	}
	flat := domain.FlattenOutline(volumes)
	equal, err := jsonSemanticallyEqual(outline, flat)
	if err != nil || !equal || len(volumes) == 0 {
		return nil, nil, nil, 0, fmt.Errorf("truncated flat/layered proposal mismatch")
	}
	removed := make([]int, 0, len(proposed)-keep)
	for _, chapter := range proposed[keep:] {
		removed = append(removed, chapter.Chapter)
	}
	return outline, volumes, removed, scenes, nil
}

func userOverrideDecision(at time.Time, manifest string, removed []int) ([]byte, error) {
	record := map[string]any{
		"schema_version": 1, "id": "dec-user-" + manifest[:12], "at": at.Format(time.RFC3339),
		"kind": "user_override", "decider": "user",
		"input":    "清除第34章以后的既有规划，让 Architect 后续重新生成",
		"facts":    map[string]any{"completed_chapters": 34, "removed_chapters": removed, "approved_manifest_sha256": manifest},
		"decision": map[string]any{"action": "replan_after_chapter", "keep_through": 34},
		"reason":   "用户条件批准 SceneBeat v3 迁移，并明确撤回第35章及以后既有规划。",
	}
	data, err := json.Marshal(record)
	return append(data, '\n'), err
}

func newApplyReceiptDir(bookDir string, now func() time.Time, random func([]byte) error) (string, error) {
	bookAbs, err := filepath.Abs(bookDir)
	if err != nil {
		return "", err
	}
	if err := ensurePlainDir(bookAbs, "book-dir"); err != nil {
		return "", err
	}
	migrations := filepath.Join(filepath.Dir(bookAbs), "migrations")
	if err := ensurePlainDir(migrations, "migrations directory"); err != nil {
		return "", err
	}
	rnd := make([]byte, 8)
	if err := random(rnd); err != nil {
		return "", err
	}
	name := "scene-beat-v3-apply-" + now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd)
	dir := filepath.Join(migrations, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	if err := ensurePlainDir(dir, "apply receipt directory"); err != nil {
		return "", err
	}
	return dir, nil
}

func fileHashForBytes(path string, data []byte) FileHash {
	return FileHash{Path: path, SHA256: hashBytes(data), Size: int64(len(data))}
}

func atomicReplaceCanonical(root, rel string, data []byte) error {
	clean, err := cleanArtifactPath(rel)
	if err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(clean))
	if err := ensureNoReparseComponents(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || isReparse(info) {
		return fmt.Errorf("canonical target is not a plain existing file: %s", rel)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scene-beat-v3-apply-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
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
	if err := ensureNoReparseComponents(filepath.Dir(path)); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func atomicCreateCanonical(root, rel string, data []byte) error {
	if err := ensureNoReparseComponents(filepath.Dir(filepath.Join(root, filepath.FromSlash(rel)))); err != nil {
		return err
	}
	return atomicCreateArtifact(root, rel, data)
}
