package migratev3

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

type providerExchange struct {
	Chapter              int             `json:"chapter"`
	Attempt              int             `json:"attempt"`
	Record               ProviderAttempt `json:"record"`
	Request              ChapterRequest  `json:"request"`
	OutboundBody         string          `json:"outbound_body"`
	ProviderResponse     json.RawMessage `json:"provider_response,omitempty"`
	ProviderResponseText string          `json:"provider_response_text,omitempty"`
	Response             json.RawMessage `json:"response,omitempty"`
	ResponseText         string          `json:"response_text,omitempty"`
}

// Preflight performs the complete source/config/pricing/reservation validation
// without creating a run directory or invoking Generator.Generate.
func Preflight(opts DraftOptions) (PreflightResult, error) {
	if opts.Generator == nil {
		return PreflightResult{}, fmt.Errorf("generator is required")
	}
	desc := opts.Generator.Descriptor()
	if err := validateDescriptor(desc); err != nil {
		return PreflightResult{}, err
	}
	if opts.AllowProviderCalls {
		if opts.ProviderConfigPath == "" || !desc.RealProvider {
			return PreflightResult{}, fmt.Errorf("real Provider calls require --provider-config and a reviewed real adapter")
		}
	} else if opts.ProviderConfigPath != "" || desc.RealProvider {
		return PreflightResult{}, fmt.Errorf("real Provider configuration/adapter requires explicit --allow-provider-calls")
	}
	if opts.BookDir == "" {
		return PreflightResult{}, fmt.Errorf("--book-dir is required")
	}
	if opts.MaxCostUSD == 0 {
		opts.MaxCostUSD = MaxApprovedCost
	}
	if opts.MaxCostUSD <= 0 || opts.MaxCostUSD > MaxApprovedCost {
		return PreflightResult{}, fmt.Errorf("max cost must be > 0 and <= %.2f USD", MaxApprovedCost)
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 2
	}
	if opts.MaxAttempts < 1 || opts.MaxAttempts > 3 {
		return PreflightResult{}, fmt.Errorf("max attempts must be between 1 and 3")
	}
	source, err := loadSource(opts.BookDir, opts.ExpectedEnrolled)
	if err != nil {
		return PreflightResult{}, err
	}
	worst := 0.0
	for _, chapter := range source.Outline {
		req := buildChapterRequest(chapter)
		for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
			feedback := ""
			if attempt > 1 {
				feedback = strings.Repeat("X", maxRetryFeedbackBytes)
			}
			prepared, err := prepareGeneratorAttempt(opts.Generator, req, attempt, feedback)
			if err != nil {
				return PreflightResult{}, err
			}
			bound := len([]byte(prepared.OutboundBody))
			if bound <= 0 {
				return PreflightResult{}, fmt.Errorf("chapter %d attempt %d has unknown input reservation", chapter.Chapter, attempt)
			}
			worst += estimateWorstCost(desc.Pricing, bound)
		}
	}
	if worst > opts.MaxCostUSD+1e-9 {
		return PreflightResult{}, fmt.Errorf("worst-case complete run cost %.6f exceeds max %.2f", worst, opts.MaxCostUSD)
	}
	return PreflightResult{Provider: desc.Provider, Model: desc.Model, LogicalBatches: len(source.Outline), MaxAttempts: opts.MaxAttempts, WorstCaseCostUSD: worst}, nil
}

