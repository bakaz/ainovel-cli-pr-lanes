package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/host"
)

// handleIdleWritingTick 是 TUI 内的轻量调度器：它不另起常驻工作线程，
// 只在 Bubble Tea 事件循环中按窗口和运行事实决定是否提交一次 Resume。
func (m Model) handleIdleWritingTick(now time.Time) (tea.Model, tea.Cmd) {
	next := tickIdleWriting()
	schedule := host.IdleWritingStatusAt(now)
	if m.snapshot.IsRunning && schedule.InPeak &&
		(m.idleWritingActive || m.snapshot.PeakAutoPauseEnabled) {
		m.idleWritingActive = false
		m.idleWritingStartPending = false
		return m, tea.Batch(next, pauseForPeak(m.runtime))
	}
	if m.mode != modeRunning || m.starting || m.cocreate != nil || m.askState != nil || m.help != nil ||
		m.modelSwitch != nil || m.report != nil || m.importer != nil || m.simulator != nil {
		return m, next
	}

	if !m.snapshot.IdleWritingEnabled || schedule.InPeak || m.idleWritingSuspended ||
		m.snapshot.IsRunning || m.idleWritingStartPending || m.snapshot.Phase == "complete" ||
		m.snapshot.AdvanceMode != "auto" || m.snapshot.PendingSteer != "" || m.snapshot.HasAdvanceHold {
		return m, next
	}
	if m.snapshot.RuntimeState != "idle" && m.snapshot.RuntimeState != "paused" {
		return m, next
	}
	// 自然停机和高峰停机可以在下一窗口接力；人工暂停、预算停机、失败熔断等
	// 必须保留给用户处理，不能因为开关仍为 on 而循环烧钱。
	if reason := m.snapshot.LastStopReason; reason != "" && reason != "引擎自然停止" && !strings.Contains(reason, "高峰时段") {
		return m, next
	}
	m.idleWritingStartPending = true
	return m, tea.Batch(next, startIdleWriting(m.runtime, now))
}

func idleWritingStatusSummary(snap host.UISnapshot, now time.Time) string {
	schedule := host.IdleWritingStatusAt(now)
	enabled := "关闭"
	if snap.IdleWritingEnabled {
		enabled = "开启"
	}
	window := "当前为闲时"
	if schedule.InPeak {
		window = "当前为高峰时段"
	}
	next := "-"
	if !schedule.NextTransition.IsZero() {
		next = schedule.NextTransition.Format("01-02 15:04")
	}
	return fmt.Sprintf("闲时写作：%s；%s；下次切换北京时间 %s", enabled, window, next)
}

func peakAutoPauseStatusSummary(snap host.UISnapshot, now time.Time) string {
	schedule := host.IdleWritingStatusAt(now)
	enabled := "关闭"
	if snap.PeakAutoPauseEnabled {
		enabled = "开启"
	}
	window := "当前为闲时"
	if schedule.InPeak {
		window = "当前为高峰时段"
	}
	next := "-"
	if !schedule.NextTransition.IsZero() {
		next = schedule.NextTransition.Format("01-02 15:04")
	}
	return fmt.Sprintf("高峰自动暂停：%s；%s；下次切换北京时间 %s", enabled, window, next)
}

func (m Model) handleIdleWritingCommand(args []string) (tea.Model, tea.Cmd) {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off" && args[0] != "status") {
		m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "用法：/idle-writing on|off|status", Level: "error"})
		m.refreshEventViewport()
		return m, nil
	}
	if args[0] == "status" {
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: idleWritingStatusSummary(m.snapshot, time.Now()), Level: "info"})
		m.refreshEventViewport()
		return m, nil
	}

	enabled := args[0] == "on"
	wasActive := m.idleWritingActive
	if err := m.runtime.SetIdleWritingEnabled(enabled); err != nil {
		m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "切换闲时写作失败：" + err.Error(), Level: "error"})
		m.refreshEventViewport()
		return m, nil
	}
	m.idleWritingStartPending = false
	m.snapshot.IdleWritingEnabled = enabled
	m.snapshot.IdleWritingInPeak = host.IdleWritingStatusAt(time.Now()).InPeak
	if enabled {
		// 显式 on 代表用户重新授权自动恢复，可解除本次会话的手动暂停阻塞。
		m.idleWritingSuspended = false
	} else {
		m.idleWritingActive = false
	}

	cmds := []tea.Cmd{fetchSnapshot(m.runtime)}
	if !enabled && wasActive {
		cmds = append(cmds, stopIdleWriting(m.runtime))
	}
	if enabled && !m.idleWritingActive && m.mode == modeRunning && !m.starting && !m.snapshot.IsRunning &&
		!host.IdleWritingStatusAt(time.Now()).InPeak {
		m.idleWritingStartPending = true
		cmds = append(cmds, startIdleWriting(m.runtime, time.Now()))
	}
	return m, tea.Batch(cmds...)
}

// handlePeakAutoPauseCommand 切换北京时间高峰时段的全局自动暂停。
// 无参数时切换开关，on/off/status 用于显式操作或查询。
func (m Model) handlePeakAutoPauseCommand(args []string) (tea.Model, tea.Cmd) {
	if len(args) > 1 || (len(args) == 1 && args[0] != "on" && args[0] != "off" && args[0] != "status") {
		m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "用法：/idle-start [on|off|status]", Level: "error"})
		m.refreshEventViewport()
		return m, nil
	}
	if len(args) == 1 && args[0] == "status" {
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: peakAutoPauseStatusSummary(m.snapshot, time.Now()), Level: "info"})
		m.refreshEventViewport()
		return m, nil
	}

	enabled := !m.snapshot.PeakAutoPauseEnabled
	if len(args) == 1 {
		enabled = args[0] == "on"
	}
	if err := m.runtime.SetPeakAutoPauseEnabled(enabled); err != nil {
		m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "切换高峰自动暂停失败：" + err.Error(), Level: "error"})
		m.refreshEventViewport()
		return m, nil
	}
	m.snapshot.PeakAutoPauseEnabled = enabled

	now := time.Now()
	cmds := []tea.Cmd{fetchSnapshot(m.runtime)}
	if enabled && m.snapshot.IsRunning && host.IdleWritingStatusAt(now).InPeak {
		m.idleWritingActive = false
		m.idleWritingStartPending = false
		cmds = append(cmds, pauseForPeak(m.runtime))
	}
	return m, tea.Batch(cmds...)
}
