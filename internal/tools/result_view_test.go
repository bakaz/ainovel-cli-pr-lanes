package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/store"
)

func TestResultViewGateSelectsFullWhenWindowAllows(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	t.Cleanup(func() { st.Close() })
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	gate := NewResultViewGate(st)
	gate.Bind(nil, 200_000, 8_000)

	full, _ := json.Marshal(map[string]string{"hello": "world", "body": strings.Repeat("字", 40)})
	got := gate.Select("novel_context", full)
	if string(got) != string(full) {
		t.Fatalf("expected full result when window is large, got %s", got)
	}
}

func TestResultViewGateSelectsSummaryWhenWindowTight(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	t.Cleanup(func() { st.Close() })
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	gate := NewResultViewGate(st)
	gate.window = 100
	gate.reserve = 80

	full, _ := json.Marshal(map[string]string{
		"_loading_summary": "章节上下文",
		"body":             strings.Repeat("长文本", 400),
	})
	got := gate.Select("novel_context", full)
	if string(got) == string(full) {
		t.Fatal("expected summary when remaining window is tiny")
	}
	var rec map[string]any
	if err := json.Unmarshal(got, &rec); err != nil {
		t.Fatalf("summary should be JSON: %v", err)
	}
	if rec["_view"] != "summary" {
		t.Fatalf("_view=%v", rec["_view"])
	}
	if rec["tool"] != "novel_context" {
		t.Fatalf("tool=%v", rec["tool"])
	}
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatal("expected sidecar id")
	}
	loaded, err := st.Sessions.LoadToolResult(id)
	if err != nil {
		t.Fatalf("load sidecar: %v", err)
	}
	var gotFull, wantFull map[string]any
	if err := json.Unmarshal(loaded, &gotFull); err != nil {
		t.Fatalf("sidecar JSON: %v", err)
	}
	if err := json.Unmarshal(full, &wantFull); err != nil {
		t.Fatalf("full JSON: %v", err)
	}
	if gotFull["body"] != wantFull["body"] || gotFull["_loading_summary"] != wantFull["_loading_summary"] {
		t.Fatalf("sidecar mismatch:\n loaded=%s\n full=%s", loaded, full)
	}
}