func Draft(ctx context.Context, opts DraftOptions) (DraftResult, error) {
	if opts.Generator == nil {
		return DraftResult{}, fmt.Errorf("generator is required")
	}
	desc := opts.Generator.Descriptor()
	if err := validateDescriptor(desc); err != nil {
		return DraftResult{}, err
	}
	if opts.AllowProviderCalls {
		if opts.ProviderConfigPath == "" || !desc.RealProvider {
			return DraftResult{}, fmt.Errorf("real Provider calls require --provider-config and a reviewed real adapter")
		}
	} else {
		if opts.ProviderConfigPath != "" {
			return DraftResult{}, fmt.Errorf("Stage 3a fake mode rejects --provider-config; no non-whitelisted configuration is read")
		}
		if desc.RealProvider {
			return DraftResult{}, fmt.Errorf("real Provider adapter requires explicit --allow-provider-calls")
		}
	}
	if opts.BookDir == "" {
		return DraftResult{}, fmt.Errorf("--book-dir is required")
	}
	if opts.MaxCostUSD == 0 {
		opts.MaxCostUSD = MaxApprovedCost
	}
	if opts.MaxCostUSD <= 0 || opts.MaxCostUSD > MaxApprovedCost {
		return DraftResult{}, fmt.Errorf("max cost must be > 0 and <= %.2f USD", MaxApprovedCost)
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 2
	}
	if opts.MaxAttempts < 1 || opts.MaxAttempts > 3 {
		return DraftResult{}, fmt.Errorf("max attempts must be between 1 and 3")
	}
	if opts.Now == nil {
		opts.Now = defaultNow
	}
	if opts.RandomBytes == nil {
		opts.RandomBytes = defaultRandom
	}
	source, err := loadSource(opts.BookDir, opts.ExpectedEnrolled)
	if err != nil {
		return DraftResult{}, err
	}
	runDir, runID, err := newRunDir(source.BookDir, opts.Now, opts.RandomBytes)
	if err != nil {
		return DraftResult{}, err
	}
	result := DraftResult{RunDir: runDir}
	if err := source.copySnapshot(runDir); err != nil {
		return result, err
	}

	proposed := make([]domain.OutlineEntry, 0, len(source.Outline))
	var audit []AuditEntry
	var diffs []ReviewDiffEntry
	var records []ProviderAttempt
	spent := 0.0
	totalAttempts := 0

	for _, chapter := range source.Outline {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("draft cancelled before chapter %d: %w", chapter.Chapter, err)
		}
		req := buildChapterRequest(chapter)
		var accepted domain.OutlineEntry
		var chapterDiff []ReviewDiffEntry
		filled := 0
		var lastErr error
		acceptedAttempt := 0
		retryFeedback := ""

		for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
			prepared, err := prepareGeneratorAttempt(opts.Generator, req, attempt, retryFeedback)
			if err != nil {
				return result, fmt.Errorf("prepare generator request for chapter %d attempt %d: %w", chapter.Chapter, attempt, err)
			}
			inputUpperBound := len([]byte(prepared.OutboundBody))
			if inputUpperBound <= 0 {
				return result, fmt.Errorf("input token upper bound for chapter %d attempt %d is unknown", chapter.Chapter, attempt)
			}
			reserved := estimateWorstCost(desc.Pricing, inputUpperBound)
			if spent+reserved > opts.MaxCostUSD+1e-9 {
				return result, fmt.Errorf("cost stop before chapter %d attempt %d: spent %.6f + reserved %.6f > max %.2f",
					chapter.Chapter, attempt, spent, reserved, opts.MaxCostUSD)
			}
			generated, genErr := opts.Generator.Generate(ctx, req, prepared, attempt)
			totalAttempts++
			record := ProviderAttempt{
				Chapter: chapter.Chapter, Attempt: attempt,
				Provider: sanitizeLabel(desc.Provider), Model: sanitizeLabel(desc.Model), UsageKnown: generated.Usage.Known,
				InputTokens: generated.Usage.InputTokens, ReservedInputTokens: inputUpperBound, OutputTokens: generated.Usage.OutputTokens,
				ReservedUSD: reserved, Retry: attempt > 1, FinishReason: sanitizeFinishReason(generated.FinishReason),
			}
			persistExchange := func() error {
				exchange := providerExchange{Chapter: chapter.Chapter, Attempt: attempt, Record: record, Request: req, OutboundBody: prepared.OutboundBody}
				if json.Valid(generated.ProviderResponse) {
					exchange.ProviderResponse = append(json.RawMessage(nil), generated.ProviderResponse...)
				} else {
					exchange.ProviderResponseText = string(generated.ProviderResponse)
				}
				if json.Valid(generated.RawResponse) {
					exchange.Response = append(json.RawMessage(nil), generated.RawResponse...)
				} else {
					exchange.ResponseText = string(generated.RawResponse)
				}
				exchangeData, err := marshalJSON(exchange)
				if err != nil {
					return err
				}
				exchangePath := fmt.Sprintf("provider/exchanges/ch-%02d-attempt-%02d.json", chapter.Chapter, attempt)
				return atomicCreateArtifact(runDir, exchangePath, exchangeData)
			}
			if !generated.Usage.Known || generated.Usage.InputTokens < 0 || generated.Usage.OutputTokens < 0 {
				record.ErrorCategory = "usage_error"
				if err := persistExchange(); err != nil {
					return result, err
				}
				return result, fmt.Errorf("unknown usage for chapter %d attempt %d", chapter.Chapter, attempt)
			}
			if generated.Usage.OutputTokens > desc.Pricing.MaxOutputTokens {
				record.ErrorCategory = "usage_error"
				if err := persistExchange(); err != nil {
					return result, err
				}
				return result, fmt.Errorf("output token budget exceeded for chapter %d attempt %d", chapter.Chapter, attempt)
			}
			if generated.Usage.InputTokens > inputUpperBound {
				record.ErrorCategory = "usage_error"
				if err := persistExchange(); err != nil {
					return result, err
				}
				return result, fmt.Errorf("input token upper bound exceeded for chapter %d attempt %d: used %d > reserved %d", chapter.Chapter, attempt, generated.Usage.InputTokens, inputUpperBound)
			}
			cost := actualCost(desc.Pricing, generated.Usage)
			record.CostUSD = cost
			spent += cost
			if spent > opts.MaxCostUSD+1e-9 {
				record.ErrorCategory = "cost_error"
				if err := persistExchange(); err != nil {
					return result, err
				}
				return result, fmt.Errorf("actual cost exceeded max after chapter %d attempt %d", chapter.Chapter, attempt)
			}
			if genErr != nil {
				record.ErrorCategory = "generator_error"
			}
			if genErr == nil {
				finish := sanitizeFinishReason(generated.FinishReason)
				if finish == "" || finish == "length" || finish == "redacted" {
					lastErr = fmt.Errorf("non-terminal/unsafe finish reason %q", finish)
					record.ErrorCategory = "protocol_error"
				} else {
					accepted, chapterDiff, filled, lastErr = mergeProposal(chapter, req, generated.RawResponse)
					if lastErr != nil {
						record.ErrorCategory = "protocol_error"
					}
				}
			} else {
				lastErr = genErr
			}
			if lastErr != nil {
				retryFeedback = sanitizeRetryFeedback(lastErr.Error())
				record.ErrorDetail = retryFeedback
			}
			if err := persistExchange(); err != nil {
				return result, err
			}
			records = append(records, record)
			if lastErr == nil {
				acceptedAttempt = attempt
				var normalized proposalEnvelope
				if err := strictJSON(generated.RawResponse, &normalized); err != nil {
					return result, err
				}
				proposalData, err := marshalJSON(normalized)
				if err != nil {
					return result, err
				}
				if err := atomicCreateArtifact(runDir, fmt.Sprintf("proposals/ch-%02d.json", chapter.Chapter), proposalData); err != nil {
					return result, err
				}
				break
			}
		}
		if acceptedAttempt == 0 {
			return result, fmt.Errorf("chapter %d failed after %d attempts: %w", chapter.Chapter, opts.MaxAttempts, lastErr)
		}
		legacy := 0
		for _, scene := range chapter.Scenes {
			if scene.IsLegacy() {
				legacy++
			}
		}
		proposed = append(proposed, accepted)
		diffs = append(diffs, chapterDiff...)
		audit = append(audit, AuditEntry{
			Chapter: chapter.Chapter, SceneCount: len(chapter.Scenes), LegacyScenes: legacy,
			StructuredScenes: len(chapter.Scenes) - legacy, FilledFields: filled, Attempts: acceptedAttempt,
		})
		if opts.AfterBatch != nil {
			opts.AfterBatch(chapter.Chapter)
		}
	}
	if len(proposed) != 42 {
		return result, fmt.Errorf("logical batch count = %d, want 42", len(proposed))
	}

	volumes, err := replaceExpandedChapters(source.Volumes, proposed)
	if err != nil {
		return result, err
	}
	if err := validateCandidateAgainstSource(source, proposed, volumes); err != nil {
		return result, err
	}
	if err := writeReviewArtifacts(runDir, proposed, volumes, records, audit, diffs, source, desc, spent, opts.MaxCostUSD); err != nil {
		return result, err
	}
	if err := source.verifyUnchanged(); err != nil {
		return result, err // deliberately no final manifest for stale runs
	}

	artifactFiles, artifactDirs, err := listArtifactTree(runDir)
	if err != nil {
		return result, err
	}
	expectedArtifacts := expectedArtifactPaths(records)
	if err := verifyArtifactDirectories(artifactDirs); err != nil {
		return result, err
	}
	if len(artifactFiles) != len(expectedArtifacts) {
		return result, fmt.Errorf("draft artifact protocol set mismatch: got %d want %d", len(artifactFiles), len(expectedArtifacts))
	}
	for _, rel := range artifactFiles {
		if !expectedArtifacts[rel] {
			return result, fmt.Errorf("draft produced artifact outside fixed protocol: %s", rel)
		}
	}
	var artifacts []FileHash
	for _, rel := range artifactFiles {
		if rel == "manifest.json" || rel == "manifest.sha256" {
			continue
		}
		h, err := hashFileWithin(runDir, rel)
		if err != nil {
			return result, err
		}
		h.Path = rel
		artifacts = append(artifacts, h)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	manifest := ArtifactManifest{
		Protocol: ProtocolVersion, Status: "complete", RunID: runID, BookDir: source.BookDir,
		CreatedAt: opts.Now().UTC(), Fingerprint: source.Fingerprint, LogicalBatches: len(proposed),
		TotalScenes: source.TotalScenes, LegacyScenes: source.LegacyScenes, StructuredScenes: source.Structured,
		Attempts: totalAttempts, CostUSD: spent, MaxCostUSD: opts.MaxCostUSD, Artifacts: artifacts,
		Provider: sanitizeLabel(desc.Provider), Model: sanitizeLabel(desc.Model),
		Pricing:       ManifestPricing{InputUSDPerMillion: desc.Pricing.InputUSDPerMillion, OutputUSDPerMillion: desc.Pricing.OutputUSDPerMillion, MaxOutputTokens: desc.Pricing.MaxOutputTokens},
		ProviderCalls: desc.RealProvider,
	}
	manifestData, err := marshalJSON(manifest)
	if err != nil {
		return result, err
	}
	if err := atomicCreateArtifact(runDir, "manifest.json", manifestData); err != nil {
		return result, err
	}
	manifestHash := hashBytes(manifestData)
	if err := atomicCreateArtifact(runDir, "manifest.sha256", []byte(manifestHash+"  manifest.json\n")); err != nil {
		return result, err
	}
	result.ManifestSHA256 = manifestHash
	result.CostUSD = spent
	return result, nil
}

