package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── Shared test constants ───────────────────────────────────────────

var (
	testDraft = DigestDraft("draft")
	testBasis = DigestReviewBasis(ReviewBasis{
		FactualOutline: "c",
		CriticVersion:  "v",
	})
	testFind = StyleReviewFinding{Dimension: FindingDimensionConsistency, Severity: FindingSeverityWarning, Category: FindingCategoryPlot, Evidence: "needs work"}
)

// ── StyleQualityMode ────────────────────────────────────────────────

func TestStyleQualityMode_Valid(t *testing.T) {
	tests := []struct {
		mode  StyleQualityMode
		valid bool
	}{
		{StyleQualityOff, true}, {StyleQualityCritic, true},
		{StyleQualityMode(""), false}, {StyleQualityMode("auto"), false},
	}
	for _, tc := range tests {
		got := tc.mode.Valid()
		if got != tc.valid {
			t.Errorf("StyleQualityMode(%q).Valid() = %v, want %v", tc.mode, got, tc.valid)
		}
	}
}

func TestStyleQualityMode_Enabled(t *testing.T) {
	if StyleQualityOff.Enabled() {
		t.Error("off should not be enabled")
	}
	if !StyleQualityCritic.Enabled() {
		t.Error("critic should be enabled")
	}
}

// ── StyleReviewStatus ───────────────────────────────────────────────

func TestStyleReviewStatus_Valid(t *testing.T) {
	for _, s := range []StyleReviewStatus{
		ReviewStatusInitialPending, ReviewStatusAcceptedInitial, ReviewStatusRevisionOpen,
		ReviewStatusFinalPending, ReviewStatusAcceptedRev,
		ReviewStatusExhausted, ReviewStatusDegraded, ReviewStatusOverridden,
	} {
		if !s.Valid() {
			t.Errorf("status %q should be valid", s)
		}
	}
	if StyleReviewStatus("unknown").Valid() || StyleReviewStatus("").Valid() {
		t.Error("unknown/empty status should be invalid")
	}
}

func TestStyleReviewStatus_IsTerminal(t *testing.T) {
	tests := []struct {
		s    StyleReviewStatus
		want bool
	}{
		{ReviewStatusInitialPending, false}, {ReviewStatusAcceptedInitial, true},
		{ReviewStatusRevisionOpen, false}, {ReviewStatusFinalPending, false},
		{ReviewStatusAcceptedRev, true}, {ReviewStatusExhausted, false},
		{ReviewStatusDegraded, true}, {ReviewStatusOverridden, true},
	}
	for _, tc := range tests {
		if tc.s.IsTerminal() != tc.want {
			t.Errorf("IsTerminal(%q) = %v, want %v", tc.s, tc.s.IsTerminal(), tc.want)
		}
	}
}

func TestStyleReviewStatus_IsActive(t *testing.T) {
	tests := []struct {
		s    StyleReviewStatus
		want bool
	}{
		{ReviewStatusInitialPending, true}, {ReviewStatusAcceptedInitial, false},
		{ReviewStatusRevisionOpen, true}, {ReviewStatusFinalPending, true},
		{ReviewStatusAcceptedRev, false}, {ReviewStatusExhausted, false},
		{ReviewStatusDegraded, false}, {ReviewStatusOverridden, false},
	}
	for _, tc := range tests {
		if tc.s.IsActive() != tc.want {
			t.Errorf("IsActive(%q) = %v, want %v", tc.s, tc.s.IsActive(), tc.want)
		}
	}
}

// ── StyleReviewVerdict ──────────────────────────────────────────────

func TestStyleReviewVerdict_Valid(t *testing.T) {
	if !ReviewVerdictPass.Valid() || !ReviewVerdictRevise.Valid() {
		t.Error("pass/revise should be valid")
	}
	if StyleReviewVerdict("").Valid() || StyleReviewVerdict("maybe").Valid() {
		t.Error("empty/unknown verdict should be invalid")
	}
}

// ── StyleReviewFinding ──────────────────────────────────────────────

func TestStyleReviewFinding_Valid(t *testing.T) {
	f := &StyleReviewFinding{Dimension: FindingDimensionConsistency, Severity: FindingSeverityError, Category: FindingCategoryPlot, Evidence: "text"}
	if !f.Valid() {
		t.Error("valid finding should be valid")
	}
}

func TestStyleReviewFinding_InvalidCases(t *testing.T) {
	var nilPtr *StyleReviewFinding
	if nilPtr.Valid() {
		t.Error("nil finding should be invalid via pointer receiver")
	}

	tests := []struct {
		name string
		f    StyleReviewFinding
	}{
		{"bad dimension", StyleReviewFinding{Dimension: "bogus", Severity: FindingSeverityWarning, Category: FindingCategoryStyle, Evidence: "t"}},
		{"bad severity", StyleReviewFinding{Dimension: FindingDimensionPacing, Severity: "catastrophic", Category: FindingCategoryLogic, Evidence: "t"}},
		{"bad category", StyleReviewFinding{Dimension: FindingDimensionAesthetic, Severity: FindingSeverityInfo, Category: "unknown", Evidence: "t"}},
		{"empty evidence", StyleReviewFinding{Dimension: FindingDimensionCharacter, Severity: FindingSeverityError, Category: FindingCategoryTone, Evidence: ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.f.Valid() {
				t.Error("should be invalid")
			}
		})
	}
}

// ── StyleReviewResult ───────────────────────────────────────────────

func TestStyleReviewResult_Valid(t *testing.T) {
	r := &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok", Findings: []StyleReviewFinding{
		{Dimension: FindingDimensionConsistency, Severity: FindingSeverityWarning, Category: FindingCategoryPlot, Evidence: "ev1"},
	}}
	if !r.Valid() {
		t.Error("valid result should pass")
	}
}

func TestStyleReviewResult_RejectsNil(t *testing.T) {
	var r *StyleReviewResult
	if r.Valid() {
		t.Error("nil result should be invalid")
	}
}

func TestStyleReviewResult_RejectsInvalidVerdict(t *testing.T) {
	r := &StyleReviewResult{Verdict: "bogus", Evidence: "e"}
	if r.Valid() {
		t.Error("invalid verdict should be rejected")
	}
}

