package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/schema"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── Rune-safe critic input bound ─────────────────────────────────────
//
// callCritic limits the draft sent to the critic to at most maxCriticRunes
// runes (not bytes). This avoids mid-rune truncation that could produce
// invalid UTF-8 or confuse the LLM. The bound is documented in the task
// text so the critic knows which portion it sees.
const maxCriticRunes = 12000

// maxFinalRevisionsPerEpoch 是同一评审 epoch 内 final revision 轮次上限
// （ora-1 P1-7，建议 2-3，取 3）：final 评审返回 revise（final_pending →
// revision_open）的次数达到该值后，critic 再返回 revise 即进入 exhausted。
// 与同签名停滞（DetectFinalReviewStagnation）叠加：同签名立即 exhausted；
// 不同 finding 的振荡（ch130：critic revise → edit → polish → check → review
// 循环）在 3 轮后 exhausted，防止无限消耗 writer 轮次。
const maxFinalRevisionsPerEpoch = 3

// ── Critic empty-output retry ────────────────────────────────────────
//
// Production observations: the critic model (deepseek-v4-flash via the
// go1 zen proxy) intermittently returns an empty output (63/65 chapters
// succeed, i.e. the failure is transient). An empty output is NOT a review
// verdict, so callCritic retries with exponential backoff before falling
// back to the degraded terminal state. Only empty/whitespace-only output
// triggers a retry — runner errors and non-empty (but malformed) outputs
// are returned immediately.
//
// Both knobs are package-level vars so tests can shrink the backoff.
var (
	// criticEmptyRetryMax 是 critic 空输出时的最大总尝试次数（1 次初始调用 + 3 次重试）。
	criticEmptyRetryMax = 4
	// criticEmptyRetryBase 是空输出重试的指数退避基数：2s → 4s → 8s。
	criticEmptyRetryBase = 2 * time.Second
)

// ReviewStyleTool 是状态化风格评审工具。
// 非 ReadOnly，非 ConcurrencySafe。Writer 可调用。
type ReviewStyleTool struct {
	store            *store.Store
	criticRunner     *subagent.Runner
	criticPromptHash string // sha256 前缀：实际批评者提示词内容的可溯源标识
	// pipelineEnabled 是精修流水线开关（BuildWorkers 注入）：开启时要求评审前存在
	// 与当前草稿 digest 匹配的 polish checkpoint（精修先于评审的时序保证）。
	// 与 fsmConfig.PipelineEnabled 同源（SetPipelineEnabled 同步两者）。
	pipelineEnabled bool
	// fsmConfig 是章节流水线强制状态机配置（BuildWorkers 注入）；Enabled 时
	// Execute 入口调用 RequireChapterAction 强制顺序（needs_review 才允许评审，
	// 保证非法评审不消耗模型调用）。
	fsmConfig ChapterFSMConfig
}

func NewReviewStyleTool(s *store.Store, criticRunner *subagent.Runner, criticPromptHash string) *ReviewStyleTool {
	return &ReviewStyleTool{store: s, criticRunner: criticRunner, criticPromptHash: criticPromptHash}
}

// SetPipelineEnabled 设置精修流水线开关（BuildWorkers 注入）。
func (t *ReviewStyleTool) SetPipelineEnabled(v bool) {
	t.pipelineEnabled = v
	t.fsmConfig.PipelineEnabled = v
}

// SetChapterFSMConfig 注入章节流水线强制状态机配置（BuildWorkers 调用）。
func (t *ReviewStyleTool) SetChapterFSMConfig(cfg ChapterFSMConfig) { t.fsmConfig = cfg }

