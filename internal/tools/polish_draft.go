package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// ── Polisher output bounds ───────────────────────────────────────────
//
// maxPolishOutputRunes 是 polisher 输出的防御性上限：打磨产物是整章正文，
// 正常章节约数千至数万 runes；超出上限视为异常输出拒绝落盘（防模型幻觉
// 输出"整本书"或重复拼接）。
const maxPolishOutputRunes = 200000

// maxFullTextOutputRatio 是 full-text 回退路径的输出/输入长度比例上限（P0-5）：
// 精修应基本保持篇幅，输出超过输入 2 倍视为异常膨胀拒绝落盘。full-text 是整章
// 重输出（覆盖≈100% 是其协议本质，无法套用 edit list 的 old_string 覆盖上限），
// 故用长度比例作为覆盖政策的等价约束（40% 保底 + 本上限），见 applyFullTextPolish。
const maxFullTextOutputRatio = 2

// ── Polisher empty-output retry ──────────────────────────────────────
//
// 与 critic 空输出重试同构：空输出（空串/仅空白）是瞬态故障，指数退避重试；
// runner 错误与非空输出（即使校验失败）立即返回。包级变量便于测试收缩退避。
var (
	// polisherEmptyRetryMax 是 polisher 空输出时的最大总尝试次数（1 次初始调用 + 3 次重试）。
	polisherEmptyRetryMax = 4
	// polisherEmptyRetryBase 是空输出重试的指数退避基数：2s → 4s → 8s。
	polisherEmptyRetryBase = 2 * time.Second
)

// PolishDraftTool 是状态化文风精修工具：在 Writer 单次 Run 内嵌套调用独立
// Polisher Runner（roles.polisher），对当前草稿做文风/节奏/色气精修。
// 非 ReadOnly，非 ConcurrencySafe。Writer 可调用。
//
// 职责边界：本工具只负责"调 polisher → 校验 → 保存草稿 → 写 polish checkpoint →
// 返回摘要"。评审（review_style）与提交（commit_chapter）由调用方在精修完成后执行，
// 保证 checkpoint 先于 critic pass 落盘（commit gate 的时序校验依赖此顺序）。
type PolishDraftTool struct {
	store              *store.Store
	polisherRunner     *subagent.Runner
	polisherPromptHash string // sha256 前缀：实际精修者提示词内容的可溯源标识
	enabled            bool   // chapter pipeline 开关；关闭时工具返回 skipped（旧项目行为不变）
	// fsmConfig 是章节流水线强制状态机配置（BuildWorkers 注入）；Enabled 时
	// Execute 入口调用 RequireChapterAction 强制顺序（needs_polish 才允许精修，
	// 保证非法 polish 不消耗模型调用）。
	fsmConfig ChapterFSMConfig
	// mechRejectStreak 记录同章连续"机械回归拒绝"（edit 路径与 full-text 路径
	// 候选引入新的 error 级机械违规，P0-5 统一门禁）的次数。内存态、进程生命周期
	// 内有效（Writer 单次 Run 内连续重派同一章即命中）；每次成功精修或收敛落盘后
	// 清零。连续 2 次 → 写 rejected 性质 polish checkpoint（复用 Degraded+
	// ErrorCategory，正文未变）→ FSM 收敛到 post-check，防"精修反复引入机械违规 →
	// 永久 fail-closed 死循环"（循环 B）。
	mechRejectStreak map[int]int
}

func NewPolishDraftTool(s *store.Store, polisherRunner *subagent.Runner, polisherPromptHash string) *PolishDraftTool {
	return &PolishDraftTool{
		store:              s,
		polisherRunner:     polisherRunner,
		polisherPromptHash: polisherPromptHash,
		mechRejectStreak:   make(map[int]int),
	}
}

// SetEnabled 设置 chapter pipeline 开关。由 BuildWorkers 按配置注入。
func (t *PolishDraftTool) SetEnabled(v bool) {
	t.enabled = v
	t.fsmConfig.PipelineEnabled = v
}

// SetChapterFSMConfig 注入章节流水线强制状态机配置（BuildWorkers 调用）。
func (t *PolishDraftTool) SetChapterFSMConfig(cfg ChapterFSMConfig) { t.fsmConfig = cfg }

// FSMConfig 返回注入的章节流水线配置（构建/测试诊断用）。
func (t *PolishDraftTool) FSMConfig() ChapterFSMConfig { return t.fsmConfig }

// SetPolisherRunner 设置嵌套调用的 polisher runner 与提示词版本标识。
// BuildWorkers 在完成 polisher 子代理装配后调用（runner 构造前工具实例已注入 writer 工具集）。
func (t *PolishDraftTool) SetPolisherRunner(runner *subagent.Runner, promptHash string) {
	t.polisherRunner = runner
	t.polisherPromptHash = promptHash
}

func (t *PolishDraftTool) Name() string { return "polish_draft" }
func (t *PolishDraftTool) Description() string {
	return "对章节草稿做文风精修（独立 polisher 模型）：保留章节事实与契约，只修文风/节奏/色气/已给评审意见。" +
		"返回前后摘要与改动标记；返回 skipped=true 表示当前项目未启用精修流水线。writer 可调用。"
}
func (t *PolishDraftTool) Label() string { return "文风精修" }

func (t *PolishDraftTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *PolishDraftTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *PolishDraftTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
	)
}