func TestStyleReviewResult_RequiresEvidence(t *testing.T) {
	r := &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: ""}
	if r.Valid() {
		t.Error("empty evidence should be rejected")
	}
}

func TestStyleReviewResult_AllowsUpToThreeFindings(t *testing.T) {
	for n := 0; n <= 3; n++ {
		findings := make([]StyleReviewFinding, n)
		for i := range findings {
			findings[i] = StyleReviewFinding{Dimension: FindingDimensionConsistency, Severity: FindingSeverityWarning, Category: FindingCategoryPlot, Evidence: "x"}
		}
		r := &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "s", Findings: findings}
		if !r.Valid() {
			t.Errorf("result with %d findings should be valid", n)
		}
	}
}

func TestStyleReviewResult_RejectsMoreThanThreeFindings(t *testing.T) {
	findings := make([]StyleReviewFinding, 4)
	for i := range findings {
		findings[i] = StyleReviewFinding{Dimension: FindingDimensionConsistency, Severity: FindingSeverityWarning, Category: FindingCategoryPlot, Evidence: "x"}
	}
	r := &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "s", Findings: findings}
	if r.Valid() {
		t.Error("result with 4 findings should be invalid")
	}
}

// ── StyleReviewRequest ──────────────────────────────────────────────

func TestStyleReviewRequest_Normalize_TruncatesLongPrompt(t *testing.T) {
	r := &StyleReviewRequest{Prompt: strings.Repeat("a", maxReviewRequestPromptBytes+100)}
	r.Normalize()
	if len(r.Prompt) != maxReviewRequestPromptBytes {
		t.Errorf("prompt length = %d, want %d", len(r.Prompt), maxReviewRequestPromptBytes)
	}
	if !r.PromptTrunc {
		t.Error("PromptTrunc should be true")
	}
}

func TestStyleReviewRequest_Normalize_NilSafe(t *testing.T) {
	var r *StyleReviewRequest
	r.Normalize()
}

func TestStyleReviewRequest_Normalize_ShortPromptUntouched(t *testing.T) {
	r := &StyleReviewRequest{Prompt: "short"}
	r.Normalize()
	if r.Prompt != "short" || r.PromptTrunc {
		t.Error("short prompt should not be truncated")
	}
}

// ── Ledger helpers ──────────────────────────────────────────────────

func TestStyleReviewLedger_CurrentCycle_Empty(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff}
	if c := l.CurrentCycle(); c != nil {
		t.Errorf("expected nil, got %+v", c)
	}
}

func TestStyleReviewLedger_CurrentStatus_Empty(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff}
	if s := l.CurrentStatus(); s != "" {
		t.Errorf("expected empty string for empty ledger, got %q", s)
	}
}

func TestStyleReviewLedger_IsEmpty(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff}
	if !l.IsEmpty() {
		t.Error("should be empty")
	}
	l.Cycles = []StyleReviewEntry{mkEntry(1, ReviewStatusAcceptedInitial, ReviewVerdictPass)}
	if l.IsEmpty() {
		t.Error("should not be empty")
	}
}

func TestStyleReviewLedger_EmptyIsNotUnderReview(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff}
	if l.IsUnderReview() {
		t.Error("empty ledger should not be under review")
	}
}

func TestStyleReviewLedger_IsUnderReview(t *testing.T) {
	tests := []struct {
		status StyleReviewStatus
		want   bool
	}{
		{ReviewStatusInitialPending, true}, {ReviewStatusAcceptedInitial, false},
		{ReviewStatusRevisionOpen, true}, {ReviewStatusFinalPending, true},
		{ReviewStatusAcceptedRev, false}, {ReviewStatusExhausted, false},
		{ReviewStatusDegraded, false}, {ReviewStatusOverridden, false},
	}
	for _, tc := range tests {
		l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
			Cycles: []StyleReviewEntry{mkEntry(1, tc.status, ReviewVerdictPass)}}
		if l.IsUnderReview() != tc.want {
			t.Errorf("IsUnderReview() for %q = %v, want %v", tc.status, l.IsUnderReview(), tc.want)
		}
	}
}

func TestStyleReviewLedger_HasOverrides(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{mkEntry(1, ReviewStatusAcceptedInitial, ReviewVerdictPass)}}
	if l.HasOverrides() {
		t.Error("no overrides yet")
	}
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusOverridden, ReviewVerdictPass))
	if !l.HasOverrides() {
		t.Error("should have overrides")
	}
}

// ── EntriesEqual ─────────────────────────────────────────────────────

func TestEntriesEqual_Identical(t *testing.T) {
	a := mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)
	b := mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)
	if !EntriesEqual(a, b) {
		t.Error("identical entries should be equal")
	}
}

func TestEntriesEqual_DifferentAttemptID(t *testing.T) {
	a := mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)
	b := mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)
	b.AttemptID = "different"
	if EntriesEqual(a, b) {
		t.Error("entries with different attempt_id should not be equal")
	}
}

func TestEntriesEqual_DifferentDigest(t *testing.T) {
	a := mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)
	b := mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)
	b.DraftDigest = DigestDraft("other")
	if EntriesEqual(a, b) {
		t.Error("entries with different draft_digest should not be equal")
	}
}

func TestEntriesEqual_DifferentResult(t *testing.T) {
	a := mkEntry(1, ReviewStatusAcceptedInitial, ReviewVerdictPass)
	b := mkEntry(1, ReviewStatusAcceptedInitial, ReviewVerdictPass)
	b.Result.Evidence = "different"
	if EntriesEqual(a, b) {
		t.Error("entries with different result should not be equal")
	}
}

// ── ValidateLedger: structural ──────────────────────────────────────

func TestValidateLedger_Nil(t *testing.T) {
	err := ValidateLedger(nil)
	if err == nil {
		t.Fatal("expected error for nil")
	}
}

func TestValidateLedger_BadSchemaVersion(t *testing.T) {
	err := ValidateLedger(&StyleReviewLedger{SchemaVersion: 99, Chapter: 1, Mode: StyleQualityOff})
	if err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("expected schema version error, got %v", err)
	}
}