// FSMConfig 返回注入的章节流水线配置（构建/测试诊断用）。
func (t *ReviewStyleTool) FSMConfig() ChapterFSMConfig { return t.fsmConfig }

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

	// ── 1.5 章节流水线强制状态机（Enabled 时）：needs_review 才允许评审；
	//    在加载草稿/启动 critic 之前拦截，保证非法评审不消耗模型调用。 ──
	if err := RequireChapterAction(t.store, a.Chapter, ChapterActionReview, t.fsmConfig); err != nil {
		return nil, fmt.Errorf("review_style: %w", err)
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

	// ── 3.5 校验精修流水线检查点（pipeline 启用时） ──
	// 精修必须先于评审：commit gate 要求 polish checkpoint 早于当前 critic pass，
	// 这里在评审发起侧提前拦截，避免"critic revise 后正文又被改 → 直接评审 →
	// 终态账本锁死"的流程死结（terminal 状态拒绝新评审）。
	if t.pipelineEnabled && !polishCheckpointMatches(t.store, a.Chapter, draftDigest) {
		return nil, fmt.Errorf("pipeline：章节 %d 缺少与当前草稿匹配的 polish 记录，请先调用 polish_draft 再评审: %w",
			a.Chapter, errs.ErrToolPrecondition)
	}

	// ── 3.6 顺序绑定（存在 polish checkpoint 时，无论 pipeline 开关） ──
	// 要求最新 consistency checkpoint 的 Seq > 最新 polish checkpoint 的 Seq，
	// 证明 polish → consistency → critic 的执行顺序。polish_draft 与 check_consistency
	// 每次执行都追加新 checkpoint（AppendAlways/AppendPolish 不做 digest 去重），
	// seq 单调递增，顺序绑定因此可机械判定。
	if polishCP := t.store.Checkpoints.LatestByStep(domain.ChapterScope(a.Chapter), "polish"); polishCP != nil {
		if consistencyCP == nil || consistencyCP.Seq <= polishCP.Seq {
			return nil, fmt.Errorf("章节 %d 的 consistency 检查点未晚于 polish 检查点（顺序必须是 polish_draft → check_consistency → review_style），请先调用 check_consistency: %w",
				a.Chapter, errs.ErrToolPrecondition)
		}
	}

	// ── 3.7 机械规则前置闸（C2 文学腔硬闸死锁防护） ──
	// 草稿存在 error 级文学腔违例时直接拒绝本次评审（不创建 pending、不调用
	// critic）：账本保持为空或 revision_open——pending 状态会被 mutation guard
	// 锁定（禁止修改草稿），若评审已发起才拦截，用户将无法修改带违例的草稿，
	// 造成新的死锁。写入 accepted 前另有 append 侧闸（checkMechanicalGate）
	// 纵深防御（草稿在 pending 期间被 guard 锁定，正常流程不会触发）。
	if err := t.checkMechanicalGate(a.Chapter); err != nil {
		return nil, err
	}

	// ── 4. 加载账本 ──
	ledger, err := t.store.StyleReview.Load(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load style review ledger: %w: %w", errs.ErrStoreRead, err)
	}

	// ── 5. 构建规范基础 payload（包含实际数据）并计算摘要 ──
	basis := t.buildCriticBasis(a.Chapter)
	basisDigest := t.computeBasisDigest(a.Chapter)

	// ── 6. 决定操作路径 ──
	var currentStatus domain.StyleReviewStatus
	if ledger == nil || ledger.IsEmpty() {
		currentStatus = ""
	} else {
		currentStatus = ledger.CurrentStatus()
	}
	// 当前评审周期代数：同一 epoch 内按 V2 状态机流转；返工队列章节可从旧
	// terminal/exhausted 开启新 epoch 重新评审。
	epoch := 1
	inRewriteQueue := isCompletedAndInRewriteQueue(t.store, a.Chapter)
	if ledger != nil && !ledger.IsEmpty() {
		epoch = ledger.MaxEpoch()
	}
	// 评审发起时绑定到的最新 polish checkpoint seq（0 = 未走精修流水线，legacy）。
	var polishSeq int64
	if cp := t.store.Checkpoints.LatestByStep(domain.ChapterScope(a.Chapter), "polish"); cp != nil {
		polishSeq = cp.Seq
	}

	// degraded 是"评审调用故障"（瞬态技术故障，非评审结论）——允许发起新 attempt
	// 重新评审（同 epoch 流转；候选更新后的分流见下方 degraded 分支）。其余
	// terminal 状态（accepted_initial/accepted_revised/overridden）是"当前 epoch 的
	// 评审权威"：返工队列章节可以从旧 terminal 开启新 epoch，进入 initial_pending
	// （D1 有意设计：返工走完整评审周期 initial → revise → final，而不是直接终审），
	// 非返工章节拒绝新评审。exhausted 必须先经 /style-override 覆盖（覆盖后账本变
	// 为 overridden terminal）才能开启新周期——返工章节同样不例外（与 engine 派发
	// 前拦截一致，C1-H3）。
	if currentStatus.IsTerminal() && currentStatus != domain.ReviewStatusDegraded {
		if inRewriteQueue {
			epoch = epoch + 1 // 新 epoch：旧 terminal 权威不跨代延续
			return t.executeInitialReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger, epoch, polishSeq)
		}
		return nil, fmt.Errorf("章节 %d 风格评审已终结（%s），不能再发起新的评审: %w",
			a.Chapter, currentStatus, errs.ErrToolPrecondition)
	}
	if currentStatus == domain.ReviewStatusExhausted {
		return nil, fmt.Errorf("章节 %d 风格评审已耗尽（exhausted），必须先通过 /style-override 覆盖后才能继续（返工章节同样不例外）: %w",
			a.Chapter, errs.ErrToolPrecondition)
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
		return t.executeInitialReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger, epoch, polishSeq)
	case domain.ReviewStatusRevisionOpen, domain.ReviewStatusFinalPending:
		return t.executeFinalReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger, epoch, polishSeq)
	case domain.ReviewStatusDegraded:
		// C2 degraded 恢复语义（oracle 设计）：候选身份判定与恢复策略解耦——
		// "候选是否更新"只决定返工章节是否开新 epoch，不再作为非返工章节能否
		// 恢复的拦截条件（旧实现会拒绝非返工章节的旧候选重评，导致
		// degraded → 重新 polish → 无法评审 → commit digest 不匹配 → 死锁）。
		//
		// sameCandidate：有 seq 绑定（R>0）时按 R == P 判定；legacy（R=0）且
		// 存在 polish 候选时按 degraded 绑定 digest 是否仍为当前草稿判定；
		// 无 polish 候选（R=0 且 P=0）时恒为同候选（纯评审调用故障重试）。
		degradedEntry := ledger.CurrentCycle()
		degradedSeq := int64(0)
		degradedDigest := ""
		if degradedEntry != nil {
			degradedDigest = degradedEntry.DraftDigest
			if degradedEntry.Request != nil {
				degradedSeq = degradedEntry.Request.PolishCheckpointSeq
			}
		}
		sameCandidate := degradedEntry != nil
		switch {
		case degradedSeq > 0:
			sameCandidate = degradedSeq == polishSeq
		case polishSeq > 0:
			sameCandidate = degradedDigest != "" && degradedDigest == draftDigest
		default:
			// 无 polish 候选：纯评审调用故障重试，视为同候选。
		}
		if inRewriteQueue && !sameCandidate {
			// 返工章节旧候选：开启新 epoch（MaxEpoch+1），按 initial review 完整
			// 重评（D1 设计：返工走 initial → revise → final 完整周期）。
			epoch = epoch + 1
			return t.executeInitialReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger, epoch, polishSeq)
		}
		// 同候选，或非返工章节（即使重新 polish、候选已变化）→ 在当前 epoch 恢复：
		// degraded 前的周期是 final_pending → 继续终审；否则 → 重新初评。
		// 新 attempt 的 Request 绑定当前最新 polish seq（P2），保证评审对象是
		// 当前精修版本，commit gate 的 digest/seq 绑定随后可通过。
		if degradedRecoversAsFinal(ledger) {
			return t.executeFinalReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger, epoch, polishSeq)
		}
		return t.executeInitialReview(ctx, a.Chapter, content, wordCount, draftDigest, basisDigest, basis, ledger, epoch, polishSeq)
	default:
		return nil, fmt.Errorf("章节 %d 帐本状态 %q 不支持 review_style: %w",
			a.Chapter, currentStatus, errs.ErrToolPrecondition)
	}
}

