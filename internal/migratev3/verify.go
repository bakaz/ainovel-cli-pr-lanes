package migratev3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// Verify validates a completed review run without creating or changing files.
func Verify(opts VerifyOptions) (VerifyResult, error) {
	if opts.BookDir == "" || opts.RunDir == "" {
		return VerifyResult{}, fmt.Errorf("--book-dir and --run-dir are required")
	}
	bookDir, err := filepath.Abs(opts.BookDir)
	if err != nil {
		return VerifyResult{}, err
	}
	runDir, err := filepath.Abs(opts.RunDir)
	if err != nil {
		return VerifyResult{}, err
	}
	if err := ensureRunLocation(bookDir, runDir); err != nil {
		return VerifyResult{}, err
	}

	manifestData, err := readArtifactFile(runDir, "manifest.json")
	if err != nil {
		return VerifyResult{}, fmt.Errorf("run is incomplete (manifest missing): %w", err)
	}
	detached, err := readArtifactFile(runDir, "manifest.sha256")
	if err != nil {
		return VerifyResult{}, fmt.Errorf("run is incomplete (detached manifest hash missing): %w", err)
	}
	manifestHash := hashBytes(manifestData)
	if string(detached) != manifestHash+"  manifest.json\n" {
		return VerifyResult{}, fmt.Errorf("detached manifest hash mismatch")
	}
	if err := validateJSONLexically(manifestData); err != nil {
		return VerifyResult{}, fmt.Errorf("manifest: %w", err)
	}
	var manifest ArtifactManifest
	if err := strictJSON(manifestData, &manifest); err != nil {
		return VerifyResult{}, fmt.Errorf("manifest: %w", err)
	}
	if manifest.Protocol != ProtocolVersion || manifest.Status != "complete" {
		return VerifyResult{}, fmt.Errorf("manifest is not a complete %s run", ProtocolVersion)
	}
	if manifest.RunID != filepath.Base(runDir) {
		return VerifyResult{}, fmt.Errorf("manifest run id does not match run directory")
	}
	resolvedBook, err := filepath.EvalSymlinks(bookDir)
	if err != nil {
		return VerifyResult{}, err
	}
	if !samePath(manifest.BookDir, resolvedBook) {
		return VerifyResult{}, fmt.Errorf("manifest book directory mismatch")
	}
	if manifest.LogicalBatches != 42 || manifest.TotalScenes != 182 || manifest.LegacyScenes != 157 || manifest.StructuredScenes != 25 {
		return VerifyResult{}, fmt.Errorf("manifest source counts are invalid")
	}
	if manifest.MaxCostUSD <= 0 || manifest.MaxCostUSD > MaxApprovedCost || manifest.CostUSD < 0 || manifest.CostUSD > manifest.MaxCostUSD+1e-9 {
		return VerifyResult{}, fmt.Errorf("manifest cost accounting is invalid")
	}
	if !runIDPattern.MatchString(manifest.RunID) || manifest.CreatedAt.IsZero() || manifest.CreatedAt.Location() != time.UTC {
		return VerifyResult{}, fmt.Errorf("manifest run id/creation time is invalid")
	}
	if sanitizeLabel(manifest.Provider) != manifest.Provider || sanitizeLabel(manifest.Model) != manifest.Model || manifest.Pricing.InputUSDPerMillion < 0 || manifest.Pricing.OutputUSDPerMillion < 0 || manifest.Pricing.MaxOutputTokens <= 0 {
		return VerifyResult{}, fmt.Errorf("manifest generator descriptor is invalid")
	}
	actualFiles, actualDirs, err := listArtifactTree(runDir)
	if err != nil {
		return VerifyResult{}, err
	}
	var records []ProviderAttempt
	if err := readStrictArtifact(runDir, "provider/records.json", &records); err != nil {
		return VerifyResult{}, err
	}
	if err := validateRecordSequence(records, manifest); err != nil {
		return VerifyResult{}, err
	}
	expectedArtifacts := expectedArtifactPaths(records)
	if err := verifyArtifactDirectories(actualDirs); err != nil {
		return VerifyResult{}, err
	}
	if err := verifyArtifactSet(runDir, actualFiles, manifest.Artifacts, expectedArtifacts); err != nil {
		return VerifyResult{}, err
	}

	source, err := loadSource(bookDir, opts.ExpectedEnrolled)
	if err != nil {
		return VerifyResult{}, err
	}
	if source.Fingerprint != manifest.Fingerprint {
		return VerifyResult{}, fmt.Errorf("canonical source fingerprint drift")
	}
	if err := verifySourceSnapshot(runDir, source); err != nil {
		return VerifyResult{}, err
	}
	if err := verifyProposalsAndCandidate(runDir, source); err != nil {
		return VerifyResult{}, err
	}
	if err := verifyRecords(runDir, manifest, source, records); err != nil {
		return VerifyResult{}, err
	}
	desc := GeneratorDescriptor{Provider: manifest.Provider, Model: manifest.Model, RealProvider: manifest.ProviderCalls, Pricing: Pricing{Known: true, InputUSDPerMillion: manifest.Pricing.InputUSDPerMillion, OutputUSDPerMillion: manifest.Pricing.OutputUSDPerMillion, MaxOutputTokens: manifest.Pricing.MaxOutputTokens}}
	review, err := readArtifactFile(runDir, "REVIEW.md")
	if err != nil {
		return VerifyResult{}, err
	}
	if string(review) != renderReview(source, desc, manifest.CostUSD, manifest.MaxCostUSD) {
		return VerifyResult{}, fmt.Errorf("REVIEW.md is not the deterministic protocol rendering")
	}
	if err := source.verifyUnchanged(); err != nil {
		return VerifyResult{}, fmt.Errorf("canonical source changed during verify: %w", err)
	}
	return VerifyResult{ManifestSHA256: manifestHash, LogicalBatches: 42, TotalScenes: 182}, nil
}

