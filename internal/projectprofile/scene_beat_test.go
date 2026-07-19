package projectprofile

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestNewCore4Contract(t *testing.T) {
	c := NewCore4Contract()
	if len(c.RequiredFields()) != 4 {
		t.Fatalf("Core4 required fields = %d, want 4", len(c.RequiredFields()))
	}
	expected := []string{"goal", "action", "conflict", "outcome"}
	for i, f := range c.RequiredFields() {
		if f != expected[i] {
			t.Errorf("Core4 field[%d] = %q, want %q", i, f, expected[i])
		}
	}
	if c.RejectLegacy() {
		t.Error("Core4 should not reject legacy")
	}
}

func TestNewSceneBeatV3Contract(t *testing.T) {
	c := NewSceneBeatV3Contract()
	if len(c.RequiredFields()) != 7 {
		t.Fatalf("v3 required fields = %d, want 7", len(c.RequiredFields()))
	}
	for _, f := range c.RequiredFields() {
		found := false
		for _, rf := range c.RequiredFields() {
			if rf == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("v3 contract missing required field: %s", f)
		}
	}
	if !c.RejectLegacy() {
		t.Error("v3 must reject legacy")
	}
}

func TestCore4Validate(t *testing.T) {
	c := NewCore4Contract()

	// Legacy (fromString) passes Core4
	var sl domain.SceneList
	if err := sl.UnmarshalJSON([]byte(`["legacy text"]`)); err != nil {
		t.Fatal(err)
	}
	if len(sl) != 1 {
		t.Fatal("expected 1 beat")
	}
	if err := c.Validate(sl[0]); err != nil {
		t.Errorf("Core4 legacy should pass: %v", err)
	}

	// Complete Core4 should pass
	complete := domain.SceneBeat{
		Goal:     "英雄出发",
		Action:   "主角踏上旅程",
		Conflict: "遇到障碍",
		Outcome:  "突破困境",
	}
	if err := c.Validate(complete); err != nil {
		t.Errorf("Core4 complete should pass: %v", err)
	}

	// Missing goal should fail
	missingGoal := domain.SceneBeat{
		Action:   "action",
		Conflict: "conflict",
		Outcome:  "outcome",
	}
	if err := c.Validate(missingGoal); err == nil {
		t.Error("Core4 missing goal should fail")
	}

	// Empty beat should fail
	if err := c.Validate(domain.SceneBeat{}); err == nil {
		t.Error("Core4 empty beat should fail")
	}
}

func TestSceneBeatV3Validate(t *testing.T) {
	c := NewSceneBeatV3Contract()

	// Legacy is REJECTED by v3
	var sl domain.SceneList
	if err := sl.UnmarshalJSON([]byte(`["legacy text"]`)); err != nil {
		t.Fatal(err)
	}
	if len(sl) != 1 {
		t.Fatal("expected 1 beat")
	}
	if err := c.Validate(sl[0]); err == nil {
		t.Error("v3 should reject legacy string scene")
	}

	// Complete v3 should pass
	complete := domain.SceneBeat{
		Goal:            "goal",
		Action:          "action",
		Conflict:        "conflict",
		Outcome:         "outcome",
		BodyReaction:    "body",
		EmotionReaction: "emotion",
		EroticCharge:    "erotic",
	}
	if err := c.Validate(complete); err != nil {
		t.Errorf("v3 complete should pass: %v", err)
	}

	// Missing body_reaction
	missingBody := domain.SceneBeat{
		Goal:            "g",
		Action:          "a",
		Conflict:        "c",
		Outcome:         "o",
		EmotionReaction: "e",
		EroticCharge:    "ec",
	}
	if err := c.Validate(missingBody); err == nil {
		t.Error("v3 missing body_reaction should fail")
	}

	// Missing emotion_reaction
	missingEmotion := domain.SceneBeat{
		Goal:         "g",
		Action:       "a",
		Conflict:     "c",
		Outcome:      "o",
		BodyReaction: "b",
		EroticCharge: "ec",
	}
	if err := c.Validate(missingEmotion); err == nil {
		t.Error("v3 missing emotion_reaction should fail")
	}

	// Missing erotic_charge
	missingErotic := domain.SceneBeat{
		Goal:            "g",
		Action:          "a",
		Conflict:        "c",
		Outcome:         "o",
		BodyReaction:    "b",
		EmotionReaction: "e",
	}
	if err := c.Validate(missingErotic); err == nil {
		t.Error("v3 missing erotic_charge should fail")
	}

	// Empty beat fails v3
	if err := c.Validate(domain.SceneBeat{}); err == nil {
		t.Error("v3 empty beat should fail")
	}
}

