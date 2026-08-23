package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ── Shared test helpers ─────────────────────────────────────────────

var (
	testDraft = domain.DigestDraft("draft")
	testBasis = domain.DigestReviewBasis(domain.ReviewBasis{
		FactualOutline: "c",
		CriticVersion:  "v",
	})
	testFind = []domain.StyleReviewFinding{
		{Dimension: domain.FindingDimensionConsistency, Severity: domain.FindingSeverityWarning,
			Category: domain.FindingCategoryPlot, Evidence: "needs revision"},
	}
)

// initialPendingLedger creates a valid critic-mode ledger at initial_pending.
func initialPendingLedger() domain.StyleReviewLedger {
	return domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusInitialPending,
			CreatedAt:   "2026-07-25T10:00:00Z",
			AttemptID:   "a1",
			Request:     &domain.StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
			DraftDigest: testDraft, BasisDigest: testBasis,
		}},
	}
}

// appendRevisionOpen appends a revision_open cycle to a ledger.
func appendRevisionOpen(l *domain.StyleReviewLedger) {
	l.Cycles = append(l.Cycles, domain.StyleReviewEntry{
		Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
		CreatedAt:   "2026-07-25T11:00:00Z",
		AttemptID:   "a1",
		Request:     &domain.StyleReviewRequest{Prompt: "initial review", Model: "gpt-4o"},
		Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "needs work", Findings: testFind},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
}

// ── Load / Save sanity ──────────────────────────────────────────────

func TestStyleReviewStore_LoadNonExistentReturnsNil(t *testing.T) {
	s := NewStore(t.TempDir())
	ledger, err := s.StyleReview.Load(1)
	if err != nil || ledger != nil {
		t.Fatalf("expected nil,nil for non-existent: err=%v ledger=%+v", err, ledger)
	}
}

func TestStyleReviewStore_SaveAndLoad(t *testing.T) {
	s := NewStore(t.TempDir())
	ledger := initialPendingLedger()
	if err := s.StyleReview.Save(ledger); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := s.StyleReview.Load(1)
	if err != nil || loaded == nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Chapter != 1 || loaded.Cycles[0].Status != domain.ReviewStatusInitialPending {
		t.Errorf("unexpected: %+v", loaded)
	}
}

func TestStyleReviewStore_LoadRejectsChapterZero(t *testing.T) {
	s := NewStore(t.TempDir())
	_, err := s.StyleReview.Load(0)
	if err == nil {
		t.Fatal("expected error for chapter 0")
	}
}

// ── Save append-only: rejects existing ──────────────────────────────

func TestStyleReviewStore_SaveRejectsExisting(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}
	err := s.StyleReview.Save(l) // same chapter
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
}

// ── Chapter identity enforcement ────────────────────────────────────

func TestStyleReviewStore_LoadRejectsChapterMismatch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	path := filepath.Join(dir, "meta", "style_review", "01.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"schema_version":1,"chapter":99,"mode":"critic","cycles":[]}`), 0o644)
	_, err := s.StyleReview.Load(1)
	if err == nil || !strings.Contains(err.Error(), "chapter mismatch") {
		t.Fatalf("expected chapter mismatch error, got %v", err)
	}
}

func TestStyleReviewStore_UpdateRejectsChapterMismatch(t *testing.T) {
	s := NewStore(t.TempDir())
	err := s.StyleReview.Update(1, func(l *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		x := initialPendingLedger()
		x.Chapter = 2
		return &x, nil
	})
	if err == nil || !strings.Contains(err.Error(), "chapter mismatch") {
		t.Fatalf("expected chapter mismatch error, got %v", err)
	}
}

// ── Update append-only enforcement ──────────────────────────────────

func TestStyleReviewStore_UpdateAppendOneCycle(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		appendRevisionOpen(cur)
		return cur, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	loaded, _ := s.StyleReview.Load(1)
	if len(loaded.Cycles) != 2 || loaded.Cycles[1].Status != domain.ReviewStatusRevisionOpen {
		t.Fatalf("expected 2 cycles, got %d", len(loaded.Cycles))
	}
}

func TestStyleReviewStore_UpdateRejectsNoExtraCycle(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		return cur, nil // same as current, no new cycle
	})
	if err == nil || !strings.Contains(err.Error(), "must append exactly one") {
		t.Fatalf("expected must-append-one error, got %v", err)
	}
}