var runIDPattern = regexp.MustCompile(`^scene-beat-v3-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{16}$`)

func verifyArtifactSet(runDir string, actual []string, artifacts []FileHash, expectedArtifacts map[string]bool) error {
	want := map[string]FileHash{
		"manifest.json":   {Path: "manifest.json"},
		"manifest.sha256": {Path: "manifest.sha256"},
	}
	last := ""
	for _, artifact := range artifacts {
		if _, err := cleanArtifactPath(artifact.Path); err != nil {
			return err
		}
		if artifact.Path <= last {
			return fmt.Errorf("artifact paths are duplicate or unsorted at %q", artifact.Path)
		}
		last = artifact.Path
		if _, exists := want[artifact.Path]; exists {
			return fmt.Errorf("duplicate artifact path %q", artifact.Path)
		}
		if !expectedArtifacts[artifact.Path] {
			return fmt.Errorf("artifact path is outside the fixed protocol: %s", artifact.Path)
		}
		if artifact.Size < 0 || !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("artifact hash metadata is invalid: %s", artifact.Path)
		}
		want[artifact.Path] = artifact
	}
	if len(artifacts) != len(expectedArtifacts) {
		return fmt.Errorf("manifest artifact protocol set mismatch: got %d want %d", len(artifacts), len(expectedArtifacts))
	}
	for rel := range expectedArtifacts {
		if _, ok := want[rel]; !ok {
			return fmt.Errorf("manifest omits required artifact %s", rel)
		}
	}
	if len(actual) != len(want) {
		return fmt.Errorf("artifact file set mismatch: got %d files, want %d", len(actual), len(want))
	}
	for _, rel := range actual {
		expected, ok := want[rel]
		if !ok {
			return fmt.Errorf("unexpected artifact %s", rel)
		}
		if rel == "manifest.json" || rel == "manifest.sha256" {
			continue
		}
		got, err := hashFileWithin(runDir, rel)
		if err != nil {
			return err
		}
		if got.SHA256 != expected.SHA256 || got.Size != expected.Size {
			return fmt.Errorf("artifact tampered: %s", rel)
		}
	}
	return nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func expectedArtifactPaths(records []ProviderAttempt) map[string]bool {
	want := map[string]bool{
		"REVIEW.md": true, "audit.json": true, "review.diff.json": true,
		"source_manifest.json": true, "provider/records.json": true,
		"proposed/outline.json": true, "proposed/outline.md": true,
		"proposed/layered_outline.json": true, "proposed/layered_outline.md": true,
	}
	for _, rel := range approvedSourcePaths() {
		want["source_snapshot/"+rel] = true
	}
	for chapter := 1; chapter <= 42; chapter++ {
		want[fmt.Sprintf("proposals/ch-%02d.json", chapter)] = true
	}
	for _, record := range records {
		want[fmt.Sprintf("provider/exchanges/ch-%02d-attempt-%02d.json", record.Chapter, record.Attempt)] = true
	}
	return want
}

