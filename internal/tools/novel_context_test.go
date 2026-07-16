package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestContextToolSelectsRoleRuleView(t *testing.T) {
	st := store.NewStore(t.TempDir())
	snap := rules.BuildSnapshot([]rules.Candidate{
		{Source: "d", Preferences: "共同", Scope: rules.ScopeDefault},
		{Source: "a", Preferences: "规划", Scope: rules.ScopeArchitect},
		{Source: "w", Preferences: "正文", Scope: rules.ScopeWriter},
		{Source: "e", Preferences: "审阅", Scope: rules.ScopeEditor},
	})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ yes, no []string }{
		"coordinator": {[]string{"共同"}, []string{"规划", "正文", "审阅"}},
		"architect":   {[]string{"共同", "规划"}, []string{"正文", "审阅"}},
		"writer":      {[]string{"共同", "正文"}, []string{"规划", "审阅"}},
		"editor":      {[]string{"共同", "正文", "审阅"}, []string{"规划"}},
	}
	for role, want := range cases {
		result := map[string]any{}
		NewContextToolForRole(st, References{}, "default", role).buildUserRules(result)
		working := result["working_memory"].(map[string]any)
		payload := working["user_rules"].(map[string]any)
		text := payload["preferences"].(string)
		for _, item := range want.yes {
			if !strings.Contains(text, item) {
				t.Fatalf("%s 应看到 %q: %q", role, item, text)
			}
		}
		for _, item := range want.no {
			if strings.Contains(text, item) {
				t.Fatalf("%s 不应看到 %q: %q", role, item, text)
			}
		}
	}
}

func TestContextSoftBudgetForRole(t *testing.T) {
	for _, role := range []string{"architect"} {
		if got := contextSoftBudgetForRole(role); got != 500*1024 {
			t.Fatalf("role=%s budget=%d", role, got)
		}
	}
	for _, role := range []string{"coordinator", "writer", "editor"} {
		if got := contextSoftBudgetForRole(role); got != 200*1024 {
			t.Fatalf("role=%s budget=%d", role, got)
		}
	}
}

func TestContextToolCompassAndLongReferenceVisibleByRole(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			Reference:       json.RawMessage(`{"schema":"long-reference.v1"}`),
		},
		Current: &domain.Compass{Direction: "近期"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role        string
		chapter     int
		wantCompass bool
		wantRef     bool
	}{
		{role: "architect", wantCompass: true, wantRef: false},
		{role: "coordinator", wantCompass: true, wantRef: false},
		{role: "editor", chapter: 1, wantCompass: true, wantRef: false},
		{role: "writer", chapter: 1, wantCompass: false, wantRef: false},
	} {
		args, _ := json.Marshal(map[string]any{"chapter": tc.chapter})
		out, err := NewContextToolForRole(st, References{}, "default", tc.role).Execute(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		planning, _ := got["planning_memory"].(map[string]any)
		compass, exists := planning["compass"].(map[string]any)
		if exists != tc.wantCompass {
			t.Fatalf("role=%s compass exists=%v want=%v", tc.role, exists, tc.wantCompass)
		}
		if !exists {
			continue
		}
		long, _ := compass["long"].(map[string]any)
		_, hasRef := long["reference"]
		if hasRef != tc.wantRef {
			t.Fatalf("role=%s long reference exists=%v want=%v", tc.role, hasRef, tc.wantRef)
		}
	}
}

func TestContextToolInjectsStyleStats(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	progress := &domain.Progress{TotalChapters: 10}
	body := "# 第N章\n他不是迟疑，而是恐惧。沉默了几息。像一道光。\n夜色落下。\n他走了。"
	for ch := 1; ch <= 6; ch++ {
		if err := st.Drafts.SaveFinalChapter(ch, body); err != nil {
			t.Fatalf("SaveFinalChapter: %v", err)
		}
		progress.CompletedChapters = append(progress.CompletedChapters, ch)
	}
	if err := st.Progress.Save(progress); err != nil {
		t.Fatalf("Save progress: %v", err)
	}

	tool := NewContextToolForRole(st, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 7})
	raw, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload struct {
		Episodic map[string]json.RawMessage `json:"episodic_memory"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	statsRaw, ok := payload.Episodic["style_stats"]
	if !ok {
		t.Fatalf("expected episodic_memory.style_stats, got keys %v", keysOf(payload.Episodic))
	}
	var stats struct {
		Chapters int `json:"chapters"`
		Patterns []struct {
			Name  string `json:"name"`
			Total int    `json:"total"`
		} `json:"patterns"`
	}
	if err := json.Unmarshal(statsRaw, &stats); err != nil {
		t.Fatalf("Unmarshal stats: %v", err)
	}
	if stats.Chapters != 6 || len(stats.Patterns) == 0 {
		t.Errorf("stats content: %+v", stats)
	}
	if usage, ok := payload.Episodic["_usage"]; !ok || len(usage) == 0 {
		t.Error("expected episodic_memory._usage annotation")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestContextToolReportsWarningsForCorruptedState(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "outline.json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("write outline.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("write progress.json: %v", err)
	}

	tool := NewContextToolForRole(store, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload struct {
		Warnings []string `json:"_warnings"`
		Summary  string   `json:"_loading_summary"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(payload.Warnings) == 0 {
		t.Fatal("expected context warnings for corrupted files")
	}
	if !containsWarning(payload.Warnings, "outline") {
		t.Fatalf("expected outline warning, got %v", payload.Warnings)
	}
	if !containsWarning(payload.Warnings, "progress") {
		t.Fatalf("expected progress warning, got %v", payload.Warnings)
	}
	if !strings.Contains(payload.Summary, "告警:") {
		t.Fatalf("expected loading summary to contain warning count, got %q", payload.Summary)
	}
}

func containsWarning(warnings []string, key string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, key) {
			return true
		}
	}
	return false
}

