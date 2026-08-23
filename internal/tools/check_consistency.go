package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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
	// 与 fsmConfig.PipelineEnabled 同源（SetPipelineEnabled 同步两者）。
	pipelineEnabled bool
	// fsmConfig 是章节流水线强制状态机配置（BuildWorkers 注入）；Enabled 时
	// Execute 入口调用 RequireChapterAction 强制顺序（ReadOnly=true 也强制：
	// 阻止 clean check 后重复 check、needs_polish/needs_commit 时重复 check）。
	fsmConfig ChapterFSMConfig
}

func NewCheckConsistencyTool(store *store.Store) *CheckConsistencyTool {
	return &CheckConsistencyTool{store: store}
}

// SetPipelineEnabled 设置精修流水线开关（BuildWorkers 注入）。
func (t *CheckConsistencyTool) SetPipelineEnabled(v bool) {
	t.pipelineEnabled = v
	t.fsmConfig.PipelineEnabled = v
}

// SetChapterFSMConfig 注入章节流水线强制状态机配置（BuildWorkers 调用）。
func (t *CheckConsistencyTool) SetChapterFSMConfig(cfg ChapterFSMConfig) { t.fsmConfig = cfg }

// FSMConfig 返回注入的章节流水线配置（构建/测试诊断用）。
func (t *CheckConsistencyTool) FSMConfig() ChapterFSMConfig { return t.fsmConfig }

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

	// 章节流水线强制状态机（Enabled 时）：draft_dirty/needs_post_polish_check
	// 允许 check；needs_polish/needs_review/needs_commit 等阶段拒绝重复 check。
	if err := RequireChapterAction(t.store, a.Chapter, ChapterActionCheck, t.fsmConfig); err != nil {
		return nil, fmt.Errorf("check_consistency: %w", err)
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

	// 角色受控状态：按当前章正文角色筛选（角色名/别名出现在本章草稿中的实体），
	// 同时做代码可确定的重复 key 校验（(entity, field) 唯一键重复 → 报告，其余交 LLM）。
	if entries, _ := t.store.World.LoadCharacterState(); len(entries) > 0 {
		inChapter := make(map[string]bool)
		if chars, _ := t.store.Characters.Load(); len(chars) > 0 {
			for _, c := range chars {
				if strings.Contains(content, c.Name) {
					inChapter[c.Name] = true
				}
				for _, alias := range c.Aliases {
					if strings.Contains(content, alias) {
						inChapter[c.Name] = true
					}
				}
			}
		}
		seenKey := make(map[string]struct{}, len(entries))
		var filtered []domain.CharacterStateEntry
		var issues []map[string]any
		for _, e := range entries {
			if inChapter[e.Entity] || strings.Contains(content, e.Entity) {
				filtered = append(filtered, e)
			}
			key := e.Entity + "\x00" + e.Field
			if _, dup := seenKey[key]; dup {
				issues = append(issues, map[string]any{
					"type": "duplicate_key", "entity": e.Entity, "field": e.Field,
				})
			}
			seenKey[key] = struct{}{}
		}
		if len(filtered) > 0 {
			result["character_state"] = filtered
		}
		if len(issues) > 0 {
			result["character_state_issues"] = issues
		}
	}

	// 当前章 plan/contract：让审阅对照本章承诺（required_beats/payoff 等）。
	if plan, _ := t.store.Drafts.LoadChapterPlan(a.Chapter); plan != nil {
		result["chapter_plan"] = plan
		result["chapter_contract"] = plan.Contract
	} else if entry, _ := t.store.Outline.GetChapterOutline(a.Chapter); entry != nil {
		result["current_chapter_outline"] = entry
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

	// ── 计算 required_next_action 辅助提示（唯一控制面：ChapterStage 决策） ──
	// checkpoint 成功追加后再解析阶段：guard 用的是追加前的快照（拦截重复 check），
	// 这里用追加后的快照（本次 check 已是状态事实）生成下一步建议。
	decision, err := ResolveChapterStage(t.store, a.Chapter, t.fsmConfig)
	if err != nil {
		// Store 读失败：不伪装正常阶段——写入 pipeline_state_error 并记录日志，
		// 主结果（digest/violations）仍然返回。
		slog.Warn("resolve chapter pipeline stage failed", "module", "tools", "chapter", a.Chapter, "err", err)
		result["pipeline_state_error"] = err.Error()
	} else if next := decision.RequiredNextAction(); next != nil {
		result["required_next_action"] = next
		if decision.DraftMode == "append" {
			if guidance, ok := underMinWordCountGuidance(t.store, wordCount); ok {
				result["word_count_guidance"] = guidance
			}
		}
	}

	return json.Marshal(result)
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