func verifyArtifactDirectories(actual []string) error {
	want := []string{"proposals", "proposed", "provider", "provider/exchanges", "source_snapshot", "source_snapshot/chapters"}
	if len(actual) != len(want) {
		return fmt.Errorf("artifact directory protocol set mismatch: got %v want %v", actual, want)
	}
	for i := range want {
		if actual[i] != want[i] {
			return fmt.Errorf("artifact directory protocol set mismatch: got %v want %v", actual, want)
		}
	}
	return nil
}

func verifySourceSnapshot(runDir string, source *sourceBook) error {
	data, err := readArtifactFile(runDir, "source_manifest.json")
	if err != nil {
		return err
	}
	if err := validateJSONLexically(data); err != nil {
		return fmt.Errorf("source manifest: %w", err)
	}
	var manifest SourceManifest
	if err := strictJSON(data, &manifest); err != nil {
		return fmt.Errorf("source manifest: %w", err)
	}
	if manifest.Protocol != ProtocolVersion || !samePath(manifest.BookDir, source.BookDir) || manifest.Fingerprint != source.Fingerprint {
		return fmt.Errorf("source manifest identity mismatch")
	}
	equal, err := jsonSemanticallyEqual(source.Hashes, manifest.Files)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("source manifest hashes differ from canonical source")
	}
	for _, file := range source.Hashes {
		data, err := readArtifactFile(runDir, "source_snapshot/"+file.Path)
		if err != nil {
			return err
		}
		if hashBytes(data) != file.SHA256 || int64(len(data)) != file.Size || !bytes.Equal(data, source.Files[file.Path]) {
			return fmt.Errorf("source snapshot mismatch: %s", file.Path)
		}
	}
	return nil
}

func verifyProposalsAndCandidate(runDir string, source *sourceBook) error {
	accepted := make([]domain.OutlineEntry, 0, 42)
	expectedDiff := make([]ReviewDiffEntry, 0, 182)
	for _, chapter := range source.Outline {
		data, err := readArtifactFile(runDir, fmt.Sprintf("proposals/ch-%02d.json", chapter.Chapter))
		if err != nil {
			return err
		}
		merged, chapterDiff, _, err := mergeProposal(chapter, buildChapterRequest(chapter), data)
		if err != nil {
			return fmt.Errorf("proposal ch-%02d: %w", chapter.Chapter, err)
		}
		accepted = append(accepted, merged)
		expectedDiff = append(expectedDiff, chapterDiff...)
	}
	outlineData, err := readArtifactFile(runDir, "proposed/outline.json")
	if err != nil {
		return err
	}
	if err := validateJSONLexically(outlineData); err != nil {
		return err
	}
	if err := validateOutlineShape(outlineData); err != nil {
		return err
	}
	var outline []domain.OutlineEntry
	if err := strictJSON(outlineData, &outline); err != nil {
		return err
	}
	layeredData, err := readArtifactFile(runDir, "proposed/layered_outline.json")
	if err != nil {
		return err
	}
	if err := validateJSONLexically(layeredData); err != nil {
		return err
	}
	if err := validateLayeredShape(layeredData); err != nil {
		return err
	}
	var volumes []domain.VolumeOutline
	if err := strictJSON(layeredData, &volumes); err != nil {
		return err
	}
	equal, err := jsonSemanticallyEqual(accepted, outline)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("proposed outline does not equal mechanical proposal merge")
	}
	if err := validateCandidateAgainstSource(source, outline, volumes); err != nil {
		return err
	}
	if stringMust(readArtifactFile(runDir, "proposed/outline.md")) != renderOutline(outline) {
		return fmt.Errorf("proposed outline markdown is not deterministic")
	}
	if stringMust(readArtifactFile(runDir, "proposed/layered_outline.md")) != renderLayeredOutline(volumes) {
		return fmt.Errorf("proposed layered markdown is not deterministic")
	}
	var diff []ReviewDiffEntry
	if err := readStrictArtifact(runDir, "review.diff.json", &diff); err != nil {
		return err
	}
	equal, err = jsonSemanticallyEqual(expectedDiff, diff)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("review diff does not equal deterministic mechanical merge diff")
	}
	return nil
}

