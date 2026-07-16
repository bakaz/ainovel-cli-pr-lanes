package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

// TestLoadEmpty 统一验证所有领域的空读取行为。
func TestLoadEmpty(t *testing.T) {
	s := newTestStore(t)

	if v, err := s.World.LoadTimeline(); err != nil || v != nil {
		t.Errorf("Timeline: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadForeshadowLedger(); err != nil || v != nil {
		t.Errorf("Foreshadow: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadRelationships(); err != nil || v != nil {
		t.Errorf("Relationships: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadStateChanges(); err != nil || v != nil {
		t.Errorf("StateChanges: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadStyleRules(); err != nil || v != nil {
		t.Errorf("StyleRules: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadWorldRules(); err != nil || v != nil {
		t.Errorf("WorldRules: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadReview(99); err != nil || v != nil {
		t.Errorf("Review: want (nil, nil), got (%v, %v)", v, err)
	}
	if v, err := s.World.LoadLastReview(10); err != nil || v != nil {
		t.Errorf("LastReview: want (nil, nil), got (%v, %v)", v, err)
	}
}

// ── Timeline ──

func TestTimeline_Append(t *testing.T) {
	s := newTestStore(t)

	if err := s.World.AppendTimelineEvents([]domain.TimelineEvent{
		{Chapter: 1, Time: "清晨", Event: "事件一"},
	}); err != nil {
		t.Fatalf("batch1: %v", err)
	}
	if err := s.World.AppendTimelineEvents([]domain.TimelineEvent{
		{Chapter: 2, Time: "午后", Event: "事件二"},
		{Chapter: 3, Time: "傍晚", Event: "事件三"},
	}); err != nil {
		t.Fatalf("batch2: %v", err)
	}

	loaded, err := s.World.LoadTimeline()
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("want 3, got %d", len(loaded))
	}
	if loaded[2].Event != "事件三" {
		t.Errorf("third event: %+v", loaded[2])
	}
}

func TestTimeline_AppendIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	event := domain.TimelineEvent{
		Chapter:    1,
		Time:       "清晨",
		Event:      "林墨入住客栈",
		Characters: []string{"林墨", "老周"},
	}
	if err := s.World.AppendTimelineEvents([]domain.TimelineEvent{event}); err != nil {
		t.Fatalf("append first: %v", err)
	}
	event.Characters = []string{"老周", "林墨"} // 角色顺序不应影响同一事件判定
	if err := s.World.AppendTimelineEvents([]domain.TimelineEvent{event}); err != nil {
		t.Fatalf("append duplicate: %v", err)
	}

	loaded, err := s.World.LoadTimeline()
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("duplicate timeline event should be ignored, got %d: %+v", len(loaded), loaded)
	}
}

func TestTimeline_LoadRecent(t *testing.T) {
	s := newTestStore(t)
	_ = s.World.SaveTimeline([]domain.TimelineEvent{
		{Chapter: 1}, {Chapter: 3}, {Chapter: 5}, {Chapter: 7},
	})

	for _, tt := range []struct {
		current, window, want int
	}{
		{7, 10, 4}, // 全部
		{7, 3, 2},  // ch5,ch7
		{5, 2, 3},  // ch3,ch5,ch7
	} {
		got, _ := s.World.LoadRecentTimeline(tt.current, tt.window)
		if len(got) != tt.want {
			t.Errorf("LoadRecent(%d,%d): want %d, got %d", tt.current, tt.window, tt.want, len(got))
		}
	}
}

// ── Foreshadow ──

func TestForeshadow_UpdateLifecycle(t *testing.T) {
	s := newTestStore(t)

	// plant
	_ = s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "plant", Description: "黑影"},
		{ID: "f2", Action: "plant", Description: "断剑"},
	})
	// advance f1, resolve f2
	_ = s.World.UpdateForeshadow(3, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "advance"},
		{ID: "f2", Action: "resolve"},
	})

	all, _ := s.World.LoadForeshadowLedger()
	if len(all) != 2 {
		t.Fatalf("want 2, got %d", len(all))
	}
	if all[0].Status != "advanced" {
		t.Errorf("f1: want advanced, got %s", all[0].Status)
	}
	if all[1].Status != "resolved" || all[1].ResolvedAt != 3 {
		t.Errorf("f2: want resolved@3, got %s@%d", all[1].Status, all[1].ResolvedAt)
	}

	// LoadActive 应排除 resolved
	active, _ := s.World.LoadActiveForeshadow()
	if len(active) != 1 || active[0].ID != "f1" {
		t.Errorf("active: want [f1], got %v", active)
	}
}