// degradedRecoversAsFinal 判断 degraded 状态恢复时新 attempt 的类型：
// 若账本最后一个周期（degraded）的前一个周期是 final_pending，说明是终审调用
// 失败，恢复时应继续终审；否则视为初评失败，恢复为初评。
func degradedRecoversAsFinal(ledger *domain.StyleReviewLedger) bool {
	if ledger == nil || ledger.IsEmpty() {
		return false
	}
	cycles := ledger.Cycles
	if len(cycles) < 2 {
		return false
	}
	return cycles[len(cycles)-2].Status == domain.ReviewStatusFinalPending
}

// ── Initial review ───────────────────────────────────────────────────

func (t *ReviewStyleTool) executeInitialReview(ctx context.Context, chapter int, content string, wordCount int, draftDigest, basisDigest string, basis domain.ReviewBasis, ledger *domain.StyleReviewLedger, epoch int, polishSeq int64) (json.RawMessage, error) {
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
			// 双摘要兼容：同时接受 canonical 与 legacy（旧字段排列）摘要，升级前
			// 落盘的 pending basis_digest 不会因字段重排被误判为漂移。
			if !domain.BasisDigestMatches(basis, cp.BasisDigest, basisDigest) {
				return t.appendDegraded(chapter, cp.AttemptID, cp.DraftDigest, cp.BasisDigest, request,
					fmt.Errorf("章节 %d 的评审基础已变更，初始评审待定（attempt %s）已失效，需要新的 check_consistency: %w",
						chapter, cp.AttemptID, errs.ErrToolPrecondition))
			}
			// 复用旧 attempt：后续写入（degraded/result）必须沿用账本中已落盘的
			// basis_digest——旧版本落盘的可能是 legacy（旧字段排列）摘要，若换绑
			// canonical 摘要，append-only 校验会拒绝结果落盘。内容一致性已由
			// BasisDigestMatches 保证；cp.BasisDigest 为空（未绑定）时保持新摘要。
			if cp.BasisDigest != "" {
				basisDigest = cp.BasisDigest
			}

			pendingEntry.DraftDigest = draftDigest
			pendingEntry.BasisDigest = basisDigest
		}
	}

	// 注：无需同步 pending 周期 digest——C2 机械闸在创建 pending 之前（Execute
	// 3.7）拦截，闸拒绝不会留下 pending；而 pending 存在期间 mutation guard
	// 锁定草稿（禁止修改），复用 attempt 时草稿 digest 必然与 pending 一致，
	// 结果落盘不会触发 draft_digest changed 校验拒绝。

	if attemptID == "" {
		attemptID = fmt.Sprintf("initial-%d-%d", chapter, time.Now().UnixNano())
		request = &domain.StyleReviewRequest{
			Prompt:              t.criticPromptHash,
			Model:               criticModel,
			IncludeBasis:        true,
			RequestedAt:         now,
			PolishCheckpointSeq: polishSeq,
		}
		pendingEntry = domain.StyleReviewEntry{
			Cycle:       1,
			Status:      domain.ReviewStatusInitialPending,
			CreatedAt:   now,
			AttemptID:   attemptID,
			Request:     request,
			DraftDigest: draftDigest,
			BasisDigest: basisDigest,
			Epoch:       epoch,
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
				if cur.CurrentStatus() == domain.ReviewStatusDegraded {
					// degraded（评审调用故障，非评审结论）后允许发起新的初评 attempt，
					// 追加一个新的 initial_pending 周期。Epoch 沿用调用方传入的
					// pendingEntry.Epoch：当前候选 retry 同 epoch；旧候选（C1-H3 分流）
					// 为 MaxEpoch()+1 开启新 epoch。
					pendingEntry.Cycle = len(cur.Cycles) + 1
					cur.Cycles = append(cur.Cycles, pendingEntry)
					return cur, nil
				}
				if (cur.CurrentStatus().IsTerminal() || cur.CurrentStatus() == domain.ReviewStatusExhausted) &&
					isCompletedAndInRewriteQueue(t.store, chapter) {
					// C1：返工队列章节从旧 terminal 开启新 epoch 重新评审
					// （D1：进入 initial_pending，走完整评审周期）。
					pendingEntry.Cycle = len(cur.Cycles) + 1
					pendingEntry.Epoch = cur.MaxEpoch() + 1
					cur.Cycles = append(cur.Cycles, pendingEntry)
					return cur, nil
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
			finalReq = &domain.StyleReviewRequest{Prompt: t.criticPromptHash, Model: criticModel, PolishCheckpointSeq: polishSeq}
		}
		return t.appendDegraded(chapter, attemptID, pendingEntry.DraftDigest, pendingEntry.BasisDigest, finalReq, degradedErr)
	}

	return t.appendInitialResult(chapter, attemptID, request, result, draftDigest, basisDigest)
}

