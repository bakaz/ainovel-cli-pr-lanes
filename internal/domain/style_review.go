package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// ── StyleQualityMode ────────────────────────────────────────────────

type StyleQualityMode string

const (
	StyleQualityOff    StyleQualityMode = "off"
	StyleQualityCritic StyleQualityMode = "critic"
)

func (m StyleQualityMode) Valid() bool {
	return m == StyleQualityOff || m == StyleQualityCritic
}

func (m StyleQualityMode) Enabled() bool {
	return m == StyleQualityCritic
}

// ── StyleReviewStatus ───────────────────────────────────────────────

type StyleReviewStatus string

const (
	ReviewStatusInitialPending  StyleReviewStatus = "initial_pending"
	ReviewStatusAcceptedInitial StyleReviewStatus = "accepted_initial"
	ReviewStatusRevisionOpen    StyleReviewStatus = "revision_open"
	ReviewStatusFinalPending    StyleReviewStatus = "final_pending"
	ReviewStatusAcceptedRev     StyleReviewStatus = "accepted_revised"
	ReviewStatusExhausted       StyleReviewStatus = "exhausted"
	ReviewStatusDegraded        StyleReviewStatus = "degraded"
	ReviewStatusOverridden      StyleReviewStatus = "overridden"
)

// Exact V1 transition graph (one pre-commit cycle, at most two critic attempts):
//
//	initial_pending ──accepted_initial──→ [terminal]
//	initial_pending ──revision_open──────→ final_pending
//	initial_pending ──degraded───────────→ [terminal]
//	revision_open ──final_pending────────→ (continued)
//	final_pending ──accepted_revised─────→ [terminal]
//	final_pending ──exhausted────────────→ overridden
//	final_pending ──degraded─────────────→ [terminal]
//	exhausted ──overridden───────────────→ [terminal]

func (s StyleReviewStatus) Valid() bool {
	switch s {
	case ReviewStatusInitialPending, ReviewStatusAcceptedInitial, ReviewStatusRevisionOpen,
		ReviewStatusFinalPending, ReviewStatusAcceptedRev,
		ReviewStatusExhausted, ReviewStatusDegraded, ReviewStatusOverridden:
		return true
	default:
		return false
	}
}

func (s StyleReviewStatus) IsTerminal() bool {
	switch s {
	case ReviewStatusAcceptedInitial, ReviewStatusAcceptedRev,
		ReviewStatusDegraded, ReviewStatusOverridden:
		return true
	default:
		return false
	}
}

func (s StyleReviewStatus) IsActive() bool {
	switch s {
	case ReviewStatusInitialPending, ReviewStatusRevisionOpen, ReviewStatusFinalPending:
		return true
	default:
		return false
	}
}

// ── V1 transition table ─────────────────────────────────────────────

var v1Transitions = map[StyleReviewStatus]map[StyleReviewStatus]bool{
	ReviewStatusInitialPending: {
		ReviewStatusAcceptedInitial: true,
		ReviewStatusRevisionOpen:    true,
		ReviewStatusDegraded:        true,
	},
	ReviewStatusRevisionOpen: {
		ReviewStatusFinalPending: true,
	},
	ReviewStatusFinalPending: {
		ReviewStatusAcceptedRev: true,
		ReviewStatusExhausted:   true,
		ReviewStatusDegraded:    true,
	},
	ReviewStatusExhausted: {
		ReviewStatusOverridden: true,
	},
}

func isValidV1Transition(from, to StyleReviewStatus) bool {
	if m, ok := v1Transitions[from]; ok {
		return m[to]
	}
	return false
}

// ── StyleReviewVerdict ──────────────────────────────────────────────

type StyleReviewVerdict string

const (
	ReviewVerdictPass   StyleReviewVerdict = "pass"
	ReviewVerdictRevise StyleReviewVerdict = "revise"
)

func (v StyleReviewVerdict) Valid() bool {
	return v == ReviewVerdictPass || v == ReviewVerdictRevise
}

