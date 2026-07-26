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
}

func NewCheckConsistencyTool(store *store.Store) *CheckConsistencyTool {
	return &CheckConsistencyTool{store: store}
}

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
	violations := append([]rules.Violation{}, rules.Lint(content)...)
	structured := rules.SystemDefaults().Structured
	if snap, loadErr := t.store.UserRules.Load(); loadErr == nil && snap != nil {
		structured = snap.Structured
	}
	violations = append(violations, rules.Check(content, wordCount, structured)...)
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

	if _, err := t.store.Checkpoints.Append(
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

	return ComputeRequiredNextAction(
		meta.StyleReviewMode,
		chapter,
		hasErrorViolations(violations),
		digestStr,
		ledger,
		inRewriteQueue,
		finalDigest,
	)
}