func TestContextToolChapterModeIncludesWorkingAndReferenceFields(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Outline.SavePremise(`## 题材和基调
少年成长，偏紧张压迫。

## 题材定位
少年升级流

## 核心冲突
主角必须在宗门竞争中活下来。

## 主角目标
进入内门。

## 终局方向
成为真正的执棋者。

## 写作禁区
不提前揭露师尊真相。

## 差异化卖点
弱者逆袭。

## 差异化钩子
每阶段都要用更高代价换成长。

## 核心兑现承诺
持续兑现危机与突破。

## 故事引擎
试炼、资源争夺与身份升级共同推进。

## 中段转折
主角被迫转向另一条修行路线。
`); err != nil {
		t.Fatalf("SavePremise: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "入门", CoreEvent: "主角进入宗门", Scenes: []string{"拜师", "立誓"}},
		{Chapter: 2, Title: "试炼", CoreEvent: "参加外门试炼", Scenes: []string{"集合", "出发"}},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Characters.Save([]domain.Character{
		{Name: "林砚", Role: "主角", Description: "少年修士", Arc: "成长", Traits: []string{"冷静"}},
	}); err != nil {
		t.Fatalf("SaveCharacters: %v", err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{
		{Category: "magic", Rule: "灵气可以炼化", Boundary: "凡人不可直接驾驭"},
	}); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}
	if err := s.Progress.Init("test", 2); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Summaries.SaveSummary(domain.ChapterSummary{
		Chapter:    1,
		Summary:    "主角拜入宗门，确立目标。",
		Characters: []string{"林砚"},
		KeyEvents:  []string{"拜师"},
	}); err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(1, "第一章正文结尾，留下试炼悬念。"); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 2,
		Title:   "试炼",
		Goal:    "通过第一关",
		Contract: domain.ChapterContract{
			RequiredBeats:    []string{"必须让主角通过第一关", "必须埋下内门试炼邀请"},
			ForbiddenMoves:   []string{"不能提前揭露师尊真实身份"},
			ContinuityChecks: []string{"主角左臂旧伤仍未痊愈"},
			EvaluationFocus:  []string{"重点检查试炼节奏是否拖沓"},
		},
	}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	if err := s.World.SaveStyleRules(domain.WritingStyleRules{
		Volume: 1,
		Arc:    1,
		Prose:  []string{"叙述保持克制"},
	}); err != nil {
		t.Fatalf("SaveStyleRules: %v", err)
	}
	if err := s.RunMeta.SetPlanningTier(domain.PlanningTierLong); err != nil {
		t.Fatalf("SetPlanningTier: %v", err)
	}

	tool := NewContextToolForRole(s, References{
		Consistency:      "一致性检查",
		HookTechniques:   "钩子技巧",
		QualityChecklist: "质量清单",
	}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, key := range []string{
		"premise",
		"premise_sections",
		"premise_structure",
		"outline",
		"world_rules",
		"memory_policy",
		"working_memory",
		"episodic_memory",
		"reference_pack",
	} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("expected key %q in chapter context", key)
		}
	}
	working := payload["working_memory"].(map[string]any)
	for _, key := range []string{"current_chapter_outline", "recent_summaries", "chapter_plan", "chapter_contract", "previous_tail"} {
		if _, ok := working[key]; !ok {
			t.Fatalf("expected working_memory.%s", key)
		}
	}
	episodic := payload["episodic_memory"].(map[string]any)
	if _, ok := episodic["planning_tier"]; !ok {
		t.Fatal("expected episodic_memory.planning_tier")
	}
	referencePack := payload["reference_pack"].(map[string]any)
	for _, key := range []string{"style_rules", "references"} {
		if _, ok := referencePack[key]; !ok {
			t.Fatalf("expected reference_pack.%s", key)
		}
	}
	for _, mirror := range []string{"planning_tier", "current_chapter_outline", "recent_summaries", "chapter_plan", "chapter_contract", "previous_tail", "style_rules", "references"} {
		if _, ok := payload[mirror]; ok {
			t.Fatalf("unexpected top-level compatibility mirror %q", mirror)
		}
	}
}

func TestContextToolArchitectModeIncludesPlanningAndFoundation(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 6); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}
	if err := s.Progress.UpdateVolumeArc(1, 1); err != nil {
		t.Fatalf("UpdateVolumeArc: %v", err)
	}
	if err := s.Outline.SavePremise(`## 题材和基调
群像冒险，偏冷峻史诗。

## 题材定位
群像长篇冒险

## 核心冲突
众人必须在不断失控的旧秩序中寻找新秩序。

## 主角目标
抵达真相核心。

## 终局方向
揭开古老真相并重建秩序。

## 写作禁区
不靠天降设定收尾。

## 差异化卖点
群像关系推进。

## 差异化钩子
每卷都改变队伍关系结构。

## 核心兑现承诺
持续提供发现、牺牲与选择。

## 故事引擎
旅途推进、真相调查与队伍关系共同驱动。

## 关系/成长主线
队伍从互不信任走向分裂再重组。

## 升级路径
从地方事件走向世界级危机。

## 中期转向
真相并非敌人，而是秩序本身有问题。

## 终局命题
秩序应由谁定义。
`); err != nil {
		t.Fatalf("SavePremise: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "起点", CoreEvent: "旅途开始"},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Characters.Save([]domain.Character{
		{Name: "沈曜", Role: "主角", Description: "流浪剑客", Arc: "寻找真相", Traits: []string{"敏锐"}},
	}); err != nil {
		t.Fatalf("SaveCharacters: %v", err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{
		{Category: "society", Rule: "城邦林立", Boundary: "皇权不可直辖边地"},
	}); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}
	if err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{
		{
			Index: 1, Title: "第一卷", Theme: "踏上旅途",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "启程", Goal: "建立队伍", Chapters: []domain.OutlineEntry{{Chapter: 1, Title: "起点"}}},
				{Index: 2, Title: "迷雾", Goal: "逼近秘密", EstimatedChapters: 5},
			},
		},
	}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := s.Summaries.SaveArcSummary(domain.ArcSummary{
		Volume: 1, Arc: 1, Title: "启程", Summary: "队伍建立，但因真相分歧出现裂痕。", KeyEvents: []string{"队伍建立", "分歧浮现"},
	}); err != nil {
		t.Fatalf("SaveArcSummary: %v", err)
	}
	if err := s.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{EndingDirection: "揭开古老真相", EstimatedScale: "预计 3 卷"},
	}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}
	if err := s.World.SaveStyleRules(domain.WritingStyleRules{
		Volume: 1,
		Arc:    1,
		Prose:  []string{"保持冷峻节制"},
	}); err != nil {
		t.Fatalf("SaveStyleRules: %v", err)
	}
	if err := s.RunMeta.SetPlanningTier(domain.PlanningTierLong); err != nil {
		t.Fatalf("SetPlanningTier: %v", err)
	}

	tool := NewContextToolForRole(s, References{
		OutlineTemplate:   "大纲模板",
		CharacterTemplate: "角色模板",
		LongformPlanning:  "长篇规划",
	}, "default", "architect")
	args, err := json.Marshal(map[string]any{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, key := range []string{
		"memory_policy",
		"planning_memory",
		"foundation_memory",
		"reference_pack",
	} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("expected key %q in architect context", key)
		}
	}
	planning := payload["planning_memory"].(map[string]any)
	for _, key := range []string{"planning_tier", "layered_outline", "skeleton_arcs", "compass"} {
		if _, ok := planning[key]; !ok {
			t.Fatalf("expected planning_memory.%s", key)
		}
	}
	foundation := payload["foundation_memory"].(map[string]any)
	for _, key := range []string{"premise_sections", "premise_structure", "characters", "foundation_status"} {
		if _, ok := foundation[key]; !ok {
			t.Fatalf("expected foundation_memory.%s", key)
		}
	}
	referencePack := payload["reference_pack"].(map[string]any)
	for _, key := range []string{"style_rules", "references"} {
		if _, ok := referencePack[key]; !ok {
			t.Fatalf("expected reference_pack.%s", key)
		}
	}
	for _, mirror := range []string{"planning_tier", "layered_outline", "skeleton_arcs", "compass", "premise_sections", "premise_structure", "characters", "foundation_status", "style_rules", "references"} {
		if _, ok := payload[mirror]; ok {
			t.Fatalf("unexpected top-level compatibility mirror %q", mirror)
		}
	}
}

