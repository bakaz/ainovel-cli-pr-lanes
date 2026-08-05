package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// CheckConsistencyTool 返回草稿摘要、机械违规和全部状态数据，供 Agent 自行对照判断。
// 纯 IO 工具：只负责加载数据，不注入指令。
type CheckConsistencyTool struct {
	store *store.Store
	// pipelineEnabled 是精修流水线开关（BuildWorkers 注入）：开启时若草稿缺少
	// 与当前 digest 匹配的 polish checkpoint，required_next_action 建议 polish_draft。
	pipelineEnabled bool
}

func NewCheckConsistencyTool(store *store.Store) *CheckConsistencyTool {
	return &CheckConsistencyTool{store: store}
}

// SetPipelineEnabled 设置精修流水线开关（BuildWorkers 注入）。
func (t *CheckConsistencyTool) SetPipelineEnabled(v bool) { t.pipelineEnabled = v }

func (t *CheckConsistencyTool) Name() string { return "check_consistency" }
func (t *CheckConsistencyTool) Description() string {
	return "检查已写草稿的规则违规，并加载世界规则、伏笔、关系、别名、最近摘要供语义自审。只返回草稿摘要，不重复返回全文；必须在 draft_chapter 之后调用"
}
func (t *CheckConsistencyTool) Label() string { return "一致性检查" }

// 只读工具（仅追加 checkpoint 事件，不改状态），可被并发调度。
func (t *CheckConsistencyTool) ReadOnly(_ json.RawMessage) bool        { return true }
func (t *CheckConsistencyTool) ConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *CheckConsistencyTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("要检查的章节号")).Required(),
	)
}

func (t *CheckConsistencyTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
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

	result := map[string]any{"chapter": a.Chapter}

	// 章节内容
	content, wordCount, err := t.store.Drafts.LoadChapterContent(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load chapter content: %w: %w", errs.ErrStoreRead, err)
	}
	if content == "" {
		return nil, fmt.Errorf("no content found for chapter %d: %w", a.Chapter, errs.ErrToolPrecondition)
	}
	digest := sha256.Sum256([]byte(content))
	digestStr := fmt.Sprintf("sha256:%x", digest[:])
	result["content_digest"] = digestStr
	result["word_count"] = wordCount
	// 用户规则机械检查 + 文学腔句式硬闸。draft/check 阶段只报事实不阻断
	// （error 级拦截发生在 commit_chapter 的 CheckLiteraryProseGate 与
	// review_style 的 accepted 前置闸——见 ReviewStyleTool.checkMechanicalGate）。
	violations := computeMechanicalViolations(t.store, content, wordCount)
	result["rule_violations"] = violations

	// 对照数据：保留全局性的一致性检查数据，避免重复加载 novel_context 已有的窗口数据
	if rules, _ := t.store.World.LoadWorldRules(); len(rules) > 0 {
		result["world_rules"] = rules
	}
	if foreshadow, _ := t.store.World.LoadActiveForeshadow(); len(foreshadow) > 0 {
		result["foreshadow_ledger"] = foreshadow
	}
	if relationships, _ := t.store.World.LoadRelationships(); len(relationships) > 0 {
		result["relationships"] = relationships
	}
	if chars, _ := t.store.Characters.Load(); len(chars) > 0 {
		aliasMap := make(map[string]string)
		for _, c := range chars {
			for _, alias := range c.Aliases {
				aliasMap[alias] = c.Name
			}
		}
		if len(aliasMap) > 0 {
			result["alias_map"] = aliasMap
		}
	}
	if summaries, _ := t.store.Summaries.LoadRecentSummaries(a.Chapter, 2); len(summaries) > 0 {
		result["recent_summaries"] = summaries
	}

	// 每次执行都追加新 checkpoint（不做 digest 幂等去重）：review_style 依赖
	// consistency_check 的 seq > polish checkpoint 的 seq 证明 polish → consistency
	// → critic 的顺序；幂等去重会让"打磨后重复 check"不再推进 seq，破坏顺序绑定。
	if _, err := t.store.Checkpoints.AppendAlways(
		domain.ChapterScope(a.Chapter), "consistency_check",
		fmt.Sprintf("drafts/%02d.draft.md", a.Chapter),
		digestStr,
	); err != nil {
		return nil, fmt.Errorf("checkpoint consistency check: %w", err)
	}

	// ── 计算 required_next_action 辅助提示 ──
	action := computeNextAction(t, a.Chapter, violations, digestStr)
	if action != nil {
		result["required_next_action"] = action
	}

	return json.Marshal(result)
}

