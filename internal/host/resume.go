package host

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// resumeLabel 基于事实生成 Resume 的 UI 标签。
// label 为空表示无可恢复状态（应走新建）。恢复本身不需要任何 prompt——
// Engine 只恢复事实：从 store 重算路由续跑（docs/engine-rfc.md §6）。
func resumeLabel(store *storepkg.Store) (string, error) {
	progress, err := store.Progress.Load()
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if progress == nil || progress.Phase == domain.PhaseComplete {
		return "", nil
	}
	return describeResume(store, progress), nil
}

// describeResume 生成人类可读的恢复标签；不影响 Engine 路由。
// 所有执行路由由 Flow Router 按事实推导；这里仅面向 UI 的 "恢复：xxx"。
func describeResume(store *storepkg.Store, progress *domain.Progress) string {
	switch progress.Phase {
	case domain.PhasePremise, domain.PhaseOutline:
		return fmt.Sprintf("恢复：规划阶段（%s）", progress.Phase)
	case domain.PhaseWriting:
		// 优先级与 Router 的决策优先级对齐，让 label 与即将派发的指令一致。
		if pending, _ := store.Signals.LoadPendingCommit(); pending != nil {
			return fmt.Sprintf("恢复：第 %d 章提交中断", pending.Chapter)
		}
		if len(progress.PendingRewrites) > 0 {
			verb := "重写"
			if progress.Flow == domain.FlowPolishing {
				verb = "打磨"
			}
			return fmt.Sprintf("%s恢复：%d 章待处理", verb, len(progress.PendingRewrites))
		}
		if progress.Flow == domain.FlowReviewing {
			return "恢复：审阅中断"
		}
		if progress.InProgressChapter > 0 {
			return fmt.Sprintf("恢复：第 %d 章进行中", progress.InProgressChapter)
		}
		if label := describeArcEndLabel(store, progress); label != "" {
			return label
		}
		return fmt.Sprintf("恢复：从第 %d 章继续", progress.NextChapter())
	}
	return "恢复"
}

// startupResumeBlocked 判断启动时是否必须把恢复权交给用户。
//
// RunControl.AutoResume 是高峰/闲时调度使用的一次性许可；旧项目以及普通
// 会话退出并不一定拥有这张许可，但仍应保留原先“启动 exe 自动续跑”的行为。
// 这里仅对明确需要人工确认的事实 fail closed，避免启动恢复绕过裁决、
// 一次性暂停、逐章验收或已知错误；普通返工/打磨队列仍可从断点续跑。
func startupResumeBlocked(meta *domain.RunMeta, progress *domain.Progress) bool {
	if meta != nil {
		if strings.TrimSpace(meta.PendingSteer) != "" || meta.AdvanceHold != nil {
			return true
		}
		if meta.AdvanceMode == domain.ChapterAdvanceReview {
			return true
		}
		if control := meta.Control; control != nil {
			// Active 且没有停止记录表示上一次进程未完成终态收尾，原因未知。
			// 不自动重启，避免把崩溃/错误状态误判成普通断点。
			if control.Active && control.LastStop == nil && control.AutoResume == nil {
				return true
			}
			if stop := control.LastStop; stop != nil && !startupResumeStopAllowed(stop.Category) {
				return true
			}
		}
	}

	if progress == nil {
		return false
	}
	switch progress.Flow {
	case domain.FlowSteering:
		return true
	default:
		return false
	}
}

func startupResumeStopAllowed(category domain.StopCategory) bool {
	switch category {
	case "", domain.StopCategoryNaturalStop, domain.StopCategorySessionExit,
		domain.StopCategoryPeakPolicy, domain.StopCategoryIdleWindowEnd:
		return true
	default:
		return false
	}
}

// startupResumeDeferred 保留高峰策略：即使普通恢复状态允许自动续跑，
// 闲时写作或高峰自动暂停开启时，也要等到非高峰窗口再启动。
func startupResumeDeferred(meta *domain.RunMeta, now time.Time) bool {
	if meta == nil || !IdleWritingStatusAt(now).InPeak {
		return false
	}
	return meta.IdleWritingEnabled || meta.PeakAutoPauseEnabled
}

// describeArcEndLabel 为弧末/卷末的多种中间状态生成贴合 UI 的标签。
// 与 flow.Route 的弧末分支保持同序，保证 label 与 Router 首条指令对齐。
func describeArcEndLabel(store *storepkg.Store, progress *domain.Progress) string {
	if !progress.Layered || len(progress.CompletedChapters) == 0 {
		return ""
	}
	lastCh := progress.CompletedChapters[len(progress.CompletedChapters)-1]
	boundary, err := store.Outline.CheckArcBoundary(lastCh)
	if err != nil || boundary == nil || !boundary.IsArcEnd {
		return ""
	}
	vol, arc := boundary.Volume, boundary.Arc
	switch {
	case !store.World.HasArcReview(lastCh):
		return fmt.Sprintf("恢复：弧末评审待处理（V%d A%d）", vol, arc)
	case !store.Summaries.HasArcSummary(vol, arc):
		return fmt.Sprintf("恢复：弧摘要待生成（V%d A%d）", vol, arc)
	case boundary.IsVolumeEnd && !store.Summaries.HasVolumeSummary(vol):
		return fmt.Sprintf("恢复：卷摘要待生成（V%d）", vol)
	case boundary.NeedsExpansion && boundary.NextArc > 0:
		return fmt.Sprintf("恢复：待展开下一弧（V%d A%d）", boundary.NextVolume, boundary.NextArc)
	case boundary.NeedsNewVolume:
		return fmt.Sprintf("恢复：待决策下一卷（V%d 末）", vol)
	}
	return ""
}