func TestStyleReviewStore_UpdateRejectsTooManyCycles(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		appendRevisionOpen(cur)
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle: 3, Status: domain.ReviewStatusFinalPending,
			CreatedAt: "2026-07-25T12:00:00Z",
			AttemptID: "a2", Request: &domain.StyleReviewRequest{Prompt: "final"},
			DraftDigest: testDraft, BasisDigest: testBasis,
		})
		return cur, nil // 2 new cycles instead of 1
	})
	if err == nil || !strings.Contains(err.Error(), "must append exactly one") {
		t.Fatalf("expected must-append-one error, got %v", err)
	}
}

func TestStyleReviewStore_UpdateRejectsHistoryRewrite(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		// Change the existing cycle's attempt_id
		cur.Cycles[0].AttemptID = "hacked"
		appendRevisionOpen(cur)
		return cur, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed cycle[0]") {
		t.Fatalf("expected history rewrite rejection, got %v", err)
	}
}

func TestStyleReviewStore_UpdateRejectsChangedPriorEntry(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		// Change the request prompt of the existing cycle
		cur.Cycles[0].Request.Prompt = "changed"
		appendRevisionOpen(cur)
		return cur, nil
	})
	// Either the append-only prefix check catches it (changed cycle[0]),
	// or the attempt-ID binding check catches it (request prompt changed).
	if err == nil {
		t.Fatal("expected rejection of history rewrite")
	}
	if !strings.Contains(err.Error(), "changed cycle[0]") &&
		!strings.Contains(err.Error(), "request prompt changed") {
		t.Fatalf("expected history rewrite or binding error, got %v", err)
	}
}

func TestStyleReviewStore_UpdateNoopNilReturn(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		return nil, nil // no-op
	})
	if err != nil {
		t.Fatalf("no-op should succeed: %v", err)
	}
}

// ── Stale append detection ──────────────────────────────────────────

func TestStyleReviewStore_UpdateStaleAppend(t *testing.T) {
	s := NewStore(t.TempDir())

	// Pre-populate with 2 cycles
	l := initialPendingLedger()
	appendRevisionOpen(&l)
	l.Chapter = 1
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}

	// Another client tries to append from stale state (1 cycle instead of 2)
	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		// cur has 2 cycles now, but simulate stale: ignore cur and return old data + 1
		stale := initialPendingLedger()
		appendRevisionOpen(&stale)
		return &stale, nil
	})
	if err == nil || !strings.Contains(err.Error(), "must append exactly one") {
		t.Fatalf("expected stale append rejection, got %v", err)
	}
}

// ── Concurrent append enforcement ───────────────────────────────────

func TestStyleReviewStore_UpdateConcurrentAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent test skipped in short mode")
	}
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}

	// 3 goroutines all try to append revision_open from the same starting state.
	// Only the first succeeds; others fail because after the first append the
	// ledger has 2 cycles (ending in revision_open) and appending another
	// revision_open violates both the V1 graph and append-only cycle count.
	done := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			done <- s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
				appendRevisionOpen(cur)
				return cur, nil
			})
		}()
	}

	results := make([]error, 3)
	for i := 0; i < 3; i++ {
		results[i] = <-done
	}

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 success, got %d", succeeded)
	}
	loaded, _ := s.StyleReview.Load(1)
	if len(loaded.Cycles) != 2 {
		t.Fatalf("expected 2 cycles, got %d", len(loaded.Cycles))
	}
}

// ── Malformed / corrupt ledger ──────────────────────────────────────

func TestStyleReviewStore_LoadRejectsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	p := filepath.Join(dir, "meta", "style_review", "01.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("not json"), 0o644)
	_, err := s.StyleReview.Load(1)
	if err == nil || !strings.Contains(err.Error(), "load chapter") {
		t.Fatalf("expected load error, got %v", err)
	}
}

