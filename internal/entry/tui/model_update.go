package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	"github.com/voocel/ainovel-cli/internal/utils"
)

const maxPromptEventCols = 160

// layoutAffecting 报告一条消息是否可能改变面板布局几何（顶栏高度、输入框
// 行数、viewport 宽高）。updateViewportSize 为量高要完整跑一遍 lipgloss 渲染
// （renderTopBar + renderBottomBar），是每条消息的固定开销；高频纯视觉 tick
// （spinner/cursor/stream delta，30FPS 流式下每秒上百条）不会改变几何，
// 跳过重布局可消除持续 CPU 空转。
//
// 保守策略：只对确认纯视觉的消息返回 false；不确定的消息一律返回 true
// 重布局——宁可多做一次量高，也不冒布局失真的风险。
func layoutAffecting(msg tea.Msg) bool {
	switch msg.(type) {
	case spinnerTickMsg, toolSpinnerTickMsg, cursorTickMsg:
		// 动画帧推进：只重渲对应 viewport 内容，不改任何几何输入。
		return false
	case streamBatchMsg, streamDeltaMsg, streamClearMsg:
		// 流式增量：只写 streamRounds + refreshStreamViewport，不改几何。
		return false
	case cursor.BlinkMsg:
		// textarea 光标闪烁：内容不变 → refitTextareaHeight 结果不变。
		return false
	default:
		return true
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	mm, ok := next.(Model)
	if !ok {
		return next, cmd
	}
	// View 禁止改 Model。textarea 多行、顶栏高度变化都在消息处理之后才稳定，
	// 所以在 Update 末尾同步 viewport 尺寸，供下一帧 View 使用。
	// 纯视觉 tick 消息（见 layoutAffecting）不重算布局。
	if mm.width > 0 && layoutAffecting(msg) {
		mm.updateViewportSize()
	}
	return mm, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		syncBodyTextTheme()
		m.width = msg.Width
		m.height = msg.Height
		m.resizeTextarea()
		m.updateViewportSize()
		// viewport 只保存已经排好版的行；改宽度不会自动重新换行。
		// 因此 resize 必须按新尺寸重建四个面板，否则事件/流式内容会继续
		// 使用旧窗口宽度生成的行，表现为不刷新、残留截断或错位。
		m.refreshEventViewport()
		m.refreshStreamViewport()
		m.refreshDetailViewport()
		m.refreshStateViewport()
		if m.streamScroll {
			m.streamVP.GotoBottom()
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)
	case tea.MouseMsg:
		return m.handleMouseMsg(msg)
	default:
		if next, cmd, handled := m.handleRuntimeMsg(msg); handled {
			return next, cmd
		}
		return m.handleTextareaMsg(msg)
	}
}

func (m Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if next, cmd, handled := m.handleOverlayKeyMsg(msg); handled {
		return next, cmd
	}

	if msg.Type == tea.KeyCtrlC {
		if m.quitPending {
			return m, quitTUI()
		}
		m.quitPending = true
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return quitResetMsg{} })
	}
	m.quitPending = false

	if next, cmd, handled := m.handleCommandPaletteKey(msg); handled {
		return next, cmd
	}

	return m.handleBaseKeyMsg(msg)
}

func (m Model) handleOverlayKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch {
	case m.askState != nil:
		return m.handleBlockingModalKey(msg, m.handleAskUserKey)
	case m.cocreate != nil:
		return m.handleBlockingModalKey(msg, m.handleCoCreateKey)
	case m.help != nil:
		return m.handleBlockingModalKey(msg, m.handleHelpKey)
	case m.modelSwitch != nil:
		return m.handleBlockingModalKey(msg, m.handleModelSwitchKey)
	case m.report != nil:
		return m.handleBlockingModalKey(msg, m.handleReportKey)
	case m.importer != nil:
		return m.handleBlockingModalKey(msg, m.handleImportKey)
	case m.simulator != nil:
		return m.handleBlockingModalKey(msg, m.handleSimulationKey)
	default:
		return m, nil, false
	}
}

func (m Model) handleBlockingModalKey(msg tea.KeyMsg, next func(tea.KeyMsg) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd, bool) {
	if msg.Type == tea.KeyCtrlC {
		if m.quitPending {
			return m, quitTUI(), true
		}
		m.quitPending = true
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return quitResetMsg{} }), true
	}
	m.quitPending = false
	// 跨模态全局快捷键：modal 打开期间也要能切鼠标上报，否则共创/help/report 等
	// 锁屏式 modal 下用户无法用原生拖拽选中复制。
	if msg.Type == tea.KeyCtrlR {
		next, cmd := m.toggleMouseReporting()
		return next, cmd, true
	}
	model, cmd := next(msg)
	return model, cmd, true
}

