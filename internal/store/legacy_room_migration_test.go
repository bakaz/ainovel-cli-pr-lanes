package store

import (
	"encoding/json"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ── successful migration ──

func TestMigrateLegacyRooms_Success(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "ancient_temple", "name": "上古神殿", "danger": "high"},
				{"room": 42, "name": "密道", "traps": true},
				{"room": "treasure_vault", "title": "宝藏库", "guard": "dragon"}
			]
		},
		"custom_ref_field": "preserve-me"
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "migrated" {
		t.Fatalf("expected status=migrated, got %q: %s", result.Status, result.Message)
	}
	if result.EntriesCount != 3 {
		t.Fatalf("expected 3 entries, got %d", result.EntriesCount)
	}

	// verify archive
	archive, err := s.PlanningArchive.Load()
	if err != nil {
		t.Fatalf("Load archive: %v", err)
	}
	if archive == nil {
		t.Fatal("archive is nil")
	}
	if err := archive.Validate(); err != nil {
		t.Fatalf("archive validate: %v", err)
	}
	if len(archive.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(archive.Entries))
	}

	entryMap := make(map[string]domain.PlanningArchiveEntry)
	for _, e := range archive.Entries {
		entryMap[e.ID] = e
	}

	// String ID preserved
	if e, ok := entryMap["ancient_temple"]; !ok {
		t.Fatal("missing entry: ancient_temple")
	} else if e.Kind != "room" {
		t.Fatalf("expected kind=room, got %q", e.Kind)
	} else if e.Summary != "上古神殿" {
		t.Fatalf("summary: got %q", e.Summary)
	} else if !jsonHasKeyValue(e.Data, "name", "上古神殿") {
		t.Fatalf("data missing name: %s", string(e.Data))
	} else if !jsonHasKeyValue(e.Data, "danger", "high") {
		t.Fatalf("data missing danger: %s", string(e.Data))
	}

	// Int ID → string
	if e, ok := entryMap["42"]; !ok {
		t.Fatal("missing entry: 42 (from int room)")
	} else if e.Summary != "密道" {
		t.Fatalf("id=42 summary: got %q", e.Summary)
	} else if !jsonHasKeyValue(e.Data, "traps", "true") {
		t.Fatalf("id=42 data missing traps: %s", string(e.Data))
	}

	// Title-derived summary
	if e, ok := entryMap["treasure_vault"]; !ok {
		t.Fatal("missing entry: treasure_vault")
	} else if e.Summary != "宝藏库" {
		t.Fatalf("treasure_vault summary: got %q", e.Summary)
	}

	// Verify compass Reference cleaned
	compass, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatalf("LoadCompass: %v", err)
	}
	if compass == nil {
		t.Fatal("compass is nil")
	}
	if jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should have been removed from Reference")
	}
	if !jsonContains(compass.Long.Reference, "custom_ref_field") {
		t.Fatal("custom_ref_field should be preserved")
	}
}

// ── data preserved intact (no keys trimmed/modified) ──

func TestMigrateLegacyRooms_DataPreservedIntact(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "room_x", "unknown_key": "keep-me", "nested": {"a": 1}, "summary": "existing-summary"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "migrated" {
		t.Fatalf("expected migrated, got %q", result.Status)
	}

	archive, _ := s.PlanningArchive.Load()
	e := archive.Entries[0]

	// Data should preserve all original fields (including "summary" in data)
	if !jsonHasKeyValue(e.Data, "unknown_key", "keep-me") {
		t.Fatalf("unknown_key not preserved: %s", string(e.Data))
	}
	if !strContains(string(e.Data), `"nested"`) {
		t.Fatalf("nested not preserved: %s", string(e.Data))
	}
	// "room" key must NOT be in data
	if strContains(string(e.Data), `"room"`) {
		t.Fatalf("room key should not be in data: %s", string(e.Data))
	}
	// Entry-level summary should be extracted from title/name, NOT from data's summary
	// Since data has no title/name, entry summary should be empty
	if e.Summary != "" {
		t.Fatalf("expected empty summary (no title/name), got %q", e.Summary)
	}
}

// ── canonical ID conflict (int 3 + string "3") ──

func TestMigrateLegacyRooms_CanonicalIDConflict(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": 3, "name": "int three"},
				{"room": "3", "name": "string three"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms should not return system error for conflict: %v", err)
	}
	if result.Status != "conflict" {
		t.Fatalf("expected status=conflict, got %q: %s", result.Status, result.Message)
	}

	// Archive should not have been written
	archive, _ := s.PlanningArchive.Load()
	if archive != nil {
		t.Fatal("archive should not have been created on conflict")
	}
}

