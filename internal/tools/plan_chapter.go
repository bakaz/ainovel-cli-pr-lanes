package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

// PlanChapterTool 保存章节构思，Agent 自主决定规划粒度。
type PlanChapterTool struct {
	store    *store.Store
	contract *projectprofile.SceneBeatContract
}

func NewPlanChapterTool(store *store.Store, contract *projectprofile.SceneBeatContract) *PlanChapterTool {
	return &PlanChapterTool{store: store, contract: contract}
}

func (t *PlanChapterTool) Name() string { return "plan_chapter" }
func (t *PlanChapterTool) Description() string {
	return "保存章节写作构思。Agent 自主决定规划粒度，不强制场景拆分"
}
func (t *PlanChapterTool) Label() string { return "规划章节" }

func (t *PlanChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *PlanChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *PlanChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
		schema.Property("title", schema.String("章节标题")).Required(),
		schema.Property("goal", schema.String("本章目标")).Required(),
		schema.Property("conflict", schema.String("核心冲突")).Required(),
		schema.Property("hook", schema.String("章末钩子")).Required(),
		schema.Property("emotion_arc", schema.String("情绪曲线")),
		schema.Property("notes", schema.String("自由备忘（任何你觉得写作时需要记住的东西）")),
		schema.Property("required_beats", schema.Array("本章必须完成的推进项", schema.String(""))),
		schema.Property("forbidden_moves", schema.Array("本章明确不能发生的推进", schema.String(""))),
		schema.Property("continuity_checks", schema.Array("本章需特别核对的连续性点", schema.String(""))),
		schema.Property("evaluation_focus", schema.Array("Editor 重点检查项", schema.String(""))),
		schema.Property("emotion_target", schema.String("可选：本章希望读者主要感受到的情绪")),
		schema.Property("payoff_points", schema.Array("可选：关键章希望回应的情节点或兑现点", schema.String(""))),
		schema.Property("hook_goal", schema.String("可选：章末希望驱动的追读欲望或悬念目标")),
		schema.Property("style_goal", map[string]any{
			"type":        "object",
			"description": "本章风格目标：必须提供全部五个正向指导字段（每个字段 ≤200 字）。字段名固定：focal_filter, prose_movement, detail_strategy, rhythm, variation_from_recent",
			"properties": map[string]any{
				"focal_filter":          schema.String("视角/焦点过滤：POV 选择、信息披露策略（≤200 字）"),
				"prose_movement":        schema.String("叙述推进方式：场景流、过渡风格（≤200 字）"),
				"detail_strategy":       schema.String("细节密度策略：详略分配、感官侧重（≤200 字）"),
				"rhythm":                schema.String("节奏预期：句式变化、段落节奏（≤200 字）"),
				"variation_from_recent": schema.String("与近几章的差异化提示（≤200 字）"),
			},
			"required": []string{"focal_filter", "prose_movement", "detail_strategy", "rhythm", "variation_from_recent"},
		}).Required(),
	)
}

