package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
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

// V2 transition graph (multi-cycle: revision_open can loop through final review):
//
//	initial_pending ──accepted_initial──→ [terminal]
//	initial_pending ──revision_open──────→ final_pending (via next review_style call)
//	initial_pending ──degraded───────────→ initial_pending (retry: degraded is a transient
//	                                         call failure, not a review verdict)
//	revision_open ──final_pending────────→ (via edit→check_consistency→review_style)
//	final_pending ──accepted_revised─────→ [terminal]
//	final_pending ──revision_open────────→ (loop: revise → edit → check → final_pending → revise → ...)
//	final_pending ──exhausted────────────→ (stagnation: repeated identical final findings;
//	                                         or final-revision count cap exceeded)
//	final_pending ──degraded─────────────→ final_pending (retry: degraded is a transient
//	                                         call failure, not a review verdict)
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

// v1Transitions covers the original V1 graph (pre-v2-upgrade) so that
// existing exhausted-holding ledgers remain valid after upgrade.
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
	ReviewStatusDegraded: {
		// degraded 是评审调用故障（瞬态），允许在其后发起新的评审 attempt 重试。
		ReviewStatusInitialPending: true,
		ReviewStatusFinalPending:   true,
	},
	ReviewStatusExhausted: {
		ReviewStatusOverridden: true,
	},
}

// v2Transitions is the V2 graph: final_pending revise loops back to revision_open.
var v2Transitions = map[StyleReviewStatus]map[StyleReviewStatus]bool{
	ReviewStatusInitialPending: {
		ReviewStatusAcceptedInitial: true,
		ReviewStatusRevisionOpen:    true,
		ReviewStatusDegraded:        true,
	},
	ReviewStatusRevisionOpen: {
		ReviewStatusFinalPending: true,
	},
	ReviewStatusFinalPending: {
		ReviewStatusAcceptedRev:  true,
		ReviewStatusRevisionOpen: true, // V2: revise → revision_open (loop)
		ReviewStatusDegraded:     true,
	},
	ReviewStatusDegraded: {
		// degraded 是评审调用故障（瞬态），允许在其后发起新的评审 attempt 重试。
		ReviewStatusInitialPending: true,
		ReviewStatusFinalPending:   true,
	},
	ReviewStatusExhausted: {
		ReviewStatusOverridden: true, // legacy only
	},
}

