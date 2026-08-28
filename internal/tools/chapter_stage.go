package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── ChapterAction：章节流水线动作枚举（唯一权威） ─────────────────────
// 原 check_consistency_next_action.go 的 ActionDraftChapter 等独立字符串集合
// 已删除，改为本枚举的别名（见该文件），不再维护第二套常量。

type ChapterAction string

const (
	ChapterActionDraft  ChapterAction = "draft_chapter"
	ChapterActionEdit   ChapterAction = "edit_chapter"
	ChapterActionCheck  ChapterAction = "check_consistency"
	ChapterActionPolish ChapterAction = "polish_draft"
	ChapterActionReview ChapterAction = "review_style"
	ChapterActionCommit ChapterAction = "commit_chapter"
)

// ── ChapterStage：章节流水线阶段枚举 ──────────────────────────────────

type ChapterStage string

const (
	ChapterStageDisabled             ChapterStage = "disabled"                // pipeline 关 && critic=off，维持旧行为
	ChapterStageNeedsDraft           ChapterStage = "needs_draft"             // 新章节无草稿
	ChapterStageRewriteNotStarted    ChapterStage = "rewrite_not_started"     // rewrite queue 但草稿==原终稿
	ChapterStageDraftDirty           ChapterStage = "draft_dirty"             // 草稿已产生/修改，缺匹配 digest 的 checkpoint
	ChapterStageNeedsEdit            ChapterStage = "needs_edit"              // 已 check，有 error 级机械违规
	ChapterStageNeedsPolish          ChapterStage = "needs_polish"            // 首次 check 通过，缺 fresh/合法 polish
	ChapterStageNeedsPostPolishCheck ChapterStage = "needs_post_polish_check" // polish 完成，缺 seq 更晚的 check
	ChapterStageNeedsReview          ChapterStage = "needs_review"            // critic 需开始/继续/重绑定
	ChapterStageRevisionOpen         ChapterStage = "revision_open"           // critic 给 revision_open，草稿仍是旧 digest
	ChapterStageNeedsCommit          ChapterStage = "needs_commit"            // 所有门控事实满足，只允许 commit
	ChapterStageComplete             ChapterStage = "complete"                // 已完成且不在 rewrite queue
	ChapterStageBlocked              ChapterStage = "blocked"                 // exhausted/账本损坏/pending digest 冲突
)

// ── 配置与输入输出 ────────────────────────────────────────────────────

// ChapterFSMConfig 是章节流水线状态机的运行配置（BuildWorkers 注入六工具）。
type ChapterFSMConfig struct {
	Enabled               bool   // 生产 Writer 工具集 true；standalone 旧测试默认 false
	PipelineEnabled       bool   // 精修流水线开关（与 polish_draft/review_style/commit 的既有开关同源）
	ExpectedPolisherModel string // roles.polisher 显式配置时使用；空表示不检查
	// ViolationDetailEnabled 控制 FSM 拒绝消息是否携带 error 级机械违规明细
	// （rule/target/actual/limit 紧凑摘要）。默认开启（bootstrap.Config
	// ViolationDetailEnabled() 缺省 true）；关闭时错误消息与旧版逐字节一致。
	ViolationDetailEnabled bool
}

// ChapterFSMConfigurable 是六工具实现 FSM 配置注入的接口。
type ChapterFSMConfigurable interface {
	SetChapterFSMConfig(ChapterFSMConfig)
}

// ChapterStageInput 是 ComputeChapterStage 的纯函数输入快照。
// 由 ResolveChapterStage 从 store 加载；测试可直接构造。
type ChapterStageInput struct {
	Chapter               int
	PipelineEnabled       bool
	ExpectedPolisherModel string
	StyleReviewMode       domain.StyleQualityMode
	Completed             bool
	InRewriteQueue        bool
	DraftExists           bool
	DraftDigest           string
	FinalExists           bool
	FinalDigest           string
	HasMechanicalErrors   bool
	OnlyUnderMinError     bool
	LatestConsistency     *domain.Checkpoint
	LatestPolish          *domain.Checkpoint
	ReviewLedger          *domain.StyleReviewLedger
	// MechanicalViolations 是 error 级机械违规明细（ResolveChapterStage 重跑
	// computeMechanicalViolations 后按 hasErrorViolations 过滤的实际条目）。
	// 供 FSM 拒绝消息携带"修什么"的确定性事实（rule/target/actual/limit），
	// 避免模型在 needs_edit 阶段盲目重写（ch43 死循环根因）。
	MechanicalViolations []rules.Violation
}

// ChapterStageDecision 是一次 FSM 判定的完整结果。
type ChapterStageDecision struct {
	Stage     ChapterStage
	Allowed   []ChapterAction
	Required  ChapterAction
	Reason    string
	Recovery  string
	DraftMode string
	// Violations 是 error 级机械违规明细（与 ChapterStageInput.MechanicalViolations
	// 同源，由 ResolveChapterStage 填充；纯函数 ComputeChapterStage 不产生）。
	Violations []rules.Violation
}