func TestTrimByBudgetStyleRulesDoNotChangeOtherPriorities(t *testing.T) {
	build := func(withStyle bool) map[string]any {
		pack := map[string]any{"references": map[string]string{"guide": strings.Repeat("r", 1000)}}
		if withStyle {
			pack["style_rules"] = map[string]any{"prose": []string{strings.Repeat("风格", 4000)}}
		}
		return map[string]any{
			"outline":        strings.Repeat("o", 1000),
			"working_memory": map[string]any{"timeline": strings.Repeat("t", 500)},
			"reference_pack": pack,
		}
	}
	withoutStyle := build(false)
	withStyle := build(true)
	trimByBudget(withoutStyle, 700)
	trimByBudget(withStyle, 700)
	if fmt.Sprint(withoutStyle["_trimmed"]) != fmt.Sprint(withStyle["_trimmed"]) {
		t.Fatalf("style rules changed non-style trim order: without=%v with=%v", withoutStyle["_trimmed"], withStyle["_trimmed"])
	}
	if _, ok := withStyle["reference_pack"].(map[string]any)["style_rules"]; !ok {
		t.Fatal("style_rules must survive context trimming")
	}
	if _, ok := withStyle["working_memory"].(map[string]any)["timeline"]; !ok {
		t.Fatal("style protection must not evict later-priority timeline")
	}
}

func TestPlanningLayeredOutlineViewKeepsAllArcOutlinesAndNearbyChapterDetail(t *testing.T) {
	chapter := func(n int) domain.OutlineEntry {
		return domain.OutlineEntry{Chapter: n, Title: fmt.Sprintf("第%d章", n), CoreEvent: strings.Repeat("细节", 100)}
	}
	var layered []domain.VolumeOutline
	for volume := 1; volume <= 5; volume++ {
		layered = append(layered, domain.VolumeOutline{
			Index: volume, Title: fmt.Sprintf("第%d卷", volume), Theme: fmt.Sprintf("主题%d", volume),
			Arcs: []domain.ArcOutline{{
				Index: 1, Title: fmt.Sprintf("第%d卷主弧", volume), Goal: fmt.Sprintf("目标%d", volume),
				Chapters: []domain.OutlineEntry{chapter(volume*10 + 1), chapter(volume*10 + 2)},
			}},
		})
	}
	progress := &domain.Progress{CurrentVolume: 3, CurrentArc: 1}
	view := planningLayeredOutlineView(layered, progress, true)
	if len(view) != 5 {
		t.Fatalf("all volume outlines must remain: %d", len(view))
	}
	for i, volume := range view {
		arcs := volume["arcs"].([]map[string]any)
		if volume["title"] == "" || volume["theme"] == "" || arcs[0]["title"] == "" || arcs[0]["goal"] == "" {
			t.Fatalf("volume/arc outline lost at index %d: %#v", i, volume)
		}
		_, hasChapters := arcs[0]["chapters"]
		wantDetail := i >= 1 && i <= 3
		if hasChapters != wantDetail || arcs[0]["chapter_detail"] != wantDetail {
			t.Fatalf("volume %d chapter detail=%v want=%v: %#v", i+1, hasChapters, wantDetail, arcs[0])
		}
	}
	if arcs := view[2]["arcs"].([]map[string]any); arcs[0]["status"] != "active" {
		t.Fatalf("current arc status lost: %#v", arcs[0])
	}
	policy := planningOutlineDetailPolicy(layered, progress)
	loaded := policy["chapter_detail_volumes"].([]int)
	if fmt.Sprint(loaded) != "[2 3 4]" || policy["all_volume_outlines"] != true || policy["all_arc_outlines"] != true {
		t.Fatalf("unexpected detail policy: %#v", policy)
	}
	compact := planningLayeredOutlineView(layered, progress, false)
	for _, volume := range compact {
		arc := volume["arcs"].([]map[string]any)[0]
		if _, hasChapters := arc["chapters"]; hasChapters || arc["title"] == "" || arc["goal"] == "" {
			t.Fatalf("compact role should keep arc outline without chapters: %#v", arc)
		}
	}
}

func TestPlanningVolumeSummaryViewKeepsAllVolumesFull(t *testing.T) {
	var summaries []domain.VolumeSummary
	for i := 1; i <= 4; i++ {
		summaries = append(summaries, domain.VolumeSummary{
			Volume: i, Title: fmt.Sprintf("第%d卷", i), Summary: strings.Repeat("旧事", 400), KeyEvents: []string{"事件"},
		})
	}
	view := planningVolumeSummaryView(summaries)
	if len(view) != len(summaries) {
		t.Fatalf("volume summaries lost: got=%d want=%d", len(view), len(summaries))
	}
	for i := range view {
		if view[i].Summary != summaries[i].Summary || len(view[i].KeyEvents) != len(summaries[i].KeyEvents) {
			t.Fatalf("volume %d summary was compacted: %#v", i+1, view[i])
		}
	}
}