func TestForeshadow_PlantIsIdempotent(t *testing.T) {
	s := newTestStore(t)

	_ = s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "plant", Description: "黑影"},
	})
	_ = s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "plant", Description: "黑影"},
	})
	_ = s.World.UpdateForeshadow(3, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "advance"},
	})
	_ = s.World.UpdateForeshadow(3, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "plant", Description: "黑影"},
	})

	all, _ := s.World.LoadForeshadowLedger()
	if len(all) != 1 {
		t.Fatalf("duplicate plant should not append entries, got %d: %+v", len(all), all)
	}
	if all[0].Status != "advanced" {
		t.Fatalf("duplicate plant should not downgrade status, got %s", all[0].Status)
	}
}

// ── Relationships ──

func TestRelationships_UpdateMerge(t *testing.T) {
	s := newTestStore(t)
	_ = s.World.SaveRelationships([]domain.RelationshipEntry{
		{CharacterA: "张三", CharacterB: "李四", Relation: "师徒", Chapter: 1},
	})

	// 更新已有 + 新增
	_ = s.World.UpdateRelationships([]domain.RelationshipEntry{
		{CharacterA: "张三", CharacterB: "李四", Relation: "挚友", Chapter: 5},
		{CharacterA: "王五", CharacterB: "赵六", Relation: "同门", Chapter: 5},
	})

	loaded, _ := s.World.LoadRelationships()
	if len(loaded) != 2 {
		t.Fatalf("want 2, got %d", len(loaded))
	}
	if loaded[0].Relation != "挚友" {
		t.Errorf("update failed: %+v", loaded[0])
	}
}

func TestRelationships_PairKeySymmetry(t *testing.T) {
	s := newTestStore(t)
	_ = s.World.SaveRelationships([]domain.RelationshipEntry{
		{CharacterA: "张三", CharacterB: "李四", Relation: "师徒", Chapter: 1},
	})
	// B-A 顺序更新，应匹配同一条
	_ = s.World.UpdateRelationships([]domain.RelationshipEntry{
		{CharacterA: "李四", CharacterB: "张三", Relation: "反目", Chapter: 3},
	})

	loaded, _ := s.World.LoadRelationships()
	if len(loaded) != 1 {
		t.Fatalf("want 1 (merged), got %d", len(loaded))
	}
	if loaded[0].Relation != "反目" {
		t.Errorf("not updated: %+v", loaded[0])
	}
}

// ── StateChanges ──

func TestStateChanges_Append(t *testing.T) {
	s := newTestStore(t)
	_ = s.World.AppendStateChanges([]domain.StateChange{
		{Chapter: 1, Entity: "张三", Field: "realm", NewValue: "练气期"},
	})
	_ = s.World.AppendStateChanges([]domain.StateChange{
		{Chapter: 3, Entity: "张三", Field: "realm", OldValue: "练气期", NewValue: "筑基期"},
	})

	loaded, _ := s.World.LoadStateChanges()
	if len(loaded) != 2 {
		t.Fatalf("want 2, got %d", len(loaded))
	}
	if loaded[1].NewValue != "筑基期" {
		t.Errorf("second: %+v", loaded[1])
	}
}