// Allows 报告动作是否被当前阶段允许。
func (d ChapterStageDecision) Allows(action ChapterAction) bool {
	return slices.Contains(d.Allowed, action)
}

// RequiredNextAction 生成下一步正常建议（唯一来源：FSM 判定）。
// disabled/complete/blocked 无建议（返回 nil，不输出字段）；error 级机械违规
// 返回 edit_chapter 或 draft_chapter——不允许 hasErrors→nil 让模型自行猜测。
func (d ChapterStageDecision) RequiredNextAction() *RequiredNextAction {
	switch d.Stage {
	case ChapterStageDisabled, ChapterStageComplete, ChapterStageBlocked:
		return nil
	}
	action := d.Required
	if action == "" {
		switch d.Stage {
		case ChapterStageNeedsDraft:
			action = ChapterActionDraft
		case ChapterStageRewriteNotStarted:
			action = ChapterActionEdit
		case ChapterStageDraftDirty:
			action = ChapterActionCheck
		case ChapterStageNeedsEdit:
			action = ChapterActionEdit
		case ChapterStageNeedsPolish:
			action = ChapterActionPolish
		case ChapterStageNeedsPostPolishCheck:
			action = ChapterActionCheck
		case ChapterStageNeedsReview, ChapterStageRevisionOpen:
			action = ChapterActionReview
		case ChapterStageNeedsCommit:
			action = ChapterActionCommit
		}
	}
	if action == "" {
		return nil
	}
	return &RequiredNextAction{Action: string(action), Reason: d.Reason, Mode: d.DraftMode}
}

// ── 共享判定 helper ──────────────────────────────────────────────────

// freshConsistency 判断最新 consistency_check checkpoint 是否与当前草稿摘要
// 精确匹配（有效 digest + 相等）。
func freshConsistency(cp *domain.Checkpoint, draftDigest string) bool {
	return cp != nil && domain.IsValidDigest(cp.Digest) && cp.Digest == draftDigest
}

// validPolish 判断最新 polish checkpoint 是否为当前草稿的合法精修记录：
// digest 匹配 + stage 场景匹配（重写队列期望 "rewrite"，其余期望 "draft"）
// + 显式配置 polisher 模型时模型必须一致（ExpectedPolisherModel 空 = 不检查）。
// no-op（Changed=false）同样合法——防模型为改而改。
//
// degraded polish checkpoint（Degraded=true，精修失败降级记录，正文未变）同样视为
// 合法 polish 记录：digest 匹配 + stage 匹配即可，跳过 ExpectedPolisherModel 检查
// ——degraded 根本没调用模型（模型字段仅审计用）。这是"degraded → post-polish
// check → review"推进链的 FSM 侧开关：degraded 后不再判 needs_polish，杜绝
// "polish 失败 → 永远 needs_polish → 无脑重派同一章"的生产死锁（ch71 类）。
func validPolish(in ChapterStageInput, cp *domain.Checkpoint) bool {
	if cp == nil || !domain.IsValidDigest(cp.Digest) || cp.Digest != in.DraftDigest {
		return false
	}
	expectedStage := "draft"
	if in.InRewriteQueue {
		expectedStage = "rewrite"
	}
	if cp.Stage != expectedStage {
		return false
	}
	if cp.Degraded {
		return true
	}
	if in.ExpectedPolisherModel != "" && cp.PolisherModel != in.ExpectedPolisherModel {
		return false
	}
	return true
}

// reviewBindsPolish 判断 legacy（seq==0）评审条目是否按 wall-clock 绑定给定
// polish checkpoint：critic 完成时刻不得早于 polish 完成时刻超过 1 秒
// （CreatedAt 为 RFC3339 整秒精度，丢失小数秒；OccurredAt 保留小数秒——
// 同秒内 critic 实际晚于 polish 也可能被判更早，故容忍 1 秒窗口）。
// 时间缺失或解析失败时按"已绑定"处理（fail-open）：legacy 账本可能缺失时间
// 字段，fail-closed 会让旧账本突然死锁。与 CheckPolishPipelineGate 的 legacy
// 回退语义一致（两处共用本 helper，规格 §4）。
func reviewBindsPolish(entry *domain.StyleReviewEntry, polish *domain.Checkpoint) bool {
	if entry == nil || polish == nil {
		return false
	}
	if entry.CreatedAt == "" || polish.OccurredAt.IsZero() {
		return true
	}
	criticAt, err := time.Parse(time.RFC3339, entry.CreatedAt)
	if err != nil {
		return true
	}
	return !criticAt.Add(time.Second).Before(polish.OccurredAt)
}

