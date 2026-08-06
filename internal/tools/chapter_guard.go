package tools

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
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
// 在 critic 模式下，仅当没有账本、当前状态为 revision_open 或 degraded 时
// 才允许 draft/edit。
// pending（initial_pending/final_pending）以及 terminal 状态
// （accepted_initial、accepted_revised、overridden）拒绝修改；exhausted 也拒绝修改。
// 已完成且在 PendingRewrites 队列中的章节不再无条件豁免，改为 digest/status 感知
// （见下方重写队列分支）：pending/exhausted 优先拒绝；原终稿未开始重写
// （rewriteNotStarted）且账本为 terminal 时允许开始修改；revision_open/degraded
// 允许；当前重写候选已获终态评审且 digest 匹配 → 正文已锁定，只允许 commit；
// 旧 terminal digest 不匹配当前返工草稿 → 返工进行中，允许继续。
//
// degraded 是"评审调用故障"（如 critic 空输出的瞬态技术故障），不是评审结论：
// 允许重新起草（draft/edit），随后 check_consistency + review_style 发起新 attempt
// 重新评审，避免被瞬态故障永久锁死。
//
// 其余 Terminal 状态（accepted_initial、accepted_revised、overridden）代表
// "快照权威"——评审时草稿的快照已被接受/覆盖，即使后续基础配置（风格目标、规则、
// 锚点、批评者提示）发生变更，也不应再修改草稿。基础变更后若要更新草稿，
// 须通过重写/打磨路径（PendingRewrites）。
func CheckStyleReviewMutationGuard(st *store.Store, chapter int) error {
	meta, err := st.RunMeta.Load()
	if err != nil {
		return fmt.Errorf("load run meta: %w: %w", errs.ErrStoreRead, err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return nil // off 模式不拦截
	}

	ledger, err := st.StyleReview.Load(chapter)
	if err != nil {
		return fmt.Errorf("load style review ledger: %w: %w", errs.ErrStoreRead, err)
	}

	// 已完成 + 重写队列 → digest/status 感知豁免（不再无条件 bypass）：
	// 场景 1. 原终稿未开始重写（draft=="" 或 Digest(draft)==Digest(final)）+ terminal
	//          → 允许开始修改。
	// 场景 2. revision_open → 必须允许修改（不要求 digest 已变化）。
	// 场景 3. 当前候选已获 terminal 评审（draft!=final && digest 匹配）
	//          → 正文已锁定，只允许 commit_chapter（或重新评审）。
	// 场景 4. 旧 terminal digest 不匹配当前返工草稿 → 返工进行中，允许继续。
	// 特别：exhausted 不是"允许开始重写"的 terminal，必须优先拒绝并要求
	// /style-override，否则第一次修改后立即进入无法 review/commit 的死路。
	if isCompletedAndInRewriteQueue(st, chapter) && ledger != nil && !ledger.IsEmpty() {
		status := ledger.CurrentStatus()
		cycle := ledger.CurrentCycle()

		if status == domain.ReviewStatusInitialPending || status == domain.ReviewStatusFinalPending {
			return fmt.Errorf("critic 模式：章节 %d 有未完成评审（%s），不能修改: %w",
				chapter, status, errs.ErrToolPrecondition)
		}
		if status == domain.ReviewStatusExhausted {
			return fmt.Errorf("critic 模式：章节 %d 评审已耗尽，必须先 /style-override，不能修改: %w",
				chapter, errs.ErrToolPrecondition)
		}

		draft, dErr := st.Drafts.LoadDraft(chapter)
		if dErr != nil {
			return fmt.Errorf("load draft: %w: %w", errs.ErrStoreRead, dErr)
		}
		final, fErr := st.Drafts.LoadChapterText(chapter)
		if fErr != nil {
			return fmt.Errorf("load final chapter: %w: %w", errs.ErrStoreRead, fErr)
		}
		draftExists, finalExists := draft != "", final != ""
		draftDigest := ""
		if draftExists {
			draftDigest = domain.DigestDraft(draft)
		}
		rewriteNotStarted := !draftExists || (finalExists && draftDigest == domain.DigestDraft(final))

		if rewriteNotStarted && status.IsTerminal() {
			return nil // 原终稿未开始重写：允许开始修改
		}
		if status == domain.ReviewStatusRevisionOpen {
			return nil // 必须允许修改
		}
		if status == domain.ReviewStatusDegraded {
			return nil // 保留技术故障恢复语义
		}
		terminalMatchesCurrent := status.IsTerminal() && cycle != nil &&
			domain.IsValidDigest(cycle.DraftDigest) && cycle.DraftDigest == draftDigest
		if terminalMatchesCurrent && !rewriteNotStarted {
			return fmt.Errorf("critic 模式：章节 %d 当前重写候选已获终态评审（%s）且摘要匹配，正文已锁定；只能 commit_chapter: %w",
				chapter, status, errs.ErrToolPrecondition)
		}
		if status.IsTerminal() {
			return nil // 旧 terminal digest 不匹配当前返工草稿：返工进行中，允许继续
		}
		return fmt.Errorf("critic 模式：章节 %d 评审状态 %s 不允许修改: %w",
			chapter, status, errs.ErrToolPrecondition)
	}

	if ledger == nil || ledger.IsEmpty() {
		return nil // 尚无风格评审，允许首次起草
	}
	curr := ledger.CurrentStatus()
	switch curr {
	case domain.ReviewStatusRevisionOpen:
		return nil // 修订打开的章可以改
	case domain.ReviewStatusDegraded:
		return nil // degraded=评审调用故障（非评审结论），允许重新起草后重新评审
	case domain.ReviewStatusInitialPending, domain.ReviewStatusFinalPending:
		return fmt.Errorf("critic 模式：章节 %d 有未完成的评审（%s），不能修改: %w",
			chapter, curr, errs.ErrToolPrecondition)
	default:
		// terminal（accepted_initial/accepted_revised/overridden）+ exhausted 拒绝修改
		return fmt.Errorf("critic 模式：章节 %d 评审状态 %s 不允许修改: %w",
			chapter, curr, errs.ErrToolPrecondition)
	}
}

// CheckCommitStyleGate 在 critic 模式下校验 commit 的前置条件。
// 要求：
//   - 最新 consistency checkpoint 若存在，其摘要必须精确匹配当前草稿摘要（通用 freshness 校验）
//   - 账本状态为 eligible terminal（accepted_initial、accepted_revised、degraded、overridden）
//   - 当前草稿摘要匹配账本中最新条目的摘要
//   - 当前基础摘要匹配账本中最新条目的基础摘要（检测风格目标/规则/锚点/批评者提示变更）
//
// Freshness 校验在 mode 检查之前执行，确保一致性检查点不被绕过。
// 无 checkpoint + off 模式仍允许（兼容边界）。
//
// Terminal 状态（accepted_initial、accepted_revised、degraded、overridden）
// 是"快照权威"：评审锁定的是草稿快照，而非当时的基础配置。因此 terminal 状态
// 下即使基础配置（风格目标、规则、锚点、批评者提示、大纲）已变更，commit 仍被
// 允许——无需、也不可能重新评审已终结的审查。
//
// C1：返工/重写队列不再跳过批评者门控——重写章节同样必须满足账本 terminal +
// digest 匹配（返工路径必须走 Critic 终验才能 commit）。返工章节经 review_style
// 开启新 epoch 完成终验后，其最新周期即满足本门控。
func CheckCommitStyleGate(st *store.Store, chapter int) error {
	// 1. 加载草稿并计算摘要（同时用于 checkpoint freshness 和账本校验）
	content, _, err := st.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return fmt.Errorf("load content for commit gate: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return fmt.Errorf("章节 %d 无草稿: %w", chapter, errs.ErrToolPrecondition)
	}
	currentDraftDigest := domain.DigestDraft(content)

	// 2. 通用 checkpoint freshness 校验（在 mode 检查之前）
	//    若存在 consistency_check checkpoint，其 digest 必须有效且匹配当前草稿。
	consistencyCP := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "consistency_check")
	if consistencyCP != nil {
		if !domain.IsValidDigest(consistencyCP.Digest) || consistencyCP.Digest != currentDraftDigest {
			return fmt.Errorf("章节 %d 的草稿已变更或一致性检查点摘要无效，请重新运行 check_consistency: %w",
				chapter, errs.ErrToolPrecondition)
		}
	}
	// 无 checkpoint：off 模式允许（step 3），critic 模式会继续到账本校验。

	// 3. 模式检查：off 模式不拦截（若存在 checkpoint 已在上方校验）
	meta, err := st.RunMeta.Load()
	if err != nil {
		return fmt.Errorf("load run meta: %w: %w", errs.ErrStoreRead, err)
	}
	if meta == nil || meta.StyleReviewMode != domain.StyleQualityCritic {
		return nil
	}

	// 4. 加载账本（重写队列同样必须满足账本校验，无 bypass）
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
		// 双摘要兼容：同时接受 canonical 与 legacy（旧字段排列）摘要——升级前
		// 落盘的 basis_digest 按旧 wire 顺序计算，若只按 canonical 比对会误判
		// basis 已变更而错误拒绝 commit。
		currentBasis := buildCriticBasis(st, chapter, criticVersion)
		if !domain.BasisDigestMatches(currentBasis, currCycle.BasisDigest, domain.DigestReviewBasis(currentBasis)) {
			return fmt.Errorf("critic 模式：章节 %d 的评审基础已变更（风格目标/规则/锚点/大纲），需要重新评审: %w",
				chapter, errs.ErrToolPrecondition)
		}
	}

	return nil
}