func TestStateChanges_AppendIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	change := domain.StateChange{
		Chapter:  1,
		Entity:   "张三",
		Field:    "realm",
		OldValue: "凡人",
		NewValue: "练气期",
	}
	_ = s.World.AppendStateChanges([]domain.StateChange{change})
	_ = s.World.AppendStateChanges([]domain.StateChange{change})

	loaded, _ := s.World.LoadStateChanges()
	if len(loaded) != 1 {
		t.Fatalf("duplicate state change should be ignored, got %d: %+v", len(loaded), loaded)
	}
}

// ── StyleRules ──

func TestStyleRules_SaveAndLoad(t *testing.T) {
	s := newTestStore(t)
	rules := domain.WritingStyleRules{
		Volume: 1, Arc: 2,
		Prose:    []string{"短句为主"},
		Dialogue: []domain.CharacterVoice{{Name: "张三", Rules: []string{"粗犷"}}},
		Taboos:   []string{"不用网络用语"},
	}
	_ = s.World.SaveStyleRules(rules)

	loaded, _ := s.World.LoadStyleRules()
	if loaded == nil || loaded.Volume != 1 || len(loaded.Dialogue) != 1 {
		t.Errorf("roundtrip failed: %+v", loaded)
	}
}

func TestStyleRulesCompass_SaveCurrentPreservesLong(t *testing.T) {
	s := newTestStore(t)

	// 先保存 long 层（prose + taboos）
	longRules := domain.StyleRulesLong{
		Prose:  []string{"跨弧基线"},
		Taboos: []string{"全局禁忌"},
	}
	if err := s.World.SaveStyleRulesLong(longRules, "test: initial long setup"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// 保存 current 层（只有 dialogue，不与 long 的 prose/taboos 冲突）
	currentRules := domain.StyleRulesCurrent{
		Volume:      1,
		Arc:         1,
		Dialogue:    []domain.CharacterVoice{{Name: "李四", Rules: []string{"直白"}}},
		LastUpdated: "2024-01-01T00:00:00Z",
	}
	if err := s.World.SaveStyleRulesCurrent(currentRules); err != nil {
		t.Fatalf("SaveStyleRulesCurrent: %v", err)
	}

	// 验证 long 层仍然保留
	compass, err := s.World.LoadStyleRulesCompass()
	if err != nil {
		t.Fatalf("LoadStyleRulesCompass: %v", err)
	}
	if compass.Long == nil || len(compass.Long.Prose) != 1 || compass.Long.Prose[0] != "跨弧基线" {
		t.Fatalf("long not preserved after current save: %+v", compass.Long)
	}
	if compass.Current == nil || len(compass.Current.Dialogue) != 1 {
		t.Fatalf("current not saved: %+v", compass.Current)
	}
	if compass.Current.Volume != 1 || compass.Current.Arc != 1 {
		t.Fatalf("volume/arc not preserved: %+v", compass.Current)
	}
	// long 的 taboos 应保持不变
	if len(compass.Long.Taboos) != 1 || compass.Long.Taboos[0] != "全局禁忌" {
		t.Fatalf("long taboos lost: %+v", compass.Long.Taboos)
	}
}

func TestStyleRulesCompass_DifferentCurrentProseAllowed(t *testing.T) {
	s := newTestStore(t)

	// 先保存 long 层
	longRules := domain.StyleRulesLong{
		Prose:  []string{"保持短句"},
		Taboos: []string{"避免解释"},
	}
	if err := s.World.SaveStyleRulesLong(longRules, "test: initial long setup"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// 不同的 prose 在 current 中目前不被视为硬冲突
	//（由 long 优先的上下文合并保证一致性）
	current := domain.StyleRulesCurrent{
		Volume: 1,
		Arc:    2,
		Prose:  []string{"用长句描写"},
	}
	err := s.World.SaveStyleRulesCurrent(current)
	if err != nil {
		t.Fatalf("different current prose should be allowed, got: %v", err)
	}

	// 验证 long 层未被覆盖
	compass, _ := s.World.LoadStyleRulesCompass()
	if compass.Long == nil || compass.Long.Prose[0] != "保持短句" {
		t.Fatal("long prose should be preserved")
	}
	if compass.Current == nil || compass.Current.Prose[0] != "用长句描写" {
		t.Fatal("current prose should be saved")
	}
}

func TestStyleRulesCompass_AllowNonConflictCurrent(t *testing.T) {
	s := newTestStore(t)

	// 先保存 long 层
	longRules := domain.StyleRulesLong{
		Prose:  []string{"保持短句"},
		Taboos: []string{"避免解释"},
	}
	if err := s.World.SaveStyleRulesLong(longRules, "test: initial long setup"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// current 只有 dialogue（long 没有 dialogue）→ 无冲突
	currentRules := domain.StyleRulesCurrent{
		Volume: 1,
		Arc:    1,
		Dialogue: []domain.CharacterVoice{
			{Name: "主角", Rules: []string{"话少"}},
		},
		LastUpdated: "2024-01-01T00:00:00Z",
	}
	if err := s.World.SaveStyleRulesCurrent(currentRules); err != nil {
		t.Fatalf("SaveStyleRulesCurrent should succeed when no conflict, got: %v", err)
	}

	compass, _ := s.World.LoadStyleRulesCompass()
	if compass.Current == nil || len(compass.Current.Dialogue) != 1 {
		t.Fatal("current dialogue not saved")
	}
	// long 不受影响
	if compass.Long.Prose[0] != "保持短句" {
		t.Fatal("long prose modified by current save")
	}
}

func TestStyleRulesCompass_RejectLongWithForbiddenContent(t *testing.T) {
	s := newTestStore(t)

	tests := []struct {
		name string
		long domain.StyleRulesLong
	}{
		{"round pattern", domain.StyleRulesLong{Prose: []string{"第3轮战斗要激烈"}}},
		{"loop count", domain.StyleRulesLong{Prose: []string{"循环次数不超过3次"}}},
		{"score algorithm", domain.StyleRulesLong{Prose: []string{"积分算法采用加权平均"}}},
		{"parameter threshold", domain.StyleRulesLong{Prose: []string{"精神力阈值超过50时"}}},
		{"config parameter", domain.StyleRulesLong{Prose: []string{"参数配置要合理"}}},
		{"forbidden in dialogue", domain.StyleRulesLong{
			Dialogue: []domain.CharacterVoice{{Name: "角色", Rules: []string{"第2轮对白要简短"}}},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.World.SaveStyleRulesLong(tc.long, "test: verify forbidden content")
			if err == nil {
				t.Fatal("expected long validation error, got nil")
			}
			t.Logf("validation error: %v", err)
		})
	}
}

func TestStyleRulesCompass_AllowValidLong(t *testing.T) {
	s := newTestStore(t)

	validLong := domain.StyleRulesLong{
		Prose:  []string{"保持冷峻克制", "多用短句推进"},
		Taboos: []string{"避免直白解释"},
		Dialogue: []domain.CharacterVoice{
			{Name: "主角", Rules: []string{"话少沉稳"}},
		},
	}
	if err := s.World.SaveStyleRulesLong(validLong, "test: valid long baseline"); err != nil {
		t.Fatalf("valid long should be accepted, got: %v", err)
	}
}

func TestStyleRulesCompass_LoadLegacyFormat(t *testing.T) {
	s := newTestStore(t)

	// 直接写入旧单体格式的文件
	legacyJSON := `{"volume":3,"arc":1,"prose":["旧格式规则"],"updated_at":"2024-06-01T00:00:00Z"}`
	legacyPath := filepath.Join(s.Dir(), "meta", "style_rules.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyJSON), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	// 通过 compass 读取 → 自动迁移
	compass, err := s.World.LoadStyleRulesCompass()
	if err != nil {
		t.Fatalf("LoadStyleRulesCompass: %v", err)
	}
	if compass == nil || compass.Current == nil {
		t.Fatal("compass should have current from legacy migration")
	}
	if compass.Current.Volume != 3 || compass.Current.Arc != 1 {
		t.Fatalf("volume/arc from legacy: %+v", compass.Current)
	}
	if len(compass.Current.Prose) != 1 || compass.Current.Prose[0] != "旧格式规则" {
		t.Fatalf("prose from legacy: %+v", compass.Current.Prose)
	}
	if compass.Long != nil {
		t.Fatal("legacy should not populate long")
	}

	// 通过旧 API 读取也应能获取数据
	oldRules, err := s.World.LoadStyleRules()
	if err != nil {
		t.Fatalf("LoadStyleRules after migration: %v", err)
	}
	if oldRules == nil || oldRules.Volume != 3 || len(oldRules.Prose) != 1 {
		t.Fatalf("LoadStyleRules after migration: %+v", oldRules)
	}
}

// ── Reviews ──

func TestReview_SaveAndLoad(t *testing.T) {
	s := newTestStore(t)
	_ = s.World.SaveReview(domain.ReviewEntry{Chapter: 3, Scope: "chapter", Verdict: "polish"})

	loaded, _ := s.World.LoadReview(3)
	if loaded == nil || loaded.Verdict != "polish" {
		t.Errorf("chapter review: %+v", loaded)
	}
}

func TestReview_GlobalScopeIsolation(t *testing.T) {
	s := newTestStore(t)
	_ = s.World.SaveReview(domain.ReviewEntry{Chapter: 5, Scope: "global", Verdict: "accept"})

	// chapter-scoped load 不应找到 global review
	if got, _ := s.World.LoadReview(5); got != nil {
		t.Errorf("chapter load should not find global: %+v", got)
	}
}

func TestReview_LoadLastReview(t *testing.T) {
	s := newTestStore(t)
	for _, ch := range []int{2, 5, 8} {
		_ = s.World.SaveReview(domain.ReviewEntry{Chapter: ch, Scope: "global", Verdict: "accept"})
	}

	for _, tt := range []struct {
		from, want int
	}{
		{10, 8}, {5, 5}, {3, 2},
	} {
		got, _ := s.World.LoadLastReview(tt.from)
		if got == nil || got.Chapter != tt.want {
			t.Errorf("LoadLastReview(%d): want ch%d, got %+v", tt.from, tt.want, got)
		}
	}
	// from=1 找不到
	if got, _ := s.World.LoadLastReview(1); got != nil {
		t.Errorf("from=1 should be nil, got %+v", got)
	}
}

// ── WorldRules ──

func TestWorldRules_SaveAndLoad(t *testing.T) {
	s := newTestStore(t)
	rules := []domain.WorldRule{
		{Category: "magic", Rule: "法术消耗精神力", Boundary: "精神力耗尽会昏迷"},
		{Category: "society", Rule: "贵族拥有裁判权", Boundary: "不得越权"},
	}
	_ = s.World.SaveWorldRules(rules)

	if _, err := os.Stat(filepath.Join(s.Dir(), "world_rules.json")); err != nil {
		t.Fatalf("json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "world_rules.md")); err != nil {
		t.Fatalf("md not created: %v", err)
	}

	loaded, _ := s.World.LoadWorldRules()
	if len(loaded) != 2 || loaded[0].Rule != "法术消耗精神力" {
		t.Errorf("roundtrip: %+v", loaded)
	}
}

func TestRenderWorldRules(t *testing.T) {
	md := renderWorldRules([]domain.WorldRule{
		{Category: "magic", Rule: "法术消耗精神力", Boundary: "精神力耗尽会昏迷"},
		{Category: "society", Rule: "贵族有裁判权"},
		{Category: "magic", Rule: "禁咒需三人", Boundary: "单人施放会死"},
	})

	// magic 分组应在 society 之前
	if strings.Index(md, "## magic") >= strings.Index(md, "## society") {
		t.Error("magic should appear before society")
	}
	if !strings.Contains(md, "边界：精神力耗尽会昏迷") {
		t.Error("missing boundary")
	}
	// 无 boundary 不应输出空边界行
	if strings.Contains(md, "边界：\n") {
		t.Error("empty boundary rendered")
	}
}

func TestStyleRulesCompass_SaveStyleRulesPreservesLong(t *testing.T) {
	s := newTestStore(t)

	// 先保存 long 层
	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose: []string{"长期基线"},
	}, "test: initial long"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// 用旧 API 写入
	if err := s.World.SaveStyleRules(domain.WritingStyleRules{
		Volume: 2, Arc: 1,
		Prose:     []string{"旧 API 写入"},
		Taboos:    []string{"旧 API 禁忌"},
		UpdatedAt: "2024-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SaveStyleRules: %v", err)
	}

	// long 层应保留
	compass, err := s.World.LoadStyleRulesCompass()
	if err != nil {
		t.Fatalf("LoadStyleRulesCompass: %v", err)
	}
	if compass.Long == nil || len(compass.Long.Prose) != 1 || compass.Long.Prose[0] != "长期基线" {
		t.Fatalf("long preserved after compatible SaveStyleRules: %+v", compass.Long)
	}
	if compass.Current == nil || compass.Current.Prose[0] != "旧 API 写入" {
		t.Fatalf("current from compatible SaveStyleRules: %+v", compass.Current)
	}
	if compass.Current.Volume != 2 || compass.Current.Arc != 1 {
		t.Fatalf("volume/arc mismatch: %+v", compass.Current)
	}
}

func TestStyleRulesCompass_LongOnlyLoadStyleRulesNotNil(t *testing.T) {
	s := newTestStore(t)

	// 只保存 long 层
	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose:  []string{"仅有 long"},
		Taboos: []string{"long 禁忌"},
	}, "test: long-only baseline"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// LoadStyleRules（旧 API）应返回非 nil 结果
	rules, err := s.World.LoadStyleRules()
	if err != nil {
		t.Fatalf("LoadStyleRules: %v", err)
	}
	if rules == nil {
		t.Fatal("LoadStyleRules should return non-nil for long-only compass")
	}
	if len(rules.Prose) != 1 || rules.Prose[0] != "仅有 long" {
		t.Fatalf("prose from long-only: %+v", rules.Prose)
	}
	if len(rules.Taboos) != 1 || rules.Taboos[0] != "long 禁忌" {
		t.Fatalf("taboos from long-only: %+v", rules.Taboos)
	}
	// Volume/Arc 等应为零值（long 没有这些字段）
	if rules.Volume != 0 || rules.Arc != 0 {
		t.Fatalf("volume/arc should be zero for long-only: %+v", rules)
	}
}

func TestStyleRulesCompass_LongFieldMerge(t *testing.T) {
	s := newTestStore(t)

	// 初始 long 层：prose + taboos
	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose:  []string{"长期prose"},
		Taboos: []string{"长期taboos"},
	}, "test: initial long"); err != nil {
		t.Fatalf("initial SaveStyleRulesLong: %v", err)
	}

	// 字段级合并：只更新 prose，taboos 应保留
	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose: []string{"更新后prose"},
	}, "test: field merge update"); err != nil {
		t.Fatalf("merge SaveStyleRulesLong: %v", err)
	}

	compass, err := s.World.LoadStyleRulesCompass()
	if err != nil {
		t.Fatalf("LoadStyleRulesCompass: %v", err)
	}
	if compass.Long.Prose[0] != "更新后prose" {
		t.Fatalf("prose should be updated: %+v", compass.Long.Prose)
	}
	if len(compass.Long.Taboos) != 1 || compass.Long.Taboos[0] != "长期taboos" {
		t.Fatalf("taboos should be preserved: %+v", compass.Long.Taboos)
	}
}