// ── Final review ─────────────────────────────────────────────────────

func (t *ReviewStyleTool) executeFinalReview(ctx context.Context, chapter int, content string, wordCount int, draftDigest, basisDigest string, basis domain.ReviewBasis, ledger *domain.StyleReviewLedger, epoch int, polishSeq int64) (json.RawMessage, error) {
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
			// 双摘要兼容：同时接受 canonical 与 legacy（旧字段排列）摘要，升级前
			// 落盘的 pending basis_digest 不会因字段重排被误判为漂移。
			if !domain.BasisDigestMatches(basis, cp.BasisDigest, basisDigest) {
				return t.appendDegraded(chapter, cp.AttemptID, cp.DraftDigest, cp.BasisDigest, request,
					fmt.Errorf("章节 %d 的评审基础已变更，最终评审待定（attempt %s）已失效，需要新的 check_consistency: %w",
						chapter, cp.AttemptID, errs.ErrToolPrecondition))
			}
			// 复用旧 attempt：后续写入（degraded/result）沿用账本中已落盘的
			// basis_digest（可能是 legacy 旧字段排列摘要），否则 append-only
			// 校验会拒绝落盘；内容一致性已由 BasisDigestMatches 保证。
			if cp.BasisDigest != "" {
				basisDigest = cp.BasisDigest
			}
		}
	}

	// 注：无需同步 pending 周期 digest——与 executeInitialReview 同因（C2 机械
	// 闸在 pending 创建前拦截；pending 期间 mutation guard 锁定草稿）。

	if attemptID == "" {
		attemptID = fmt.Sprintf("final-%d-%d", chapter, time.Now().UnixNano())
		request = &domain.StyleReviewRequest{
			Prompt:              t.criticPromptHash,
			Model:               criticModel,
			IncludeBasis:        true,
			RequestedAt:         now,
			PolishCheckpointSeq: polishSeq,
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
				Epoch:       epoch,
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

// runCriticWithEmptyRetry 调用 critic runner，仅在输出为空（空串/仅空白）时
// 自动重试，指数退避（2s/4s/8s），最多 criticEmptyRetryMax 次。
//
// 只对"空输出"这种瞬态故障重试：runner 错误与非空输出（即使 JSON 解析失败或
// 校验失败）立即返回，绝不为 critic 合法返回的 revise/pass 结论增加延迟。
// 返回合并后的输出文本（Output 优先，空时回退 TerminalResult）。
func (t *ReviewStyleTool) runCriticWithEmptyRetry(ctx context.Context, chapter int, taskText string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= criticEmptyRetryMax; attempt++ {
		if attempt > 1 {
			// 指数退避：2s → 4s → 8s
			delay := criticEmptyRetryBase * time.Duration(1<<(attempt-2))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", fmt.Errorf("critic retry aborted: %w: %w", ctx.Err(), lastErr)
			}
		}

		runResult, err := t.criticRunner.Run(ctx, "style_critic", taskText)
		if err != nil {
			return "", fmt.Errorf("critic call failed: %w", err)
		}

		outputText := runResult.Output
		if outputText == "" && runResult.TerminalResult != nil {
			outputText = string(runResult.TerminalResult)
		}
		if strings.TrimSpace(outputText) != "" {
			return outputText, nil
		}

		lastErr = fmt.Errorf("critic returned empty output (attempt %d/%d)", attempt, criticEmptyRetryMax)
		slog.Warn("critic 返回空输出，准备重试", "module", "tools", "chapter", chapter,
			"attempt", attempt, "max", criticEmptyRetryMax)
	}
	// 重试耗尽：空输出是瞬态故障（非评审结论），提示可重试或走重写/打磨队列/人工干预。
	return "", fmt.Errorf("%w；连续 %d 次空输出（瞬态故障），可稍后重新 check_consistency + review_style 重试，或走重写/打磨队列、人工干预",
		lastErr, criticEmptyRetryMax)
}

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

	// ── 任务文本（ora-1 缓存优化阶段 2：Prompt Capsule 重排）──
	// 稳定书级内容（评审依据 basis）在前，章节动态内容（章节号/字数/草稿全文）
	// 最后、短 footer 收尾——跨 spawn 的内容前缀缓存（DeepSeek 磁盘缓存按内容
	// 前缀匹配）命中稳定前缀，只有尾部的草稿段需要重新计算。
	taskText := fmt.Sprintf(`## 评审任务

### 评审依据
%s

### 章节与草稿（字数：%d）%s
第 %d 章

%s

请严格按样式批评者提示词中定义的 JSON 格式输出（含 mandatory strength.evidence）。`,
		basisPayload, wordCount, truncNote, chapter, draftForCritic)

	// ── Invoke the critic with empty-output retry ──
	// 空输出（空串/仅空白）是瞬态故障，自动重试（2s/4s/8s 指数退避）；
	// 重试耗尽仍为空才返回错误 → 由调用方映射为 degraded。runner 错误与
	// 非空输出（即使解析失败）不做重试。
	outputText, err := t.runCriticWithEmptyRetry(ctx, chapter, taskText)
	if err != nil {
		return nil, err
	}

	// ── Decode production shape ──
	// runCriticWithEmptyRetry 已统一 Output / TerminalResult 取文本，
	// 此处只需解析合并后的 outputText。
	var co criticOutput
	parseErr := json.Unmarshal([]byte(outputText), &co)
	if parseErr != nil {
		return nil, fmt.Errorf("critic output decode failed: output=%q err=%w", truncateForLog(outputText, 200), parseErr)
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

// checkMechanicalGate 重算 12 类文学腔硬闸（rules.CheckLiteraryProse，与
// commit_chapter 的 CheckLiteraryProseGate 完全同一规则集、同一严重度判定——
// 被接受的草稿必然能过 commit 硬闸，死锁从根上消除）。存在 error 级违例 →
// ErrToolPrecondition，引导先修改草稿并重新 check_consistency。
//
// 双重调用点：
//  1. Execute 前置闸（3.7）：创建 pending / 调用 critic 之前拦截——账本保持
//     为空或 revision_open。pending 状态会被 mutation guard 锁定（禁止修改
//     草稿），若评审已发起才拦截，用户将无法修改带违例的草稿，制造新死锁。
//  2. append 侧纵深防御（accepted 落盘前）：草稿在 pending 期间被 guard 锁定，
//     正常流程不会触发；兜底任何绕过前置闸的路径。
//
// 为什么只闸 12 类硬闸而非 check_consistency 的全部 error 违规：
//   - 死锁链的阻断点是 commit 的文学腔硬闸（CheckLiteraryProseGate）；
//     review 闸门只需保证"accepted ⇒ 可提交"即可闭环。
//   - check_consistency 其余 error（如 chapter_words 字数 3000-6000 上下界、
//     用户规则违例）不是 commit 阻断项——review 若拦截它们，critic 模式下
//     短章/长章将永远无法 accepted（commit 需 terminal 评审），制造新的死锁。
//
// 死锁防护：Critic 一旦对仍带文学腔 error 的草稿给出 pass（accepted_*），
// terminal 快照权威会禁止一切修改（mutation guard），commit 又被文学腔硬闸
// 拒绝，/style-override 只接受 exhausted——章节即被永久锁死。此闸保证
// "被接受的草稿"文学腔硬闸必然干净。
func (t *ReviewStyleTool) checkMechanicalGate(chapter int) error {
	content, _, err := t.store.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return fmt.Errorf("load chapter content for mechanical gate: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return fmt.Errorf("章节 %d 无草稿: %w", chapter, errs.ErrToolPrecondition)
	}
	if !hasErrorViolations(rules.CheckLiteraryProse(content)) {
		return nil
	}
	return fmt.Errorf("章节 %d 存在 error 级文学腔硬闸违例，不能接受评审结果：请先修改草稿并重新 check_consistency，再 review_style: %w",
		chapter, errs.ErrToolPrecondition)
}

// mechanicalGateFor 构造 accepted 结果落盘前的机械门禁回调（复核阻塞项 7）：
// 门禁作为回调移入 CommitReviewResult 临界区内、在 CAS 身份校验（stale 检测）
// 之后以同一草稿快照执行——critic 调用期间草稿被并发修改时先走 stale 检测标记
// degraded，不会被门禁提前返回错误而遗留 stranded pending。draft 参数由
// CommitReviewResult 传入（与 digest 校验同一快照，非空）。
func (t *ReviewStyleTool) mechanicalGateFor(chapter int) func(draft string) error {
	return func(draft string) error {
		if !hasErrorViolations(rules.CheckLiteraryProse(draft)) {
			return nil
		}
		return fmt.Errorf("章节 %d 存在 error 级文学腔硬闸违例，不能接受评审结果：请先修改草稿并重新 check_consistency，再 review_style: %w",
			chapter, errs.ErrToolPrecondition)
	}
}

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

	// C2 死锁防护：accepted 落盘前重算机械规则，error 级违例 → 拒绝。复核阻塞项 7：
	// 门禁作为回调移入 CommitReviewResult 临界区内、在 CAS 身份校验（stale 检测）
	// 之后执行——critic 调用期间草稿被并发修改时先标记 stale，不会被门禁提前返回
	// 错误而遗留 stranded pending。
	var gate func(draft string) error
	if nextStatus == domain.ReviewStatusAcceptedInitial {
		gate = t.mechanicalGateFor(chapter)
	}

	// P0-4：CAS 校验 + 落盘原子化。critic 调用期间草稿/账本/polish checkpoint 被
	// 并发修改（ora-1 死锁根因：critic 接受旧候选时另一在途 polish 覆盖草稿）→
	// accepted 结果不落盘，attempt 在账本中标记 degraded(stale)，返回明确警告让
	// writer 重新 review。
	now := time.Now().Format(time.RFC3339)
	err := t.store.CommitReviewResult(chapter, attemptID, draftDigest, reviewBoundPolishSeq(request), gate, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
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
			Epoch:       cur.MaxEpoch(),
		})
		return cur, nil
	})
	if errors.Is(err, store.ErrReviewStale) {
		return t.staleReviewOutput(chapter, err)
	}
	if err != nil {
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

	// C2 死锁防护：accepted 落盘前重算机械规则，error 级违例 → 拒绝。复核阻塞项 7：
	// 门禁作为回调移入 CommitReviewResult 临界区内、在 CAS 身份校验（stale 检测）
	// 之后执行——critic 调用期间草稿被并发修改时先标记 stale，不会被门禁提前返回
	// 错误而遗留 stranded pending。
	var gate func(draft string) error
	if nextStatus == domain.ReviewStatusAcceptedRev {
		gate = t.mechanicalGateFor(chapter)
	}

	// P0-4：CAS 校验 + 落盘原子化（同 appendInitialResult）。
	now := time.Now().Format(time.RFC3339)
	err := t.store.CommitReviewResult(chapter, attemptID, draftDigest, reviewBoundPolishSeq(request), gate, func(cur *domain.StyleReviewLedger) (*domain.StyleReviewLedger, error) {
		if cur == nil {
			return nil, fmt.Errorf("ledger disappeared during update")
		}

		// V2: detect stagnation — same finding signature as previous
		// final_revise → revision_open → exhausted to prevent infinite loops.
		// P1-7: 叠加 final revision 总数上限——同一 epoch 内 final revise 轮次
		// 达到 maxFinalRevisionsPerEpoch 后，即使 findings 各不相同（oscillation）
		// 也进入 exhausted，与同签名停滞共用同一收敛路径（/style-override 或
		// 接受当前候选）。
		if nextStatus == domain.ReviewStatusRevisionOpen {
			if domain.DetectFinalReviewStagnation(cur, result) ||
				domain.FinalRevisionCount(cur) >= maxFinalRevisionsPerEpoch {
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
			Epoch:       cur.MaxEpoch(),
		})
		return cur, nil
	})
	if errors.Is(err, store.ErrReviewStale) {
		return t.staleReviewOutput(chapter, err)
	}
	if err != nil {
		return nil, fmt.Errorf("append final result: %w", err)
	}

	return t.buildSuccessOutput(chapter, result, nextStatus)
}

