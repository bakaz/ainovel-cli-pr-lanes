package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/host"
)

func TestWindowResizeRebuildsCenterViewports(t *testing.T) {
	m := NewModel(nil, nil, "test")
	m.mode = modeRunning
	m.width = 120
	m.height = 80
	m.updateViewportSize()
	m.events = []host.Event{{
		Time:     time.Now(),
		Category: "ERROR",
		Summary:  strings.Repeat("窗口调整后事件内容必须按新宽度重新换行。", 8),
		Level:    "error",
	}}
	m.streamRounds = []string{strings.Repeat("窗口调整后流式内容必须按新宽度重新换行。", 12)}
	m.refreshEventViewport()
	m.refreshStreamViewport()

	next, cmd := m.Update(tea.WindowSizeMsg{Width: 180, Height: 80})
	if cmd != nil {
		t.Fatal("resize should not schedule a command")
	}
	got := next.(Model)

	expectedEvent := got.viewport
	expectedEvent.SetContent(renderEventContent(got.events, got.eventFlowWidth(), got.toolSpinnerIdx))
	if got.autoScroll {
		expectedEvent.GotoBottom()
	}
	if got.viewport.View() != expectedEvent.View() {
		t.Fatal("event viewport was not rebuilt for the new width")
	}

	expectedStream := got.streamVP
	expectedStream.SetContent(renderStreamContent(got.streamRounds, got.streamVP.Width, ""))
	if got.streamScroll {
		expectedStream.GotoBottom()
	}
	if got.streamVP.View() != expectedStream.View() {
		t.Fatal("stream viewport was not rebuilt for the new width")
	}
	if got.streamDirty {
		t.Fatal("resize should clear stale stream dirty state")
	}
}
