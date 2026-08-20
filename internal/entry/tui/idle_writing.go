package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/host"
)

// handleIdleWritingTick 只负责唤醒 Host 状态机；恢复资格、运行来源和停止原因
// 均由 Host 根据持久化 RunControl 裁定。
func (m Model) handleIdleWritingTick(now time.Time) (tea.Model, tea.Cmd) {
	next := tickIdleWriting()
	if m.mode != modeRunning || m.starting {
		return m, next
	}
	return m, tea.Batch(next, reconcileSchedule(m.runtime, now))
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
	skip := ""
	if !snap.PeakOverrideUntil.IsZero() && now.Before(snap.PeakOverrideUntil) {
		skip = fmt.Sprintf("；本窗口已跳过至 %s", snap.PeakOverrideUntil.In(time.FixedZone(host.IdleWritingTimezone, 8*60*60)).Format("15:04"))
	}
	return fmt.Sprintf("高峰自动暂停：%s；%s；下次切换北京时间 %s%s", enabled, window, next, skip)
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
	wasActive := m.snapshot.IsRunning && m.snapshot.RunOrigin == "idle_scheduler"
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
	if enabled && m.mode == modeRunning && !m.starting {
		cmds = append(cmds, reconcileSchedule(m.runtime, time.Now()))
	}
	return m, tea.Batch(cmds...)
}

// handlePeakPauseCommand 切换高峰自动暂停；skip 只跳过当前北京时间窗口。
func (m Model) handlePeakPauseCommand(args []string) (tea.Model, tea.Cmd) {
	if len(args) > 1 || (len(args) == 1 && args[0] != "on" && args[0] != "off" && args[0] != "status" && args[0] != "skip") {
		m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "用法：/peak-pause [on|off|status|skip]", Level: "error"})
		m.refreshEventViewport()
		return m, nil
	}
	if len(args) == 1 && args[0] == "status" {
		m.applyEvent(host.Event{Time: time.Now(), Category: "SYSTEM", Summary: peakAutoPauseStatusSummary(m.snapshot, time.Now()), Level: "info"})
		m.refreshEventViewport()
		return m, nil
	}
	if len(args) == 1 && args[0] == "skip" {
		if err := m.runtime.SetPeakPauseSkip(time.Now()); err != nil {
			m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "跳过当前高峰失败：" + err.Error(), Level: "error"})
			m.refreshEventViewport()
			return m, nil
		}
		return m, fetchSnapshot(m.runtime)
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

	cmds := []tea.Cmd{fetchSnapshot(m.runtime)}
	if enabled && m.mode == modeRunning && !m.starting {
		cmds = append(cmds, reconcileSchedule(m.runtime, time.Now()))
	}
	return m, tea.Batch(cmds...)
}
