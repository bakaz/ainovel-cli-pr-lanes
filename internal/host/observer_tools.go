package host

import (
	"fmt"
	"strings"
	"time"

	"encoding/json"
	"github.com/voocel/agentcore"
)

// handleToolUpdate 处理 Worker 的进度中继(ProgressPayload):TOOL 行、流式正文、
// thinking、retry、context。Engine 经 observer.workerProgress 喂入。
//
// 并发安全：此函数可能从 engine goroutine 与 Arbiter goroutine 两处进入
// （Arbiter 的 generateWithRetry 经 ctx ToolProgress 回调进入），因此所有
// map 访问必须持 mapMu。channel 输出（emitEv/emitD/emitC/persistEvent）在
// 解锁后发送，避免持锁期间发生 channel 阻塞反向依赖。
func (o *observer) handleToolUpdate(ev agentcore.Event) {
	if ev.Progress == nil {
		return
	}
	switch ev.Progress.Kind {
	case agentcore.ProgressToolDelta:
		if ev.Progress.Delta != "" {
			o.handleSubagentDelta(ev.Progress)
		}
	case agentcore.ProgressToolStart:
		// Worker 内部的工具调用（如 writer → draft_chapter）。
		// 注意：TOOL 行可能已经在流式识别阶段被 handleSubagentDelta 提前发出。
		// 此处：若已发 → 只更新 summary（args 此时完整，能显示 "tool(第N章)"）；否则正常发。
		if ev.Progress.Agent == "" || ev.Progress.Tool == "" {
			break
		}
		toolName := displayToolName(ev.Progress.Tool, ev.Progress.Args)
		o.mapMu.Lock()
		_, alreadyStarted := o.toolStarts[ev.Progress.Agent]
		if alreadyStarted {
			o.mapMu.Unlock()
			o.updateToolCallSummary(ev.Progress.Agent, ev.Progress.Tool, toolName)
			o.updateAgent(ev.Progress.Agent, func(a *agentState) {
				a.state = "working"
				a.tool = ev.Progress.Tool
				a.summary = fmt.Sprintf("%s → %s", ev.Progress.Agent, toolName)
			})
			break
		}
		// 未提前发过 → 正常流程
		// （非流式 tool args 的模型不会触发 ensureSubagentToolStarted，
		// fallback header 必须在这条路径上补一次，否则 read_chapter 这类
		// 无 extractor 的工具流式面板上就没有 ✻ 头部，紧贴前面思考一段。）
		id := nextEventID()
		o.toolStarts[ev.Progress.Agent] = &activeCall{id: id, start: time.Now(), summary: toolName, depth: 1}
		o.streamOwner = ev.Progress.Agent
		o.mapMu.Unlock()
		o.emitAndLog(Event{
			ID:       id,
			Time:     time.Now(),
			Category: "TOOL",
			Agent:    ev.Progress.Agent,
			Summary:  toolName,
			Level:    "info",
			Depth:    1,
		})
		o.updateAgent(ev.Progress.Agent, func(a *agentState) {
			a.state = "working"
			a.tool = ev.Progress.Tool
			a.summary = fmt.Sprintf("%s → %s", ev.Progress.Agent, toolName)
		})
		o.emitFallbackStreamHeader(ev.Progress.Tool)
	case agentcore.ProgressToolEnd:
		o.mapMu.Lock()
		delete(o.streamExtractors, ev.Progress.Agent)
		o.mapMu.Unlock()
		if ev.Progress.Agent == "" {
			return
		}
		o.mapMu.Lock()
		call, ok := o.toolStarts[ev.Progress.Agent]
		if !ok {
			o.mapMu.Unlock()
			return
		}
		delete(o.toolStarts, ev.Progress.Agent)
		o.mapMu.Unlock()
		// 同 ID 更新事件：TUI 按 ID 定位原 TOOL 行，回填 FinishedAt / Duration。
		// Summary / Depth 也带上，保证 runtime queue replay 时能还原完整行。
		finishEv := Event{
			ID:         call.id,
			Time:       call.start,
			FinishedAt: time.Now(),
			Category:   "TOOL",
			Agent:      ev.Progress.Agent,
			Summary:    call.summary,
			Level:      "info",
			Depth:      call.depth,
			Duration:   time.Since(call.start),
		}
		o.emitEv(finishEv)
		o.persistEvent(finishEv)
	case agentcore.ProgressThinking:
		o.handleThinkingProgress(ev)
	case agentcore.ProgressRetry:
		agent := ev.Progress.Agent

		// ── EventRetry 契约：当前未提交 model attempt 已丢弃 ──
		//
		// 1) 把该 agent 进行中的 TOOL 行以同 Event.ID 更新为终态 discarded
		//   （不是执行失败，只是模型输出被放弃）
		// 2) 清空该 agent 的流式残留：toolStarts、streamExtractors、
		//    streamArgPrefixes/labels、thinking、stream text round
		// 3) 仅当 retry agent 是当前 stream round 的 owner 时才 CLEAR/reset
		//    全局 stream 状态，避免 Arbiter retry 截断 Writer 正在输出的流式内容
		// 4) 清理 agent 侧栏状态

		o.mapMu.Lock()

		// ── agent 级清理（持锁执行） ──
		var discardID string
		var discardStart time.Time
		var discardSummary string
		var discardDepth int
		if call, ok := o.toolStarts[agent]; ok {
			discardID = call.id
			discardStart = call.start
			discardSummary = call.summary
			discardDepth = call.depth
			delete(o.toolStarts, agent)
		}
		delete(o.lastThinkingByAgent, agent)
		delete(o.streamExtractors, agent)
		for key := range o.streamArgPrefixes {
			if strings.HasPrefix(key, agent+"\x00") {
				delete(o.streamArgPrefixes, key)
			}
		}
		for key := range o.streamArgLabels {
			if strings.HasPrefix(key, agent+"\x00") {
				delete(o.streamArgLabels, key)
			}
		}

		// ── 全局 stream CLEAR：只有 retry agent 是 stream owner 才执行 ──
		mayClear := o.streamOwner == agent
		o.mapMu.Unlock()

		// 发射丢弃事件（持锁完成后释放，不在锁内做 channel 操作）
		if discardID != "" {
			discardEv := Event{
				ID:         discardID,
				Time:       discardStart,
				FinishedAt: time.Now(),
				Discarded:  true,
				Category:   "TOOL",
				Agent:      agent,
				Summary:    discardSummary,
				Level:      "info",
				Depth:      discardDepth,
				Duration:   time.Since(discardStart),
			}
			o.emitEv(discardEv)
			o.persistEvent(discardEv)
		}

		// 清理 agent 侧栏状态（把 agent 标记为空闲）
		o.updateAgent(agent, func(a *agentState) {
			a.state = "idle"
			a.tool = ""
		})

		// 仅 owner 发 CLEAR 重置全局 stream 状态
		if mayClear {
			o.emitC()
			o.mapMu.Lock()
			o.streamHasContent = false
			o.streamLastByte = 0
			o.streamThinking = false
			o.streamOwner = ""
			o.mapMu.Unlock()
		}

		// ── 原有的 retry SYSTEM 行发射逻辑 ──
		// Arbiter 在 Meta 里保留实际 Retry-After；旧 Worker relay 尚未携带 Delay，
		// 对它按 agentcore 的标准指数退避还原展示值。
		delay, explicitZero := retryProgressDelay(ev.Progress)
		prefix := retryPrefix(ev.Progress.Attempt, ev.Progress.MaxRetries, delay, explicitZero)
		retryEv := Event{
			ID:       o.retryEventID(agent, ev.Progress.Attempt),
			Time:     time.Now(),
			Category: "SYSTEM",
			Agent:    agent,
			Summary:  prefix + truncate(ev.Progress.Message, 80),
			Detail:   prefix + ev.Progress.Message,
			Kind:     errorKind(nil, ev.Progress.Message),
			Level:    "warn",
			Depth:    1,
		}
		o.emitEv(retryEv)
		o.persistEvent(retryEv)
	case agentcore.ProgressToolError:
		o.mapMu.Lock()
		delete(o.streamExtractors, ev.Progress.Agent)
		call, hasTool := o.toolStarts[ev.Progress.Agent]
		if hasTool {
			delete(o.toolStarts, ev.Progress.Agent)
		}
		o.mapMu.Unlock()
		msg := ev.Progress.Message
		if msg == "" {
			msg = "unknown error"
		}
		// 如果有进行中的 TOOL 行，原地标记为失败；否则独立追加 ERROR 行。
		if hasTool {
			finishEv := Event{
				ID:         call.id,
				Time:       call.start,
				FinishedAt: time.Now(),
				Failed:     true,
				Category:   "TOOL",
				Agent:      ev.Progress.Agent,
				Summary:    call.summary,
				Level:      "error",
				Depth:      call.depth,
				Duration:   time.Since(call.start),
			}
			o.emitEv(finishEv)
			o.persistEvent(finishEv)
		}
		// 附加 ERROR 详情行（补充错误信息，便于排查）
		errEv := Event{
			Time:     time.Now(),
			Category: "ERROR",
			Agent:    ev.Progress.Agent,
			Summary:  fmt.Sprintf("%s 错误: %s", ev.Progress.Tool, truncate(msg, 100)),
			Detail:   fmt.Sprintf("%s 错误: %s", ev.Progress.Tool, msg),
			Kind:     errorKind(nil, msg),
			Level:    "error",
			Depth:    1,
		}
		o.emitEv(errEv)
		o.persistEvent(errEv)
	case agentcore.ProgressContext:
		o.handleContextProgress(ev)
	}
}