func TestStyleRulesCompass_LoadStyleRulesMergesLongAndCurrent(t *testing.T) {
	s := newTestStore(t)

	// long: prose only
	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose: []string{"long prose"},
	}, "test: long with prose only"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// current: taboos + dialogue (long has no taboos/dialogue)
	if err := s.World.SaveStyleRulesCurrent(domain.StyleRulesCurrent{
		Volume:      1,
		Arc:         2,
		Taboos:      []string{"current taboos"},
		Dialogue:    []domain.CharacterVoice{{Name: "角色", Rules: []string{"current对话"}}},
		LastUpdated: "2024-02-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SaveStyleRulesCurrent: %v", err)
	}

	// LoadStyleRules should merge: prose from long, taboos/dialogue from current
	rules, err := s.World.LoadStyleRules()
	if err != nil {
		t.Fatalf("LoadStyleRules: %v", err)
	}
	if rules == nil {
		t.Fatal("LoadStyleRules should not be nil")
	}
	if len(rules.Prose) != 1 || rules.Prose[0] != "long prose" {
		t.Fatalf("prose should be from long: %+v", rules.Prose)
	}
	if len(rules.Taboos) != 1 || rules.Taboos[0] != "current taboos" {
		t.Fatalf("taboos should be from current: %+v", rules.Taboos)
	}
	if len(rules.Dialogue) != 1 || rules.Dialogue[0].Name != "角色" {
		t.Fatalf("dialogue should be from current: %+v", rules.Dialogue)
	}
	if rules.Volume != 1 || rules.Arc != 2 {
		t.Fatalf("volume/arc from current: %d/%d", rules.Volume, rules.Arc)
	}
	if rules.UpdatedAt != "2024-02-01T00:00:00Z" {
		t.Fatalf("updated_at from current: %s", rules.UpdatedAt)
	}
}

