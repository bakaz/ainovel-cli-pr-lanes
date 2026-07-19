package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSceneBeatIsLegacy(t *testing.T) {
	tests := []struct {
		name string
		beat SceneBeat
		want bool
	}{
		{"fromString 标记才是 legacy", SceneBeat{Action: "主角发现线索", fromString: true}, true},
		{"仅 action 的 object 不是 legacy", SceneBeat{Action: "主角发现线索"}, false},
		{"空 action 不算 legacy", SceneBeat{}, false},
		{"完整对象不是 legacy", SceneBeat{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"}, false},
		{"部分填充不是 legacy", SceneBeat{Goal: "揭示真相", Action: "主角发现线索"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.beat.IsLegacy(); got != tt.want {
				t.Errorf("IsLegacy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSceneBeatText(t *testing.T) {
	beat := SceneBeat{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"}
	text := beat.Text()
	if text != "揭示真相 主角发现线索 反派阻挠 找到关键证据" {
		t.Errorf("Text() = %q, want %q", text, "揭示真相 主角发现线索 反派阻挠 找到关键证据")
	}

	// legacy 场景
	legacy := SceneBeat{Action: "主角发现线索", fromString: true}
	if got := legacy.Text(); got != "主角发现线索" {
		t.Errorf("legacy Text() = %q, want %q", got, "主角发现线索")
	}

	// empty
	var empty SceneBeat
	if got := empty.Text(); got != "" {
		t.Errorf("empty Text() = %q, want ''", got)
	}
}

func TestSceneBeatValidateRequired(t *testing.T) {
	// legacy 场景跳过校验
	legacy := SceneBeat{Action: "主角发现线索", fromString: true}
	if err := legacy.ValidateRequired(); err != nil {
		t.Errorf("legacy ValidateRequired should pass: %v", err)
	}

	// action-only object 必须拒绝（不能当 legacy）
	actionOnly := SceneBeat{Action: "主角发现线索"}
	if err := actionOnly.ValidateRequired(); err == nil {
		t.Error("action-only object should fail ValidateRequired")
	}

	// 完整对象通过
	full := SceneBeat{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"}
	if err := full.ValidateRequired(); err != nil {
		t.Errorf("full ValidateRequired should pass: %v", err)
	}

	// 缺 goal
	noGoal := SceneBeat{Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"}
	if err := noGoal.ValidateRequired(); err == nil || err.Error() != "goal: required" {
		t.Errorf("expected 'goal: required', got %v", err)
	}

	// 缺 action
	noAction := SceneBeat{Goal: "揭示真相", Conflict: "反派阻挠", Outcome: "找到关键证据"}
	if err := noAction.ValidateRequired(); err == nil || err.Error() != "action: required" {
		t.Errorf("expected 'action: required', got %v", err)
	}

	// 缺 conflict
	noConflict := SceneBeat{Goal: "揭示真相", Action: "主角发现线索", Outcome: "找到关键证据"}
	if err := noConflict.ValidateRequired(); err == nil || err.Error() != "conflict: required" {
		t.Errorf("expected 'conflict: required', got %v", err)
	}

	// 缺 outcome
	noOutcome := SceneBeat{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠"}
	if err := noOutcome.ValidateRequired(); err == nil || err.Error() != "outcome: required" {
		t.Errorf("expected 'outcome: required', got %v", err)
	}
}

func TestSceneListUnmarshalJSON(t *testing.T) {
	// 旧格式 []string
	var legacy SceneList
	if err := json.Unmarshal([]byte(`["分道","追索","汇合"]`), &legacy); err != nil {
		t.Fatalf("Unmarshal []string: %v", err)
	}
	if len(legacy) != 3 {
		t.Fatalf("expected 3 items, got %d", len(legacy))
	}
	for i, sc := range legacy {
		if !sc.IsLegacy() {
			t.Errorf("item %d should be legacy, got %+v", i, sc)
		}
	}
	if legacy[0].Action != "分道" || legacy[1].Action != "追索" || legacy[2].Action != "汇合" {
		t.Errorf("unexpected legacy content: %+v", legacy)
	}

	// 新格式 []object
	var objects SceneList
	if err := json.Unmarshal([]byte(`[
		{"goal":"揭示真相","action":"主角发现线索","conflict":"反派阻挠","outcome":"找到关键证据"},
		{"goal":"危机升级","action":"反派反击","conflict":"主角受伤","outcome":"同伴救援","sensory_anchor":"雨夜巷战"}
	]`), &objects); err != nil {
		t.Fatalf("Unmarshal []object: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2 items, got %d", len(objects))
	}
	if objects[0].Goal != "揭示真相" || objects[0].Action != "主角发现线索" {
		t.Errorf("unexpected object[0]: %+v", objects[0])
	}
	if objects[1].SensoryAnchor != "雨夜巷战" {
		t.Errorf("unexpected object[1].sensory_anchor: %q", objects[1].SensoryAnchor)
	}

	// 混合格式
	var mixed SceneList
	if err := json.Unmarshal([]byte(`[
		"旧格式场景",
		{"goal":"揭示真相","action":"主角发现线索","conflict":"反派阻挠","outcome":"找到关键证据"}
	]`), &mixed); err != nil {
		t.Fatalf("Unmarshal mixed: %v", err)
	}
	if len(mixed) != 2 {
		t.Fatalf("expected 2 items, got %d", len(mixed))
	}
	if !mixed[0].IsLegacy() || mixed[0].Action != "旧格式场景" {
		t.Errorf("expected legacy first item, got %+v", mixed[0])
	}
	if mixed[1].IsLegacy() || mixed[1].Goal != "揭示真相" {
		t.Errorf("expected object second item, got %+v", mixed[1])
	}

	// null 元素拒绝
	var withNull SceneList
	if err := json.Unmarshal([]byte(`["scene",null]`), &withNull); err == nil {
		t.Fatal("expected error for [\"scene\",null]")
	}
	var allNull SceneList
	if err := json.Unmarshal([]byte(`[null]`), &allNull); err == nil {
		t.Fatal("expected error for [null]")
	}
	// null 本身仍是有效的（nil）
	var nullList SceneList
	if err := json.Unmarshal([]byte(`null`), &nullList); err != nil {
		t.Fatalf("null should be valid: %v", err)
	}
	if nullList != nil {
		t.Fatal("expected nil for null input")
	}
	// legacy string 仍可
	var legacy2 SceneList
	if err := json.Unmarshal([]byte(`["分道","追索"]`), &legacy2); err != nil {
		t.Fatalf("Unmarshal []string should still work: %v", err)
	}
	if len(legacy2) != 2 || !legacy2[0].IsLegacy() {
		t.Fatal("legacy string items should be fromString=true")
	}
}

func TestSceneListMarshalJSON(t *testing.T) {
	// 序列化 legacy 列表 → 仍写 string，避免隐式全书 object 化
	legacy := SceneList{
		{Action: "分道", fromString: true},
		{Action: "追索", fromString: true},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal legacy: %v", err)
	}
	if string(data) != `["分道","追索"]` {
		t.Fatalf("legacy marshal should keep strings, got %s", data)
	}
	var roundTrip SceneList
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if len(roundTrip) != 2 || !roundTrip[0].IsLegacy() || roundTrip[0].Action != "分道" {
		t.Errorf("round-trip content mismatch: %+v", roundTrip)
	}

	// 序列化 object 列表
	objects := SceneList{
		{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"},
		{Goal: "危机升级", Action: "反派反击", Conflict: "主角受伤", Outcome: "同伴救援"},
	}
	data, err = json.Marshal(objects)
	if err != nil {
		t.Fatalf("Marshal objects: %v", err)
	}
	var roundTrip2 SceneList
	if err := json.Unmarshal(data, &roundTrip2); err != nil {
		t.Fatalf("Unmarshal round-trip2: %v", err)
	}
	if roundTrip2[0].Goal != "揭示真相" || roundTrip2[1].Outcome != "同伴救援" {
		t.Errorf("object round-trip content mismatch: %+v", roundTrip2)
	}
}

func TestSceneListMarshalUnmarshalRoundTrip(t *testing.T) {
	// 验证 marshal → unmarshal → marshal 产生相同 JSON
	original := SceneList{
		{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"},
	}
	data1, _ := json.Marshal(original)
	var decoded SceneList
	if err := json.Unmarshal(data1, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	data2, _ := json.Marshal(decoded)
	if string(data1) != string(data2) {
		t.Errorf("round-trip JSON mismatch:\n  first:  %s\n  second: %s", string(data1), string(data2))
	}
}

func TestFlattenScenes(t *testing.T) {
	scenes := SceneList{
		{Goal: "揭示真相", Action: "主角发现线索", Conflict: "反派阻挠", Outcome: "找到关键证据"},
		{Action: "旧格式场景", fromString: true},
	}
	flattened := FlattenScenes(scenes)
	if flattened != "揭示真相 主角发现线索 反派阻挠 找到关键证据 旧格式场景" {
		t.Errorf("FlattenScenes() = %q", flattened)
	}

	// 空列表
	if got := FlattenScenes(SceneList{}); got != "" {
		t.Errorf("empty FlattenScenes = %q", got)
	}
}

func TestOutlineEntryMarshalUnmarshalScenes(t *testing.T) {
	// 包含 scenes 的完整章节 round-trip
	entry := OutlineEntry{
		Chapter:   1,
		Title:     "开局",
		CoreEvent: "主角开始冒险",
		Hook:      "悬念浮现",
		Scenes: SceneList{
			{Goal: "建立世界观", Action: "主角到达新地点", Conflict: "遇到守门人阻拦", Outcome: "通过考验进入"},
			{Goal: "引入冲突", Action: "反派出现", Conflict: "对峙", Outcome: "主角受伤逃脱"},
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded OutlineEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Chapter != 1 || decoded.Title != "开局" {
		t.Errorf("basic fields mismatch: %+v", decoded)
	}
	if len(decoded.Scenes) != 2 {
		t.Fatalf("expected 2 scenes, got %d", len(decoded.Scenes))
	}
	if decoded.Scenes[0].Goal != "建立世界观" || decoded.Scenes[1].Outcome != "主角受伤逃脱" {
		t.Errorf("scenes content mismatch: %+v", decoded.Scenes)
	}
}

func TestSceneBeatBodyReactionEmotionReactionRoundTrip(t *testing.T) {
	// body_reaction / emotion_reaction 字段 round-trip 与 omitempty
	entry := OutlineEntry{
		Chapter:   1,
		Title:     "开局",
		CoreEvent: "开始",
		Hook:      "悬念",
		Scenes: SceneList{
			{
				Goal:            "揭示真相",
				Action:          "质问",
				Conflict:        "对方否认",
				Outcome:         "发现新线索",
				BodyReaction:    "握紧拳头，额角冒出冷汗",
				EmotionReaction: "难以置信，继而愤怒",
			},
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// 验证 JSON 中包含新字段
	if !strings.Contains(string(data), "body_reaction") {
		t.Errorf("JSON should contain body_reaction, got: %s", data)
	}
	if !strings.Contains(string(data), "emotion_reaction") {
		t.Errorf("JSON should contain emotion_reaction, got: %s", data)
	}
	var decoded OutlineEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Scenes[0].BodyReaction != "握紧拳头，额角冒出冷汗" {
		t.Errorf("BodyReaction mismatch: %q", decoded.Scenes[0].BodyReaction)
	}
	if decoded.Scenes[0].EmotionReaction != "难以置信，继而愤怒" {
		t.Errorf("EmotionReaction mismatch: %q", decoded.Scenes[0].EmotionReaction)
	}
}

func TestSceneBeatNewFieldsOmitempty(t *testing.T) {
	// 空的 body_reaction / emotion_reaction 不序列化
	entry := OutlineEntry{
		Chapter: 1,
		Title:   "测试",
		Scenes: SceneList{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"},
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "body_reaction") {
		t.Errorf("empty body_reaction should be omitted, got: %s", data)
	}
	if strings.Contains(string(data), "emotion_reaction") {
		t.Errorf("empty emotion_reaction should be omitted, got: %s", data)
	}
}

func TestSceneBeatTextWithNewFields(t *testing.T) {
	beat := SceneBeat{
		Goal:            "g",
		Action:          "a",
		Conflict:        "c",
		Outcome:         "o",
		BodyReaction:    "br",
		EmotionReaction: "er",
	}
	text := beat.Text()
	if text != "g a c o br er" {
		t.Errorf("Text() = %q, want %q", text, "g a c o br er")
	}
	// legacy 场景新字段不参与（legacy 只有 Action）
	legacy := SceneBeat{Action: "旧场景", fromString: true}
	if got := legacy.Text(); got != "旧场景" {
		t.Errorf("legacy Text() = %q, want %q", got, "旧场景")
	}
	// 验证 NewFields 不改变 ValidateRequired
	if err := beat.ValidateRequired(); err != nil {
		t.Errorf("ValidateRequired 应通过: %v", err)
	}
}

func TestSceneBeatNewFieldsInUnmarshalObject(t *testing.T) {
	var list SceneList
	err := json.Unmarshal([]byte(`[
		{"goal":"g","action":"a","conflict":"c","outcome":"o","body_reaction":"br","emotion_reaction":"er"}
	]`), &list)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	if list[0].BodyReaction != "br" {
		t.Errorf("BodyReaction = %q", list[0].BodyReaction)
	}
	if list[0].EmotionReaction != "er" {
		t.Errorf("EmotionReaction = %q", list[0].EmotionReaction)
	}
	if list[0].IsLegacy() {
		t.Errorf("object with new fields should not be legacy")
	}
}

// ── EroticCharge Phase 1 tests ──

func TestSceneBeatEroticChargeRoundTrip(t *testing.T) {
	// EroticCharge field round-trip via JSON
	entry := OutlineEntry{
		Chapter: 1,
		Title:   "亲密场景",
		Scenes: SceneList{
			{
				Goal:            "推进情感",
				Action:          "两人独处",
				Conflict:        "内心挣扎",
				Outcome:         "关系升温",
				BodyReaction:    "呼吸急促",
				EmotionReaction: "欲拒还迎",
				EroticCharge:    "暧昧气氛逐渐升温，指尖轻触引发战栗",
			},
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Verify erotic_charge in JSON
	if !strings.Contains(string(data), "erotic_charge") {
		t.Errorf("JSON should contain erotic_charge, got: %s", data)
	}
	// Round-trip
	var decoded OutlineEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Scenes[0].EroticCharge != "暧昧气氛逐渐升温，指尖轻触引发战栗" {
		t.Errorf("EroticCharge mismatch: %q", decoded.Scenes[0].EroticCharge)
	}
}

func TestSceneBeatEroticChargeOmitempty(t *testing.T) {
	// Empty erotic_charge should be omitted
	entry := OutlineEntry{
		Chapter: 1,
		Title:   "普通场景",
		Scenes: SceneList{
			{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"},
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "erotic_charge") {
		t.Errorf("empty erotic_charge should be omitted, got: %s", data)
	}
}

func TestSceneBeatTextWithEroticCharge(t *testing.T) {
	// Text() should include erotic_charge after emotion_reaction
	beat := SceneBeat{
		Goal:            "g",
		Action:          "a",
		Conflict:        "c",
		Outcome:         "o",
		BodyReaction:    "br",
		EmotionReaction: "er",
		EroticCharge:    "ec",
	}
	text := beat.Text()
	if text != "g a c o br er ec" {
		t.Errorf("Text() = %q, want %q", text, "g a c o br er ec")
	}
	// Legacy should not include erotic_charge
	legacy := SceneBeat{Action: "旧场景", fromString: true}
	if got := legacy.Text(); got != "旧场景" {
		t.Errorf("legacy Text() = %q, want %q", got, "旧场景")
	}
	// Partial: only erotic_charge without body_reaction/emotion_reaction
	partial := SceneBeat{
		Goal:         "g",
		Action:       "a",
		Conflict:     "c",
		Outcome:      "o",
		EroticCharge: "ec",
	}
	partialText := partial.Text()
	if partialText != "g a c o ec" {
		t.Errorf("partial Text() = %q, want %q", partialText, "g a c o ec")
	}
}

func TestSceneBeatEroticChargeUnmarshalObject(t *testing.T) {
	var list SceneList
	err := json.Unmarshal([]byte(`[
		{"goal":"g","action":"a","conflict":"c","outcome":"o","body_reaction":"br","emotion_reaction":"er","erotic_charge":"充满张力"}
	]`), &list)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	if list[0].EroticCharge != "充满张力" {
		t.Errorf("EroticCharge = %q", list[0].EroticCharge)
	}
	if list[0].IsLegacy() {
		t.Errorf("object with erotic_charge should not be legacy")
	}
}

func TestSceneBeatEroticChargeLegacyBytesUnchanged(t *testing.T) {
	// Legacy string scenes must remain unchanged when marshaled
	legacy := SceneList{
		{Action: "经典场景描述", fromString: true},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Legacy should still be a JSON string, not object
	if string(data) != `["经典场景描述"]` {
		t.Errorf("legacy marshal should keep raw string, got: %s", data)
	}
	// Round-trip: unmarshal should still be legacy
	var roundTrip SceneList
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(roundTrip) != 1 || !roundTrip[0].IsLegacy() || roundTrip[0].Action != "经典场景描述" {
		t.Errorf("round-trip legacy broken: %+v", roundTrip)
	}
	// Adding EroticCharge to mixed list should not change legacy bytes
	mixed := SceneList{
		{Action: "旧场景", fromString: true},
		{
			Goal:         "g",
			Action:       "a",
			Conflict:     "c",
			Outcome:      "o",
			EroticCharge: "ec",
		},
	}
	mixedData, err := json.Marshal(mixed)
	if err != nil {
		t.Fatalf("Marshal mixed: %v", err)
	}
	if !strings.Contains(string(mixedData), `"旧场景"`) {
		t.Errorf("mixed marshal should keep legacy raw string: %s", mixedData)
	}
	if !strings.Contains(string(mixedData), "erotic_charge") {
		t.Errorf("mixed marshal should include erotic_charge: %s", mixedData)
	}
}
