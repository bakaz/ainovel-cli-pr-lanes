package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/host"
)

// ── P0: layoutAffecting 分类 ────────────────────────────────────────────────

func TestLayoutAffectingClassification(t *testing.T) {
	// 纯视觉消息：跳过重布局。
	visualOnly := []tea.Msg{
		spinnerTickMsg{},
		toolSpinnerTickMsg{},
		cursorTickMsg{},
		streamBatchMsg{},
		streamDeltaMsg("delta"),
		streamClearMsg{},
		cursor.BlinkMsg{},
	}
	for _, msg := range visualOnly {
		if layoutAffecting(msg) {
			t.Errorf("%T 应跳过重布局（纯视觉消息）", msg)
		}
	}

	// 布局相关 / 不确定的消息：保守重布局。
	layoutMessages := []tea.Msg{
		tea.WindowSizeMsg{Width: 120, Height: 40},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.MouseMsg{},
		eventMsg{},
		doneMsg{},
		bootstrapMsg{},
		startResultMsg{},
		snapshotMsg{},
		snapshotTickMsg{},
		askUserMsg{},
		quitResetMsg{}, // 改变 inputHints 文案，保守重布局
		idleWritingTickMsg{},
		nil, // 未知消息一律重布局
	}
	for _, msg := range layoutMessages {
		if !layoutAffecting(msg) {
			t.Errorf("%T 必须触发重布局（保守策略）", msg)
		}
	}
}

// TestUpdateWrapperSkipsRelayoutOnVisualTicks 验证纯视觉 tick 不触发
// updateViewportSize：把 m.height 绕过 wrapper 直接改大，若 tick 后仍跑了
// 重布局，viewport 高度就会跟着变 —— 以此作为"是否重布局"的可观测量。
func TestUpdateWrapperSkipsRelayoutOnVisualTicks(t *testing.T) {
	m := NewModel(nil, nil, "test")
	m.mode = modeRunning
	m.width = 160
	m.height = 40
	m.updateViewportSize()
	baseline := m.viewport.Height
	if baseline <= 0 {
		t.Fatalf("setup: viewport height = %d", baseline)
	}

	// 绕过 wrapper 模拟"几何输入已变但未走 WindowSizeMsg"。
	m.height = 80

	next, _ := m.Update(toolSpinnerTickMsg{})
	got := next.(Model)
	if got.viewport.Height != baseline {
		t.Fatalf("tool spinner tick 不应重布局: viewport height %d -> %d", baseline, got.viewport.Height)
	}

	next, _ = got.Update(streamBatchMsg{ops: []streamOp{{text: "hello"}}})
	got = next.(Model)
	if got.viewport.Height != baseline {
		t.Fatalf("stream batch 不应重布局: viewport height %d -> %d", baseline, got.viewport.Height)
	}

	// 布局相关消息必须重算：WindowSizeMsg 带来的新高度要落到 viewport。
	next, _ = got.Update(tea.WindowSizeMsg{Width: 160, Height: 80})
	got = next.(Model)
	if got.viewport.Height == baseline {
		t.Fatal("WindowSizeMsg 后应按新高度重算 viewport 尺寸")
	}
}

// ── P3: runningEventCount 一致性 ───────────────────────────────────────────

func TestRunningEventCountTracksApplyEventLifecycle(t *testing.T) {
	m := NewModel(nil, nil, "")

	m.applyEvent(host.Event{ID: "t1", Category: "TOOL", Summary: "draft"})
	if m.runningEventCount != 1 || !m.hasRunningEvent() {
		t.Fatalf("after start: count=%d hasRunning=%v", m.runningEventCount, m.hasRunningEvent())
	}

	// 同 ID finish（正常完成）
	m.applyEvent(host.Event{ID: "t1", Category: "TOOL", FinishedAt: finishedAt(0)})
	if m.runningEventCount != 0 || m.hasRunningEvent() {
		t.Fatalf("after finish: count=%d hasRunning=%v", m.runningEventCount, m.hasRunningEvent())
	}

	// discard 路径同样收敛计数
	m.applyEvent(host.Event{ID: "t2", Category: "TOOL", Summary: "draft"})
	m.applyEvent(host.Event{ID: "t2", Category: "TOOL", FinishedAt: finishedAt(1), Discarded: true})
	if m.runningEventCount != 0 {
		t.Fatalf("after discard merge: count=%d", m.runningEventCount)
	}

	// 孤儿 finish（无对应 start）不应增加计数
	m.applyEvent(host.Event{ID: "ghost", Category: "TOOL", FinishedAt: finishedAt(2)})
	if m.runningEventCount != 0 {
		t.Fatalf("orphan finish changed count: count=%d", m.runningEventCount)
	}

	// 非 ID 事件永不计入
	m.applyEvent(host.Event{Category: "SYSTEM", Summary: "note"})
	if m.runningEventCount != 0 {
		t.Fatalf("SYSTEM event changed count: count=%d", m.runningEventCount)
	}
}