// reviewBoundPolishSeq 返回评审 request 绑定的 polish checkpoint seq（无绑定 = 0）。
// P0-4 CAS 用它校验"绑定的 polish checkpoint 仍是当前 polish"。
func reviewBoundPolishSeq(request *domain.StyleReviewRequest) int64 {
	if request == nil {
		return 0
	}
	return request.PolishCheckpointSeq
}

// staleReviewOutput 返回评审候选过期（P0-4）的 degraded 摘要：accepted 结果未
// 落盘，attempt 已在账本中标记 stale（degraded 周期），writer 应重新 review。
func (t *ReviewStyleTool) staleReviewOutput(chapter int, err error) (json.RawMessage, error) {
	return json.Marshal(StyleReviewOutput{
		Chapter:  chapter,
		Verdict:  "degraded",
		Status:   string(domain.ReviewStatusDegraded),
		Degraded: true,
		Error:    err.Error(),
	})
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
		entry.Epoch = cur.MaxEpoch()
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

// buildStyleBasis 按职责角色投影加载全部实际章节数据构建规范基础 payload。
// 各字段包含实际内容而非标识符——序列化后既发送给对应角色（critic/editor 或
// polisher/writer）作为依据，也作为摘要输入——commit gate 通过重新计算相同摘要
// 检测任意输入变更。使用的 compass 作用域逻辑与 Writer context 一致
// （scopedCompassForChapter）。
// 用户规则按角色投影（loadUserRulesJSON）：polisher → writer（default+writer），
// critic → editor（default+writer+editor），与 novel_context 的 PayloadForRole 口径一致。
func buildStyleBasis(st *store.Store, chapter int, promptHash, role string) domain.ReviewBasis {
	prose, dialogue, taboos := loadScopedCompassProseDialogueTaboos(st, chapter)
	return domain.ReviewBasis{
		StyleGoal:       loadChapterStyleGoal(st, chapter),
		ChapterContract: loadChapterContract(st, chapter),
		CompassProse:    prose,
		CompassDialogue: dialogue,
		CompassTaboos:   taboos,
		AnchorExcerpts:  loadAnchorExcerpts(st, chapter),
		UserRules:       loadUserRulesJSON(st, role),
		FactualOutline:  loadFactualOutline(st, chapter),
		CriticVersion:   promptHash,
	}
}

// buildCriticBasis 是 review_style（critic）实际发送给批评者的 basis：
// 用户规则使用 editor 视图（default+writer+editor）。ComputeBasisDigest
// 固定复用本函数，保证 commit gate 与评审实际发送口径一致（防漂移）。
func buildCriticBasis(st *store.Store, chapter int, criticPromptHash string) domain.ReviewBasis {
	return buildStyleBasis(st, chapter, criticPromptHash, "editor")
}

// buildPolishBasis 是 polisher 使用的 basis：用户规则使用 writer 视图
// （default+writer），与 Writer 角色看到的分区一致；不注入 editor/architect 专属规则。
func buildPolishBasis(st *store.Store, chapter int, polisherPromptHash string) domain.ReviewBasis {
	return buildStyleBasis(st, chapter, polisherPromptHash, "writer")
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

func (t *ReviewStyleTool) buildCriticBasis(chapter int) domain.ReviewBasis {
	return buildCriticBasis(t.store, chapter, t.criticPromptHash)
}

func (t *ReviewStyleTool) computeBasisDigest(chapter int) string {
	return ComputeBasisDigest(t.store, chapter, t.criticPromptHash)
}

// ComputeBasisDigest 是 review_style 与 CheckCommitStyleGate 共享的
// 基础摘要计算函数。口径固定为 critic/editor 视图（buildCriticBasis），
// 与 review_style 实际发送给 critic 的 basis 完全一致——若未来 critic 视图
// 调整，commit gate 自动跟随，杜绝口径漂移。
func ComputeBasisDigest(st *store.Store, chapter int, criticPromptHash string) string {
	basis := buildCriticBasis(st, chapter, criticPromptHash)
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
// 单条投影上限 200 runes；总预算 3000 runes（= 15 条 × 200），
// 与 schema 的 15 条/15000 字符上限保持同步，超出后追加省略标记。
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
		if totalRunes > 3000 {
			excerpts = append(excerpts, "...(more)")
			break
		}
	}
	return excerpts
}

// loadUserRulesJSON 按职责角色投影加载用户规则（structured + 该角色分区偏好）。
// 只暴露创作相关偏好，不注入 version/status/sources/uncertain 诊断元数据
// （与 novel_context 的 PayloadForRole 口径一致）。
// 缺失快照时回退 rules.SystemDefaults()（与 writer 侧一致），保证机械底线
// （字数/禁语/疲劳词）始终存在，且返回稳定结构而非 null。
func loadUserRulesJSON(st *store.Store, role string) json.RawMessage {
	snap, err := st.UserRules.Load()
	if err != nil || snap == nil {
		def := rules.BuildSnapshot([]rules.Candidate{rules.SystemDefaults()})
		snap = &def
	}
	data, _ := json.Marshal(snap.PayloadForRole(role))
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