func TestStyleReviewStore_LoadRejectsSemanticCorruption(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	p := filepath.Join(dir, "meta", "style_review", "01.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	data := `{"schema_version":1,"chapter":1,"mode":"off","cycles":[{"cycle":1,"status":"initial_pending","created_at":"2026-07-25T10:00:00Z","request":{"prompt":"x","model":"gpt-4o"},"attempt_id":"a1","draft_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","basis_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`
	os.WriteFile(p, []byte(data), 0o644)
	_, err := s.StyleReview.Load(1)
	if err == nil || !strings.Contains(err.Error(), "active status") {
		t.Fatalf("expected off-mode active rejection, got %v", err)
	}
}

// ── V1 graph corruption ─────────────────────────────────────────────

func TestStyleReviewStore_RejectsInvalidV1Transition(t *testing.T) {
	s := NewStore(t.TempDir())
	// Write initial_pending directly, then try Save with invalid follow-up
	l := initialPendingLedger()
	l.Cycles = append(l.Cycles, domain.StyleReviewEntry{
		Cycle: 2, Status: domain.ReviewStatusAcceptedRev,
		CreatedAt:   "2026-07-25T11:00:00Z",
		AttemptID:   "a2",
		Request:     &domain.StyleReviewRequest{Prompt: "final"},
		Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "e"},
		DraftDigest: testDraft, BasisDigest: testBasis,
	})
	err := s.StyleReview.Save(l)
	if err == nil || !strings.Contains(err.Error(), "invalid V2 transition") {
		t.Fatalf("expected transition error, got %v", err)
	}
}

// ── Exists ──────────────────────────────────────────────────────────

func TestStyleReviewStore_Exists(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if s.StyleReview.Exists(1) {
		t.Error("should not exist initially")
	}
	s.StyleReview.Save(initialPendingLedger())
	if !s.StyleReview.Exists(1) {
		t.Error("should exist after save")
	}
}

// ── Legacy / off compatibility ──────────────────────────────────────

func TestStyleReviewStore_LegacyProjectNoLedger(t *testing.T) {
	s := NewStore(t.TempDir())
	ledger, err := s.StyleReview.Load(1)
	if err != nil || ledger != nil {
		t.Fatal("expected nil for legacy")
	}
}

func TestStyleReviewStore_OffModeTerminalValid(t *testing.T) {
	s := NewStore(t.TempDir())
	l := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityOff,
		Cycles: []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusAcceptedInitial,
			CreatedAt:   "2026-07-25T10:00:00Z",
			Request:     &domain.StyleReviewRequest{Prompt: "x"},
			Result:      &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "archived"},
			DraftDigest: testDraft, BasisDigest: testBasis,
		}},
	}
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatalf("off-mode terminal should be savable: %v", err)
	}
}

// ── Deep-clone immutability: callback cannot alias through ──────────

func TestStyleReviewStore_UpdateCallbackMutationOfRequestIsRejected(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		// Mutate the request prompt on the received ledger
		cur.Cycles[0].Request.Prompt = "hacked"
		appendRevisionOpen(cur)
		return cur, nil
	})
	if err == nil {
		t.Fatal("expected rejection of mutated request")
	}
}

func TestStyleReviewStore_UpdateCallbackMutationOfResultIsRejected(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		// Mutate the HISTORICAL entry by injecting a result onto the pending cycle
		cur.Cycles[0].Result = &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "injected"}
		appendRevisionOpen(cur)
		return cur, nil
	})
	if err == nil {
		t.Fatal("expected rejection of mutated historical result")
	}
}

func TestStyleReviewStore_UpdateCallbackMutationOfOverrideIsRejected(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	s.StyleReview.Save(l)

	// Build up to revision_open
	s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		appendRevisionOpen(cur)
		return cur, nil
	})

	err := s.StyleReview.Update(1, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		// Mutate HISTORICAL entry by injecting an override onto cycle 0
		cur.Cycles[0].Override = &domain.StyleReviewOverride{
			Actor: "user", Reason: "injected", DraftDigest: testDraft, BasisDigest: testBasis, OverriddenAt: "2026-07-25T14:00:00Z",
		}
		// Now try to append final_pending (V1-valid follow-up)
		n := len(cur.Cycles) + 1
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle: n, Status: domain.ReviewStatusFinalPending, CreatedAt: "2026-07-25T14:00:00Z",
			AttemptID: "a2", Request: &domain.StyleReviewRequest{Prompt: "final", Model: "gpt-4o"},
			DraftDigest: testDraft, BasisDigest: testBasis,
		})
		return cur, nil
	})
	if err == nil {
		t.Fatal("expected rejection of mutated historical override")
	}
}

// ── Non-blank model/prompt ─────────────────────────────────────────

func TestStyleReviewStore_SaveRejectsMissingModel(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	l.Cycles[0].Request.Model = "" // remove model
	err := s.StyleReview.Save(l)
	if err == nil || !strings.Contains(err.Error(), "non-blank model") {
		t.Fatalf("expected non-blank model error, got %v", err)
	}
}

func TestStyleReviewStore_SaveRejectsMissingPrompt(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	l.Cycles[0].Request.Prompt = "" // remove prompt
	err := s.StyleReview.Save(l)
	if err == nil || !strings.Contains(err.Error(), "non-blank prompt") {
		t.Fatalf("expected non-blank prompt error, got %v", err)
	}
}

// ── Digest consistency ──────────────────────────────────────────────

func TestStyleReviewStore_DigestConsistency(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}
	loaded, _ := s.StyleReview.Load(1)
	if loaded.Cycles[0].DraftDigest != testDraft {
		t.Error("digest mismatch after roundtrip")
	}
}

// ── RunMeta mode normalization ──────────────────────────────────────

func TestRunMetaStyleReviewMode_EmptyNormalizedToOff(t *testing.T) {
	s := NewStore(t.TempDir())
	s.RunMeta.Save(domain.RunMeta{
		StartedAt: "2026-07-25T10:00:00Z", Style: "f", Model: "m", Provider: "p",
		StyleReviewMode: "",
	})
	loaded, _ := s.RunMeta.Load()
	if loaded.StyleReviewMode != domain.StyleQualityOff {
		t.Errorf("expected off, got %q", loaded.StyleReviewMode)
	}
}

func TestRunMetaStyleReviewMode_RejectsUnknownMode(t *testing.T) {
	s := NewStore(t.TempDir())
	s.RunMeta.Save(domain.RunMeta{
		StartedAt: "2026-07-25T10:00:00Z", Style: "f", Model: "m", Provider: "p",
		StyleReviewMode: "future-mode",
	})
	_, err := s.RunMeta.Load()
	if err == nil || !strings.Contains(err.Error(), "不支持的 style_review_mode") {
		t.Fatalf("expected compatibility error, got %v", err)
	}
}

func TestRunMetaStyleReviewMode_SetNormalizesEmpty(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.RunMeta.SetStyleReviewMode(""); err != nil {
		t.Fatal(err)
	}
	meta, _ := s.RunMeta.Load()
	if meta.StyleReviewMode != domain.StyleQualityOff {
		t.Errorf("expected off, got %q", meta.StyleReviewMode)
	}
}

// ── Style budget: event_kind 兼容与记录（计划 §9） ─────────────────────

// TestStyleReviewStore_LegacyLedgerWithoutEventKindLoads 验证旧数据（无
// event_kind 字段）读取不崩溃、按旧语义校验通过（legacy 兼容）。
func TestStyleReviewStore_LegacyLedgerWithoutEventKindLoads(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	p := filepath.Join(dir, "meta", "style_review", "01.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	// 旧格式 JSON：无 event_kind 字段。
	data := `{"schema_version":1,"chapter":1,"mode":"critic","cycles":[{"cycle":1,"status":"initial_pending","created_at":"2026-07-25T10:00:00Z","attempt_id":"a1","request":{"prompt":"x","model":"gpt-4o"},"draft_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","basis_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`
	os.WriteFile(p, []byte(data), 0o644)
	ledger, err := s.StyleReview.Load(1)
	if err != nil {
		t.Fatalf("legacy ledger without event_kind must load: %v", err)
	}
	if ledger == nil || ledger.Cycles[0].EventKind != "" {
		t.Fatalf("legacy entry event_kind must be empty, got %+v", ledger)
	}
}

// TestStyleReviewStore_EventKindRoundtrip 验证 event_kind 保存/读取往返一致。
func TestStyleReviewStore_EventKindRoundtrip(t *testing.T) {
	s := NewStore(t.TempDir())
	l := initialPendingLedger()
	appendRevisionOpen(&l)
	l.Cycles[1].EventKind = domain.ReviewEventContentRevise
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.StyleReview.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Cycles[1].EventKind != domain.ReviewEventContentRevise {
		t.Fatalf("event_kind roundtrip failed: got %q", loaded.Cycles[1].EventKind)
	}
}

// TestStyleReviewStore_StaleMarkerRecordsEventKindStale 验证 CAS stale 标记
// （markReviewStale）单独记录：CommitReviewResult 检测到草稿变更 → 追加
// degraded 周期且 EventKind=stale（不消耗内容预算）。
func TestStyleReviewStore_StaleMarkerRecordsEventKindStale(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	// 预写入 initial_pending（绑定草稿 v1 digest）。
	l := initialPendingLedger()
	if err := s.StyleReview.Save(l); err != nil {
		t.Fatal(err)
	}
	if err := s.Drafts.SaveDraft(1, "draft v1"); err != nil {
		t.Fatal(err)
	}

	// 草稿被并发修改为 v2 → CommitReviewResult 的 CAS 检测到 digest 漂移。
	if err := s.Drafts.SaveDraft(1, "draft v2"); err != nil {
		t.Fatal(err)
	}
	err := s.CommitReviewResult(1, "a1", testDraft, 0, nil, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		return cur, nil
	})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected ErrReviewStale, got %v", err)
	}

	loaded, err := s.StyleReview.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	last := loaded.CurrentCycle()
	if last.Status != domain.ReviewStatusDegraded {
		t.Fatalf("expected degraded stale marker, got %s", last.Status)
	}
	if last.EventKind != domain.ReviewEventStale {
		t.Fatalf("stale marker must record event_kind=stale, got %q", last.EventKind)
	}
}