func TestValidateLedger_ChapterZero(t *testing.T) {
	err := ValidateLedger(&StyleReviewLedger{SchemaVersion: 1, Chapter: 0, Mode: StyleQualityOff})
	if err == nil || !strings.Contains(err.Error(), "chapter") {
		t.Fatalf("expected chapter error, got %v", err)
	}
}

func TestValidateLedger_InvalidMode(t *testing.T) {
	err := ValidateLedger(&StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: "turbo"})
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("expected mode error, got %v", err)
	}
}

func TestValidateLedger_EmptyCyclesValid(t *testing.T) {
	err := ValidateLedger(&StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff})
	if err != nil {
		t.Fatalf("empty cycles should be valid: %v", err)
	}
}

// ── Critic mode must start with initial_pending ─────────────────────

func TestValidateLedger_CriticModeFirstCycleMustBeInitialPending(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{
			Cycle: 1, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T10:00:00Z",
			Request: &StyleReviewRequest{Prompt: "x", Model: "gpt-4o"}, Result: &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "e"},
			DraftDigest: testDraft, BasisDigest: testBasis,
		}},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "first cycle must be initial_pending") {
		t.Fatalf("expected first cycle must be initial_pending, got %v", err)
	}
}

// ── Off mode ────────────────────────────────────────────────────────

// ── Critic mode zero cycles ────────────────────────────────────────

func TestValidateLedger_CriticModeZeroCyclesInvalid(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "zero cycles") {
		t.Fatalf("expected zero cycles error, got %v", err)
	}
}

func TestValidateLedger_OffModeZeroCyclesValid(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff}
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("off mode zero cycles should be valid: %v", err)
	}
}

// ── Non-blank model/prompt ─────────────────────────────────────────

func TestValidateLedger_InitialPendingRequiresModel(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{
			Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
			AttemptID: "a1", DraftDigest: testDraft, BasisDigest: testBasis,
			Request: &StyleReviewRequest{Prompt: "review", Model: ""}, // missing model
		}},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "non-blank model") {
		t.Fatalf("expected non-blank model error, got %v", err)
	}
}

func TestValidateLedger_InitialPendingRequiresPrompt(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{
			Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
			AttemptID: "a1", DraftDigest: testDraft, BasisDigest: testBasis,
			Request: &StyleReviewRequest{Prompt: "", Model: "gpt-4o"}, // missing prompt
		}},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "non-blank prompt") {
		t.Fatalf("expected non-blank prompt error, got %v", err)
	}
}

func TestValidateLedger_InitialPendingRequiresNonBlankModelAndPromptInRequest(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{
			Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
			AttemptID: "a1", DraftDigest: testDraft, BasisDigest: testBasis,
			Request: &StyleReviewRequest{Prompt: "valid prompt", Model: "gpt-4o"},
		}},
	}
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("valid request with model/prompt should pass: %v", err)
	}
}

func TestValidateLedger_OffModeAcceptsTerminal(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff,
		Cycles: []StyleReviewEntry{mkEntry(1, ReviewStatusAcceptedInitial, ReviewVerdictPass)},
	}
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("off mode terminal should be valid: %v", err)
	}
}

func TestValidateLedger_OffModeRejectsActive(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityOff,
		Cycles: []StyleReviewEntry{mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass)},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "active status") {
		t.Fatalf("expected active status rejection in off mode, got %v", err)
	}
}

// ── Cycle sequence ──────────────────────────────────────────────────

func TestValidateLedger_NonSequentialCycles(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(3, ReviewStatusAcceptedInitial, ReviewVerdictPass))
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "cycle[1]") {
		t.Fatalf("expected numbering error, got %v", err)
	}
}

func TestValidateLedger_MissingCreatedAt(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: ""}},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("expected RFC3339 error, got %v", err)
	}
}

// ── Initial_pending payload ─────────────────────────────────────────

func TestValidateLedger_InitialPendingRequiresAttemptID(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{
			Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
			Request:     &StyleReviewRequest{Prompt: "x", Model: "gpt-4o"},
			DraftDigest: testDraft, BasisDigest: testBasis,
		}},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "attempt_id") {
		t.Fatalf("expected attempt_id required, got %v", err)
	}
}

func TestValidateLedger_InitialPendingRequiresDigests(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{{
			Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z",
			Request: &StyleReviewRequest{Prompt: "x", Model: "gpt-4o"}, AttemptID: "a1",
		}},
	}
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "draft_digest") {
		t.Fatalf("expected digest required, got %v", err)
	}
}

// ── Revise requires findings ────────────────────────────────────────

func TestValidateLedger_RevisionOpenRequiresFindings(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "needs work"}, // no findings
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "requires at least one finding") {
		t.Fatalf("expected findings required for revise, got %v", err)
	}
}

func TestValidateLedger_RevisionOpenWithFindingsValid(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "needs work", Findings: []StyleReviewFinding{testFind}},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("revision_open with findings should be valid: %v", err)
	}
}

// ── Attempt-ID binding ──────────────────────────────────────────────
func TestValidateLedger_AttemptIDMismatchInitialPending(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "WRONG", // different from initial_pending's "a1"
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "attempt_id") {
		t.Fatalf("expected attempt_id mismatch error, got %v", err)
	}
}

func TestValidateLedger_StaleDraftDigest(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: DigestDraft("stale"), BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "draft_digest") {
		t.Fatalf("expected draft_digest mismatch error, got %v", err)
	}
}

func TestValidateLedger_StaleRequestPrompt(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "different prompt", Model: "gpt-4o"}, // changed!
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "request metadata differs") {
		t.Fatalf("expected request metadata mismatch error, got %v", err)
	}
}

func TestValidateLedger_RequestMetadataSubstitutionRejected(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	req := &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
	l.Cycles[0].Request = req
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o", PromptTrunc: true}, // different!
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "request metadata differs") {
		t.Fatalf("expected request metadata mismatch error for PromptTrunc, got %v", err)
	}
}

func TestValidateLedger_RequestIncludeBasisSubstitutionRejected(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	req := &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o", IncludeBasis: true}
	l.Cycles[0].Request = req
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o", IncludeBasis: false}, // different!
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "request metadata differs") {
		t.Fatalf("expected request metadata mismatch error for IncludeBasis, got %v", err)
	}
}