// ── archive already exists + legacy pending: content-equivalence verification ──

func TestMigrateLegacyRooms_ArchiveExists_AllCovered(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Pre-create archive with matching entries
	if err := s.PlanningArchive.UpsertEntry("room", "room_a", json.RawMessage(`{"name":"Room A"}`)); err != nil {
		t.Fatal(err)
	}

	// Set up matching legacy rooms
	compassJSON := `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "room_a", "name": "Room A"}
			]
		}
	}`
	saveCompass(t, s, compassJSON)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "cleaned_up" {
		t.Fatalf("expected status=cleaned_up (content matches), got %q: %s", result.Status, result.Message)
	}

	// Archive unchanged
	archive, _ := s.PlanningArchive.Load()
	if len(archive.Entries) != 1 || archive.Entries[0].ID != "room_a" {
		t.Fatal("archive should not have been overwritten")
	}

	// Legacy cleaned
	compass, _ := s.Outline.LoadCompass()
	if jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should have been cleaned")
	}
}

func TestMigrateLegacyRooms_ArchiveExists_RoomNotInArchive(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Pre-create archive with DIFFERENT entries
	if err := s.PlanningArchive.UpsertEntry("room", "existing_only", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Legacy rooms include one NOT in archive
	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "existing_only", "name": "exists"},
				{"room": "not_in_archive", "name": "missing"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "conflict" {
		t.Fatalf("expected status=conflict (room not in archive), got %q: %s", result.Status, result.Message)
	}

	// Archive unchanged
	archive, _ := s.PlanningArchive.Load()
	if len(archive.Entries) != 1 {
		t.Fatalf("archive should have 1 entry, got %d", len(archive.Entries))
	}

	// Legacy should NOT have been cleaned (fail closed)
	compass, _ := s.Outline.LoadCompass()
	if !jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should NOT have been cleaned (fail closed)")
	}
}

func TestMigrateLegacyRooms_ArchiveExists_DataMismatch(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Pre-create archive with data
	if err := s.PlanningArchive.UpsertEntry("room", "room_a", json.RawMessage(`{"name":"Original"}`)); err != nil {
		t.Fatal(err)
	}

	// Legacy room with DIFFERENT data
	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "room_a", "name": "Modified"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "conflict" {
		t.Fatalf("expected status=conflict (data mismatch), got %q: %s", result.Status, result.Message)
	}

	// Legacy should NOT have been cleaned
	compass, _ := s.Outline.LoadCompass()
	if !jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should NOT have been cleaned (fail closed on data mismatch)")
	}
}

// ── archive exists + no legacy rooms ──