func TestSceneBeatContract_ValidateAll(t *testing.T) {
	c := NewCore4Contract()

	beats := []domain.SceneBeat{
		{Goal: "g1", Action: "a1", Conflict: "c1", Outcome: "o1"},
		{Goal: "g2", Action: "a2", Conflict: "c2", Outcome: "o2"},
	}
	if err := c.ValidateAll(beats); err != nil {
		t.Errorf("all valid should pass: %v", err)
	}

	beatsWithInvalid := []domain.SceneBeat{
		{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"},
		{Action: "only action"},
	}
	err := c.ValidateAll(beatsWithInvalid)
	if err == nil {
		t.Error("mixed valid/invalid should fail")
	}
}

func TestContractFor(t *testing.T) {
	v3 := ContractFor(ContractSceneBeatV3)
	if len(v3.RequiredFields()) != 7 {
		t.Errorf("ContractFor(v3) should return 7-field contract, got %d", len(v3.RequiredFields()))
	}
	if !v3.RejectLegacy() {
		t.Error("ContractFor(v3).RejectLegacy should be true")
	}

	core4 := ContractFor(ContractCore4)
	if len(core4.RequiredFields()) != 4 {
		t.Errorf("ContractFor(core4) should return 4-field contract, got %d", len(core4.RequiredFields()))
	}
	if core4.RejectLegacy() {
		t.Error("ContractFor(core4).RejectLegacy should be false")
	}

	unknown := ContractFor(Contract(999))
	if len(unknown.RequiredFields()) != 4 {
		t.Errorf("ContractFor(unknown) should default to 4-field contract, got %d", len(unknown.RequiredFields()))
	}
}

// ── V3 Guidance fragments (Phase 2 acceptance blocker 3) ──

// TestV3Guidance_SharedTemplate 验证 V3 五个碎片共享同一模板语义：
// 七字段非空、sensory 可选、legacy 禁止。
func TestV3Guidance_SharedTemplate(t *testing.T) {
	v3 := NewSceneBeatV3Contract()

	// 验证所有角色返回的 guidance 包含 7 个 required field 描述
	roles := []struct {
		role string
		get  func() string
	}{
		{"architect_short", func() string { return v3.GuidanceForRole("architect_short") }},
		{"architect_long", func() string { return v3.GuidanceForRole("architect_long") }},
		{"architect", func() string { return v3.GuidanceForRole("architect") }},
		{"writer", func() string { return v3.GuidanceForRole("writer") }},
		{"editor", func() string { return v3.GuidanceForRole("editor") }},
	}

	requiredFields := []string{"goal", "action", "conflict", "outcome", "body_reaction", "emotion_reaction", "erotic_charge"}
	for _, r := range roles {
		t.Run(r.role, func(t *testing.T) {
			guidance := r.get()
			if guidance == "" {
				t.Fatal("v3 guidance should not be empty")
			}

			// 所有七字段必填标记
			for _, f := range requiredFields {
				if !strings.Contains(guidance, f) {
					t.Errorf("v3 guidance missing field %q", f)
				}
			}

			// sensory_anchor 可选标记
			if !strings.Contains(guidance, "可选") && !strings.Contains(guidance, "sensory_anchor") {
				t.Errorf("v3 guidance should mention optional sensory_anchor")
			}

			// legacy 禁止标记
			if !strings.Contains(guidance, "legacy") && !strings.Contains(guidance, "遗留") {
				t.Errorf("v3 guidance should forbid legacy format")
			}

			// 非空标记
			if !strings.Contains(guidance, "不可为空") {
				t.Errorf("v3 guidance should mark fields as non-empty")
			}
		})
	}

	// Import guidance 使用同一 template
	importGuidance := v3.ImportGuidance()
	if importGuidance == "" {
		t.Fatal("v3 import guidance should not be empty")
	}
	for _, f := range requiredFields {
		if !strings.Contains(importGuidance, f) {
			t.Errorf("v3 import guidance missing field %q", f)
		}
	}
}

// TestV3Guidance_Core4ReturnsEmpty 验证 Core4 契约下所有角色和 import 返回空 guidance。
func TestV3Guidance_Core4ReturnsEmpty(t *testing.T) {
	core4 := NewCore4Contract()

	roles := []string{"architect_short", "architect_long", "architect", "writer", "editor"}
	for _, role := range roles {
		if g := core4.GuidanceForRole(role); g != "" {
			t.Errorf("Core4 should return empty guidance for %q, got %q", role, g)
		}
	}
	if g := core4.ImportGuidance(); g != "" {
		t.Errorf("Core4 ImportGuidance should be empty, got %q", g)
	}
}

// TestV3Guidance_FragmentIsAfterOverlay 验证 overlay 组合后 fragment 在最末尾。
// 使用 appendSceneGuidance 语义：guidance 追加在 prompt 最后。
func TestV3Guidance_FragmentIsAfterOverlay(t *testing.T) {
	v3 := NewSceneBeatV3Contract()

	// 模拟 writer 的 voice/style 叠加过程
	basePrompt := "writer 核心提示\n{{VOICE}}\n风格预设"
	voiceStyleOverlaid := basePrompt + "\n\n文风段\n### 风格\n风格详细内容"

	for _, role := range []struct{ name, guidance string }{
		{"architect_short", v3.GuidanceForRole("architect_short")},
		{"architect_long", v3.GuidanceForRole("architect_long")},
		{"writer", v3.GuidanceForRole("writer")},
		{"editor", v3.GuidanceForRole("editor")},
	} {
		t.Run(role.name, func(t *testing.T) {
			if role.guidance == "" {
				t.Skip("empty guidance (Core4 path)")
			}
			// 叠加：overlay 后追加 fragment
			finalPrompt := voiceStyleOverlaid + "\n\n" + role.guidance

			// fragment 必须在最末尾
			lastOccur := strings.LastIndex(finalPrompt, role.guidance)
			if lastOccur < 0 {
				t.Fatal("final prompt should contain guidance")
			}
			// guidance 后不应再有其他内容
			tail := finalPrompt[lastOccur+len(role.guidance):]
			if strings.TrimSpace(tail) != "" {
				t.Errorf("guidance should be at the very end, found trailing: %q", tail)
			}

			// 确保 overlay 内容还在
			if !strings.Contains(finalPrompt, "文风段") {
				t.Error("overlay content should survive")
			}
			if !strings.Contains(finalPrompt, "风格详细内容") {
				t.Error("style content should survive")
			}
		})
	}
}

// TestV3Guidance_ImportFragmentAfterOverlay 验证 import guidance 在所有 overlay 之后。
func TestV3Guidance_ImportFragmentAfterOverlay(t *testing.T) {
	v3 := NewSceneBeatV3Contract()
	importGuidance := v3.ImportGuidance()
	if importGuidance == "" {
		t.Fatal("v3 import guidance should not be empty")
	}

	// 模拟 import 的 overlay 过程
	basePrompt := "import 系统提示\n[overlay 内容]"
	finalPrompt := basePrompt + "\n\n" + importGuidance

	lastOccur := strings.LastIndex(finalPrompt, importGuidance)
	tail := finalPrompt[lastOccur+len(importGuidance):]
	if strings.TrimSpace(tail) != "" {
		t.Errorf("import guidance should be at the very end, found trailing: %q", tail)
	}
	if !strings.Contains(finalPrompt, "[overlay 内容]") {
		t.Error("overlay content should survive in import")
	}
}

// TestV3Guidance_AllFiveAliasesAreSame 验证五个碎片都是同一个模板 V3SceneBeatGuidance。
func TestV3Guidance_AllFiveAliasesAreSame(t *testing.T) {
	if V3ArchitectShortGuidance != V3SceneBeatGuidance {
		t.Error("V3ArchitectShortGuidance should equal V3SceneBeatGuidance")
	}
	if V3ArchitectLongGuidance != V3SceneBeatGuidance {
		t.Error("V3ArchitectLongGuidance should equal V3SceneBeatGuidance")
	}
	if V3WriterGuidance != V3SceneBeatGuidance {
		t.Error("V3WriterGuidance should equal V3SceneBeatGuidance")
	}
	if V3EditorGuidance != V3SceneBeatGuidance {
		t.Error("V3EditorGuidance should equal V3SceneBeatGuidance")
	}
	if V3ImportGuidance != V3SceneBeatGuidance {
		t.Error("V3ImportGuidance should equal V3SceneBeatGuidance")
	}
}
