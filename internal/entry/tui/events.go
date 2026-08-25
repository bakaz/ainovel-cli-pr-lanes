package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/backup"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

// 消息类型
type (
	eventMsg        host.Event
	snapshotMsg     host.UISnapshot // one-shot refresh; must not schedule another periodic tick
	snapshotTickMsg host.UISnapshot // periodic refresh; owns the single recurring tick chain
	doneMsg         struct {
		complete   bool
		stopReason string
	} // complete=true 全书完成，false 出错停止
	abortResultMsg            struct{ stopped bool }
	idleWritingStartResultMsg struct {
		started bool
		err     error
	}
	idleWritingPauseResultMsg struct{ stopped bool }
	scheduleReconcileMsg      struct {
		result host.ScheduleReconcileResult
		err    error
	}
	bootstrapMsg struct {
		replay   []domain.RuntimeQueueItem
		resumed  bool
		deferred bool
		err      error
	}
	reportLoadedMsg struct {
		reqID      int
		report     diag.Report
		exportPath string // 脱敏诊断文件绝对路径；空 = 导出失败
		finishedAt time.Time
	}
	askUserMsg       askUserRequest
	askUserCancelMsg askUserCancellation
	startResultMsg   struct{ err error }
	cocreateDeltaMsg struct {
		reqID int
		kind  string // host.CoCreateProgressThinking | host.CoCreateProgressReply
		text  string
	}
	// cocreateStreamItem 是 deltaCh 内部载荷，把流式 kind 与累积文本一起送达 TUI。
	cocreateStreamItem struct {
		kind string
		text string
	}
	cocreateDoneMsg struct {
		reqID int
		reply host.CoCreateReply
		err   error
	}
	steerResultMsg     struct{}
	continueResultMsg  struct{ err error }
	spinnerTickMsg     time.Time
	toolSpinnerTickMsg time.Time // 事件流工具 spinner 独立 tick（更快、独立于顶栏/星星）
	cursorTickMsg      time.Time // 流式光标独立 tick
	streamDeltaMsg     string    // 兼容旧测试/回放调用的单个流式增量
	streamClearMsg     struct{}  // 兼容旧测试/回放调用的清空流式缓冲
	idleWritingTickMsg time.Time // 闲时写作调度 tick
	quitResetMsg       struct{}  // 双次 Ctrl+C 超时重置
	restoreResultMsg   struct {
		result *backup.RestoreResult
		err    error
	}
)

// --- Cmd 函数 ---

func listenEvents(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-rt.Events()
		if !ok {
			return nil
		}
		return eventMsg(ev)
	}
}

func listenDone(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		_, ok := <-rt.Done()
		if !ok {
			return nil
		}
		snap := rt.Snapshot()
		return doneMsg{complete: snap.Phase == "complete", stopReason: snap.LastStopReason}
	}
}

const (
	snapshotTickRunning = 3 * time.Second
	snapshotTickIdle    = 15 * time.Second
)

func tickSnapshot(rt *host.Host, idle bool) tea.Cmd {
	if rt == nil {
		return nil
	}
	d := snapshotTickRunning
	if idle {
		d = snapshotTickIdle
	}
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return snapshotTickMsg(rt.Snapshot())
	})
}

const idleWritingTickInterval = 15 * time.Second

func tickIdleWriting() tea.Cmd {
	return tea.Tick(idleWritingTickInterval, func(t time.Time) tea.Msg {
		return idleWritingTickMsg(t)
	})
}

func startIdleWriting(rt *host.Host, now time.Time) tea.Cmd {
	return func() tea.Msg {
		started, err := rt.StartIdleWriting(now)
		return idleWritingStartResultMsg{started: started, err: err}
	}
}

func pauseIdleWriting(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return idleWritingPauseResultMsg{stopped: rt.PauseIdleWriting()}
	}
}

func pauseForPeak(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return idleWritingPauseResultMsg{stopped: rt.PauseForPeak()}
	}
}

func reconcileSchedule(rt *host.Host, now time.Time) tea.Cmd {
	return func() tea.Msg {
		result, err := rt.ReconcileSchedule(now)
		return scheduleReconcileMsg{result: result, err: err}
	}
}

