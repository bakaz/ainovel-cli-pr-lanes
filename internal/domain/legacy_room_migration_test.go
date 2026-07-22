package domain

import (
	"encoding/json"
	"testing"
)

func TestConvertRawRoomID_StringRejectsWhitespace(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{"leading space", json.RawMessage(`" room_a"`)},
		{"trailing space", json.RawMessage(`"room_a "`)},
		{"leading tab", json.RawMessage("\"\troom_a\"")},
		{"trailing tab", json.RawMessage("\"room_a\t\"")},
		{"middle space", json.RawMessage(`"room a"`)},
		{"unicode non-breaking space", json.RawMessage("\"room\u00a0a\"")},
		{"only spaces", json.RawMessage(`"   "`)},
		{"mixed unicode whitespace", json.RawMessage("\"room\u2003a\"")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := convertRawRoomID(tc.raw)
			if err == nil {
				t.Fatalf("expected error for whitespace in room id: %s", string(tc.raw))
			}
		})
	}
}

func TestConvertRawRoomID_StringNoWhitespaceOK(t *testing.T) {
	tests := []struct {
		raw  json.RawMessage
		want string
	}{
		{json.RawMessage(`"room_a"`), "room_a"},
		{json.RawMessage(`"ancestral_hall"`), "ancestral_hall"},
		{json.RawMessage(`"3"`), "3"},
	}
	for _, tc := range tests {
		got, err := convertRawRoomID(tc.raw)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", string(tc.raw), err)
		}
		if got != tc.want {
			t.Fatalf("convertRawRoomID(%s) = %q, want %q", string(tc.raw), got, tc.want)
		}
	}
}

func TestConvertRawRoomID_Integer(t *testing.T) {
	got, err := convertRawRoomID(json.RawMessage(`42`))
	if err != nil {
		t.Fatalf("unexpected error for int: %v", err)
	}
	if got != "42" {
		t.Fatalf("convertRawRoomID(42) = %q, want %q", got, "42")
	}
}

func TestConvertRawRoomID_Empty(t *testing.T) {
	_, err := convertRawRoomID(json.RawMessage(``))
	if err == nil {
		t.Fatal("expected error for empty raw")
	}
}

func TestConvertRawRoomID_EmptyString(t *testing.T) {
	_, err := convertRawRoomID(json.RawMessage(`""`))
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

func TestConvertRawRoomID_UnsupportedType(t *testing.T) {
	_, err := convertRawRoomID(json.RawMessage(`true`))
	if err == nil {
		t.Fatal("expected error for boolean")
	}
	_, err = convertRawRoomID(json.RawMessage(`[]`))
	if err == nil {
		t.Fatal("expected error for array")
	}
}

func TestLegacyEntryDataEqual_Equal(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Summary: "A", Data: json.RawMessage(`{"a":1}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Summary: "B", Data: json.RawMessage(`{"a":1}`)}
	if !LegacyEntryDataEqual(a, b) {
		t.Fatal("LegacyEntryDataEqual should ignore summary and find equal data")
	}
}

func TestLegacyEntryDataEqual_KindMismatch(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{}`)}
	b := PlanningArchiveEntry{Kind: "outline", ID: "x", Data: json.RawMessage(`{}`)}
	if LegacyEntryDataEqual(a, b) {
		t.Fatal("different kind must not be equal")
	}
}

func TestLegacyEntryDataEqual_IDMismatch(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "y", Data: json.RawMessage(`{}`)}
	if LegacyEntryDataEqual(a, b) {
		t.Fatal("different id must not be equal")
	}
}

func TestLegacyEntryDataEqual_DataMismatch(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"a":1}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"a":2}`)}
	if LegacyEntryDataEqual(a, b) {
		t.Fatal("different data must not be equal")
	}
}

func TestLegacyEntryDataEqual_NormalizedData(t *testing.T) {
	// JSON 语义等价
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"a":1,"b":2}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"b":2,"a":1}`)}
	if !LegacyEntryDataEqual(a, b) {
		t.Fatal("normalized equivalent data should be equal")
	}
}

func TestLegacyEntryDataEqual_LargeIntegerPrecision(t *testing.T) {
	a := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"big": 9007199254740993}`)}
	b := PlanningArchiveEntry{Kind: "room", ID: "x", Data: json.RawMessage(`{"big": 9007199254740993}`)}
	if !LegacyEntryDataEqual(a, b) {
		t.Fatal("large integers with same value must be equal")
	}
}

func TestExtractSummaryFromRoomData_Empty(t *testing.T) {
	if got := ExtractSummaryFromRoomData(nil); got != "" {
		t.Fatalf("expected empty for nil, got %q", got)
	}
	if got := ExtractSummaryFromRoomData(json.RawMessage(`{}`)); got != "" {
		t.Fatalf("expected empty for {}, got %q", got)
	}
}

func TestExtractSummaryFromRoomData_Title(t *testing.T) {
	data := json.RawMessage(`{"title": "先祖大厅", "danger": "high"}`)
	if got := ExtractSummaryFromRoomData(data); got != "先祖大厅" {
		t.Fatalf("expected '先祖大厅', got %q", got)
	}
}

func TestExtractSummaryFromRoomData_Name(t *testing.T) {
	data := json.RawMessage(`{"name": "密道", "traps": true}`)
	if got := ExtractSummaryFromRoomData(data); got != "密道" {
		t.Fatalf("expected '密道', got %q", got)
	}
}

func TestExtractSummaryFromRoomData_TitlePreferred(t *testing.T) {
	data := json.RawMessage(`{"name": "name-val", "title": "title-val"}`)
	if got := ExtractSummaryFromRoomData(data); got != "title-val" {
		t.Fatalf("expected 'title-val' (title preferred), got %q", got)
	}
}

func TestConvertLegacyRoom_EmptyID(t *testing.T) {
	_, err := ConvertLegacyRoom(LegacyRoomRef{ID: ""})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
}
