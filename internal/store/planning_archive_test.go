package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestPlanningArchiveStore_Load_NotExist(t *testing.T) {
	s := NewStore(t.TempDir())
	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load on non-existent: %v", err)
	}
	if archive != nil {
		t.Fatal("expected nil for non-existent archive")
	}
}

func TestPlanningArchiveStore_UpsertEntry_Create(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	data := json.RawMessage(`{"scope": "volume-1", "nodes": ["a", "b"]}`)
	if err := s.PlanningArchive.UpsertEntry("outline", "main-arc", data); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if archive == nil {
		t.Fatal("expected non-nil archive")
	}
	if archive.Schema != "ainovel.planning-archive" {
		t.Fatalf("schema: got %q", archive.Schema)
	}
	if archive.Version != 1 {
		t.Fatalf("version: got %d", archive.Version)
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(archive.Entries))
	}
	if archive.Entries[0].Kind != "outline" || archive.Entries[0].ID != "main-arc" {
		t.Fatalf("entry kind/id mismatch: %q %q", archive.Entries[0].Kind, archive.Entries[0].ID)
	}
	if !jsonStrEq(t, `{"scope":"volume-1","nodes":["a","b"]}`, string(archive.Entries[0].Data)) {
		t.Fatalf("entry data mismatch: %s", string(archive.Entries[0].Data))
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "meta/planning_archive.json")); err != nil {
		t.Fatalf("archive file should exist: %v", err)
	}
}

func TestPlanningArchiveStore_UpsertEntry_Update(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create
	original := json.RawMessage(`{"version": 1}`)
	if err := s.PlanningArchive.UpsertEntry("outline", "main", original); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Update
	updated := json.RawMessage(`{"version": 2, "status": "expanded"}`)
	if err := s.PlanningArchive.UpsertEntry("outline", "main", updated); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected 1 entry after update, got %d", len(archive.Entries))
	}
	if !jsonStrEq(t, `{"version":2,"status":"expanded"}`, string(archive.Entries[0].Data)) {
		t.Fatalf("entry data should be updated, got: %s", string(archive.Entries[0].Data))
	}
}

func TestPlanningArchiveStore_UpsertEntry_EmptyValidation(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tests := []struct {
		name string
		kind string
		id   string
	}{
		{"empty kind", "", "id-1"},
		{"empty id", "outline", ""},
		{"both empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.PlanningArchive.UpsertEntry(tc.kind, tc.id, json.RawMessage(`{}`))
			if err == nil {
				t.Fatal("expected error for empty kind/id")
			}
		})
	}
}

func TestPlanningArchiveStore_DeleteEntry(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create two entries
	if err := s.PlanningArchive.UpsertEntry("outline", "arc-1", json.RawMessage(`{"order":1}`)); err != nil {
		t.Fatalf("Upsert arc-1: %v", err)
	}
	if err := s.PlanningArchive.UpsertEntry("outline", "arc-2", json.RawMessage(`{"order":2}`)); err != nil {
		t.Fatalf("Upsert arc-2: %v", err)
	}

	// Delete one
	if err := s.PlanningArchive.deleteEntry("outline", "arc-1"); err != nil {
		t.Fatalf("deleteEntry: %v", err)
	}

	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected 1 entry after delete, got %d", len(archive.Entries))
	}
	if archive.Entries[0].ID != "arc-2" {
		t.Fatalf("remaining entry should be arc-2, got %q", archive.Entries[0].ID)
	}
}

func TestPlanningArchiveStore_DeleteEntry_NotExist(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Delete when archive doesn't exist — 现在返回错误
	if err := s.PlanningArchive.deleteEntry("outline", "ghost"); err == nil {
		t.Fatal("expected error when deleting non-existent entry from nil archive")
	}

	// Create one entry then delete non-matching — 仍然返回错误
	if err := s.PlanningArchive.UpsertEntry("outline", "actual", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.PlanningArchive.deleteEntry("outline", "ghost"); err == nil {
		t.Fatal("expected error when deleting non-matching entry")
	}
	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(archive.Entries))
	}

	// 删除存在的条目成功
	if err := s.PlanningArchive.deleteEntry("outline", "actual"); err != nil {
		t.Fatalf("DeleteEntry existing: %v", err)
	}
}