func prepareGeneratorAttempt(generator Generator, req ChapterRequest, attempt int, retryFeedback string) (PreparedRequest, error) {
	if attempt > 1 {
		if retry, ok := generator.(RetryPreparer); ok {
			return retry.PrepareRetry(req, attempt, retryFeedback)
		}
	}
	return generator.Prepare(req)
}

const maxRetryFeedbackBytes = 240

func sanitizeRetryFeedback(value string) string {
	var b strings.Builder
	for _, r := range value {
		if b.Len() >= maxRetryFeedbackBytes {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune(" _./:-\"", r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "protocol validation failed"
	}
	return out
}

func validateDescriptor(desc GeneratorDescriptor) error {
	if sanitizeLabel(desc.Provider) == "redacted" || sanitizeLabel(desc.Model) == "redacted" {
		return fmt.Errorf("generator provider/model labels are unsafe")
	}
	p := desc.Pricing
	if !p.Known || p.InputUSDPerMillion < 0 || p.OutputUSDPerMillion < 0 || p.MaxOutputTokens <= 0 {
		return fmt.Errorf("generator pricing/output budget is unknown")
	}
	return nil
}

func estimateWorstCost(p Pricing, inputTokens int) float64 {
	return float64(inputTokens)*p.InputUSDPerMillion/1_000_000 + float64(p.MaxOutputTokens)*p.OutputUSDPerMillion/1_000_000
}

func actualCost(p Pricing, usage Usage) float64 {
	return float64(usage.InputTokens)*p.InputUSDPerMillion/1_000_000 + float64(usage.OutputTokens)*p.OutputUSDPerMillion/1_000_000
}

var safeLabel = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

func sanitizeLabel(value string) string {
	if !safeLabel.MatchString(value) {
		return "redacted"
	}
	return value
}

func sanitizeFinishReason(value string) string {
	if value == "" {
		return ""
	}
	if !safeLabel.MatchString(value) {
		return "redacted"
	}
	return value
}

func replaceExpandedChapters(source []domain.VolumeOutline, chapters []domain.OutlineEntry) ([]domain.VolumeOutline, error) {
	data, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var volumes []domain.VolumeOutline
	if err := json.Unmarshal(data, &volumes); err != nil {
		return nil, err
	}
	cursor := 0
	for vi := range volumes {
		for ai := range volumes[vi].Arcs {
			for ci := range volumes[vi].Arcs[ai].Chapters {
				if cursor >= len(chapters) {
					return nil, fmt.Errorf("layered outline has more expanded chapters than flat proposal")
				}
				volumes[vi].Arcs[ai].Chapters[ci] = chapters[cursor]
				cursor++
			}
		}
	}
	if cursor != len(chapters) {
		return nil, fmt.Errorf("layered outline has %d expanded chapters, proposal has %d", cursor, len(chapters))
	}
	return volumes, nil
}

func validateCandidateAgainstSource(source *sourceBook, outline []domain.OutlineEntry, volumes []domain.VolumeOutline) error {
	if len(outline) != len(source.Outline) || len(volumes) != len(source.Volumes) {
		return fmt.Errorf("candidate hierarchy count changed")
	}
	contract := newV3Contract()
	for i, chapter := range outline {
		src := source.Outline[i]
		if chapter.Chapter != src.Chapter || chapter.Title != src.Title || chapter.CoreEvent != src.CoreEvent || chapter.Hook != src.Hook || len(chapter.Scenes) != len(src.Scenes) {
			return fmt.Errorf("candidate changed chapter metadata/order/count at chapter %d", src.Chapter)
		}
		for j, scene := range chapter.Scenes {
			if scene.IsLegacy() {
				return fmt.Errorf("candidate retained legacy scene ch-%02d/s-%02d", chapter.Chapter, j+1)
			}
			if src.Scenes[j].Action != scene.Action || src.Scenes[j].SensoryAnchor != scene.SensoryAnchor {
				return fmt.Errorf("candidate rewrote preserved action/sensory field ch-%02d/s-%02d", chapter.Chapter, j+1)
			}
			for key, value := range nonemptySceneFields(src.Scenes[j]) {
				if sceneFieldMap(scene)[key] != value {
					return fmt.Errorf("candidate rewrote preserved field %s at ch-%02d/s-%02d", key, chapter.Chapter, j+1)
				}
			}
			if err := contract.Validate(scene); err != nil {
				return fmt.Errorf("candidate ch-%02d/s-%02d: %w", chapter.Chapter, j+1, err)
			}
		}
	}
	if err := store.ValidateVolumesLayeredOutline(volumes); err != nil {
		return fmt.Errorf("candidate volumes: %w", err)
	}
	flat := domain.FlattenOutline(volumes)
	equal, err := jsonSemanticallyEqual(outline, flat)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("candidate flat outline differs from flattened proposed layered outline")
	}
	// Metadata and skeleton arcs must remain byte-semantically unchanged.
	for vi := range volumes {
		a, b := source.Volumes[vi], volumes[vi]
		if a.Index != b.Index || a.Title != b.Title || a.Theme != b.Theme || a.Final != b.Final || len(a.Arcs) != len(b.Arcs) {
			return fmt.Errorf("candidate changed volume metadata")
		}
		for ai := range a.Arcs {
			sa, sb := a.Arcs[ai], b.Arcs[ai]
			if sa.Index != sb.Index || sa.Title != sb.Title || sa.Goal != sb.Goal || sa.EstimatedChapters != sb.EstimatedChapters || len(sa.Chapters) != len(sb.Chapters) || (sa.Chapters == nil) != (sb.Chapters == nil) {
				return fmt.Errorf("candidate changed arc metadata/skeleton state")
			}
		}
	}
	return nil
}

func writeReviewArtifacts(runDir string, outline []domain.OutlineEntry, volumes []domain.VolumeOutline, records []ProviderAttempt, audit []AuditEntry, diffs []ReviewDiffEntry, source *sourceBook, desc GeneratorDescriptor, spent, maxCost float64) error {
	writes := map[string][]byte{}
	var err error
	if writes["proposed/outline.json"], err = marshalJSON(outline); err != nil {
		return err
	}
	if writes["proposed/layered_outline.json"], err = marshalJSON(volumes); err != nil {
		return err
	}
	writes["proposed/outline.md"] = []byte(renderOutline(outline))
	writes["proposed/layered_outline.md"] = []byte(renderLayeredOutline(volumes))
	if writes["provider/records.json"], err = marshalJSON(records); err != nil {
		return err
	}
	if writes["audit.json"], err = marshalJSON(audit); err != nil {
		return err
	}
	if writes["review.diff.json"], err = marshalJSON(diffs); err != nil {
		return err
	}
	writes["REVIEW.md"] = []byte(renderReview(source, desc, spent, maxCost))
	keys := make([]string, 0, len(writes))
	for key := range writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		if err := atomicCreateArtifact(runDir, rel, writes[rel]); err != nil {
			return err
		}
	}
	return nil
}