// ── Finding enums ───────────────────────────────────────────────────

const (
	FindingDimensionConsistency = "consistency"
	FindingDimensionCharacter   = "character"
	FindingDimensionPacing      = "pacing"
	FindingDimensionContinuity  = "continuity"
	FindingDimensionForeshadow  = "foreshadow"
	FindingDimensionHook        = "hook"
	FindingDimensionAesthetic   = "aesthetic"

	FindingSeverityCritical = "critical"
	FindingSeverityError    = "error"
	FindingSeverityWarning  = "warning"
	FindingSeverityInfo     = "info"

	FindingCategoryPlot    = "plot"
	FindingCategoryStyle   = "style"
	FindingCategoryLogic   = "logic"
	FindingCategoryTone    = "tone"
	FindingCategoryGrammar = "grammar"
)

var validFindingDimensions = map[string]bool{
	FindingDimensionConsistency: true,
	FindingDimensionCharacter:   true,
	FindingDimensionPacing:      true,
	FindingDimensionContinuity:  true,
	FindingDimensionForeshadow:  true,
	FindingDimensionHook:        true,
	FindingDimensionAesthetic:   true,
}

// ValidFindingDimension reports whether s is a valid finding/strength dimension.
func ValidFindingDimension(s string) bool {
	return validFindingDimensions[s]
}

var validFindingSeverities = map[string]bool{
	FindingSeverityCritical: true,
	FindingSeverityError:    true,
	FindingSeverityWarning:  true,
	FindingSeverityInfo:     true,
}

var validFindingCategories = map[string]bool{
	FindingCategoryPlot:    true,
	FindingCategoryStyle:   true,
	FindingCategoryLogic:   true,
	FindingCategoryTone:    true,
	FindingCategoryGrammar: true,
}

// ── StyleReviewFinding ──────────────────────────────────────────────