func TestPlanningArchiveStore_DeleteEntry_EmptyValidation(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := s.PlanningArchive.deleteEntry("", "id"); err == nil {
		t.Fatal("expected error for empty kind")
	}
	if err := s.PlanningArchive.deleteEntry("kind", ""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestPlanningArchiveStore_UpsertThenLoad_PreservesUnchangedEntries(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create three entries
	if err := s.PlanningArchive.UpsertEntry("type-a", "id-1", json.RawMessage(`{"val":"a1"}`)); err != nil {
		t.Fatalf("Upsert a1: %v", err)
	}
	if err := s.PlanningArchive.UpsertEntry("type-b", "id-2", json.RawMessage(`{"val":"b2"}`)); err != nil {
		t.Fatalf("Upsert b2: %v", err)
	}
	if err := s.PlanningArchive.UpsertEntry("type-c", "id-3", json.RawMessage(`{"val":"c3"}`)); err != nil {
		t.Fatalf("Upsert c3: %v", err)
	}

	// Update only one entry
	if err := s.PlanningArchive.UpsertEntry("type-b", "id-2", json.RawMessage(`{"val":"b2-updated"}`)); err != nil {
		t.Fatalf("Upsert b2 update: %v", err)
	}

	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(archive.Entries))
	}

	for _, e := range archive.Entries {
		switch e.ID {
		case "id-1":
			if !jsonStrEq(t, `{"val":"a1"}`, string(e.Data)) {
				t.Fatalf("id-1 data changed: %s", string(e.Data))
			}
		case "id-2":
			if !jsonStrEq(t, `{"val":"b2-updated"}`, string(e.Data)) {
				t.Fatalf("id-2 data not updated: %s", string(e.Data))
			}
		case "id-3":
			if !jsonStrEq(t, `{"val":"c3"}`, string(e.Data)) {
				t.Fatalf("id-3 data changed: %s", string(e.Data))
			}
		default:
			t.Fatalf("unexpected entry id: %q", e.ID)
		}
	}
}