// retryProgressDelay 解析 retry 事件中的等待时长。
// 返回 (delay, explicitZero)：
//   - 当 payload.Meta 中有 "retry_delay_ms" 字段时，explicitZero=true 且 delay 为该字段值（允许 0）；
//   - 无该字段时 explicitZero=false，按 attempt 指数退避（首 attempt 返回 1s）。
//
// explicitZero 由调用方用于 UI 展示：上游显式声明 0 delay 时显示"即时重试"而非 fallback 1s。
func retryProgressDelay(p *agentcore.ProgressPayload) (delay time.Duration, explicitZero bool) {
	if p == nil {
		return 0, false
	}
	if len(p.Meta) > 0 {
		var raw map[string]json.RawMessage
		if json.Unmarshal(p.Meta, &raw) == nil {
			if rawMS, ok := raw["retry_delay_ms"]; ok {
				var ms int64
				if json.Unmarshal(rawMS, &ms) == nil {
					return time.Duration(ms) * time.Millisecond, true
				}
			}
		}
	}
	return retryFallbackDelay(p.Attempt), false
}

// retryFallbackDelay 当上游未携带 retry_delay_ms 时，按 attempt 做指数退避。
func retryFallbackDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	delay := time.Second
	for i := 1; i < attempt && delay < 60*time.Second; i++ {
		delay *= 2
	}
	if delay > 60*time.Second {
		return 60 * time.Second
	}
	return delay
}

