package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanningArchiveV1_Validate_Valid(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []PlanningArchiveEntry{
			{Kind: "outline", ID: "main-arc", Data: json.RawMessage(`{"chapters":3}`)},
			{Kind: "compass", ID: "long", Data: json.RawMessage(`{"direction":"north"}`)},
		},
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestPlanningArchiveV1_Validate_EmptyEntries(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("empty entries should be valid, got: %v", err)
	}
}

func TestPlanningArchiveV1_Validate_WrongSchema(t *testing.T) {
	a := &PlanningArchiveV1{Schema: "wrong-schema", Version: 1}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for wrong schema")
	}
}

func TestPlanningArchiveV1_Validate_WrongVersion(t *testing.T) {
	a := &PlanningArchiveV1{Schema: "ainovel.planning-archive", Version: 2}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for wrong version")
	}
}

func TestPlanningArchiveV1_Validate_EmptyKind(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []PlanningArchiveEntry{
			{Kind: "", ID: "x", Data: json.RawMessage(`{}`)},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for empty kind")
	}
}

func TestPlanningArchiveV1_Validate_EmptyID(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []PlanningArchiveEntry{
			{Kind: "outline", ID: "", Data: json.RawMessage(`{}`)},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestPlanningArchiveV1_Validate_DuplicateEntry(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []PlanningArchiveEntry{
			{Kind: "outline", ID: "main", Data: json.RawMessage(`{"a":1}`)},
			{Kind: "outline", ID: "main", Data: json.RawMessage(`{"b":2}`)},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for duplicate (kind, id)")
	}
}

func TestPlanningArchiveV1_JSON_TopLevelExtra_RoundTrip(t *testing.T) {
	// JSON with extra top-level fields
	input := `{
		"schema": "ainovel.planning-archive",
		"version": 1,
		"entries": [
			{"kind": "outline", "id": "main", "data": {"chapters": 3}}
		],
		"custom_field": "preserve-me",
		"another_extra": {"nested": true}
	}`

	var a PlanningArchiveV1
	if err := json.Unmarshal([]byte(input), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Schema != "ainovel.planning-archive" {
		t.Fatalf("schema: got %q", a.Schema)
	}
	if len(a.Entries) != 1 {
		t.Fatalf("entries: got %d", len(a.Entries))
	}
	if len(a.Extra) == 0 {
		t.Fatal("Extra should contain unknown fields")
	}

	// Round-trip
	output, err := json.Marshal(&a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored map[string]json.RawMessage
	if err := json.Unmarshal(output, &restored); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if string(restored["custom_field"]) != `"preserve-me"` {
		t.Fatalf("custom_field not preserved: %s", string(restored["custom_field"]))
	}
	if string(restored["another_extra"]) != `{"nested":true}` {
		t.Fatalf("another_extra not preserved: %s", string(restored["another_extra"]))
	}
}

func TestPlanningArchiveV1_JSON_NoExtra_RoundTrip(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []PlanningArchiveEntry{
			{Kind: "outline", ID: "main", Data: json.RawMessage(`{"x":1}`)},
		},
	}

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored PlanningArchiveV1
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.Schema != "ainovel.planning-archive" {
		t.Fatalf("schema: got %q", restored.Schema)
	}
	if len(restored.Entries) != 1 || restored.Entries[0].Kind != "outline" {
		t.Fatalf("entries not preserved")
	}
}

func TestPlanningArchiveEntry_JSON_Extra_RoundTrip(t *testing.T) {
	entryJSON := `{"kind": "outline", "id": "main", "data": {"chapters": 3}, "entry_extra": 42, "tags": ["a","b"]}`

	var e PlanningArchiveEntry
	if err := json.Unmarshal([]byte(entryJSON), &e); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if e.Kind != "outline" || e.ID != "main" {
		t.Fatalf("kind/id: %q %q", e.Kind, e.ID)
	}
	if len(e.Extra) == 0 {
		t.Fatal("Extra should contain unknown fields")
	}

	// Round-trip
	out, err := json.Marshal(&e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var restored map[string]json.RawMessage
	if err := json.Unmarshal(out, &restored); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if string(restored["entry_extra"]) != "42" {
		t.Fatalf("entry_extra not preserved: %s", string(restored["entry_extra"]))
	}
	if string(restored["tags"]) != `["a","b"]` {
		t.Fatalf("tags not preserved: %s", string(restored["tags"]))
	}
}

func TestPlanningArchiveEntry_JSON_NoExtra_RoundTrip(t *testing.T) {
	e := PlanningArchiveEntry{
		Kind: "outline",
		ID:   "main",
		Data: json.RawMessage(`{"x":1}`),
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored PlanningArchiveEntry
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.Kind != "outline" || restored.ID != "main" {
		t.Fatalf("kind/id not preserved")
	}
	if string(restored.Data) != `{"x":1}` {
		t.Fatalf("data not preserved: %s", string(restored.Data))
	}
}

func TestPlanningArchiveEntry_Summary_RoundTrip(t *testing.T) {
	// Summary 应持久化且 roundtrip 正确（MarshalJSON 定义在指针接收者上，传 &e）
	e := PlanningArchiveEntry{
		Kind:    "room",
		ID:      "ancestral_hall",
		Summary: "先祖大厅",
		Data:    json.RawMessage(`{"name":"先祖大厅","danger":"low"}`),
	}
	data, err := json.Marshal(&e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytesContains(data, `"summary":"先祖大厅"`) {
		t.Fatalf("summary not in marshalled JSON: %s", string(data))
	}
	var restored PlanningArchiveEntry
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.Summary != "先祖大厅" {
		t.Fatalf("summary roundtrip failed: got %q", restored.Summary)
	}
	if restored.Kind != "room" || restored.ID != "ancestral_hall" {
		t.Fatalf("kind/id: %q %q", restored.Kind, restored.ID)
	}
}

func TestPlanningArchiveEntry_Summary_EmptyOmitted(t *testing.T) {
	// 空 Summary 应被 omitempty
	e := PlanningArchiveEntry{
		Kind: "room",
		ID:   "no_summary",
	}
	data, err := json.Marshal(&e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytesContains(data, `"summary"`) {
		t.Fatalf("empty summary should be omitted: %s", string(data))
	}
}

func TestPlanningArchiveEntry_Summary_DoesNotOverwriteExtra(t *testing.T) {
	// 入口 JSON 中的 summary 是 Entry 已知字段，不应进入 Extra
	input := `{"kind":"room","id":"test","summary":"my-summary","data":{},"custom_extra":42}`
	var e PlanningArchiveEntry
	if err := json.Unmarshal([]byte(input), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Summary != "my-summary" {
		t.Fatalf("summary: got %q", e.Summary)
	}
	if len(e.Extra) == 0 {
		t.Fatalf("Extra should contain custom_extra, Extra=%q", string(e.Extra))
	}
	// Roundtrip — marshal via &e 以确保调用指针接收者的 MarshalJSON
	out, err := json.Marshal(&e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored map[string]json.RawMessage
	if err := json.Unmarshal(out, &restored); err != nil {
		t.Fatalf("unmarshal rt: %v", err)
	}
	if string(restored["custom_extra"]) != "42" {
		t.Fatalf("custom_extra not preserved: %q", string(restored["custom_extra"]))
	}
}

func TestPlanningArchiveV1_Validate_CompositeKeyCollision(t *testing.T) {
	// kind+"/"+id 有可能碰撞：Kind="room/foo" ID="bar" vs Kind="room" ID="foo/bar"
	// 用 struct key 消除碰撞。以下两条必须视为不同条目。
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: []PlanningArchiveEntry{
			{Kind: "room/foo", ID: "bar", Data: json.RawMessage(`{"a":1}`)},
			{Kind: "room", ID: "foo/bar", Data: json.RawMessage(`{"b":2}`)},
		},
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("composite keys must not collide, got: %v", err)
	}
}

func TestPlanningArchiveV1_Validate_RejectsVersion2(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 2,
		Entries: []PlanningArchiveEntry{
			{Kind: "room", ID: "x"},
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for version 2")
	}
}

func TestPlanningArchiveV1_Validate_RejectsInvalidSchema(t *testing.T) {
	a := &PlanningArchiveV1{
		Schema:  "custom.schema",
		Version: 1,
	}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for invalid schema")
	}
}

func TestLegacyEntryEqual_True(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Summary: "X", Data: json.RawMessage(`{"a":1}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Summary: "X", Data: json.RawMessage(`{"a":1}`)}
	if !LegacyEntryEqual(a, b) {
		t.Fatal("expected equal")
	}
}

func TestLegacyEntryEqual_False_Kind(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x"}
	b := PlanningArchiveEntry{Kind: "outline", ID: "x"}
	if LegacyEntryEqual(a, b) {
		t.Fatal("expected not equal (kind)")
	}
}

func TestLegacyEntryEqual_False_Data(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"a":1}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"a":2}`)}
	if LegacyEntryEqual(a, b) {
		t.Fatal("expected not equal (data)")
	}
}

func TestLegacyEntryEqual_DataNormalized(t *testing.T) {
	// JSON 语义等价（key 顺序不同）
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"a":1,"b":2}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"b":2,"a":1}`)}
	if !LegacyEntryEqual(a, b) {
		t.Fatal("expected equal (normalized)")
	}
}

func TestLegacyEntryEqual_SummaryIgnored(t *testing.T) {
	// Summary 严格比较
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Summary: "A"}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Summary: "B"}
	if LegacyEntryEqual(a, b) {
		t.Fatal("expected not equal (summary differs)")
	}
}

func TestJsonBytesEqual_LargeIntegerPrecision(t *testing.T) {
	// >53 位整数必须被精确比较（json.Decoder.UseNumber 保证）
	a := json.RawMessage(`{"big": 9007199254740993}`)
	b := json.RawMessage(`{"big": 9007199254740993}`)
	if !jsonBytesEqual(a, b) {
		t.Fatal("identical large integers should be equal")
	}

	// 不同的大整数必须不等
	c := json.RawMessage(`{"big": 9007199254740994}`)
	if jsonBytesEqual(a, c) {
		t.Fatal("different large integers must not be equal")
	}
}

func TestJsonBytesEqual_UseNumberDoesNotAffectSmallInts(t *testing.T) {
	// 小整数行为不变
	a := json.RawMessage(`{"a":1,"b":2}`)
	b := json.RawMessage(`{"b":2,"a":1}`)
	if !jsonBytesEqual(a, b) {
		t.Fatal("key order should not matter")
	}
}

// bytesContains 检查字节切片是否包含子串。
func bytesContains(b []byte, s string) bool {
	return len(b) > 0 && string(b) != "" && strings.Contains(string(b), s)
}