func TestContextToolSelectedMemoryRecallsStoryThreadsAndReviewLessons(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "邀约", CoreEvent: "长老暗中给出内门试炼邀请", Scenes: []string{"密谈", "留下试炼令"}},
		{Chapter: 2, Title: "试炼前夜", CoreEvent: "林砚准备回应内门试炼邀请", Hook: "谁在背后推动这场试炼", Scenes: []string{"整理线索", "决定赴约"}},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Progress.Init("test", 8); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.World.SaveForeshadowLedger([]domain.ForeshadowEntry{
		{ID: "trial_invite", Description: "内门试炼邀请的真实目的", PlantedAt: 1, Status: "planted"},
		{ID: "trial_mastermind", Description: "谁在背后推动这场试炼", PlantedAt: 1, Status: "planted"},
		{ID: "trial_rules", Description: "试炼规则碑文残卷", PlantedAt: 1, Status: "planted"},
		{ID: "outer_disciple", Description: "外门弟子的旧债纠纷", PlantedAt: 1, Status: "planted"},
		{ID: "elder_token", Description: "长老手中令牌的来历", PlantedAt: 1, Status: "planted"},
		{ID: "hidden_gate", Description: "山门背后的隐藏通道", PlantedAt: 1, Status: "planted"},
		{ID: "trial_bet", Description: "试炼盘口的幕后操盘人", PlantedAt: 1, Status: "planted"},
	}); err != nil {
		t.Fatalf("SaveForeshadowLedger: %v", err)
	}
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 2,
		Title:   "试炼前夜",
		Goal:    "决定是否回应邀请",
		Contract: domain.ChapterContract{
			PayoffPoints: []string{"回应内门试炼邀请"},
			HookGoal:     "抛出谁在背后推动试炼",
		},
	}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	if err := s.World.SaveReview(domain.ReviewEntry{
		Chapter:        1,
		Scope:          "chapter",
		Verdict:        "polish",
		Summary:        "主线启动完成，但伏笔不够明确。",
		ContractStatus: "partial",
		ContractMisses: []string{"未明确埋下内门试炼邀请"},
		Issues: []domain.ConsistencyIssue{
			{Type: "hook", Severity: "warning", Description: "章末钩子不够具体"},
		},
	}); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload struct {
		Selected struct {
			StoryThreads  []domain.RecallItem `json:"story_threads"`
			ReviewLessons []domain.RecallItem `json:"review_lessons"`
		} `json:"selected_memory"`
		Summary string `json:"_loading_summary"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(payload.Selected.StoryThreads) == 0 {
		t.Fatal("expected story thread recall items")
	}
	if len(payload.Selected.ReviewLessons) == 0 {
		t.Fatal("expected review lesson recall items")
	}
	if !containsRecallSummary(payload.Selected.StoryThreads, "内门试炼邀请") {
		t.Fatalf("expected story thread recall to mention invite, got %+v", payload.Selected.StoryThreads)
	}
	if !containsRecallSummary(payload.Selected.StoryThreads, "推动这场试炼") {
		t.Fatalf("expected story thread recall to mention trial mastermind, got %+v", payload.Selected.StoryThreads)
	}
	if containsRecallSummary(payload.Selected.StoryThreads, "试炼规则碑文残卷") {
		t.Fatalf("expected weak-overlap foreshadow to stay out, got %+v", payload.Selected.StoryThreads)
	}
	if containsRecallSummary(payload.Selected.StoryThreads, "建议回看第") {
		t.Fatalf("expected related_chapters not to be duplicated into story_threads, got %+v", payload.Selected.StoryThreads)
	}
	if !containsRecallSummary(payload.Selected.ReviewLessons, "contract 漏项") {
		t.Fatalf("expected review lesson recall to mention contract miss, got %+v", payload.Selected.ReviewLessons)
	}
	if !strings.Contains(payload.Summary, "线索召回:") || !strings.Contains(payload.Summary, "评审召回:") {
		t.Fatalf("expected loading summary to report selected memory, got %q", payload.Summary)
	}
}

// 久挂未回收的伏笔即使与当前章关键词无关，也应被账龄回填进 story_threads——
// 这正是相关性召回的盲区（独自悬挂太久、却没在本章撞上关键词的那根线）。
// 近期埋下的伏笔（账龄 < 阈值）不应被误标为"未回收"。
func TestContextToolSelectedMemorySurfacesAgingForeshadow(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 当前章主题与所有伏笔都不沾边，确保相关性召回为空，只剩账龄回填生效。
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 50, Title: "瘟疫", CoreEvent: "林砚在城南医馆救治瘟疫病患", Scenes: []string{"熬药", "封锁街巷"}},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Progress.Init("test", 60); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	// 6 条满足召回阈值；前两条账龄 ≥30（久挂），后四条账龄 <30（近期）。
	if err := s.World.SaveForeshadowLedger([]domain.ForeshadowEntry{
		{ID: "ancient_seal", Description: "上古封印的裂隙", PlantedAt: 3, Status: "planted"},
		{ID: "lost_bloodline", Description: "主角失落的血脉来历", PlantedAt: 5, Status: "advanced"},
		{ID: "market_feud", Description: "昨夜集市的口角", PlantedAt: 47, Status: "planted"},
		{ID: "rumor_a", Description: "近日传闻甲", PlantedAt: 48, Status: "planted"},
		{ID: "rumor_b", Description: "近日传闻乙", PlantedAt: 48, Status: "planted"},
		{ID: "rumor_c", Description: "近日传闻丙", PlantedAt: 49, Status: "planted"},
	}); err != nil {
		t.Fatalf("SaveForeshadowLedger: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 50})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload struct {
		Selected struct {
			StoryThreads []domain.RecallItem `json:"story_threads"`
		} `json:"selected_memory"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// 两条久挂伏笔应被回填，且带"未回收"账龄标注。
	if !containsRecallSummary(payload.Selected.StoryThreads, "上古封印的裂隙") {
		t.Fatalf("expected aging foreshadow to surface despite no relevance, got %+v", payload.Selected.StoryThreads)
	}
	if !containsRecallSummary(payload.Selected.StoryThreads, "失落的血脉") {
		t.Fatalf("expected second aging foreshadow to surface, got %+v", payload.Selected.StoryThreads)
	}
	if !containsRecallSummary(payload.Selected.StoryThreads, "未回收") {
		t.Fatalf("expected aging item to carry overdue annotation, got %+v", payload.Selected.StoryThreads)
	}
	// 近期伏笔（账龄 <30 且不相关）不应被回填。
	if containsRecallSummary(payload.Selected.StoryThreads, "昨夜集市的口角") {
		t.Fatalf("recent foreshadow must not be labeled overdue, got %+v", payload.Selected.StoryThreads)
	}
}