// PolishDraftOutput 是 polish_draft 的返回结构（摘要，不回传全文）。
type PolishDraftOutput struct {
	Chapter       int    `json:"chapter"`
	Polished      bool   `json:"polished"`
	Changed       bool   `json:"changed"`
	InputDigest   string `json:"input_digest"`
	OutputDigest  string `json:"output_digest"`
	PolisherModel string `json:"polisher_model"`
	Stage         string `json:"stage"`
	WordCount     int    `json:"word_count"`
	Skipped       bool   `json:"skipped,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Error         string `json:"error,omitempty"`
	// NextStep 明确精修后的强制下一步（FSM：polish → check_consistency → review）。
	NextStep string `json:"next_step,omitempty"`
	// Degraded=true 表示精修失败已降级记录（正文未变）：polisher 经有限重试仍失败
	// （可恢复类错误），写入了绑定当前草稿 digest 的 degraded polish checkpoint。
	// 调用方应继续执行 post-polish check → review，而不是重试 polish。
	Degraded bool `json:"degraded,omitempty"`
	// ErrorCategory 是降级原因的稳定分类（stream_idle/max_turns/timeout/network/
	// rate_limit/overloaded），审计用。
	ErrorCategory string `json:"error_category,omitempty"`
	// 以下审计字段是 edit_list 路径部分接受/归一化匹配的摘要（ora-1 ④）：只含计数
	// 与原因分类，绝不含正文/old_string/new_string 内容。仅审计用。
	ProposedEditCount    int      `json:"proposed_edit_count,omitempty"`
	DroppedEditCount     int      `json:"dropped_edit_count,omitempty"`
	DropReasons          []string `json:"drop_reasons,omitempty"`
	NormalizedMatchCount int      `json:"normalized_match_count,omitempty"`
	Partial              bool     `json:"partial,omitempty"`
	MatchModes           []string `json:"match_modes,omitempty"`
}

func (t *PolishDraftTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
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

	// ── 1. pipeline 开关：未启用 → skipped（旧项目行为不变） ──
	if !t.enabled {
		return json.Marshal(PolishDraftOutput{
			Chapter: a.Chapter,
			Skipped: true,
			Reason:  "chapter pipeline not enabled",
		})
	}

	// ── 2. 章节流水线强制状态机（Enabled 时）：needs_polish 才允许精修；
	//    在加载草稿/启动 polisher 之前拦截，保证非法 polish 不消耗模型调用。 ──
	if err := RequireChapterAction(t.store, a.Chapter, ChapterActionPolish, t.fsmConfig); err != nil {
		return nil, fmt.Errorf("polish_draft: %w", err)
	}

	// ── 3. 复核阻塞项 5：原子捕获 polish 输入基线（草稿快照 + digest + 账本状态
	//    + polish seq 绑定 + mutation 许可判定，统一锁序内一次读取）──
	// 基线捕获即拒绝不可修改的账本（terminal/pending/exhausted 锁定；重写队列
	// 豁免与 CheckStyleReviewMutationGuard 同语义）——guard 判定与捕获之间无
	// 间隙，杜绝"baseline 捕获时 ledger 已 terminal 仍被接受"的窗口。
	baseline, err := t.store.CapturePolishBaseline(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if baseline.Content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	if !baseline.MutationAllowed {
		return nil, fmt.Errorf("polish_draft: %s: %w", baseline.MutationBlockedReason, errs.ErrToolPrecondition)
	}
	content := baseline.Content
	wordCount := utf8.RuneCountInString(content)
	inputDigest := baseline.InputDigest

	// ── 4. 构建任务 payload（草稿 + 评审依据 + 已给意见 + 重写 brief）并调用 polisher ──
	taskText := t.buildPolishTask(a.Chapter, content, wordCount, "")

	// 基线观测（§14 第 2 步）：polisher 调用前后记录耗时与进程 CPU 时间差。
	// 观测只写 slog 日志，失败绝不影响主流程（见 logPolisherRunObs）。
	obs := &polisherRunObs{}
	obsStart := time.Now()
	cpuStart, cpuOK := processCPUTime()
	outputText, err := t.runPolisherWithEmptyRetry(ctx, a.Chapter, taskText, obs)
	obs.elapsed = time.Since(obsStart)
	if cpuOK {
		if cpuEnd, ok := processCPUTime(); ok {
			obs.cpuDelta, obs.cpuOK = cpuEnd-cpuStart, true
		}
	}
	if err != nil {
		// 可降级错误（stream idle / provider timeout / network 类 / MaxTurns）→
		// 写 degraded polish checkpoint 后返回成功摘要；不可降级原样返回。
		logPolisherRunObs(a.Chapter, wordCount, obs, nil, err)
		return t.handlePolisherFailure(a.Chapter, inputDigest, wordCount, baseline, err)
	}

	// ── 5. 输出解析与执行路径（ora-1 形态 2 + ④）： ──
	//    - 成功解析为 edit list → edit 路径：内存中基于同一输入快照逐条验证 +
	//      按优先级部分接受（单条无效只丢弃该条）→ 一次 SaveDraft → 一个最终
	//      polish checkpoint（Method=edit_list、EditCount=实际应用数、审计字段）。
	//    - 解析失败（非 JSON/围栏/未知字段/纯正文）→ 回退现有整章模式（旧协议，
	//      渐进切换；整章模式现有校验/落盘/checkpoint 全保留）。
	//    - 契约错误（edit plan 形状但 version 不受支持等）→ fail-closed，草稿原样、
	//      不写 checkpoint、返回明确错误（不混入 Degraded=true）。
	plan, fallback, parseErr := ParsePolishEditPlan(outputText)
	if parseErr != nil {
		if !fallback {
			logPolisherRunObs(a.Chapter, wordCount, obs, nil, parseErr)
			return nil, fmt.Errorf("polisher 输出 edit plan 契约错误：%v: %w", parseErr, errs.ErrToolPrecondition)
		}
		summary, sErr := t.applyFullTextPolish(a.Chapter, content, wordCount, inputDigest, outputText, baseline)
		logPolisherRunObs(a.Chapter, wordCount, obs, summary, sErr)
		return summary, sErr
	}
	summary, sErr := t.applyEditPlan(a.Chapter, content, wordCount, inputDigest, plan, baseline)
	logPolisherRunObs(a.Chapter, wordCount, obs, summary, sErr)
	return summary, sErr
}

// applyFullTextPolish 是整章重输出回退路径（旧协议）：polisher 输出无法解析为
// edit plan（非 JSON/围栏/未知字段/纯正文）时进入。现有校验/落盘/checkpoint
// 全保留（渐进切换：旧协议模型仍可用），仅 checkpoint 增加 Method=full_text 审计字段。
func (t *PolishDraftTool) applyFullTextPolish(chapter int, content string, wordCount int, inputDigest, outputText string, baseline *store.PolishBaseline) (json.RawMessage, error) {
	// ── 校验：非空 / UTF-8 / 最短长度（防"好的，已完成精修"式短文本被当正文保存）/
	// 最大长度 / 非纯 JSON / 非代码围栏整体包裹 ──
	if strings.TrimSpace(outputText) == "" {
		return nil, fmt.Errorf("polisher returned empty output: %w", errs.ErrToolPrecondition)
	}
	if !utf8.ValidString(outputText) {
		return nil, fmt.Errorf("polisher 输出含非法 UTF-8 序列，拒绝落盘: %w", errs.ErrToolPrecondition)
	}
	inputRunes := utf8.RuneCountInString(content)
	outputRunes := utf8.RuneCountInString(outputText)
	// 最小输出：整章正文的合法输出不可能比输入草稿短太多；几十字的"已精修"式
	// 短文本视为非正文，拒绝落盘。
	if outputRunes < inputRunes*2/5 {
		return nil, fmt.Errorf("polisher 输出 %d runes 不足输入草稿（%d runes）的 40%%，疑似非正文（如“好的，已完成精修”式短文本），拒绝落盘: %w",
			outputRunes, inputRunes, errs.ErrToolPrecondition)
	}
	trimmed := strings.TrimSpace(outputText)
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		return nil, fmt.Errorf("polisher 输出疑似纯 JSON（非正文），拒绝落盘: %w", errs.ErrToolPrecondition)
	}
	if strings.HasPrefix(trimmed, "```") && strings.HasSuffix(trimmed, "```") {
		return nil, fmt.Errorf("polisher 输出被代码围栏整体包裹，拒绝落盘: %w", errs.ErrToolPrecondition)
	}

	// P0-5 覆盖比例上限（新增）：full-text 回退是整章重输出，无法像 edit list 一样
	// 用"old_string 覆盖比例"表达改动面（整章替换≈100% 是其协议本质），因此用
	// 输出/输入长度比例做"覆盖政策的等价物"：40% 保底（上方）+ 本上限检查。
	// 精修应基本保持篇幅（正常 ±30%），输出超过输入 2 倍视为异常膨胀（如模型
	// 幻觉重复拼接/整本书式输出，maxPolishOutputRunes 之外的更紧防线），拒绝落盘。
	// 这保证 full-text 不再"自动逃逸" edit-list 的覆盖政策（P0-5：遵守协议的反被拒、
	// 违反协议的反成功）——full-text 路径同样受"输出不得异常大于输入"约束。
	// 顺序说明：协议形态检查（JSON/围栏）先于长度比例检查，保证协议违规输出
	// 得到其专属错误信息，长度比例只拦截形态合法但篇幅异常膨胀的正文。
	if outputRunes > inputRunes*maxFullTextOutputRatio {
		return nil, fmt.Errorf("polisher 输出 %d runes 超过输入草稿（%d runes）的 %d 倍，异常膨胀，拒绝落盘: %w",
			outputRunes, inputRunes, maxFullTextOutputRatio, errs.ErrToolPrecondition)
	}
	if outputRunes > maxPolishOutputRunes {
		return nil, fmt.Errorf("polisher 输出 %d runes 超过上限 %d，拒绝落盘: %w",
			outputRunes, maxPolishOutputRunes, errs.ErrToolPrecondition)
	}

	// P0-5 机械回归门禁（与 edit 路径同一统一检查器 computeMechanicalViolations）：
	// 若 polisher 输入无 error 级机械违规，则整章输出也必须无——full-text 回退
	// 不得因"绕开 edit list 协议"而逃过机械底线（防"换协议 → 引入禁词/文学腔"）。
	// 拒绝走与 edit 路径相同的连续收敛（mechRejectStreak 共享，Method=full_text）。
	if !hasErrorViolations(computeMechanicalViolations(t.store, content, wordCount)) {
		if hasErrorViolations(computeMechanicalViolations(t.store, outputText, outputRunes)) {
			_, summary, mErr := t.mechanicalRegression(chapter, inputDigest, wordCount, "full_text", 0, baseline)
			if mErr != nil {
				return nil, mErr
			}
			return summary, nil
		}
	}

	// ── 7+8. P0-3：CAS 校验 + 原子落盘（保存草稿 → draft checkpoint → polish checkpoint）──
	// 模型调用期间若草稿/账本/polish checkpoint 被并发修改，候选被丢弃（草稿不
	// 被覆盖），返回明确错误让 writer 重新走流程。
	outputDigest := domain.DigestDraft(outputText)
	stage := polishStageForChapter(t.store, chapter)
	changed := outputDigest != inputDigest
	polisherModel := t.loadPolisherModelName()
	if _, err := t.store.CommitPolishCandidate(
		chapter, outputText, baseline,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       changed,
			Method:        "full_text",
		},
	); err != nil {
		if errors.Is(err, store.ErrPolishCandidateStale) {
			return nil, fmt.Errorf("polish candidate stale: draft changed during polish, discard and retry: %w: %w",
				errs.ErrToolPrecondition, store.ErrPolishCandidateStale)
		}
		return nil, fmt.Errorf("polish commit: %w: %w", errs.ErrStoreWrite, err)
	}
	// 成功推进：清零机械回归连续计数（本调用已产出合法结果）。
	t.mechRejectStreak[chapter] = 0

	// ── 9. 摘要返回（不回传全文） ──
	return json.Marshal(PolishDraftOutput{
		Chapter:       chapter,
		Polished:      true,
		Changed:       changed,
		InputDigest:   inputDigest,
		OutputDigest:  outputDigest,
		PolisherModel: polisherModel,
		Stage:         stage,
		WordCount:     utf8.RuneCountInString(outputText),
		NextStep:      "精修完成。下一步**必须**调用 check_consistency 重新核验；通过后按返回的 required_next_action 继续（review_style → terminal → commit_chapter）",
	})
}

// applyEditPlan 是 edit list 路径主体（ora-1 ④）：逐条局部验证 + 按优先级部分接受
// （ApplyPolishEditPlanDetailed）→ 机械回归子流程（责任 edit 剔除，安全子集部分
// 接受）→ 一次 SaveDraft → draft checkpoint → 一个最终 polish checkpoint
// （Method=edit_list、EditCount=实际应用数、Partial/DropReasons/NormalizedMatchCount
// 等审计字段）。
//
// 失败收敛（④ 取代 P0-4 模型纠错：不再触发第二次模型调用 recoverEditPlanValidation）：
//   - 原始 edits=[] → 合法 no-op（非 degraded）。
//   - 原始非空但全部被丢弃（无安全 edit）→ 写 rejected/degraded polish checkpoint
//     （category 按全部 drop reasons 判定：coverage_exceeded / edit_plan_invalid）→
//     FSM 收敛到 post-check → 返回成功摘要（Degraded=true）。
//   - 机械回归且无安全子集 → mechRejectStreak（首次 fail-closed，连续 2 次收敛为
//     rejected checkpoint）；有安全子集时不增加 streak。
func (t *PolishDraftTool) applyEditPlan(chapter int, content string, wordCount int, inputDigest string, plan *PolishEditPlan, baseline *store.PolishBaseline) (json.RawMessage, error) {
	res, err := ApplyPolishEditPlanDetailed(content, plan, polishCoverageLimitForChapter(t.store, chapter))
	if err != nil {
		// 全无效（原始非空且 0 应用）：degraded 收敛，不再第二次调用 polisher。
		return t.handleEditPlanRejected(chapter, inputDigest, wordCount, res, baseline)
	}
	res2, summary, mErr := t.salvageMechanical(chapter, content, wordCount, inputDigest, res, baseline)
	if mErr != nil {
		return nil, mErr
	}
	if summary != nil {
		return summary, nil
	}
	return t.applyEditPlanCandidate(chapter, wordCount, inputDigest, res2, baseline)
}

// applyEditPlanCandidate 是 edit 候选通过内容校验与机械门禁后的公共收尾：
// 一次 SaveDraft + draft checkpoint + 一个 polish checkpoint（Method=edit_list、
// EditCount=实际应用数、ProposedEditCount/DroppedEditCount/DropReasons/
// NormalizedMatchCount/Partial/MatchModes 审计）→ 成功摘要（不回传全文）。
func (t *PolishDraftTool) applyEditPlanCandidate(chapter int, wordCount int, inputDigest string, res *PolishEditPlanResult, baseline *store.PolishBaseline) (json.RawMessage, error) {
	// ── P0-3：CAS 校验 + 原子落盘（一次 SaveDraft + draft checkpoint + 一个 polish
	//    checkpoint）。模型调用期间草稿/账本/polish checkpoint 被并发修改时候选被
	//    丢弃（草稿不被覆盖），返回明确错误让 writer 重新走流程。 ──
	candidate := res.Candidate
	outputDigest := domain.DigestDraft(candidate)
	stage := polishStageForChapter(t.store, chapter)
	changed := outputDigest != inputDigest
	polisherModel := t.loadPolisherModelName()
	audit := auditFromResult(res)
	if _, err := t.store.CommitPolishCandidate(
		chapter, candidate, baseline,
		domain.PolishCheckpointMeta{
			InputDigest:          inputDigest,
			PolisherModel:        polisherModel,
			Stage:                stage,
			Changed:              changed,
			Method:               "edit_list",
			EditCount:            len(res.Applied),
			ProposedEditCount:    audit.ProposedEditCount,
			DroppedEditCount:     audit.DroppedEditCount,
			DropReasons:          audit.DropReasons,
			NormalizedMatchCount: audit.NormalizedMatchCount,
			Partial:              audit.Partial,
			MatchModes:           audit.MatchModes,
		},
	); err != nil {
		if errors.Is(err, store.ErrPolishCandidateStale) {
			return nil, fmt.Errorf("polish candidate stale: draft changed during polish, discard and retry: %w: %w",
				errs.ErrToolPrecondition, store.ErrPolishCandidateStale)
		}
		return nil, fmt.Errorf("polish commit: %w: %w", errs.ErrStoreWrite, err)
	}
	// 成功推进：清零机械回归连续计数。
	t.mechRejectStreak[chapter] = 0

	// ── 摘要返回（不回传全文） ──
	return json.Marshal(PolishDraftOutput{
		Chapter:              chapter,
		Polished:             true,
		Changed:              changed,
		InputDigest:          inputDigest,
		OutputDigest:         outputDigest,
		PolisherModel:        polisherModel,
		Stage:                stage,
		WordCount:            utf8.RuneCountInString(candidate),
		ProposedEditCount:    audit.ProposedEditCount,
		DroppedEditCount:     audit.DroppedEditCount,
		DropReasons:          audit.DropReasons,
		NormalizedMatchCount: audit.NormalizedMatchCount,
		Partial:              audit.Partial,
		MatchModes:           audit.MatchModes,
		NextStep:             "精修完成。下一步**必须**调用 check_consistency 重新核验；通过后按返回的 required_next_action 继续（review_style → terminal → commit_chapter）",
	})
}

// PolishCheckpointAudit 是 edit_list 路径的审计字段集（部分接受/归一化匹配，只含
// 计数与原因分类，绝不含正文内容）。
type PolishCheckpointAudit struct {
	ProposedEditCount    int
	DroppedEditCount     int
	DropReasons          []string
	NormalizedMatchCount int
	Partial              bool
	MatchModes           []string
}

// auditFromResult 从部分接受结果导出 checkpoint/摘要审计字段。
func auditFromResult(res *PolishEditPlanResult) PolishCheckpointAudit {
	return PolishCheckpointAudit{
		ProposedEditCount:    res.ProposedEditCount,
		DroppedEditCount:     len(res.Dropped),
		DropReasons:          res.DropReasons(),
		NormalizedMatchCount: res.NormalizedMatchCount,
		Partial:              res.Partial,
		MatchModes:           res.AppliedMatchModes(),
	}
}

// salvageMechanical 是 edit 路径的机械回归子流程（ora-1 ④ 第 6 条）：
//
//   - 输入无 error 级机械违规时门禁激活（与 P0-5 同一检查器）；候选无新违规 →
//     直接返回原结果。
//   - 候选引入新违规 → 逐条单独应用，找出责任 edit（单独应用即引入违规的 edit）
//     → 剔除责任 edit、基于同一输入快照重建候选。
//   - 剔除后存在安全子集（含空子集）→ 部分接受成功（不增加 mechRejectStreak，
//     审计记录 mechanical drop reasons）。
//   - 仍无法安全（违规来自组合效应，无法定位责任 edit / 剔除后仍违规）→ 走
//     mechRejectStreak：首次 fail-closed（返回错误），连续 2 次收敛为 rejected
//     checkpoint（返回成功摘要，不消耗 writer turns）。
//
// 返回三元组：res2（安全子集结果）、summary（非 nil = 已写 rejected checkpoint
// 并返回成功摘要）、err（fail-closed 错误）。
func (t *PolishDraftTool) salvageMechanical(chapter int, content string, wordCount int, inputDigest string, res *PolishEditPlanResult, baseline *store.PolishBaseline) (*PolishEditPlanResult, json.RawMessage, error) {
	// 门禁前提：polisher 输入无 error 级机械违规（精修不得引入新的机械违规）。
	if hasErrorViolations(computeMechanicalViolations(t.store, content, wordCount)) {
		return res, nil, nil
	}
	if !hasErrorViolations(computeMechanicalViolations(t.store, res.Candidate, utf8.RuneCountInString(res.Candidate))) {
		return res, nil, nil
	}

	// 逐条单独应用，找出责任 edit。
	responsible := map[int]bool{}
	for _, a := range res.Applied {
		solo := ApplySinglePolishEdit(content, a)
		if hasErrorViolations(computeMechanicalViolations(t.store, solo, utf8.RuneCountInString(solo))) {
			responsible[a.Idx] = true
		}
	}
	if len(responsible) > 0 {
		res.DropApplied(content, responsible)
		if !hasErrorViolations(computeMechanicalViolations(t.store, res.Candidate, utf8.RuneCountInString(res.Candidate))) {
			// 安全子集：部分接受成功（不增加 mechRejectStreak）。
			slog.Warn("polisher edit 候选机械回归：已剔除责任 edit，部分接受安全子集",
				"module", "tools", "chapter", chapter, "responsible", len(responsible),
				"applied", len(res.Applied), "dropped", len(res.Dropped))
			return res, nil, nil
		}
	}
	// 仍无法安全（组合违规/剔除后仍违规）→ mechRejectStreak 收敛。
	return t.mechanicalRegression(chapter, inputDigest, wordCount, "edit_list", res.ProposedEditCount, baseline)
}

// handleMechanicalRegression 处理机械回归拒绝（循环 B 收敛）。edit 路径与
// full-text 路径共用（P0-5：full-text 候选同样受机械门禁约束，method 参数区分
// 审计记录）：
//
//   - 第一次拒绝：fail-closed——草稿原样、不写 polish checkpoint、返回明确错误
//     （区分"机械回归"类别，不混入 Degraded=true），模型可重试。
//   - 连续第 2 次拒绝（同章，内存计数，两路径共享）：写 rejected 性质的 polish
//     checkpoint（复用 Degraded=true + ErrorCategory="mechanical_regression"，
//     Digest=当前草稿、Changed=false）→ FSM 视其为合法 polish 记录（degraded 语义，
//     与 provider 降级同路径）→ 收敛到 post-check → check → review，防"精修反复
//     引入机械违规 → 永久 fail-closed 死循环"。返回成功摘要（Degraded=true +
//     ErrorCategory=mechanical_regression，调用方继续 post-polish check）。
func (t *PolishDraftTool) mechanicalRegression(chapter int, inputDigest string, wordCount int, method string, proposedEditCount int, baseline *store.PolishBaseline) (*PolishEditPlanResult, json.RawMessage, error) {
	t.mechRejectStreak[chapter]++
	if t.mechRejectStreak[chapter] < 2 {
		return nil, nil, fmt.Errorf("polisher 候选机械回归：候选引入新的 error 级机械违规，拒绝落盘（草稿未变）；连续 2 次将自动收敛为 rejected checkpoint: %w",
			errs.ErrToolPrecondition)
	}
	t.mechRejectStreak[chapter] = 0

	audit := PolishCheckpointAudit{
		ProposedEditCount: proposedEditCount,
		DroppedEditCount:  proposedEditCount,
		DropReasons:       []string{string(PolishEditDropMechanical)},
	}
	reason := fmt.Sprintf("polisher 候选连续 2 次引入 error 级机械违规，已写入 rejected polish checkpoint（正文未变，digest=%s）",
		inputDigest)
	summary, err := t.writeDegradedPolishCheckpoint(chapter, inputDigest, wordCount, "mechanical_regression", method, audit, reason, baseline)
	if err != nil {
		return nil, nil, err
	}
	return nil, summary, nil
}

// handleEditPlanRejected 处理"原始非空但全部 edit 被丢弃"的收敛（ora-1 ④）：
// 不再调用 recoverEditPlanValidation（不再触发第二次模型纠错——④ 取代 P0-4）。
// category 按全部 drop reasons 判定：含 coverage_limit → coverage_exceeded，
// 否则 → edit_plan_invalid。写审计明确的 rejected/degraded polish checkpoint
// （Digest=当前草稿、Changed=false、Method=edit_list、EditCount=0 实际应用数、
// ProposedEditCount/DroppedEditCount/DropReasons 审计）→ FSM 收敛到 post-check →
// check → review → commit。返回成功摘要（Degraded=true），不再消耗 writer turns。
func (t *PolishDraftTool) handleEditPlanRejected(chapter int, inputDigest string, wordCount int, res *PolishEditPlanResult, baseline *store.PolishBaseline) (json.RawMessage, error) {
	category := "edit_plan_invalid"
	for _, d := range res.Dropped {
		if d.DropReason == PolishEditDropCoverageLimit {
			category = "coverage_exceeded"
			break
		}
	}
	reasons := res.DropReasons()
	audit := PolishCheckpointAudit{
		ProposedEditCount: res.ProposedEditCount,
		DroppedEditCount:  len(res.Dropped),
		DropReasons:       reasons,
	}
	reason := fmt.Sprintf("polisher edit plan 全部 %d 条 edit 均被丢弃（%s），已写入 rejected/degraded polish checkpoint（正文未变，digest=%s）",
		len(res.Dropped), category, inputDigest)
	slog.Warn("polisher edit plan 全部 edit 均被丢弃，已写入 rejected/degraded polish checkpoint",
		"module", "tools", "chapter", chapter, "category", category, "digest", inputDigest, "drop_reasons", reasons)
	return t.writeDegradedPolishCheckpoint(chapter, inputDigest, wordCount, category, "edit_list", audit, reason, baseline)
}

// writeDegradedPolishCheckpoint 写 rejected/degraded 性质 polish checkpoint
// （正文未变、Digest=当前草稿、Changed=false、Degraded=true）并返回成功摘要。
// provider 失败（handlePolisherFailure）、内容校验全无效（handleEditPlanRejected）、
// 机械回归连续拒绝（mechanicalRegression）的收敛共用此路径：FSM 将 degraded
// 记录视作合法 polish 记录 → 强制 post-polish check → review，防永久 fail-closed
// 死循环。checkpoint/store 写失败不可收敛：原样返回（账本未留痕，状态不变）。
//
// 复核阻塞项 6：写 checkpoint 走 store 层原子方法 CommitDegradedPolishCheckpoint
// （统一锁序内验证 digest + 账本基线 + polish 基线后追加）——替换旧的"读后再写"
// 预检，校验与追加之间无窗口；模型调用期间草稿已被并发修改（inputDigest 不再
// 是当前草稿 digest）时跳过写入、返回 stale 错误，绝不写绑定旧 digest 的陈旧
// checkpoint。
func (t *PolishDraftTool) writeDegradedPolishCheckpoint(chapter int, inputDigest string, wordCount int, category, method string, audit PolishCheckpointAudit, reason string, baseline *store.PolishBaseline) (json.RawMessage, error) {
	polisherModel := t.loadPolisherModelName()
	stage := polishStageForChapter(t.store, chapter)
	if _, aErr := t.store.CommitDegradedPolishCheckpoint(
		chapter, baseline,
		domain.PolishCheckpointMeta{
			InputDigest:          inputDigest,
			PolisherModel:        polisherModel,
			Stage:                stage,
			Changed:              false,
			Degraded:             true,
			ErrorCategory:        category,
			Method:               method,
			EditCount:            0, // EditCount=实际应用数：degraded 路径未应用任何 edit
			ProposedEditCount:    audit.ProposedEditCount,
			DroppedEditCount:     audit.DroppedEditCount,
			DropReasons:          audit.DropReasons,
			NormalizedMatchCount: audit.NormalizedMatchCount,
			Partial:              audit.Partial,
			MatchModes:           audit.MatchModes,
		},
	); aErr != nil {
		if errors.Is(aErr, store.ErrPolishCandidateStale) {
			return nil, fmt.Errorf("degraded polish candidate stale: draft changed during polish, discard and retry (不写陈旧绑定): %w: %w",
				errs.ErrToolPrecondition, store.ErrPolishCandidateStale)
		}
		return nil, fmt.Errorf("checkpoint polish (degraded): %w", aErr)
	}
	slog.Warn("已写入 rejected/degraded polish checkpoint", "module", "tools", "chapter", chapter,
		"category", category, "digest", inputDigest, "drop_reasons", audit.DropReasons)

	return json.Marshal(PolishDraftOutput{
		Chapter:              chapter,
		Polished:             true,
		Changed:              false,
		Degraded:             true,
		ErrorCategory:        category,
		InputDigest:          inputDigest,
		OutputDigest:         inputDigest,
		PolisherModel:        polisherModel,
		Stage:                stage,
		WordCount:            wordCount,
		ProposedEditCount:    audit.ProposedEditCount,
		DroppedEditCount:     audit.DroppedEditCount,
		DropReasons:          audit.DropReasons,
		NormalizedMatchCount: audit.NormalizedMatchCount,
		Partial:              audit.Partial,
		MatchModes:           audit.MatchModes,
		Reason:               reason,
		NextStep:             "精修被拒绝并已收敛记录（正文未变）。下一步**必须**调用 check_consistency 重新核验；通过后按返回的 required_next_action 继续（review_style → terminal → commit_chapter）",
	})
}

// polishCoverageLimitForChapter 按场景返回 edit list 覆盖上限（P1-6）：
// 重写/打磨队列章节（stage=rewrite）放宽到 70%（更大改动面是重写的协议内诉求）；
// 普通 draft 保持 50%。超过上限仍拒绝（要求显式整章 rewrite 路径，edit list
// 不隐式放开）。与 polishStageForChapter 共用同一 stage 判定，保证校验与
// checkpoint 审计一致。
func polishCoverageLimitForChapter(st *store.Store, chapter int) float64 {
	if polishStageForChapter(st, chapter) == "rewrite" {
		return maxPolishEditCoverageRatioRewrite
	}
	return maxPolishEditCoverageRatio
}

// buildPolishTask 构造发送给 polisher runner 的任务文本：
// 规范评审依据（风格目标/契约/指南针文风/锚点/用户规则/事实大纲）
// + 已给的 revise findings（完整六字段）+ 重写/打磨 brief（PendingRewrites 时）
// + 章节与草稿全文（动态内容放最后）。
// 与 review_style 的 basis 共用同一数据源（buildStyleBasis），保证精修与评审看到
// 同一份风格事实（style goal/contract/compass/anchors/structured/factual outline）；
// 用户规则按职责角色投影：polisher → writer 视图（default+writer），
// critic → editor 视图（default+writer+editor）。
//
// 布局按 ora-1 缓存优化阶段 2（Prompt Capsule 重排）：稳定书级内容
// （basis/findings/brief）在前，章节动态内容（章节号/字数/草稿全文）最后——
// 跨 spawn 的内容前缀缓存（DeepSeek 磁盘缓存按内容前缀匹配）命中稳定前缀，
// 只有尾部的草稿段需要重新计算。correction 参数保留（当前恒为空串：④ 已取消
// P0-4 纠错重试，不再注入纠错反馈段，任务文本与主路径完全一致）。
func (t *PolishDraftTool) buildPolishTask(chapter int, content string, wordCount int, correction string) string {
	basis := buildPolishBasis(t.store, chapter, t.polisherPromptHash)
	basisJSON, _ := json.Marshal(basis)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## 精修任务\n\n### 精修依据（风格目标/章节契约/指南针文风/锚点/用户规则/事实大纲）\n%s\n\n", basisJSON)

	// 已给的 revise findings：完整投影（status/verdict/draft_digest/findings 六字段）。
	if ledger, lErr := t.store.StyleReview.Load(chapter); lErr == nil && ledger != nil {
		if view := buildFullStyleReviewCriticView(ledger); view != nil {
			viewJSON, _ := json.Marshal(view)
			fmt.Fprintf(&sb, "### 既有评审意见（只按 findings 修改，不扩大范围）\n%s\n\n", viewJSON)
		}
	}

	// 重写/打磨任务背景（PendingRewrites 时）：与 novel_context 的 rewrite_brief 同源。
	if progress, pErr := t.store.Progress.Load(); pErr == nil && progress != nil &&
		slices.Contains(progress.PendingRewrites, chapter) {
		meta, mErr := t.store.RunMeta.Load()
		if mErr != nil {
			meta = nil
		}
		if brief := buildRewriteBrief(t.store, meta, chapter, nil); len(brief) > 0 {
			briefJSON, _ := json.Marshal(brief)
			fmt.Fprintf(&sb, "### 重写/打磨任务背景\n%s\n\n", briefJSON)
		}
	}

	// 章节动态内容放最后（稳定前缀缓存之后）：章节号/字数变化量小、token 占比低，
	// 草稿全文是每章唯一的大块动态内容，放在尾部让前缀缓存最大化复用。
	fmt.Fprintf(&sb, "### 章节与草稿（字数：%d）\n第 %d 章\n\n%s\n\n", wordCount, chapter, content)

	// correction 段保留（当前恒为空串：④ 已取消 P0-4 纠错重试，不再注入）。
	if correction != "" {
		fmt.Fprintf(&sb, "### 上次计划纠错反馈（必须遵守，不得重复原计划）\n%s\n\n", correction)
	}

	// P1-6 场景覆盖上限：随任务文本告知实际上限（普通精修 50%、重写 70%），
	// 让 polisher 在 rewrite 场景充分利用 70% 能力，而不是被静态 50% 文案限制。
	limit := polishCoverageLimitForChapter(t.store, chapter)
	fmt.Fprintf(&sb, "### 覆盖上限\n本任务所有 old_string 覆盖范围合计不得超过输入草稿的 %.0f%%。\n\n", limit*100)

	fmt.Fprintf(&sb, "请严格按精修者提示词（polisher）输出结构化精修 edit 列表 JSON（{\"version\":1,\"edits\":[{\"old_string\":\"原文唯一连续片段\",\"new_string\":\"精修后片段\"}]}）；无修改时输出 {\"version\":1,\"edits\":[]}。")
	return sb.String()
}

// runPolisherWithEmptyRetry 调用 polisher runner，仅在输出为空（空串/仅空白）时
// 自动重试，指数退避（2s/4s/8s），最多 polisherEmptyRetryMax 次。
// 只对"空输出"这种瞬态故障重试：runner 错误与非空输出（即使校验失败）立即返回。
// 返回合并后的输出文本（Output 优先，空时回退 TerminalResult）。
// obs 是基线观测累加器（§14 第 2 步）：每次尝试的 usage/空输出标记逐次累加，
// 并输出单次尝试观测日志；观测失败绝不影响主流程。
func (t *PolishDraftTool) runPolisherWithEmptyRetry(ctx context.Context, chapter int, taskText string, obs *polisherRunObs) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= polisherEmptyRetryMax; attempt++ {
		if attempt > 1 {
			delay := polisherEmptyRetryBase * time.Duration(1<<(attempt-2))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", fmt.Errorf("polisher retry aborted: %w: %w", ctx.Err(), lastErr)
			}
		}

		runResult, err := t.polisherRunner.Run(ctx, "polisher", taskText)
		if err != nil {
			// 基线观测：失败尝试同样消耗了 token（RunResult 在失败路径仍携带
			// 已消耗 usage），记录后原样返回，错误处理不变。
			obs.recordAttempt(runResult, false)
			logPolisherAttempt(chapter, attempt, runResult, false, err)
			return "", fmt.Errorf("polisher call failed: %w", err)
		}

		outputText := runResult.Output
		if outputText == "" && runResult.TerminalResult != nil {
			outputText = string(runResult.TerminalResult)
		}
		empty := strings.TrimSpace(outputText) == ""
		obs.recordAttempt(runResult, empty)
		logPolisherAttempt(chapter, attempt, runResult, empty, nil)
		if !empty {
			return outputText, nil
		}

		lastErr = fmt.Errorf("polisher returned empty output (attempt %d/%d)", attempt, polisherEmptyRetryMax)
		slog.Warn("polisher 返回空输出，准备重试", "module", "tools", "chapter", chapter,
			"attempt", attempt, "max", polisherEmptyRetryMax)
	}
	return "", fmt.Errorf("%w；连续 %d 次空输出（瞬态故障），可稍后重新调用 polish_draft 重试",
		lastErr, polisherEmptyRetryMax)
}

// ── Polisher 基线观测（§14 第 2 步）──────────────────────────────────
//
// 纯增量观测：为后续 one-shot → 多轮 polisher 改造提供基线数据（实际 API
// 调用数 / token usage / 截断恢复 / edit 数 / 耗时与 CPU）。观测只写 slog
// 日志，不改协议、不改落盘、不改错误处理、不改任何返回结构；观测代码自身
// 错误只记日志，绝不影响主流程。

// polisherRunObs 汇总一次 polish run 的观测数据：runPolisherWithEmptyRetry
// 逐次尝试累加，Execute 主路径在 polisher 调用前后补计时，返回前输出汇总日志。
type polisherRunObs struct {
	attempts        int           // 实际 API 调用数（runPolisherWithEmptyRetry 尝试次数）
	emptyOutputs    int           // 空输出次数（触发重试的瞬态故障数）
	usageInput      int           // 全部尝试 input token 合计
	usageOutput     int           // 全部尝试 output token 合计
	usageCacheRead  int           // 全部尝试 cache_read token 合计
	usageCacheWrite int           // 全部尝试 cache_write token 合计
	usageCost       float64       // 全部尝试 cost 合计
	usageTurns      int           // 全部尝试 turns 合计（含 length recovery 轮）
	usageTools      int           // 全部尝试 tools 合计（polisher 无工具，恒 0）
	elapsed         time.Duration // polisher 调用总耗时（Run 返回后由 Execute 补记）
	cpuDelta        time.Duration // 进程 CPU 时间差（user+kernel）；不可得时为 0
	cpuOK           bool          // 进程 CPU 时间是否可得
}

// recordAttempt 累加一次 Run 的观测数据（成功与失败尝试都消耗 token，
// RunResult 在失败路径同样携带已消耗 usage）。
func (o *polisherRunObs) recordAttempt(runResult subagent.RunResult, empty bool) {
	o.attempts++
	if empty {
		o.emptyOutputs++
	}
	u := runResult.Usage
	o.usageInput += u.Input
	o.usageOutput += u.Output
	o.usageCacheRead += u.CacheRead
	o.usageCacheWrite += u.CacheWrite
	o.usageCost += u.Cost
	o.usageTurns += u.Turns
	o.usageTools += u.Tools
}

// lengthRecoveryEst 估算本次 run 的 length recovery 次数。agentcore v1.7.13
// 的 RunResult 不直接暴露 recovery 计数（loop 内 lengthRecoveryCount 不对外），
// 但 polisher 无工具（ts.Polisher 恒为空）、无 steering/follow-up 注入，
// AgentLoop 内每次 length 截断恢复恰好多消耗一轮模型调用（turns+1），故
// turns - attempts ≈ recovery 次数。若未来 polisher 引入工具调用
// （usageTools>0）该推导失效，返回 -1 表示不可得。
func (o *polisherRunObs) lengthRecoveryEst() int {
	if o.usageTools != 0 {
		return -1
	}
	return o.usageTurns - o.attempts
}

// logPolisherAttempt 输出单次尝试的观测日志（runPolisherWithEmptyRetry 内部）。
// finish_reason 恒为 "unavailable"：agentcore v1.7.13 RunResult 不暴露
// finish reason（仅事件流 EventModelResponse.Message.StopReason 可见，而
// Runner.SetEventObserver 是单槽位，已被 BuildWorkers 的 usage observer 占用，
// 不得替换）。
func logPolisherAttempt(chapter, attempt int, runResult subagent.RunResult, empty bool, err error) {
	attrs := []any{
		"module", "tools",
		"chapter", chapter,
		"attempt", attempt,
		"max", polisherEmptyRetryMax,
		"finish_reason", "unavailable",
		"output_empty", empty,
		"usage_input", runResult.Usage.Input,
		"usage_output", runResult.Usage.Output,
		"usage_cache_read", runResult.Usage.CacheRead,
		"usage_cache_write", runResult.Usage.CacheWrite,
		"usage_cost", runResult.Usage.Cost,
		"usage_turns", runResult.Usage.Turns,
		"usage_tools", runResult.Usage.Tools,
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	slog.Info("polisher 调用观测（单次尝试）", attrs...)
}

// logPolisherRunObs 输出一次 polish run 的汇总观测日志（Execute 主路径，
// polisher 调用完成后、返回前）。out 是各路径返回的 PolishDraftOutput JSON
// （提取 edit 审计字段与 stage/model）；err 非 nil 表示本次 run 失败。
// 观测失败（如 out 无法解析）只记日志，绝不影响主流程。
func logPolisherRunObs(chapter, wordCount int, obs *polisherRunObs, out json.RawMessage, err error) {
	var audit PolishDraftOutput
	if len(out) > 0 {
		_ = json.Unmarshal(out, &audit) // 观测失败不影响主流程
	}
	attrs := []any{
		"module", "tools",
		"chapter", chapter,
		"word_count", wordCount,
		"attempts", obs.attempts,
		"empty_outputs", obs.emptyOutputs,
		"length_recovery_est", obs.lengthRecoveryEst(),
		"finish_reason", "unavailable",
		"usage_input", obs.usageInput,
		"usage_output", obs.usageOutput,
		"usage_cache_read", obs.usageCacheRead,
		"usage_cache_write", obs.usageCacheWrite,
		"usage_cost", obs.usageCost,
		"usage_turns", obs.usageTurns,
		"usage_tools", obs.usageTools,
		"proposed_edit_count", audit.ProposedEditCount,
		"dropped_edit_count", audit.DroppedEditCount,
		// applied = proposed - dropped：每条 edit 恰好落入 Applied 或 Dropped
		// 一个桶（ApplyPolishEditPlanDetailed / DropApplied 保证）。
		"applied_edit_count", audit.ProposedEditCount - audit.DroppedEditCount,
		"partial", audit.Partial,
		"normalized_match_count", audit.NormalizedMatchCount,
		"match_modes", audit.MatchModes,
		"stage", audit.Stage,
		"polisher_model", audit.PolisherModel,
		"elapsed_ms", obs.elapsed.Milliseconds(),
		"cpu_ms", obs.cpuDelta.Milliseconds(),
		"cpu_available", obs.cpuOK,
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	slog.Info("polisher run 观测（汇总）", attrs...)
}

// polishStageForChapter 判定精修 stage：重写/打磨队列章节 → "rewrite"，其余 → "draft"。
// 与 buildPolishTask/写 checkpoint 共用同一判定，保证 normal 与 degraded 路径一致。
func polishStageForChapter(st *store.Store, chapter int) string {
	stage := "draft"
	progress, pErr := st.Progress.Load()
	if pErr == nil && progress != nil && slices.Contains(progress.PendingRewrites, chapter) {
		stage = "rewrite"
	}
	return stage
}

// classifyPolishDegradableError 分类 polisher 经有限重试后仍失败的错误：返回
// （ErrorCategory, 是否可降级）。可降级 → 写入 degraded polish checkpoint 后
// 推进流水线；不可降级 → 原样返回错误。
//
// 可降级（瞬态/可恢复类，agentcore 错误分类，见 .slim/forks/agentcore/errors.go）：
//   - stream idle 超时：errors.Is(err, agentcore.ErrProviderStreamIdle)
//   - provider timeout / network / rate_limit / overloaded：
//     agentcore.IsFailoverEligible（含 stream idle，已先单独判定）
//   - polisher MaxTurns：errors.Is(err, agentcore.ErrMaxTurns)
//     （长度截断 recovery 预算耗尽——正文从未被接受，降级留痕优于失败重派）
//
// 不可降级（必须原样失败，绝不写 degraded）：
//   - context.Canceled（用户取消 / Host 关闭）
//   - content filter / 安全拒绝（ErrProviderContentFilter）
//   - 其余未分类错误（含空输出重试耗尽、auth、quota、context_overflow）
func classifyPolishDegradableError(err error) (string, bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return "", false
	}
	if errors.Is(err, agentcore.ErrProviderContentFilter) {
		return "", false
	}
	if errors.Is(err, agentcore.ErrMaxTurns) {
		return "max_turns", true
	}
	if errors.Is(err, agentcore.ErrProviderStreamIdle) {
		return "stream_idle", true
	}
	if agentcore.IsFailoverEligible(err) {
		if reason := agentcore.FailoverReason(err); reason != "" {
			return reason, true
		}
		return "provider", true
	}
	return "", false
}

// handlePolisherFailure 处理 polisher 经有限重试后仍失败的情况：
//
// 可降级错误 → 写入绑定当前草稿 digest 的 degraded polish checkpoint（正文不变），
// 返回成功摘要（Degraded:true + ErrorCategory + 下一步必须 check_consistency）。
// 这是"degraded polish checkpoint"（Oracle 方案第 3 步）：FSM 将 degraded 记录视作
// 合法 polish 记录 → 强制 post-polish check → review → commit，消除生产故障
// "polish 连续失败 → 永远 needs_polish → commit 被拒 → 无脑重派同一章烧钱"
// （ch71 类死锁）——失败有了可接受终态。
//
// 不可降级错误 → 原样返回（无任何 checkpoint 落盘，状态不变）。
//
// 降级只允许一次：同一 digest 已存在 degraded polish 记录时不再重复降级（原样返回
// 错误）——正常情况下 FSM 在 degraded 后已不允许再次 polish_draft（stage 为
// needs_post_polish_check），本守卫是纵深防御，防"降级 → 重派 → 再降级"自循环。
func (t *PolishDraftTool) handlePolisherFailure(chapter int, inputDigest string, wordCount int, baseline *store.PolishBaseline, err error) (json.RawMessage, error) {
	category, degradable := classifyPolishDegradableError(err)
	if !degradable {
		return nil, err
	}
	if cp := t.store.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "polish"); cp != nil && cp.Degraded && cp.Digest == inputDigest {
		return nil, err
	}
	// 复核阻塞项 6：降级留痕走 store 层原子 CAS——模型调用期间草稿/账本/polish
	// checkpoint 被并发修改时不写绑定旧 digest 的陈旧 degraded checkpoint
	// （返回 stale 错误，writer 重新走流程）。
	polisherModel := t.loadPolisherModelName()
	stage := polishStageForChapter(t.store, chapter)
	if _, aErr := t.store.CommitDegradedPolishCheckpoint(
		chapter, baseline,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       false,
			Degraded:      true,
			ErrorCategory: category,
		},
	); aErr != nil {
		if errors.Is(aErr, store.ErrPolishCandidateStale) {
			return nil, fmt.Errorf("polisher 失败但草稿已在调用期间被修改，不写降级绑定（避免陈旧绑定）: %w: %w",
				errs.ErrToolPrecondition, store.ErrPolishCandidateStale)
		}
		// checkpoint/store 写失败不可降级：原样返回（账本未留痕，状态不变）。
		return nil, fmt.Errorf("checkpoint polish (degraded): %w", aErr)
	}
	slog.Warn("polisher 失败已降级为 degraded polish checkpoint", "module", "tools", "chapter", chapter,
		"category", category, "digest", inputDigest, "err", err)

	return json.Marshal(PolishDraftOutput{
		Chapter:       chapter,
		Polished:      true,
		Changed:       false,
		Degraded:      true,
		ErrorCategory: category,
		InputDigest:   inputDigest,
		OutputDigest:  inputDigest,
		PolisherModel: polisherModel,
		Stage:         stage,
		WordCount:     wordCount,
		Reason: fmt.Sprintf("polisher 经有限重试仍失败（%s），已写入 degraded polish checkpoint，正文未变（digest=%s）",
			category, inputDigest),
		NextStep: "精修失败已降级记录（正文未变）。下一步**必须**调用 check_consistency 重新核验；通过后按 required_next_action 继续（review_style → terminal → commit_chapter）",
	})
}

// loadPolisherModelName 从 polisher runner 的注册配置读取当前模型名。
// 与 review_style 的 loadCriticModelName 同构。
func (t *PolishDraftTool) loadPolisherModelName() string {
	cfg, ok := t.polisherRunner.AgentConfig("polisher")
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