func TestMigrateLegacyRooms_ArchiveExistsNoLegacy(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	if err := s.PlanningArchive.UpsertEntry("room", "existing", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Compass with reference but NO long_rooms
	saveCompass(t, s, `{"other_field": "value"}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "already_exists" {
		t.Fatalf("expected already_exists, got %q: %s", result.Status, result.Message)
	}
}

func TestMigrateLegacyRooms_ArchiveExistsNoReference(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	if err := s.PlanningArchive.UpsertEntry("room", "x", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// Compass with NO reference at all
	saveCompass(t, s, ``) // empty string means no reference set

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "already_exists" {
		t.Fatalf("expected already_exists, got %q: %s", result.Status, result.Message)
	}
}

// ── no legacy rooms ──

func TestMigrateLegacyRooms_NoLegacyRooms(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	saveCompass(t, s, `{"other":"data"}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "no_legacy_rooms" {
		t.Fatalf("expected no_legacy_rooms, got %q: %s", result.Status, result.Message)
	}

	archive, _ := s.PlanningArchive.Load()
	if archive != nil {
		t.Fatal("archive should not be created")
	}
}

func TestMigrateLegacyRooms_NoReference(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Compass with no reference
	if err := s.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{EndingDirection: "终局"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "no_legacy_rooms" {
		t.Fatalf("expected no_legacy_rooms, got %q: %s", result.Status, result.Message)
	}
}

// ── empty long_rooms array ──

func TestMigrateLegacyRooms_EmptyLongRooms(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [],
			"other_plan_field": 42
		},
		"top_field": "keep"
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "no_legacy_rooms" {
		t.Fatalf("expected no_legacy_rooms, got %q: %s", result.Status, result.Message)
	}

	compass, _ := s.Outline.LoadCompass()
	refStr := string(compass.Long.Reference)
	t.Logf("cleaned reference: %s", refStr)
	if strContains(refStr, "long_rooms") {
		t.Fatal("empty long_rooms should be removed")
	}
	// detailed_plan should still exist (has other_plan_field)
	if !strContains(refStr, "other_plan_field") {
		t.Fatal("other_plan_field should be preserved in detailed_plan")
	}
	if !strContains(refStr, "top_field") {
		t.Fatal("top_field should be preserved")
	}
}

// ── null long_rooms ──

func TestMigrateLegacyRooms_NullLongRooms(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": null
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "no_legacy_rooms" {
		t.Fatalf("expected no_legacy_rooms, got %q: %s", result.Status, result.Message)
	}

	compass, _ := s.Outline.LoadCompass()
	refStr := string(compass.Long.Reference)
	if strContains(refStr, "long_rooms") {
		t.Fatal("null long_rooms should be removed")
	}
}

// ── room with minimal / no data ──

func TestMigrateLegacyRooms_RoomWithNoExtraData(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "minimal_room"},
				{"room": "empty_data_room"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "migrated" {
		t.Fatalf("expected migrated, got %q", result.Status)
	}

	archive, _ := s.PlanningArchive.Load()
	if len(archive.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(archive.Entries))
	}
	for _, e := range archive.Entries {
		if e.ID == "minimal_room" || e.ID == "empty_data_room" {
			// Data should be nil/omitted when room has no extra fields
			if len(e.Data) != 0 {
				t.Fatalf("expected empty data for %q, got: %s", e.ID, string(e.Data))
			}
		}
	}
}

// ── power of attorney / idempotent repeated call ──

func TestMigrateLegacyRooms_IdempotentRepeatedCall(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [{"room": "room_a", "name": "A"}]
		}
	}`)

	r1, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatal(err)
	}
	if r1.Status != "migrated" {
		t.Fatalf("first call: expected migrated, got %q", r1.Status)
	}

	// Second call: archive exists, no legacy → already_exists
	r2, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != "already_exists" {
		t.Fatalf("second call: expected already_exists, got %q: %s", r2.Status, r2.Message)
	}

	archive, _ := s.PlanningArchive.Load()
	if len(archive.Entries) != 1 || archive.Entries[0].ID != "room_a" {
		t.Fatal("archive should be unchanged after second call")
	}
}

// ── error: empty room id ──

func TestMigrateLegacyRooms_EmptyRoomID(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "", "name": "empty id"}
			]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected system error for empty id")
	}
}

// ── error: missing room key ──

func TestMigrateLegacyRooms_MissingRoomKey(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"name": "no room key here"}
			]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected system error for missing room key")
	}
}

// ── error: non-object in long_rooms ──

func TestMigrateLegacyRooms_NonObjectInLongRooms(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": ["just_a_string"]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected system error for non-object in long_rooms")
	}
}

// ── error: unsupported room id type ──

func TestMigrateLegacyRooms_UnsupportedRoomIDType(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": true, "name": "boolean room"}
			]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected system error for boolean room id")
	}
}

// ── no compass file at all ──

func TestMigrateLegacyRooms_NoCompass(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected error when compass doesn't exist")
	}
}

// ── archive v2 → fail closed ──

func TestMigrateLegacyRooms_ArchiveV2_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Write a v2 archive directly
	v2 := domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 2,
		Entries: []domain.PlanningArchiveEntry{
			{Kind: "room", ID: "x"},
		},
	}
	writeArchiveRaw(t, s, &v2)

	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [{"room": "x", "name": "X"}]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected error for v2 archive")
	}
}

// ── archive with duplicate entries → fail closed ──

func TestMigrateLegacyRooms_ArchiveDuplicate_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Write archive with duplicate (kind,id)
	dup := domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{
			{Kind: "room", ID: "x"},
			{Kind: "room", ID: "x"},
		},
	}
	writeArchiveRaw(t, s, &dup)

	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [{"room": "x", "name": "X"}]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected error for duplicate entries in archive")
	}
}

// ── archive with invalid schema → fail closed ──

func TestMigrateLegacyRooms_ArchiveInvalidSchema_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	bad := domain.PlanningArchiveV1{
		Schema:  "custom.bad",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{
			{Kind: "room", ID: "x"},
		},
	}
	writeArchiveRaw(t, s, &bad)

	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [{"room": "x", "name": "X"}]
		}
	}`)

	_, err := s.MigrateLegacyRooms()
	if err == nil {
		t.Fatal("expected error for invalid archive schema")
	}
}

// ── detailed_plan has other fields preserved ──

