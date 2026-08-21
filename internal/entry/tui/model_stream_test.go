package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/voocel/ainovel-cli/internal/host"
)

func TestStreamBatchAppendsCoalescedDeltas(t *testing.T) {
	m := Model{
		streamVP:     viewport.New(80, 10),
		streamScroll: false,
	}

	next, cmd, handled := m.handleRuntimeMsg(streamBatchMsg{
		ops: []streamOp{{text: "first"}, {text: " second"}},
	})
	if !handled {
		t.Fatal("stream batch should be handled")
	}
	if cmd != nil {
		t.Fatal("nil runtime should not schedule a listener")
	}
	got := next.(Model)
	if len(got.streamRounds) != 1 {
		t.Fatalf("round count = %d, want 1", len(got.streamRounds))
	}
	if got.streamRounds[0].text() != "first second" {
		t.Fatalf("stream text = %q, want %q", got.streamRounds[0].text(), "first second")
	}
}

func TestStreamBatchPreservesClearOrder(t *testing.T) {
	m := Model{streamVP: viewport.New(80, 10), streamScroll: false}
	next, _, handled := m.handleRuntimeMsg(streamBatchMsg{
		ops: []streamOp{{text: "before"}, {clear: true}, {text: "after"}},
	})
	if !handled {
		t.Fatal("stream batch should be handled")
	}
	got := next.(Model)
	if len(got.streamRounds) != 2 {
		t.Fatalf("round count = %d, want 2", len(got.streamRounds))
	}
	if got.streamRounds[0].text() != "before" || got.streamRounds[1].text() != "after" {
		t.Fatalf("rounds = %q / %q, want before / after", got.streamRounds[0].text(), got.streamRounds[1].text())
	}
}

func TestCollectStreamBatchFlushesBeforeClose(t *testing.T) {
	ch := make(chan string, 2)
	ch <- "a"
	ch <- "b"
	close(ch)

	got := collectStreamBatch(ch)
	if !got.closed {
		t.Fatal("closed source should mark the final batch closed")
	}
	if len(got.ops) != 2 || got.ops[0].text != "a" || got.ops[1].text != "b" {
		t.Fatalf("ops = %#v, want two ordered deltas", got.ops)
	}
}

func TestCollectStreamBatchStopsAtClearSentinel(t *testing.T) {
	ch := make(chan string, 3)
	ch <- "before"
	ch <- host.StreamClearSentinel
	ch <- "after"
	close(ch)

	first := collectStreamBatch(ch)
	if len(first.ops) != 2 || !first.ops[1].clear {
		t.Fatalf("first batch = %#v, want delta then clear", first.ops)
	}
	second := collectStreamBatch(ch)
	if len(second.ops) != 1 || second.ops[0].text != "after" || !second.closed {
		t.Fatalf("second batch = %#v closed=%v, want after + close", second.ops, second.closed)
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
	if m.events[2].Running() || !m.events[2].Discarded {
		t.Fatal("stale decision should be finalized when no ask-user modal is open")
	}
}

func TestFinalizeStaleEngineEventsKeepsAskUserDecision(t *testing.T) {
	started := time.Now().Add(-time.Second)
	m := Model{
		askState: &askUserState{},
		events: []host.Event{
			{ID: "decision", Time: started, Category: "DECISION", Summary: "用户干预裁定"},
		},
	}
	m.finalizeStaleEngineEvents(time.Now())
	if !m.events[0].Running() {
		t.Fatal("open ask-user modal must keep the decision event running")
	}
}
