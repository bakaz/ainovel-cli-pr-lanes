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
	// mechRejectStreak 记录同章连续"机械回归拒绝"（edit 路径候选引入新的 error 级
	// 机械违规）的次数。内存态、进程生命周期内有效（Writer 单次 Run 内连续重派
	// 同一章即命中）；每次成功精修或收敛落盘后清零。连续 2 次 → 写 rejected 性质
	// polish checkpoint（复用 Degraded+ErrorCategory，正文未变）→ FSM 收敛到
	// post-check，防"edit plan 反复引入机械违规 → 永久 fail-closed 死循环"（循环 B）。
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

	// ── 3. 加载草稿 ──
	content, wordCount, err := t.store.Drafts.LoadChapterContent(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	inputDigest := domain.DigestDraft(content)

	// ── 4. 修改门禁：terminal 评审状态下的章节不允许精修（fail-fast，避免 polisher 空转） ──
	if err := CheckStyleReviewMutationGuard(t.store, a.Chapter); err != nil {
		return nil, fmt.Errorf("polish_draft: %w", err)
	}

	// ── 5. 构建任务 payload（草稿 + 评审依据 + 已给意见 + 重写 brief）并调用 polisher ──
	taskText := t.buildPolishTask(a.Chapter, content, wordCount)
	outputText, err := t.runPolisherWithEmptyRetry(ctx, a.Chapter, taskText)
	if err != nil {
		// 可降级错误（stream idle / provider timeout / network 类 / MaxTurns）→
		// 写 degraded polish checkpoint 后返回成功摘要；不可降级原样返回。
		return t.handlePolisherFailure(a.Chapter, inputDigest, wordCount, err)
	}

	// ── 6. 输出解析与执行路径（ora-1 形态 2）： ──
	//    - 成功解析为 edit list → edit 路径：内存中基于同一输入快照原子应用全部
	//      edit → 一次 SaveDraft → 一个最终 polish checkpoint（Method=edit_list）。
	//    - 解析失败（非 JSON/围栏/未知字段/纯正文）→ 回退现有整章模式（旧协议，
	//      渐进切换；整章模式现有校验/落盘/checkpoint 全保留）。
	//    - 契约错误（edit plan 形状但 version 不受支持等）→ fail-closed，草稿原样、
	//      不写 checkpoint、返回明确错误（不混入 Degraded=true）。
	plan, fallback, parseErr := ParsePolishEditPlan(outputText)
	if parseErr != nil {
		if !fallback {
			return nil, fmt.Errorf("polisher 输出 edit plan 契约错误：%v: %w", parseErr, errs.ErrToolPrecondition)
		}
		return t.applyFullTextPolish(a.Chapter, content, wordCount, inputDigest, outputText)
	}
	return t.applyEditPlan(a.Chapter, content, wordCount, inputDigest, plan)
}