// toggleMouseReporting 切换鼠标上报开关。开 → 关让用户原生拖拽选中复制；
// 关 → 开恢复点击切焦点 / 滚轮。base 路径与 blocking modal 路径共用。
func (m Model) toggleMouseReporting() (Model, tea.Cmd) {
	// 欢迎页(modeNew)本就不开鼠标上报，原生拖拽即可复制；此处忽略 Ctrl+R，
	// 避免误开上报反而破坏原生复制。鼠标上报由 enterRunning 在进入工作台时打开。
	if m.mode == modeNew {
		return m, nil
	}
	m.mouseOff = !m.mouseOff
	if m.mouseOff {
		return m, tea.DisableMouse
	}
	return m, tea.EnableMouseCellMotion
}

// enterRunning 进入创作工作台：开启鼠标上报（工作台需要点击切面板 / 滚轮 /
// 拖拽侧边栏）。返回的命令需由调用方 Batch 进最终返回值。
func (m *Model) enterRunning() tea.Cmd {
	m.mode = modeRunning
	m.mouseOff = false
	return tea.EnableMouseCellMotion
}

func (m Model) handleCommandPaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if !m.compActive {
		return m, nil, false
	}

	switch msg.Type {
	case tea.KeyEsc:
		m.clearCommandPalette()
		return m, nil, true
	case tea.KeyUp:
		if m.compIdx > 0 {
			m.compIdx--
		}
		return m, nil, true
	case tea.KeyDown:
		if m.compIdx < len(m.compItems)-1 {
			m.compIdx++
		}
		return m, nil, true
	case tea.KeyTab:
		m.acceptCommandCompletion()
		return m, nil, true
	case tea.KeyEnter:
		item, ok := m.acceptCommandCompletion()
		if !ok {
			return m, nil, true
		}
		if item.AutoExecute {
			m.textarea.Reset()
			next, cmd := m.handleSlashCommand(slashCommand{name: item.Name})
			return next, cmd, true
		}
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m Model) handleBaseKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 节流防御：粘贴 \n 在不支持 bracketed paste 的终端会退化成连续 KeyEnter；
	// 真人按 Enter 与前一字符间隔通常 > 100ms，<50ms 极可能是粘贴流残片。
	// 只记 KeyRunes（字符流）—— 功能键（↑↓/Tab/Ctrl-x）不应污染节流，
	// 否则用户翻历史选定后立刻按 Enter 会被误吞。
	if msg.Type == tea.KeyRunes {
		m.lastKeyAt = time.Now()
	}
	switch msg.Type {
	case tea.KeyEscape:
		if m.mode == modeRunning && m.snapshot.IsRunning {
			m.idleWritingActive = false
			m.idleWritingStartPending = false
			m.idleWritingSuspended = true
			return m, abortRuntime(m.runtime)
		}
		m.textarea.Reset()
		m.historyIdx = len(m.inputHistory)
		m.historyDraft = ""
		m.refitTextareaHeight()
		m.clearCommandPalette()
		return m, nil
	case tea.KeyCtrlL:
		m.resetOutputPanels()
		return m, nil
	case tea.KeyCtrlU:
		// 清空当前输入；同时退出历史浏览态。
		m.textarea.Reset()
		m.historyIdx = len(m.inputHistory)
		m.historyDraft = ""
		m.refitTextareaHeight()
		m.clearCommandPalette()
		return m, nil
	case tea.KeyCtrlR:
		return m.toggleMouseReporting()
	case tea.KeyTab:
		if m.mode == modeNew {
			if m.cocreate != nil {
				return m, nil
			}
			if m.startupMode == startupModeQuick {
				m.startupMode = startupModeCoCreate
			} else {
				m.startupMode = startupModeQuick
			}
			m.textarea.Placeholder = placeholderForNewMode(m.startupMode)
			return m, nil
		}
		m.focusPane = (m.focusPane + 1) % focusPaneCount
		return m, nil
	case tea.KeyEnter:
		// Alt+Enter 是主动换行，让 textarea.Update 接管（KeyMap.InsertNewline 已绑到此键）。
		if msg.Alt {
			break
		}
		// 与上一次非 Enter 按键间隔过短 → 视为粘贴流的 \n 残片：
		// 替换为空格保留视觉间隔，与 cleanHumanKeyRunes 路径语义一致（"abc\ndef" → "abc def"）。
		// 防御 bracketed paste 失效的终端环境（旧 SSH/某些 tmux 配置）。
		if !m.lastKeyAt.IsZero() && time.Since(m.lastKeyAt) < 50*time.Millisecond {
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
			m.refitTextareaHeight()
			return m, cmd
		}
		return m.handleEnterKey()
	case tea.KeyUp:
		// 多行输入：让 textarea 接管光标行内移动（落到 switch 后的 textarea.Update）
		if m.textareaIsMultiline() {
			break
		}
		// 单行：优先翻历史，没有可用历史时回退到事件流滚动
		if m.tryHistoryUp() {
			return m, nil
		}
		return m.handleVerticalScrollKey(msg, true)
	case tea.KeyDown:
		if m.textareaIsMultiline() {
			break
		}
		if m.tryHistoryDown() {
			return m, nil
		}
		return m.handleVerticalScrollKey(msg, false)
	case tea.KeyPgUp:
		return m.handleVerticalScrollKey(msg, true)
	case tea.KeyPgDown:
		return m.handleVerticalScrollKey(msg, false)
	case tea.KeyEnd:
		switch m.focusPane {
		case focusStream:
			m.streamScroll = true
			m.streamVP.GotoBottom()
		case focusDetail:
			m.detailVP.GotoBottom()
		case focusState:
			m.stateVP.GotoBottom()
		default:
			m.autoScroll = true
			m.viewport.GotoBottom()
		}
		return m, nil
	}

	if msg.Type == tea.KeyRunes && (containsSGRFragment(string(msg.Runes)) || isCSILeak(msg.Runes)) {
		return m, nil
	}
	var ok bool
	if msg, ok = cleanHumanKeyRunes(msg); !ok {
		return m, nil
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refitTextareaHeight()
	m.updateCommandPalette()
	return m, cmd
}