func TestValidateLedger_RequestRequestedAtSubstitutionRejected(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	req := &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o", RequestedAt: "2026-07-25T10:00:00Z"}
	l.Cycles[0].Request = req
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusAcceptedInitial, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o", RequestedAt: "2026-07-25T11:00:00Z"}, // different!
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "request metadata differs") {
		t.Fatalf("expected request metadata mismatch error for RequestedAt, got %v", err)
	}
}

func TestValidateLedger_AttemptIDBindingFinalPending(t *testing.T) {
	// Build a full flow with final attempt ID mismatch
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "needs work", Findings: []StyleReviewFinding{testFind}},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
		AttemptID:   "a2",
		Request:     &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	// The successor of final_pending must share attempt_id "a2"
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 4, Status: ReviewStatusAcceptedRev, CreatedAt: "2026-07-25T13:00:00Z",
		AttemptID:   "WRONG", // should be "a2"
		Request:     &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "attempt_id") {
		t.Fatalf("expected final pending attempt_id mismatch error, got %v", err)
	}
}

// ── V1 transitions ──────────────────────────────────────────────────

func TestValidateLedger_V1_InitialToAcceptedInitial(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusAcceptedInitial, ReviewVerdictPass))
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("initial→accepted_initial should be valid: %v", err)
	}
}

func TestValidateLedger_V1_InitialToRevisionOpen(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusRevisionOpen, ReviewVerdictRevise))
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("initial→revision_open should be valid: %v", err)
	}
}

func TestValidateLedger_V1_RevisionToFinalPending(t *testing.T) {
	l := validFlow(ReviewStatusRevisionOpen)
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("revision→final_pending should be valid: %v", err)
	}
}

func TestValidateLedger_V1_FinalToAcceptedRevised(t *testing.T) {
	l := validFlow(ReviewStatusAcceptedRev)
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("final→accepted_revised should be valid: %v", err)
	}
}

func TestValidateLedger_V2_FinalToRevisionOpen(t *testing.T) {
	l := validFlowForV2()
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("final→revision_open should be valid in V2: %v", err)
	}
}

func TestValidateLedger_LegacyExhaustedToOverridden(t *testing.T) {
	l := validFlow(ReviewStatusOverridden)
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("exhausted→overridden should be valid: %v", err)
	}
}

// ── Negative V2 transitions ─────────────────────────────────────────

func TestValidateLedger_V2_RejectsInitialToFinalPending(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusFinalPending, ReviewVerdictPass))
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "invalid V2 transition") {
		t.Fatalf("expected V2 transition error, got %v", err)
	}
}

func TestValidateLedger_V2_RejectsRevisionToAcceptedInitial(t *testing.T) {
	l := validFlow(ReviewStatusRevisionOpen)
	l.Cycles = append(l.Cycles, mkEntry(3, ReviewStatusAcceptedInitial, ReviewVerdictPass))
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "invalid V2 transition") {
		t.Fatalf("expected V2 transition error, got %v", err)
	}
}

// ── Degraded payload ────────────────────────────────────────────────

func TestValidateLedger_DegradedRequiresDigests(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 2, Status: ReviewStatusDegraded, CreatedAt: "2026-07-25T11:00:00Z",
		AttemptID: "a1",
		Request:   &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Error:     "network failure",
		// no digests
	})
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "draft_digest") {
		t.Fatalf("expected digest required for degraded, got %v", err)
	}
}

// ── Overridden digest equality ──────────────────────────────────────

func TestValidateLedger_OverriddenDigestMismatch(t *testing.T) {
	// Build valid flow to overridden, then corrupt the override digest
	l := validFlow(ReviewStatusOverridden)
	l.Cycles[4].Override.DraftDigest = DigestDraft("wrong")
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "override draft_digest") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}

// ── Terminality ─────────────────────────────────────────────────────

func TestValidateLedger_TerminalBlocksSubsequent(t *testing.T) {
	// Build valid flow, then add a cycle after the terminal accepted_revised
	l := validFlow(ReviewStatusAcceptedRev)
	l.Cycles = append(l.Cycles, mkEntry(5, ReviewStatusInitialPending, ReviewVerdictPass))
	err := ValidateLedger(l)
	if err == nil || !strings.Contains(err.Error(), "invalid V2 transition") {
		t.Fatalf("expected V1 transition error, got %v", err)
	}
}

// ── Degraded retry: new attempt after degraded (transient call failure) ──

// degraded 是评审调用故障（瞬态），允许在其后追加新的 initial_pending attempt，
// 完成初评（accepted_initial）后账本仍然合法。
func TestValidateLedger_DegradedAllowsNewInitialAttempt(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusDegraded, ReviewVerdictPass))
	l.Cycles = append(l.Cycles, mkEntry(3, ReviewStatusInitialPending, ReviewVerdictPass))
	l.Cycles = append(l.Cycles, mkEntry(4, ReviewStatusAcceptedInitial, ReviewVerdictPass))
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("degraded → new initial attempt → accepted_initial should be valid: %v", err)
	}
}

// 终审失败降级后，允许追加新的 final_pending attempt，完成终审（accepted_revised）。
func TestValidateLedger_DegradedAllowsNewFinalAttempt(t *testing.T) {
	l := validFlow(ReviewStatusFinalPending) // cycles 1-3: initial_pending → revision_open → final_pending
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 4, Status: ReviewStatusDegraded, CreatedAt: "2026-07-25T13:00:00Z",
		AttemptID:   "a2",
		Request:     &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
		Error:       "critic returned empty output",
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 5, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T14:00:00Z",
		AttemptID:   "a3",
		Request:     &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	l.Cycles = append(l.Cycles, StyleReviewEntry{
		Cycle: 6, Status: ReviewStatusAcceptedRev, CreatedAt: "2026-07-25T15:00:00Z",
		AttemptID:   "a3",
		Request:     &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
		Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "ok"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("degraded → new final attempt → accepted_revised should be valid: %v", err)
	}
}

// degraded 后必须接 pending attempt，直接跳到 terminal（accepted_initial）非法。
func TestValidateLedger_DegradedRejectsDirectTerminalFollowup(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusDegraded, ReviewVerdictPass))
	l.Cycles = append(l.Cycles, mkEntry(3, ReviewStatusAcceptedInitial, ReviewVerdictPass))
	err := ValidateLedger(l)
	if err == nil {
		t.Fatal("degraded → accepted_initial directly must be invalid (missing new attempt)")
	}
}