// applyFullTextPolish 是整章重输出回退路径（旧协议）：polisher 输出无法解析为
// edit plan（非 JSON/围栏/未知字段/纯正文）时进入。现有校验/落盘/checkpoint
// 全保留（渐进切换：旧协议模型仍可用），仅 checkpoint 增加 Method=full_text 审计字段。
func (t *PolishDraftTool) applyFullTextPolish(chapter int, content string, wordCount int, inputDigest, outputText string) (json.RawMessage, error) {
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
	if outputRunes > maxPolishOutputRunes {
		return nil, fmt.Errorf("polisher 输出 %d runes 超过上限 %d，拒绝落盘: %w",
			outputRunes, maxPolishOutputRunes, errs.ErrToolPrecondition)
	}
	trimmed := strings.TrimSpace(outputText)
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		return nil, fmt.Errorf("polisher 输出疑似纯 JSON（非正文），拒绝落盘: %w", errs.ErrToolPrecondition)
	}
	if strings.HasPrefix(trimmed, "```") && strings.HasSuffix(trimmed, "```") {
		return nil, fmt.Errorf("polisher 输出被代码围栏整体包裹，拒绝落盘: %w", errs.ErrToolPrecondition)
	}

	// ── 7. 保存为草稿（与 draft_chapter 同路径：SaveDraft + draft checkpoint） ──
	outputDigest := domain.DigestDraft(outputText)
	if err := t.store.Drafts.SaveDraft(chapter, outputText); err != nil {
		return nil, fmt.Errorf("save polished draft: %w: %w", errs.ErrStoreWrite, err)
	}
	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(chapter), "draft",
		fmt.Sprintf("drafts/%02d.draft.md", chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint draft after polish: %w", err)
	}

	// ── 8. 写 polish checkpoint（input_digest/output_digest/polisher_model/stage/changed） ──
	stage := polishStageForChapter(t.store, chapter)
	changed := outputDigest != inputDigest
	polisherModel := t.loadPolisherModelName()
	if _, err := t.store.Checkpoints.AppendPolish(
		domain.ChapterScope(chapter), "polish",
		fmt.Sprintf("drafts/%02d.draft.md", chapter),
		outputDigest,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       changed,
			Method:        "full_text",
		},
	); err != nil {
		return nil, fmt.Errorf("checkpoint polish: %w", err)
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

// applyEditPlan 是 edit list 路径（ora-1 形态 2 主体）：校验 → 原子应用 →
// 机械回归门禁 → 一次 SaveDraft → draft checkpoint → 一个最终 polish checkpoint
// （Method=edit_list、EditCount=len(edits)）。任一校验失败 fail-closed：草稿原样、
// 不写 polish checkpoint、返回明确错误（区分契约/内容/机械回归，不混入 Degraded=true）。
func (t *PolishDraftTool) applyEditPlan(chapter int, content string, wordCount int, inputDigest string, plan *PolishEditPlan) (json.RawMessage, error) {
	// ── 1. 校验 + 应用（纯函数，基于同一输入快照；无部分结果） ──
	candidate, err := ApplyPolishEditPlan(content, plan)
	if err != nil {
		return nil, fmt.Errorf("polisher edit plan 内容校验错误：%v: %w", err, errs.ErrToolPrecondition)
	}

	// ── 2. 机械回归门禁（统一检查器 computeMechanicalViolations，与 check_consistency
	//    同源）：若 polisher 输入无 error 级机械违规，则候选必须也无——精修不得
	//    引入新的机械违规（防"修文风 → 引入禁词/文学腔"的静默劣化）。 ──
	if !hasErrorViolations(computeMechanicalViolations(t.store, content, wordCount)) {
		if hasErrorViolations(computeMechanicalViolations(t.store, candidate, utf8.RuneCountInString(candidate))) {
			return t.handleMechanicalRegression(chapter, content, wordCount, inputDigest, plan)
		}
	}

	// ── 3. 原子落盘：一次 SaveDraft + draft checkpoint + 一个 polish checkpoint ──
	outputDigest := domain.DigestDraft(candidate)
	if err := t.store.Drafts.SaveDraft(chapter, candidate); err != nil {
		return nil, fmt.Errorf("save polished draft: %w: %w", errs.ErrStoreWrite, err)
	}
	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(chapter), "draft",
		fmt.Sprintf("drafts/%02d.draft.md", chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint draft after polish: %w", err)
	}

	stage := polishStageForChapter(t.store, chapter)
	changed := outputDigest != inputDigest
	polisherModel := t.loadPolisherModelName()
	if _, err := t.store.Checkpoints.AppendPolish(
		domain.ChapterScope(chapter), "polish",
		fmt.Sprintf("drafts/%02d.draft.md", chapter),
		outputDigest,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       changed,
			Method:        "edit_list",
			EditCount:     len(plan.Edits),
		},
	); err != nil {
		return nil, fmt.Errorf("checkpoint polish: %w", err)
	}
	// 成功推进：清零机械回归连续计数。
	t.mechRejectStreak[chapter] = 0

	// ── 4. 摘要返回（不回传全文） ──
	return json.Marshal(PolishDraftOutput{
		Chapter:       chapter,
		Polished:      true,
		Changed:       changed,
		InputDigest:   inputDigest,
		OutputDigest:  outputDigest,
		PolisherModel: polisherModel,
		Stage:         stage,
		WordCount:     utf8.RuneCountInString(candidate),
		NextStep:      "精修完成。下一步**必须**调用 check_consistency 重新核验；通过后按返回的 required_next_action 继续（review_style → terminal → commit_chapter）",
	})
}

// handleMechanicalRegression 处理 edit 路径的机械回归拒绝（循环 B 收敛）：
//
//   - 第一次拒绝：fail-closed——草稿原样、不写 polish checkpoint、返回明确错误
//     （区分"机械回归"类别，不混入 Degraded=true），模型可重试。
//   - 连续第 2 次拒绝（同章，内存计数）：写 rejected 性质的 polish checkpoint
//     （复用 Degraded=true + ErrorCategory="mechanical_regression"，Digest=当前草稿、
//     Changed=false、Method=edit_list）→ FSM 视其为合法 polish 记录（degraded 语义，
//     与 provider 降级同路径）→ 收敛到 post-check → check → review，防"edit plan
//     反复引入机械违规 → 永久 fail-closed 死循环"。返回成功摘要（Degraded=true +
//     ErrorCategory=mechanical_regression，调用方继续 post-polish check）。
func (t *PolishDraftTool) handleMechanicalRegression(chapter int, content string, wordCount int, inputDigest string, plan *PolishEditPlan) (json.RawMessage, error) {
	t.mechRejectStreak[chapter]++
	if t.mechRejectStreak[chapter] < 2 {
		return nil, fmt.Errorf("polisher edit plan 机械回归：候选引入新的 error 级机械违规，拒绝落盘（草稿未变）；连续 2 次将自动收敛为 rejected checkpoint: %w",
			errs.ErrToolPrecondition)
	}
	t.mechRejectStreak[chapter] = 0

	polisherModel := t.loadPolisherModelName()
	stage := polishStageForChapter(t.store, chapter)
	if _, aErr := t.store.Checkpoints.AppendPolish(
		domain.ChapterScope(chapter), "polish",
		fmt.Sprintf("drafts/%02d.draft.md", chapter),
		inputDigest,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       false,
			Degraded:      true,
			ErrorCategory: "mechanical_regression",
			Method:        "edit_list",
			EditCount:     len(plan.Edits),
		},
	); aErr != nil {
		// checkpoint/store 写失败不可收敛：原样返回（账本未留痕，状态不变）。
		return nil, fmt.Errorf("checkpoint polish (mechanical regression rejected): %w", aErr)
	}
	slog.Warn("polisher edit plan 连续 2 次机械回归拒绝，已写入 rejected polish checkpoint", "module", "tools", "chapter", chapter,
		"digest", inputDigest, "edits", len(plan.Edits))

	return json.Marshal(PolishDraftOutput{
		Chapter:       chapter,
		Polished:      true,
		Changed:       false,
		Degraded:      true,
		ErrorCategory: "mechanical_regression",
		InputDigest:   inputDigest,
		OutputDigest:  inputDigest,
		PolisherModel: polisherModel,
		Stage:         stage,
		WordCount:     wordCount,
		Reason: fmt.Sprintf("polisher edit plan 连续 2 次引入 error 级机械违规，已写入 rejected polish checkpoint（正文未变，digest=%s）",
			inputDigest),
		NextStep: "精修被拒绝并已收敛记录（正文未变）。下一步**必须**调用 check_consistency 重新核验；通过后按返回的 required_next_action 继续（review_style → terminal → commit_chapter）",
	})
}