// CheckLiteraryProseGate 文学腔句式硬闸（commit 级打回）。
//
// 对将要提交的新正文统计 8 类文学腔句式 + 4 类形态检查（见
// rules.CheckLiteraryProse：否定修正句/抽象情绪概括/明喻滥用/升华收尾/伪停顿/
// 物化句式/抽象主语+状态动词/破折号伪深刻，以及段落碎片化单句段超标、
// 自评口吻不足、节拍账本、禁词清零），任一触发 → error 级违例，直接阻止
// commit。纯正则机械判定，不经 LLM judge。
//
// 行为：
//   - 命中句（原文片段 ≤40 字）随违例写入 meta/rule_violations.jsonl 供审计——
//     即使 commit 被拦下也落盘，editor/writer 返工时据此定位修改。
//   - 不改变 fatigue_words / forbidden_phrases 等既有机制（唯一交集 —— 破折号
//     由 rules.CheckLiteraryGate 做取代处理，见其注释）。
//   - 调用方负责只在"真正提交新正文"的路径上调用（已完成章节的重复提交跳过）。
func CheckLiteraryProseGate(st *store.Store, chapter int) error {
	content, _, err := st.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return fmt.Errorf("load content for literary gate: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return fmt.Errorf("章节 %d 无草稿: %w", chapter, errs.ErrToolPrecondition)
	}
	violations := rules.CheckLiteraryProse(content)
	if len(violations) == 0 {
		return nil
	}
	// 审计落盘：被拦下的命中句也要进 rule_violations（best-effort，不叠加失败原因）
	if err := st.World.SaveRuleViolations(chapter, violations); err != nil {
		slog.Warn("文学腔硬闸违规落盘失败", "module", "tools", "chapter", chapter, "err", err)
	}
	parts := make([]string, 0, len(violations))
	for _, v := range violations {
		parts = append(parts, fmt.Sprintf("%s（命中 %v 处，阈值 %v）", v.Target, v.Actual, v.Limit))
	}
	return fmt.Errorf("文学腔句式硬闸拦截：%s。请修改正文后重新 check_consistency，再 commit_chapter: %w",
		strings.Join(parts, "；"), errs.ErrToolPrecondition)
}

