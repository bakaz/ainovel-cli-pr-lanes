package host

import (
	"context"
	"fmt"
	"strings"
	"time"

	"encoding/json"
	"log/slog"

	"github.com/voocel/agentcore"
)

func retryPrefix(attempt, maxRetries int, delay time.Duration, explicitZero bool) string {
	if text := formatRetryDelay(delay, explicitZero); text != "" {
		return fmt.Sprintf("重试 (%d/%d，%s后): ", attempt, maxRetries, text)
	}
	return fmt.Sprintf("重试 (%d/%d): ", attempt, maxRetries)
}

// formatRetryDelay 格式化等待时长文字。
// explicitZero 为 true 表示上游明确设置了 0 delay（即时重试），显示"即时"而非空字符串。
func formatRetryDelay(delay time.Duration, explicitZero bool) string {
	if delay < 0 {
		return ""
	}
	if delay == 0 {
		if explicitZero {
			return "即时"
		}
		return ""
	}
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return (time.Duration(seconds) * time.Second).String()
}

func (o *observer) handleThinkingProgress(ev agentcore.Event) {
	agent := ev.Progress.Agent
	thinking := ev.Progress.Thinking
	if agent == "" || thinking == "" {
		return
	}

	o.mapMu.Lock()
	prev := o.lastThinkingByAgent[agent]
	var delta string
	if strings.HasPrefix(thinking, prev) {
		delta = thinking[len(prev):]
	} else {
		delta = thinking
	}
	o.lastThinkingByAgent[agent] = thinking
	o.mapMu.Unlock()
	if delta == "" {
		return
	}
	o.emitStreamDelta(delta, true)
}

func (o *observer) handleContextProgress(ev agentcore.Event) {
	if ev.Progress == nil || len(ev.Progress.Meta) == 0 {
		return
	}
	var payload struct {
		Tokens        int     `json:"tokens"`
		ContextWindow int     `json:"context_window"`
		Percent       float64 `json:"percent"`
		Scope         string  `json:"scope"`
		Strategy      string  `json:"strategy"`
		LastChanged   bool    `json:"last_changed"`
	}
	if json.Unmarshal(ev.Progress.Meta, &payload) != nil {
		return
	}

	agent := ev.Progress.Agent
	if agent == "" {
		return
	}

	// 更新 agent 快照（TUI 侧边栏始终可见）
	var prevPercent float64
	o.updateAgent(agent, func(a *agentState) {
		prevPercent = a.lastLogPercent
		snap := AgentContextSnapshot{
			Tokens:         payload.Tokens,
			ContextWindow:  payload.ContextWindow,
			Percent:        payload.Percent,
			Scope:          payload.Scope,
			Strategy:       payload.Strategy,
			LastChanged:    payload.LastChanged,
			ActiveMessages: a.context.ActiveMessages,
		}
		if payload.LastChanged {
			snap.StrategyAt = time.Now()
		} else {
			snap.StrategyAt = a.context.StrategyAt
			if snap.Strategy == "" {
				snap.Strategy = a.context.Strategy
			}
		}
		a.context = snap
	})

	level := "info"
	if payload.Percent > 85 {
		level = "warn"
	}
	summary := fmt.Sprintf("%s 上下文 %.0f%% (%d/%d) 策略: %s", agent, payload.Percent, payload.Tokens, payload.ContextWindow, payload.Strategy)

	if payload.LastChanged && contextStrategyNotifiesTUI(payload.Strategy) {
		ctxEv := Event{Time: time.Now(), Category: "SYSTEM", Agent: agent, Summary: summary, Level: level, Depth: 1}
		o.emitEv(ctxEv)
		o.persistEvent(ctxEv)
		o.updateAgent(agent, func(a *agentState) { a.lastLogPercent = payload.Percent })
		return
	}

	crossed := contextPercentCrossed(prevPercent, payload.Percent)
	jumped := absFloat(payload.Percent-prevPercent) >= 5
	if !crossed && !jumped {
		return
	}
	o.updateAgent(agent, func(a *agentState) { a.lastLogPercent = payload.Percent })
	slogLevel := slog.LevelInfo
	if level == "warn" {
		slogLevel = slog.LevelWarn
	}
	slog.Log(context.Background(), slogLevel, summary, "module", "context", "agent", agent)
}

func contextPercentCrossed(prev, next float64) bool {
	for _, line := range []float64{70, 85} {
		if (prev < line && next >= line) || (prev >= line && next < line) {
			return true
		}
	}
	return false
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// contextStrategyNotifiesTUI 只有真正改写了上下文窗口的策略才进事件流。
// 工具结果微压缩每轮都可能发生，侧栏用 LastChanged 亮一次即可，不能当 SYSTEM 通知刷屏。
func contextStrategyNotifiesTUI(strategy string) bool {
	switch strategy {
	case "", "tool_result_microcompact":
		return false
	default:
		return true
	}
}
