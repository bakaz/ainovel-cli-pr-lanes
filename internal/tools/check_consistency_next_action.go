package tools

import (
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
)

// ── Action constants (normal, guard-allowed tool names only) ─────────

const (
	ActionDraftChapter  = "draft_chapter"
	ActionEditChapter   = "edit_chapter"
	ActionReviewStyle   = "review_style"
	ActionCommitChapter = "commit_chapter"
	ActionPolishDraft   = "polish_draft"
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

// PolishPipelineBinding 是精修流水线对下一步建议的绑定约束（nil = 流水线关闭）。
// pipeline 启用时，重写队列章节的 terminal 评审必须绑定当前最新 polish checkpoint
// seq（R == latest P）才建议 commit——与 CheckPolishPipelineGate 的严格绑定一致，
// 避免"gate 拒绝但 check 建议 commit"的控制面错误建议（如 Critic 已评审 P1 后
// 又生成同 digest 的 no-op polish P2：P2 != R1 时必须建议 review_style 重新终验）。
type PolishPipelineBinding struct {
	LatestPolishSeq int64 // 当前最新 polish checkpoint seq（0 = 无 polish 记录）
}

// ComputeRequiredNextAction 根据当前章节状态计算下一步正常建议。
//
// 纯函数：不依赖 store，所有状态数据由调用方传入。guards 不依赖此函数。
// 只在正常、guard 允许且可直接执行时返回非 nil。
// 异常、机械 error、digest 不匹配、exhausted、未知状态均返回 nil（字段缺省）。
//
// finalDigest 仅在 inRewriteQueue 时有意义，为空表示终稿不存在。
// binding（可选）为精修流水线的 seq 绑定约束：nil = 流水线关闭（不做绑定）。
func ComputeRequiredNextAction(
	styleReviewMode domain.StyleQualityMode,
	chapter int,
	hasErrors bool,
	currentDraftDigest string,
	ledger *domain.StyleReviewLedger,
	inRewriteQueue bool,
	finalDigest string,
	binding ...*PolishPipelineBinding,
) *RequiredNextAction {
	// 任何机械 error → 不输出建议
	if hasErrors {
		return nil
	}

	// ── Critic off（D2：mode=off 时 rewrite 走旧规则，不进入 review_style 分支） ──
	//    off 模式重写队列：draft != final → 直接 commit；draft == final → 提示需修改。
	if styleReviewMode != domain.StyleQualityCritic {
		if inRewriteQueue && (finalDigest == "" || currentDraftDigest == finalDigest) {
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

	// ── Critic on + rewrite queue（C1：返工路径必须经 critic 终验才能 commit） ──
	var b *PolishPipelineBinding
	if len(binding) > 0 {
		b = binding[0]
	}
	if inRewriteQueue {
		return rewriteQueueAction(chapter, currentDraftDigest, finalDigest, ledger, b)
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
		domain.ReviewStatusOverridden:
		if !digestValid {
			return nil
		}
		return &RequiredNextAction{
			Action: ActionCommitChapter,
			Reason: fmt.Sprintf("第 %d 章评审状态 %s，可以提交", chapter, currStatus),
		}

	case domain.ReviewStatusDegraded:
		// C2：degraded 是"评审调用故障"（非评审结论），可恢复——候选未变
		// （digest 匹配当前草稿，且绑定 seq == 最新 polish seq，或无 pipeline
		// 绑定约束）→ 建议 commit（与既有可提交语义一致）；候选已变化
		// （digest 或 polish seq 与新草稿/最新 polish 不匹配）→ 建议
		// review_style 重新评审（非返工章节同 epoch 恢复，返工章节开新 epoch），
		// 而不是返回 nil——否则 writer 无指令可循，只能空转到超时。
		if digestValid && (b == nil || polishSeqBoundToLatest(currCycle, b.LatestPolishSeq)) {
			return &RequiredNextAction{
				Action: ActionCommitChapter,
				Reason: fmt.Sprintf("第 %d 章评审状态 %s（候选未变化），可以提交", chapter, currStatus),
			}
		}
		return &RequiredNextAction{
			Action: ActionReviewStyle,
			Reason: fmt.Sprintf("第 %d 章评审候选已更新（degraded 绑定与新草稿/最新 polish 不匹配），请调用 review_style 重新评审", chapter),
		}

	default:
		// exhausted / unknown → 不输出建议
		return nil
	}
}

// rewriteQueueAction 处理重写/打磨队列中的章节（C1：必须走 Critic 终验才能 commit）。
// 已修改但缺少绑定当前草稿的 terminal 评审结果 → 建议 review_style（将开启新评审
// 周期）；账本存在绑定当前草稿的 terminal 结果 → 建议 commit；未修改 → edit。
// 精修流水线（binding != nil）启用时，terminal 评审还必须绑定当前最新 polish
// checkpoint seq（R == latest P）才建议 commit——no-op polish 后未重新终验
// （P2 > R1）→ 建议 review_style，与 CheckPolishPipelineGate 的严格绑定一致。
func rewriteQueueAction(chapter int, draftDigest, finalDigest string, ledger *domain.StyleReviewLedger, binding *PolishPipelineBinding) *RequiredNextAction {
	if finalDigest == "" || draftDigest == finalDigest {
		return &RequiredNextAction{
			Action: ActionEditChapter,
			Reason: fmt.Sprintf("第 %d 章草稿与终稿相同，请用 edit_chapter 开始修改", chapter),
		}
	}
	// 账本存在绑定当前草稿的 terminal 结果（新 epoch 终验已通过）才可 commit；
	// degraded 是评审调用故障的终态，沿用既有可提交语义。
	if ledger != nil && !ledger.IsEmpty() {
		curr := ledger.CurrentCycle()
		if curr != nil && curr.DraftDigest == draftDigest {
			switch ledger.CurrentStatus() {
			case domain.ReviewStatusAcceptedInitial, domain.ReviewStatusAcceptedRev,
				domain.ReviewStatusDegraded, domain.ReviewStatusOverridden:
				// C1-H1：pipeline 启用时要求 terminal 绑定 seq == 最新 polish seq。
				if binding == nil || polishSeqBoundToLatest(curr, binding.LatestPolishSeq) {
					return &RequiredNextAction{
						Action: ActionCommitChapter,
						Reason: fmt.Sprintf("第 %d 章返工草稿已通过评审终验（%s），可以提交", chapter, ledger.CurrentStatus()),
					}
				}
			}
		}
	}
	return &RequiredNextAction{
		Action: ActionReviewStyle,
		Reason: fmt.Sprintf("第 %d 章返工草稿尚未通过评审终验，请调用 review_style（将开启新评审周期）", chapter),
	}
}

// polishSeqBoundToLatest 判断 terminal 评审条目是否绑定当前最新 polish checkpoint：
// 请求记录存在且 PolishCheckpointSeq（R）> 0 且等于最新 polish seq（P）。
// legacy（R=0，无 seq 绑定）视为未绑定 → 建议重新评审。
func polishSeqBoundToLatest(curr *domain.StyleReviewEntry, latestPolishSeq int64) bool {
	return curr != nil && curr.Request != nil &&
		curr.Request.PolishCheckpointSeq > 0 &&
		curr.Request.PolishCheckpointSeq == latestPolishSeq
}