// polishCheckpointMatches 返回章节是否存在 digest 与当前草稿匹配的 polish checkpoint。
// 供 commit gate / review_style / check_consistency 共用同一"fresh polish"判定。
// degraded polish checkpoint（精修失败降级记录，Digest=当前草稿）同样匹配——
// degraded 后允许 review（review_style 前置检查的 degraded 识别即由此实现）。
func polishCheckpointMatches(st *store.Store, chapter int, currentDraftDigest string) bool {
	cp := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "polish")
	return cp != nil && domain.IsValidDigest(cp.Digest) && cp.Digest == currentDraftDigest
}

// CheckPolishPipelineGate 校验精修流水线的 commit 前置条件（仅 pipeline 启用时由
// commit_chapter 调用；pipeline 关闭时不生效，旧项目行为不变）。
//
// 要求（顺序执行）：
//  1. 存在当前草稿 digest 对应的 polish checkpoint（fresh 校验：草稿在精修后又被改
//     动 → 拒绝，要求重新 polish_draft；"若 Critic revise 后正文又被改，要求新的
//     polish checkpoint"即由此覆盖）。
//  2. checkpoint 记录的 polisher_model 与配置的 roles.polisher 当前模型一致
//     （expectedPolisherModel 为空 = 未显式配置 polisher 角色，跳过模型一致性校验；
//     配置了模型但 checkpoint 未记录模型 → 拒绝，fail-closed）。
//     2b. stage 场景一致性（pipeline 自身契约，与模式无关）：重写队列章节期望
//     stage=rewrite，其余章节期望 stage=draft。
//  3. seq 绑定（仅 critic 模式；off 模式跳过整个绑定段——D2：mode=off 时 rewrite
//     走旧规则，历史 critic 账本不得拒绝 off 模式的提交）：以账本当前 terminal
//     条目为权威（C1-C1，不用 lastStyleReviewResultEntry——degraded/overridden 无
//     Result 会错误回退到旧 epoch 结果），其 Request.PolishCheckpointSeq（R）必须
//     与最新 polish checkpoint seq（P）严格相等，并通过 BySeq(R) 复核 bound
//     checkpoint 的完整身份（存在/scope/step/digest/stage/model）。legacy（R=0）
//     回退 OccurredAt 比较（RFC3339 丢失小数秒，容忍同秒）。
//     无账本（nil）时跳过绑定段（不 panic）。C1：返工/重写队列章节同样适用
//     （无 bypass）——返工路径必须经新 epoch 的 critic 终验后才允许 commit。
//
// no-op 允许：changed=false 的 checkpoint 同样满足 fresh 校验（防模型为改而改）。
//
// degraded 允许：Degraded=true 的 checkpoint（polisher 失败降级记录，正文未变）同样
// 满足 fresh 校验；其 polisher 模型检查被跳过（未调用模型，模型字段仅审计用），
// 但 digest/stage/seq 绑定校验原样执行——degraded 后 review 绑定的正是该记录，
// R == P 自然成立。这使"polish 失败 → 降级 → check → review → commit"成为
// 可接受终态，消除永远 needs_polish 的死锁。
func CheckPolishPipelineGate(st *store.Store, chapter int, expectedPolisherModel string) error {
	content, _, err := st.Drafts.LoadChapterContent(chapter)
	if err != nil {
		return fmt.Errorf("load content for polish gate: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return fmt.Errorf("章节 %d 无草稿: %w", chapter, errs.ErrToolPrecondition)
	}
	currentDraftDigest := domain.DigestDraft(content)

	// 1. fresh polish checkpoint
	polishCP := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "polish")
	if polishCP == nil {
		return fmt.Errorf("pipeline：章节 %d 缺少 polish checkpoint，请先调用 polish_draft: %w",
			chapter, errs.ErrToolPrecondition)
	}
	if !domain.IsValidDigest(polishCP.Digest) || polishCP.Digest != currentDraftDigest {
		return fmt.Errorf("pipeline：章节 %d 的草稿已变更或 polish checkpoint 摘要无效，请重新调用 polish_draft: %w",
			chapter, errs.ErrToolPrecondition)
	}

	// 2. polisher model 一致性（显式配置 roles.polisher 时）。checkpoint 未记录
	//    polisher 模型（空）同样拒绝——配置了模型就必须能对上，fail-closed。
	//    degraded checkpoint（polisher 调用失败降级，未调用模型）跳过模型一致性
	//    检查：其模型字段仅审计用（可能为 unknown/空），不代表配置漂移。
	if expectedPolisherModel != "" && !polishCP.Degraded && polishCP.PolisherModel != expectedPolisherModel {
		if polishCP.PolisherModel == "" {
			return fmt.Errorf("pipeline：章节 %d 的 polish checkpoint 未记录 polisher 模型，与当前配置的 polisher 模型 %s 不一致，请重新调用 polish_draft: %w",
				chapter, expectedPolisherModel, errs.ErrToolPrecondition)
		}
		return fmt.Errorf("pipeline：章节 %d 的 polish 由模型 %s 完成，与当前配置的 polisher 模型 %s 不一致，请重新调用 polish_draft: %w",
			chapter, polishCP.PolisherModel, expectedPolisherModel, errs.ErrToolPrecondition)
	}

	// 2b. stage 场景一致性（pipeline 自身契约，与模式无关）：重写/打磨队列章节的
	//     polish 必须 stage=rewrite，其余章节必须 stage=draft。
	inRewriteQueue := isCompletedAndInRewriteQueue(st, chapter)
	if inRewriteQueue && polishCP.Stage != "rewrite" {
		return fmt.Errorf("pipeline：章节 %d 处于重写队列但 polish stage=%q（期望 rewrite），请重新执行 polish_draft: %w",
			chapter, polishCP.Stage, errs.ErrToolPrecondition)
	}
	if !inRewriteQueue && polishCP.Stage != "draft" {
		return fmt.Errorf("pipeline：章节 %d 不在重写队列但 polish stage=%q（期望 draft），请重新执行 polish_draft: %w",
			chapter, polishCP.Stage, errs.ErrToolPrecondition)
	}

	// 3. seq 绑定（权威 entry = ledger.CurrentCycle()，C1-C1）：仅 critic 模式执行。
	//    off 模式（D2：rewrite 走旧规则，不套用 critic 语义）跳过整个绑定段——
	//    pipeline 自身契约（fresh polish / stage / model）已在上方校验，绝不因
	//    历史账本拒绝 off 模式的提交。所有 ledger 访问均带 nil 防护（无账本 →
	//    跳过绑定段，不 panic）。
	//    绑定语义：当前 terminal 条目（accepted/revised/overridden/degraded）绑定的
	//    PolishCheckpointSeq（R）必须与最新 polish checkpoint seq（P）严格相等
	//    （C1-H1），并通过 BySeq(R) 复核 bound checkpoint 的完整身份
	//    （存在/scope/step/digest/stage/model）。legacy（R=0）回退 OccurredAt 比较
	//    （RFC3339 丢失小数秒，容忍同秒）。
	//    注意：P == R 时 BySeq(R) 即本章最新 polish checkpoint（scope/step 天然匹配、
	//    digest 与 step 1 一致），其余校验（stage/model）是纵深防御——任何不一致都
	//    要求重新执行 polish_draft → check_consistency → review_style。
	meta, mErr := st.RunMeta.Load()
	if mErr != nil {
		return fmt.Errorf("load run meta for polish gate: %w: %w", errs.ErrStoreRead, mErr)
	}
	if meta != nil && meta.StyleReviewMode == domain.StyleQualityCritic {
		ledger, lErr := st.StyleReview.Load(chapter)
		if lErr != nil {
			return fmt.Errorf("load style review ledger for polish gate: %w: %w", errs.ErrStoreRead, lErr)
		}
		if ledger != nil {
			if curr := ledger.CurrentCycle(); curr != nil {
				var boundSeq int64
				if curr.Request != nil {
					boundSeq = curr.Request.PolishCheckpointSeq
				}
				if boundSeq > 0 {
					// P == R 严格相等：评审必须基于当前 polish candidate。
					if polishCP.Seq != boundSeq {
						return fmt.Errorf("pipeline：章节 %d 的评审绑定 polish checkpoint seq %d，当前 polish seq %d 不一致（评审对象不是当前精修版本），请重新执行 polish_draft → check_consistency → review_style: %w",
							chapter, boundSeq, polishCP.Seq, errs.ErrToolPrecondition)
					}
					// 复核 bound checkpoint 的完整身份。
					bound := st.Checkpoints.BySeq(boundSeq)
					if bound == nil {
						return fmt.Errorf("pipeline：章节 %d 绑定的 polish checkpoint seq %d 不存在，请重新执行 polish_draft → check_consistency → review_style: %w",
							chapter, boundSeq, errs.ErrToolPrecondition)
					}
					if !bound.Scope.Matches(domain.ChapterScope(chapter)) || bound.Step != "polish" {
						return fmt.Errorf("pipeline：章节 %d 绑定的 checkpoint seq %d 不是本章的 polish 检查点（scope=%s step=%s），请重新执行 polish_draft → check_consistency → review_style: %w",
							chapter, boundSeq, bound.Scope, bound.Step, errs.ErrToolPrecondition)
					}
					if bound.Digest != currentDraftDigest {
						return fmt.Errorf("pipeline：章节 %d 绑定的 polish checkpoint seq %d 摘要与当前草稿不匹配，请重新执行 polish_draft → check_consistency → review_style: %w",
							chapter, boundSeq, errs.ErrToolPrecondition)
					}
					if expectedPolisherModel != "" && !bound.Degraded && bound.PolisherModel != expectedPolisherModel {
						if bound.PolisherModel == "" {
							return fmt.Errorf("pipeline：章节 %d 绑定的 polish checkpoint seq %d 未记录 polisher 模型，与当前配置的 polisher 模型 %s 不一致，请重新调用 polish_draft: %w",
								chapter, boundSeq, expectedPolisherModel, errs.ErrToolPrecondition)
						}
						return fmt.Errorf("pipeline：章节 %d 的 polish 由模型 %s 完成，与当前配置的 polisher 模型 %s 不一致，请重新调用 polish_draft: %w",
							chapter, bound.PolisherModel, expectedPolisherModel, errs.ErrToolPrecondition)
					}
				} else if !reviewBindsPolish(curr, polishCP) {
					// legacy（R=0）：回退 wall-clock 比较（与 FSM 共用
					// reviewBindsPolish，规格 §4）。criticAt 来自 RFC3339
					// （整秒精度，丢失小数秒），而 polishCP.OccurredAt 保留小数秒
					// ——同秒内 Critic 实际晚于 polish 也可能被判更早，故容忍 1 秒
					// 窗口：critic 比 polish 早不超过 1 秒视为合法（同秒 + 边界）。
					// 时间缺失/解析失败按已绑定放行（legacy 账本兼容，避免旧账本死锁）。
					return fmt.Errorf("pipeline：章节 %d 的评审先于 polish 完成（评审对象不是精修后的正文），请重新执行 polish_draft → check_consistency → review_style: %w",
						chapter, errs.ErrToolPrecondition)
				}
			}
		}
	}

	return nil
}
