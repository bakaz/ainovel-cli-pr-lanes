package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestSaveArcSummaryPersistsStyleRulesDialogueObjects(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume":     1,
		"arc":        2,
		"title":      "入山",
		"summary":    "主角完成入山试炼，确认后续追索方向。",
		"key_events": []string{"通过试炼", "发现旧案线索"},
		"character_snapshots": []map[string]any{
			{"name": "沈渊", "status": "存活", "motivation": "追查旧案"},
		},
		"style_rules": map[string]any{
			"prose": []string{"环境描写优先触觉和嗅觉", "动作戏用短句推进", "心理描写不解释结论"},
			"dialogue": []map[string]any{
				{"name": "沈渊", "rules": []string{"对话极简", "少用疑问句"}},
			},
			"taboos": []string{"避免章末长独白"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	rules, err := s.World.LoadStyleRules()
	if err != nil {
		t.Fatalf("LoadStyleRules: %v", err)
	}
	if rules == nil || len(rules.Dialogue) != 1 {
		t.Fatalf("expected one dialogue rule, got %+v", rules)
	}
	if rules.Dialogue[0].Name != "沈渊" || len(rules.Dialogue[0].Rules) != 2 {
		t.Fatalf("unexpected dialogue rule: %+v", rules.Dialogue[0])
	}
}

func TestSaveArcSummaryRejectsDialogueStringArray(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume":              1,
		"arc":                 2,
		"title":               "入山",
		"summary":             "主角完成入山试炼，确认后续追索方向。",
		"key_events":          []string{"通过试炼"},
		"character_snapshots": []map[string]any{},
		"style_rules": map[string]any{
			"prose":    []string{"环境描写优先触觉和嗅觉"},
			"dialogue": []string{"沈渊对话极简"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "style_rules.dialogue") {
		t.Fatalf("expected style_rules.dialogue validation error, got %v", err)
	}
}

func TestSaveArcSummaryRejectsMeteringPollutionInProse(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume":              1,
		"arc":                 2,
		"title":               "课",
		"summary":             "学校循环压成身体节律。",
		"key_events":          []string{"椅缘碾磨"},
		"character_snapshots": []map[string]any{},
		"style_rules": map[string]any{
			"prose": []string{
				"盆底基线从20升至22，用次/分记录安静被重写",
			},
			"dialogue": []map[string]any{
				{"name": "照料者", "rules": []string{"短句指令"}},
			},
			"taboos": []string{"避免盆底N次/分账本推进"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "pollution") {
		t.Fatalf("expected pollution validation error, got %v", err)
	}
}

func TestSaveArcSummaryAllowsAntiMeteringInTaboosOnly(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume":              1,
		"arc":                 2,
		"title":               "课",
		"summary":             "学校循环压成身体节律。",
		"key_events":          []string{"椅缘碾磨"},
		"character_snapshots": []map[string]any{},
		"style_rules": map[string]any{
			"prose": []string{
				"椅缘与门槛条立刻落到穴壁夹绞与渗液",
				"暗语释放写可预期的松，颠簸写无预告乱绞",
			},
			"dialogue": []map[string]any{
				{"name": "照料者", "rules": []string{"极短指令", "不解释"}},
			},
			"taboos": []string{"避免盆底N次/分与跳至XX账本推进", "避免意味着解释链"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestSaveArcSummaryAttachesCurrentStateBasis 结果附带按 entity 分组的 current state 基底，
// 快照明显与 current state 矛盾（存在装置却写"完好"）时输出 warning 但不拒绝保存。
func TestSaveArcSummaryAttachesCurrentStateBasis(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林砚", Field: "body_device.缠足", Value: "存在", UpdatedChapter: 10},
		{Entity: "林砚", Field: "status.道途", Value: "练气五层", UpdatedChapter: 12},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}

	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume":     1,
		"arc":        2,
		"title":      "入山",
		"summary":    "主角完成入山试炼。",
		"key_events": []string{"通过试炼"},
		"character_snapshots": []map[string]any{
			// 矛盾：current 有 body_device.缠足=存在，快照却写双腿完好。
			{"name": "林砚", "status": "双腿完好", "motivation": "继续追索"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute should not reject on loose conflict: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	basis, ok := result["current_state_basis"].(map[string]any)
	if !ok {
		t.Fatal("expected current_state_basis in result")
	}
	lin, ok := basis["林砚"].(map[string]any)
	if !ok || lin["body_device.缠足"] != "存在" {
		t.Fatalf("expected 林砚 body_device.缠足=存在 in basis, got %+v", basis)
	}
	warns, ok := result["snapshot_conflict_warnings"].([]any)
	if !ok || len(warns) == 0 {
		t.Fatalf("expected conflict warning, got %+v", result["snapshot_conflict_warnings"])
	}
	joined := ""
	for _, w := range warns {
		joined += w.(string)
	}
	if !strings.Contains(joined, "林砚") || !strings.Contains(joined, "完好") {
		t.Fatalf("conflict warning should mention entity and keyword, got %v", warns)
	}
}

// TestSaveArcSummaryNoConflictWithoutSnapshots 无快照或无 current state 时不产冲突 warning。
func TestSaveArcSummaryNoConflictWithoutSnapshots(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林砚", Field: "status.道途", Value: "练气五层", UpdatedChapter: 12},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}
	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume":     1,
		"arc":        2,
		"title":      "入山",
		"summary":    "主角完成入山试炼。",
		"key_events": []string{"通过试炼"},
		"character_snapshots": []map[string]any{
			{"name": "林砚", "status": "双腿完好", "motivation": "继续追索"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// current state 无"存在"类值 → 无冲突 warning。
	if _, ok := result["snapshot_conflict_warnings"]; ok {
		t.Fatalf("no conflict expected, got %+v", result["snapshot_conflict_warnings"])
	}
}
