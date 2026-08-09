package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// migratedCharacterState 与 workspace/output/novel/meta/character_state.json 迁移后
// 形状一致：entity=女孩（主角正式名，非"主角"占位——迁移工具从 characters.json 解析）。
func migratedCharacterState() []domain.CharacterStateEntry {
	return []domain.CharacterStateEntry{
		{Entity: "女孩", Field: "body_device.左乳榨乳器", Value: "左乳永久榨乳器固定，持续榨乳", UpdatedChapter: 35, Evidence: "左乳的榨乳器还挂在原位"},
		{Entity: "女孩", Field: "body_device.导尿管", Value: "导尿管与尿道融合，排尿由装置控制", UpdatedChapter: 15, Evidence: "导尿管从尿道口延伸出来"},
		{Entity: "女孩", Field: "body_device.缠足", Value: "脚骨液压折断后缠足缩小，永久改变行走方式", UpdatedChapter: 28, Evidence: "一双精钢打造的马蹄形高跟鞋被套在了女孩的脚上"},
		{Entity: "女孩", Field: "body_device.折叠腿", Value: "双腿被大腿环和钢杆永久折叠固定，只能爬行移动", UpdatedChapter: 32, Evidence: "双腿被大腿环和钢杆永久折叠固定"},
		{Entity: "女孩", Field: "body_device.背后电源", Value: "背后电源为全身装置供电，需定时充电", UpdatedChapter: 126, Evidence: "背后电源，后腰的那块皮肤温温的"},
		{Entity: "女孩", Field: "health.高潮能力", Value: "频繁寸止磨损高潮能力，完整释放越来越难", UpdatedChapter: 139, Evidence: "第六次高潮衰减暗示完整释放正变得越来越难"},
		{Entity: "女孩", Field: "health.痛快混淆通路", Value: "痛快混淆药建立的痛觉-快感交叉通路；效果逐渐衰减，不会一直持续", UpdatedChapter: 99, Evidence: "痛快混淆药在脊髓背角建立痛觉-快感交叉通路"},
		{Entity: "女孩", Field: "body_device.口塞", Value: "口塞与通气口收窄（ch44 安装），ch80 起由项圈链接管", UpdatedChapter: 80, Evidence: "口塞安装完成。通气口收窄"},
		{Entity: "女孩", Field: "status.驯顺因果", Value: "已内化「挣扎只会让黑暗更长」因果，驯顺加深", UpdatedChapter: 59, Evidence: "挣扎，加时"},
		{Entity: "女孩", Field: "status.幻觉触碰", Value: "密室3建立的对装置幻触的湿回应，后续持续引用", UpdatedChapter: 58, Evidence: "被幻觉碰了太多次"},
		{Entity: "女孩", Field: "status.默认姿态", Value: "默认屈从姿态已成形（提示音未响身体先动）", UpdatedChapter: 123, Evidence: "默认姿态验收"},
		{Entity: "女孩", Field: "body_device.双乳状态", Value: "左空右堵逻辑已转型：左榨右环仍在，右乳卸环侧红肿淤紫，两边同出同伤", UpdatedChapter: 238, Evidence: "右环解除后双乳状态转型"},
	}
}

// realDataStore 构造迁移后真实数据形状的 store：角色正式名"女孩"（role=主角）+
// 12 条 entity="女孩" 的 character_state + 含"女孩"的章节正文。
func realDataStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Characters.Save([]domain.Character{{
		Name: "女孩", Role: "主角",
		Aliases: []string{"自缚者", "囚中雀", "承裁者"},
	}}); err != nil {
		t.Fatalf("Save characters: %v", err)
	}
	if err := s.World.SaveCharacterState(migratedCharacterState()); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, "密室里的女孩被固定在装置上。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	return s
}

// TestMigratedDataWriterContextInjectsCharacterState 迁移数据（entity=女孩）在 writer
// context 顶层注入 character_state（主角常驻路径：role 含"主角" → 实体匹配）。
func TestMigratedDataWriterContextInjectsCharacterState(t *testing.T) {
	s := realDataStore(t)
	tool := NewContextToolForRole(s, References{}, "default", "writer")
	args, _ := json.Marshal(map[string]any{"chapter": 1})
	raw, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	proj, ok := got["character_state"].(map[string]any)
	if !ok {
		t.Fatalf("writer context 应注入顶层 character_state（主角常驻），got %T", got["character_state"])
	}
	entries, ok := proj["entries"].([]any)
	if !ok || len(entries) != 12 {
		t.Fatalf("主角常驻投影应含 12 条（entity=女孩），got %d 条: %+v", len(entries), proj)
	}
}

// TestMigratedDataCheckConsistencyMatches 正文含"女孩"时 check_consistency 匹配
// entity=女孩 的 character_state 并注入（正式名匹配路径）。
func TestMigratedDataCheckConsistencyMatches(t *testing.T) {
	s := realDataStore(t)
	out, err := NewCheckConsistencyTool(s).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	cs, ok := got["character_state"].([]any)
	if !ok {
		t.Fatalf("expected character_state injection, got %T", got["character_state"])
	}
	if len(cs) != 12 {
		t.Fatalf("正文含「女孩」应注入全部 12 条 entity=女孩 状态，got %d: %+v", len(cs), cs)
	}
}

// TestMigratedDataArcSummaryBasisAndConflict 迁移数据按"女孩"分组生成
// current_state_basis；迁移 value 形态（body_device.* 装置状态，value 无"存在"字样）
// 也能触发快照冲突 warning（P1-3：基于 field 前缀 + 矛盾词扩展）。
func TestMigratedDataArcSummaryBasisAndConflict(t *testing.T) {
	s := realDataStore(t)
	tool := NewSaveArcSummaryTool(s)
	args, err := json.Marshal(map[string]any{
		"volume": 1, "arc": 2, "title": "密室", "summary": "女孩在密室中的改造。",
		"key_events": []string{"装置安装"},
		"character_snapshots": []map[string]any{
			// 矛盾：current 有 body_device.缠足（装置在位），快照却写双腿完好。
			{"name": "女孩", "status": "双腿完好，手脚自由", "motivation": "继续沉沦"},
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
	basis, ok := result["current_state_basis"].(map[string]any)
	if !ok {
		t.Fatal("expected current_state_basis in result")
	}
	girl, ok := basis["女孩"].(map[string]any)
	if !ok || len(girl) != 12 {
		t.Fatalf("basis 应按 entity=女孩 分组且含 12 字段，got %+v", basis)
	}
	warns, ok := result["snapshot_conflict_warnings"].([]any)
	if !ok || len(warns) == 0 {
		t.Fatalf("迁移 value 形态（body_device.*，无「存在」字样）也应触发冲突 warning，got %+v",
			result["snapshot_conflict_warnings"])
	}
	joined := ""
	for _, w := range warns {
		joined += w.(string)
	}
	if !strings.Contains(joined, "女孩") || !strings.Contains(joined, "完好") {
		t.Fatalf("warning 应提及角色与矛盾词，got %v", warns)
	}
}
