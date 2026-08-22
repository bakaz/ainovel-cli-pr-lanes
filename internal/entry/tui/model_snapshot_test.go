package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

func TestSnapshotStateChangedDetectsAgentContext(t *testing.T) {
	prev := host.UISnapshot{
		RuntimeState: "running",
		Agents: []host.AgentSnapshot{{
			Name:  "writer",
			State: "running",
			Context: host.AgentContextSnapshot{
				Tokens: 1000, ContextWindow: 10000, Percent: 10,
			},
		}},
	}
	next := prev
	next.Agents = []host.AgentSnapshot{{
		Name:  "writer",
		State: "running",
		Context: host.AgentContextSnapshot{
			Tokens: 8000, ContextWindow: 10000, Percent: 80,
		},
	}}
	if snapshotStateChanged(prev, prev) {
		t.Fatal("identical snapshots should not look changed")
	}
	if !snapshotStateChanged(prev, next) {
		t.Fatal("agent ctx percent/tokens should refresh the sidebar")
	}
}

func TestApplySnapshotRefreshesStateWhenAgentContextChanges(t *testing.T) {
	m := NewModel(nil, nil, "test")
	m.mode = modeRunning
	m.width = 160
	m.height = 40
	m.snapshot = host.UISnapshot{
		RuntimeState: "running",
		IsRunning:    true,
		Agents: []host.AgentSnapshot{{
			Name:  "writer",
			State: "running",
			Context: host.AgentContextSnapshot{
				Tokens: 1000, ContextWindow: 10000, Percent: 10,
			},
		}},
	}
	m.updateViewportSize()
	m.refreshStateViewport()
	before := m.stateVP.View()
	if !strings.Contains(before, "ctx 10%") {
		t.Fatalf("setup missing ctx 10%%: %q", before)
	}

	next := m.snapshot
	next.Agents = []host.AgentSnapshot{{
		Name:  "writer",
		State: "running",
		Context: host.AgentContextSnapshot{
			Tokens: 8000, ContextWindow: 10000, Percent: 80,
		},
	}}
	m.applySnapshot(next)
	got := m.stateVP.View()
	if got == before {
		t.Fatal("agent ctx change should rebuild the state panel")
	}
	if !strings.Contains(got, "ctx 80%") {
		t.Fatalf("state panel missing updated ctx: %q", got)
	}
}

func TestViewDoesNotMutateViewportDimensions(t *testing.T) {
	m := NewModel(nil, nil, "test")
	m.mode = modeRunning
	m.width = 160
	m.height = 40
	m.updateViewportSize()
	w, h := m.viewport.Width, m.viewport.Height
	_ = m.View()
	if m.viewport.Width != w || m.viewport.Height != h {
		t.Fatalf("View mutated viewport size %d×%d -> %d×%d", w, h, m.viewport.Width, m.viewport.Height)
	}
}

func TestMouseMotionUpdatesHoverWithoutScrolling(t *testing.T) {
	m := NewModel(nil, nil, "test")
	m.mode = modeRunning
	m.width = 160
	m.height = 40
	m.updateViewportSize()
	m.viewport.SetYOffset(3)
	before := m.viewport.YOffset

	x := m.width - m.detailWidth() + 1
	y := m.hitTopH + 2
	next, cmd := m.handleMouseMsg(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	if cmd != nil {
		t.Fatal("hover motion should not emit a command")
	}
	got := next.(Model)
	if got.viewport.YOffset != before {
		t.Fatalf("motion scrolled events pane %d -> %d", before, got.viewport.YOffset)
	}
	if !got.hoverActive || got.hoverPane != focusDetail {
		t.Fatalf("hoverActive=%v pane=%v, want detail", got.hoverActive, got.hoverPane)
	}
}
