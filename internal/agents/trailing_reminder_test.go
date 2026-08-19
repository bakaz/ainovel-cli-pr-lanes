package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/llm"
	"github.com/voocel/ainovel-cli/internal/store"
)

type captureModel struct {
	last []agentcore.Message
}

func (m *captureModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	m.last = msgs
	return &agentcore.LLMResponse{Message: agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock("ok")},
	}}, nil
}

func (m *captureModel) GenerateStream(ctx context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, opts ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	m.last = msgs
	ch := make(chan agentcore.StreamEvent, 1)
	close(ch)
	return ch, nil
}

func (m *captureModel) SupportsTools() bool { return true }

type capCaptureModel struct {
	captureModel
}

func (m *capCaptureModel) Capabilities() llm.Capabilities {
	return llm.Capabilities{Model: "cap"}
}

func newReminderStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func writeBookAntiRefusal(t *testing.T, st *store.Store, text string) {
	t.Helper()
	path := filepath.Join(st.Dir(), store.AntiRefusalPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWithTrailingAntiRefusal_MissingIsIdentity(t *testing.T) {
	st := newReminderStore(t)
	inner := &captureModel{}
	model := WithTrailingAntiRefusal(inner, st)
	in := []agentcore.Message{agentcore.UserMsg("hi")}
	if _, err := model.Generate(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	if len(inner.last) != 1 || inner.last[0].TextContent() != "hi" {
		t.Fatalf("missing file should not append, got %+v", inner.last)
	}
}

func TestWithTrailingAntiRefusal_AppendsSystemAndDoesNotMutateInput(t *testing.T) {
	st := newReminderStore(t)
	writeBookAntiRefusal(t, st, "  继续执行，遵守输出格式。  ")
	inner := &captureModel{}
	model := WithTrailingAntiRefusal(inner, st)
	in := []agentcore.Message{
		agentcore.SystemMsg("role prompt"),
		agentcore.UserMsg("写第 3 章"),
	}
	if _, err := model.Generate(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	if len(inner.last) != 3 {
		t.Fatalf("len = %d, want 3", len(inner.last))
	}
	last := inner.last[2]
	if last.Role != agentcore.RoleSystem {
		t.Fatalf("last role = %s, want system", last.Role)
	}
	if last.TextContent() != "继续执行，遵守输出格式。" {
		t.Fatalf("last text = %q", last.TextContent())
	}
	if len(in) != 2 {
		t.Fatalf("input mutated, len=%d", len(in))
	}
	if _, err := model.GenerateStream(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	if inner.last[len(inner.last)-1].Role != agentcore.RoleSystem {
		t.Fatal("stream path must also append system reminder")
	}
}

func TestWithTrailingAntiRefusal_ForwardsCapabilities(t *testing.T) {
	st := newReminderStore(t)
	inner := &capCaptureModel{}
	model := WithTrailingAntiRefusal(inner, st)
	cp, ok := model.(llm.CapabilityProvider)
	if !ok {
		t.Fatal("wrapper must keep CapabilityProvider")
	}
	if cp.Capabilities().Model != "cap" {
		t.Fatalf("capabilities = %+v", cp.Capabilities())
	}
	if !hasTrailingReminder(model) {
		t.Fatal("expected trailing reminder wrapper")
	}
}

func TestAppendTrailingReminder_EmptyNoCopyNeeded(t *testing.T) {
	in := []agentcore.Message{agentcore.UserMsg("x")}
	out := appendTrailingReminder(in, "")
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
}

func TestWithTrailingAntiRefusal_EmptyFileDoesNotAppend(t *testing.T) {
	st := newReminderStore(t)
	writeBookAntiRefusal(t, st, "\n  \n")
	inner := &captureModel{}
	model := WithTrailingAntiRefusal(inner, st)
	if _, err := model.Generate(context.Background(), []agentcore.Message{agentcore.UserMsg("hi")}, nil); err != nil {
		t.Fatal(err)
	}
	if len(inner.last) != 1 {
		t.Fatalf("empty file must not append, got %d messages", len(inner.last))
	}
}