func dispatchSummary(agent, task string) string {
	if agent == "" {
		agent = "subagent"
	}
	if task == "" {
		return agent
	}
	firstLine := strings.TrimSpace(strings.SplitN(task, "\n", 2)[0])
	if firstLine == "" {
		return agent
	}
	return agent + "（" + truncate(firstLine, 30) + "）"
}

func (o *observer) updateToolCallSummary(agent, tool, summary string) {
	if agent == "" || summary == "" {
		return
	}
	o.mapMu.Lock()
	call, ok := o.toolStarts[agent]
	if !ok || call.summary == summary {
		o.mapMu.Unlock()
		return
	}
	call.summary = summary
	o.mapMu.Unlock()
	o.emitEv(Event{
		ID:       call.id,
		Time:     call.start,
		Category: "TOOL",
		Agent:    agent,
		Summary:  summary,
		Level:    "info",
		Depth:    call.depth,
	})
	o.updateAgent(agent, func(a *agentState) {
		a.state = "working"
		a.tool = tool
		a.summary = fmt.Sprintf("%s → %s", agent, summary)
	})
}

func (o *observer) updateToolCallSummaryFromDelta(agent, tool, delta string) {
	o.mapMu.Lock()
	key := streamArgKey(agent, tool)
	prefix := o.streamArgPrefixes[key] + delta
	if len(prefix) > 512 {
		prefix = prefix[:512]
	}
	o.streamArgPrefixes[key] = prefix

	summary := streamedToolLabel(tool, prefix)
	if summary == "" {
		o.mapMu.Unlock()
		return
	}
	if o.streamArgLabels[key] == summary {
		o.mapMu.Unlock()
		return
	}
	o.streamArgLabels[key] = summary
	o.mapMu.Unlock()
	o.updateToolCallSummary(agent, tool, summary)
}