func (m Model) handleEnterKey() (tea.Model, tea.Cmd) {
	text := utils.CleanInputLine(m.textarea.Value())
	if text == "" {
		return m, nil
	}
	m.clearCommandPalette()
	if cmd, ok := parseSlashCommand(text); ok {
		m.pushInputHistory(text)
		m.textarea.Reset()
		m.refitTextareaHeight()
		return m.handleSlashCommand(cmd)
	}

	m.pushInputHistory(text)
	m.textarea.Reset()
	m.refitTextareaHeight()
	switch m.mode {
	case modeNew:
		m.err = nil
		if m.startupMode == startupModeQuick {
			plan, err := startup.PrepareQuick(startup.Request{
				Mode:        startup.ModeQuick,
				UserPrompt:  text,
				OutputDir:   m.runtime.Dir(),
				Interactive: true,
			})
			if err != nil {
				m.err = err
				return m, nil
			}
			cmd := m.enterStarting(plan.RawPrompt)
			return m, tea.Batch(startRuntime(m.runtime, plan), cmd)
		}
		m.cocreate = newCoCreateState(text)
		return m, m.sendCoCreate()
	case modeRunning:
		m.idleWritingActive = false
		m.idleWritingStartPending = false
		// 不本地回显 USER 事件 —— Host.Continue/Steer 入口已 emit "USER" 事件，
		// 走 events channel 回流到 TUI。架构 §2.3：观察层只观察，不产生事实。
		if !m.snapshot.IsRunning {
			return m, continueRuntime(m.runtime, text)
		}
		return m, steerRuntime(m.runtime, text)
	case modeDone:
		// 完结后用户输入（返工/续写诉求）：唤醒新一轮 run。Continue 在停机态走 Inject
		// 自动恢复，Arbiter 裁定用户干预；返工已写章时由 Engine 重开全书并入队。
		// 切回 modeRunning 重入工作台；本轮跑完
		// doneMsg(complete) 会再置 modeDone。斜杠命令已在上面提前处理，不经此分支。
		m.mode = modeRunning
		return m, continueRuntime(m.runtime, text)
	default:
		return m, nil
	}
}