// ── Reason 与合并视图相关测试 ──

func TestStyleRulesCompass_RejectLongWithoutReason(t *testing.T) {
	s := newTestStore(t)

	err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose: []string{"规则"},
	}, "")
	if err == nil {
		t.Fatal("expected error for empty reason, got nil")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Fatalf("expected reason validation error, got: %v", err)
	}
}

func TestStyleRulesCompass_LongPersistsReason(t *testing.T) {
	s := newTestStore(t)

	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose: []string{"保持克制"},
	}, "用户要求调整文风：更简洁"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	compass, err := s.World.LoadStyleRulesCompass()
	if err != nil {
		t.Fatalf("LoadStyleRulesCompass: %v", err)
	}
	if compass.Long == nil {
		t.Fatal("long should not be nil")
	}
	if compass.Long.Reason != "用户要求调整文风：更简洁" {
		t.Fatalf("reason mismatch: %q", compass.Long.Reason)
	}
	if compass.Long.LastUpdated == "" {
		t.Fatal("last_updated should be set")
	}
}

func TestStyleRulesCompass_LoadMergesLongAndCurrentNonDuplicate(t *testing.T) {
	s := newTestStore(t)

	// long: prose (A, B), taboos (X)
	if err := s.World.SaveStyleRulesLong(domain.StyleRulesLong{
		Prose:  []string{"A", "B"},
		Taboos: []string{"X"},
		Dialogue: []domain.CharacterVoice{
			{Name: "角色甲", Rules: []string{"甲long规则"}},
		},
	}, "test: long baseline"); err != nil {
		t.Fatalf("SaveStyleRulesLong: %v", err)
	}

	// current: prose (B, C) — B 重复, C 新增; taboos (Y) — 全新; dialogue: 同角色甲不同规则 + 角色乙
	if err := s.World.SaveStyleRulesCurrent(domain.StyleRulesCurrent{
		Volume: 1, Arc: 1,
		Prose:  []string{"B", "C"},
		Taboos: []string{"Y"},
		Dialogue: []domain.CharacterVoice{
			{Name: "角色甲", Rules: []string{"甲current规则"}},
			{Name: "角色乙", Rules: []string{"乙规则"}},
		},
	}); err != nil {
		t.Fatalf("SaveStyleRulesCurrent: %v", err)
	}

	// LoadStyleRules 应合并：
	//   prose: A, B, C（long 优先，C 追加）
	//   taboos: X, Y（long 优先，Y 追加）
	//   dialogue: 角色甲(long优先), 角色乙(current 独有)
	rules, err := s.World.LoadStyleRules()
	if err != nil {
		t.Fatalf("LoadStyleRules: %v", err)
	}
	if rules == nil {
		t.Fatal("LoadStyleRules returned nil")
	}

	// prose: long 优先 + current 非重复
	if len(rules.Prose) != 3 {
		t.Fatalf("expected 3 prose (A,B,C), got %v", rules.Prose)
	}
	if rules.Prose[0] != "A" || rules.Prose[1] != "B" || rules.Prose[2] != "C" {
		t.Fatalf("prose order/contents wrong: %v", rules.Prose)
	}

	// taboos: long 优先 + current 非重复
	if len(rules.Taboos) != 2 {
		t.Fatalf("expected 2 taboos (X,Y), got %v", rules.Taboos)
	}
	if rules.Taboos[0] != "X" || rules.Taboos[1] != "Y" {
		t.Fatalf("taboos order/contents wrong: %v", rules.Taboos)
	}

	// dialogue: 角色甲 long 优先，角色乙从 current
	if len(rules.Dialogue) != 2 {
		t.Fatalf("expected 2 dialogue entries (角色甲 long + 角色乙), got %d: %+v", len(rules.Dialogue), rules.Dialogue)
	}
	if rules.Dialogue[0].Name != "角色甲" || rules.Dialogue[0].Rules[0] != "甲long规则" {
		t.Fatalf("角色甲 should be from long: %+v", rules.Dialogue[0])
	}
	if rules.Dialogue[1].Name != "角色乙" || rules.Dialogue[1].Rules[0] != "乙规则" {
		t.Fatalf("角色乙 should be appended from current: %+v", rules.Dialogue[1])
	}
}