func streamArgKey(agent, tool string) string {
	return agent + "\x00" + tool
}

func streamedToolLabel(tool, delta string) string {
	if tool != "save_foundation" || delta == "" {
		return ""
	}
	typ := firstJSONStringField(delta, "type")
	if typ == "" {
		return ""
	}
	return fmt.Sprintf("%s[%s]", tool, typ)
}

func firstJSONStringField(raw, field string) string {
	needle := `"` + field + `"`
	idx := strings.Index(raw, needle)
	if idx < 0 {
		return ""
	}
	rest := raw[idx+len(needle):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	rest = strings.TrimLeft(rest[colon+1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	var value strings.Builder
	escape := false
	for i := 1; i < len(rest); i++ {
		c := rest[i]
		if escape {
			value.WriteByte(c)
			escape = false
			continue
		}
		switch c {
		case '\\':
			escape = true
		case '"':
			return value.String()
		default:
			value.WriteByte(c)
		}
	}
	return ""
}

func (o *observer) emitCallFinish(call *activeCall, category, agentName string, failed bool) {
	if call == nil {
		return
	}
	level := "success"
	if failed {
		level = "error"
	}
	finishEv := Event{
		ID:         call.id,
		Time:       call.start,
		FinishedAt: time.Now(),
		Failed:     failed,
		Category:   category,
		Agent:      agentName,
		Summary:    call.summary,
		Level:      level,
		Depth:      call.depth,
		Duration:   time.Since(call.start),
	}
	o.emitEv(finishEv)
	o.persistEvent(finishEv)
}

func displayToolName(tool string, args json.RawMessage) string {
	if len(args) == 0 {
		return tool
	}
	switch tool {
	case "save_foundation":
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(args, &p) == nil && p.Type != "" {
			return fmt.Sprintf("%s[%s]", tool, p.Type)
		}
	case "commit_chapter", "plan_chapter", "draft_chapter", "check_consistency":
		var p struct {
			Chapter int `json:"chapter"`
		}
		if json.Unmarshal(args, &p) == nil && p.Chapter > 0 {
			return fmt.Sprintf("%s(第%d章)", tool, p.Chapter)
		}
	case "save_review":
		var p struct {
			Chapter int    `json:"chapter"`
			Scope   string `json:"scope"`
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal(args, &p) == nil {
			label := ""
			switch p.Scope {
			case "arc":
				label = "本弧"
			case "global":
				label = "全局"
			default:
				if p.Chapter > 0 {
					label = fmt.Sprintf("第%d章", p.Chapter)
				}
			}
			if label == "" {
				return tool
			}
			if p.Verdict != "" {
				return fmt.Sprintf("%s(%s·%s)", tool, label, p.Verdict)
			}
			return fmt.Sprintf("%s(%s)", tool, label)
		}
	case "novel_context":
		var p struct {
			Chapter int `json:"chapter"`
		}
		if json.Unmarshal(args, &p) == nil && p.Chapter > 0 {
			return fmt.Sprintf("%s(第%d章)", tool, p.Chapter)
		}
	case "read_chapter":
		var p struct {
			Chapter   int    `json:"chapter"`
			Source    string `json:"source"`
			Character string `json:"character"`
		}
		if json.Unmarshal(args, &p) == nil && p.Chapter > 0 {
			suffix := ""
			if p.Character != "" {
				suffix = "·" + p.Character + "对话"
			} else if p.Source == "draft" {
				suffix = "·草稿"
			}
			return fmt.Sprintf("%s(第%d章%s)", tool, p.Chapter, suffix)
		}
	}
	return tool
}