// 其他 terminal 状态（accepted_initial）仍不得有后续周期。
func TestValidateLedger_AcceptedInitialStillTerminal(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles = append(l.Cycles, mkEntry(2, ReviewStatusAcceptedInitial, ReviewVerdictPass))
	l.Cycles = append(l.Cycles, mkEntry(3, ReviewStatusInitialPending, ReviewVerdictPass))
	err := ValidateLedger(l)
	if err == nil {
		t.Fatal("accepted_initial must remain terminal (no subsequent cycles)")
	}
}

// ── Digest helpers ──────────────────────────────────────────────────

func TestDigestDraft_Format(t *testing.T) {
	d := DigestDraft("hello")
	if !strings.HasPrefix(d, "sha256:") || len(d) != 7+64 {
		t.Errorf("bad format: %q", d)
	}
}

func TestDigestDraft_Deterministic(t *testing.T) {
	if DigestDraft("x") != DigestDraft("x") {
		t.Error("not deterministic")
	}
}

func TestIsValidDigest(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"sha256:" + strings.Repeat("a", 64), true},
		{"sha256:" + strings.Repeat("A", 64), false},
		{"sha256:abc", false},
		{"", false},
	}
	for _, tc := range tests {
		if IsValidDigest(tc.s) != tc.want {
			t.Errorf("IsValidDigest(%q) = %v, want %v", tc.s, !tc.want, tc.want)
		}
	}
}

func TestDigestReviewBasis_Deterministic(t *testing.T) {
	basis := ReviewBasis{
		FactualOutline: "c",
		CriticVersion:  "v",
	}
	a := DigestReviewBasis(basis)
	b := DigestReviewBasis(basis)
	if a != b {
		t.Error("not deterministic")
	}
}

// legacyDigestTestBasis 构造全字段填充的 ReviewBasis，用于锁定 legacy 摘要
// 算法与验证 canonical/legacy 的差异。
func legacyDigestTestBasis() ReviewBasis {
	return ReviewBasis{
		CriticVersion: "critic-v1",
		UserRules:     json.RawMessage(`{"default":["规则A"],"editor":["规则B"]}`),
		CompassProse:  []string{"稳重", "克制"},
		CompassDialogue: []CharacterVoice{
			{Name: "主角", Rules: []string{"沉稳", "简洁"}},
		},
		CompassTaboos:  []string{"网络用语"},
		AnchorExcerpts: []string{"锚点一"},
		StyleGoal: &ChapterStyleGoal{
			FocalFilter:   "聚焦主角视角",
			ProseMovement: "明快",
		},
		ChapterContract: &ChapterContract{
			RequiredBeats: []string{"伏笔兑现"},
			HookGoal:      "留下悬念",
		},
		FactualOutline: "第一章大纲事实",
	}
}

// TestDigestReviewBasis_LegacyGolden 锁定 legacy（旧字段排列）摘要算法：
// golden 值对应 ae540649（stable-first prompt capsule 字段重排）之前旧版
// DigestReviewBasis（整体 json.Marshal + sha256）的输出。若此值变化，说明
// legacy 兼容路径失效，升级后旧 pending 账本会被误判为 basis 漂移而降级。
func TestDigestReviewBasis_LegacyGolden(t *testing.T) {
	want := "sha256:b04dc9762b2ea7f310138a2984853b0ad330fb71502008851551ef639888a592"
	if got := DigestReviewBasisLegacy(legacyDigestTestBasis()); got != want {
		t.Errorf("DigestReviewBasisLegacy = %q, want %q（legacy 摘要算法不得漂移）", got, want)
	}
}

// TestDigestReviewBasis_ContentSensitive 验证 canonical 摘要是内容的纯函数：
// 相同内容 → 相同摘要；任一字段内容变化 → 摘要变化。
func TestDigestReviewBasis_ContentSensitive(t *testing.T) {
	basis := legacyDigestTestBasis()
	if DigestReviewBasis(basis) != DigestReviewBasis(basis) {
		t.Error("same content must produce same digest")
	}
	changed := legacyDigestTestBasis()
	changed.FactualOutline = "大纲事实已变更"
	if DigestReviewBasis(changed) == DigestReviewBasis(basis) {
		t.Error("content change must change canonical digest")
	}
}

// TestDigestReviewBasis_LegacyDiffersFromCanonical 演示迁移问题的存在：
// 同一语义内容在旧字段排列（legacy）与新 canonical 排列下摘要不同——这正是
// BasisDigestMatches 双摘要兼容要解决的升级期误判来源。
func TestDigestReviewBasis_LegacyDiffersFromCanonical(t *testing.T) {
	basis := legacyDigestTestBasis()
	if DigestReviewBasis(basis) == DigestReviewBasisLegacy(basis) {
		t.Error("legacy 与 canonical 摘要必须不同（否则双摘要兼容无意义）")
	}
}

// TestBasisDigestMatches_DualAcceptance 覆盖双摘要兼容判定：
// 新写入（canonical）、旧落盘（legacy）、未绑定（空）均通过；内容真实漂移拒绝。
func TestBasisDigestMatches_DualAcceptance(t *testing.T) {
	basis := legacyDigestTestBasis()
	current := DigestReviewBasis(basis)
	legacy := DigestReviewBasisLegacy(basis)

	tests := []struct {
		name     string
		recorded string
		want     bool
	}{
		{"new canonical recorded matches", current, true},
		{"legacy wire-order recorded matches (upgrade compat)", legacy, true},
		{"unbound (empty) recorded always matches", "", true},
	}
	for _, tc := range tests {
		if got := BasisDigestMatches(basis, tc.recorded, current); got != tc.want {
			t.Errorf("%s: BasisDigestMatches = %v, want %v", tc.name, got, tc.want)
		}
	}

	// 内容真实漂移：recorded 是旧内容的 legacy 摘要，当前内容已变更，
	// canonical 与 legacy 两种算法均不匹配 → 必须判定为漂移（拒绝）。
	changed := legacyDigestTestBasis()
	changed.StyleGoal = &ChapterStyleGoal{FocalFilter: "新聚焦", ProseMovement: "新节奏"}
	if BasisDigestMatches(changed, legacy, DigestReviewBasis(changed)) {
		t.Error("drifted basis must not match under dual acceptance")
	}
}

