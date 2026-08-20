package tui

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/host"
)

func TestAdvanceCommandsAreRegistered(t *testing.T) {
	registry := commandRegistryInstance()
	review, ok := registry.Find("review")
	if !ok || review.NeedsIdle {
		t.Fatalf("/review should be available while running: %+v", review)
	}
	next, ok := registry.Find("next")
	if !ok || !next.NeedsIdle || !next.AutoExecute {
		t.Fatalf("/next should be an idle one-shot command: %+v", next)
	}
	items := builtinCommandItems()
	if !hasPaletteItem(items, "review") || !hasPaletteItem(items, "next") {
		t.Fatalf("advance commands missing from palette: %+v", items)
	}
	idle, ok := registry.Find("idle-writing")
	if !ok || len(idle.Aliases) != 1 || idle.Aliases[0] != "idle" {
		t.Fatalf("idle writing command should be registered with /idle alias: %+v", idle)
	}
	peak, ok := registry.Find("peak-pause")
	if !ok || !peak.AutoExecute || peak.NeedsIdle || peak.Usage != "/peak-pause [on|off|status|skip]" {
		t.Fatalf("peak auto pause command should be available while running: %+v", peak)
	}
	items = builtinCommandItems()
	if !hasPaletteItem(items, "peak-pause") {
		t.Fatalf("peak auto pause command missing from palette: %+v", items)
	}
	if _, ok := registry.Find("idle-start"); ok {
		t.Fatal("temporary /idle-start command should not remain registered")
	}
}

func TestReviewWaitingPlaceholder(t *testing.T) {
	m := Model{
		mode: modeRunning,
		snapshot: host.UISnapshot{
			RuntimeState: "paused",
			Phase:        "writing",
			AdvanceMode:  "review",
		},
	}
	m.syncRuntimePlaceholder()
	if got := m.textarea.Placeholder; !strings.Contains(got, "/next") || !strings.Contains(got, "修改意见") {
		t.Fatalf("review placeholder should expose both choices, got %q", got)
	}
}