func verifyRecords(runDir string, manifest ArtifactManifest, source *sourceBook, records []ProviderAttempt) error {
	pricing := Pricing{Known: true, InputUSDPerMillion: manifest.Pricing.InputUSDPerMillion, OutputUSDPerMillion: manifest.Pricing.OutputUSDPerMillion, MaxOutputTokens: manifest.Pricing.MaxOutputTokens}
	totalCost := 0.0
	for _, record := range records {
		var exchange providerExchange
		rel := fmt.Sprintf("provider/exchanges/ch-%02d-attempt-%02d.json", record.Chapter, record.Attempt)
		if err := readStrictArtifact(runDir, rel, &exchange); err != nil {
			return err
		}
		if exchange.Chapter != record.Chapter || exchange.Attempt != record.Attempt {
			return fmt.Errorf("provider exchange identity mismatch")
		}
		if exchange.Record != record {
			return fmt.Errorf("provider exchange embedded record mismatch for chapter %d attempt %d", record.Chapter, record.Attempt)
		}
		wantReq, err := json.Marshal(buildChapterRequest(source.Outline[record.Chapter-1]))
		if err != nil {
			return err
		}
		gotReq, err := json.Marshal(exchange.Request)
		if err != nil {
			return err
		}
		if !bytes.Equal(wantReq, gotReq) {
			return fmt.Errorf("provider exchange request mismatch for chapter %d attempt %d", record.Chapter, record.Attempt)
		}
		if exchange.OutboundBody == "" || len([]byte(exchange.OutboundBody)) != record.ReservedInputTokens {
			return fmt.Errorf("outbound body/input reservation mismatch for chapter %d attempt %d", record.Chapter, record.Attempt)
		}
		if manifest.Provider == "offline-fake" && exchange.OutboundBody != string(gotReq) {
			return fmt.Errorf("offline fake outbound body differs from protocol request")
		}
		reserved := estimateWorstCost(pricing, record.ReservedInputTokens)
		cost := actualCost(pricing, Usage{Known: true, InputTokens: record.InputTokens, OutputTokens: record.OutputTokens})
		if !floatEqual(record.ReservedUSD, reserved) || !floatEqual(record.CostUSD, cost) {
			return fmt.Errorf("provider cost/reservation mismatch for chapter %d attempt %d", record.Chapter, record.Attempt)
		}
		totalCost += cost
		if len(exchange.Response) > 0 && exchange.ResponseText != "" {
			return fmt.Errorf("provider exchange has both JSON and text responses")
		}
		if len(exchange.ProviderResponse) > 0 && exchange.ProviderResponseText != "" {
			return fmt.Errorf("provider exchange has both JSON and text wire responses")
		}
		var response []byte
		if len(exchange.Response) > 0 {
			response = exchange.Response
		} else {
			response = []byte(exchange.ResponseText)
		}
		var wireResponse []byte
		if len(exchange.ProviderResponse) > 0 {
			wireResponse = exchange.ProviderResponse
		} else {
			wireResponse = []byte(exchange.ProviderResponseText)
		}
		if manifest.ProviderCalls {
			if len(wireResponse) == 0 {
				if record.ErrorCategory != "generator_error" {
					return fmt.Errorf("real Provider exchange has no wire response for chapter %d attempt %d", record.Chapter, record.Attempt)
				}
			} else if normalized, normalizeErr := normalizeProviderValueResponse(exchange.Request, wireResponse); record.ErrorCategory == "generator_error" {
				if normalizeErr == nil {
					return fmt.Errorf("generator_error record has a valid Provider wire response")
				}
			} else {
				if normalizeErr != nil {
					return fmt.Errorf("accepted Provider wire response cannot be normalized: %w", normalizeErr)
				}
				var normalizedEnv, responseEnv proposalEnvelope
				if err := strictJSON(normalized, &normalizedEnv); err != nil {
					return fmt.Errorf("deterministic Provider normalization is invalid: %w", err)
				}
				if err := strictJSON(response, &responseEnv); err != nil {
					return fmt.Errorf("persisted normalized Provider response is invalid: %w", err)
				}
				equal, err := jsonSemanticallyEqual(normalizedEnv, responseEnv)
				if err != nil || !equal {
					return fmt.Errorf("Provider wire response differs from deterministic normalization")
				}
			}
		} else if len(wireResponse) != 0 {
			return fmt.Errorf("offline exchange unexpectedly contains a Provider wire response")
		}
		_, _, _, mergeErr := mergeProposal(source.Outline[record.Chapter-1], exchange.Request, response)
		switch record.ErrorCategory {
		case "":
			if record.FinishReason == "" || record.FinishReason == "length" {
				return fmt.Errorf("successful provider record has non-terminal finish reason %q", record.FinishReason)
			}
			if mergeErr != nil {
				return fmt.Errorf("successful provider record has invalid response: %w", mergeErr)
			}
			proposal, err := readArtifactFile(runDir, fmt.Sprintf("proposals/ch-%02d.json", record.Chapter))
			if err != nil {
				return err
			}
			var responseEnv, proposalEnv proposalEnvelope
			if strictJSON(response, &responseEnv) != nil || strictJSON(proposal, &proposalEnv) != nil {
				return fmt.Errorf("accepted response/proposal decode mismatch")
			}
			equal, err := jsonSemanticallyEqual(responseEnv, proposalEnv)
			if err != nil {
				return err
			}
			if !equal {
				return fmt.Errorf("accepted provider response differs from normalized proposal for chapter %d", record.Chapter)
			}
		case "protocol_error":
			if mergeErr == nil {
				return fmt.Errorf("protocol_error record contains a valid response")
			}
		case "generator_error":
			// Transport/provider errors may have no parseable response.
		default:
			return fmt.Errorf("unknown provider error category %q", record.ErrorCategory)
		}
	}
	if !floatEqual(totalCost, manifest.CostUSD) {
		return fmt.Errorf("manifest total cost differs from provider records")
	}
	var audit []AuditEntry
	if err := readStrictArtifact(runDir, "audit.json", &audit); err != nil {
		return err
	}
	if len(audit) != 42 {
		return fmt.Errorf("audit must contain 42 chapter entries")
	}
	for i, entry := range audit {
		chapter := source.Outline[i]
		legacy, filled, attempts := 0, 0, 0
		for _, scene := range chapter.Scenes {
			if scene.IsLegacy() {
				legacy++
			}
		}
		for _, request := range buildChapterRequest(chapter).Scenes {
			filled += len(request.MissingFields)
		}
		for _, record := range records {
			if record.Chapter == chapter.Chapter {
				attempts++
			}
		}
		if entry.Chapter != chapter.Chapter || entry.SceneCount != len(chapter.Scenes) || entry.LegacyScenes != legacy || entry.StructuredScenes != len(chapter.Scenes)-legacy || entry.FilledFields != filled || entry.Attempts != attempts {
			return fmt.Errorf("audit mismatch at chapter %d", chapter.Chapter)
		}
	}
	return nil
}

