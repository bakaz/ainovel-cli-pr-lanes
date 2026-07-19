package tools

import (
	"encoding/json"
	"testing"
)

func TestNormalizeIntegerStringFields(t *testing.T) {
	raw := json.RawMessage(`{"chapter":"46","volume":" 2 ","title":"123","bad":"1.5"}`)
	normalized := normalizeIntegerStringFields(raw, "chapter", "volume", "bad")
	var got struct {
		Chapter int    `json:"chapter"`
		Volume  int    `json:"volume"`
		Title   string `json:"title"`
		Bad     string `json:"bad"`
	}
	if err := json.Unmarshal(normalized, &got); err != nil {
		t.Fatalf("normalized args should unmarshal: %v", err)
	}
	if got.Chapter != 46 || got.Volume != 2 {
		t.Fatalf("unexpected numeric fields: chapter=%d volume=%d", got.Chapter, got.Volume)
	}
	if got.Title != "123" || got.Bad != "1.5" {
		t.Fatalf("unexpected untouched fields: title=%q bad=%q", got.Title, got.Bad)
	}
}

func TestNormalizeIntegerStringFieldsLeavesInvalidIntegersForStrictDecode(t *testing.T) {
	raw := json.RawMessage(`{"chapter":"1.5"}`)
	normalized := normalizeIntegerStringFields(raw, "chapter")
	var got struct {
		Chapter int `json:"chapter"`
	}
	if err := json.Unmarshal(normalized, &got); err == nil {
		t.Fatal("invalid integer string should still fail strict int decoding")
	}
}
