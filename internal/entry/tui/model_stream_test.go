package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/voocel/ainovel-cli/internal/host"
)

func TestStreamFlushIsDemandDriven(t *testing.T) {
	m := Model{
		streamVP:     viewport.New(80, 10),
		streamScroll: false,
	}

	next, cmd, handled := m.handleRuntimeMsg(streamDeltaMsg("first"))
	if !handled {
		t.Fatal("stream delta should be handled")
	}
	if cmd == nil {
		t.Fatal("first stream delta should schedule listening and one flush tick")
	}
	got := next.(Model)
	if !got.streamDirty {
		t.Fatal("stream delta should mark the stream dirty")
	}
	if !got.flushPending {
		t.Fatal("first stream delta should schedule a flush")
	}

	next, _, handled = got.handleRuntimeMsg(streamDeltaMsg(" second"))
	if !handled {
		t.Fatal("second stream delta should be handled")
	}
	got = next.(Model)
	if !got.flushPending {
		t.Fatal("additional stream delta should reuse the pending flush")
	}

	next, cmd, handled = got.handleRuntimeMsg(streamFlushTickMsg{})
	if !handled {
		t.Fatal("stream flush tick should be handled")
	}
	if cmd != nil {
		t.Fatal("clean stream should not schedule another flush tick")
	}
	got = next.(Model)
	if got.streamDirty {
		t.Fatal("flush tick should clear the dirty flag")
	}
	if got.flushPending {
		t.Fatal("flush tick should clear the scheduled flag")
	}
}

func TestIdleStreamFlushTickDoesNothing(t *testing.T) {
	m := Model{streamVP: viewport.New(80, 10)}
	next, cmd, handled := m.handleRuntimeMsg(streamFlushTickMsg{})
	if !handled {
		t.Fatal("stream flush tick should be handled")
	}
	if cmd != nil {
		t.Fatal("idle stream flush should not reschedule itself")
	}
	if next.(Model).streamDirty {
		t.Fatal("idle stream flush should leave the stream clean")
	}
}

func TestIdleAnimationTicksStopWithoutAnimationTargets(t *testing.T) {
	m := Model{streamVP: viewport.New(80, 10)}
	for _, msg := range []any{spinnerTickMsg{}, toolSpinnerTickMsg{}, cursorTickMsg{}} {
		next, cmd, handled := m.handleRuntimeMsg(msg)
		if !handled {
			t.Fatalf("%T should be handled", msg)
		}
		if cmd != nil {
			t.Fatalf("%T should not reschedule while idle", msg)
		}
		m = next.(Model)
	}
}

func TestFinalizeStaleEngineEventsStopsOnlyEngineCalls(t *testing.T) {
	started := time.Now().Add(-time.Second)
	m := Model{events: []host.Event{
		{ID: "dispatch", Time: started, Category: "DISPATCH", Summary: "writer"},
		{ID: "tool", Time: started, Category: "TOOL", Summary: "draft"},
		{ID: "decision", Time: started, Category: "DECISION", Summary: "用户干预裁定"},
	}}
	m.finalizeStaleEngineEvents(time.Now())

	if m.events[0].Running() || !m.events[0].Discarded {
		t.Fatal("stale dispatch should be finalized as discarded")
	}
	if m.events[1].Running() || !m.events[1].Discarded {
		t.Fatal("stale tool should be finalized as discarded")
	}
	if !m.events[2].Running() {
		t.Fatal("decision events must remain animatable for停机后的干预裁定")
	}
}