func TestContextToolSelectedMemoryIncludesGlobalReviewLessons(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "开端", CoreEvent: "故事开始"},
		{Chapter: 2, Title: "推进", CoreEvent: "主线继续推进"},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Progress.Init("test", 6); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.World.SaveReview(domain.ReviewEntry{
		Chapter: 1,
		Scope:   "global",
		Verdict: "polish",
		Summary: "全局推进合格，但角色目标表达还不够稳定。",
		Issues: []domain.ConsistencyIssue{
			{Type: "character", Severity: "warning", Description: "主角目标表达不够稳定"},
		},
	}); err != nil {
		t.Fatalf("SaveReview(global): %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload struct {
		Selected struct {
			ReviewLessons []domain.RecallItem `json:"review_lessons"`
		} `json:"selected_memory"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !containsRecallSummary(payload.Selected.ReviewLessons, "主角目标表达不够稳定") {
		t.Fatalf("expected global review lesson to be recalled, got %+v", payload.Selected.ReviewLessons)
	}
}

func TestContextToolKeepsFullForeshadowWhenRecallNotTriggered(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "起势", CoreEvent: "故事起势"},
		{Chapter: 2, Title: "推进", CoreEvent: "继续推进"},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Progress.Init("test", 4); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.World.SaveForeshadowLedger([]domain.ForeshadowEntry{
		{ID: "small_1", Description: "第一条小伏笔", PlantedAt: 1, Status: "planted"},
		{ID: "small_2", Description: "第二条小伏笔", PlantedAt: 1, Status: "planted"},
	}); err != nil {
		t.Fatalf("SaveForeshadowLedger: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	episodic := payload["episodic_memory"].(map[string]any)
	if _, ok := episodic["foreshadow_ledger"]; !ok {
		t.Fatal("expected full foreshadow ledger to remain when selected recall is not triggered")
	}
	if _, ok := payload["selected_memory"]; ok {
		t.Fatalf("expected no selected_memory for small foreshadow sets, got %+v", payload["selected_memory"])
	}
}

func TestContextToolFallsBackToFullForeshadowWhenSelectionIsTooSparse(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "邀约", CoreEvent: "长老暗中给出内门试炼邀请"},
		{Chapter: 2, Title: "试炼前夜", CoreEvent: "林砚准备回应内门试炼邀请", Scenes: []string{"整理线索", "决定赴约"}},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Progress.Init("test", 8); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.World.SaveForeshadowLedger([]domain.ForeshadowEntry{
		{ID: "trial_invite", Description: "内门试炼邀请的真实目的", PlantedAt: 1, Status: "planted"},
		{ID: "trial_rules", Description: "试炼规则碑文残卷", PlantedAt: 1, Status: "planted"},
		{ID: "outer_disciple", Description: "外门弟子的旧债纠纷", PlantedAt: 1, Status: "planted"},
		{ID: "elder_token", Description: "长老手中令牌的来历", PlantedAt: 1, Status: "planted"},
		{ID: "hidden_gate", Description: "山门背后的隐藏通道", PlantedAt: 1, Status: "planted"},
		{ID: "trial_bet", Description: "试炼盘口的幕后操盘人", PlantedAt: 1, Status: "planted"},
	}); err != nil {
		t.Fatalf("SaveForeshadowLedger: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	episodic := payload["episodic_memory"].(map[string]any)
	if _, ok := episodic["foreshadow_ledger"]; !ok {
		t.Fatal("expected full foreshadow ledger when selection is too sparse")
	}
	if selected, ok := payload["selected_memory"].(map[string]any); ok {
		if _, exists := selected["story_threads"]; exists {
			t.Fatalf("expected sparse story_threads to fall back to full ledger, got %+v", selected["story_threads"])
		}
	}
}

func containsRecallSummary(items []domain.RecallItem, want string) bool {
	for _, item := range items {
		if strings.Contains(item.Summary, want) {
			return true
		}
	}
	return false
}

func TestContextToolInjectsRewriteBriefForPendingRewriteChapter(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 3); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, 3000, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := s.Progress.SetPendingRewrites([]int{2}, "节奏拖沓，需要压缩前半段"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.World.SaveReview(domain.ReviewEntry{
		Chapter: 2,
		Scope:   "chapter",
		Verdict: "rewrite",
		Summary: "前半段铺垫过长，冲突迟迟不出现。",
		Issues: []domain.ConsistencyIssue{
			{Type: "pacing", Severity: "error", Description: "前 2000 字无推进"},
		},
		ContractMisses: []string{"未兑现试炼开场"},
	}); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	working := payload["working_memory"].(map[string]any)
	brief, ok := working["rewrite_brief"].(map[string]any)
	if !ok {
		t.Fatalf("expected working_memory.rewrite_brief in chapter context, got %T", working["rewrite_brief"])
	}
	if got := brief["reason"]; got != "节奏拖沓，需要压缩前半段" {
		t.Fatalf("expected rewrite reason, got %v", got)
	}
	if got, _ := brief["review_summary"].(string); !strings.Contains(got, "铺垫过长") {
		t.Fatalf("expected review summary from chapter review, got %v", brief["review_summary"])
	}
	if issues, _ := brief["issues"].([]any); len(issues) == 0 {
		t.Fatalf("expected review issues in rewrite_brief, got %v", brief["issues"])
	}
	if misses, _ := brief["contract_misses"].([]any); len(misses) == 0 {
		t.Fatalf("expected contract misses in rewrite_brief, got %v", brief["contract_misses"])
	}
}

func TestContextToolOmitsRewriteBriefForNormalChapter(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 3); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, err := json.Marshal(map[string]any{"chapter": 2})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := payload["rewrite_brief"]; ok {
		t.Fatal("expected no rewrite_brief for chapter outside PendingRewrites")
	}
}

func TestContextToolDoesNotInjectUserDirectives(t *testing.T) {
	// save_directive 已移除：novel_context 不再注入 working_memory.user_directives，
	// 长期写作要求统一走 user_rules。锁死这条，防止回归。
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 3); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	for name, chapter := range map[string]int{"writer": 1, "architect": 0} {
		args, _ := json.Marshal(map[string]any{"chapter": chapter})
		result, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("[%s] Execute: %v", name, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(result, &payload); err != nil {
			t.Fatalf("[%s] Unmarshal: %v", name, err)
		}
		working, ok := payload["working_memory"].(map[string]any)
		if !ok {
			t.Fatalf("[%s] missing working_memory", name)
		}
		if _, exists := working["user_directives"]; exists {
			t.Errorf("[%s] working_memory 不应再有 user_directives（已统一到 user_rules）", name)
		}
		// user_rules 仍应稳定注入
		if _, ok := working["user_rules"].(map[string]any); !ok {
			t.Errorf("[%s] working_memory.user_rules 应稳定注入", name)
		}
	}
}

func TestBuildCompassInjectionView_LongPriority(t *testing.T) {
	compass := &domain.WritingStyleRulesCompass{
		Long: &domain.StyleRulesLong{
			Prose:  []string{"long优先规则"},
			Taboos: []string{"long禁忌"},
			Dialogue: []domain.CharacterVoice{
				{Name: "全局角色", Rules: []string{"全局对话风格"}},
			},
		},
		Current: &domain.StyleRulesCurrent{
			Volume: 1,
			Arc:    2,
			Prose:  []string{"current补充"},
			Taboos: []string{"current禁忌"},
			Dialogue: []domain.CharacterVoice{
				{Name: "全局角色", Rules: []string{"current对话"}}, // 同角色→合并时 long 优先
				{Name: "弧角色", Rules: []string{"弧特有对话"}},    // long 没有→应出现
			},
			LastUpdated: "2024-03-01T00:00:00Z",
		},
	}

	view := buildCompassInjectionView(compass)

	// 1. 子对象完整呈现
	longObj, ok := view["long"].(map[string]any)
	if !ok {
		t.Fatal("expected 'long' sub-object")
	}
	if longObj["prose"].([]string)[0] != "long优先规则" {
		t.Fatal("long sub-object prose mismatch")
	}

	currentObj, ok := view["current"].(map[string]any)
	if !ok {
		t.Fatal("expected 'current' sub-object")
	}
	if currentObj["prose"].([]string)[0] != "current补充" {
		t.Fatal("current sub-object prose should contain current values")
	}
	if currentObj["last_updated"] != "2024-03-01T00:00:00Z" {
		t.Fatal("current sub-object last_updated missing")
	}

	// 2. 扁平合并视图：long 优先
	prose, ok := view["prose"].([]string)
	if !ok || prose[0] != "long优先规则" {
		t.Fatalf("flat prose should be from long, got %v", view["prose"])
	}
	taboos, ok := view["taboos"].([]string)
	if !ok || taboos[0] != "long禁忌" {
		t.Fatalf("flat taboos should be from long, got %v", view["taboos"])
	}

	// dialogue: 全局角色来自 long，弧角色补充自 current
	dialogue, ok := view["dialogue"].([]domain.CharacterVoice)
	if !ok {
		t.Fatalf("expected dialogue as []CharacterVoice, got %T", view["dialogue"])
	}
	if len(dialogue) != 2 {
		t.Fatalf("expected 2 merged dialogue entries, got %d", len(dialogue))
	}
	// long 的全局角色优先
	if dialogue[0].Name != "全局角色" || dialogue[0].Rules[0] != "全局对话风格" {
		t.Fatalf("long dialogue should have priority: %+v", dialogue[0])
	}
	// current 独有的弧角色被补充
	if dialogue[1].Name != "弧角色" || dialogue[1].Rules[0] != "弧特有对话" {
		t.Fatalf("current dialogue should supplement: %+v", dialogue[1])
	}

	// current 的 volume/arc 在扁平层
	if view["volume"] != 1 || view["arc"] != 2 {
		t.Fatalf("volume/arc from current: volume=%v arc=%v", view["volume"], view["arc"])
	}
}

func TestBuildCompassInjectionView_CurrentFillsLongGaps(t *testing.T) {
	// long 只有 prose，current 补充 taboos + dialogue
	compass := &domain.WritingStyleRulesCompass{
		Long: &domain.StyleRulesLong{
			Prose: []string{"长期规则"},
		},
		Current: &domain.StyleRulesCurrent{
			Volume: 2,
			Arc:    3,
			Taboos: []string{"弧级禁忌"},
			Dialogue: []domain.CharacterVoice{
				{Name: "弧角色", Rules: []string{"弧对话"}},
			},
			LastUpdated: "2024-03-15T00:00:00Z",
		},
	}

	view := buildCompassInjectionView(compass)

	// 子对象
	longObj := view["long"].(map[string]any)
	if longObj["prose"].([]string)[0] != "长期规则" {
		t.Fatal("long prose mismatch")
	}
	currentObj := view["current"].(map[string]any)
	if _, has := currentObj["prose"]; has {
		t.Fatal("current should not have prose (only taboos+dialogue)")
	}
	if currentObj["taboos"].([]string)[0] != "弧级禁忌" {
		t.Fatal("current taboos mismatch")
	}

	// 扁平层：prose 来自 long，taboos+dialogue 来自 current
	if view["prose"].([]string)[0] != "长期规则" {
		t.Fatal("flat prose from long")
	}
	if view["taboos"].([]string)[0] != "弧级禁忌" {
		t.Fatal("flat taboos from current (long gap)")
	}
	if view["volume"] != 2 || view["arc"] != 3 {
		t.Fatalf("volume/arc: %v/%v", view["volume"], view["arc"])
	}
	if view["last_updated"] != "2024-03-15T00:00:00Z" {
		t.Fatalf("last_updated: %v", view["last_updated"])
	}
}

func TestBuildCompassInjectionView_FallsBackToAnchorsWhenEmpty(t *testing.T) {
	// 空 compass 不注入内容字段（由外部 fallback 逻辑处理）
	compass := &domain.WritingStyleRulesCompass{}
	view := buildCompassInjectionView(compass)

	if _, ok := view["prose"]; ok {
		t.Fatal("expected no prose for empty compass")
	}
	if _, ok := view["dialogue"]; ok {
		t.Fatal("expected no dialogue for empty compass")
	}
	if _, ok := view["long"]; ok {
		t.Fatal("expected no long sub-object for empty compass")
	}
	if _, ok := view["current"]; ok {
		t.Fatal("expected no current sub-object for empty compass")
	}
	if _, ok := view["_compass"]; !ok {
		t.Fatal("expected _compass annotation even for empty compass")
	}
}

// ── style_anchors 集成测试 ──

func writeStyleAnchorsJSON(t *testing.T, dir string, data string) {
	t.Helper()
	path := filepath.Join(dir, "meta", "style_anchors.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// anchorTestText 是 ≥50 个字符的正文片段，确保 ExtractStyleAnchors 能提取。
const anchorTestText = "夜色如墨，他在城头站了整夜。风掀起残破的披风，像一个无声的誓言。远处的篝火映在他眼中，却没有一丝温度。他在等，等一个不会来的人。"

func initTestStore(t *testing.T, fn func(s *store.Store)) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 30); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "开端", CoreEvent: "故事开始"},
		{Chapter: 5, Title: "中段", CoreEvent: "剧情推进"},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Outline.SavePremise("## 题材和基调\n测试小说\n"); err != nil {
		t.Fatalf("SavePremise: %v", err)
	}
	if fn != nil {
		fn(s)
	}
	return s
}

func initTestStoreWithAnchors(t *testing.T, anchorsJSON string, fn func(s *store.Store)) *store.Store {
	t.Helper()
	s := initTestStore(t, func(s *store.Store) {
		writeStyleAnchorsJSON(t, s.Dir(), anchorsJSON)
		if fn != nil {
			fn(s)
		}
	})
	return s
}

func refPack(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	rp, ok := payload["reference_pack"].(map[string]any)
	if !ok {
		t.Fatal("missing reference_pack")
	}
	return rp
}

// ── 角色可见性 ──

func TestAnchor_InjectedForWriter(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":false,
		"anchors":[{"id":"a1","excerpt":"Ex1."},{"id":"a2","excerpt":"Ex2."}]
	}`, nil)
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp := refPack(t, p)

	manual, ok := rp["style_anchors_manual"].([]any)
	if !ok || len(manual) != 2 {
		t.Fatalf("expected 2 manual anchors for writer")
	}
	// only id+excerpt in injection view
	item := manual[0].(map[string]any)
	if _, has := item["provenance"]; has {
		t.Fatal("provenance must be stripped from injection view")
	}
	if _, has := item["applies_to"]; has {
		t.Fatal("applies_to must be stripped from injection view")
	}
	if item["id"] != "a1" || item["excerpt"] != "Ex1." {
		t.Fatalf("wrong injection item: %+v", item)
	}
}

