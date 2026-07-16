package domain

import (
	"encoding/json"
	"testing"
)

func TestStyleRulesCompassMigratesLegacyFormat(t *testing.T) {
	// 旧单体格式（顶层有 volume/arc/prose）
	oldData := `{
		"volume": 1,
		"arc": 2,
		"prose": ["短句为主", "少用比喻"],
		"dialogue": [{"name":"张三","rules":["粗犷","直白"]}],
		"taboos": ["不用网络用语"],
		"updated_at": "2024-01-15T10:00:00Z"
	}`
	var compass WritingStyleRulesCompass
	if err := json.Unmarshal([]byte(oldData), &compass); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if compass.Current == nil {
		t.Fatal("legacy migration should populate current")
	}
	if compass.Long != nil {
		t.Fatal("legacy migration should NOT populate long")
	}
	if compass.Current.Volume != 1 || compass.Current.Arc != 2 {
		t.Fatalf("volume/arc mismatch: %+v", compass.Current)
	}
	if len(compass.Current.Prose) != 2 || compass.Current.Prose[0] != "短句为主" {
		t.Fatalf("prose migration failed: %+v", compass.Current.Prose)
	}
	if len(compass.Current.Dialogue) != 1 || compass.Current.Dialogue[0].Name != "张三" {
		t.Fatalf("dialogue migration failed: %+v", compass.Current.Dialogue)
	}
	if len(compass.Current.Taboos) != 1 || compass.Current.Taboos[0] != "不用网络用语" {
		t.Fatalf("taboos migration failed: %+v", compass.Current.Taboos)
	}
	if compass.Current.LastUpdated != "2024-01-15T10:00:00Z" {
		t.Fatalf("last_updated migration failed: %s", compass.Current.LastUpdated)
	}
}

func TestStyleRulesCompassNewFormatRoundTrip(t *testing.T) {
	compass := WritingStyleRulesCompass{
		Long: &StyleRulesLong{
			Prose:  []string{"跨弧稳定规则"},
			Taboos: []string{"全局禁忌"},
		},
		Current: &StyleRulesCurrent{
			Volume: 1,
			Arc:    3,
			Prose:  []string{"当前弧特化"},
			Dialogue: []CharacterVoice{
				{Name: "李四", Rules: []string{"话少"}},
			},
			LastUpdated: "2024-02-20T15:30:00Z",
		},
	}
	data, err := json.Marshal(compass)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got WritingStyleRulesCompass
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Long == nil || got.Current == nil {
		t.Fatal("round trip lost layers")
	}
	if got.Long.Prose[0] != "跨弧稳定规则" {
		t.Fatalf("long prose: %v", got.Long.Prose)
	}
	if got.Current.Arc != 3 || got.Current.Prose[0] != "当前弧特化" {
		t.Fatalf("current: %+v", got.Current)
	}
}

func TestStyleRulesLongValidateRejectsForbiddenContent(t *testing.T) {
	tests := []struct {
		name    string
		long    StyleRulesLong
		wantErr bool
	}{
		{
			name: "valid long",
			long: StyleRulesLong{
				Prose:  []string{"保持冷峻克制", "多用短句"},
				Taboos: []string{"避免直白解释"},
			},
			wantErr: false,
		},
		{
			name: "contains round pattern",
			long: StyleRulesLong{
				Prose:  []string{"第三轮战斗要激烈"},
				Taboos: []string{},
			},
			wantErr: true,
		},
		{
			name: "contains loop count",
			long: StyleRulesLong{
				Prose:  []string{"循环次数不超过3次"},
				Taboos: []string{},
			},
			wantErr: true,
		},
		{
			name: "contains parameter threshold",
			long: StyleRulesLong{
				Prose:  []string{"当精神力阈值超过50时"},
				Taboos: []string{},
			},
			wantErr: true,
		},
		{
			name: "contains config parameter",
			long: StyleRulesLong{
				Prose:  []string{"参数配置要合理"},
				Taboos: []string{},
			},
			wantErr: true,
		},
		{
			name: "forbidden in dialogue rules",
			long: StyleRulesLong{
				Dialogue: []CharacterVoice{
					{Name: "角色", Rules: []string{"第2轮对白要简洁"}},
				},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.long.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.name, err)
			}
		})
	}
}

func TestConflictsWithLongReturnsNil(t *testing.T) {
	// ConflictsWithLong 现在不做文本级冲突拒绝——不同层级的增量规则
	// 不属于硬冲突，由 long 优先的上下文合并保证一致性。
	tests := []struct {
		name    string
		long    *StyleRulesLong
		current *StyleRulesCurrent
	}{
		{
			name:    "both nil",
			long:    nil,
			current: nil,
		},
		{
			name: "different prose is not a hard conflict",
			long: &StyleRulesLong{Prose: []string{"短句"}},
			current: &StyleRulesCurrent{
				Volume: 1, Arc: 1,
				Prose: []string{"长句"},
			},
		},
		{
			name: "same prose not a conflict",
			long: &StyleRulesLong{Prose: []string{"短句"}},
			current: &StyleRulesCurrent{
				Volume: 1, Arc: 1,
				Prose: []string{"短句"},
			},
		},
		{
			name: "current-only no conflict",
			long: nil,
			current: &StyleRulesCurrent{
				Volume: 1, Arc: 1,
				Prose: []string{"规则"},
			},
		},
		{
			name: "different dialogue not a hard conflict",
			long: &StyleRulesLong{
				Dialogue: []CharacterVoice{{Name: "角色A", Rules: []string{"粗犷"}}},
			},
			current: &StyleRulesCurrent{
				Volume: 1, Arc: 1,
				Dialogue: []CharacterVoice{{Name: "角色A", Rules: []string{"文雅"}}},
			},
		},
		{
			name: "different taboos not a hard conflict",
			long:    &StyleRulesLong{Taboos: []string{"避免解释"}},
			current: &StyleRulesCurrent{Volume: 1, Arc: 1, Taboos: []string{"避免描述"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ConflictsWithLong(tc.current, tc.long)
			if err != nil {
				t.Fatalf("expected nil (no hard conflict), got: %v", err)
			}
		})
	}
}

func TestStyleRulesCompassHasContent(t *testing.T) {
	tests := []struct {
		name     string
		compass  *WritingStyleRulesCompass
		expected bool
	}{
		{"nil compass", nil, false},
		{"empty compass", &WritingStyleRulesCompass{}, false},
		{"only long prose", &WritingStyleRulesCompass{Long: &StyleRulesLong{Prose: []string{"规则"}}}, true},
		{"only long dialogue", &WritingStyleRulesCompass{Long: &StyleRulesLong{Dialogue: []CharacterVoice{{Name: "A", Rules: []string{"规则"}}}}}, true},
		{"only long taboos", &WritingStyleRulesCompass{Long: &StyleRulesLong{Taboos: []string{"禁忌"}}}, true},
		{"only current prose", &WritingStyleRulesCompass{Current: &StyleRulesCurrent{Volume: 1, Arc: 1, Prose: []string{"规则"}}}, true},
		{"empty long and current", &WritingStyleRulesCompass{Long: &StyleRulesLong{}, Current: &StyleRulesCurrent{Volume: 1, Arc: 1}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.compass.HasContent()
			if got != tc.expected {
				t.Fatalf("HasContent=%v, want %v", got, tc.expected)
			}
		})
	}
}
