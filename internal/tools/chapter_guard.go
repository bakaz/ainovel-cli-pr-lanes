package tools

import (
	"fmt"
	"slices"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// EnsureChapterExpanded verifies that a chapter is inside the currently
// expanded layered outline. Non-layered books and non-writing phases have no
// layered range constraint.
func EnsureChapterExpanded(st *store.Store, chapter int) error {
	if st == nil || chapter <= 0 {
		return nil
	}
	progress, err := st.Progress.Load()
	if err != nil {
		return fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if progress == nil || !progress.Layered || progress.Phase != domain.PhaseWriting {
		return nil
	}
	boundary, err := st.Outline.CheckArcBoundary(chapter)
	if err != nil {
		return fmt.Errorf("check layered outline: %w: %w", errs.ErrStoreRead, err)
	}
	if boundary != nil {
		return nil
	}
	return fmt.Errorf(
		"第 %d 章不在分层大纲范围内：写作必须先 expand_arc 扩展弧或 append_volume 追加卷；若全书已完结请调 save_foundation type=complete_book: %w",
		chapter, errs.ErrToolPrecondition)
}

// isCompletedAndInRewriteQueue 检查章节是否已完成且在重写/打磨队列中。
// 此类章节跳过 critic 模式的门控。
func isCompletedAndInRewriteQueue(st *store.Store, chapter int) bool {
	if !st.Progress.IsChapterCompleted(chapter) {
		return false
	}
	progress, err := st.Progress.Load()
	if err != nil || progress == nil {
		return false
	}
	return slices.Contains(progress.PendingRewrites, chapter)
}

// CheckStyleReviewMutationGuard 检查 critic 模式下是否允许修改草稿。
// 在 critic 模式下，仅当没有账本或当前状态为 revision_open 时才允许 draft/edit。
// pending（initial_pending/final_pending）以及所有 terminal 状态
// （accepted_initial、accepted_revised、degraded、overridden）拒绝修改。
// exhausted 也拒绝修改。
// 已完成且在 PendingRewrites 队列中的章节不受批评者门控（打磨/重写路径）。
//
// Terminal 状态（accepted_initial、accepted_revised、degraded、overridden）
// 代表"快照权威"——评审时草稿的快照已被接受/降级/覆盖，即使后续基础配置
// （风格目标、规则、锚点、批评者提示）发生变更，也不应再修改草稿。
// 基础变更后若要更新草稿，须通过重写/打磨路径（PendingRewrites）。
func CheckStyleReviewMutationGuard(st *store.Store, chapter int) error {
	meta, err := st.RunMeta.Load()
	if err != nil {
		return fmt.Errorf("load run meta: %w: %w", errs.ErrStoreRead, err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return nil // off 模式不拦截
	}

	// 已完成 + 重写队列中 → 跳过批评者门控
	if isCompletedAndInRewriteQueue(st, chapter) {
		return nil
	}

	ledger, err := st.StyleReview.Load(chapter)
	if err != nil {
		return fmt.Errorf("load style review ledger: %w: %w", errs.ErrStoreRead, err)
	}
	if ledger == nil || ledger.IsEmpty() {
		return nil // 尚无风格评审，允许首次起草
	}
	curr := ledger.CurrentStatus()
	switch curr {
	case domain.ReviewStatusRevisionOpen:
		return nil // 修订打开的章可以改
	case domain.ReviewStatusInitialPending, domain.ReviewStatusFinalPending:
		return fmt.Errorf("critic 模式：章节 %d 有未完成的评审（%s），不能修改: %w",
			chapter, curr, errs.ErrToolPrecondition)
	default:
		// terminal / exhausted / degraded / overridden
		return fmt.Errorf("critic 模式：章节 %d 评审状态 %s 不允许修改: %w",
			chapter, curr, errs.ErrToolPrecondition)
	}
}

// CheckCommitStyleGate 在 critic 模式下校验 commit 的前置条件。
// 要求：
//   - 最新的 consistency checkpoint 摘要精确匹配当前草稿摘要
//   - 账本状态为 eligible terminal（accepted_initial、accepted_revised、degraded、overridden）
//   - 当前草稿摘要匹配账本中最新条目的摘要
//   - 当前基础摘要匹配账本中最新条目的基础摘要（检测风格目标/规则/锚点/批评者提示变更）
//
// Terminal 状态（accepted_initial、accepted_revised、degraded、overridden）
// 是"快照权威"：评审锁定的是草稿快照，而非当时的基础配置。因此 terminal 状态
// 下即使基础配置（风格目标、规则、锚点、批评者提示、大纲）已变更，commit 仍被
// 允许——无需、也不可能重新评审已终结的审查。
//
// 已完成且在 PendingRewrites 队列中的章节不受批评者门控（重写/打磨路径）。
func CheckCommitStyleGate(st *store.Store, chapter int) error {
	meta, err := st.RunMeta.Load()
	if err != nil {
		return fmt.Errorf("load run meta: %w: %w", errs.ErrStoreRead, err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return nil // off 模式不拦截
	}

	// 已完成 + 重写队列中 → 跳过批评者门控
	if isCompletedAndInRewriteQueue(st, chapter) {
		return nil
	}

	// 加载草稿并计算摘要
	content, _, err := st.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return fmt.Errorf("load content for commit gate: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return fmt.Errorf("章节 %d 无草稿: %w", chapter, errs.ErrToolPrecondition)
	}
	currentDraftDigest := domain.DigestDraft(content)

	// 校验一致性检查点存在且摘要匹配当前草稿
	consistencyCP := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "consistency_check")
	if consistencyCP == nil {
		return fmt.Errorf("critic 模式：commit 前必须在章节 %d 上调用 check_consistency: %w",
			chapter, errs.ErrToolPrecondition)
	}
	if !domain.IsValidDigest(consistencyCP.Digest) || consistencyCP.Digest != currentDraftDigest {
		return fmt.Errorf("critic 模式：章节 %d 的草稿已变更或一致性检查点摘要无效，请重新运行 check_consistency: %w",
			chapter, errs.ErrToolPrecondition)
	}

	// 加载账本
	ledger, err := st.StyleReview.Load(chapter)
	if err != nil {
		return fmt.Errorf("load style review ledger: %w: %w", errs.ErrStoreRead, err)
	}
	if ledger == nil || ledger.IsEmpty() {
		return fmt.Errorf("critic 模式：章节 %d 尚未进行风格评审，不能 commit: %w",
			chapter, errs.ErrToolPrecondition)
	}

	currStatus := ledger.CurrentStatus()
	// 只允许 terminal 状态提交
	if currStatus.IsActive() {
		return fmt.Errorf("critic 模式：章节 %d 评审状态 %s 为活跃中，不能 commit: %w",
			chapter, currStatus, errs.ErrToolPrecondition)
	}
	if currStatus == domain.ReviewStatusExhausted {
		return fmt.Errorf("critic 模式：章节 %d 评审已耗尽（exhausted），不能 commit: %w",
			chapter, errs.ErrToolPrecondition)
	}

	// 校验摘要匹配
	currCycle := ledger.CurrentCycle()
	if currCycle == nil {
		return fmt.Errorf("critic 模式：章节 %d 账本无有效条目: %w", chapter, errs.ErrToolPrecondition)
	}
	if currCycle.DraftDigest != "" && currCycle.DraftDigest != currentDraftDigest {
		return fmt.Errorf("critic 模式：章节 %d 当前草稿摘要与评审账本不匹配（草稿已被修改但未重新评审）: %w",
			chapter, errs.ErrToolPrecondition)
	}

	// 校验基础摘要是否仍然最新（检测风格目标/规则/锚点/批评者提示/大纲变更）。
	// Terminal 状态（accepted_initial/accepted_revised/degraded/overridden）是
	// "快照权威"：评审锁定的是草稿快照而非基础配置，因此跳过此检查——基础变更不
	// 阻挡 commit。非 terminal 状态（仅 exhausted，已在上方拦截）本不应到达此处。
	if !currStatus.IsTerminal() && currCycle.BasisDigest != "" {
		criticVersion := ""
		if currCycle.Request != nil {
			criticVersion = currCycle.Request.Prompt
		}
		currentBasisDigest := ComputeBasisDigest(st, chapter, criticVersion)
		if currentBasisDigest != currCycle.BasisDigest {
			return fmt.Errorf("critic 模式：章节 %d 的评审基础已变更（风格目标/规则/锚点/大纲），需要重新评审: %w",
				chapter, errs.ErrToolPrecondition)
		}
	}

	return nil
}