// Execute 首先验证目标章的 outline entry 场景符合契约（v3 拒绝 legacy），
// 验证通过后才进行任何写入。
func (t *PlanChapterTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	plan, err := decodeChapterPlanArgs(args)
	if err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if plan.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}

	// Oracle gate 1: style_goal 必须存在且全部五个字段非空（仅对新 plan_chapter 调用生效）。
	if plan.StyleGoal == nil {
		return nil, fmt.Errorf("style_goal is required: %w", errs.ErrToolArgs)
	}
	var emptyFields []string
	for name, val := range map[string]string{
		"focal_filter":          plan.StyleGoal.FocalFilter,
		"prose_movement":        plan.StyleGoal.ProseMovement,
		"detail_strategy":       plan.StyleGoal.DetailStrategy,
		"rhythm":                plan.StyleGoal.Rhythm,
		"variation_from_recent": plan.StyleGoal.VariationFromRecent,
	} {
		if strings.TrimSpace(val) == "" {
			emptyFields = append(emptyFields, "style_goal."+name)
		}
	}
	if len(emptyFields) > 0 {
		return nil, fmt.Errorf("%s 不能为空: %w", strings.Join(emptyFields, ", "), errs.ErrToolArgs)
	}
	if err := plan.StyleGoal.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", errs.ErrToolArgs, err)
	}

	// 预检：验证 outline 中该章的场景是否符合契约
	if t.contract != nil {
		if err := t.validateOutlineChapter(plan.Chapter); err != nil {
			return nil, err
		}
	}

	if t.store.Progress.IsChapterCompleted(plan.Chapter) {
		return json.Marshal(map[string]any{
			"chapter":   plan.Chapter,
			"skipped":   true,
			"completed": true,
			"reason":    fmt.Sprintf("第 %d 章已提交完成，不能重新规划", plan.Chapter),
		})
	}
	if err := t.store.Progress.ValidateChapterWork(plan.Chapter); err != nil {
		return nil, err
	}
	if err := EnsureChapterExpanded(t.store, plan.Chapter); err != nil {
		return nil, err
	}

	if err := t.store.Drafts.SaveChapterPlan(plan); err != nil {
		return nil, fmt.Errorf("save chapter plan: %w", err)
	}
	if err := t.store.Progress.StartChapter(plan.Chapter); err != nil {
		return nil, fmt.Errorf("mark chapter in progress: %w", err)
	}

	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(plan.Chapter), "plan",
		fmt.Sprintf("drafts/%02d.plan.json", plan.Chapter),
	); err != nil {
		return nil, fmt.Errorf("checkpoint chapter plan: %w", err)
	}

	return json.Marshal(map[string]any{
		"planned":   true,
		"chapter":   plan.Chapter,
		"next_step": "立即调用 draft_chapter(chapter=本章节号, content=完整正文字符串) 写入正文，不要重复规划同一章",
	})
}

// validateOutlineChapter 使用共享 ValidateOutlineEntry 校验目标章场景。
func (t *PlanChapterTool) validateOutlineChapter(chapter int) error {
	if err := ValidateOutlineEntry(t.store, t.contract, chapter); err != nil {
		return fmt.Errorf("plan_chapter: %w", err)
	}
	return nil
}

func decodeChapterPlanArgs(args json.RawMessage) (domain.ChapterPlan, error) {
	args = normalizeIntegerStringFields(args, "chapter")
	var a struct {
		Chapter          int                      `json:"chapter"`
		Title            string                   `json:"title"`
		Goal             string                   `json:"goal"`
		Conflict         string                   `json:"conflict"`
		Hook             string                   `json:"hook"`
		EmotionArc       string                   `json:"emotion_arc"`
		Notes            string                   `json:"notes"`
		RequiredBeats    []string                 `json:"required_beats"`
		ForbiddenMoves   []string                 `json:"forbidden_moves"`
		ContinuityChecks []string                 `json:"continuity_checks"`
		EvaluationFocus  []string                 `json:"evaluation_focus"`
		EmotionTarget    string                   `json:"emotion_target"`
		PayoffPoints     []string                 `json:"payoff_points"`
		HookGoal         string                   `json:"hook_goal"`
		StyleGoal        *domain.ChapterStyleGoal `json:"style_goal"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return domain.ChapterPlan{}, err
	}

	return domain.ChapterPlan{
		Chapter:    a.Chapter,
		Title:      a.Title,
		Goal:       a.Goal,
		Conflict:   a.Conflict,
		Hook:       a.Hook,
		EmotionArc: a.EmotionArc,
		Notes:      a.Notes,
		Contract: domain.ChapterContract{
			RequiredBeats:    a.RequiredBeats,
			ForbiddenMoves:   a.ForbiddenMoves,
			ContinuityChecks: a.ContinuityChecks,
			EvaluationFocus:  a.EvaluationFocus,
			EmotionTarget:    a.EmotionTarget,
			PayoffPoints:     a.PayoffPoints,
			HookGoal:         a.HookGoal,
		},
		StyleGoal: a.StyleGoal,
	}, nil
}