func TestRunningEventCountSurvivesTruncation(t *testing.T) {
	m := NewModel(nil, nil, "")

	total := maxEvents + 25
	for i := 0; i < total; i++ {
		ev := host.Event{Category: "SYSTEM", Summary: fmt.Sprintf("ev-%d", i)}
		if i%3 == 0 {
			ev.ID = fmt.Sprintf("e%d", i)
			ev.Category = "TOOL"
		}
		m.applyEvent(ev)
	}

	if len(m.events) != maxEvents {
		t.Fatalf("events len = %d, want %d", len(m.events), maxEvents)
	}
	scan := 0
	for _, e := range m.events {
		if e.Running() {
			scan++
		}
	}
	if m.runningEventCount != scan {
		t.Fatalf("count cache %d != actual running %d after truncation", m.runningEventCount, scan)
	}
	if scan == 0 {
		t.Fatal("setup: expected surviving running events past the truncation boundary")
	}
}

func TestFinalizeStaleEngineEventsUpdatesRunningCount(t *testing.T) {
	m := NewModel(nil, nil, "")
	m.applyEvent(host.Event{ID: "d1", Category: "DISPATCH", Summary: "writer"})
	m.applyEvent(host.Event{ID: "t1", Category: "TOOL", Summary: "draft"})
	m.applyEvent(host.Event{ID: "dec1", Category: "DECISION", Summary: "裁定"})
	if m.runningEventCount != 3 {
		t.Fatalf("setup count = %d, want 3", m.runningEventCount)
	}

	m.finalizeStaleEngineEvents(finishedAt(0))
	if m.runningEventCount != 0 || m.hasRunningEvent() {
		t.Fatalf("after finalize: count=%d hasRunning=%v", m.runningEventCount, m.hasRunningEvent())
	}
}

func TestFinalizeStaleEngineEventsKeepsAskDecisionInCount(t *testing.T) {
	m := NewModel(nil, nil, "")
	m.askState = &askUserState{}
	m.applyEvent(host.Event{ID: "dec1", Category: "DECISION", Summary: "裁定"})

	m.finalizeStaleEngineEvents(finishedAt(0))
	if m.runningEventCount != 1 || !m.hasRunningEvent() {
		t.Fatalf("open ask modal must keep decision counted: count=%d", m.runningEventCount)
	}
}

func TestDoneMsgRecalibratesRunningCount(t *testing.T) {
	m := NewModel(nil, nil, "")
	m.mode = modeRunning
	m.width = 160
	m.height = 40
	m.updateViewportSize()
	// 模拟计数缓存与实际漂移（如直接构造/历史路径遗漏）：
	// 实际 2 条 running，缓存记 5。
	m.applyEvent(host.Event{ID: "d1", Category: "DISPATCH", Summary: "writer"})
	m.applyEvent(host.Event{ID: "t1", Category: "TOOL", Summary: "draft"})
	m.runningEventCount = 5

	next, _, handled := m.handleRuntimeMsg(doneMsg{})
	if !handled {
		t.Fatal("doneMsg should be handled")
	}
	got := next.(Model)
	if got.runningEventCount != 0 || got.hasRunningEvent() {
		t.Fatalf("doneMsg should recalibrate to actual (0): count=%d", got.runningEventCount)
	}
}

func TestResetOutputPanelsZeroesRunningCount(t *testing.T) {
	m := NewModel(nil, nil, "")
	m.applyEvent(host.Event{ID: "t1", Category: "TOOL", Summary: "draft"})
	if m.runningEventCount != 1 {
		t.Fatalf("setup count = %d", m.runningEventCount)
	}
	m.resetOutputPanels()
	if m.runningEventCount != 0 || m.hasRunningEvent() {
		t.Fatalf("resetOutputPanels left count=%d", m.runningEventCount)
	}
}

// finishedAt 返回一个非零完成时间（Running() == false）。
func finishedAt(seconds int) time.Time {
	return time.Now().Add(time.Duration(seconds) * time.Second)
}