func isValidV2Transition(from, to StyleReviewStatus) bool {
	// Accept transition if allowed by V2 (preferred) or V1 (legacy fallback).
	if m, ok := v2Transitions[from]; ok && m[to] {
		return true
	}
	if m, ok := v1Transitions[from]; ok && m[to] {
		return true
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

// ── StyleReviewEventKind（style budget 账本，计划 §9）──────────────────
//
// 分类"产生该周期的评审事件"类型，用于区分内容计数（有效内容 revise，可触发
// exhausted）与技术计数（调用故障，不可触发 exhausted）。空值 = legacy（旧数据
// 无分类，按旧语义解释：degraded 视为技术失败，revision_open 视为内容 revise）。
// 仅审计/预算计数用，不影响状态机流转。

type StyleReviewEventKind string

const (
	// ReviewEventContentRevise 是有效内容 revise：style critic 对当前有效候选
	// 返回内容性 revise。内容计数 +1，可触发 exhausted。
	ReviewEventContentRevise StyleReviewEventKind = "content_revise"
	// ReviewEventPass 是 pass 判定。不消耗任何预算。
	ReviewEventPass StyleReviewEventKind = "pass"
	// ReviewEventTechnical 是技术失败：length/empty/EOF/network/timeout/
	// malformed JSON/audit 超限等调用故障。技术计数 +1，不可触发 exhausted。
	ReviewEventTechnical StyleReviewEventKind = "technical"
	// ReviewEventStale 是 CAS stale：评审候选过期（草稿/账本/polish checkpoint
	// 在 critic 调用期间被并发修改）。单独记录，不消耗内容预算也不消耗技术预算。
	ReviewEventStale StyleReviewEventKind = "stale"
	// ReviewEventCandidateOutOfBounds 是 Polisher 候选越界（包 6 polish 流水线
	// 记录，本包仅定义分类）。不消耗任何预算。
	ReviewEventCandidateOutOfBounds StyleReviewEventKind = "candidate_out_of_bounds"
	// ReviewEventOverride 是 user override（/style-override）。不消耗任何预算。
	ReviewEventOverride StyleReviewEventKind = "override"
)

// Valid 报告事件分类是否合法（空值 = legacy，由调用方按旧语义解释）。
func (k StyleReviewEventKind) Valid() bool {
	switch k {
	case ReviewEventContentRevise, ReviewEventPass, ReviewEventTechnical,
		ReviewEventStale, ReviewEventCandidateOutOfBounds, ReviewEventOverride:
		return true
	default:
		return false
	}
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
	Problem    string `json:"problem,omitempty"`
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
	// PolishCheckpointSeq 本次评审发起时绑定到的 polish checkpoint seq（该章最新
	// "polish" step 的 checkpoint seq）。用于 commit gate 的 seq 绑定校验：评审依据的
	// polish 不得晚于当前 polish candidate。0 = legacy（未走精修流水线）。
	PolishCheckpointSeq int64 `json:"polish_checkpoint_seq,omitempty"`
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

// FindingsSignature returns a deterministic hash of the normalized finding list
// (excluding Evidence which is citation text, not the issue identity).
// Used for stagnation detection: identical signatures across consecutive final
// reviews indicate the critic is raising the same issues despite edits.
// findingSigIdentity extracts the identity-bearing fields from a finding.
// Priority: dimension/category/severity + (problem+suggestion when non-empty),
// falling back to normalized evidence only when both problem and suggestion are empty.
// Normalization: trimmed, lowercased, single-spaced.
func findingSigIdentity(f StyleReviewFinding) string {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		// Normalize whitespace: trim + collapse runs to single space + lowercase
		s = strings.ToLower(s)
		re := regexp.MustCompile(`\s+`)
		return re.ReplaceAllString(s, " ")
	}
	major := norm(f.Problem) + "|" + norm(f.Suggestion)
	if major == "|" {
		major = norm(f.Evidence)
	}
	return norm(f.Dimension) + "|" + norm(f.Category) + "|" + norm(f.Severity) + "|" + major
}

// FindingsSignature returns a deterministic hash of the normalized finding list.
// Each finding's identity is determined by dimension/category/severity plus
// the descriptive identity (problem+suggestion, falling back to evidence when
// both are empty).  Used for stagnation detection: identical signatures across
// consecutive final reviews indicate the critic is raising the same issues.
func (r *StyleReviewResult) FindingsSignature() string {
	if r == nil || len(r.Findings) == 0 {
		return ""
	}
	// Extract identity strings, sort for determinism, hash.
	ids := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		ids[i] = findingSigIdentity(f)
	}
	sort.Strings(ids)
	data, _ := json.Marshal(ids)
	h := sha256.Sum256(data)
	return "findingsig:" + hex.EncodeToString(h[:8])
}

// isStrictAdjacentFinalRevise checks whether the ledgers i-th cycle is a
// revision_open that was produced by a final_pending → revise transition
// (i.e. the cycle before it is a final_pending).  Cycles[0] (initial
// revision_open) returns false.
func isStrictAdjacentFinalRevise(ledger *StyleReviewLedger, i int) bool {
	if i <= 0 || i >= len(ledger.Cycles) {
		return false
	}
	return ledger.Cycles[i].Status == ReviewStatusRevisionOpen &&
		ledger.Cycles[i-1].Status == ReviewStatusFinalPending
}

// DetectFinalReviewStagnation checks whether a final-review revise result is
// stagnant: the normalized finding signature matches the immediately preceding
// final_pending → revision_open cycle's result, meaning the critic returned
// the same issues despite an edit attempt.  When true the caller should
// produce exhausted instead of looping back to revision_open.
//
// C1（epoch 隔离）：只扫描与当前评审周期相同 EpochValue() 的历史 cycle——stagnation
// 绑定当前 epoch，旧 epoch 的相同 finding 不触发新返工周期的 exhausted。
//
// Only strictly adjacent final cycles are compared — the initial
// initial_pending → revision_open never triggers exhaustion.
//
// A nil/empty ledger, nil/empty result, or no prior final-revise cycle means
// no stagnation.
func DetectFinalReviewStagnation(ledger *StyleReviewLedger, currentResult *StyleReviewResult) bool {
	if currentResult == nil || ledger == nil || ledger.IsEmpty() {
		return false
	}
	currentSig := currentResult.FindingsSignature()
	if currentSig == "" {
		return false
	}
	// 当前评审周期 = 账本最大 epoch（新结果即将追加到该 epoch）。
	epoch := ledger.MaxEpoch()
	// Scan backwards for the most recent revision_open that was produced by a
	// strict adjacent final_pending → revision_open transition in the SAME epoch.
	for i := len(ledger.Cycles) - 1; i >= 0; i-- {
		if ledger.Cycles[i].EpochValue() != epoch {
			continue
		}
		if isStrictAdjacentFinalRevise(ledger, i) {
			entry := ledger.Cycles[i]
			if entry.Result != nil {
				prevSig := entry.Result.FindingsSignature()
				return prevSig != "" && prevSig == currentSig
			}
		}
	}
	return false
}

// MaxContentRevisionsPerEpoch 是同一评审 epoch 内有效内容 revise 上限（计划 §9）：
// 只有 style critic 对当前有效候选返回内容性 revise 才消耗内容预算；技术失败
// （length/empty/EOF/network/timeout/malformed JSON/audit 超限）、CAS stale、
// Polisher 候选越界与 user override 均不消耗内容预算，不得触发 exhausted。
const MaxContentRevisionsPerEpoch = 3

// ContentRevisionCount 统计当前评审 epoch 内的有效内容 revise 次数（内容预算，
// 计划 §9）。语义与既有 final-revision 计数对齐：只统计由 final_pending →
// revision_open 严格相邻转换产生的 revision_open 周期（"进入过 revision_open 后
// 再次 final review 返回 revise"的次数）。initial 评审的 revision_open
// （initial_pending → revision_open）不计入；degraded（技术失败/CAS stale）周期
// 不计入——技术失败不得消耗内容预算（核心修复目标）。
//
// 计数绑定当前 epoch（MaxEpoch，与即将追加的新周期同代）：返工队列章节开启
// 新 epoch 后从 0 重新计数（P1-7：每个评审 epoch 的 final revision 总数上限）。
//
// 与 DetectFinalReviewStagnation 叠加使用：同签名停滞立即 exhausted；不同
// finding 的振荡（critic 反复换问题要求修订）在轮次达到上限后同样 exhausted，
// 防止无限消耗 writer 轮次。
func ContentRevisionCount(ledger *StyleReviewLedger) int {
	if ledger == nil || ledger.IsEmpty() {
		return 0
	}
	epoch := ledger.MaxEpoch()
	count := 0
	for i := range ledger.Cycles {
		if ledger.Cycles[i].EpochValue() != epoch {
			continue
		}
		if isStrictAdjacentFinalRevise(ledger, i) {
			count++
		}
	}
	return count
}

// TechnicalFailureCount 统计当前评审 epoch 内的技术失败次数（技术预算，计划 §9）：
// length/empty/EOF/network/timeout/malformed JSON/audit 超限等调用故障。实现上
// 统计 degraded 周期中 EventKind 为 technical 或 legacy（空——旧数据无分类，按
// 旧语义视为技术失败）的条目。CAS stale（EventKind=stale）单独记录，不计入
// 技术计数。技术计数不触发 exhausted。
func TechnicalFailureCount(ledger *StyleReviewLedger) int {
	if ledger == nil || ledger.IsEmpty() {
		return 0
	}
	epoch := ledger.MaxEpoch()
	count := 0
	for i := range ledger.Cycles {
		c := ledger.Cycles[i]
		if c.EpochValue() != epoch || c.Status != ReviewStatusDegraded {
			continue
		}
		if c.EventKind == ReviewEventStale {
			continue
		}
		count++ // technical 或 legacy（空）
	}
	return count
}

// StaleCount 统计当前评审 epoch 内的 CAS stale 次数（单独记录，计划 §9）：
// degraded 周期中 EventKind=stale 的条目。stale 不消耗内容预算也不消耗技术预算。
func StaleCount(ledger *StyleReviewLedger) int {
	if ledger == nil || ledger.IsEmpty() {
		return 0
	}
	epoch := ledger.MaxEpoch()
	count := 0
	for i := range ledger.Cycles {
		c := ledger.Cycles[i]
		if c.EpochValue() != epoch || c.Status != ReviewStatusDegraded {
			continue
		}
		if c.EventKind == ReviewEventStale {
			count++
		}
	}
	return count
}

// ContentBudgetExhausted 报告当前评审 epoch 的内容预算是否耗尽：有效内容 revise
// 达到 MaxContentRevisionsPerEpoch 后，critic 再返回 revise 即进入 exhausted。
// 技术失败/CAS stale/override 均不影响本判定（技术失败不得错误触发 exhausted）。
func ContentBudgetExhausted(ledger *StyleReviewLedger) bool {
	return ContentRevisionCount(ledger) >= MaxContentRevisionsPerEpoch
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
	// Epoch 评审周期代数：同一 epoch 内按 V2 状态机流转；返工队列章节可从旧 epoch
	// 的 terminal 状态（accepted/revised/overridden；exhausted 须先经 /style-override
	// 覆盖为 overridden）开启新 epoch（Epoch = 旧 max + 1）重新评审。
	// 0 = legacy（读取时归一化为 1，见 StyleReviewStore.loadUnlocked）。
	Epoch int `json:"epoch,omitempty"`
	// EventKind 分类产生该周期的评审事件（style budget 账本，计划 §9）：
	// content_revise/pass/technical/stale/candidate_out_of_bounds/override。
	// 空 = legacy（旧数据无分类，按旧语义解释）。仅审计/预算计数用，不影响状态机
	// 流转；序列化 omitempty 保证旧数据读取兼容。
	EventKind StyleReviewEventKind `json:"event_kind,omitempty"`
}

// EpochValue 返回归一化后的 epoch：仅 0（legacy 数据/未设置）视为 1；
// 负数按非法数据处理（由 ValidateLedger 拒绝，不在本方法归一化）。
func (e StyleReviewEntry) EpochValue() int {
	if e.Epoch == 0 {
		return 1
	}
	return e.Epoch
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

// MaxEpoch 返回账本当前最大 epoch（归一化：无周期或全为 0 时返回 1）。
// 返工队列章节开启新评审周期时 Epoch = MaxEpoch + 1。
func (l *StyleReviewLedger) MaxEpoch() int {
	max := 1
	if l == nil {
		return max
	}
	for _, c := range l.Cycles {
		if e := c.EpochValue(); e > max {
			max = e
		}
	}
	return max
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
//
// 字段声明顺序 = JSON wire 顺序（encoding/json 按声明序输出 struct 字段）。
// 按 ora-1 缓存优化阶段 2（Prompt Capsule 重排）从稳定到动态排列：稳定书级
// 内容（prompt 版本/用户规则/长期 compass/锚点）在前，章节级动态内容
// （风格目标/章节契约/事实大纲）在后——跨 spawn 的内容前缀缓存（DeepSeek
// 磁盘缓存按内容前缀匹配）因此能在动态字段变化前命中尽量长的稳定前缀。
// 注意：字段声明顺序只影响 JSON wire 输出（前缀缓存），不影响摘要——
// DigestReviewBasis 使用与声明顺序无关的 canonical payload（按字段名排序），
// 因此重排字段不会使已落盘账本的 basis_digest 失效；字段集合不得增减
// （增减会改变 canonical 摘要，需显式迁移已落盘账本）。
type ReviewBasis struct {
	CriticVersion   string            `json:"critic_version"`
	UserRules       json.RawMessage   `json:"user_rules,omitempty"`
	CompassProse    []string          `json:"compass_prose,omitempty"`
	CompassDialogue []CharacterVoice  `json:"compass_dialogue,omitempty"`
	CompassTaboos   []string          `json:"compass_taboos,omitempty"`
	AnchorExcerpts  []string          `json:"anchor_excerpts,omitempty"`
	StyleGoal       *ChapterStyleGoal `json:"style_goal,omitempty"`
	ChapterContract *ChapterContract  `json:"chapter_contract,omitempty"`
	FactualOutline  string            `json:"factual_outline"`
}

// DigestReviewBasis 对完整规范 ReviewBasis 做确定性摘要。
//
// 摘要使用 canonical payload：以 map[string]any 重建全部字段后由
// encoding/json 按键名升序输出——结果只依赖字段内容与 JSON 标签名，
// 与字段声明顺序（JSON wire 排列）无关。缓存优化阶段重排 ReviewBasis
// 字段（ae540649 stable-first prompt capsule）因此不再改变摘要，已落盘
// 账本（pending basis_digest）在升级后依然匹配，不会误判 basis 漂移。
func DigestReviewBasis(basis ReviewBasis) string {
	data, _ := json.Marshal(canonicalReviewBasisPayload(basis))
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// canonicalReviewBasisPayload 重建 ReviewBasis 全字段为 map（不省略空值）。
// encoding/json 对 map 的字符串键按字典序升序输出，保证摘要与字段声明
// 顺序解耦；嵌套结构（ChapterStyleGoal/ChapterContract/CharacterVoice）
// 按各自结构体声明序输出，其字段顺序不受本处影响。
func canonicalReviewBasisPayload(basis ReviewBasis) map[string]any {
	return map[string]any{
		"anchor_excerpts":  basis.AnchorExcerpts,
		"chapter_contract": basis.ChapterContract,
		"compass_dialogue": basis.CompassDialogue,
		"compass_prose":    basis.CompassProse,
		"compass_taboos":   basis.CompassTaboos,
		"critic_version":   basis.CriticVersion,
		"factual_outline":  basis.FactualOutline,
		"style_goal":       basis.StyleGoal,
		"user_rules":       basis.UserRules,
	}
}

// legacyReviewBasisWire 是 ae540649（stable-first prompt capsule）之前旧版
// ReviewBasis 的 JSON wire 字段排列（动态在前、稳定在后）。仅用于复现旧版
// DigestReviewBasis 的整体 marshal 输出；不得再作为任何新摘要的输入。
type legacyReviewBasisWire struct {
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

// DigestReviewBasisLegacy 按旧版（ae540649 之前）字段排列计算 legacy 摘要，
// 仅用于升级期读取兼容：旧版落盘的 pending 账本 basis_digest 是按旧 wire
// 顺序整体 json.Marshal 计算的，恢复比对时需同时接受 canonical 与 legacy
// 两种摘要。新写入的 basis_digest 一律使用 DigestReviewBasis。
func DigestReviewBasisLegacy(basis ReviewBasis) string {
	data, _ := json.Marshal(legacyReviewBasisWire{
		StyleGoal:       basis.StyleGoal,
		ChapterContract: basis.ChapterContract,
		CompassProse:    basis.CompassProse,
		CompassDialogue: basis.CompassDialogue,
		CompassTaboos:   basis.CompassTaboos,
		AnchorExcerpts:  basis.AnchorExcerpts,
		UserRules:       basis.UserRules,
		FactualOutline:  basis.FactualOutline,
		CriticVersion:   basis.CriticVersion,
	})
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// BasisDigestMatches 判断账本已落盘 digest（recorded）与当前 basis 是否一致，
// 同时接受 canonical（DigestReviewBasis）与 legacy（DigestReviewBasisLegacy，
// 旧字段排列）两种算法——升级瞬间旧 pending 账本的 basis_digest 仍按旧算法
// 计算，双摘要接受可避免升级后恢复 pending 时误判 basis 漂移而降级。
// recorded 为空（历史条目未绑定）时视为一致，返回 true。
func BasisDigestMatches(basis ReviewBasis, recorded, current string) bool {
	if recorded == "" {
		return true
	}
	return current == recorded || DigestReviewBasisLegacy(basis) == recorded
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
		a.RequestedAt == b.RequestedAt && a.PolishCheckpointSeq == b.PolishCheckpointSeq
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
		a.Error != b.Error || a.CreatedAt != b.CreatedAt || a.Epoch != b.Epoch ||
		a.EventKind != b.EventKind {
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
		// C1-H3：epoch 负数非法（只有 0 允许被读取层归一化为 1）。
		if cycle.Epoch < 0 {
			return fmt.Errorf("ledger: cycle[%d] epoch must not be negative, got %d", i, cycle.Epoch)
		}
		// style budget（计划 §9）：event_kind 空 = legacy（旧数据无分类，允许）；
		// 非空必须是合法枚举，且与状态语义一致（fail-closed，防误分类导致
		// 技术失败被计入内容预算）。
		if cycle.EventKind != "" && !cycle.EventKind.Valid() {
			return fmt.Errorf("ledger: cycle[%d] invalid event_kind %q", i, cycle.EventKind)
		}
		switch cycle.Status {
		case ReviewStatusInitialPending, ReviewStatusFinalPending:
			// pending 周期不产生评审事件，不得携带 event_kind。
			if cycle.EventKind != "" {
				return fmt.Errorf("ledger: cycle[%d] %s must not have event_kind, got %q", i, cycle.Status, cycle.EventKind)
			}
		case ReviewStatusRevisionOpen, ReviewStatusExhausted:
			// revise 结果（含触发 exhausted 的第 4 次 revise）只可能是内容事件。
			if cycle.EventKind != "" && cycle.EventKind != ReviewEventContentRevise {
				return fmt.Errorf("ledger: cycle[%d] %s requires event_kind content_revise or empty, got %q", i, cycle.Status, cycle.EventKind)
			}
		case ReviewStatusAcceptedInitial, ReviewStatusAcceptedRev:
			if cycle.EventKind != "" && cycle.EventKind != ReviewEventPass {
				return fmt.Errorf("ledger: cycle[%d] %s requires event_kind pass or empty, got %q", i, cycle.Status, cycle.EventKind)
			}
		case ReviewStatusDegraded:
			// degraded 只可能是技术失败或 CAS stale（legacy 空值按技术失败解释）。
			if cycle.EventKind != "" && cycle.EventKind != ReviewEventTechnical && cycle.EventKind != ReviewEventStale {
				return fmt.Errorf("ledger: cycle[%d] degraded requires event_kind technical/stale or empty, got %q", i, cycle.EventKind)
			}
		case ReviewStatusOverridden:
			if cycle.EventKind != "" && cycle.EventKind != ReviewEventOverride {
				return fmt.Errorf("ledger: cycle[%d] overridden requires event_kind override or empty, got %q", i, cycle.EventKind)
			}
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

	// ── 2. V2 transition edges ──
	for i := 0; i < len(l.Cycles)-1; i++ {
		from, to := l.Cycles[i].Status, l.Cycles[i+1].Status
		fromEpoch, toEpoch := l.Cycles[i].EpochValue(), l.Cycles[i+1].EpochValue()
		if toEpoch > fromEpoch {
			// 新 epoch 边界：仅允许从旧 epoch 的 terminal / exhausted 状态开启新一轮
			// 初评（Epoch = 旧 max + 1），且只允许 +1（禁止跳号如 1→99）。
			// 同一 epoch 内不允许跨越状态机。
			if to == ReviewStatusInitialPending && (from.IsTerminal() || from == ReviewStatusExhausted) && toEpoch == fromEpoch+1 {
				continue
			}
			return fmt.Errorf("ledger: invalid epoch transition %q(epoch %d) → %q(epoch %d) at cycles [%d]→[%d]",
				from, fromEpoch, to, toEpoch, i, i+1)
		}
		if toEpoch < fromEpoch {
			// C1-H3：epoch 禁止倒退。
			return fmt.Errorf("ledger: epoch regression %q(epoch %d) → %q(epoch %d) at cycles [%d]→[%d]",
				from, fromEpoch, to, toEpoch, i, i+1)
		}
		if !isValidV2Transition(from, to) {
			return fmt.Errorf("ledger: invalid V2 transition %q → %q at cycles [%d]→[%d]", from, to, i, i+1)
		}
	}

	// ── 3. Terminality ──
	for i, cycle := range l.Cycles {
		if i < len(l.Cycles)-1 && cycle.Status.IsTerminal() {
			next := l.Cycles[i+1]
			// 新 epoch 边界：terminal 周期后允许开启新 epoch（旧 epoch 权威不跨代延续）。
			if next.EpochValue() > cycle.EpochValue() {
				continue
			}
			// degraded 是"评审调用故障"（瞬态技术故障，而非评审结论），允许在其后
			// 追加一个新的评审 attempt（initial_pending/final_pending）以便重试。
			// 其他 terminal 状态（accepted_initial/accepted_revised/overridden）是
			// 最终评审权威，不得再有后续周期。
			if cycle.Status == ReviewStatusDegraded {
				nextStatus := next.Status
				if nextStatus == ReviewStatusInitialPending || nextStatus == ReviewStatusFinalPending {
					continue
				}
			}
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
