package tools

import (
	"context"
	"encoding/json"
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
}

func NewPolishDraftTool(s *store.Store, polisherRunner *subagent.Runner, polisherPromptHash string) *PolishDraftTool {
	return &PolishDraftTool{store: s, polisherRunner: polisherRunner, polisherPromptHash: polisherPromptHash}
}

// SetEnabled 设置 chapter pipeline 开关。由 BuildWorkers 按配置注入。
func (t *PolishDraftTool) SetEnabled(v bool) { t.enabled = v }

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

	// ── 2. 加载草稿 ──
	content, wordCount, err := t.store.Drafts.LoadChapterContent(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	inputDigest := domain.DigestDraft(content)

	// ── 3. 修改门禁：terminal 评审状态下的章节不允许精修（fail-fast，避免 polisher 空转） ──
	if err := CheckStyleReviewMutationGuard(t.store, a.Chapter); err != nil {
		return nil, fmt.Errorf("polish_draft: %w", err)
	}

	// ── 4. 构建任务 payload（草稿 + 评审依据 + 已给意见 + 重写 brief）并调用 polisher ──
	taskText := t.buildPolishTask(a.Chapter, content, wordCount)
	outputText, err := t.runPolisherWithEmptyRetry(ctx, a.Chapter, taskText)
	if err != nil {
		return nil, err
	}

	// ── 5. 校验：非空 / UTF-8 / 最短长度（防"好的，已完成精修"式短文本被当正文保存）/
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

	// ── 6. 保存为草稿（与 draft_chapter 同路径：SaveDraft + draft checkpoint） ──
	outputDigest := domain.DigestDraft(outputText)
	if err := t.store.Drafts.SaveDraft(a.Chapter, outputText); err != nil {
		return nil, fmt.Errorf("save polished draft: %w: %w", errs.ErrStoreWrite, err)
	}
	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(a.Chapter), "draft",
		fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint draft after polish: %w", err)
	}

	// ── 7. 写 polish checkpoint（input_digest/output_digest/polisher_model/stage/changed） ──
	stage := "draft"
	progress, pErr := t.store.Progress.Load()
	if pErr == nil && progress != nil && slices.Contains(progress.PendingRewrites, a.Chapter) {
		stage = "rewrite"
	}
	changed := outputDigest != inputDigest
	polisherModel := t.loadPolisherModelName()
	if _, err := t.store.Checkpoints.AppendPolish(
		domain.ChapterScope(a.Chapter), "polish",
		fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		outputDigest,
		domain.PolishCheckpointMeta{
			InputDigest:   inputDigest,
			PolisherModel: polisherModel,
			Stage:         stage,
			Changed:       changed,
		},
	); err != nil {
		return nil, fmt.Errorf("checkpoint polish: %w", err)
	}

	// ── 8. 摘要返回（不回传全文） ──
	return json.Marshal(PolishDraftOutput{
		Chapter:       a.Chapter,
		Polished:      true,
		Changed:       changed,
		InputDigest:   inputDigest,
		OutputDigest:  outputDigest,
		PolisherModel: polisherModel,
		Stage:         stage,
		WordCount:     utf8.RuneCountInString(outputText),
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

	fmt.Fprintf(&sb, "请严格按精修者提示词（polisher）输出整章打磨后的完整正文。")
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
