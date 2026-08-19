package agents

import (
	"context"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/llm"
	"github.com/voocel/ainovel-cli/internal/store"
)

// WithTrailingAntiRefusal 在每次 Generate/GenerateStream 的 messages 末尾
// 追加一条 RoleSystem reminder（有非空 meta/anti_refusal.md 时）。
// 缺文件或空文本时原样转发，不改 messages。包在 failover / SwappableModel 之外。
func WithTrailingAntiRefusal(inner agentcore.ChatModel, st *store.Store) agentcore.ChatModel {
	if inner == nil || st == nil || st.AntiRefusal == nil {
		return inner
	}
	m := &trailingReminderModel{inner: inner, load: st.AntiRefusal.LoadText}
	if cp, ok := inner.(llm.CapabilityProvider); ok {
		return &capabilityTrailingReminder{trailingReminderModel: m, capabilities: cp}
	}
	return m
}

type trailingReminderModel struct {
	inner agentcore.ChatModel
	load  func() string
}

type capabilityTrailingReminder struct {
	*trailingReminderModel
	capabilities llm.CapabilityProvider
}

func (m *capabilityTrailingReminder) Capabilities() llm.Capabilities {
	return m.capabilities.Capabilities()
}

func appendTrailingReminder(msgs []agentcore.Message, text string) []agentcore.Message {
	if text == "" {
		return msgs
	}
	out := make([]agentcore.Message, len(msgs)+1)
	copy(out, msgs)
	out[len(msgs)] = agentcore.SystemMsg(text)
	return out
}

func (m *trailingReminderModel) reminderText() string {
	if m == nil || m.load == nil {
		return ""
	}
	return m.load()
}

func (m *trailingReminderModel) Generate(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return m.inner.Generate(ctx, appendTrailingReminder(msgs, m.reminderText()), tools, opts...)
}

func (m *trailingReminderModel) GenerateStream(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	return m.inner.GenerateStream(ctx, appendTrailingReminder(msgs, m.reminderText()), tools, opts...)
}

func (m *trailingReminderModel) SupportsTools() bool { return m.inner.SupportsTools() }

func (m *trailingReminderModel) ProviderName() string {
	if pn, ok := m.inner.(agentcore.ProviderNamer); ok {
		return pn.ProviderName()
	}
	return ""
}

func (m *trailingReminderModel) ModelName() string {
	if mn, ok := m.inner.(agentcore.ModelNamer); ok {
		return mn.ModelName()
	}
	return ""
}

func (m *trailingReminderModel) Info() llm.ModelInfo {
	if info, ok := m.inner.(interface{ Info() llm.ModelInfo }); ok {
		return info.Info()
	}
	return llm.ModelInfo{}
}

func hasTrailingReminder(m agentcore.ChatModel) bool {
	switch m.(type) {
	case *trailingReminderModel, *capabilityTrailingReminder:
		return true
	default:
		return false
	}
}