func TestAnchor_InjectedForEditor(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":false,
		"anchors":[{"id":"a1","excerpt":"Ex."}]
	}`, nil)
	tool := NewContextToolForRole(s, References{}, "default", "editor")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp := refPack(t, p)
	if _, ok := rp["style_anchors_manual"]; !ok {
		t.Fatal("expected style_anchors_manual for editor")
	}
}

func TestAnchor_NotInjectedForCoordinatorOrArchitect(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,
		"anchors":[{"id":"a1","excerpt":"Ex."}]
	}`, nil)

	check := func(role string, ch int) {
		t.Helper()
		tool := NewContextToolForRole(s, References{}, "default", role)
		args, _ := json.Marshal(map[string]any{"chapter": ch})
		result, _ := tool.Execute(context.Background(), args)
		var p map[string]any
		json.Unmarshal(result, &p)

		// Check all containers for anchor keys
		for _, container := range []string{"reference_pack", "working_memory", "episodic_memory", "planning_memory", "foundation_memory"} {
			section, _ := p[container].(map[string]any)
			if section == nil {
				continue
			}
			for _, key := range []string{"style_anchors_manual", "style_anchors_auto", "style_anchors"} {
				if _, exists := section[key]; exists {
					t.Fatalf("%s(ch=%d) must not see %s in %s", role, ch, key, container)
				}
			}
		}
	}
	for _, role := range []string{"coordinator", "architect"} {
		check(role, 0)
		check(role, 1)
	}
}

// ── 按章节过滤（AnchorMatchesChapter） ──