// ── Helpers ─────────────────────────────────────────────────────────

// validLedger creates a single-cycle critic-mode ledger starting at the given
// initial_pending status with canonical test digests and attempt ID "a1".
func validLedger(chapter int, status StyleReviewStatus, verdict StyleReviewVerdict) *StyleReviewLedger {
	return &StyleReviewLedger{
		SchemaVersion: 1, Chapter: chapter, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{mkEntry(1, status, verdict)},
	}
}

// mkEntry creates a single entry that passes its status payload validation.
func mkEntry(cycle int, status StyleReviewStatus, verdict StyleReviewVerdict) StyleReviewEntry {
	e := StyleReviewEntry{
		Cycle:       cycle,
		Status:      status,
		CreatedAt:   "2026-07-25T10:00:00Z",
		AttemptID:   "a1",
		DraftDigest: testDraft,
		BasisDigest: testBasis,
	}
	switch status {
	case ReviewStatusInitialPending:
		e.Request = &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
	case ReviewStatusAcceptedInitial:
		e.Request = &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
		e.Result = &StyleReviewResult{Verdict: verdict, Evidence: "ok"}
	case ReviewStatusRevisionOpen:
		e.Request = &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
		e.Result = &StyleReviewResult{Verdict: verdict, Evidence: "needs work", Findings: []StyleReviewFinding{testFind}}
	case ReviewStatusFinalPending:
		e.Request = &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}
		e.AttemptID = "a2"
	case ReviewStatusAcceptedRev:
		e.Request = &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}
		e.Result = &StyleReviewResult{Verdict: verdict, Evidence: "ok"}
		e.AttemptID = "a2"
	case ReviewStatusExhausted:
		e.Request = &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}
		e.Result = &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "still needs work", Findings: []StyleReviewFinding{testFind}}
		e.AttemptID = "a2"
	case ReviewStatusDegraded:
		e.Error = "infrastructure failure"
		e.Request = &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
	case ReviewStatusOverridden:
		e.AttemptID = "a2"
		e.Override = &StyleReviewOverride{
			Actor: "user", Reason: "manual override",
			DraftDigest: testDraft, BasisDigest: testBasis, OverriddenAt: "2026-07-25T11:00:00Z",
		}
	}
	return e
}

// validFlow builds a multi-cycle critic-mode ledger following the V1 graph
// up through the given terminal status.
func validFlow(terminal Status) *StyleReviewLedger {
	d := testDraft
	b := testBasis
	req := &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
	find := []StyleReviewFinding{testFind}

	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: req, DraftDigest: d, BasisDigest: b},
		},
	}

	switch terminal {
	case ReviewStatusRevisionOpen:
		l.Cycles = append(l.Cycles, StyleReviewEntry{
			Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
			AttemptID: "a1", Request: req,
			Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "revise", Findings: find},
			DraftDigest: d, BasisDigest: b,
		})
	case ReviewStatusFinalPending:
		l.Cycles = append(l.Cycles,
			StyleReviewEntry{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: req,
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "revise", Findings: find},
				DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}, DraftDigest: d, BasisDigest: b},
		)
	case ReviewStatusAcceptedRev:
		l.Cycles = append(l.Cycles,
			StyleReviewEntry{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: req,
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "revise", Findings: find},
				DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}, DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 4, Status: ReviewStatusAcceptedRev, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictPass, Evidence: "pass"},
				DraftDigest: d, BasisDigest: b},
		)
	case ReviewStatusExhausted:
		l.Cycles = append(l.Cycles,
			StyleReviewEntry{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: req,
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "revise", Findings: find},
				DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}, DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 4, Status: ReviewStatusExhausted, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "exhausted", Findings: find},
				DraftDigest: d, BasisDigest: b},
		)
	case ReviewStatusOverridden:
		l.Cycles = append(l.Cycles,
			StyleReviewEntry{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: req,
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "revise", Findings: find},
				DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}, DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 4, Status: ReviewStatusExhausted, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "exhausted", Findings: find},
				DraftDigest: d, BasisDigest: b},
			StyleReviewEntry{Cycle: 5, Status: ReviewStatusOverridden, CreatedAt: "2026-07-25T14:00:00Z",
				AttemptID:   "a2",
				DraftDigest: d, BasisDigest: b,
				Override: &StyleReviewOverride{Actor: "user", Reason: "bypass", DraftDigest: d, BasisDigest: b, OverriddenAt: "2026-07-25T15:00:00Z"},
			},
		)
	}
	return l
}

// validFlowForV2 builds a V2 ledger: initial_pending → revision_open → final_pending → revision_open (loop).
func validFlowForV2() *StyleReviewLedger {
	d := testDraft
	b := testBasis
	req := &StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"}
	find := []StyleReviewFinding{testFind}
	return &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: req, DraftDigest: d, BasisDigest: b},
			{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: req,
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "revise", Findings: find},
				DraftDigest: d, BasisDigest: b},
			{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"}, DraftDigest: d, BasisDigest: b},
			{Cycle: 4, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "gpt-4o"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "still needs work", Findings: find},
				DraftDigest: d, BasisDigest: b},
		},
	}
}

// Status alias for validFlow parameter.
type Status = StyleReviewStatus

// ── FindingsSignature tests ──────────────────────────────────────────

func TestFindingsSignature_DifferentFindings(t *testing.T) {
	r1 := &StyleReviewResult{Findings: []StyleReviewFinding{
		{Dimension: "pacing", Category: "style", Severity: "warning", Suggestion: "慢"},
	}}
	r2 := &StyleReviewResult{Findings: []StyleReviewFinding{
		{Dimension: "hook", Category: "plot", Severity: "error", Suggestion: "悬念弱"},
	}}
	sig1 := r1.FindingsSignature()
	sig2 := r2.FindingsSignature()
	if sig1 == "" {
		t.Fatal("expected non-empty sig for r1")
	}
	if sig2 == "" {
		t.Fatal("expected non-empty sig for r2")
	}
	if sig1 == sig2 {
		t.Fatal("different findings should produce different signatures")
	}
}

