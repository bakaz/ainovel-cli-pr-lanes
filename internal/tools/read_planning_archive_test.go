package tools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestReadPlanningArchive_Ok(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "ancient_temple", json.RawMessage(`{"name":"上古神殿","danger":"high"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "artifact_hall", json.RawMessage(`{"name":"神器大厅","traps":true}`)); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"ancient_temple"},{"kind":"room","id":"artifact_hall"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status  string                        `json:"status"`
		Refs    []PlanningRef                 `json:"refs"`
		Found   []domain.PlanningArchiveEntry `json:"found"`
		Missing []PlanningRef                 `json:"missing"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("expected status=ok, got %q", result.Status)
	}
	if len(result.Found) != 2 {
		t.Fatalf("expected 2 found, got %d", len(result.Found))
	}
	if len(result.Missing) != 0 {
		t.Fatalf("expected 0 missing, got %d", len(result.Missing))
	}
}

func TestReadPlanningArchive_Partial(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "ancient_temple", json.RawMessage(`{"name":"上古神殿"}`)); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"ancient_temple"},{"kind":"room","id":"nonexistent"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status  string                        `json:"status"`
		Found   []domain.PlanningArchiveEntry `json:"found"`
		Missing []PlanningRef                 `json:"missing"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" {
		t.Fatalf("expected status=partial, got %q", result.Status)
	}
	if len(result.Found) != 1 {
		t.Fatalf("expected 1 found, got %d", len(result.Found))
	}
	if len(result.Missing) != 1 || result.Missing[0].ID != "nonexistent" {
		t.Fatalf("expected 1 missing [nonexistent], got %+v", result.Missing)
	}
}

func TestReadPlanningArchive_NotFound(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "other", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"nonexistent"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status  string        `json:"status"`
		Missing []PlanningRef `json:"missing"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "not_found" {
		t.Fatalf("expected status=not_found, got %q", result.Status)
	}
}

func TestReadPlanningArchive_ArchiveAbsent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	// Archive 文件不存在

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"some_room"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status string `json:"status"`
		Hint   string `json:"_hint"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "archive_absent" {
		t.Fatalf("expected status=archive_absent, got %q (hint: %s)", result.Status, result.Hint)
	}
}

func TestReadPlanningArchive_ArchiveAbsentWithLegacyFallback(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	// 设置 legacy compass.long.reference 含 detailed_plan.long_rooms[].room
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			Reference: json.RawMessage(`{
				"detailed_plan": {
					"long_rooms": [
						{"room": "legacy_room", "name": "旧房间", "legacy": true},
						{"room": "legacy_room2", "name": "旧房间二"}
					]
				}
			}`),
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"legacy_room"},{"kind":"room","id":"legacy_room2"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status  string                        `json:"status"`
		Hint    string                        `json:"_hint"`
		Found   []domain.PlanningArchiveEntry `json:"found"`
		Missing []PlanningRef                 `json:"missing"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("expected status=ok from legacy fallback, got %q (hint: %s)", result.Status, result.Hint)
	}
	if len(result.Found) != 2 {
		t.Fatalf("expected 2 found from legacy, got %d", len(result.Found))
	}
}

func TestReadPlanningArchive_ArchiveAbsentLegacyPartial(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			Reference: json.RawMessage(`{
				"detailed_plan": {
					"long_rooms": [
						{"room": "legacy_room", "name": "旧房间"}
					]
				}
			}`),
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"legacy_room"},{"kind":"room","id":"missing_room"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status  string                        `json:"status"`
		Found   []domain.PlanningArchiveEntry `json:"found"`
		Missing []PlanningRef                 `json:"missing"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" {
		t.Fatalf("expected status=partial from legacy, got %q", result.Status)
	}
	if len(result.Found) != 1 {
		t.Fatalf("expected 1 found, got %d", len(result.Found))
	}
	if len(result.Missing) != 1 || result.Missing[0].ID != "missing_room" {
		t.Fatalf("expected 1 missing_room, got %+v", result.Missing)
	}
}

func TestReadPlanningArchive_DedupRefs(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.PlanningArchive.UpsertEntry("room", "dup_room", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}

	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [
			{"kind":"room","id":"dup_room"},
			{"kind":"room","id":"dup_room"},
			{"kind":"room","id":"dup_room"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var result struct {
		Status string                        `json:"status"`
		Refs   []PlanningRef                 `json:"refs"`
		Found  []domain.PlanningArchiveEntry `json:"found"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("expected ok, got %q", result.Status)
	}
	if len(result.Refs) != 1 {
		t.Fatalf("expected 1 ref after dedup, got %d", len(result.Refs))
	}
	if len(result.Found) != 1 {
		t.Fatalf("expected 1 found, got %d", len(result.Found))
	}
}

func TestReadPlanningArchive_MaxRefs(t *testing.T) {
	_, err := NewReadPlanningArchiveTool(store.NewStore(t.TempDir())).Execute(t.Context(), json.RawMessage(`{
		"refs": [
			{"kind":"room","id":"a"},{"kind":"room","id":"b"},{"kind":"room","id":"c"},
			{"kind":"room","id":"d"},{"kind":"room","id":"e"},{"kind":"room","id":"f"},
			{"kind":"room","id":"g"},{"kind":"room","id":"h"},{"kind":"room","id":"i"}
		]
	}`))
	if err == nil {
		t.Fatal("expected error for > 8 refs")
	}
}

func TestReadPlanningArchive_EmptyRefs(t *testing.T) {
	_, err := NewReadPlanningArchiveTool(store.NewStore(t.TempDir())).Execute(t.Context(), json.RawMessage(`{"refs":[]}`))
	if err == nil {
		t.Fatal("expected error for empty refs")
	}
}

func TestReadPlanningArchive_UnsupportedSchema(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	// 直接写入错误 schema 文件（不通过 UpsertEntry，因为 loadUnlocked 会 Validate）
	badArchive := domain.PlanningArchiveV1{Schema: "bad-schema", Version: 1}
	raw, _ := json.Marshal(badArchive)
	if err := os.WriteFile(st.Dir()+"/meta/planning_archive.json", raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Load() 在 loadUnlocked 内 Validate，返回 error
	_, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"room","id":"x"}]
	}`))
	if err == nil {
		t.Fatal("expected error for invalid archive schema")
	}
	if !strings.Contains(err.Error(), "invalid schema") {
		t.Fatalf("expected error mentioning invalid schema, got %v", err)
	}
}

func TestReadPlanningArchive_NonRoomRefWithAbsentArchive(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	// 非 room 类型且 archive 不存在 → archive_absent 不分回退
	out, err := NewReadPlanningArchiveTool(st).Execute(t.Context(), json.RawMessage(`{
		"refs": [{"kind":"outline","id":"main-arc"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "archive_absent" {
		t.Fatalf("expected archive_absent for non-room ref with absent archive, got %q", result.Status)
	}
}