func TestAnchor_ChapterFilter(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":false,
		"anchors":[
			{"id":"global","excerpt":"Global."},
			{"id":"early","excerpt":"Early.","applies_to":{"chapter_ranges":[[1,3]]}},
			{"id":"mid","excerpt":"Mid.","applies_to":{"chapter_ranges":[[4,7]]}},
			{"id":"late","excerpt":"Late.","applies_to":{"chapter_ranges":[[11,15]]}}
		]
	}`, nil)

	tool := NewContextToolForRole(s, References{}, "default", "writer")

	getIDs := func(ch int) map[string]bool {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"chapter": ch})
		result, _ := tool.Execute(context.Background(), args)
		var p map[string]any
		json.Unmarshal(result, &p)
		rp := refPack(t, p)
		raw, exists := rp["style_anchors_manual"]
		if !exists {
			return nil
		}
		ids := make(map[string]bool)
		for _, m := range raw.([]any) {
			ids[m.(map[string]any)["id"].(string)] = true
		}
		return ids
	}

	ids := getIDs(2)
	if len(ids) != 2 || !ids["global"] || !ids["early"] {
		t.Fatalf("ch2 expected global+early, got %v", ids)
	}

	ids = getIDs(5)
	if len(ids) != 2 || !ids["global"] || !ids["mid"] {
		t.Fatalf("ch5 expected global+mid, got %v", ids)
	}

	ids = getIDs(10)
	if ids == nil || len(ids) != 1 || !ids["global"] {
		t.Fatalf("ch10 expected only global, got %v", ids)
	}
}

func TestAnchor_ChapterFilterBoundary(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":false,
		"anchors":[
			{"id":"r","excerpt":"Range [3,7].","applies_to":{"chapter_ranges":[[3,7]]}}
		]
	}`, nil)

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	check := func(ch int, expectPresent bool) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"chapter": ch})
		result, _ := tool.Execute(context.Background(), args)
		var p map[string]any
		json.Unmarshal(result, &p)
		rp := refPack(t, p)
		manualRaw, exists := rp["style_anchors_manual"]
		if !exists {
			if expectPresent {
				t.Fatalf("ch%d expected style_anchors_manual to exist", ch)
			}
			return
		}
		manual := manualRaw.([]any)
		if expectPresent && len(manual) != 1 {
			t.Fatalf("ch%d expected 1 anchor, got %d", ch, len(manual))
		}
		if !expectPresent && len(manual) > 0 {
			t.Fatalf("ch%d expected 0 anchors, got %d", ch, len(manual))
		}
	}
	check(2, false) // before range
	check(3, true)  // range start
	check(5, true)  // range mid
	check(7, true)  // range end
	check(8, false) // after range
}

func TestAnchor_ChapterFilterMultiRange(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":false,
		"anchors":[
			{"id":"mr","excerpt":"Multi","applies_to":{"chapter_ranges":[[1,2],[10,12]]}}
		]
	}`, nil)

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	check := func(ch int, expectHas bool) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"chapter": ch})
		result, _ := tool.Execute(context.Background(), args)
		var p map[string]any
		json.Unmarshal(result, &p)
		rp := refPack(t, p)
		manualRaw, exists := rp["style_anchors_manual"]
		if !exists {
			if expectHas {
				t.Fatalf("ch%d expected style_anchors_manual to exist", ch)
			}
			return
		}
		manual := manualRaw.([]any)
		if expectHas && len(manual) != 1 {
			t.Fatalf("ch%d expected 1 anchor, got %d", ch, len(manual))
		}
		if !expectHas && len(manual) > 0 {
			t.Fatalf("ch%d expected 0 anchors, got %d", ch, len(manual))
		}
	}
	check(1, true)
	check(2, true)
	check(5, false)
	check(10, true)
	check(12, true)
	check(13, false)
}

// ── 自动锚点注入决策 ──

func TestAnchor_IncludeAutoTrue(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":true,
		"anchors":[{"id":"m1","excerpt":"手动。"}]
	}`, func(s *store.Store) {
		s.Drafts.SaveFinalChapter(1, anchorTestText)
		p, _ := s.Progress.Load()
		p.CompletedChapters = []int{1}
		s.Progress.Save(p)
	})
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp := refPack(t, p)

	if _, exists := rp["style_anchors_manual"]; !exists {
		t.Fatal("expected manual anchors")
	}
	auto, exists := rp["style_anchors_auto"]
	if !exists {
		t.Fatal("expected style_anchors_auto when include_auto=true")
	}
	if slice, ok := auto.([]any); !ok || len(slice) == 0 {
		t.Fatal("expected non-empty style_anchors_auto")
	}
	if _, exists := rp["style_anchors"]; exists {
		t.Fatal("no legacy style_anchors when manual file present")
	}
}

func TestAnchor_IncludeAutoFalseNoAuto(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":false,
		"anchors":[{"id":"m1","excerpt":"手动。"}]
	}`, func(s *store.Store) {
		s.Drafts.SaveFinalChapter(1, anchorTestText)
		p, _ := s.Progress.Load()
		p.CompletedChapters = []int{1}
		s.Progress.Save(p)
	})
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp := refPack(t, p)

	for _, key := range []string{"style_anchors_auto", "style_anchors"} {
		if _, exists := rp[key]; exists {
			t.Fatalf("unexpected %s when include_auto=false and manual exists", key)
		}
	}
}

func TestAnchor_CorruptedFailClosed(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"include_auto":true,
		"anchors":[{"id":"a1","excerpt":"`+strings.Repeat("好", 1001)+`"}]
	}`, func(s *store.Store) {
		s.Drafts.SaveFinalChapter(1, anchorTestText)
		p, _ := s.Progress.Load()
		p.CompletedChapters = []int{1}
		s.Progress.Save(p)
	})
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp, _ := p["reference_pack"].(map[string]any)
	if rp != nil {
		for _, key := range []string{"style_anchors_manual", "style_anchors_auto", "style_anchors"} {
			if _, exists := rp[key]; exists {
				t.Fatalf("unexpected %s for corrupted file", key)
			}
		}
	}
	warns, _ := p["_warnings"].([]any)
	if len(warns) == 0 {
		t.Fatal("expected _warnings for corrupted file")
	}
}

func TestAnchor_CorruptedNoStyleRulesNoLegacyAuto(t *testing.T) {
	// 损坏的 manual 文件 + 无 style_rules → 绝不触发 legacy auto
	dir := t.TempDir()
	s := store.NewStore(dir)
	s.Init()
	s.Progress.Init("test", 5)
	s.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "开端", CoreEvent: "开始"}})
	s.Outline.SavePremise("## 题材和基调\n测试\n")
	s.Drafts.SaveFinalChapter(1, anchorTestText)
	p, _ := s.Progress.Load()
	p.CompletedChapters = []int{1}
	s.Progress.Save(p)
	writeStyleAnchorsJSON(t, dir, `{
		"version":1,"include_auto":true,
		"anchors":[{"id":"a1","excerpt":"`+strings.Repeat("好", 1001)+`"}]
	}`)

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	result, _ := tool.Execute(context.Background(), args)
	var payload map[string]any
	json.Unmarshal(result, &payload)
	rp, _ := payload["reference_pack"].(map[string]any)
	if rp != nil {
		for _, key := range []string{"style_anchors_manual", "style_anchors_auto", "style_anchors"} {
			if _, exists := rp[key]; exists {
				t.Fatalf("corrupted+no style_rules must not inject %s", key)
			}
		}
	}
}

