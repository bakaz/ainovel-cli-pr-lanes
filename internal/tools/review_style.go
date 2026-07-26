package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── Rune-safe critic input bound ─────────────────────────────────────
//
// callCritic limits the draft sent to the critic to at most maxCriticRunes
// runes (not bytes). This avoids mid-rune truncation that could produce
// invalid UTF-8 or confuse the LLM. The bound is documented in the task
// text so the critic knows which portion it sees.
const maxCriticRunes = 12000

// ReviewStyleTool 是状态化风格评审工具。
// 非 ReadOnly，非 ConcurrencySafe。Writer 可调用。
type ReviewStyleTool struct {
	store            *store.Store
	criticRunner     *subagent.Runner
	criticPromptHash string // sha256 前缀：实际批评者提示词内容的可溯源标识
}

func NewReviewStyleTool(s *store.Store, criticRunner *subagent.Runner, criticPromptHash string) *ReviewStyleTool {
	return &ReviewStyleTool{store: s, criticRunner: criticRunner, criticPromptHash: criticPromptHash}
}

func (t *ReviewStyleTool) Name() string { return "review_style" }
func (t *ReviewStyleTool) Description() string {
	return "对章节草稿做文风评审。要求 critic 模式、已有草稿、最近一致性检查。" +
		"返回 pass/revise/degraded 判定及结构化发现。writer 可调用。"
}
func (t *ReviewStyleTool) Label() string { return "风格评审" }

func (t *ReviewStyleTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *ReviewStyleTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *ReviewStyleTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
	)
}

// StyleReviewOutput 是 review_style 的返回结构。
type StyleReviewOutput struct {
	Chapter  int                         `json:"chapter"`
	Verdict  string                      `json:"verdict"`
	Status   string                      `json:"status"`
	Evidence string                      `json:"evidence,omitempty"`
	Strength string                      `json:"strength,omitempty"`
	Findings []domain.StyleReviewFinding `json:"findings,omitempty"`
	Degraded bool                        `json:"degraded,omitempty"`
	Error    string                      `json:"error,omitempty"`
	Skipped  bool                        `json:"skipped,omitempty"`
	Reason   string                      `json:"reason,omitempty"`
}

// ── Production critic output shape ───────────────────────────────────

type criticOutput struct {
	Verdict  string          `json:"verdict"`
	Strength *criticStrength `json:"strength"`
	Findings []criticFinding `json:"findings,omitempty"`
}

type criticStrength struct {
	Dimension string `json:"dimension"`
	Evidence  string `json:"evidence"`
}

type criticFinding struct {
	Dimension string `json:"dimension"`
	Category  string `json:"category"`
	Severity  string `json:"severity"`
	Evidence  string `json:"evidence"`
	Problem   string `json:"problem,omitempty"`
	Revision  string `json:"revision,omitempty"`
}

// ── Execute ──────────────────────────────────────────────────────────

