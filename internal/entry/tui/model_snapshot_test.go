package tui

import (
	"testing"

	"github.com/voocel/ainovel-cli/internal/host"
)

func TestIdleSnapshotSkipsUnchangedViewports(t *testing.T) {
	snap := host.UISnapshot{
		RuntimeState:   "paused",
		CompletedCount: 10,
		Outline: []host.OutlineSnapshot{
			{Chapter: 1, Title: "一"},
			{Chapter: 2, Title: "二"},
		},
	}
	m := NewModel(nil, nil, "test")
	m.mode = modeRunning
	m.width = 160
	m.height = 40
	m.snapshot = snap
	m.updateViewportSize()
	m.refreshDetailViewport()
	beforeDetail := m.detailVP.View()
	m.streamVP.SetContent("SENTINEL-STREAM")
	beforeStream := m.streamVP.View()

	next, _, handled := m.handleRuntimeMsg(snapshotMsg(snap))
	if !handled {
		t.Fatal("snapshot should be handled")
	}
	got := next.(Model)
	if got.detailVP.View() != beforeDetail {
		t.Fatal("unchanged idle snapshot should not rebuild the outline panel")
	}
	if got.streamVP.View() != beforeStream {
		t.Fatal("idle snapshot should not rebuild the stream panel")
	}
}

func TestWelcomeSnapshotSkipsWorkbenchRefresh(t *testing.T) {
	m := NewModel(nil, nil, "test")
	m.mode = modeNew
	m.width = 160
	m.height = 40
	m.detailVP.SetContent("keep-welcome")
	before := m.detailVP.View()
	next, _, _ := m.handleRuntimeMsg(snapshotMsg(host.UISnapshot{
		Outline: []host.OutlineSnapshot{{Chapter: 1, Title: "一"}},
	}))
	got := next.(Model)
	if got.detailVP.View() != before {
		t.Fatalf("welcome snapshot should not paint outline, got %q", got.detailVP.View())
	}
}