func stopIdleWriting(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return idleWritingPauseResultMsg{stopped: rt.StopIdleWriting()}
	}
}

func fetchSnapshot(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return snapshotMsg(rt.Snapshot())
	}
}

func restoreSnapshot(rt *host.Host, snapshotID string) tea.Cmd {
	return func() tea.Msg {
		result, err := rt.RestoreSnapshot(snapshotID, true)
		return restoreResultMsg{result: result, err: err}
	}
}

func bootstrapRuntime(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		replay, err := rt.ReplayStreamQueue(0)
		if err != nil {
			return bootstrapMsg{err: err}
		}
		label, deferred, err := rt.ResumeForTUI(time.Now())
		if err != nil {
			return bootstrapMsg{replay: replay, err: err}
		}
		if label == "" && len(replay) == 0 && !deferred {
			return nil
		}
		return bootstrapMsg{replay: replay, resumed: label != "" && !deferred, deferred: deferred}
	}
}

func startRuntime(rt *host.Host, plan startup.Plan) tea.Cmd {
	return func() tea.Msg {
		// 启动侧确定性生成本书用户规则快照（用原始 prompt 归一化），须在 StartPrepared 前。
		if err := rt.PrepareUserRules(plan.RawPrompt); err != nil {
			return startResultMsg{err: err}
		}
		err := rt.StartPrepared(plan.RawPrompt)
		return startResultMsg{err: err}
	}
}

func runCoCreate(rt *host.Host, state *cocreateState) tea.Cmd {
	history := state.session.History()
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	state.deltaCh = make(chan cocreateStreamItem, 64)
	state.doneCh = make(chan cocreateDoneMsg, 1)
	// 阶段共创带故事状态摘要、产出"后续方向 brief"；冷启动从零澄清需求。两者签名一致。
	stream := rt.CoCreateStream
	if state.stage {
		stream = rt.StageCoCreateStream
	}
	start := func() tea.Msg {
		go func() {
			reply, err := stream(ctx, history, func(kind, text string) {
				select {
				case state.deltaCh <- cocreateStreamItem{kind: kind, text: text}:
				default:
				}
			})
			state.doneCh <- cocreateDoneMsg{reply: reply, err: err}
			close(state.deltaCh)
			close(state.doneCh)
		}()
		return nil
	}
	return tea.Batch(start, listenCoCreateDelta(state), listenCoCreateDone(state))
}

func listenCoCreateDelta(state *cocreateState) tea.Cmd {
	if state == nil || state.deltaCh == nil {
		return nil
	}
	// 抓取 channel 局部引用：避免后续 state.deltaCh 被 reassign 时
	// 旧 listen 闭包错读新 channel（虽然当前流程不触发，留作维护陷阱不应该）。
	reqID := state.reqID
	ch := state.deltaCh
	return func() tea.Msg {
		item, ok := <-ch
		if !ok {
			return nil
		}
		return cocreateDeltaMsg{reqID: reqID, kind: item.kind, text: item.text}
	}
}

func listenCoCreateDone(state *cocreateState) tea.Cmd {
	if state == nil || state.doneCh == nil {
		return nil
	}
	reqID := state.reqID
	ch := state.doneCh
	return func() tea.Msg {
		result, ok := <-ch
		if !ok {
			return nil
		}
		result.reqID = reqID
		return result
	}
}

func steerRuntime(rt *host.Host, text string) tea.Cmd {
	return func() tea.Msg {
		rt.Steer(text)
		return steerResultMsg{}
	}
}

func continueRuntime(rt *host.Host, text string) tea.Cmd {
	return func() tea.Msg {
		err := rt.Continue(text)
		return continueResultMsg{err: err}
	}
}

// resumeFromCoCreate 把阶段共创产出的后续方向 brief 注入并恢复创作。
// 复用 continueResultMsg：成功即接 listenDone 续跑，失败回显错误。
func resumeFromCoCreate(rt *host.Host, draft string) tea.Cmd {
	return func() tea.Msg {
		err := rt.ResumeFromCoCreate(draft)
		return continueResultMsg{err: err}
	}
}