func TestPlanningArchiveStore_PreservesExtraFields(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Directly write JSON with extra fields
	raw := `{
		"schema": "ainovel.planning-archive",
		"version": 1,
		"entries": [
			{"kind": "outline", "id": "main", "data": {"x":1}, "extra_field": "keep-me"},
			{"kind": "compass", "id": "north", "data": {"dir":"n"}, "custom_flag": true}
		],
		"top_level_extra": "preserved"
	}`
	if err := os.WriteFile(filepath.Join(dir, "meta/planning_archive.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write raw: %v", err)
	}

	// Load — extra fields should be preserved
	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if archive == nil {
		t.Fatal("expected archive")
	}
	if len(archive.Extra) == 0 {
		t.Fatal("expected top-level extra fields")
	}

	// Upsert — existing entry's extra should be preserved
	if err := s.PlanningArchive.UpsertEntry("outline", "main", json.RawMessage(`{"x":2}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Reload and verify
	archive2, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load after upsert: %v", err)
	}
	if len(archive2.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(archive2.Entries))
	}

	// Check the unchanged entry's Extra is intact
	var compassEntry *domain.PlanningArchiveEntry
	for i := range archive2.Entries {
		if archive2.Entries[i].Kind == "compass" && archive2.Entries[i].ID == "north" {
			compassEntry = &archive2.Entries[i]
			break
		}
	}
	if compassEntry == nil {
		t.Fatal("compass entry not found")
	}
	if len(compassEntry.Extra) == 0 {
		t.Fatal("compass entry's Extra should preserve custom_flag")
	}
	// compass data should be unchanged
	if !jsonStrEq(t, `{"dir":"n"}`, string(compassEntry.Data)) {
		t.Fatalf("compass data changed: %s", string(compassEntry.Data))
	}

	// Top-level extra should be preserved
	outData, err := os.ReadFile(filepath.Join(dir, "meta/planning_archive.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !containsJSONKey(outData, "top_level_extra") {
		t.Fatal("top_level_extra not preserved in file")
	}
}

func TestPlanningArchiveStore_CrossStoreConsistency(t *testing.T) {
	// Verify that PlanningArchive coexists with other stores
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Operations on unrelated stores should not interfere
	if err := s.Outline.SavePremise("test premise"); err != nil {
		t.Fatalf("SavePremise: %v", err)
	}
	if err := s.PlanningArchive.UpsertEntry("test", "item", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	premise, err := s.Outline.LoadPremise()
	if err != nil {
		t.Fatalf("LoadPremise: %v", err)
	}
	if premise != "test premise" {
		t.Fatalf("premise corrupted: %q", premise)
	}
}

func TestPlanningArchiveStore_UpsertEntry_ValidateRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Pre-write an archive that's valid
	if err := s.PlanningArchive.UpsertEntry("room", "r1", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Manually add a duplicate via raw write to test Validate catches it
	dup := domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{
			{Kind: "room", ID: "r1", Data: json.RawMessage(`{"a":1}`)},
			{Kind: "room", ID: "r1", Data: json.RawMessage(`{"a":2}`)},
		},
	}
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, &dup); err != nil {
		t.Fatalf("WriteJSONUnlocked: %v", err)
	}

	// Upsert should fail validation because archive now has duplicates
	err := s.PlanningArchive.UpsertEntry("room", "r2", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when archive has duplicate entries")
	}
}

func TestPlanningArchiveStore_UpsertEntry_EmptySummaryClearsOld(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create with summary
	if err := s.PlanningArchive.UpsertEntryWithSummary("room", "r1", "旧摘要", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("UpsertWithSummary: %v", err)
	}

	// Update with empty summary via UpsertEntry (calls UpsertEntryWithSummary with "")
	if err := s.PlanningArchive.UpsertEntry("room", "r1", json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(archive.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(archive.Entries))
	}
	if archive.Entries[0].Summary != "" {
		t.Fatalf("expected empty summary after upsert with empty summary, got %q", archive.Entries[0].Summary)
	}
	if !jsonStrEq(t, `{"a":2}`, string(archive.Entries[0].Data)) {
		t.Fatalf("data mismatch: %s", string(archive.Entries[0].Data))
	}
}

func TestPlanningArchiveStore_UpsertEntry_ValidateRejectsInvalidSchema(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Pre-write archive with bad schema
	bad := domain.PlanningArchiveV1{
		Schema:  "bad.schema",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{
			{Kind: "room", ID: "r1"},
		},
	}
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, &bad); err != nil {
		t.Fatalf("WriteJSONUnlocked: %v", err)
	}

	// Upsert must fail validation
	err := s.PlanningArchive.UpsertEntry("room", "r2", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for archive with invalid schema")
	}
}

func TestPlanningArchiveStore_UpsertEntry_ValidateRejectsVersion2(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Pre-write archive with version 2
	v2 := domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 2,
		Entries: []domain.PlanningArchiveEntry{
			{Kind: "room", ID: "r1"},
		},
	}
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, &v2); err != nil {
		t.Fatalf("WriteJSONUnlocked: %v", err)
	}

	err := s.PlanningArchive.UpsertEntry("room", "r2", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for version 2 archive")
	}
}

func TestPlanningArchiveStore_Load_InvalidRejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write archive with invalid schema
	bad := domain.PlanningArchiveV1{
		Schema:  "bad.schema",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{{Kind: "room", ID: "r1"}},
	}
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, &bad); err != nil {
		t.Fatalf("WriteJSONUnlocked: %v", err)
	}

	// Load must reject invalid archive
	_, err := s.PlanningArchive.Load()
	if err == nil {
		t.Fatal("expected error for invalid archive schema")
	}
}

func TestPlanningArchiveStore_Load_V2Rejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write v2 archive
	v2 := domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 2,
		Entries: []domain.PlanningArchiveEntry{{Kind: "room", ID: "r1"}},
	}
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, &v2); err != nil {
		t.Fatalf("WriteJSONUnlocked: %v", err)
	}

	_, err := s.PlanningArchive.Load()
	if err == nil {
		t.Fatal("expected error for v2 archive")
	}
}

func TestPlanningArchiveStore_DeleteEntry_InvalidArchive_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write invalid archive
	bad := domain.PlanningArchiveV1{
		Schema:  "bad",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{{Kind: "room", ID: "r1"}},
	}
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, &bad); err != nil {
		t.Fatalf("WriteJSONUnlocked: %v", err)
	}

	// Delete with invalid archive must fail (loadUnlocked fails Validate)
	err := s.PlanningArchive.deleteEntry("room", "r1")
	if err == nil {
		t.Fatal("expected error when archive is invalid")
	}
}

// jsonStrEq compares two JSON strings after normalizing both.
func jsonStrEq(t *testing.T, want, got string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(want), &a); err != nil {
		t.Fatalf("jsonStrEq: unmarshal want: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &b); err != nil {
		t.Fatalf("jsonStrEq: unmarshal got: %v", err)
	}
	aw, _ := json.Marshal(a)
	bw, _ := json.Marshal(b)
	return string(aw) == string(bw)
}

// jsonCompact compact-prints JSON bytes for comparison.
func jsonCompact(t *testing.T, data []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("jsonCompact: %v", err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// containsJSONKey is a simple helper to check if raw JSON bytes contain a key.
func containsJSONKey(data []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// containsJSONKeys checks if raw JSON bytes contain all given keys (substring match on compact form).
func containsJSONKeys(data []byte, keys ...string) bool {
	compact := strings.TrimSpace(string(data))
	for _, k := range keys {
		if !strings.Contains(compact, k) {
			return false
		}
	}
	return true
}
