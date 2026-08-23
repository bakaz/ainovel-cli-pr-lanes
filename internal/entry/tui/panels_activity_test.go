package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/host"
)

// ── renderEventLine: TOOL 三态（running / discarded / failed） ───────────

func TestRenderEventLine_TOOL_RunningShowsSpinner(t *testing.T) {
	ev := host.Event{
		ID:       "e1",
		Time:     time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Category: "TOOL",
		Agent:    "writer",
		Summary:  "draft_chapter",
		Level:    "info",
		Depth:    1,
		// FinishedAt zero → Running()=true
	}
	out := renderEventLine(ev, 100, 0)

	// running TOOL uses a spinner frame character; NOT ✕, NOT ⊘
	if strings.Contains(out, "✕") {
		t.Errorf("running TOOL should NOT contain ✕: %q", out)
	}
	if strings.Contains(out, "⊘") {
		t.Errorf("running TOOL should NOT contain ⊘: %q", out)
	}
	// Should have some spinner char (any of the toolSpinnerFrames)
	hasSpinner := false
	for _, f := range toolSpinnerFrames {
		if strings.Contains(out, f) {
			hasSpinner = true
			break
		}
	}
	if !hasSpinner {
		t.Errorf("running TOOL should contain a spinner frame: %q", out)
	}
	// Depth > 0 → indented
	if !strings.Contains(out, "  ") {
		t.Errorf("running TOOL Depth=1 should be indented: %q", out)
	}
}

func TestRenderEventLine_TOOL_DiscardedShowsOStroke(t *testing.T) {
	ev := host.Event{
		ID:         "e1",
		Time:       time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2025, 1, 1, 10, 0, 5, 0, time.UTC),
		Discarded:  true,
		Failed:     false,
		Category:   "TOOL",
		Agent:      "writer",
		Summary:    "draft_chapter",
		Level:      "info",
		Depth:      1,
		Duration:   5 * time.Second,
	}
	out := renderEventLine(ev, 100, 0)

	// discarded TOOL → ⊘ icon + dim italic
	if !strings.Contains(out, "⊘") {
		t.Errorf("discarded TOOL should contain ⊘: %q", out)
	}
	if strings.Contains(out, "✕") {
		t.Errorf("discarded TOOL should NOT contain ✕: %q", out)
	}
	// Should NOT have spinner frames
	for _, f := range toolSpinnerFrames {
		if strings.Contains(out, f) {
			t.Errorf("discarded TOOL should NOT contain spinner frame %q: %q", f, out)
		}
	}
	// Should show duration (rendered as "5.0s" for 5-second duration)
	if !strings.Contains(out, "5.0s") {
		t.Errorf("discarded TOOL should show duration (5.0s): %q", out)
	}
	if !strings.Contains(out, "tool") {
		t.Errorf("discarded TOOL should label tool duration: %q", out)
	}
}

func TestRenderEventLine_DispatchAndToolUseSeparateClocks(t *testing.T) {
	events := []host.Event{
		{
			ID:       "d1",
			Time:     time.Now().Add(-20*time.Minute - 8*time.Second),
			Category: "DISPATCH",
			Agent:    "writer",
			Summary:  "写第 845 章",
		},
		{
			ID:       "t1",
			Time:     time.Now().Add(-4*time.Minute - 2*time.Second),
			Category: "TOOL",
			Agent:    "writer",
			Summary:  "plan_chapter",
			Depth:    1,
		},
	}
	out := renderEventContent(events, 120, 0)
	if !strings.Contains(out, "started") || !strings.Contains(out, "tool") {
		t.Fatalf("expected (started …) (tool …), got %q", out)
	}
	if !strings.Contains(out, "20m") {
		t.Fatalf("started clock should count from dispatch: %q", out)
	}
	if !strings.Contains(out, "4m") {
		t.Fatalf("tool clock should count from tool_call: %q", out)
	}
}

func TestRenderEventContent_ThinkClockBetweenTools(t *testing.T) {
	now := time.Now()
	events := []host.Event{
		{
			ID:       "d1",
			Time:     now.Add(-10 * time.Minute),
			Category: "DISPATCH",
			Agent:    "writer",
			Summary:  "写第 845 章",
		},
		{
			ID:         "t1",
			Time:       now.Add(-8 * time.Minute),
			FinishedAt: now.Add(-7 * time.Minute),
			Category:   "TOOL",
			Agent:      "writer",
			Summary:    "plan_chapter",
			Depth:      1,
			Duration:   time.Minute,
		},
		{
			ID:       "t2",
			Time:     now.Add(-2 * time.Minute),
			Category: "TOOL",
			Agent:    "writer",
			Summary:  "draft_chapter",
			Depth:    1,
		},
	}
	out := renderEventContent(events, 140, 0)
	if !strings.Contains(out, "think") {
		t.Fatalf("gap between tools should show think clock: %q", out)
	}
	if !strings.Contains(out, "5m") {
		t.Fatalf("think clock should be ~5m between tool finish and next tool_call: %q", out)
	}
}

