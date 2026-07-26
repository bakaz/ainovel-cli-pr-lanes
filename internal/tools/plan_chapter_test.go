package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func planArgs(chapter int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"chapter":     chapter,
		"title":       "测试章",
		"goal":        "推进剧情",
		"conflict":    "外部阻力",
		"hook":        "留下悬念",
		"emotion_arc": "紧张到期待",
		"style_goal": map[string]any{
			"focal_filter":          "主角限知视角",
			"prose_movement":        "线性场景推进",
			"detail_strategy":       "战斗详写过渡略写",
			"rhythm":                "短句加快节奏",
			"variation_from_recent": "减少景物描写",
		},
	})
	return b
}

func TestPlanChapterRejectsUnexpandedLayeredChapter(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1,
		Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1,
			Title: "第一弧",
			Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "一"},
				{Chapter: 2, Title: "二"},
			},
		}, {
			Index:             2,
			Title:             "第二弧",
			EstimatedChapters: 3,
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := st.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewPlanChapterTool(st, testContract)
	if _, err := tool.Execute(context.Background(), planArgs(3)); err == nil || !strings.Contains(err.Error(), "expand_arc") {
		t.Fatalf("expected unexpanded chapter rejection, got %v", err)
	}
	if p, _ := st.Progress.Load(); p != nil && p.InProgressChapter == 3 {
		t.Fatal("unexpanded chapter should not become in-progress")
	}
}

func TestPlanChapterAllowsExpandedLayeredChapter(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 2); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1,
		Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1,
			Title: "第一弧",
			Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "一"},
				{Chapter: 2, Title: "二"},
			},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := st.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewPlanChapterTool(st, testContract)
	if _, err := tool.Execute(context.Background(), planArgs(2)); err != nil {
		t.Fatalf("expected expanded chapter to plan, got %v", err)
	}
}

// ── ChapterStyleGoal tests ──

func TestChapterStyleGoalRoundTrip(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "第一弧",
			Chapters: []domain.OutlineEntry{{Chapter: 1, Title: "一"}},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}

	csg := &domain.ChapterStyleGoal{
		FocalFilter:         "主角限知视角，只展示主角所见所想",
		ProseMovement:       "场景切换用空行过渡，保持叙述线性",
		DetailStrategy:      "重点描写战斗动作和环境感官，省略日常对话",
		Rhythm:              "短句为主，段落控制在 3-5 行",
		VariationFromRecent: "相比第 1 章减少景物描写，加快节奏",
	}
	input := map[string]any{
		"chapter":    1,
		"title":      "测试章",
		"goal":       "推进剧情",
		"conflict":   "外部阻力",
		"hook":       "留下悬念",
		"style_goal": csg,
	}

	tool := NewPlanChapterTool(st, testContract)
	b, _ := json.Marshal(input)
	if _, err := tool.Execute(context.Background(), b); err != nil {
		t.Fatalf("Execute with style_goal: %v", err)
	}

	// Load back from store
	loaded, err := st.Drafts.LoadChapterPlan(1)
	if err != nil {
		t.Fatalf("LoadChapterPlan: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded plan is nil")
	}
	if loaded.StyleGoal == nil {
		t.Fatal("StyleGoal is nil after round-trip")
	}
	if loaded.StyleGoal.FocalFilter != csg.FocalFilter {
		t.Errorf("FocalFilter: got %q, want %q", loaded.StyleGoal.FocalFilter, csg.FocalFilter)
	}
	if loaded.StyleGoal.ProseMovement != csg.ProseMovement {
		t.Errorf("ProseMovement: got %q, want %q", loaded.StyleGoal.ProseMovement, csg.ProseMovement)
	}
	if loaded.StyleGoal.DetailStrategy != csg.DetailStrategy {
		t.Errorf("DetailStrategy: got %q, want %q", loaded.StyleGoal.DetailStrategy, csg.DetailStrategy)
	}
	if loaded.StyleGoal.Rhythm != csg.Rhythm {
		t.Errorf("Rhythm: got %q, want %q", loaded.StyleGoal.Rhythm, csg.Rhythm)
	}
	if loaded.StyleGoal.VariationFromRecent != csg.VariationFromRecent {
		t.Errorf("VariationFromRecent: got %q, want %q", loaded.StyleGoal.VariationFromRecent, csg.VariationFromRecent)
	}
}