func TestFindingsSignature_ReorderedSame(t *testing.T) {
	r1 := &StyleReviewResult{Findings: []StyleReviewFinding{
		{Dimension: "pacing", Category: "style", Severity: "warning", Suggestion: "慢"},
		{Dimension: "hook", Category: "plot", Severity: "error", Suggestion: "悬念弱"},
	}}
	r2 := &StyleReviewResult{Findings: []StyleReviewFinding{
		{Dimension: "hook", Category: "plot", Severity: "error", Suggestion: "悬念弱"},
		{Dimension: "pacing", Category: "style", Severity: "warning", Suggestion: "慢"},
	}}
	sig1 := r1.FindingsSignature()
	sig2 := r2.FindingsSignature()
	if sig1 == "" || sig2 == "" {
		t.Fatal("expected non-empty signatures")
	}
	if sig1 != sig2 {
		t.Fatal("reordered same findings should produce identical signature")
	}
}

func TestFindingsSignature_NilResult(t *testing.T) {
	var r *StyleReviewResult
	if sig := r.FindingsSignature(); sig != "" {
		t.Fatalf("nil result should return empty sig, got %q", sig)
	}
}

func TestFindingsSignature_EmptyFindings(t *testing.T) {
	r := &StyleReviewResult{Findings: []StyleReviewFinding{}}
	if sig := r.FindingsSignature(); sig != "" {
		t.Fatalf("empty findings should return empty sig, got %q", sig)
	}
}

// ── DetectFinalReviewStagnation tests ────────────────────────────────

func TestDetectStagnation_NoPriorRevOpen(t *testing.T) {
	find := []StyleReviewFinding{{Dimension: "hook", Category: "plot", Severity: "error", Problem: "平淡", Suggestion: "加悬念"}}
	ledger := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"}, DraftDigest: testDraft, BasisDigest: testBasis},
		},
	}
	result := &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "e", Findings: find}
	if DetectFinalReviewStagnation(ledger, result) {
		t.Fatal("no prior revision_open → should not detect stagnation")
	}
}

func TestDetectStagnation_InitialRevOpenNeverTriggers(t *testing.T) {
	find := []StyleReviewFinding{{Dimension: "hook", Category: "plot", Severity: "error", Problem: "平淡", Suggestion: "加悬念"}}
	d := testDraft
	b := testBasis
	// Ledger: initial_pending → revision_open only.  This is the initial review
	// cycle, NOT a final-revise cycle.  Stagnation must NOT trigger even if
	// the result has the same findings, because there is no adjacent final_pending.
	ledger := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"}, DraftDigest: d, BasisDigest: b},
			{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "needs work", Findings: find},
				DraftDigest: d, BasisDigest: b},
		},
	}
	// Same findings but no adjacent final_pending → must NOT detect stagnation
	result := &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "still same", Findings: find}
	if DetectFinalReviewStagnation(ledger, result) {
		t.Fatal("initial (non-final) revision_open must never trigger stagnation")
	}
}

func TestDetectStagnation_SameSig_Detected(t *testing.T) {
	find := []StyleReviewFinding{{Dimension: "hook", Category: "plot", Severity: "error", Problem: "平淡", Suggestion: "加悬念"}}
	d := testDraft
	b := testBasis
	// Build a ledger with a strict adjacent final_pending → revision_open chain
	ledger := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"}, DraftDigest: d, BasisDigest: b},
			{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "needs work", Findings: find},
				DraftDigest: d, BasisDigest: b},
			{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "m"}, DraftDigest: d, BasisDigest: b},
			{Cycle: 4, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "m"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "same issues", Findings: find},
				DraftDigest: d, BasisDigest: b},
		},
	}
	// Same findings again → stagnation (would be cycle 5)
	result := &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "still same", Findings: find}
	if !DetectFinalReviewStagnation(ledger, result) {
		t.Fatal("same finding signature with adjacent final_revise should detect stagnation")
	}
}

func TestDetectStagnation_DifferentSig_NotDetected(t *testing.T) {
	find1 := []StyleReviewFinding{{Dimension: "hook", Category: "plot", Severity: "error", Problem: "平淡", Suggestion: "加悬念"}}
	find2 := []StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Problem: "拖沓", Suggestion: "压缩"}}
	d := testDraft
	b := testBasis
	// Build strict adjacent final chain
	ledger := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"}, DraftDigest: d, BasisDigest: b},
			{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z",
				AttemptID: "a1", Request: &StyleReviewRequest{Prompt: "init", Model: "m"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "needs work", Findings: find1},
				DraftDigest: d, BasisDigest: b},
			{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "m"}, DraftDigest: d, BasisDigest: b},
			{Cycle: 4, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T13:00:00Z",
				AttemptID: "a2", Request: &StyleReviewRequest{Prompt: "final review", Model: "m"},
				Result:      &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "different issues", Findings: find1},
				DraftDigest: d, BasisDigest: b},
		},
	}
	// Different findings → no stagnation
	result := &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "new issues", Findings: find2}
	if DetectFinalReviewStagnation(ledger, result) {
		t.Fatal("different finding signature should not detect stagnation")
	}
}

func TestDetectStagnation_NilLedger(t *testing.T) {
	result := &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "e", Findings: []StyleReviewFinding{{Dimension: "hook", Category: "plot", Severity: "error", Suggestion: "弱"}}}
	if DetectFinalReviewStagnation(nil, result) {
		t.Fatal("nil ledger → no stagnation")
	}
}

func TestDetectStagnation_NilResult(t *testing.T) {
	ledger := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic}
	if DetectFinalReviewStagnation(ledger, nil) {
		t.Fatal("nil result → no stagnation")
	}
}

// ── Epoch state machine: 禁止倒退/跳号/负数 ────────────────────────────

// TestValidateLedger_EpochRegressionRejected 验证 epoch 倒退被拒绝：
// epoch 2 的 terminal 之后不允许出现 epoch 1 的周期。
func TestValidateLedger_EpochRegressionRejected(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass),
			mkEntry(2, ReviewStatusAcceptedInitial, ReviewVerdictPass),
			mkEntry(3, ReviewStatusInitialPending, ReviewVerdictPass),
		},
	}
	l.Cycles[0].Epoch = 2
	l.Cycles[1].Epoch = 2
	l.Cycles[2].Epoch = 1 // 倒退：2 → 1
	err := ValidateLedger(l)
	if err == nil {
		t.Fatal("epoch regression must be rejected")
	}
	if !strings.Contains(err.Error(), "regression") && !strings.Contains(err.Error(), "epoch") {
		t.Errorf("expected epoch regression error, got: %v", err)
	}
}