func TestRenderEventLine_TOOL_FailedShowsCross(t *testing.T) {
	ev := host.Event{
		ID:         "e1",
		Time:       time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2025, 1, 1, 10, 0, 5, 0, time.UTC),
		Failed:     true,
		Discarded:  false,
		Category:   "TOOL",
		Agent:      "writer",
		Summary:    "draft_chapter",
		Level:      "error",
		Depth:      1,
		Duration:   5 * time.Second,
	}
	out := renderEventLine(ev, 100, 0)

	if !strings.Contains(out, "✕") {
		t.Errorf("failed TOOL should contain ✕: %q", out)
	}
	if strings.Contains(out, "⊘") {
		t.Errorf("failed TOOL should NOT contain ⊘: %q", out)
	}
}

func TestRenderEventLine_TOOL_CompletedShowsTree(t *testing.T) {
	ev := host.Event{
		ID:         "e1",
		Time:       time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2025, 1, 1, 10, 0, 5, 0, time.UTC),
		Category:   "TOOL",
		Agent:      "writer",
		Summary:    "draft_chapter",
		Level:      "info",
		Depth:      1,
		Duration:   5 * time.Second,
	}
	out := renderEventLine(ev, 100, 0)

	// completed (no fail/no discard) → ├ icon
	if !strings.Contains(out, "├") {
		t.Errorf("completed TOOL should contain ├: %q", out)
	}
	if strings.Contains(out, "⊘") {
		t.Errorf("completed TOOL should NOT contain ⊘: %q", out)
	}
	if strings.Contains(out, "✕") {
		t.Errorf("completed TOOL should NOT contain ✕: %q", out)
	}
}

// ── Model.applyEvent: Discarded field merge ─────────────────────────────

func TestModelApplyEvent_DiscardedMerge(t *testing.T) {
	m := NewModel(nil, nil, "")

	// 1. Start TOOL (running)
	startEv := host.Event{
		ID:       "tool-1",
		Time:     time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Category: "TOOL",
		Agent:    "writer",
		Summary:  "draft_chapter",
		Level:    "info",
		Depth:    1,
		// FinishedAt zero → running
	}
	m.applyEvent(startEv)

	if len(m.events) != 1 {
		t.Fatalf("expected 1 event after start, got %d", len(m.events))
	}
	if !m.events[0].Running() {
		t.Fatal("TOOL start event should be Running()")
	}

	// 2. Discard (retry strikes)
	discardEv := host.Event{
		ID:         "tool-1",
		Time:       time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2025, 1, 1, 10, 0, 5, 0, time.UTC),
		Discarded:  true,
		Category:   "TOOL",
		Agent:      "writer",
		Summary:    "draft_chapter",
		Level:      "info",
		Depth:      1,
		Duration:   5 * time.Second,
	}
	m.applyEvent(discardEv)

	if len(m.events) != 1 {
		t.Fatalf("expected still 1 event after discard merge, got %d", len(m.events))
	}
	ev := m.events[0]
	if !ev.Discarded {
		t.Error("event should be Discarded=true after merge")
	}
	if ev.Failed {
		t.Error("discarded event should NOT be Failed")
	}
	if ev.FinishedAt.IsZero() {
		t.Error("discarded event should have FinishedAt set after merge")
	}
	if ev.Running() {
		t.Error("discarded event should NOT be Running() after merge")
	}
	if ev.Duration != 5*time.Second {
		t.Errorf("discarded event Duration should be 5s, got %v", ev.Duration)
	}

	// 3. New TOOL after retry should get a different ID
	newStartEv := host.Event{
		ID:       "tool-2",
		Time:     time.Date(2025, 1, 1, 10, 0, 6, 0, time.UTC),
		Category: "TOOL",
		Agent:    "writer",
		Summary:  "draft_chapter(第2章)",
		Level:    "info",
		Depth:    1,
	}
	m.applyEvent(newStartEv)

	if len(m.events) != 2 {
		t.Fatalf("expected 2 events after new tool, got %d", len(m.events))
	}
	if !m.events[1].Running() {
		t.Error("new TOOL should be Running()")
	}
	if m.events[1].ID == m.events[0].ID {
		t.Error("new TOOL should have different ID from discarded tool")
	}
}

// ── Zero-delay retry rendering via renderEventLine ──────────────────────

func TestRenderEventLine_RetryZeroDelayShowsImmediate(t *testing.T) {
	// Simulate the retry SYSTEM event emitted when upstream sets retry_delay_ms=0
	ev := host.Event{
		Time:     time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Category: "SYSTEM",
		Agent:    "writer",
		Summary:  "重试 (2/7，即时后): provider stream idle",
		Level:    "warn",
		Depth:    1,
	}
	out := renderEventLine(ev, 100, 0)

	if !strings.Contains(out, "即时") {
		t.Errorf("zero-delay retry should show '即时' in rendering: %q", out)
	}
}

func TestRenderEventLine_RetryFallbackDelayShowsSeconds(t *testing.T) {
	// Exponential backoff fallback: attempt 2 → 2s
	ev := host.Event{
		Time:     time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
		Category: "SYSTEM",
		Agent:    "arbiter",
		Summary:  "重试 (2/7，2s后): rate limited",
		Level:    "warn",
		Depth:    1,
	}
	out := renderEventLine(ev, 100, 0)

	if !strings.Contains(out, "2s") {
		t.Errorf("fallback delay retry should show seconds: %q", out)
	}
	if strings.Contains(out, "即时") {
		t.Errorf("fallback delay should NOT show '即时': %q", out)
	}
}