func (m Model) handleVerticalScrollKey(msg tea.KeyMsg, upward bool) (tea.Model, tea.Cmd) {
	if m.focusPane == focusStream {
		if upward {
			m.streamScroll = false
		}
		var cmd tea.Cmd
		m.streamVP, cmd = m.streamVP.Update(msg)
		if !upward && m.streamVP.AtBottom() {
			m.streamScroll = true
		}
		return m, cmd
	}
	if m.focusPane == focusDetail {
		var cmd tea.Cmd
		m.detailVP, cmd = m.detailVP.Update(msg)
		return m, cmd
	}
	if m.focusPane == focusState {
		var cmd tea.Cmd
		m.stateVP, cmd = m.stateVP.Update(msg)
		return m, cmd
	}
	if upward {
		m.autoScroll = false
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	if !upward && m.viewport.AtBottom() {
		m.autoScroll = true
	}
	return m, cmd
}

func (m Model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.cocreate != nil {
		// 鼠标按 X 坐标分流：屏幕左半 = conv 面板，右半 = prompt 面板。
		// modal 居中且 conv 占左 ~58%，用屏幕中线判别足够准确。
		// 用户在 conv 区滚轮自动停止 follow（让其能稳定停在某个历史位置）。
		var cmd tea.Cmd
		if msg.X < m.width/2 {
			m.cocreate.convFollow = false
			m.cocreate.convVP, cmd = m.cocreate.convVP.Update(msg)
			if m.cocreate.convVP.AtBottom() {
				m.cocreate.convFollow = true
			}
		} else {
			m.cocreate.promptVP, cmd = m.cocreate.promptVP.Update(msg)
		}
		return m, cmd
	}
	if m.modelSwitch != nil || m.askState != nil {
		return m, nil
	}
	if pane, ok := m.paneAtMouse(msg.X, msg.Y); ok {
		m.hoverPane = pane
		m.hoverActive = true
		if msg.Action == tea.MouseActionPress {
			m.focusPane = pane
		}
	} else {
		m.hoverActive = false
	}
	if msg.Action == tea.MouseActionMotion {
		return m, nil
	}

	var cmd tea.Cmd
	if m.focusPane == focusStream {
		m.streamVP, cmd = m.streamVP.Update(msg)
		if msg.Action == tea.MouseActionPress {
			m.streamScroll = m.streamVP.AtBottom()
		}
		return m, cmd
	}
	if m.focusPane == focusDetail {
		m.detailVP, cmd = m.detailVP.Update(msg)
		return m, cmd
	}
	if m.focusPane == focusState {
		m.stateVP, cmd = m.stateVP.Update(msg)
		return m, cmd
	}
	m.viewport, cmd = m.viewport.Update(msg)
	if msg.Action == tea.MouseActionPress {
		m.autoScroll = m.viewport.AtBottom()
	}
	return m, cmd
}

func (m Model) handleRuntimeMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case eventMsg:
		ev := host.Event(msg)
		m.applyEvent(ev)
		m.refreshEventViewport()
		m.refreshStateViewport()
		return m, tea.Batch(listenEvents(m.runtime), m.scheduleAnimationTicks()), true
	case bootstrapMsg:
		// 先回放历史事件再处理错误：Resume 被拒（如预算上限）是常规路径，
		// 用户需要在看得到历史的前提下读到拒绝原因，而不是面对空白事件流。
		m.applyRuntimeReplay(msg.replay)
		if msg.err != nil {
			m.err = msg.err
			return m, fetchSnapshot(m.runtime), true
		}
		if (msg.resumed || msg.deferred) && m.mode == modeNew {
			enableMouse := m.enterRunning()
			m.resizeTextarea()
			m.textarea.Placeholder = defaultSteerPlaceholder()
			return m, tea.Batch(fetchSnapshot(m.runtime), enableMouse, m.scheduleAnimationTicks()), true
		}
		return m, fetchSnapshot(m.runtime), true
	case askUserMsg:
		m.askState = newAskUserState(askUserRequest(msg))
		m.textarea.Blur()
		summary := "等待用户补充关键信息"
		if len(m.askState.request.questions) > 0 && m.askState.request.questions[0].Header == "长线更新审批" {
			summary = "等待用户批准 compass.long 更新（30 分钟内未批准将自动拒绝）"
		}
		m.applyEvent(host.Event{
			Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: "info",
		})
		m.refreshEventViewport()
		return m, listenAskUser(m.askBridge), true
	case askUserCancelMsg:
		cancellation := askUserCancellation(msg)
		if m.askState != nil && m.askState.request.id == cancellation.id {
			m.askState = nil
			summary := "用户交互已取消"
			level := "info"
			if cancellation.timedOut {
				summary = "用户审批已超时，已拒绝 compass.long 更新并继续"
				level = "warn"
			}
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "SYSTEM", Summary: summary, Level: level,
			})
			m.refreshEventViewport()
			return m, tea.Batch(listenAskUser(m.askBridge), m.textarea.Focus()), true
		}
		return m, listenAskUser(m.askBridge), true
	case snapshotMsg:
		m.applySnapshot(host.UISnapshot(msg))
		// fetchSnapshot is an immediate, one-shot refresh used by lifecycle and
		// command results. It must not create another recurring timer chain.
		return m, m.scheduleAnimationTicks(), true
	case snapshotTickMsg:
		m.applySnapshot(host.UISnapshot(msg))
		// Only the periodic tick owns renewal. Init starts exactly one chain;
		// separating this message from snapshotMsg prevents every one-shot
		// refresh from permanently multiplying timers.
		idle := !m.snapshot.IsRunning && !m.starting
		return m, tea.Batch(tickSnapshot(m.runtime, idle), m.scheduleAnimationTicks()), true
	case idleWritingTickMsg:
		next, cmd := m.handleIdleWritingTick(time.Time(msg))
		return next, cmd, true
	case scheduleReconcileMsg:
		if msg.err != nil {
			m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "闲时调度检查失败：" + msg.err.Error(), Level: "error"})
			m.refreshEventViewport()
			return m, fetchSnapshot(m.runtime), true
		}
		if msg.result.Started {
			m.idleWritingStartPending = false
			m.idleWritingActive = false
			m.err = nil
			m.mode = modeRunning
		}
		if msg.result.PauseRequested {
			m.idleWritingActive = false
			m.idleWritingStartPending = false
			m.abortPending = true
			m.textarea.Placeholder = "已请求高峰自动暂停，将在当前任务安全边界生效..."
		}
		return m, fetchSnapshot(m.runtime), true
	case idleWritingStartResultMsg:
		m.idleWritingStartPending = false
		if msg.err != nil {
			m.idleWritingActive = false
			m.applyEvent(host.Event{Time: time.Now(), Category: "ERROR", Summary: "闲时写作启动失败：" + msg.err.Error(), Level: "error"})
			m.refreshEventViewport()
			return m, fetchSnapshot(m.runtime), true
		}
		if !msg.started {
			return m, fetchSnapshot(m.runtime), true
		}
		m.idleWritingActive = true
		m.err = nil
		m.mode = modeRunning
		m.textarea.Placeholder = defaultSteerPlaceholder()
		m.refreshEventViewport()
		return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus()), true
	case idleWritingPauseResultMsg:
		m.idleWritingActive = false
		if msg.stopped {
			m.abortPending = true
			m.textarea.Placeholder = "正在暂停创作..."
		}
		return m, fetchSnapshot(m.runtime), true
	case doneMsg:
		m.idleWritingStartPending = false
		// 每次运行结束都释放 TUI 的显示缓存；是否自动接力由 Host 持久化
		// RunControl/ResumePermit 裁定，TUI 不再根据文案猜测停止原因。
		m.idleWritingActive = false
		m.finalizeStaleEngineEvents(time.Now())
		// 运行边界全量校准计数缓存：finalize 后通常归零；ask 弹窗打开时
		// 挂起的 DECISION 合法保持 running，不能盲目清零（否则该行 spinner
		// 停止动画，与逐行扫描的旧行为不一致）。
		m.recountRunningEvents()
		m.snapshot.LastStopReason = msg.stopReason
		m.snapshot.IsRunning = false
		m.refreshEventViewport()
		m.refreshStreamViewport()
		m.refreshStateViewport()
		if msg.complete {
			m.abortPending = false
			m.mode = modeDone
			// 完成态不锁输入框：停止自动续写，但用户仍可输入返工要求（modeDone 输入经
			// Continue 唤醒新一轮 run，Arbiter 裁定返工或继续创作；/export、/model
			// 等命令也需可用，输入框必须保持聚焦（issue #27、#38）。
			m.textarea.Placeholder = "创作已完成 · 可输入返工要求(如\"重写第3章\")、/export 导出，或输入 / 看命令"
			return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime), m.textarea.Focus()), true
		}
		if m.abortPending {
			m.abortPending = false
			m.snapshot.RuntimeState = "paused"
			m.syncRuntimePlaceholder()
		} else {
			m.textarea.Placeholder = "运行中断，输入任意内容恢复创作"
		}
		return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime)), true
	case abortResultMsg:
		if msg.stopped {
			m.abortPending = true
			m.textarea.Placeholder = "正在暂停创作..."
		}
		return m, nil, true
	case reportLoadedMsg:
		if m.report == nil || msg.reqID != m.report.reqID {
			return m, nil, true
		}
		boxW, _ := reportModalSize(m.width, m.height)
		m.report.load(msg.report, paddedModalContentWidth(boxW), msg.exportPath, msg.finishedAt)
		return m, nil, true
	case importEventMsg:
		if m.importer == nil || msg.reqID != m.importer.reqID {
			return m, nil, true
		}
		boxW, _ := reportModalSize(m.width, m.height)
		m.importer.appendEvent(msg.ev, paddedModalContentWidth(boxW))
		if msg.ev.Stage == imp.StageError {
			return m, nil, true
		}
		if msg.ev.Stage == imp.StageDone {
			// 导入成功 → 自动接力续写：Resume 会启用 Router 并派发首条指令，
			// 走与"重开项目恢复"完全一致的续写流程（补上同会话导入→续写的衔接）。
			// 随后的 bootstrapMsg 处理会 enterRunning() 切到创作态。
			return m, bootstrapRuntime(m.runtime), true
		}
		return m, listenImportEvent(msg.reqID, msg.ch), true
	case simEventMsg:
		if m.simulator == nil || msg.reqID != m.simulator.reqID {
			return m, nil, true
		}
		boxW, _ := reportModalSize(m.width, m.height)
		m.simulator.appendEvent(msg.ev, paddedModalContentWidth(boxW))
		if msg.terminal() {
			return m, nil, true
		}
		return m, listenSimulationEvent(msg.reqID, msg.ch), true
	case exportDoneMsg:
		if msg.err != nil {
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "ERROR", Summary: "导出失败：" + msg.err.Error(), Level: "error",
			})
		} else if msg.result != nil {
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "SYSTEM", Summary: formatExportSuccess(msg.result), Level: "success",
			})
		}
		m.refreshEventViewport()
		return m, nil, true
	case restoreResultMsg:
		m.applyRestoreResult(msg)
		m.refreshEventViewport()
		return m, fetchSnapshot(m.runtime), true
	case startResultMsg:
		next, cmd := m.handleStartResultMsg(msg)
		return next, cmd, true
	case cocreateDeltaMsg:
		if m.cocreate == nil || msg.reqID != m.cocreate.reqID {
			return m, nil, true
		}
		m.cocreate.applyDelta(msg.kind, msg.text)
		return m, listenCoCreateDelta(m.cocreate), true
	case cocreateDoneMsg:
		next, cmd := m.handleCoCreateDoneMsg(msg)
		return next, cmd, true
	case steerResultMsg:
		return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime)), true
	case continueResultMsg:
		if msg.err != nil {
			m.err = msg.err
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "ERROR", Summary: msg.err.Error(), Level: "error",
			})
			m.refreshEventViewport()
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus()), true
		}
		m.err = nil
		m.textarea.Placeholder = defaultSteerPlaceholder()
		return m, tea.Batch(fetchSnapshot(m.runtime), listenDone(m.runtime), m.textarea.Focus()), true
	case spinnerTickMsg:
		m.spinnerPending = false
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		if m.snapshot.IsRunning || m.starting {
			// 星星 / 顶栏 spinner 的视觉刷新都走这里（350ms）
			m.refreshEventViewport()
			m.spinnerPending = true
			return m, tickSpinner(), true
		}
		return m, nil, true
	case toolSpinnerTickMsg:
		m.toolSpinnerPending = false
		m.toolSpinnerIdx = (m.toolSpinnerIdx + 1) % len(toolSpinnerFrames)
		// 事件流"进行中"行的 spinner 刷新（350ms，与主 spinner 同频，见
		// toolSpinnerTickInterval）。Arbiter 可在 Engine 停机态处理 Continue/查询，
		// 因此不能用 snapshot.IsRunning 作为动画前提；只要存在调用类 running
		// 事件就刷新。没有时跳过全量重渲。
		if m.hasRunningEvent() {
			m.refreshEventViewport()
			m.toolSpinnerPending = true
			return m, tickToolSpinner(), true
		}
		return m, nil, true
	case cursorTickMsg:
		m.cursorPending = false
		m.cursorIdx++
		if m.snapshot.IsRunning {
			// cursor 闪烁需要全量重渲流式面板（光标位于 content 末尾）。
			m.refreshStreamViewport()
			m.cursorPending = true
			return m, tickCursor(), true
		}
		return m, nil, true
	case streamBatchMsg:
		changed := false
		for _, op := range msg.ops {
			if op.clear {
				m.appendStreamClear()
				changed = true
				continue
			}
			if m.appendStreamText(op.text) {
				changed = true
			}
		}
		if changed {
			m.trimStreamRounds()
			m.refreshStreamViewport()
			if m.streamScroll {
				m.streamVP.GotoBottom()
			}
		}
		if msg.closed {
			return m, nil, true
		}
		return m, m.nextStreamListen(), true
	case streamDeltaMsg:
		if m.appendStreamText(string(msg)) {
			m.trimStreamRounds()
			m.refreshStreamViewport()
			if m.streamScroll {
				m.streamVP.GotoBottom()
			}
		}
		return m, m.nextStreamListen(), true
	case streamClearMsg:
		m.appendStreamClear()
		m.trimStreamRounds()
		m.refreshStreamViewport()
		if m.streamScroll {
			m.streamVP.GotoBottom()
		}
		return m, m.nextStreamListen(), true
	case quitResetMsg:
		m.quitPending = false
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m Model) handleStartResultMsg(msg startResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		wasStarting := m.starting
		m.starting = false
		if m.mode != modeNew {
			m.applyEvent(host.Event{
				Time: time.Now(), Category: "ERROR", Summary: msg.err.Error(), Level: "error",
			})
			m.refreshEventViewport()
		}
		if m.cocreate != nil {
			m.cocreate.awaiting = false
			m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus())
		}
		if wasStarting {
			// 回车后已经进入工作台；启动阶段的 LLM 错误就在当前工作台展示，
			// 不再退回欢迎页。
			m.mode = modeRunning
			m.snapshot.IsRunning = false
			m.snapshot.RuntimeState = "idle"
			m.textarea.Placeholder = "启动失败，请检查模型配置或使用 /model 切换模型"
			m.refreshStreamViewport()
			m.refreshStateViewport()
			return m, m.textarea.Focus()
		}
		if m.mode == modeNew {
			m.textarea.Placeholder = placeholderForNewMode(m.startupMode)
			return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus())
		}
		return m, fetchSnapshot(m.runtime)
	}
	m.starting = false

	if m.mode == modeNew {
		m.cocreate = nil
		enableMouse := m.enterRunning()
		m.resizeTextarea()
		m.textarea.Placeholder = defaultSteerPlaceholder()
		return m, tea.Batch(fetchSnapshot(m.runtime), m.textarea.Focus(), enableMouse)
	}

	return m, fetchSnapshot(m.runtime)
}