// reviewBindingValid 判断评审周期是否绑定当前候选（critic 分支的 commit 前置）：
// pipeline 关闭时不要求 polish 绑定（旧行为）——先于 Request/polish 检查，
// 只要存在评审周期即视为绑定（legacy 条目可能缺失 Request，不得因此拒绝）；
// pipeline 开启时要求 Request.PolishCheckpointSeq 严格等于最新 polish seq
// （R == latest P）；legacy（seq==0）回退 reviewBindsPolish 的 wall-clock
// 一秒容差。
func reviewBindingValid(in ChapterStageInput, cycle *domain.StyleReviewEntry) bool {
	if !in.PipelineEnabled {
		return cycle != nil
	}
	if cycle == nil || cycle.Request == nil {
		return false
	}
	if in.LatestPolish == nil {
		return false
	}
	if bound := cycle.Request.PolishCheckpointSeq; bound > 0 {
		return bound == in.LatestPolish.Seq
	}
	return reviewBindsPolish(cycle, in.LatestPolish)
}

// ── RequiredNextAction：下一步建议（迁移自 check_consistency_next_action.go） ──
// 对当前章节状态的下一步正常建议，作为辅助提示而非指令。
// 仅由 (ChapterStageDecision) RequiredNextAction() 生成，是"下一步是什么"
// 的唯一来源（规格第 11 节：不允许 hasErrors→nil）。

type RequiredNextAction struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	Mode   string `json:"mode,omitempty"`
}

// ── 判定构造器 ───────────────────────────────────────────────────────

func decision(stage ChapterStage, allowed []ChapterAction, required ChapterAction, reason string) ChapterStageDecision {
	return ChapterStageDecision{Stage: stage, Allowed: allowed, Required: required, Reason: reason}
}

func disabledDecision() ChapterStageDecision {
	return ChapterStageDecision{Stage: ChapterStageDisabled, Reason: "流水线关闭且 critic 关闭，维持旧行为"}
}

func completeDecision(reason string) ChapterStageDecision {
	return decision(ChapterStageComplete, nil, "", reason)
}

func blockedDecision(reason, recovery string) ChapterStageDecision {
	return ChapterStageDecision{Stage: ChapterStageBlocked, Reason: reason, Recovery: recovery}
}

func needsReviewDecision(reason string) ChapterStageDecision {
	return decision(ChapterStageNeedsReview, []ChapterAction{ChapterActionReview}, ChapterActionReview, reason)
}

// commitArgsHint 是 needs_commit reason 的固定后缀：commit_chapter 参数多，
// 模型若不知道必传参数会反复被工具拒参。world_state_mode 仅在重写已完成章节
// （PendingRewrites 队列）时必传。
const commitArgsHint = " commit_chapter 需要 summary/characters/key_events 等参数；world_state_mode 仅在重写已完成章节时必传（preserve=纯文风重写/replace=剧情重写）。"

func needsCommitDecision(reason string) ChapterStageDecision {
	return decision(ChapterStageNeedsCommit, []ChapterAction{ChapterActionCommit}, ChapterActionCommit, reason+commitArgsHint)
}

// postPolishEdit 判断当前草稿是否在最后一次 polish 之后又被修改过：
// 最后一次 polish checkpoint 存在且 digest（即 output_digest，见
// domain.Checkpoint 注释）合法但与当前草稿 digest 不一致。
// 这是生产日志中 needs_polish 反复拒绝的最大错误源——writer 在 polish 后
// 继续 edit 草稿，导致 polish checkpoint digest 失效、commit 被拒。
func postPolishEdit(in ChapterStageInput) bool {
	return in.LatestPolish != nil &&
		domain.IsValidDigest(in.LatestPolish.Digest) &&
		in.LatestPolish.Digest != in.DraftDigest
}

// ── ComputeChapterStage：纯函数状态机计算器 ──────────────────────────
// 不依赖 store：所有状态数据由调用方传入（ChapterStageInput）。
// 控制面唯一权威：任何工具的"下一步"建议与 guard 允许集都只来自本函数。