func TestChapterStyleGoalBackwardCompat(t *testing.T) {
	// Old plan JSON without style_goal must load and remain nil.
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	oldJSON := `{"chapter":1,"title":"旧版","goal":"g","conflict":"c","hook":"h"}`
	var plan domain.ChapterPlan
	if err := json.Unmarshal([]byte(oldJSON), &plan); err != nil {
		t.Fatalf("Unmarshal old plan: %v", err)
	}
	if plan.StyleGoal != nil {
		t.Fatal("StyleGoal should be nil for old plan JSON")
	}
	if plan.Chapter != 1 || plan.Title != "旧版" {
		t.Fatalf("old fields not preserved: %+v", plan)
	}

	// Also verify through store persistence
	if err := st.Drafts.SaveChapterPlan(plan); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	loaded, err := st.Drafts.LoadChapterPlan(1)
	if err != nil {
		t.Fatalf("LoadChapterPlan: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded plan is nil")
	}
	if loaded.StyleGoal != nil {
		t.Fatal("StyleGoal should be nil for persisted old plan")
	}
}

func TestChapterStyleGoalRejectsOverlongField(t *testing.T) {
	long := string(make([]byte, 300)) // 300 bytes, in ASCII = 300 chars
	csg := &domain.ChapterStyleGoal{FocalFilter: long}
	err := csg.Validate()
	if err == nil {
		t.Error("expected validation error for overlong field")
	}
}

func TestChapterStyleGoalAcceptsValidInput(t *testing.T) {
	csg := &domain.ChapterStyleGoal{
		FocalFilter:         "主角限知视角",
		ProseMovement:       "线性场景推进",
		DetailStrategy:      "战斗详写，过渡略写",
		Rhythm:              "短句加快节奏",
		VariationFromRecent: "减少景物描写",
	}
	if err := csg.Validate(); err != nil {
		t.Errorf("expected nil error for valid input, got %v", err)
	}
}

func TestChapterStyleGoalNilIsValid(t *testing.T) {
	var csg *domain.ChapterStyleGoal
	if err := csg.Validate(); err != nil {
		t.Errorf("nil style goal should be valid, got %v", err)
	}
}

func TestChapterStyleGoalEmptyFieldsAreValid(t *testing.T) {
	csg := &domain.ChapterStyleGoal{}
	if err := csg.Validate(); err != nil {
		t.Errorf("empty style goal should be valid, got %v", err)
	}
}

// ── Oracle gate 1: plan_chapter must require complete style_goal ──

func setupOneChapterStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "第一弧",
			Chapters: []domain.OutlineEntry{{Chapter: 1, Title: "一"}},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	return st
}

func validStyleGoal() map[string]any {
	return map[string]any{
		"focal_filter":          "主角限知视角",
		"prose_movement":        "线性场景推进",
		"detail_strategy":       "战斗详写过渡略写",
		"rhythm":                "短句加快节奏",
		"variation_from_recent": "减少景物描写",
	}
}

func TestPlanChapterRejectsMissingStyleGoal(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	input := map[string]any{
		"chapter":  1,
		"title":    "测试章",
		"goal":     "推进剧情",
		"conflict": "外部阻力",
		"hook":     "留下悬念",
	}
	b, _ := json.Marshal(input)
	_, err := tool.Execute(context.Background(), b)
	if err == nil {
		t.Fatal("expected error for missing style_goal")
	}
	if !strings.Contains(err.Error(), "style_goal is required") {
		t.Errorf("expected 'style_goal is required' error, got: %v", err)
	}
}