func (t *ReviewStyleTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	args = normalizeIntegerStringFields(args, "chapter")
	var a struct {
		Chapter int `json:"chapter"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}

	// ── 1. 检查 critic 模式 ──
	meta, err := t.store.RunMeta.Load()
	if err != nil {
		return nil, fmt.Errorf("load run meta: %w: %w", errs.ErrStoreRead, err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return json.Marshal(StyleReviewOutput{
			Chapter: a.Chapter,
			Skipped: true,
			Reason:  "style review not enabled (mode must be 'critic')",
		})
	}

	// ── 2. 加载草稿 ──
	content, wordCount, err := t.store.Drafts.LoadChapterContent(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}

	draftDigest := domain.DigestDraft(content)

	// ── 3. 校验一致性检查点 ──
	consistencyCP := t.store.Checkpoints.LatestByStep(domain.ChapterScope(a.Chapter), "consistency_check")
	if consistencyCP == nil {
		return nil, fmt.Errorf("必须先在章节 %d 上调用 check_consistency: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	if !domain.IsValidDigest(consistencyCP.Digest) || consistencyCP.Digest != draftDigest {
		return nil, fmt.Errorf("章节 %d 的草稿已变更或一致性检查点摘要无效，请重新调用 check_consistency: %w",
			a.Chapter, errs.ErrToolPrecondition)
	}

	// ── 4. 加载账本 ──
	ledger, err := t.store.StyleReview.Load(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load style review ledger: %w: %w", errs.ErrStoreRead, err)
	}

	// ── 5. 构建规范基础 payload（包含实际数据）并计算摘要 ──
	basis := t.buildReviewBasis(a.Chapter)
	basisDigest := t.computeBasisDigest(a.Chapter)

	// ── 6. 决定操作路径 ──
	var currentStatus domain.StyleReviewStatus
	if ledger == nil || ledger.IsEmpty() {
		currentStatus = ""
	} else {
		currentStatus = ledger.CurrentStatus()
	}

	if currentStatus.IsTerminal() {
		return nil, fmt.Errorf("章节 %d 风格评审已终结（%s），不能再发起新的评审: %w",
			a.Chapter, currentStatus, errs.ErrToolPrecondition)
	}

	if currentStatus == domain.ReviewStatusRevisionOpen {
		prevCycle := ledger.CurrentCycle()
		if prevCycle != nil && prevCycle.DraftDigest == draftDigest {
			return nil, fmt.Errorf("修订意见已给出但草稿未变更（摘要一致），请先修改草稿: %w",
				errs.ErrToolPrecondition)
		}
	}

	switch currentStatus {
	case "", domain.ReviewStatusInitialPending:
		return t.executeInitialReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger)
	case domain.ReviewStatusRevisionOpen, domain.ReviewStatusFinalPending:
		return t.executeFinalReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger)
	default:
		return nil, fmt.Errorf("章节 %d 帐本状态 %q 不支持 review_style: %w",
			a.Chapter, currentStatus, errs.ErrToolPrecondition)
	}
}

// ── Initial review ───────────────────────────────────────────────────

func (t *ReviewStyleTool) executeInitialReview(ctx context.Context, chapter int, content string, wordCount int, draftDigest, basisDigest string, basis domain.ReviewBasis, ledger *domain.StyleReviewLedger) (json.RawMessage, error) {
	now := time.Now().Format(time.RFC3339)
	criticModel := t.loadCriticModelName()

	var attemptID string
	var request *domain.StyleReviewRequest
	var pendingEntry domain.StyleReviewEntry

	if ledger != nil && !ledger.IsEmpty() && ledger.CurrentStatus() == domain.ReviewStatusInitialPending {
		cp := ledger.CurrentCycle()
		if cp != nil {
			attemptID = cp.AttemptID
			request = cp.Request
			pendingEntry = *cp

			// Stale-basis deadlock prevention: if the basis has drifted since
			// the pending attempt was created, degrade immediately with the
			// persisted authority rather than leaving a stranded pending entry.
			if cp.BasisDigest != "" && basisDigest != cp.BasisDigest {
				return t.appendDegraded(chapter, cp.AttemptID, cp.DraftDigest, cp.BasisDigest, request,
					fmt.Errorf("章节 %d 的评审基础已变更，初始评审待定（attempt %s）已失效，需要新的 check_consistency: %w",
						chapter, cp.AttemptID, errs.ErrToolPrecondition))
			}

			pendingEntry.DraftDigest = draftDigest
			pendingEntry.BasisDigest = basisDigest
		}
	}

	if attemptID == "" {
		attemptID = fmt.Sprintf("initial-%d-%d", chapter, time.Now().UnixNano())
		request = &domain.StyleReviewRequest{
			Prompt:       t.criticPromptHash,
			Model:        criticModel,
			IncludeBasis: true,
			RequestedAt:  now,
		}
		pendingEntry = domain.StyleReviewEntry{
			Cycle:       1,
			Status:      domain.ReviewStatusInitialPending,
			CreatedAt:   now,
			AttemptID:   attemptID,
			Request:     request,
			DraftDigest: draftDigest,
			BasisDigest: basisDigest,
		}

		pendingLedger := domain.StyleReviewLedger{
			SchemaVersion: 1,
			Chapter:       chapter,
			Mode:          domain.StyleQualityCritic,
			Cycles:        []domain.StyleReviewEntry{pendingEntry},
		}

		if !t.store.StyleReview.Exists(chapter) {
			if err := t.store.StyleReview.Save(pendingLedger); err != nil {
				return nil, fmt.Errorf("save initial pending ledger: %w", err)
			}
		} else {
			if err := t.store.StyleReview.Update(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
				if cur == nil || cur.IsEmpty() {
					return &pendingLedger, nil
				}
				if cur.CurrentStatus() == domain.ReviewStatusInitialPending {
					return nil, nil
				}
				return nil, fmt.Errorf("unexpected ledger state: %s", cur.CurrentStatus())
			}); err != nil {
				return nil, fmt.Errorf("append initial pending: %w", err)
			}
		}
	}

	result, degradedErr := t.callCritic(ctx, chapter, content, wordCount, basis)

	if degradedErr != nil {
		finalReq := request
		if finalReq == nil {
			finalReq = &domain.StyleReviewRequest{Prompt: t.criticPromptHash, Model: criticModel}
		}
		return t.appendDegraded(chapter, attemptID, pendingEntry.DraftDigest, pendingEntry.BasisDigest, finalReq, degradedErr)
	}

	return t.appendInitialResult(chapter, attemptID, request, result, draftDigest, basisDigest)
}

// ── Final review ─────────────────────────────────────────────────────

func (t *ReviewStyleTool) executeFinalReview(ctx context.Context, chapter int, content string, wordCount int, draftDigest, basisDigest string, basis domain.ReviewBasis, ledger *domain.StyleReviewLedger) (json.RawMessage, error) {
	now := time.Now().Format(time.RFC3339)
	criticModel := t.loadCriticModelName()

	var attemptID string
	var request *domain.StyleReviewRequest

	if ledger != nil && !ledger.IsEmpty() && ledger.CurrentStatus() == domain.ReviewStatusFinalPending {
		cp := ledger.CurrentCycle()
		if cp != nil {
			attemptID = cp.AttemptID
			request = cp.Request

			// Stale-basis deadlock prevention: if the basis has drifted since
			// the final pending attempt was created, degrade immediately.
			if cp.BasisDigest != "" && basisDigest != cp.BasisDigest {
				return t.appendDegraded(chapter, cp.AttemptID, cp.DraftDigest, cp.BasisDigest, request,
					fmt.Errorf("章节 %d 的评审基础已变更，最终评审待定（attempt %s）已失效，需要新的 check_consistency: %w",
						chapter, cp.AttemptID, errs.ErrToolPrecondition))
			}
		}
	}

	if attemptID == "" {
		attemptID = fmt.Sprintf("final-%d-%d", chapter, time.Now().UnixNano())
		request = &domain.StyleReviewRequest{
			Prompt:       t.criticPromptHash,
			Model:        criticModel,
			IncludeBasis: true,
			RequestedAt:  now,
		}

		if err := t.store.StyleReview.Update(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
			if cur == nil {
				return nil, fmt.Errorf("ledger disappeared for chapter %d", chapter)
			}
			nextCycle := len(cur.Cycles) + 1
			cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
				Cycle:       nextCycle,
				Status:      domain.ReviewStatusFinalPending,
				CreatedAt:   now,
				AttemptID:   attemptID,
				Request:     request,
				DraftDigest: draftDigest,
				BasisDigest: basisDigest,
			})
			return cur, nil
		}); err != nil {
			return nil, fmt.Errorf("append final pending: %w", err)
		}
	}

	result, degradedErr := t.callCritic(ctx, chapter, content, wordCount, basis)

	if degradedErr != nil {
		finalReq := request
		if finalReq == nil {
			finalReq = &domain.StyleReviewRequest{Prompt: t.criticPromptHash, Model: criticModel}
		}
		return t.appendDegraded(chapter, attemptID, draftDigest, basisDigest, finalReq, degradedErr)
	}

	return t.appendFinalResult(chapter, attemptID, request, result, draftDigest, basisDigest)
}

// ── Critic invocation and result validation ──────────────────────────

// callCritic invokes the critic subagent, parses the production JSON shape,
// validates all enums, and returns a domain.StyleReviewResult only if every
// mapped field is valid. Any failure (network, JSON, enum, missing strength)
// returns an error which callers MUST map to a degraded ledger entry.
func (t *ReviewStyleTool) callCritic(ctx context.Context, chapter int, content string, wordCount int, basis domain.ReviewBasis) (*domain.StyleReviewResult, error) {
	// ── Rune-safe bounded draft ──
	runeCount := utf8.RuneCountInString(content)
	draftForCritic := content
	truncated := false
	if runeCount > maxCriticRunes {
		truncated = true
		var sb strings.Builder
		sb.Grow(maxCriticRunes * 4)
		i := 0
		for _, r := range content {
			if i >= maxCriticRunes {
				break
			}
			sb.WriteRune(r)
			i++
		}
		draftForCritic = sb.String()
	}

	// ── Serialize the same canonical basis that the digest covers ──
	basisJSON, _ := json.Marshal(basis)
	basisPayload := string(basisJSON)

	truncNote := ""
	if truncated {
		truncNote = fmt.Sprintf("（草稿共 %d runes，仅发送前 %d runes）", runeCount, maxCriticRunes)
	}

	taskText := fmt.Sprintf(`## 评审任务

### 章节
第 %d 章

### 草稿（字数：%d）%s
%s

### 评审依据
%s

请严格按样式批评者提示词中定义的 JSON 格式输出（含 mandatory strength.evidence）。`,
		chapter, wordCount, truncNote, draftForCritic, basisPayload)

	runResult, err := t.criticRunner.Run(ctx, "style_critic", taskText)
	if err != nil {
		return nil, fmt.Errorf("critic call failed: %w", err)
	}

	outputText := runResult.Output
	if outputText == "" && runResult.TerminalResult != nil {
		outputText = string(runResult.TerminalResult)
	}
	if outputText == "" {
		return nil, fmt.Errorf("critic returned empty output")
	}

	// ── Decode production shape ──
	var co criticOutput
	parseErr := json.Unmarshal([]byte(outputText), &co)
	if parseErr != nil {
		if runResult.TerminalResult != nil {
			if err2 := json.Unmarshal(runResult.TerminalResult, &co); err2 != nil {
				return nil, fmt.Errorf("critic output decode failed: output=%q err=%w", truncateForLog(outputText, 200), parseErr)
			}
		} else {
			return nil, fmt.Errorf("critic output decode failed: output=%q err=%w", truncateForLog(outputText, 200), parseErr)
		}
	}

	// ── Validations before any ledger mutation ──

	// 1. Verdict must be valid
	verdict := domain.StyleReviewVerdict(strings.TrimSpace(co.Verdict))
	if !verdict.Valid() {
		return nil, fmt.Errorf("invalid verdict %q from critic (must be 'pass' or 'revise')", co.Verdict)
	}

	// 2. Mandatory strength.evidence and strength.dimension
	if co.Strength == nil || strings.TrimSpace(co.Strength.Evidence) == "" {
		return nil, fmt.Errorf("critic output missing mandatory strength.evidence")
	}
	strengthDimension := strings.TrimSpace(co.Strength.Dimension)
	if strengthDimension == "" || !domain.ValidFindingDimension(strengthDimension) {
		return nil, fmt.Errorf("critic output has invalid strength.dimension %q", strengthDimension)
	}
	strengthEvidence := strings.TrimSpace(co.Strength.Evidence)

	// 3. Build and validate findings — reject on any invalid field, enforce max 3
	if len(co.Findings) > 3 {
		return nil, fmt.Errorf("critic returned %d findings, maximum is 3", len(co.Findings))
	}
	var findings []domain.StyleReviewFinding
	for _, f := range co.Findings {
		finding := domain.StyleReviewFinding{
			Dimension:  strings.TrimSpace(f.Dimension),
			Category:   strings.TrimSpace(f.Category),
			Severity:   strings.TrimSpace(f.Severity),
			Evidence:   strings.TrimSpace(f.Evidence),
			Problem:    strings.TrimSpace(f.Problem),
			Suggestion: strings.TrimSpace(f.Revision),
		}
		if !finding.Valid() {
			return nil, fmt.Errorf("critic returned invalid finding: dimension=%q category=%q severity=%q evidence=%q",
				finding.Dimension, finding.Category, finding.Severity, finding.Evidence)
		}
		findings = append(findings, finding)
	}

	// 4. Revise verdict requires at least one finding
	if verdict == domain.ReviewVerdictRevise && len(findings) == 0 {
		return nil, fmt.Errorf("critic returned revise with no findings")
	}

	// 5. Build final domain.StyleReviewResult and validate before returning
	result := &domain.StyleReviewResult{
		Verdict:  verdict,
		Evidence: strengthEvidence,
		Findings: findings,
	}
	if !result.Valid() {
		return nil, fmt.Errorf("constructed StyleReviewResult failed domain validation")
	}

	return result, nil
}

// ── Append results ───────────────────────────────────────────────────

func (t *ReviewStyleTool) appendInitialResult(chapter int, attemptID string, request *domain.StyleReviewRequest, result *domain.StyleReviewResult, draftDigest, basisDigest string) (json.RawMessage, error) {
	var nextStatus domain.StyleReviewStatus
	switch result.Verdict {
	case domain.ReviewVerdictPass:
		nextStatus = domain.ReviewStatusAcceptedInitial
	case domain.ReviewVerdictRevise:
		nextStatus = domain.ReviewStatusRevisionOpen
	default:
		return t.appendDegraded(chapter, attemptID, draftDigest, basisDigest, request, fmt.Errorf("unexpected verdict %q", result.Verdict))
	}

	now := time.Now().Format(time.RFC3339)
	if err := t.store.StyleReview.Update(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, fmt.Errorf("ledger disappeared during update")
		}
		nextCycle := len(cur.Cycles) + 1
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle:       nextCycle,
			Status:      nextStatus,
			CreatedAt:   now,
			AttemptID:   attemptID,
			Request:     request,
			Result:      result,
			DraftDigest: draftDigest,
			BasisDigest: basisDigest,
		})
		return cur, nil
	}); err != nil {
		return nil, fmt.Errorf("append initial result: %w", err)
	}

	return t.buildSuccessOutput(chapter, result, nextStatus)
}

func (t *ReviewStyleTool) appendFinalResult(chapter int, attemptID string, request *domain.StyleReviewRequest, result *domain.StyleReviewResult, draftDigest, basisDigest string) (json.RawMessage, error) {
	var nextStatus domain.StyleReviewStatus
	switch result.Verdict {
	case domain.ReviewVerdictPass:
		nextStatus = domain.ReviewStatusAcceptedRev
	case domain.ReviewVerdictRevise:
		nextStatus = domain.ReviewStatusRevisionOpen // V2: loop back to revision_open by default
	default:
		return nil, fmt.Errorf("unexpected verdict %q for final review", result.Verdict)
	}

	now := time.Now().Format(time.RFC3339)
	if err := t.store.StyleReview.Update(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, fmt.Errorf("ledger disappeared during update")
		}

		// V2: detect stagnation — same finding signature as previous
		// final_revise → revision_open → exhausted to prevent infinite loops.
		if nextStatus == domain.ReviewStatusRevisionOpen {
			if domain.DetectFinalReviewStagnation(cur, result) {
				nextStatus = domain.ReviewStatusExhausted
			}
		}

		req := request
		if req == nil {
			for i := len(cur.Cycles) - 1; i >= 0; i-- {
				if cur.Cycles[i].Request != nil {
					req = cur.Cycles[i].Request
					break
				}
			}
		}
		nextCycle := len(cur.Cycles) + 1
		cur.Cycles = append(cur.Cycles, domain.StyleReviewEntry{
			Cycle:       nextCycle,
			Status:      nextStatus,
			CreatedAt:   now,
			AttemptID:   attemptID,
			Request:     req,
			Result:      result,
			DraftDigest: draftDigest,
			BasisDigest: basisDigest,
		})
		return cur, nil
	}); err != nil {
		return nil, fmt.Errorf("append final result: %w", err)
	}

	return t.buildSuccessOutput(chapter, result, nextStatus)
}

// appendDegraded 追加 valid degraded terminal entry，永不 strand pending。
func (t *ReviewStyleTool) appendDegraded(chapter int, attemptID string, draftDigest, basisDigest string, request *domain.StyleReviewRequest, cause error) (json.RawMessage, error) {
	now := time.Now().Format(time.RFC3339)

	req := request
	if req == nil {
		req = &domain.StyleReviewRequest{Prompt: t.criticPromptHash, Model: t.loadCriticModelName()}
	}

	entry := domain.StyleReviewEntry{
		Status:      domain.ReviewStatusDegraded,
		CreatedAt:   now,
		AttemptID:   attemptID,
		Request:     req,
		DraftDigest: draftDigest,
		BasisDigest: basisDigest,
		Error:       cause.Error(),
	}

	if err := t.store.StyleReview.Update(chapter, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, nil
		}
		entry.Cycle = len(cur.Cycles) + 1
		cur.Cycles = append(cur.Cycles, entry)
		return cur, nil
	}); err != nil {
		return nil, fmt.Errorf("append degraded: %w", err)
	}

	return json.Marshal(StyleReviewOutput{
		Chapter:  chapter,
		Verdict:  "degraded",
		Status:   string(domain.ReviewStatusDegraded),
		Degraded: true,
		Error:    cause.Error(),
	})
}

func (t *ReviewStyleTool) buildSuccessOutput(chapter int, result *domain.StyleReviewResult, status domain.StyleReviewStatus) (json.RawMessage, error) {
	output := StyleReviewOutput{
		Chapter:  chapter,
		Verdict:  string(result.Verdict),
		Status:   string(status),
		Evidence: result.Evidence,
	}
	if len(result.Findings) > 0 {
		output.Findings = result.Findings
	}
	return json.Marshal(output)
}

// ── Canonical basis payload ──────────────────────────────────────────

// buildReviewBasis 加载全部实际章节数据构建规范基础 payload。
// 各字段包含实际内容而非标识符——序列化后既发送给 critic 作为依据，
// 也作为摘要输入——commit gate 通过重新计算相同摘要来检测任意输入变更。
// 使用的 compass 作用域逻辑与 Writer context 一致（scopedCompassForChapter）。
func buildReviewBasis(st *store.Store, chapter int, criticPromptHash string) domain.ReviewBasis {
	prose, dialogue, taboos := loadScopedCompassProseDialogueTaboos(st, chapter)
	return domain.ReviewBasis{
		StyleGoal:       loadChapterStyleGoal(st, chapter),
		ChapterContract: loadChapterContract(st, chapter),
		CompassProse:    prose,
		CompassDialogue: dialogue,
		CompassTaboos:   taboos,
		AnchorExcerpts:  loadAnchorExcerpts(st, chapter),
		UserRules:       loadUserRulesJSON(st),
		FactualOutline:  loadFactualOutline(st, chapter),
		CriticVersion:   criticPromptHash,
	}
}

// loadScopedCompassProseDialogueTaboos loads the compass with chapter-scoped
// current section (same logic as Writer context) and returns the merged
// prose/dialogue/taboos lists (long + scoped current).
func loadScopedCompassProseDialogueTaboos(st *store.Store, chapter int) (prose []string, dialogue []domain.CharacterVoice, taboos []string) {
	scoped := scopedCompassForChapter(st, chapter, nil)
	if scoped == nil {
		return nil, nil, nil
	}
	// Long always included
	if scoped.Long != nil {
		prose = append(prose, scoped.Long.Prose...)
		taboos = append(taboos, scoped.Long.Taboos...)
		seen := make(map[string]bool)
		for _, v := range scoped.Long.Dialogue {
			if !seen[v.Name] {
				dialogue = append(dialogue, v)
				seen[v.Name] = true
			}
		}
		// Current supplements (only present if scoping allowed it)
		if scoped.Current != nil {
			prose = append(prose, scoped.Current.Prose...)
			taboos = append(taboos, scoped.Current.Taboos...)
			for _, v := range scoped.Current.Dialogue {
				if !seen[v.Name] {
					dialogue = append(dialogue, v)
					seen[v.Name] = true
				}
			}
		}
	} else if scoped.Current != nil {
		// No Long, only Current
		prose = scoped.Current.Prose
		taboos = scoped.Current.Taboos
		dialogue = scoped.Current.Dialogue
	}
	return
}

func (t *ReviewStyleTool) buildReviewBasis(chapter int) domain.ReviewBasis {
	return buildReviewBasis(t.store, chapter, t.criticPromptHash)
}

func (t *ReviewStyleTool) computeBasisDigest(chapter int) string {
	return ComputeBasisDigest(t.store, chapter, t.criticPromptHash)
}

// ComputeBasisDigest 是 review_style 与 CheckCommitStyleGate 共享的
// 基础摘要计算函数。使用与 buildReviewBasis 相同的数据源计算确定性摘要。
func ComputeBasisDigest(st *store.Store, chapter int, criticPromptHash string) string {
	basis := buildReviewBasis(st, chapter, criticPromptHash)
	return domain.DigestReviewBasis(basis)
}

// ── Model identity ───────────────────────────────────────────────────

func (t *ReviewStyleTool) loadCriticModelName() string {
	cfg, ok := t.criticRunner.AgentConfig("style_critic")
	if !ok {
		return "unknown"
	}
	if cfg.Model == nil {
		return "unknown"
	}
	if mn, ok2 := cfg.Model.(agentcore.ModelNamer); ok2 {
		if name := mn.ModelName(); name != "" {
			return name
		}
	}
	return "unknown"
}

// ── Data loading helpers ─────────────────────────────────────────────

// loadChapterStyleGoal 加载当前章节的 typed StyleGoal（来自 ChapterPlan）。
// nil 表示无风格目标（兼容旧数据或用户未设定）。
func loadChapterStyleGoal(st *store.Store, chapter int) *domain.ChapterStyleGoal {
	plan, err := st.Drafts.LoadChapterPlan(chapter)
	if err != nil || plan == nil {
		return nil
	}
	return plan.StyleGoal
}

// loadChapterContract 加载当前章节的 ChapterContract（来自 ChapterPlan）。
// nil 表示无契约。
func loadChapterContract(st *store.Store, chapter int) *domain.ChapterContract {
	plan, err := st.Drafts.LoadChapterPlan(chapter)
	if err != nil || plan == nil {
		return nil
	}
	return &plan.Contract
}

// loadAnchorExcerpts 加载与当前章节匹配的锚点 excerpts（bounded projection）。
// 使用与 Writer context 相同的章节过滤逻辑（ToInjectionView），
// 截断使用 rune-safe 方式，永不 byte-slice。
func loadAnchorExcerpts(st *store.Store, chapter int) []string {
	result := st.StyleAnchors.LoadManual()
	if result.Anchors == nil {
		return nil
	}
	injection := result.Anchors.ToInjectionView(chapter)
	var excerpts []string
	totalRunes := 0
	for _, item := range injection {
		snippet := item.Excerpt
		runes := []rune(snippet)
		if len(runes) > 200 {
			runes = runes[:200]
			snippet = string(runes)
		}
		excerpts = append(excerpts, snippet)
		totalRunes += utf8.RuneCountInString(snippet)
		if totalRunes > 2000 {
			excerpts = append(excerpts, "...(more)")
			break
		}
	}
	return excerpts
}

// loadUserRulesJSON 加载用户规则作为 JSON RawMessage。
func loadUserRulesJSON(st *store.Store) json.RawMessage {
	rules, err := st.UserRules.Load()
	if err != nil || rules == nil {
		return nil
	}
	data, _ := json.Marshal(rules)
	return json.RawMessage(data)
}

// loadFactualOutline 加载该章的实际大纲/事实数据作为 faithful bounded projection。
func loadFactualOutline(st *store.Store, chapter int) string {
	// 优先分层大纲
	volumes, vErr := st.Outline.LoadLayeredOutline()
	if vErr == nil && len(volumes) > 0 {
		for _, vol := range volumes {
			for _, arc := range vol.Arcs {
				for _, ch := range arc.Chapters {
					if ch.Chapter == chapter {
						data, _ := json.Marshal(struct {
							Volume    int    `json:"volume"`
							Arc       int    `json:"arc"`
							Title     string `json:"title"`
							CoreEvent string `json:"core_event"`
							Hook      string `json:"hook"`
						}{
							Volume:    vol.Index,
							Arc:       arc.Index,
							Title:     ch.Title,
							CoreEvent: ch.CoreEvent,
							Hook:      ch.Hook,
						})
						return "layered:" + string(data)
					}
				}
			}
		}
		return "layered:no-chapter-match"
	}

	// 回退到平面大纲
	outline, err := st.Outline.LoadOutline()
	if err == nil {
		for _, ch := range outline {
			if ch.Chapter == chapter {
				data, _ := json.Marshal(struct {
					Chapter   int    `json:"chapter"`
					Title     string `json:"title"`
					CoreEvent string `json:"core_event"`
					Hook      string `json:"hook"`
				}{
					Chapter:   ch.Chapter,
					Title:     ch.Title,
					CoreEvent: ch.CoreEvent,
					Hook:      ch.Hook,
				})
				return "outline:" + string(data)
			}
		}
	}
	return "no-outline"
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
