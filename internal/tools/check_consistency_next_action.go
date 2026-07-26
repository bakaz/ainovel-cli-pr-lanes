package tools

import (
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
)

// ── Action constants (normal, guard-allowed tool names only) ─────────

const (
	ActionDraftChapter   = "draft_chapter"
	ActionEditChapter    = "edit_chapter"
	ActionReviewStyle    = "review_style"
	ActionCommitChapter  = "commit_chapter"
)

// RequiredNextAction 是对当前章节状态的下一步正常建议，作为辅助提示而非指令。
// 仅在 guard 允许且可直接执行时存在；异常/error/读取失败时缺省（不输出）。
// guards（CheckStyleReviewMutationGuard / CheckCommitStyleGate）不依赖此字段。
type RequiredNextAction struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// hasErrorViolations 检查违规列表中是否存在 error 级的违规。
func hasErrorViolations(violations []rules.Violation) bool {
	for _, v := range violations {
		if v.Severity == rules.SeverityError {
			return true
		}
	}
	return false
}

// ComputeRequiredNextAction 根据当前章节状态计算下一步正常建议。
//
// 纯函数：不依赖 store，所有状态数据由调用方传入。guards 不依赖此函数。
// 只在正常、guard 允许且可直接执行时返回非 nil。
// 异常、机械 error、digest 不匹配、exhausted、未知状态均返回 nil（字段缺省）。
//
// finalDigest 仅在 inRewriteQueue 时有意义，为空表示终稿不存在。
func ComputeRequiredNextAction(
	styleReviewMode domain.StyleQualityMode,
	chapter int,
	hasErrors bool,
	currentDraftDigest string,
	ledger *domain.StyleReviewLedger,
	inRewriteQueue bool,
	finalDigest string,
) *RequiredNextAction {
	// 任何机械 error → 不输出建议
	if hasErrors {
		return nil
	}

	// ── Rewrite queue bypass ─────────────────────────────────────────
	if inRewriteQueue {
		return rewriteQueueAction(chapter, currentDraftDigest, finalDigest)
	}

	// ── Critic off ───────────────────────────────────────────────────
	if styleReviewMode != domain.StyleQualityCritic {
		return &RequiredNextAction{
			Action: ActionCommitChapter,
			Reason: fmt.Sprintf("第 %d 章检查通过，可以提交", chapter),
		}
	}

	// ── Critic on: 无账本或空账本 ───────────────────────────────────
	if ledger == nil || ledger.IsEmpty() {
		return &RequiredNextAction{
			Action: ActionReviewStyle,
			Reason: fmt.Sprintf("第 %d 章尚未进行风格评审，请调用 review_style 启动初评", chapter),
		}
	}

	currStatus := ledger.CurrentStatus()
	currCycle := ledger.CurrentCycle()

	// digestValid: 账本当前周期 DraftDigest 有效且匹配当前草稿
	digestValid := currCycle != nil && domain.IsValidDigest(currCycle.DraftDigest) &&
		currCycle.DraftDigest == currentDraftDigest

	switch currStatus {
	case domain.ReviewStatusInitialPending:
		if !digestValid {
			return nil
		}
		return &RequiredNextAction{
			Action: ActionReviewStyle,
			Reason: fmt.Sprintf("第 %d 章初评待处理，请调用 review_style 获取评审结果", chapter),
		}

	case domain.ReviewStatusRevisionOpen:
		draftUnchanged := currCycle != nil && currCycle.DraftDigest != "" &&
			currCycle.DraftDigest == currentDraftDigest
		if draftUnchanged {
			return &RequiredNextAction{
				Action: ActionEditChapter,
				Reason: fmt.Sprintf("第 %d 章草稿未变更，请根据 review findings 用 edit_chapter 修改", chapter),
			}
		}
		return &RequiredNextAction{
			Action: ActionReviewStyle,
			Reason: fmt.Sprintf("第 %d 章草稿已修改，请调用 review_style 进行最终评审", chapter),
		}

	case domain.ReviewStatusFinalPending:
		if !digestValid {
			return nil
		}
		return &RequiredNextAction{
			Action: ActionReviewStyle,
			Reason: fmt.Sprintf("第 %d 章终审待处理，请调用 review_style 获取结果", chapter),
		}

	case domain.ReviewStatusAcceptedInitial, domain.ReviewStatusAcceptedRev,
		domain.ReviewStatusDegraded, domain.ReviewStatusOverridden:
		if !digestValid {
			return nil
		}
		return &RequiredNextAction{
			Action: ActionCommitChapter,
			Reason: fmt.Sprintf("第 %d 章评审状态 %s，可以提交", chapter, currStatus),
		}

	default:
		// exhausted / unknown → 不输出建议
		return nil
	}
}

// rewriteQueueAction 处理重写/打磨队列中的章节。
// 已修改无 error→commit，未修改→edit，error→nil。
func rewriteQueueAction(chapter int, draftDigest, finalDigest string) *RequiredNextAction {
	if finalDigest == "" || draftDigest == finalDigest {
		return &RequiredNextAction{
			Action: ActionEditChapter,
			Reason: fmt.Sprintf("第 %d 章草稿与终稿相同，请用 edit_chapter 开始修改", chapter),
		}
	}
	return &RequiredNextAction{
		Action: ActionCommitChapter,
		Reason: fmt.Sprintf("第 %d 章检查通过，可以提交", chapter),
	}
}