func TestPlanChapterRejectsBlankStyleGoalFields(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	input := map[string]any{
		"chapter":  1,
		"title":    "测试章",
		"goal":     "推进剧情",
		"conflict": "外部阻力",
		"hook":     "留下悬念",
		"style_goal": map[string]any{
			"focal_filter":          "主角限知视角",
			"prose_movement":        "",
			"detail_strategy":       "战斗详写",
			"rhythm":                "短句",
			"variation_from_recent": "减少描写",
		},
	}
	b, _ := json.Marshal(input)
	_, err := tool.Execute(context.Background(), b)
	if err == nil {
		t.Fatal("expected error for blank style_goal field")
	}
	if !strings.Contains(err.Error(), "style_goal.prose_movement") {
		t.Errorf("expected error mentioning blank prose_movement, got: %v", err)
	}
}

func TestPlanChapterRejectsBlankStyleGoalFieldWithWhitespace(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	input := map[string]any{
		"chapter":  1,
		"title":    "测试章",
		"goal":     "推进剧情",
		"conflict": "外部阻力",
		"hook":     "留下悬念",
		"style_goal": map[string]any{
			"focal_filter":          "   ",
			"prose_movement":        "线性场景",
			"detail_strategy":       "战斗详写",
			"rhythm":                "短句",
			"variation_from_recent": "减少描写",
		},
	}
	b, _ := json.Marshal(input)
	_, err := tool.Execute(context.Background(), b)
	if err == nil {
		t.Fatal("expected error for whitespace-only style_goal field")
	}
	if !strings.Contains(err.Error(), "style_goal.focal_filter") {
		t.Errorf("expected error mentioning blank focal_filter, got: %v", err)
	}
}

func TestPlanChapterRejectsPartialStyleGoal(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	// Only 3 of 5 fields provided
	input := map[string]any{
		"chapter":  1,
		"title":    "测试章",
		"goal":     "推进剧情",
		"conflict": "外部阻力",
		"hook":     "留下悬念",
		"style_goal": map[string]any{
			"focal_filter":    "主角限知视角",
			"prose_movement":  "线性场景推进",
			"detail_strategy": "战斗详写",
		},
	}
	b, _ := json.Marshal(input)
	_, err := tool.Execute(context.Background(), b)
	if err == nil {
		t.Fatal("expected error for partial style_goal (missing rhythm, variation_from_recent)")
	}
	if !strings.Contains(err.Error(), "style_goal.rhythm") || !strings.Contains(err.Error(), "style_goal.variation_from_recent") {
		t.Errorf("expected error mentioning missing fields, got: %v", err)
	}
}

func TestPlanChapterRejectsOverlongFieldViaExecute(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	long := strings.Repeat("长", 201)
	input := map[string]any{
		"chapter":  1,
		"title":    "测试章",
		"goal":     "推进剧情",
		"conflict": "外部阻力",
		"hook":     "留下悬念",
		"style_goal": map[string]any{
			"focal_filter":          long,
			"prose_movement":        "线性场景推进",
			"detail_strategy":       "战斗详写过渡略写",
			"rhythm":                "短句加快节奏",
			"variation_from_recent": "减少景物描写",
		},
	}
	b, _ := json.Marshal(input)
	_, err := tool.Execute(context.Background(), b)
	if err == nil {
		t.Fatal("expected error for overlong style_goal field")
	}
	if !strings.Contains(err.Error(), "style_goal.focal_filter") {
		t.Errorf("expected length error for focal_filter, got: %v", err)
	}
}

func TestPlanChapterAcceptsCompleteStyleGoal(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	input := map[string]any{
		"chapter":    1,
		"title":      "测试章",
		"goal":       "推进剧情",
		"conflict":   "外部阻力",
		"hook":       "留下悬念",
		"style_goal": validStyleGoal(),
	}
	b, _ := json.Marshal(input)
	if _, err := tool.Execute(context.Background(), b); err != nil {
		t.Fatalf("expected success for complete style_goal, got: %v", err)
	}
}