func (m *Model) enterStarting(rawPrompt string) tea.Cmd {
	m.cocreate = nil
	m.err = nil
	m.starting = true
	m.snapshot.IsRunning = true
	m.snapshot.RuntimeState = "running"
	enableMouse := m.enterRunning()
	m.resetOutputPanels()
	m.resizeTextarea()
	m.textarea.Placeholder = "正在初始化创作..."
	m.applyStartupPromptEvent(rawPrompt)
	m.applyEvent(host.Event{
		Time: time.Now(), Category: "SYSTEM", Summary: "正在初始化创作", Level: "info",
	})
	m.refreshEventViewport()
	m.refreshStreamViewport()
	m.refreshStateViewport()
	return tea.Batch(m.textarea.Focus(), enableMouse, m.scheduleAnimationTicks())
}

func (m *Model) applyStartupPromptEvent(rawPrompt string) {
	text := utils.CleanInputLine(rawPrompt)
	if text == "" {
		return
	}
	m.applyEvent(host.Event{
		Time:     time.Now(),
		Category: "USER",
		Summary:  "创作需求: " + truncate(text, maxPromptEventCols),
		Detail:   text,
		Level:    "info",
	})
}

func (m Model) handleCoCreateDoneMsg(msg cocreateDoneMsg) (tea.Model, tea.Cmd) {
	if m.cocreate == nil || msg.reqID != m.cocreate.reqID {
		return m, nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.cocreate.awaiting = false
		m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
		return m, m.textarea.Focus()
	}
	m.err = nil
	m.cocreate.apply(msg.reply)
	m.textarea.Placeholder = placeholderForCoCreate(m.cocreate)
	return m, m.textarea.Focus()
}