func TestMigrateLegacyRooms_DetailedPlanPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [{"room": "r1", "name": "R1"}],
			"plan_version": 2,
			"notes": "some notes"
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "migrated" {
		t.Fatalf("expected migrated, got %q", result.Status)
	}

	compass, _ := s.Outline.LoadCompass()
	ref := string(compass.Long.Reference)
	if !jsonContains(compass.Long.Reference, "plan_version") {
		t.Fatalf("plan_version not preserved in reference: %s", ref)
	}
	if !jsonContains(compass.Long.Reference, "notes") {
		t.Fatalf("notes not preserved in reference: %s", ref)
	}
	if jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should be removed")
	}
}

// ── detailed_plan becomes empty after removal → should be removed ──

func TestMigrateLegacyRooms_DetailedPlanRemovedWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithCompass(t, dir, `{
		"detailed_plan": {
			"long_rooms": [{"room": "r1", "name": "R1"}]
		},
		"top_level_field": "stay"
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "migrated" {
		t.Fatalf("expected migrated, got %q", result.Status)
	}

	compass, _ := s.Outline.LoadCompass()
	ref := string(compass.Long.Reference)
	if jsonContains(compass.Long.Reference, "detailed_plan") {
		t.Fatalf("detailed_plan should be removed when empty: %s", ref)
	}
	if !jsonContains(compass.Long.Reference, "top_level_field") {
		t.Fatalf("top_level_field should remain: %s", ref)
	}
}

// ── archive exists + legacy: migration verification ignores summary differences ──

func TestMigrateLegacyRooms_ArchiveExists_IgnoresSummaryDiff(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Pre-create archive with entry that has a non-empty summary (as if migrated earlier)
	archive := &domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []domain.PlanningArchiveEntry{
			{
				Kind:    "room",
				ID:      "room_a",
				Summary: "Room A Title", // summary derived from previous migration
				Data:    json.RawMessage(`{"name":"Room A"}`),
			},
		},
	}
	writeArchiveRaw(t, s, archive)

	// Legacy room with same data (but summary would be empty if re-derived)
	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "room_a", "name": "Room A"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "cleaned_up" {
		t.Fatalf("expected status=cleaned_up (summary diff must be ignored), got %q: %s",
			result.Status, result.Message)
	}

	// Archive unchanged
	archive2, _ := s.PlanningArchive.Load()
	if len(archive2.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(archive2.Entries))
	}
	if archive2.Entries[0].Summary != "Room A Title" {
		t.Fatalf("existing summary should be preserved, got %q", archive2.Entries[0].Summary)
	}

	// Legacy cleaned
	compass, _ := s.Outline.LoadCompass()
	if jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should have been cleaned")
	}
}

// ── archive exists + canonical ID conflict → fail closed ──

func TestMigrateLegacyRooms_ArchiveExists_CanonicalIDConflict(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)

	// Pre-create archive with a matching room
	if err := s.PlanningArchive.UpsertEntry("room", "3", json.RawMessage(`{"name":"int three"}`)); err != nil {
		t.Fatal(err)
	}

	// Legacy rooms with canonical ID conflict (int 3 vs string "3")
	saveCompass(t, s, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": 3, "name": "int three"},
				{"room": "3", "name": "string three"}
			]
		}
	}`)

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms should not return system error for conflict: %v", err)
	}
	if result.Status != "conflict" {
		t.Fatalf("expected status=conflict (canonical ID conflict when archive exists), got %q: %s",
			result.Status, result.Message)
	}

	// Archive must stay unchanged
	archive, _ := s.PlanningArchive.Load()
	if len(archive.Entries) != 1 || archive.Entries[0].ID != "3" {
		t.Fatal("archive should be unchanged after canonical conflict")
	}

	// Legacy must NOT have been cleaned (fail closed)
	compass, _ := s.Outline.LoadCompass()
	if !jsonContains(compass.Long.Reference, "long_rooms") {
		t.Fatal("long_rooms should NOT have been cleaned (fail closed on canonical ID conflict)")
	}
}

// ── unknown compass fields preserved through migration ──