// buildPolishTask 构造发送给 polisher runner 的任务文本：
// 当前草稿全文 + 规范评审依据（风格目标/契约/指南针文风/锚点/用户规则/事实大纲）
// + 已给的 revise findings（完整六字段）+ 重写/打磨 brief（PendingRewrites 时）。
// 与 review_style 的 basis 使用同一数据源（buildReviewBasis），保证精修与评审看到
// 同一份风格事实。
func (t *PolishDraftTool) buildPolishTask(chapter int, content string, wordCount int) string {
	basis := buildReviewBasis(t.store, chapter, t.polisherPromptHash)
	basisJSON, _ := json.Marshal(basis)

	var sb strings.Builder
	fmt.Fprintf(&sb, "## 精修任务\n\n### 章节\n第 %d 章\n\n### 待精修草稿（字数：%d）\n%s\n\n",
		chapter, wordCount, content)
	fmt.Fprintf(&sb, "### 精修依据（风格目标/章节契约/指南针文风/锚点/用户规则/事实大纲）\n%s\n\n", basisJSON)

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

	fmt.Fprintf(&sb, "请严格按精修者提示词（polisher）输出结构化精修 edit 列表 JSON（{\"version\":1,\"edits\":[{\"old_string\":\"原文唯一连续片段\",\"new_string\":\"精修后片段\"}]}）；无修改时输出 {\"version\":1,\"edits\":[]}。")
	return sb.String()
}

// runPolisherWithEmptyRetry 调用 polisher runner，仅在输出为空（空串/仅空白）时
// 自动重试，指数退避（2s/4s/8s），最多 polisherEmptyRetryMax 次。
// 只对"空输出"这种瞬态故障重试：runner 错误与非空输出（即使校验失败）立即返回。
// 返回合并后的输出文本（Output 优先，空时回退 TerminalResult）。
func (t *PolishDraftTool) runPolisherWithEmptyRetry(ctx context.Context, chapter int, taskText string) (string, error) {
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
			return "", fmt.Errorf("polisher call failed: %w", err)
		}

		outputText := runResult.Output
		if outputText == "" && runResult.TerminalResult != nil {
			outputText = string(runResult.TerminalResult)
		}
		if strings.TrimSpace(outputText) != "" {
			return outputText, nil
		}

		lastErr = fmt.Errorf("polisher returned empty output (attempt %d/%d)", attempt, polisherEmptyRetryMax)
		slog.Warn("polisher 返回空输出，准备重试", "module", "tools", "chapter", chapter,
			"attempt", attempt, "max", polisherEmptyRetryMax)
	}
	return "", fmt.Errorf("%w；连续 %d 次空输出（瞬态故障），可稍后重新调用 polish_draft 重试",
		lastErr, polisherEmptyRetryMax)
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
func (t *PolishDraftTool) handlePolisherFailure(chapter int, inputDigest string, wordCount int, err error) (json.RawMessage, error) {
	category, degradable := classifyPolishDegradableError(err)
	if !degradable {
		return nil, err
	}
	if cp := t.store.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "polish"); cp != nil && cp.Degraded && cp.Digest == inputDigest {
		return nil, err
	}

	polisherModel := t.loadPolisherModelName()
	stage := polishStageForChapter(t.store, chapter)
	if _, aErr := t.store.Checkpoints.AppendPolish(
		domain.ChapterScope(chapter), "polish",
		fmt.Sprintf("drafts/%02d.draft.md", chapter),
		inputDigest,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       false,
			Degraded:      true,
			ErrorCategory: category,
		},
	); aErr != nil {
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
