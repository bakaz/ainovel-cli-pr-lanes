package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestSavePlanningArchiveEntry_UpsertRoom(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action":  "upsert",
		"kind":    "room",
		"id":      "ancient_temple",
		"data":    map[string]any{"name": "上古神殿", "danger": "high"},
		"summary": "上古神殿存档",
		"reason":  "新增上古神殿 room",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved {
		t.Fatal("expected saved=true")
	}
	if result.Action != "upsert" || result.Kind != "room" || result.ID != "ancient_temple" || result.Status != "upserted" {
		t.Fatalf("unexpected result: %+v", result)
	}

	archive, err := st.PlanningArchive.Load()
	if err != nil {
		t.Fatal(err)
	}
	if archive == nil {
		t.Fatal("expected archive to exist")
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(archive.Entries))
	}
	if archive.Entries[0].Kind != "room" || archive.Entries[0].ID != "ancient_temple" {
		t.Fatalf("unexpected entry: %+v", archive.Entries[0])
	}
	// Summary 已持久化
	if archive.Entries[0].Summary != "上古神殿存档" {
		t.Fatalf("expected summary to be persisted, got %q", archive.Entries[0].Summary)
	}
}

func TestSavePlanningArchiveEntry_UpsertRoomRoundtripSummary(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args1, _ := json.Marshal(map[string]any{
		"action":  "upsert",
		"kind":    "room",
		"id":      "test_room",
		"data":    map[string]any{"name": "测试"},
		"summary": "测试条目摘要",
		"reason":  "初始创建",
	})
	if _, err := tool.Execute(t.Context(), args1); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// 读取确认 summary 持久化
	archive, err := st.PlanningArchive.Load()
	if err != nil {
		t.Fatal(err)
	}
	if archive.Entries[0].Summary != "测试条目摘要" {
		t.Fatalf("summary not persisted: %q", archive.Entries[0].Summary)
	}

	// 更新 data 但不传 summary，应保留旧 summary
	args2, _ := json.Marshal(map[string]any{
		"action": "upsert",
		"kind":   "room",
		"id":     "test_room",
		"data":   map[string]any{"name": "更新后"},
		"reason": "更新数据",
	})
	if _, err := tool.Execute(t.Context(), args2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	archive, _ = st.PlanningArchive.Load()
	if archive.Entries[0].Summary != "测试条目摘要" {
		t.Fatalf("summary should be preserved on update: %q", archive.Entries[0].Summary)
	}
}

func TestSavePlanningArchiveEntry_DeleteRoom(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntryWithSummary("room", "ancient_temple", "", json.RawMessage(`{"name":"上古神殿"}`)); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
		"kind":   "room",
		"id":     "ancient_temple",
		"reason": "该 room 不再需要",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved {
		t.Fatal("expected saved=true")
	}
	if result.Action != "delete" || result.Status != "deleted" {
		t.Fatalf("unexpected result: %+v", result)
	}

	archive, err := st.PlanningArchive.Load()
	if err != nil {
		t.Fatal(err)
	}
	if archive == nil || len(archive.Entries) != 0 {
		t.Fatalf("expected archive with 0 entries, got %+v", archive)
	}
}

func TestSavePlanningArchiveEntry_DeleteRejectedWhenReferenced(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "ancient_temple", json.RawMessage(`{"name":"上古神殿"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			OpenThreads:     []string{"探索遗迹 [room:ancient_temple]"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
		"kind":   "room",
		"id":     "ancient_temple",
		"reason": "测试拒绝场景",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute should not fail: %v", err)
	}

	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if result.Saved {
		t.Fatal("expected saved=false when open_threads still reference the room")
	}
	if result.Status != "rejected" {
		t.Fatalf("expected rejected status, got %q", result.Status)
	}

	archive, err := st.PlanningArchive.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected archive entry to remain, got %d entries", len(archive.Entries))
	}
}

func TestSavePlanningArchiveEntry_DeleteAllowedWhenThreadHasDifferentRoom(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "target_room", json.RawMessage(`{"name":"目标房间"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			OpenThreads:     []string{"探索遗迹 [room:other_room]"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
		"kind":   "room",
		"id":     "target_room",
		"reason": "该 room 已完成使命",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved {
		t.Fatal("expected saved=true when no thread references the room")
	}
	if result.Status != "deleted" {
		t.Fatalf("expected deleted status, got %q", result.Status)
	}
}

func TestSavePlanningArchiveEntry_DeleteRejectedWithMalformedMarker(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "target_room", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	// 有 malformed marker → fail-closed
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			OpenThreads:     []string{"中间有 [room:bad] 标记"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
		"kind":   "room",
		"id":     "target_room",
		"reason": "测试",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if result.Saved {
		t.Fatalf("expected saved=false when malformed marker exists, got status=%q", result.Status)
	}
	if result.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", result.Status)
	}
}

func TestSavePlanningArchiveEntry_Validation(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	tool := NewSavePlanningArchiveEntryTool(st)

	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing action", map[string]any{"kind": "room", "id": "x", "reason": "r"}},
		{"invalid action", map[string]any{"action": "invalid", "kind": "room", "id": "x", "reason": "r"}},
		{"missing kind", map[string]any{"action": "upsert", "id": "x", "data": map[string]any{}, "reason": "r"}},
		{"missing id", map[string]any{"action": "upsert", "kind": "room", "data": map[string]any{}, "reason": "r"}},
		{"upsert missing data", map[string]any{"action": "upsert", "kind": "room", "id": "x", "reason": "r"}},
		{"upsert null data", map[string]any{"action": "upsert", "kind": "room", "id": "x", "data": nil, "reason": "r"}},
		{"upsert data not object", map[string]any{"action": "upsert", "kind": "room", "id": "x", "data": []string{"a"}, "reason": "r"}},
		{"delete with data", map[string]any{"action": "delete", "kind": "room", "id": "x", "data": map[string]any{"x": 1}, "reason": "r"}},
		{"missing reason", map[string]any{"action": "upsert", "kind": "room", "id": "x", "data": map[string]any{}}},
		{"wrong kind", map[string]any{"action": "upsert", "kind": "outline", "id": "x", "data": map[string]any{}, "reason": "r"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, _ := json.Marshal(tt.args)
			_, err := tool.Execute(t.Context(), args)
			if err == nil {
				t.Fatal("expected error for invalid args")
			}
		})
	}
}

func TestSavePlanningArchiveEntry_DeleteNonExistent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
		"kind":   "room",
		"id":     "nonexistent",
		"reason": "测试",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if result.Saved {
		t.Fatal("expected saved=false for non-existent delete")
	}
	if result.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", result.Status)
	}
}

func TestSavePlanningArchiveEntry_CheckpointStep(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	tool := NewSavePlanningArchiveEntryTool(st)

	args, _ := json.Marshal(map[string]any{
		"action":  "upsert",
		"kind":    "room",
		"id":      "test_room",
		"data":    map[string]any{"name": "测试"},
		"summary": "测试条目",
		"reason":  "测试创建",
	})
	if _, err := tool.Execute(t.Context(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	cp := st.Checkpoints.LatestByStep(domain.GlobalScope(), "planning_archive_upsert")
	if cp == nil {
		t.Fatal("expected checkpoint with step planning_archive_upsert")
	}
	if !strings.Contains(cp.Artifact, "planning_archive.json") {
		t.Fatalf("expected artifact to contain planning_archive.json, got %q", cp.Artifact)
	}
}

func TestSavePlanningArchiveEntry_DeleteWithSpaceSeparatedMultiMarker(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "other_room", json.RawMessage(`{"name":"other"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "referenced_room", json.RawMessage(`{"name":"referenced"}`)); err != nil {
		t.Fatal(err)
	}
	// space-separated multi-marker thread 只引用 referenced_room
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			OpenThreads:     []string{"探索 [room:other_room] [room:referenced_room]"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)

	// other_room 也被线程引用（两条 marker 在同一线程），因此删除被拒绝
	argsOther, _ := json.Marshal(map[string]any{
		"action": "delete", "kind": "room", "id": "other_room", "reason": "测试",
	})
	resOther, err := tool.Execute(t.Context(), argsOther)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resultOther SavePlanningArchiveEntryResult
	json.Unmarshal(resOther, &resultOther)
	if resultOther.Saved {
		t.Fatal("expected saved=false: other_room is also referenced by the multi-marker thread")
	}
	if resultOther.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", resultOther.Status)
	}

	// 同样 referenced_room 也被拒绝
	argsRef, _ := json.Marshal(map[string]any{
		"action": "delete", "kind": "room", "id": "referenced_room", "reason": "测试",
	})
	resRef, err := tool.Execute(t.Context(), argsRef)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resultRef SavePlanningArchiveEntryResult
	json.Unmarshal(resRef, &resultRef)
	if resultRef.Saved {
		t.Fatal("expected saved=false for referenced room")
	}
	if resultRef.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", resultRef.Status)
	}
}

func TestSavePlanningArchiveEntry_DeleteRejectedWithSuspectMarker(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "target", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	// 未闭合 marker 也应 fail-closed
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			OpenThreads:     []string{"未闭合 [room:bad"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewSavePlanningArchiveEntryTool(st)
	args, _ := json.Marshal(map[string]any{
		"action": "delete",
		"kind":   "room",
		"id":     "target",
		"reason": "测试",
	})
	res, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result SavePlanningArchiveEntryResult
	if err := json.Unmarshal(res, &result); err != nil {
		t.Fatal(err)
	}
	if result.Saved {
		t.Fatal("expected saved=false with unclosed marker in open_threads")
	}
}