func ComputeChapterStage(in ChapterStageInput) ChapterStageDecision {
	// pipeline 关 && critic=off → 维持旧行为（无控制面约束）。
	if !in.PipelineEnabled && in.StyleReviewMode != domain.StyleQualityCritic {
		return disabledDecision()
	}
	if in.Completed && !in.InRewriteQueue {
		return completeDecision("章节已完成且不在重写队列")
	}

	var status domain.StyleReviewStatus
	var cycle *domain.StyleReviewEntry
	if in.ReviewLedger != nil {
		status = in.ReviewLedger.CurrentStatus()
		cycle = in.ReviewLedger.CurrentCycle()
	}

	if status == domain.ReviewStatusExhausted {
		return blockedDecision("风格评审已耗尽", "先执行 /style-override，禁止继续修改或提交")
	}
	if status == domain.ReviewStatusInitialPending || status == domain.ReviewStatusFinalPending {
		if !in.DraftExists || cycle == nil || cycle.DraftDigest != in.DraftDigest {
			return blockedDecision("pending 评审绑定的草稿与当前草稿不一致", "停止自动写作并人工恢复账本")
		}
		return needsReviewDecision("已有待完成的评审")
	}
	if !in.DraftExists {
		if in.InRewriteQueue {
			return decision(ChapterStageRewriteNotStarted,
				[]ChapterAction{ChapterActionDraft, ChapterActionEdit}, ChapterActionEdit, "重写草稿尚未播种")
		}
		// 阻塞项 8：非返工章节存在 terminal ledger 但草稿丢失 → blocked。
		// CheckStyleReviewMutationGuard 对 terminal 状态拒绝 draft_chapter，返回
		// needs_draft 会让 FSM 允许一个被 mutation guard 拒绝的动作（与 P1-5
		// 同类 FSM/guard 不变量破坏）。degraded 除外：guard 允许起草（degraded
		// 是评审调用故障而非评审结论）。
		if status.IsTerminal() && status != domain.ReviewStatusDegraded {
			return blockedDecision("ledger 终态但草稿不存在", "terminal 账本与草稿不一致：停止自动写作并人工恢复草稿/账本")
		}
		return decision(ChapterStageNeedsDraft,
			[]ChapterAction{ChapterActionDraft}, ChapterActionDraft, "新章节尚无草稿")
	}

	// 重写队列但草稿仍等于原终稿：尚未开始重写。
	rewriteNotStarted := in.InRewriteQueue && in.FinalExists && in.DraftDigest == in.FinalDigest
	if rewriteNotStarted {
		return decision(ChapterStageRewriteNotStarted,
			[]ChapterAction{ChapterActionDraft, ChapterActionEdit}, ChapterActionEdit, "当前草稿仍等于原终稿，尚未开始重写")
	}

	reviewDigestMatches := cycle != nil && domain.IsValidDigest(cycle.DraftDigest) && cycle.DraftDigest == in.DraftDigest
	terminalCurrent := status.IsTerminal() && reviewDigestMatches

	if status == domain.ReviewStatusRevisionOpen && reviewDigestMatches {
		return decision(ChapterStageRevisionOpen,
			[]ChapterAction{ChapterActionDraft, ChapterActionEdit}, ChapterActionEdit, "评审要求修订，当前草稿尚未修改")
	}

	// P1-5：非返工章节的 terminal ledger 与当前草稿 digest 不匹配 → blocked（人工恢复）。
	// 必须早于 pipeline freshness 判定：否则（accepted_revised + digest 不匹配 +
	// polish 陈旧）会先返回 needs_polish——FSM 允许 polish_draft，但
	// CheckStyleReviewMutationGuard 对 terminal 状态锁定正文、拒绝修改，模型照
	// required 调用 polish_draft 必被拒，形成 ch450 类死锁（FSM 允许了一个随后
	// 必然被 mutation guard 拒绝的动作）。同理必须早于 consistency/机械违规分支：
	// draft/edit 同样被 terminal 锁定，返回 draft_dirty/needs_edit 会让 FSM 与
	// guard 自相矛盾（P1-6）。
	// degraded 除外：degraded 是评审调用故障而非评审结论，guard 允许修改
	// （draft/edit/polish 均放行），needs_review/needs_polish 均合法，不得误伤。
	if status.IsTerminal() && status != domain.ReviewStatusDegraded && !reviewDigestMatches && !in.InRewriteQueue {
		return blockedDecision("ledger 终态与当前草稿不匹配", "停止自动写作并修复 ledger/候选状态")
	}

	consistencyFresh := freshConsistency(in.LatestConsistency, in.DraftDigest)
	if !consistencyFresh {
		if terminalCurrent {
			return decision(ChapterStageDraftDirty,
				[]ChapterAction{ChapterActionCheck}, ChapterActionCheck, "终审候选缺少当前摘要的一致性检查点")
		}
		allowed := []ChapterAction{ChapterActionDraft, ChapterActionCheck}
		if in.InRewriteQueue || status == domain.ReviewStatusRevisionOpen {
			allowed = append(allowed, ChapterActionEdit)
		}
		return decision(ChapterStageDraftDirty, allowed, ChapterActionCheck, "草稿已修改，必须重新检查")
	}

	if in.HasMechanicalErrors {
		if terminalCurrent {
			return blockedDecision("已终审候选在当前规则下出现机械 error，禁止静默改写", "显式重新加入重写队列或人工处理")
		}
		allowed := []ChapterAction{ChapterActionDraft}
		required := ChapterActionDraft
		if in.InRewriteQueue || status == domain.ReviewStatusRevisionOpen {
			allowed = append(allowed, ChapterActionEdit)
			required = ChapterActionEdit
		}
		reason := "一致性检查仍有 error 级机械违规"
		draftMode := ""
		if in.OnlyUnderMinError && !in.InRewriteQueue && status == "" && required == ChapterActionDraft {
			reason = "当前唯一 error 是章节字数低于下限；已有正文应调用 draft_chapter(mode=append) 续写，禁止用 mode=write 整章重写。"
			draftMode = "append"
		}
		d := decision(ChapterStageNeedsEdit, allowed, required, reason)
		d.DraftMode = draftMode
		return d
	}

	if in.PipelineEnabled {
		polishFresh := validPolish(in, in.LatestPolish)
		if !polishFresh {
			if terminalCurrent {
				return blockedDecision("终审候选缺少合法 polish 绑定，正文已锁定", "显式开启新的重写/评审周期")
			}
			reason := "首次 consistency 已通过，需要精修当前草稿"
			if postPolishEdit(in) {
				// 当前阶段已是 needs_polish：check_consistency 会被 FSM 拒绝
				// （allowed 只有 polish_draft），故不得再引导"先 check"。
				// 唯一动作是 polish；成功后 check 一次，再跟随 required_next_action。
				reason = fmt.Sprintf("草稿在精修后已被修改（digest 与最后一次 polish 记录不一致）。当前唯一动作：调用 polish_draft(chapter=%d)，成功后调用一次 check_consistency，再严格执行 required_next_action。禁止 edit_chapter / commit_chapter。", in.Chapter)
			}
			return decision(ChapterStageNeedsPolish,
				[]ChapterAction{ChapterActionPolish}, ChapterActionPolish, reason)
		}
		if in.LatestConsistency.Seq <= in.LatestPolish.Seq {
			return decision(ChapterStageNeedsPostPolishCheck,
				[]ChapterAction{ChapterActionCheck}, ChapterActionCheck, "polish 已产生新候选，必须重新检查")
		}
	}

	if in.StyleReviewMode == domain.StyleQualityCritic {
		switch {
		case status == "":
			return needsReviewDecision("当前候选尚未评审")
		case status == domain.ReviewStatusRevisionOpen:
			return needsReviewDecision("修订已完成，需要最终评审")
		case status == domain.ReviewStatusDegraded:
			if terminalCurrent && reviewBindingValid(in, cycle) {
				return needsCommitDecision("degraded 候选未变化，沿用现有可提交语义")
			}
			return needsReviewDecision("degraded 评审需要恢复或绑定新候选")
		case status.IsTerminal():
			if terminalCurrent && reviewBindingValid(in, cycle) {
				return needsCommitDecision("当前候选已通过终验")
			}
			if in.InRewriteQueue {
				return needsReviewDecision("现有 terminal 属于旧候选，需开启新 epoch")
			}
			// P1-5 兜底：非重写章节的 terminal mismatch 已在流水线判定之前拦截为
			// blocked（见上方 P1-5 分支）；此处仅在绑定校验失败（R!=P 等）时可达，
			// 保持 blocked 语义不变（纵深防御，不返回 needs_polish）。
			return blockedDecision("非重写章节的 terminal 账本与当前草稿不匹配", "停止自动写作并人工恢复")
		default:
			return blockedDecision("未知评审状态", "检查 style review ledger")
		}
	}
	return needsCommitDecision("pipeline 已完成，critic 模式关闭")
}