// TestValidateLedger_EpochJumpRejected 验证 epoch 跳号被拒绝：
// 新 epoch 必须 == 旧 epoch + 1（禁止 1 → 3 跳过 2）。
func TestValidateLedger_EpochJumpRejected(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass),
			mkEntry(2, ReviewStatusAcceptedInitial, ReviewVerdictPass),
			mkEntry(3, ReviewStatusInitialPending, ReviewVerdictPass),
		},
	}
	l.Cycles[0].Epoch = 1
	l.Cycles[1].Epoch = 1
	l.Cycles[2].Epoch = 3 // 跳号：1 → 3
	err := ValidateLedger(l)
	if err == nil {
		t.Fatal("epoch jump must be rejected")
	}
	if !strings.Contains(err.Error(), "epoch") {
		t.Errorf("expected epoch transition error, got: %v", err)
	}
}

// TestValidateLedger_NegativeEpochRejected 验证负数 epoch 被拒绝。
func TestValidateLedger_NegativeEpochRejected(t *testing.T) {
	l := validLedger(1, ReviewStatusInitialPending, ReviewVerdictPass)
	l.Cycles[0].Epoch = -1
	err := ValidateLedger(l)
	if err == nil {
		t.Fatal("negative epoch must be rejected")
	}
	if !strings.Contains(err.Error(), "epoch") {
		t.Errorf("expected epoch error, got: %v", err)
	}
}

// TestValidateLedger_NewEpochFromTerminalValid 验证合法的 epoch 边界：
// 返工队列章节从旧 epoch 的 terminal（accepted_initial）开启新 epoch（+1）初评合法。
func TestValidateLedger_NewEpochFromTerminalValid(t *testing.T) {
	l := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			mkEntry(1, ReviewStatusInitialPending, ReviewVerdictPass),
			mkEntry(2, ReviewStatusAcceptedInitial, ReviewVerdictPass),
			mkEntry(3, ReviewStatusInitialPending, ReviewVerdictPass),
		},
	}
	l.Cycles[0].Epoch = 1
	l.Cycles[1].Epoch = 1
	l.Cycles[2].Epoch = 2 // accepted_initial(epoch 1) → initial_pending(epoch 2)：合法
	if err := ValidateLedger(l); err != nil {
		t.Fatalf("new epoch from terminal should be valid: %v", err)
	}
}

// ── Stagnation: 不跨 epoch ────────────────────────────────────────────

// TestDetectStagnation_CrossEpochNotDetected 验证 stagnation 只扫描同 EpochValue 的
// cycle：新 epoch 的终审结果与旧 epoch 相同的 finding signature 不触发 exhausted
// （旧 epoch 权威不跨代延续）。
func TestDetectStagnation_CrossEpochNotDetected(t *testing.T) {
	d := testDraft
	b := testBasis
	reqInit := &StyleReviewRequest{Prompt: "init", Model: "m"}
	reqFinal := &StyleReviewRequest{Prompt: "final review", Model: "m"}
	findA := []StyleReviewFinding{{Dimension: "pacing", Category: "style", Severity: "warning", Problem: "问题A", Suggestion: "改法A"}}
	findB := []StyleReviewFinding{{Dimension: "hook", Category: "plot", Severity: "error", Problem: "问题B", Suggestion: "改法B"}}
	revResult := func(f []StyleReviewFinding) *StyleReviewResult {
		return &StyleReviewResult{Verdict: ReviewVerdictRevise, Evidence: "e", Findings: f}
	}
	ledger := &StyleReviewLedger{SchemaVersion: 1, Chapter: 1, Mode: StyleQualityCritic,
		Cycles: []StyleReviewEntry{
			// epoch 1：initial → revise(findA) → final → revise(findA) → exhausted(findA)
			{Cycle: 1, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T10:00:00Z", AttemptID: "a1", Request: reqInit, DraftDigest: d, BasisDigest: b, Epoch: 1},
			{Cycle: 2, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T11:00:00Z", AttemptID: "a1", Request: reqInit, Result: revResult(findA), DraftDigest: d, BasisDigest: b, Epoch: 1},
			{Cycle: 3, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T12:00:00Z", AttemptID: "a2", Request: reqFinal, DraftDigest: d, BasisDigest: b, Epoch: 1},
			{Cycle: 4, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T13:00:00Z", AttemptID: "a2", Request: reqFinal, Result: revResult(findA), DraftDigest: d, BasisDigest: b, Epoch: 1},
			{Cycle: 5, Status: ReviewStatusExhausted, CreatedAt: "2026-07-25T14:00:00Z", AttemptID: "a2", Request: reqFinal, Result: revResult(findA), DraftDigest: d, BasisDigest: b, Epoch: 1},
			// epoch 2：initial → revise(findB) → final（当前评审将产出 findA）
			{Cycle: 6, Status: ReviewStatusInitialPending, CreatedAt: "2026-07-25T15:00:00Z", AttemptID: "a3", Request: reqInit, DraftDigest: d, BasisDigest: b, Epoch: 2},
			{Cycle: 7, Status: ReviewStatusRevisionOpen, CreatedAt: "2026-07-25T16:00:00Z", AttemptID: "a3", Request: reqInit, Result: revResult(findB), DraftDigest: d, BasisDigest: b, Epoch: 2},
			{Cycle: 8, Status: ReviewStatusFinalPending, CreatedAt: "2026-07-25T17:00:00Z", AttemptID: "a4", Request: reqFinal, DraftDigest: d, BasisDigest: b, Epoch: 2},
		},
	}
	// 当前终审结果与 epoch 1 的 findA 相同——但跨 epoch，不得判为停滞。
	result := revResult(findA)
	if DetectFinalReviewStagnation(ledger, result) {
		t.Fatal("cross-epoch identical findings must NOT trigger stagnation")
	}
}