func (m *Model) applySnapshot(next host.UISnapshot) {
	prev := m.snapshot
	m.snapshot = next
	m.syncRuntimePlaceholder()
	if m.mode == modeNew {
		return
	}
	runningChanged := prev.IsRunning != next.IsRunning || m.starting
	if runningChanged {
		m.refreshEventViewport()
		m.refreshStreamViewport()
		m.refreshDetailViewport()
		m.refreshStateViewport()
		return
	}
	if snapshotStateChanged(prev, next) {
		m.refreshEventViewport()
		m.refreshStateViewport()
	}
	if snapshotDetailChanged(prev, next) {
		m.refreshDetailViewport()
	}
}

func snapshotStateChanged(prev, next host.UISnapshot) bool {
	return prev.RuntimeState != next.RuntimeState ||
		prev.CompletedCount != next.CompletedCount ||
		prev.TotalWordCount != next.TotalWordCount ||
		prev.InProgressChapter != next.InProgressChapter ||
		prev.StatusLabel != next.StatusLabel ||
		prev.TotalCostUSD != next.TotalCostUSD ||
		prev.TotalInputTokens != next.TotalInputTokens ||
		prev.TotalOutputTokens != next.TotalOutputTokens ||
		prev.AutoResumePending != next.AutoResumePending ||
		prev.IdleWritingInPeak != next.IdleWritingInPeak ||
		prev.RecoveryLabel != next.RecoveryLabel ||
		agentContextChanged(prev.Agents, next.Agents)
}