func TestMigrateLegacyRooms_UnknownCompassFieldsPreserved(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write compass with unknown fields at top level, long sub-level, and current sub-level,
	// plus a reference with legacy rooms.
	compassRaw := `{
		"schema": "ainovel.compass",
		"version": 1,
		"unknown_top": "preserved-top",
		"long": {
			"ending_direction": "终局",
			"unknown_long": "preserved-long",
			"reference": {
				"detailed_plan": {
					"long_rooms": [
						{"room": "room_a", "name": "Room A"}
					],
					"unknown_dp": "preserved-dp"
				},
				"unknown_ref": "preserved-ref"
			},
			"open_threads": ["thread-1"],
			"unknown_long_extra": {"nested": true}
		},
		"current": {
			"unknown_current": "preserved-current"
		},
		"unknown_root_list": [1,2,3]
	}`
	if err := s.Outline.io.WriteFileUnlocked("meta/compass.json", []byte(compassRaw)); err != nil {
		t.Fatalf("WriteFileUnlocked: %v", err)
	}

	result, err := s.MigrateLegacyRooms()
	if err != nil {
		t.Fatalf("MigrateLegacyRooms: %v", err)
	}
	if result.Status != "migrated" {
		t.Fatalf("expected migrated, got %q: %s", result.Status, result.Message)
	}

	// Read compass raw and verify unknown fields preserved
	raw, err := s.Outline.io.ReadFileUnlocked("meta/compass.json")
	if err != nil {
		t.Fatalf("ReadFileUnlocked: %v", err)
	}
	rawStr := string(raw)

	// Top-level unknown fields
	if !strContains(rawStr, "unknown_top") || !strContains(rawStr, "preserved-top") {
		t.Fatal("top-level unknown_top not preserved")
	}
	if !strContains(rawStr, "unknown_root_list") {
		t.Fatal("top-level unknown_root_list not preserved")
	}

	// Long-level unknown fields
	if !strContains(rawStr, "unknown_long") || !strContains(rawStr, "preserved-long") {
		t.Fatal("long.unknown_long not preserved")
	}
	if !strContains(rawStr, "unknown_long_extra") {
		t.Fatal("long.unknown_long_extra not preserved")
	}

	// Current-level unknown fields
	if !strContains(rawStr, "unknown_current") || !strContains(rawStr, "preserved-current") {
		t.Fatal("current.unknown_current not preserved")
	}

	// Reference-level unknown fields (within reference itself)
	if !strContains(rawStr, "unknown_ref") || !strContains(rawStr, "preserved-ref") {
		t.Fatal("reference.unknown_ref not preserved")
	}
	if !strContains(rawStr, "unknown_dp") || !strContains(rawStr, "preserved-dp") {
		t.Fatal("reference.detailed_plan.unknown_dp not preserved")
	}

	// long_rooms must be removed from reference
	if strContains(rawStr, "long_rooms") {
		t.Fatal("long_rooms should have been removed from reference")
	}

	// Verify typed load also works (backward compat)
	compass, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatalf("LoadCompass: %v", err)
	}
	if compass.Long.EndingDirection != "终局" {
		t.Fatalf("long.ending_direction: got %q", compass.Long.EndingDirection)
	}
}

// ── helpers ──

// newStore 创建初始化好的 Store。
func newStore(t *testing.T, dir string) *Store {
	t.Helper()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

// newStoreWithCompass 创建 Store 并写入带指定 Reference 的 compass。
func newStoreWithCompass(t *testing.T, dir, referenceJSON string) *Store {
	t.Helper()
	s := newStore(t, dir)
	saveCompass(t, s, referenceJSON)
	return s
}

// saveCompass 用指定 JSON 作为 Reference 保存 compass。
func saveCompass(t *testing.T, s *Store, referenceJSON string) {
	t.Helper()
	ref := json.RawMessage(nil)
	if referenceJSON != "" {
		ref = json.RawMessage(referenceJSON)
	}
	if err := s.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			Reference:       ref,
		},
	}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}
}

// writeArchiveRaw 直接写入 archive 文件（绕过 UpsertEntry 的 validate）。
func writeArchiveRaw(t *testing.T, s *Store, archive *domain.PlanningArchiveV1) {
	t.Helper()
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, archive); err != nil {
		t.Fatalf("writeArchiveRaw: %v", err)
	}
}

// strContains 纯字符串子串检查（不使用 JSON 解析）。
func strContains(s, substr string) bool {
	return len(s) >= len(substr) && strSearch(s, substr)
}

// strSearch 朴素子串搜索。
func strSearch(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// jsonContains 检查 JSON 字节是否包含子串（原样字符串匹配）。
func jsonContains(data []byte, substr string) bool {
	return len(data) > 0 && strContains(string(data), substr)
}

// jsonHasKeyValue 检查 JSON data 中是否存在指定的 key→value 条目。
// value 传原始字符串值（数字传 "42"，布尔传 "true"），string 值不需要加引号。
func jsonHasKeyValue(data json.RawMessage, key, value string) bool {
	compact := compactJSON(data)
	needle := `"` + key + `":` + value
	if strContains(compact, needle) {
		return true
	}
	// string 值需要引号
	needle2 := `"` + key + `":"` + value + `"`
	return strContains(compact, needle2)
}

// compactJSON 将 JSON 压缩为一行（去除空白）。
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	compacted, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(compacted)
}
