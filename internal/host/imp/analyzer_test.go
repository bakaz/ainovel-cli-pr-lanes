package imp

import (
	"context"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

const validAnalyzerEnvelope = `=== SUMMARY ===
林晚收到匿名爆料后，在档案馆发现失踪者全部姓陈，并在祭品旁找到陈姓家族祖宅地址。

=== CHARACTERS ===
["林晚","档案馆管理员"]

=== KEY_EVENTS ===
["林晚收到匿名信","在档案馆发现陈姓共同点","找到祖宅地址"]

=== TIMELINE ===
[
  {"time":"傍晚","event":"林晚收到匿名信","characters":["林晚"]},
  {"time":"次日","event":"档案馆走访","characters":["林晚","档案馆管理员"]}
]

=== FORESHADOW ===
[
  {"id":"hk-chen-family","action":"plant","description":"陈姓家族与连环失踪案的关联","horizon":"book"}
]

=== RELATIONSHIPS ===
[]

=== STATE_CHANGES ===
[
  {"entity":"林晚","field":"location","old_value":"编辑部","new_value":"档案馆","reason":"循迹追查"}
]

=== CHARACTER_STATE ===
[
  {"entity":"林晚","field":"location.place","value":"陈氏祖宅","reason":"循迹追查"}
]

=== HOOK_TYPE ===
mystery

=== DOMINANT_STRAND ===
quest
`

func TestParseAnalyzer_Valid(t *testing.T) {
	got, err := parseAnalyzerOutput(validAnalyzerEnvelope)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.HookType != "mystery" || got.DominantStrand != "quest" {
		t.Errorf("hook/strand: %+v", got)
	}
	if len(got.Characters) != 2 || len(got.KeyEvents) != 3 {
		t.Errorf("counts: %+v", got)
	}
	if len(got.ForeshadowUpdates) != 1 || got.ForeshadowUpdates[0].ID != "hk-chen-family" {
		t.Errorf("foreshadow: %+v", got.ForeshadowUpdates)
	}
	if got.ForeshadowUpdates[0].Horizon != "book" {
		t.Errorf("foreshadow horizon: %+v", got.ForeshadowUpdates[0])
	}
	if len(got.TimelineEvents) != 2 {
		t.Errorf("timeline: %+v", got.TimelineEvents)
	}
	if len(got.RelationshipChanges) != 0 {
		t.Errorf("relationships should be empty: %+v", got.RelationshipChanges)
	}
	if len(got.StateChanges) != 1 || got.StateChanges[0].Field != "location" {
		t.Errorf("state changes: %+v", got.StateChanges)
	}
	if len(got.CharacterStateUpdates) != 1 || got.CharacterStateUpdates[0].Field != "location.place" {
		t.Errorf("character state: %+v", got.CharacterStateUpdates)
	}
	if got.CharacterStateUpdates[0].Value != "陈氏祖宅" {
		t.Errorf("character state value: %+v", got.CharacterStateUpdates[0])
	}
}

func TestParseAnalyzer_RejectsInvalidHookType(t *testing.T) {
	bad := strings.Replace(validAnalyzerEnvelope, "mystery", "weird", 1)
	if _, err := parseAnalyzerOutput(bad); err == nil ||
		!strings.Contains(err.Error(), "invalid hook_type") {
		t.Fatalf("want hook_type error, got %v", err)
	}
}

func TestParseAnalyzer_RejectsPlantWithoutDescription(t *testing.T) {
	bad := strings.Replace(
		validAnalyzerEnvelope,
		`{"id":"hk-chen-family","action":"plant","description":"陈姓家族与连环失踪案的关联","horizon":"book"}`,
		`{"id":"hk-chen-family","action":"plant"}`,
		1,
	)
	if _, err := parseAnalyzerOutput(bad); err == nil ||
		!strings.Contains(err.Error(), "requires description") {
		t.Fatalf("want plant-without-desc error, got %v", err)
	}
}

func TestParseAnalyzer_RejectsPlantWithoutHorizon(t *testing.T) {
	bad := strings.Replace(
		validAnalyzerEnvelope,
		`{"id":"hk-chen-family","action":"plant","description":"陈姓家族与连环失踪案的关联","horizon":"book"}`,
		`{"id":"hk-chen-family","action":"plant","description":"陈姓家族与连环失踪案的关联"}`,
		1,
	)
	if _, err := parseAnalyzerOutput(bad); err == nil ||
		!strings.Contains(err.Error(), "horizon") {
		t.Fatalf("want plant-without-horizon error, got %v", err)
	}
}

func TestParseAnalyzer_RejectsAdvanceWithoutEvidence(t *testing.T) {
	bad := strings.Replace(
		validAnalyzerEnvelope,
		`{"id":"hk-chen-family","action":"plant","description":"陈姓家族与连环失踪案的关联","horizon":"book"}`,
		`{"id":"hk-chen-family","action":"advance"}`,
		1,
	)
	if _, err := parseAnalyzerOutput(bad); err == nil ||
		!strings.Contains(err.Error(), "requires evidence") {
		t.Fatalf("want advance-without-evidence error, got %v", err)
	}
}

func TestParseAnalyzer_RejectsBadCharacterStateField(t *testing.T) {
	bad := strings.Replace(
		validAnalyzerEnvelope,
		`{"entity":"林晚","field":"location.place","value":"陈氏祖宅","reason":"循迹追查"}`,
		`{"entity":"林晚","field":"freeform","value":"陈氏祖宅"}`,
		1,
	)
	if _, err := parseAnalyzerOutput(bad); err == nil ||
		!strings.Contains(err.Error(), "受控命名空间") {
		t.Fatalf("want character_state namespace error, got %v", err)
	}
}

func TestParseAnalyzer_MissingRequiredTag(t *testing.T) {
	bad := strings.Replace(validAnalyzerEnvelope, "=== HOOK_TYPE ===\nmystery\n", "", 1)
	if _, err := parseAnalyzerOutput(bad); err == nil ||
		!strings.Contains(err.Error(), "missing required tags") {
		t.Fatalf("want missing-tag error, got %v", err)
	}
}

func TestBuildAnalyzerUserPrompt_InjectsChapterBaselineCharacterState(t *testing.T) {
	prompt := buildAnalyzerUserPrompt(
		3, "追查", "正文内容", "前提", "角色块",
		[]domain.ForeshadowEntry{{ID: "hk-chen-family", Description: "陈姓家族关联", PlantedAt: 1, Status: "planted"}},
		[]domain.CharacterStateEntry{
			{Entity: "林晚", Field: "body_device.collar", Value: "声控锁", UpdatedChapter: 2},
			{Entity: "林晚", Field: "health.injury", Value: "左臂旧伤", UpdatedChapter: 1},
		},
	)
	for _, want := range []string{
		"## 已知角色状态（开章基线）",
		"`林晚` `body_device.collar`：声控锁",
		"（截至第 2 章）",
		"`林晚` `health.injury`：左臂旧伤",
		"不要重复输出",
		"不输出该冲突值为新当前值",
		"不得静默覆盖",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q, got:\n%s", want, prompt)
		}
	}
	// 基线段必须位于伏笔池与正文之前
	idxBase := strings.Index(prompt, "已知角色状态")
	idxHooks := strings.Index(prompt, "已知伏笔池")
	idxBody := strings.Index(prompt, "## 本章正文")
	if idxBase < 0 || idxHooks < 0 || idxBody < 0 || !(idxBase < idxHooks && idxHooks < idxBody) {
		t.Fatalf("baseline section ordering wrong (base=%d hooks=%d body=%d)", idxBase, idxHooks, idxBody)
	}
}