func renderReview(source *sourceBook, desc GeneratorDescriptor, spent, maxCost float64) string {
	mode := "Stage 3a offline; no real Provider calls"
	if desc.RealProvider {
		mode = "Stage 3b real Provider review run"
	}
	return fmt.Sprintf(`# SceneBeat v3 migration review

Status: review only. This run cannot approve or apply canonical data.

- Chapters: 42
- Scenes: %d total (%d legacy strings, %d structured v2)
- Generator: %s/%s (%s)
- Recorded cost: $%.6f / $%.2f hard cap

Review `+"`proposed/layered_outline.json`"+` as the canonical proposal view, then inspect `+"`review.diff.json`"+`, `+"`audit.json`"+`, all 42 files under `+"`proposals/`"+`, and provider records/exchanges. The other proposed outline files are deterministic renderings for comparison.

No approval/apply command exists in this build. Run `+"`ainovel-migrate scene-beat-v3 verify --book-dir <book> --run-dir <run>`"+` after review.
`, source.TotalScenes, source.LegacyScenes, source.Structured, sanitizeLabel(desc.Provider), sanitizeLabel(desc.Model), mode, spent, maxCost)
}

func noSecretLeak(root string, forbidden []string) error {
	files, err := listRegularFiles(root)
	if err != nil {
		return err
	}
	for _, rel := range files {
		data, err := readPlainFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		for _, secret := range forbidden {
			if secret != "" && strings.Contains(string(data), secret) {
				return fmt.Errorf("secret leaked into artifact %s", rel)
			}
		}
	}
	return nil
}