// TestRuleViolationsContract 违规事实存储契约(第五轮评审):
// 同章最新覆盖旧记录;重写后空列表视为已清;跨重启可读。
func TestRuleViolationsContract(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.World.SaveRuleViolations(3, []rules.Violation{
		{Rule: "fatigue_words", Target: "不禁", Actual: 9, Severity: rules.SeverityWarning},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := s.World.LoadRuleViolations(3); len(got) != 1 || got[0].Target != "不禁" {
		t.Fatalf("首次读取: %+v", got)
	}

	// 同章重写:最新记录(空列表=已清)覆盖旧违规
	if err := s.World.SaveRuleViolations(3, nil); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	if got := s.World.LoadRuleViolations(3); len(got) != 0 {
		t.Fatalf("重写后旧违规应被清除: %+v", got)
	}

	// 其他章不受影响 + 跨重启(新 Store 实例)可读
	if err := s.World.SaveRuleViolations(5, []rules.Violation{{Rule: "forbidden_phrases", Target: "某种程度上", Actual: 2, Severity: rules.SeverityWarning}}); err != nil {
		t.Fatalf("save ch5: %v", err)
	}
	s2 := NewStore(dir)
	if got := s2.World.LoadRuleViolations(5); len(got) != 1 || got[0].Rule != "forbidden_phrases" {
		t.Fatalf("跨重启读取: %+v", got)
	}
	if got := s2.World.LoadRuleViolations(99); got != nil {
		t.Fatalf("无记录章节应返回 nil: %+v", got)
	}
}