// computeNextAction 加载运行元信息和风格评审账本，调用纯函数计算下一步建议。
// 读取失败/异常时返回 nil（字段缺省，不阻塞 check_consistency 主结果）。
func computeNextAction(t *CheckConsistencyTool, chapter int, violations []rules.Violation, digestStr string) *RequiredNextAction {
	meta, err := t.store.RunMeta.Load()
	if err != nil || meta == nil {
		return nil
	}

	ledger, lerr := t.store.StyleReview.Load(chapter)
	if lerr != nil {
		return nil
	}

	// rewrite queue 检测：直接加载 Progress（不复用吞错的 boolean helper）
	inRewriteQueue := false
	progress, pErr := t.store.Progress.Load()
	if pErr != nil {
		return nil
	}
	if progress != nil {
		inRewriteQueue = slices.Contains(progress.CompletedChapters, chapter) &&
			slices.Contains(progress.PendingRewrites, chapter)
	}

	// 精修流水线（pipeline 启用时）：草稿缺少 fresh polish checkpoint → 建议 polish_draft。
	// 覆盖两类场景：初稿写完后直接 check（应先 polish_draft）；critic revise 后正文被改
	// （commit gate 会拒绝 stale checkpoint，这里提前引导重跑 polish_draft）。
	if t.pipelineEnabled && !hasErrorViolations(violations) && !polishCheckpointMatches(t.store, chapter, digestStr) {
		return &RequiredNextAction{
			Action: ActionPolishDraft,
			Reason: fmt.Sprintf("第 %d 章缺少与当前草稿匹配的 polish 记录，请先调用 polish_draft 精修后再继续", chapter),
		}
	}

	// rewrite 队列需要终稿 digest 来判断草稿是否实际变更
	var finalDigest string
	if inRewriteQueue {
		finalContent, fErr := t.store.Drafts.LoadChapterText(chapter)
		if fErr != nil {
			return nil
		}
		if finalContent != "" {
			h := sha256.Sum256([]byte(finalContent))
			finalDigest = fmt.Sprintf("sha256:%x", h[:])
		}
	}

	// pipeline 启用时把最新 polish checkpoint seq 传给下一步建议（R == latest P
	// 绑定，与 CheckPolishPipelineGate 的严格绑定一致）；关闭时传 nil（不绑定）。
	var binding *PolishPipelineBinding
	if t.pipelineEnabled {
		if cp := t.store.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "polish"); cp != nil {
			binding = &PolishPipelineBinding{LatestPolishSeq: cp.Seq}
		}
	}

	return ComputeRequiredNextAction(
		meta.StyleReviewMode,
		chapter,
		hasErrorViolations(violations),
		digestStr,
		ledger,
		inRewriteQueue,
		finalDigest,
		binding,
	)
}

// computeMechanicalViolations 计算章节草稿的机械违规集：rules.Lint（内置产品底线）
// + rules.CheckLiteraryGate（用户规则机械检查 + 文学腔句式硬闸，含本书用户规则
// 快照）。check_consistency 与 review_style 的 accepted 前置闸共用此函数，
// 保证"评审可接受的草稿"与"一致性检查可见的 error"使用同一规则集、同一严重度判定。
func computeMechanicalViolations(st *store.Store, content string, wordCount int) []rules.Violation {
	vs := append([]rules.Violation{}, rules.Lint(content)...)
	structured := rules.SystemDefaults().Structured
	if snap, loadErr := st.UserRules.Load(); loadErr == nil && snap != nil {
		structured = snap.Structured
	}
	return append(vs, rules.CheckLiteraryGate(content, wordCount, structured)...)
}