// cancelCoCreate 放弃阶段共创：清占用标记、保持暂停。事件经 events 通道回流，无需返回消息。
func cancelCoCreate(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		rt.CancelCoCreate()
		return nil
	}
}

func abortRuntime(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return abortResultMsg{stopped: rt.Abort()}
	}
}

func loadReport(dir string, reqID int) tea.Cmd {
	return func() tea.Msg {
		// 复核阻塞项 2 只读模式：诊断/导出只读，用 NewReadOnlyStore（Host 持有
		// 写锁时也可生成报告）。
		s := store.NewReadOnlyStore(dir)
		// Diagnose = 创作诊断 + 运行时检测，运行时 Finding 也进屏上报告。
		rep, rc := diag.Diagnose(s)
		// 复用 rep+rc 写出脱敏诊断文件（导出失败不影响屏上报告）。
		exportPath, _ := diag.WriteExport(s, rep, rc)
		return reportLoadedMsg{
			reqID:      reqID,
			report:     rep,
			exportPath: exportPath,
			finishedAt: time.Now(),
		}
	}
}

func tickSpinner() tea.Cmd {
	return tea.Tick(350*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}

// toolSpinnerTickInterval 事件流"进行中"行 spinner 的刷新间隔。
// 与主 spinner 同频（350ms）：每个 tick 都要全量重渲 ≤500 行事件流，
// 旧的 150ms 节奏在长事件流下是持续 CPU 空转源；8 帧 350ms 转一圈 2.8s，
// 视觉上仍连贯。
const toolSpinnerTickInterval = 350 * time.Millisecond

// tickToolSpinner 驱动事件流"进行中"行的 spinner。独立于 tickSpinner 的帧索引，
// 节奏见 toolSpinnerTickInterval（与主 spinner 同为 350ms）。
func tickToolSpinner() tea.Cmd {
	return tea.Tick(toolSpinnerTickInterval, func(t time.Time) tea.Msg {
		return toolSpinnerTickMsg(t)
	})
}

func tickCursor() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(t time.Time) tea.Msg {
		return cursorTickMsg(t)
	})
}

func quitTUI() tea.Cmd {
	return tea.Sequence(tea.DisableMouse, tea.Quit)
}

const (
	streamBatchWindow   = 33 * time.Millisecond
	streamBatchMaxBytes = 64 * 1024
)

// collectStreamBatch continuously drains the source while a short batching
// window is open. This keeps Host.streamCh from filling during token bursts,
// while returning at most one Bubble Tea message per window. Clear sentinels
// are returned in the same ordered batch and terminate that batch immediately,
// so deltas after the sentinel are read by the next listener.
func collectStreamBatch(ch <-chan string) streamBatchMsg {
	first, ok := <-ch
	if !ok {
		return streamBatchMsg{closed: true}
	}
	ops := make([]streamOp, 0, 8)
	bytes := 0
	appendOp := func(delta string) {
		if delta == host.StreamClearSentinel {
			ops = append(ops, streamOp{clear: true})
			return
		}
		ops = append(ops, streamOp{text: delta})
		bytes += len(delta)
	}
	appendOp(first)
	if first == host.StreamClearSentinel {
		return streamBatchMsg{ops: ops}
	}

	timer := time.NewTimer(streamBatchWindow)
	defer timer.Stop()
	for {
		select {
		case delta, ok := <-ch:
			if !ok {
				return streamBatchMsg{ops: ops, closed: true}
			}
			appendOp(delta)
			if delta == host.StreamClearSentinel || bytes >= streamBatchMaxBytes {
				return streamBatchMsg{ops: ops}
			}
		case <-timer.C:
			return streamBatchMsg{ops: ops}
		}
	}
}

func listenStream(rt *host.Host) tea.Cmd {
	return func() tea.Msg {
		return collectStreamBatch(rt.Stream())
	}
}

func listenAskUser(bridge *askUserBridge) tea.Cmd {
	return func() tea.Msg {
		select {
		case req, ok := <-bridge.requests:
			if !ok {
				return nil
			}
			return askUserMsg(req)
		case cancellation := <-bridge.cancellations:
			return askUserCancelMsg(cancellation)
		}
	}
}
