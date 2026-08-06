package agents

import (
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
)

type observedRecord struct {
	agent, task, runID string
	msg                agentcore.AgentMessage
}

// TestRunUsageObserver_BindsInstanceID 验证 observer 把 InstanceID 与 task
// 绑定到每条模型响应：
//   - task 从 run 首条 user 消息（AgentLoop 注入的任务消息）捕获；
//   - 后续 steering / recovery 注入的 user 消息不覆盖（task 非空即锁定）；
//   - EventModelResponse 携带 InstanceID 透传给 UsageRecorder。
func TestRunUsageObserver_BindsInstanceID(t *testing.T) {
	var got []observedRecord
	obs := newRunUsageObserver(func(agentName, task, runID string, msg agentcore.AgentMessage) {
		got = append(got, observedRecord{agentName, task, runID, msg})
	})

	meta3 := subagent.RunMeta{Agent: "writer", InstanceID: "writer#3", Mode: subagent.ModeSingle}
	// run 1：AgentStart → 任务 user 消息 → steering user 消息 → 模型响应。
	obs.OnEvent(meta3, agentcore.Event{Type: agentcore.EventAgentStart})
	obs.OnEvent(meta3, agentcore.Event{Type: agentcore.EventMessageStart, Message: agentcore.UserMsg("写第 1 章")})
	obs.OnEvent(meta3, agentcore.Event{Type: agentcore.EventMessageStart, Message: agentcore.UserMsg("[steering] 请继续")})
	obs.OnEvent(meta3, agentcore.Event{
		Type:    agentcore.EventModelResponse,
		Message: agentcore.Message{Role: agentcore.RoleAssistant, Usage: &agentcore.Usage{Input: 100, Output: 10}},
	})

	// run 2：同一 task 再次 spawn，InstanceID 变化（无 user 消息的防御路径）。
	meta4 := subagent.RunMeta{Agent: "writer", InstanceID: "writer#4", Mode: subagent.ModeSingle}
	obs.OnEvent(meta4, agentcore.Event{
		Type:    agentcore.EventModelResponse,
		Message: agentcore.Message{Role: agentcore.RoleAssistant, Usage: &agentcore.Usage{Input: 200, Output: 20}},
	})

	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	if got[0].agent != "writer" || got[0].task != "写第 1 章" || got[0].runID != "writer#3" {
		t.Errorf("run1 绑定异常: agent=%q task=%q runID=%q", got[0].agent, got[0].task, got[0].runID)
	}
	if m, ok := got[0].msg.(agentcore.Message); !ok || m.Usage == nil || m.Usage.Input != 100 {
		t.Errorf("run1 消息透传异常: %+v", got[0].msg)
	}
	// 新 run 未捕获任务文本时 task 必须为空（不跨 run 泄漏上一 run 的 task）。
	if got[1].runID != "writer#4" || got[1].task != "" {
		t.Errorf("run2 绑定异常: task=%q runID=%q, want task=\"\" runID=writer#4", got[1].task, got[1].runID)
	}
}

// TestRunUsageObserver_RecordsUsageNilMessages 验证无 Usage 的模型响应同样
// 透传（host 侧据此统计 missingAssistantUsage，与 OnMessage 路径的判定一致）。
func TestRunUsageObserver_RecordsUsageNilMessages(t *testing.T) {
	var n int
	obs := newRunUsageObserver(func(agentName, task, runID string, msg agentcore.AgentMessage) { n++ })
	meta := subagent.RunMeta{Agent: "editor", InstanceID: "editor#1", Mode: subagent.ModeSingle}
	obs.OnEvent(meta, agentcore.Event{Type: agentcore.EventMessageStart, Message: agentcore.UserMsg("审阅")})
	obs.OnEvent(meta, agentcore.Event{
		Type:    agentcore.EventModelResponse,
		Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock("正文")}},
	})
	if n != 1 {
		t.Fatalf("无 usage 的模型响应也应透传, got %d", n)
	}
}

// TestRunUsageObserver_IgnoresNonModelEvents 验证非模型响应事件（user/tool
// 消息、工具执行等）不透传 UsageRecorder——只有 EventModelResponse 才代表
// 一次模型调用完成。
func TestRunUsageObserver_IgnoresNonModelEvents(t *testing.T) {
	var n int
	obs := newRunUsageObserver(func(agentName, task, runID string, msg agentcore.AgentMessage) { n++ })
	meta := subagent.RunMeta{Agent: "writer", InstanceID: "writer#1", Mode: subagent.ModeSingle}
	obs.OnEvent(meta, agentcore.Event{Type: agentcore.EventAgentStart})
	obs.OnEvent(meta, agentcore.Event{Type: agentcore.EventMessageStart, Message: agentcore.UserMsg("任务")})
	obs.OnEvent(meta, agentcore.Event{Type: agentcore.EventMessageEnd, Message: agentcore.Message{Role: agentcore.RoleAssistant, Usage: &agentcore.Usage{Input: 1}}})
	obs.OnEvent(meta, agentcore.Event{Type: agentcore.EventToolExecStart, Tool: "draft_chapter"})
	if n != 0 {
		t.Fatalf("非 EventModelResponse 不应透传, got %d", n)
	}
}