// ── ResolveChapterStage：快照加载器 ──────────────────────────────────
// 按规格第 3 节加载全部门控事实并调用 ComputeChapterStage。
// Store 读错误直接返回 error，不伪装正常阶段。

func ResolveChapterStage(st *store.Store, chapter int, cfg ChapterFSMConfig) (ChapterStageDecision, error) {
	// 1. RunMeta → StyleReviewMode（缺失按 off 处理）。
	styleReviewMode := domain.StyleQualityOff
	if meta, err := st.RunMeta.Load(); err != nil {
		return ChapterStageDecision{}, fmt.Errorf("load run meta: %w: %w", errs.ErrStoreRead, err)
	} else if meta != nil {
		styleReviewMode = meta.StyleReviewMode
	}

	// 2. Progress → Completed / InRewriteQueue（completed && pending_rewrites）。
	completed, inRewriteQueue := false, false
	if progress, err := st.Progress.Load(); err != nil {
		return ChapterStageDecision{}, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	} else if progress != nil {
		completed = slices.Contains(progress.CompletedChapters, chapter)
		inRewriteQueue = completed && slices.Contains(progress.PendingRewrites, chapter)
	}

	// 3. 草稿 + digest。
	draft, err := st.Drafts.LoadDraft(chapter)
	if err != nil {
		return ChapterStageDecision{}, fmt.Errorf("load draft: %w: %w", errs.ErrStoreRead, err)
	}
	draftExists := draft != ""
	draftDigest := ""
	if draftExists {
		draftDigest = domain.DigestDraft(draft)
	}

	// 4. rewrite queue 时加载原终稿 digest（判断草稿是否已实际变更）。
	finalExists, finalDigest := false, ""
	if inRewriteQueue {
		finalText, ferr := st.Drafts.LoadChapterText(chapter)
		if ferr != nil {
			return ChapterStageDecision{}, fmt.Errorf("load final chapter text: %w: %w", errs.ErrStoreRead, ferr)
		}
		finalExists = finalText != ""
		if finalExists {
			finalDigest = domain.DigestDraft(finalText)
		}
	}

	// 5. 最新 consistency_check / polish checkpoint + style review ledger。
	scope := domain.ChapterScope(chapter)
	latestConsistency := st.Checkpoints.LatestByStep(scope, "consistency_check")
	latestPolish := st.Checkpoints.LatestByStep(scope, "polish")
	ledger, lerr := st.StyleReview.Load(chapter)
	if lerr != nil {
		return ChapterStageDecision{}, fmt.Errorf("load style review ledger: %w: %w", errs.ErrStoreRead, lerr)
	}

	// 6. 草稿存在时重跑机械违规判定（checkpoint 未持久化 violation verdict；
	//    重跑确定性且无需模型），只取 hasErrorViolations。
	hasErrors := false
	onlyUnderMinError := false
	var errorViolations []rules.Violation
	if draftExists {
		wordCount := utf8.RuneCountInString(draft)
		violations := computeMechanicalViolations(st, draft, wordCount)
		hasErrors = hasErrorViolations(violations)
		onlyUnderMinError = onlyUnderMinChapterWordsError(st, draft, wordCount)
		// 只保留 error 级条目：FSM 拒绝只关心"必须修什么"，warning 由模型
		// 按文风自主裁定，不进入拒绝明细（保持消息紧凑）。
		for _, v := range violations {
			if v.Severity == rules.SeverityError {
				errorViolations = append(errorViolations, v)
			}
		}
	}

	in := ChapterStageInput{
		Chapter:               chapter,
		PipelineEnabled:       cfg.PipelineEnabled,
		ExpectedPolisherModel: cfg.ExpectedPolisherModel,
		StyleReviewMode:       styleReviewMode,
		Completed:             completed,
		InRewriteQueue:        inRewriteQueue,
		DraftExists:           draftExists,
		DraftDigest:           draftDigest,
		FinalExists:           finalExists,
		FinalDigest:           finalDigest,
		HasMechanicalErrors:   hasErrors,
		OnlyUnderMinError:     onlyUnderMinError,
		LatestConsistency:     latestConsistency,
		LatestPolish:          latestPolish,
		ReviewLedger:          ledger,
		MechanicalViolations:  errorViolations,
	}
	decision := ComputeChapterStage(in)
	decision.Violations = errorViolations
	return decision, nil
}

