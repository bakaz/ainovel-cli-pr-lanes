package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestCheckConsistencyReturnsDigestAndRuleFactsWithoutContent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Drafts.SaveDraft(1, "# 第一章\n\n某种程度上，他看见了 TEST。\n\n## 多余标题"); err != nil {
		t.Fatal(err)
	}
	snap := rules.BuildSnapshot([]rules.Candidate{rules.SystemDefaults()})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}
	out, err := NewCheckConsistencyTool(st).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["content"]; ok {
		t.Fatal("check_consistency 不应重复返回草稿全文")
	}
	if got["content_digest"] == "" || got["word_count"] == nil {
		t.Fatalf("应返回摘要事实: %+v", got)
	}
	violations, ok := got["rule_violations"].([]any)
	if !ok || len(violations) < 3 {
		t.Fatalf("应同时运行 Lint + Check，got %+v", got["rule_violations"])
	}
}

// TestCheckConsistencyInjectsCharacterStateAndPlan character_state 按正文角色筛选注入，
// 同时注入当前章 plan/contract；未出场的实体状态不注入。
func TestCheckConsistencyInjectsCharacterStateAndPlan(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Drafts.SaveDraft(1, "林砚走进山门。苏晚在后面跟着。"+strings.Repeat("内容", 100)); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "入门", CoreEvent: "林砚拜入宗门"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Characters.Save([]domain.Character{
		{Name: "林砚", Tier: "core"},
		{Name: "苏晚", Tier: "secondary"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林砚", Field: "body_device.缠足", Value: "存在", UpdatedChapter: 1},
		{Entity: "苏晚", Field: "health.伤势", Value: "左臂旧伤", UpdatedChapter: 1},
		{Entity: "路人甲", Field: "status.在场", Value: "否", UpdatedChapter: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 1, Goal: "林砚入门",
		Contract: domain.ChapterContract{RequiredBeats: []string{"必须埋下内门试炼邀请"}},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := NewCheckConsistencyTool(st).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	cs, ok := got["character_state"].([]any)
	if !ok {
		t.Fatalf("expected character_state, got %T", got["character_state"])
	}
	if len(cs) != 2 {
		t.Fatalf("only in-chapter characters (林砚/苏晚) should be injected, 路人甲 excluded; got %d: %+v", len(cs), cs)
	}
	if got["chapter_plan"] == nil || got["chapter_contract"] == nil {
		t.Fatalf("expected chapter_plan/chapter_contract injected, got %+v", got)
	}
}

// TestCheckConsistencyReportsDuplicateCharacterStateKey 重复 (entity, field) key 由代码报告。
func TestCheckConsistencyReportsDuplicateCharacterStateKey(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Drafts.SaveDraft(1, "林砚走进山门。"+strings.Repeat("内容", 100)); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "入门", CoreEvent: "林砚拜入宗门"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林砚", Field: "status.道途", Value: "练气一层", UpdatedChapter: 1},
		{Entity: "林砚", Field: "status.道途", Value: "练气二层", UpdatedChapter: 2},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := NewCheckConsistencyTool(st).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	issues, ok := got["character_state_issues"].([]any)
	if !ok || len(issues) != 1 {
		t.Fatalf("expected 1 duplicate key issue, got %+v", got["character_state_issues"])
	}
}