type StyleReviewFinding struct {
	Dimension  string `json:"dimension"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Evidence   string `json:"evidence"`
	Suggestion string `json:"suggestion,omitempty"`
}

func (f *StyleReviewFinding) Valid() bool {
	if f == nil {
		return false
	}
	if !validFindingDimensions[f.Dimension] {
		return false
	}
	if !validFindingSeverities[f.Severity] {
		return false
	}
	if !validFindingCategories[f.Category] {
		return false
	}
	if f.Evidence == "" {
		return false
	}
	return true
}

// ── StyleReviewRequest ──────────────────────────────────────────────

type StyleReviewRequest struct {
	Prompt       string `json:"prompt,omitempty"`
	PromptTrunc  bool   `json:"prompt_trunc,omitempty"`
	Model        string `json:"model,omitempty"`
	IncludeBasis bool   `json:"include_basis,omitempty"`
	RequestedAt  string `json:"requested_at,omitempty"`
}

const maxReviewRequestPromptBytes = 8 << 10

func (r *StyleReviewRequest) Normalize() {
	if r == nil {
		return
	}
	if len(r.Prompt) > maxReviewRequestPromptBytes {
		r.Prompt = r.Prompt[:maxReviewRequestPromptBytes]
		r.PromptTrunc = true
	}
}

// ── StyleReviewResult ───────────────────────────────────────────────

type StyleReviewResult struct {
	Verdict  StyleReviewVerdict   `json:"verdict"`
	Evidence string               `json:"evidence"`
	Findings []StyleReviewFinding `json:"findings,omitempty"`
}

func (r *StyleReviewResult) Valid() bool {
	if r == nil {
		return false
	}
	if !r.Verdict.Valid() {
		return false
	}
	if r.Evidence == "" {
		return false
	}
	if len(r.Findings) > 3 {
		return false
	}
	for i := range r.Findings {
		if !r.Findings[i].Valid() {
			return false
		}
	}
	return true
}

// ── StyleReviewOverride ─────────────────────────────────────────────

type StyleReviewOverride struct {
	Actor        string `json:"actor"`
	Reason       string `json:"reason"`
	DraftDigest  string `json:"draft_digest"`
	BasisDigest  string `json:"basis_digest"`
	OverriddenAt string `json:"overridden_at"`
}

// ── StyleReviewEntry ────────────────────────────────────────────────

type StyleReviewEntry struct {
	Cycle       int                  `json:"cycle"`
	Status      StyleReviewStatus    `json:"status"`
	AttemptID   string               `json:"attempt_id,omitempty"` // immutable attempt ID binding pending→completion
	Request     *StyleReviewRequest  `json:"request,omitempty"`
	Result      *StyleReviewResult   `json:"result,omitempty"`
	DraftDigest string               `json:"draft_digest,omitempty"` // sha256:<64hex>
	BasisDigest string               `json:"basis_digest,omitempty"` // sha256:<64hex>
	Error       string               `json:"error,omitempty"`
	Override    *StyleReviewOverride `json:"override,omitempty"`
	CreatedAt   string               `json:"created_at"` // RFC3339
}

// ── StyleReviewLedger ───────────────────────────────────────────────

const styleReviewSchemaVersion = 1

type StyleReviewLedger struct {
	SchemaVersion int                `json:"schema_version"`
	Chapter       int                `json:"chapter"`
	Mode          StyleQualityMode   `json:"mode"`
	Cycles        []StyleReviewEntry `json:"cycles,omitempty"`
}

// IsEmpty 报告账本是否不含任何周期（未开始状态）。
func (l *StyleReviewLedger) IsEmpty() bool {
	return len(l.Cycles) == 0
}

func (l *StyleReviewLedger) CurrentCycle() *StyleReviewEntry {
	if l.IsEmpty() {
		return nil
	}
	return &l.Cycles[len(l.Cycles)-1]
}

// CurrentStatus 返回最近周期状态。空账本返回空字符串（非 active/terminal）。
func (l *StyleReviewLedger) CurrentStatus() StyleReviewStatus {
	if c := l.CurrentCycle(); c != nil {
		return c.Status
	}
	return ""
}

// IsUnderReview 返回当前是否处于活跃评审中。空账本返回 false。
func (l *StyleReviewLedger) IsUnderReview() bool {
	return l.CurrentStatus().IsActive()
}

func (l *StyleReviewLedger) HasOverrides() bool {
	for i := range l.Cycles {
		if l.Cycles[i].Override != nil {
			return true
		}
	}
	return false
}

// DeepClone 返回账本的深度独立副本，包括所有嵌套指针和切片。
// 回调对副本的修改不会影响原始对象。
func (l *StyleReviewLedger) DeepClone() *StyleReviewLedger {
	if l == nil {
		return nil
	}
	cp := &StyleReviewLedger{
		SchemaVersion: l.SchemaVersion,
		Chapter:       l.Chapter,
		Mode:          l.Mode,
	}
	if l.Cycles != nil {
		cp.Cycles = make([]StyleReviewEntry, len(l.Cycles))
		for i, c := range l.Cycles {
			cp.Cycles[i] = c.deepCloneEntry()
		}
	}
	return cp
}

// deepCloneEntry returns a deep copy of the entry.
func (e StyleReviewEntry) deepCloneEntry() StyleReviewEntry {
	cp := e
	if e.Request != nil {
		r := *e.Request
		cp.Request = &r
	}
	if e.Result != nil {
		r := *e.Result
		if e.Result.Findings != nil {
			r.Findings = make([]StyleReviewFinding, len(e.Result.Findings))
			copy(r.Findings, e.Result.Findings)
		}
		cp.Result = &r
	}
	if e.Override != nil {
		o := *e.Override
		cp.Override = &o
	}
	return cp
}

// ── Digest helpers ─────────────────────────────────────────────────

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func IsValidDigest(s string) bool {
	return sha256DigestPattern.MatchString(s)
}

func DigestDraft(content string) string {
	h := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(h[:])
}

// ReviewBasis 是发送给批评者的规范评审依据 payload。
// 包含当前章节的完整结构化风格目标、契约、规则、锚点、
// 用户规则和大纲事实数据。序列化后既发送也给摘要检测变更。
type ReviewBasis struct {
	StyleGoal       *ChapterStyleGoal `json:"style_goal,omitempty"`
	ChapterContract *ChapterContract  `json:"chapter_contract,omitempty"`
	CompassProse    []string          `json:"compass_prose,omitempty"`
	CompassDialogue []CharacterVoice  `json:"compass_dialogue,omitempty"`
	CompassTaboos   []string          `json:"compass_taboos,omitempty"`
	AnchorExcerpts  []string          `json:"anchor_excerpts,omitempty"`
	UserRules       json.RawMessage   `json:"user_rules,omitempty"`
	FactualOutline  string            `json:"factual_outline"`
	CriticVersion   string            `json:"critic_version"`
}

// DigestReviewBasis 对完整规范 ReviewBasis 做确定性摘要。
func DigestReviewBasis(basis ReviewBasis) string {
	data, _ := json.Marshal(basis)
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ── RFC3339 validation ─────────────────────────────────────────────

func isValidRFC3339(s string) bool {
	if s == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// ── Entry deep-equality (for append-only prefix check) ──────────────

func requestEqual(a, b StyleReviewRequest) bool {
	return a.Prompt == b.Prompt && a.PromptTrunc == b.PromptTrunc &&
		a.Model == b.Model && a.IncludeBasis == b.IncludeBasis &&
		a.RequestedAt == b.RequestedAt
}

func resultEqual(a, b StyleReviewResult) bool {
	if a.Verdict != b.Verdict || a.Evidence != b.Evidence || len(a.Findings) != len(b.Findings) {
		return false
	}
	for i := range a.Findings {
		if a.Findings[i] != b.Findings[i] {
			return false
		}
	}
	return true
}

func overrideEqual(a, b StyleReviewOverride) bool {
	return a.Actor == b.Actor && a.Reason == b.Reason &&
		a.DraftDigest == b.DraftDigest && a.BasisDigest == b.BasisDigest &&
		a.OverriddenAt == b.OverriddenAt
}

// EntriesEqual reports whether two entries are deeply equal.
func EntriesEqual(a, b StyleReviewEntry) bool {
	if a.Cycle != b.Cycle || a.Status != b.Status || a.AttemptID != b.AttemptID ||
		a.DraftDigest != b.DraftDigest || a.BasisDigest != b.BasisDigest ||
		a.Error != b.Error || a.CreatedAt != b.CreatedAt {
		return false
	}
	if (a.Request == nil) != (b.Request == nil) {
		return false
	}
	if a.Request != nil && !requestEqual(*a.Request, *b.Request) {
		return false
	}
	if (a.Result == nil) != (b.Result == nil) {
		return false
	}
	if a.Result != nil && !resultEqual(*a.Result, *b.Result) {
		return false
	}
	if (a.Override == nil) != (b.Override == nil) {
		return false
	}
	if a.Override != nil && !overrideEqual(*a.Override, *b.Override) {
		return false
	}
	return true
}

// ── Validate ─────────────────────────────────────────────────────────

func ValidateLedger(l *StyleReviewLedger) error {
	if l == nil {
		return fmt.Errorf("ledger: nil")
	}
	if l.SchemaVersion != styleReviewSchemaVersion {
		return fmt.Errorf("ledger: unsupported schema version %d", l.SchemaVersion)
	}
	if l.Chapter <= 0 {
		return fmt.Errorf("ledger: chapter must be > 0, got %d", l.Chapter)
	}
	if !l.Mode.Valid() {
		return fmt.Errorf("ledger: invalid mode %q", l.Mode)
	}
	return validateCycleRules(l)
}

func validateCycleRules(l *StyleReviewLedger) error {
	// ── 0. Critic mode with zero cycles must fail (unstarted critic ledger invalid) ──
	if l.Mode == StyleQualityCritic && len(l.Cycles) == 0 {
		return fmt.Errorf("ledger: critic mode with zero cycles is invalid; use Update to create initial_pending")
	}
	if len(l.Cycles) == 0 {
		return nil
	}

	// ── 0a. Critic mode: first cycle must be initial_pending ──
	if l.Mode == StyleQualityCritic && l.Cycles[0].Status != ReviewStatusInitialPending {
		return fmt.Errorf("ledger: critic mode first cycle must be initial_pending, got %q", l.Cycles[0].Status)
	}

	// ── 1. Per-cycle payload validation ──
	for i, cycle := range l.Cycles {
		expected := i + 1
		if cycle.Cycle != expected {
			return fmt.Errorf("ledger: cycle[%d] number %d, want %d", i, cycle.Cycle, expected)
		}
		if !cycle.Status.Valid() {
			return fmt.Errorf("ledger: cycle[%d] invalid status %q", i, cycle.Status)
		}
		if !isValidRFC3339(cycle.CreatedAt) {
			return fmt.Errorf("ledger: cycle[%d] created_at not RFC3339: %q", i, cycle.CreatedAt)
		}
		if cycle.Error != "" && cycle.Result != nil {
			return fmt.Errorf("ledger: cycle[%d] has both error and result", i)
		}
		if cycle.Result != nil && !cycle.Result.Valid() {
			return fmt.Errorf("ledger: cycle[%d] invalid result", i)
		}
		if cycle.Request != nil {
			cycle.Request.Normalize()
			if cycle.Request.RequestedAt != "" && !isValidRFC3339(cycle.Request.RequestedAt) {
				return fmt.Errorf("ledger: cycle[%d] request requested_at not RFC3339: %q", i, cycle.Request.RequestedAt)
			}
		}

		switch cycle.Status {
		case ReviewStatusInitialPending:
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] initial_pending requires request", i)
			}
			if cycle.Request.Prompt == "" {
				return fmt.Errorf("ledger: cycle[%d] initial_pending requires non-blank prompt", i)
			}
			if cycle.Request.Model == "" {
				return fmt.Errorf("ledger: cycle[%d] initial_pending requires non-blank model", i)
			}
			if cycle.AttemptID == "" {
				return fmt.Errorf("ledger: cycle[%d] initial_pending requires attempt_id", i)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] initial_pending requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] initial_pending requires valid basis_digest", i)
			}
			if cycle.Result != nil {
				return fmt.Errorf("ledger: cycle[%d] initial_pending must not have result", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] initial_pending must not have error", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] initial_pending must not have override", i)
			}

		case ReviewStatusAcceptedInitial:
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial requires request", i)
			}
			if cycle.Result == nil {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial requires result", i)
			}
			if cycle.Result.Verdict != ReviewVerdictPass {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial requires verdict pass, got %q", i, cycle.Result.Verdict)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial requires valid basis_digest", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial must not have error", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] accepted_initial must not have override", i)
			}

		case ReviewStatusRevisionOpen:
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] revision_open requires request", i)
			}
			if cycle.Result == nil {
				return fmt.Errorf("ledger: cycle[%d] revision_open requires result", i)
			}
			if cycle.Result.Verdict != ReviewVerdictRevise {
				return fmt.Errorf("ledger: cycle[%d] revision_open requires verdict revise, got %q", i, cycle.Result.Verdict)
			}
			if len(cycle.Result.Findings) == 0 {
				return fmt.Errorf("ledger: cycle[%d] revision_open requires at least one finding with revise verdict", i)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] revision_open requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] revision_open requires valid basis_digest", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] revision_open must not have error", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] revision_open must not have override", i)
			}

		case ReviewStatusFinalPending:
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] final_pending requires request", i)
			}
			if cycle.Request.Prompt == "" {
				return fmt.Errorf("ledger: cycle[%d] final_pending requires non-blank prompt", i)
			}
			if cycle.Request.Model == "" {
				return fmt.Errorf("ledger: cycle[%d] final_pending requires non-blank model", i)
			}
			if cycle.AttemptID == "" {
				return fmt.Errorf("ledger: cycle[%d] final_pending requires attempt_id", i)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] final_pending requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] final_pending requires valid basis_digest", i)
			}
			if cycle.Result != nil {
				return fmt.Errorf("ledger: cycle[%d] final_pending must not have result", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] final_pending must not have error", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] final_pending must not have override", i)
			}

		case ReviewStatusAcceptedRev:
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised requires request", i)
			}
			if cycle.Result == nil {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised requires result", i)
			}
			if cycle.Result.Verdict != ReviewVerdictPass {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised requires verdict pass, got %q", i, cycle.Result.Verdict)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised requires valid basis_digest", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised must not have error", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] accepted_revised must not have override", i)
			}

		case ReviewStatusExhausted:
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] exhausted requires request", i)
			}
			if cycle.Result == nil {
				return fmt.Errorf("ledger: cycle[%d] exhausted requires result", i)
			}
			if cycle.Result.Verdict != ReviewVerdictRevise {
				return fmt.Errorf("ledger: cycle[%d] exhausted requires verdict revise, got %q", i, cycle.Result.Verdict)
			}
			if len(cycle.Result.Findings) == 0 {
				return fmt.Errorf("ledger: cycle[%d] exhausted requires at least one finding with revise verdict", i)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] exhausted requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] exhausted requires valid basis_digest", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] exhausted must not have error", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] exhausted must not have override", i)
			}

		case ReviewStatusDegraded:
			if cycle.Error == "" {
				return fmt.Errorf("ledger: cycle[%d] degraded requires non-empty error", i)
			}
			if cycle.Request == nil {
				return fmt.Errorf("ledger: cycle[%d] degraded requires request for audit", i)
			}
			if !IsValidDigest(cycle.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] degraded requires valid draft_digest", i)
			}
			if !IsValidDigest(cycle.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] degraded requires valid basis_digest", i)
			}
			if cycle.Result != nil {
				return fmt.Errorf("ledger: cycle[%d] degraded must not have result", i)
			}
			if cycle.Override != nil {
				return fmt.Errorf("ledger: cycle[%d] degraded must not have override", i)
			}

		case ReviewStatusOverridden:
			if cycle.Override == nil {
				return fmt.Errorf("ledger: cycle[%d] overridden requires override", i)
			}
			if cycle.Override.Actor == "" {
				return fmt.Errorf("ledger: cycle[%d] override missing actor", i)
			}
			if cycle.Override.Reason == "" {
				return fmt.Errorf("ledger: cycle[%d] override missing reason", i)
			}
			if !isValidRFC3339(cycle.Override.OverriddenAt) {
				return fmt.Errorf("ledger: cycle[%d] override overridden_at not RFC3339", i)
			}
			if !IsValidDigest(cycle.Override.DraftDigest) {
				return fmt.Errorf("ledger: cycle[%d] override draft_digest invalid", i)
			}
			if !IsValidDigest(cycle.Override.BasisDigest) {
				return fmt.Errorf("ledger: cycle[%d] override basis_digest invalid", i)
			}
			if cycle.Override.DraftDigest != cycle.DraftDigest {
				return fmt.Errorf("ledger: cycle[%d] override draft_digest %q != entry draft_digest %q", i, cycle.Override.DraftDigest, cycle.DraftDigest)
			}
			if cycle.Override.BasisDigest != cycle.BasisDigest {
				return fmt.Errorf("ledger: cycle[%d] override basis_digest %q != entry basis_digest %q", i, cycle.Override.BasisDigest, cycle.BasisDigest)
			}
			if cycle.Result != nil {
				return fmt.Errorf("ledger: cycle[%d] overridden must not have result", i)
			}
			if cycle.Error != "" {
				return fmt.Errorf("ledger: cycle[%d] overridden must not have error", i)
			}
		}
	}

	// ── 2. V1 transition edges ──
	for i := 0; i < len(l.Cycles)-1; i++ {
		from, to := l.Cycles[i].Status, l.Cycles[i+1].Status
		if !isValidV1Transition(from, to) {
			return fmt.Errorf("ledger: invalid V1 transition %q → %q at cycles [%d]→[%d]", from, to, i, i+1)
		}
	}

	// ── 3. Terminality ──
	for i, cycle := range l.Cycles {
		if i < len(l.Cycles)-1 && cycle.Status.IsTerminal() {
			return fmt.Errorf("ledger: cycle[%d] is terminal %q but has subsequent cycles", i, cycle.Status)
		}
	}

	// ── 4. Attempt-ID binding across pending→completion pairs ──
	for i := 0; i < len(l.Cycles)-1; i++ {
		curr, next := l.Cycles[i], l.Cycles[i+1]

		if curr.Status == ReviewStatusInitialPending {
			if next.AttemptID != curr.AttemptID {
				return fmt.Errorf("ledger: cycle[%d] initial_pending attempt_id %q != cycle[%d] %q", i, curr.AttemptID, i+1, next.AttemptID)
			}
			if next.DraftDigest != curr.DraftDigest {
				return fmt.Errorf("ledger: cycle[%d] initial_pending draft_digest changed in cycle[%d]", i, i+1)
			}
			if next.BasisDigest != curr.BasisDigest {
				return fmt.Errorf("ledger: cycle[%d] initial_pending basis_digest changed in cycle[%d]", i, i+1)
			}
			if (curr.Request == nil) != (next.Request == nil) {
				return fmt.Errorf("ledger: cycle[%d] initial_pending request presence differs in cycle[%d]", i, i+1)
			}
			if curr.Request != nil && next.Request != nil {
				// Normalize both before comparing to handle PromptTrunc consistently
				curr.Request.Normalize()
				next.Request.Normalize()
				if !requestEqual(*curr.Request, *next.Request) {
					return fmt.Errorf("ledger: cycle[%d] initial_pending request metadata differs from cycle[%d]", i, i+1)
				}
			}
		}

		if curr.Status == ReviewStatusFinalPending {
			if next.AttemptID != curr.AttemptID {
				return fmt.Errorf("ledger: cycle[%d] final_pending attempt_id %q != cycle[%d] %q", i, curr.AttemptID, i+1, next.AttemptID)
			}
			if next.DraftDigest != curr.DraftDigest {
				return fmt.Errorf("ledger: cycle[%d] final_pending draft_digest changed in cycle[%d]", i, i+1)
			}
			if next.BasisDigest != curr.BasisDigest {
				return fmt.Errorf("ledger: cycle[%d] final_pending basis_digest changed in cycle[%d]", i, i+1)
			}
			if (curr.Request == nil) != (next.Request == nil) {
				return fmt.Errorf("ledger: cycle[%d] final_pending request presence differs in cycle[%d]", i, i+1)
			}
			if curr.Request != nil && next.Request != nil {
				curr.Request.Normalize()
				next.Request.Normalize()
				if !requestEqual(*curr.Request, *next.Request) {
					return fmt.Errorf("ledger: cycle[%d] final_pending request metadata differs from cycle[%d]", i, i+1)
				}
			}
		}
	}

	// ── 5. Off-mode check: no active cycles ──
	if l.Mode == StyleQualityOff {
		for i, cycle := range l.Cycles {
			if cycle.Status.IsActive() {
				return fmt.Errorf("ledger: cycle[%d] has active status %q in off mode", i, cycle.Status)
			}
		}
	}

	return nil
}