func agentContextChanged(prev, next []host.AgentSnapshot) bool {
	if len(prev) != len(next) {
		return true
	}
	for i := range next {
		pc, nc := prev[i].Context, next[i].Context
		if prev[i].Name != next[i].Name ||
			prev[i].State != next[i].State ||
			prev[i].Tool != next[i].Tool ||
			pc.Tokens != nc.Tokens ||
			int(pc.Percent) != int(nc.Percent) ||
			pc.Strategy != nc.Strategy ||
			pc.Scope != nc.Scope ||
			pc.LastChanged != nc.LastChanged {
			return true
		}
	}
	return false
}

func snapshotDetailChanged(prev, next host.UISnapshot) bool {
	if prev.CurrentVolumeArc != next.CurrentVolumeArc ||
		prev.InProgressChapter != next.InProgressChapter ||
		prev.CompletedCount != next.CompletedCount ||
		prev.OutlinePlanned != next.OutlinePlanned ||
		len(prev.Outline) != len(next.Outline) {
		return true
	}
	if len(next.Outline) == 0 {
		return false
	}
	a := prev.Outline[0]
	b := next.Outline[0]
	if a.Chapter != b.Chapter || a.Title != b.Title {
		return true
	}
	pa := prev.Outline[len(prev.Outline)-1]
	pb := next.Outline[len(next.Outline)-1]
	return pa.Chapter != pb.Chapter || pa.Title != pb.Title
}

func (m Model) handleTextareaMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.refitTextareaHeight()
	m.updateCommandPalette()
	return m, cmd
}