// ── RequireChapterAction：统一强制入口 ────────────────────────────────
// 六工具在 Execute 入口调用；cfg.Enabled=false 时不拦截（standalone 旧行为）。

// FSM 拒绝审计记录（meta/decisions.jsonl，append-only）：
// 每次 FSM 拒绝落一条 Kind=chapter_fsm_denied 的记录，Reason 携带
// "chapter=<n> code=<拒绝码>" 稳定键。拒绝计数升级（DeniedCount）从最近
// 记录中统计"同一章节同一拒绝码"的连续拒绝次数——持久化在 decisions.jsonl，
// 重启不丢失；成功（Allows）后计数自然归零（后续记录不再是连续拒绝）。
const (
	fsmDeniedDecisionKind    = "chapter_fsm_denied"
	fsmDeniedDecisionDecider = "fsm"
	// fsmDeniedRecentWindow 统计窗口：只回看最近 N 条记录（拒绝计数只关心
	// 紧邻的连续拒绝，窗口足够覆盖"连续 ≥2 次"判定即可，避免长文件全量扫描）。
	fsmDeniedRecentWindow = 32
)

// fsmDeniedKey 生成拒绝记录的稳定键：chapter=<n> code=<拒绝码>。
// 拒绝码 = "chapter_fsm_transition_denied" + 该章节 + required 动作
// （同一章节同一 required 的连续拒绝才升级；不同 required 视为不同拒绝）。
func fsmDeniedKey(chapter int, required ChapterAction) string {
	return fmt.Sprintf("chapter=%d code=chapter_fsm_transition_denied required=%s", chapter, required)
}

// countConsecutiveFSMDenials 从 decisions.jsonl 最近记录中统计指定章节
// 指定拒绝码的连续拒绝次数。best-effort：读取失败返回 0（行为退化为现状，
// 不因审计文件问题阻断工具调用）。
//
// 扫描规则：从最新往旧只统计同一章节同一拒绝码的 chapter_fsm_denied 记录；
// 其它审计记录（worker_failure/plan_start/intervention 等）是引擎在两次拒绝
// 之间插入的旁路记录（如失败裁定），不打断拒绝链；遇到不同章节或不同拒绝码
// 的 chapter_fsm_denied 才视为链被打断、停止计数。
func countConsecutiveFSMDenials(st *store.Store, chapter int, required ChapterAction) int {
	recs, err := st.Decisions.Recent(fsmDeniedRecentWindow)
	if err != nil || len(recs) == 0 {
		return 0
	}
	key := fsmDeniedKey(chapter, required)
	count := 0
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if rec.Kind != fsmDeniedDecisionKind {
			continue
		}
		if rec.Reason != key {
			break
		}
		count++
	}
	return count
}