// TestPlanChapterAcceptsUnknownKeys 记录当前 decoder 的行为：未知字段静默忽略。
// 这是有意为之的合约——Go json.Unmarshal 默认忽略未知 key，
// 不额外增加 DisallowUnknownFields 以保持向后兼容。
func TestPlanChapterAcceptsUnknownKeys(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	input := map[string]any{
		"chapter":       1,
		"title":         "测试章",
		"goal":          "推进剧情",
		"conflict":      "外部阻力",
		"hook":          "留下悬念",
		"extra_unknown": "should be silently ignored",
		"style_goal":    validStyleGoal(),
	}
	b, _ := json.Marshal(input)
	if _, err := tool.Execute(context.Background(), b); err != nil {
		t.Fatalf("unknown keys must be silently accepted, got: %v", err)
	}
}

// TestPlanChapterAcceptsUnknownKeysInsideStyleGoal 记录 style_goal 内未知字段
// 同样被 Go decoder 静默忽略。如需要严格拒绝，需改用 json.NewDecoder + DisallowUnknownFields。
// 当前保持此合约以降低 LLM schema 同步风险。
func TestPlanChapterAcceptsUnknownKeysInsideStyleGoal(t *testing.T) {
	st := setupOneChapterStore(t)
	tool := NewPlanChapterTool(st, testContract)

	input := map[string]any{
		"chapter":  1,
		"title":    "测试章",
		"goal":     "推进剧情",
		"conflict": "外部阻力",
		"hook":     "留下悬念",
		"style_goal": map[string]any{
			"focal_filter":          "主角限知视角",
			"prose_movement":        "线性场景推进",
			"detail_strategy":       "战斗详写过渡略写",
			"rhythm":                "短句加快节奏",
			"variation_from_recent": "减少景物描写",
			"unknown_sub_field":     "should be silently ignored",
		},
	}
	b, _ := json.Marshal(input)
	if _, err := tool.Execute(context.Background(), b); err != nil {
		t.Fatalf("unknown keys inside style_goal must be silently accepted, got: %v", err)
	}
}

// ── Schema shape test: style_goal required + exact 5 sub-keys ──

func TestPlanChapterSchema_StyleGoalRequiredAndExactKeys(t *testing.T) {
	tool := NewPlanChapterTool(nil, nil)
	s := tool.Schema()

	// toSS converts any []string-like value to []string for reliable comparison.
	toSS := func(raw any) []string {
		switch v := raw.(type) {
		case []string:
			return v
		case []any:
			out := make([]string, len(v))
			for i, x := range v {
				out[i], _ = x.(string)
			}
			return out
		default:
			t.Fatalf("required field is %T, want []string or []any", raw)
			return nil
		}
	}

	// 1) Top-level "required" must include "style_goal"
	rawRequired, ok := s["required"]
	if !ok {
		t.Fatal("top-level schema missing 'required' array")
	}
	requiredList := toSS(rawRequired)
	found := false
	for _, r := range requiredList {
		if r == "style_goal" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("top-level required array %v does not contain 'style_goal'", requiredList)
	}

	// 2) style_goal sub-schema must have exactly the 5 required field keys
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing 'properties'")
	}
	sgRaw, ok := props["style_goal"]
	if !ok {
		t.Fatal("schema missing property 'style_goal'")
	}
	sg, ok := sgRaw.(map[string]any)
	if !ok {
		t.Fatalf("style_goal schema is %T, want map[string]any", sgRaw)
	}

	sgRequiredRaw, ok := sg["required"]
	if !ok {
		t.Fatal("style_goal sub-schema missing 'required' array")
	}
	sgRequired := toSS(sgRequiredRaw)

	if len(sgRequired) != 5 {
		t.Fatalf("style_goal.required has %d entries, want 5; got %v", len(sgRequired), sgRequired)
	}

	expectedKeys := []string{"focal_filter", "prose_movement", "detail_strategy", "rhythm", "variation_from_recent"}
	for i, key := range expectedKeys {
		if sgRequired[i] != key {
			t.Errorf("style_goal.required[%d] = %q, want %q; full list: %v", i, sgRequired[i], key, sgRequired)
		}
	}

	// 3) Verify no extra keys in the required list
	for _, key := range sgRequired {
		matched := false
		for _, exp := range expectedKeys {
			if key == exp {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("unexpected key %q in style_goal.required; expected only %v", key, expectedKeys)
		}
	}
}