// applyEvent 把一条事件应用到 m.events：
// - 带 ID 且已存在 → 原地更新（合并完成态字段，保留首次的 Time / Summary）
// - 新事件 → 追加，必要时记录到 eventIndex
// - 超过 maxEvents 时做滑动截断并重建索引
// 全程同步维护 runningEventCount（hasRunningEvent 的 O(1) 缓存）。
func (m *Model) applyEvent(ev host.Event) {
	if ev.ID != "" {
		if idx, ok := m.eventIndex[ev.ID]; ok && idx >= 0 && idx < len(m.events) {
			existing := &m.events[idx]
			// 合并只会把 running 推向终态（FinishedAt/Discarded/Failed 只置位），
			// 不可能反向复活，因此计数只减不增。
			wasRunning := existing.Running()
			if !ev.FinishedAt.IsZero() {
				existing.FinishedAt = ev.FinishedAt
			}
			if ev.Duration > 0 {
				existing.Duration = ev.Duration
			}
			if ev.Failed {
				existing.Failed = true
			}
			if ev.Discarded {
				existing.Discarded = true
			}
			if ev.Level != "" {
				existing.Level = ev.Level
			}
			// Summary 非空时允许覆盖（结束态可能带补充信息）；否则保留首次
			if ev.Summary != "" {
				existing.Summary = ev.Summary
			}
			// Category 允许 finish 事件覆盖（TOOL → REVIEW/CHECK 归类迁移），
			// 否则 fix-3 B5 的 finish Category 替换对 TUI 不可见。
			if ev.Category != "" && ev.Category != existing.Category {
				existing.Category = ev.Category
			}
			if wasRunning && !existing.Running() {
				m.runningEventCount--
			}
			return
		}
	}

	m.events = append(m.events, ev)
	if ev.ID != "" {
		m.eventIndex[ev.ID] = len(m.events) - 1
	}
	if ev.Running() {
		m.runningEventCount++
	}
	if len(m.events) > maxEvents {
		drop := len(m.events) - maxEvents
		// 截断把头部事件挤出窗口：被挤出的 running 行要同步减计数，
		// 否则孤儿行删除后缓存永远大于实际。
		for _, dropped := range m.events[:drop] {
			if dropped.Running() {
				m.runningEventCount--
			}
		}
		m.events = m.events[drop:]
		m.rebuildEventIndex()
	}
	if m.runningEventCount < 0 {
		m.runningEventCount = 0
	}
}

// appendStreamText adds one coalesced stream chunk without copying the round.
func (m *Model) appendStreamText(text string) bool {
	if text == "" {
		return false
	}
	if len(m.streamRounds) == 0 {
		m.streamRounds = append(m.streamRounds, streamRound{})
	}
	m.streamRounds[len(m.streamRounds)-1].append(text)
	return true
}

func (m *Model) appendStreamClear() {
	if len(m.streamRounds) == 0 {
		m.streamRounds = append(m.streamRounds, streamRound{})
		return
	}
	if !m.streamRounds[len(m.streamRounds)-1].empty() {
		m.streamRounds = append(m.streamRounds, streamRound{})
	}
}

func (m Model) nextStreamListen() tea.Cmd {
	if m.runtime == nil {
		return nil
	}
	return listenStream(m.runtime)
}

// trimStreamRounds 把 streamRounds 截断到 maxStreamRounds 段；超出从头丢弃。
// 调用时机：每次 streamClear 新开轮次后、replay 灌完所有历史项后。
func (m *Model) trimStreamRounds() {
	if len(m.streamRounds) <= maxStreamRounds {
		return
	}
	drop := len(m.streamRounds) - maxStreamRounds
	m.streamRounds = m.streamRounds[drop:]
}

func (m *Model) rebuildEventIndex() {
	m.eventIndex = make(map[string]int, len(m.events))
	for i, e := range m.events {
		if e.ID != "" {
			m.eventIndex[e.ID] = i
		}
	}
}

func (m *Model) resetOutputPanels() {
	m.events = nil
	m.eventIndex = make(map[string]int)
	m.runningEventCount = 0
	m.viewport.SetContent("")
	m.viewport.GotoTop()
	m.streamRounds = nil
	m.streamVP.SetContent("")
	m.streamVP.GotoTop()
}

func (m *Model) applyRuntimeReplay(items []domain.RuntimeQueueItem) {
	for _, item := range items {
		switch item.Kind {
		case domain.RuntimeQueueUIEvent:
			// 事件流不做回放：队列里只有完成态事件，且 Agent/Depth/Duration/Level
			// 等渲染所需字段未随 replay 还原，出来的行残缺不齐。宁可空面板也不要半截数据。
			continue
		case domain.RuntimeQueueStreamClear:
			m.appendStreamClear()
		case domain.RuntimeQueueStreamDelta:
			m.appendStreamText(host.ReplayDeltaText(item))
		}
	}
	m.trimStreamRounds()
	m.refreshEventViewport()
	m.refreshStreamViewport()
}