// recordFSMDenial 落一条 FSM 拒绝审计记录（best-effort，失败不阻断拒绝本身）。
func recordFSMDenial(st *store.Store, chapter int, required ChapterAction, attempted ChapterAction, stage ChapterStage) {
	rec := store.DecisionRecord{
		Kind:    fsmDeniedDecisionKind,
		Decider: fsmDeniedDecisionDecider,
		Reason:  fsmDeniedKey(chapter, required),
	}
	if data, err := json.Marshal(map[string]any{
		"chapter":   chapter,
		"stage":     string(stage),
		"attempted": string(attempted),
		"required":  string(required),
	}); err == nil {
		rec.Facts = data
	}
	if _, err := st.Decisions.Append(rec); err != nil {
		// 审计失败不阻断拒绝（错误消息本身已含全部指引信息）。
		return
	}
}

func RequireChapterAction(st *store.Store, chapter int, attempted ChapterAction, cfg ChapterFSMConfig) error {
	if !cfg.Enabled {
		return nil
	}
	decision, err := ResolveChapterStage(st, chapter, cfg)
	if err != nil {
		return fmt.Errorf("resolve chapter pipeline stage: %w: %w", errs.ErrStoreRead, err)
	}
	if decision.Stage == ChapterStageDisabled {
		return nil
	}
	if decision.Allows(attempted) {
		return nil
	}
	// 拒绝计数升级：先落审计记录，再统计连续拒绝次数（含本次）。
	// 顺序不可颠倒——先 Append 后 Recent 才能把本次拒绝计入连续链。
	recordFSMDenial(st, chapter, decision.Required, attempted, decision.Stage)
	deniedCount := countConsecutiveFSMDenials(st, chapter, decision.Required)
	te := &ChapterTransitionError{
		Chapter:     chapter,
		Stage:       decision.Stage,
		Attempted:   attempted,
		Required:    decision.Required,
		Allowed:     decision.Allowed,
		Reason:      decision.Reason,
		Recovery:    decision.Recovery,
		DeniedCount: deniedCount,
	}
	// 违规明细开关：开启时把 error 级机械违规复制进拒绝错误，Error() 据此
	// 追加 violations=[...] 紧凑摘要（模型可见"修什么"）。关闭时保持旧消息。
	if cfg.ViolationDetailEnabled && len(decision.Violations) > 0 {
		te.Violations = append([]rules.Violation(nil), decision.Violations...)
	}
	return te
}

// ── ChapterTransitionError：FSM 拦截错误 ─────────────────────────────
// 错误消息格式稳定、可测试、可指导模型（见规格第 7 节）。
// Unwrap 返回 errs.ErrToolPrecondition。

type ChapterTransitionError struct {
	Chapter   int
	Stage     ChapterStage
	Attempted ChapterAction
	Required  ChapterAction
	Allowed   []ChapterAction
	Reason    string
	Recovery  string
	// DeniedCount 是该章节该拒绝码（required 动作）的连续拒绝次数（含本次）。
	// >=2 时 Error() 追加强制指令，直接给出必须调用的工具与参数，并提示
	// 不要重复获取上下文（模型指令遵循差时的拒绝升级机制）。
	DeniedCount int
	// Violations 是 error 级机械违规明细（仅 ViolationDetailEnabled 时由
	// RequireChapterAction 填充）。非空时 Error() 追加 violations=[...] 紧凑
	// 摘要，让模型看到"修什么"（rule/target/actual/limit），避免 needs_edit
	// 阶段盲目重写（ch43 死循环根因）。
	Violations []rules.Violation
}

// ── 违规明细紧凑摘要（FSM 拒绝消息的确定性输出） ──────────────────────
// 43 章死循环根因：模型在 needs_edit 阶段反复 draft_chapter，但拒绝消息只有
// "一致性检查仍有 error 级机械违规"，不含 rule/target/actual/limit——模型看不到
// "修什么"只能盲目重写。本摘要把 error 级违规逐条压缩进拒绝消息：
//
//	violations=[rule=literary_prose target="禁词清零：浑身发抖 1处：她浑身发抖" actual=1 limit=1 severity=error; ...]
//
// 约束：最多 5 条；每条 target 截断到 60 runes（超长加 …）；只含 error 级
// （防御性过滤，不依赖上游）；仅在 ViolationDetailEnabled 时输出（保持旧消息兼容）。

// maxViolationSummaryEntries 拒绝消息中违规摘要的最大条数。
const maxViolationSummaryEntries = 5

// maxViolationTargetRunes 每条违规 target 在摘要中的最大长度（rune 计数）。
const maxViolationTargetRunes = 60