func TestAnchor_EmptyValidFile(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"anchors":[]
	}`, func(s *store.Store) {
		s.Drafts.SaveFinalChapter(1, anchorTestText)
		p, _ := s.Progress.Load()
		p.CompletedChapters = []int{1}
		s.Progress.Save(p)
	})
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp, _ := p["reference_pack"].(map[string]any)

	if rp != nil {
		for _, key := range []string{"style_anchors", "style_anchors_auto", "style_anchors_manual"} {
			if _, exists := rp[key]; exists {
				t.Fatalf("unexpected %s for empty-valid file", key)
			}
		}
	}
}

func TestAnchor_LegacyFallbackNoManualNoStyleRules(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	s.Init()
	s.Progress.Init("test", 5)
	s.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "开端", CoreEvent: "开始"}})
	s.Outline.SavePremise("## 题材和基调\n测试\n")
	s.Drafts.SaveFinalChapter(1, anchorTestText)
	p, _ := s.Progress.Load()
	p.CompletedChapters = []int{1}
	s.Progress.Save(p)

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	result, _ := tool.Execute(context.Background(), args)
	var payload map[string]any
	json.Unmarshal(result, &payload)
	rp := refPack(t, payload)
	if _, exists := rp["style_anchors"]; !exists {
		t.Fatal("expected legacy style_anchors when no manual and no style_rules")
	}
}

// ── 旧格式兼容 ──

func TestAnchor_LegacyFormatInject(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,"purpose":"Style guide","usage":"For writer",
		"anchors":[{"id":"old1","label":"lab1","text":"Legacy text one."}]
	}`, nil)

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp := refPack(t, p)

	manual, ok := rp["style_anchors_manual"].([]any)
	if !ok || len(manual) != 1 {
		t.Fatalf("expected 1 legacy-converted anchor, got %d", len(manual))
	}
	item := manual[0].(map[string]any)
	// id 应优先于 label
	if item["id"] != "old1" || item["excerpt"] != "Legacy text one." {
		t.Fatalf("legacy conversion failed: %+v", item)
	}
	// _warnings should include migration notice
	warns, _ := p["_warnings"].([]any)
	migrationFound := false
	for _, w := range warns {
		if strings.Contains(w.(string), "旧格式") {
			migrationFound = true
			break
		}
	}
	if !migrationFound {
		t.Fatal("expected migration warning for legacy format")
	}
}

// ── 裁剪 ──

func TestAnchor_ManualNotTrimmed(t *testing.T) {
	s := initTestStoreWithAnchors(t, `{
		"version":1,
		"anchors":[{"id":"b1","excerpt":"`+strings.Repeat("好", 500)+`"},{"id":"b2","excerpt":"`+strings.Repeat("好", 500)+`"}]
	}`, nil)
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	result, _ := tool.Execute(context.Background(), args)
	var p map[string]any
	json.Unmarshal(result, &p)
	rp, _ := p["reference_pack"].(map[string]any)
	if rp == nil {
		t.Fatal("missing reference_pack")
	}
	if _, exists := rp["style_anchors_manual"]; !exists {
		t.Fatal("manual anchors must survive trimming")
	}
	if trimmed, ok := p["_trimmed"].([]any); ok {
		for _, item := range trimmed {
			if item == "style_anchors_manual" {
				t.Fatal("style_anchors_manual must not appear in _trimmed")
			}
		}
	}
}

func TestAnchor_TrimOrder(t *testing.T) {
	pack := map[string]any{
		"outline":            strings.Repeat("o", 500),
		"style_anchors_auto": strings.Repeat("a", 500),
		"style_anchors":      strings.Repeat("s", 500),
		"references":         map[string]string{"guide": strings.Repeat("r", 500)},
		"voice_samples":      []string{strings.Repeat("v", 500)},
	}
	result := map[string]any{"outline": strings.Repeat("O", 500), "reference_pack": pack}
	trimByBudget(result, 100)
	trimmed, _ := result["_trimmed"].([]string)
	if len(trimmed) == 0 {
		t.Skip("trim not triggered")
	}
	aPos, rPos := -1, -1
	for i, k := range trimmed {
		switch k {
		case "style_anchors_auto", "style_anchors":
			if aPos < 0 {
				aPos = i
			}
		case "references":
			rPos = i
		}
	}
	if aPos >= 0 && rPos >= 0 && aPos > rPos {
		t.Fatalf("anchors (%d) must trim before references (%d)", aPos, rPos)
	}
}

// ── Nil-safety regression ──

// TestAnchor_NilProgressManualPath 验证 progress=nil + 有手动 anchors 不 panic。
func TestAnchor_NilProgressManualPath(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	s.Init()
	s.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "开端", CoreEvent: "开始"}})
	s.Outline.SavePremise("## 题材和基调\n测试\n")
	s.Characters.Save([]domain.Character{
		{Name: "林砚", Role: "主角", Description: "少年", Tier: "core"},
	})
	s.Drafts.SaveFinalChapter(1, "林砚说道：「你好。」夜色深沉。")
	writeStyleAnchorsJSON(t, dir, `{
		"version":1,"anchors":[{"id":"a1","excerpt":"Ex."}]
	}`)

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(result, &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	rp, _ := p["reference_pack"].(map[string]any)
	if rp == nil {
		t.Fatal("expected reference_pack")
	}
	if _, exists := rp["style_anchors_manual"]; !exists {
		t.Fatal("expected style_anchors_manual despite nil progress")
	}
}

// TestAnchor_NilProgressLegacyPath 验证 progress=nil + 无 manual + 无 style_rules + 有 characters/drafts
// 走 legacy style_anchors/voice_samples 路径时不 panic。
func TestAnchor_NilProgressLegacyPath(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	s.Init()
	s.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "开端", CoreEvent: "开始"}})
	s.Outline.SavePremise("## 题材和基调\n测试\n")
	s.Characters.Save([]domain.Character{
		{Name: "林砚", Role: "主角", Description: "少年", Tier: "core"},
	})
	// 写≥50字符章节，让 ExtractStyleAnchors 可能提取
	s.Drafts.SaveFinalChapter(1, "夜色如墨，他在城头站了整夜。风掀起残破的披风，像一个无声的誓言。远处的篝火映在他眼中，却没有一丝温度。他在等，等一个不会来的人。")
	// 不创建 progress 也不创建 manual anchors 文件
	// 这样 manStatus=StatusNotExist, hasStyleRules=false, progress=nil

	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(result, &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// 不 panic 就算通过；progress=nil 时 maxCompleted=0 不提取，
	// 但路径本身不应崩溃
	rp, _ := p["reference_pack"].(map[string]any)
	if rp == nil {
		t.Fatal("expected reference_pack")
	}
	// progress=nil 时 safeMaxCompleted 返回 0，因此不会提取到 style_anchors，
	// 但路径安全通过就是成功的 nil-safety 证明
	t.Log("legacy path survived nil progress without panic")
}

// TestContextToolInjectsRuleViolations 违规事实管道契约(第五轮评审):
// commit 落盘的机械违规必须经 novel_context(chapter=N) 真实注入——
// editor.md §机械检查映射消费的就是这个字段,管道断了 prompt 就成空头支票。
func TestContextToolInjectsRuleViolations(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Save(&domain.Progress{TotalChapters: 3, Phase: domain.PhaseWriting}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := st.World.SaveRuleViolations(2, []rules.Violation{
		{Rule: "fatigue_words", Target: "不禁", Actual: 9, Severity: rules.SeverityWarning},
	}); err != nil {
		t.Fatalf("save violations: %v", err)
	}

	tool := NewContextToolForRole(st, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 2})
	raw, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	vs, ok := result["rule_violations"].([]any)
	if !ok || len(vs) != 1 {
		t.Fatalf("rule_violations 必须注入章节上下文, got %v", result["rule_violations"])
	}

	// 无违规章节:字段缺省(editor.md 约定)
	args3, _ := json.Marshal(map[string]any{"chapter": 3})
	raw3, err := tool.Execute(context.Background(), args3)
	if err != nil {
		t.Fatalf("Execute ch3: %v", err)
	}
	var result3 map[string]any
	_ = json.Unmarshal(raw3, &result3)
	if _, has := result3["rule_violations"]; has {
		t.Fatal("无违规章节不应带 rule_violations 字段")
	}
}