func TestBuildAnalyzerUserPrompt_OmitsBaselineWhenEmpty(t *testing.T) {
	prompt := buildAnalyzerUserPrompt(1, "开端", "正文内容", "", "", nil, nil)
	if strings.Contains(prompt, "已知角色状态") {
		t.Fatalf("expected no baseline section for empty character state, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## 本章正文") {
		t.Fatalf("expected body section, got:\n%s", prompt)
	}
}

func TestPersistChapter_FullPipeline(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Progress.Init("ch-test", 2); err != nil {
		t.Fatal(err)
	}

	// 准备 foundation：先用 ReverseFoundation+PersistFoundation 模拟 Phase 2 已完成
	fr := mustParse(t, validEnvelope, 2)
	if err := PersistFoundation(context.Background(), st, domain.PlanningTierShort, fr, nil); err != nil {
		t.Fatal(err)
	}

	a, err := parseAnalyzerOutput(validAnalyzerEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	commitTool := tools.NewCommitChapterTool(st)
	body := "林晚翻开匿名信，发现一行潦草字迹。她心里骂自己丢人，真不要脸。\n\n（正文略，>500 字以让 LoadChapterContent 校验。）她恨自己软弱，算了吧。"
	body = strings.Repeat(body, 10) // 凑够字数

	if err := PersistChapter(context.Background(), st, commitTool, 1, "初遇", body, a); err != nil {
		t.Fatalf("PersistChapter: %v", err)
	}

	prog, _ := st.Progress.Load()
	if len(prog.CompletedChapters) != 1 || prog.CompletedChapters[0] != 1 {
		t.Errorf("completed chapters wrong: %+v", prog.CompletedChapters)
	}

	hooks, err := st.World.LoadForeshadowLedger()
	if err != nil {
		t.Fatalf("load hooks: %v", err)
	}
	if len(hooks) != 1 || hooks[0].ID != "hk-chen-family" {
		t.Errorf("foreshadow not persisted: %+v", hooks)
	}

	// 二次提交同一章应是幂等（commit_chapter.IsChapterCompleted 短路）
	if err := PersistChapter(context.Background(), st, commitTool, 1, "初遇", body, a); err != nil {
		t.Errorf("re-import should be idempotent, got: %v", err)
	}
	prog2, _ := st.Progress.Load()
	if len(prog2.CompletedChapters) != 1 {
		t.Errorf("re-import duplicated completion: %+v", prog2.CompletedChapters)
	}
}