func validateRecordSequence(records []ProviderAttempt, manifest ArtifactManifest) error {
	if len(records) != manifest.Attempts || len(records) < 42 || len(records) > 126 {
		return fmt.Errorf("provider attempt count mismatch")
	}
	index := 0
	for chapter := 1; chapter <= 42; chapter++ {
		attempts := 0
		for index < len(records) && records[index].Chapter == chapter {
			record := records[index]
			attempts++
			if attempts > 3 || record.Attempt != attempts || record.Retry != (attempts > 1) {
				return fmt.Errorf("provider attempt sequence invalid at chapter %d", chapter)
			}
			if record.Provider != manifest.Provider || record.Model != manifest.Model || !record.UsageKnown || record.InputTokens < 0 || record.ReservedInputTokens <= 0 || record.InputTokens > record.ReservedInputTokens || record.OutputTokens < 0 || record.OutputTokens > manifest.Pricing.MaxOutputTokens || record.CostUSD < 0 || record.ReservedUSD < 0 || sanitizeFinishReason(record.FinishReason) != record.FinishReason || (record.ErrorDetail != "" && sanitizeRetryFeedback(record.ErrorDetail) != record.ErrorDetail) {
				return fmt.Errorf("provider attempt metadata invalid at chapter %d", chapter)
			}
			if record.ErrorCategory == "" && (index+1 < len(records) && records[index+1].Chapter == chapter) {
				return fmt.Errorf("provider attempts continue after success at chapter %d", chapter)
			}
			index++
		}
		if attempts == 0 || records[index-1].ErrorCategory != "" {
			return fmt.Errorf("chapter %d has no final successful provider attempt", chapter)
		}
	}
	if index != len(records) {
		return fmt.Errorf("provider records are out of chapter order")
	}
	return nil
}

func floatEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}

func readStrictArtifact(runDir, rel string, out any) error {
	data, err := readArtifactFile(runDir, rel)
	if err != nil {
		return err
	}
	if err := validateJSONLexically(data); err != nil {
		return err
	}
	return strictJSON(data, out)
}

func readStrictFile(path string, out any) error {
	data, err := readPlainFile(path)
	if err != nil {
		return err
	}
	if err := validateJSONLexically(data); err != nil {
		return err
	}
	return strictJSON(data, out)
}

func readArtifactFile(runDir, rel string) ([]byte, error) {
	return readPlainFileWithin(runDir, rel)
}

func stringMust(data []byte, err error) string {
	if err != nil {
		return ""
	}
	return string(data)
}