// violationSummary 把一条违规压缩为 "rule=... target=... actual=... limit=... severity=..." 片段。
// target 超长截断到 maxViolationTargetRunes runes（加 …），绝不按 byte 切坏 UTF-8。
// limit 为 nil/空时省略（forbidden_chars/forbidden_phrases 无阈值概念）。
func violationSummary(v rules.Violation) string {
	target := v.Target
	if utf8.RuneCountInString(target) > maxViolationTargetRunes {
		target = string([]rune(target)[:maxViolationTargetRunes]) + "…"
	}
	limit := ""
	if v.Limit != nil && fmt.Sprintf("%v", v.Limit) != "" {
		limit = fmt.Sprintf(" limit=%v", v.Limit)
	}
	return fmt.Sprintf("rule=%s target=%q actual=%v%s severity=%s", v.Rule, target, v.Actual, limit, v.Severity)
}

// violationsSummary 把 error 级违规列表压缩为 "violations=[...]" 摘要串。
// 空列表返回空串；最多 maxViolationSummaryEntries 条，超出截断；防御性
// 只取 error 级条目（不依赖上游过滤——ChapterTransitionError.Violations 是
// 公开字段，调用方可能直接构造含 warning 的列表）。
func violationsSummary(vs []rules.Violation) string {
	if len(vs) == 0 {
		return ""
	}
	parts := make([]string, 0, min(len(vs), maxViolationSummaryEntries))
	for _, v := range vs {
		if v.Severity != rules.SeverityError {
			continue
		}
		parts = append(parts, violationSummary(v))
		if len(parts) >= maxViolationSummaryEntries {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " violations=[" + strings.Join(parts, "; ") + "]"
}

// recoveryHint 按 required action 生成 action-specific 的 recovery 文案。
// check_consistency/polish_draft/review_style 是单参直达工具，直接给完整调用示例；
// edit_chapter/draft_chapter 需要多参/正文——若只给 {"chapter":N} 是不完整调用，
// 模型照抄必再错（生产日志最大错误源之一），因此给出"先取数、再调用"的指引。
// blocked 阶段不走本函数（见 Error()），Recovery 字段已含升级人工的文案。
func recoveryHint(required ChapterAction, chapter int) string {
	switch required {
	case ChapterActionCheck, ChapterActionPolish, ChapterActionReview:
		return fmt.Sprintf("调用 %s({\"chapter\":%d})，然后严格执行其 required_next_action。", required, chapter)
	case ChapterActionEdit:
		return fmt.Sprintf("先 read_chapter(chapter=%d, source='draft') 读取当前草稿，从草稿中精确复制唯一的 old_string（含空白），再调用 edit_chapter({\"chapter\":%d, \"old_string\":..., \"new_string\":...})；old_string 必须与草稿逐字一致。",
			chapter, chapter)
	case ChapterActionDraft:
		return fmt.Sprintf("先 read_chapter(chapter=%d, source='draft')/novel_context 获取上下文，再调用 draft_chapter({\"chapter\":%d, \"content\":\"完整正文\", \"mode\":\"write\"|\"append\"})。",
			chapter, chapter)
	default:
		return "根据 reason 提示执行正确的下一步动作，然后严格执行其 required_next_action。"
	}
}

func (e *ChapterTransitionError) Error() string {
	required := string(e.Required)
	if required == "" {
		required = "none"
	}
	if e.Stage == ChapterStageBlocked {
		return fmt.Sprintf("code=chapter_fsm_blocked chapter=%d stage=%s attempted=%s required=%s reason=%s recovery=%s",
			e.Chapter, e.Stage, e.Attempted, required, e.Reason, e.Recovery)
	}
	allowed := make([]string, 0, len(e.Allowed))
	for _, a := range e.Allowed {
		allowed = append(allowed, string(a))
	}
	allowedStr := strings.Join(allowed, ",")
	if allowedStr == "" {
		allowedStr = "none"
	}
	msg := fmt.Sprintf("code=chapter_fsm_transition_denied chapter=%d stage=%s attempted=%s required=%s allowed=[%s] reason=%s 下一步：%s",
		e.Chapter, e.Stage, e.Attempted, required, allowedStr, e.Reason, recoveryHint(e.Required, e.Chapter))
	// 违规明细（ViolationDetailEnabled 时由 RequireChapterAction 填充）：
	// 追加紧凑摘要，让模型看到"修什么"（rule/target/actual/limit）。
	// 只含 error 级、最多 5 条、target 截断 60 runes；空列表不追加（旧消息兼容）。
	msg += violationsSummary(e.Violations)
	// 拒绝计数升级：同一章节同一拒绝码连续 ≥2 次时追加强制指令。
	// 只升级"拒绝"场景，不改变正常流程；code= 前缀保持稳定（测试依赖）。
	// required 为空（如 complete 阶段无下一步工具）时不升级——没有可强制的工具。
	if e.DeniedCount >= 2 && e.Required != "" {
		msg += fmt.Sprintf(" 你已被同一原因连续拒绝 %d 次。必须立即调用 %s（参数见上方 recoveryHint），不要再次调用 novel_context / read_chapter / check_consistency 获取上下文。",
			e.DeniedCount, required)
	}
	return msg
}

func (e *ChapterTransitionError) Unwrap() error { return errs.ErrToolPrecondition }
