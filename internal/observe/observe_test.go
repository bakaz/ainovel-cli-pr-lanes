package observe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
)

type fakeModel struct {
	kind   string
	calls  atomic.Int32
	active atomic.Int32
}

type retryableProbeError struct{}

func (retryableProbeError) Error() string   { return "retryable probe failure" }
func (retryableProbeError) Retryable() bool { return true }

type countingTool struct{ calls int }

func (t *countingTool) Name() string        { return "check_consistency" }
func (t *countingTool) Description() string { return "test tool" }
func (t *countingTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *countingTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls++
	return nil, nil
}

func (m *fakeModel) Generate(context.Context, []agentcore.Message, []agentcore.ToolSpec, ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return &agentcore.LLMResponse{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock("reply")}}}, nil
}
func (m *fakeModel) GenerateStream(ctx context.Context, _ []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	m.calls.Add(1)
	if m.kind == "retry" {
		return nil, retryableProbeError{}
	}
	out := make(chan agentcore.StreamEvent, 4)
	m.active.Add(1)
	go func() {
		defer m.active.Add(-1)
		defer close(out)
		switch m.kind {
		case "text":
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventTextDelta, Delta: "reply"}
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventDone}
		case "thinking":
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventThinkingDelta, Delta: "think"}
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventDone}
		case "tool":
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventToolCallEnd, CompletedToolCall: &agentcore.ToolCall{
				ID: "tool-1", Name: "check_consistency", Args: json.RawMessage(`{}`),
			}}
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventDone}
		case "error":
			out <- agentcore.StreamEvent{Type: agentcore.StreamEventError, Err: context.Canceled}
		case "race":
			select {
			case <-ctx.Done():
				return
			case <-time.After(11 * time.Millisecond):
				out <- agentcore.StreamEvent{Type: agentcore.StreamEventTextDelta, Delta: "too late"}
			}
		case "stuck":
			select {}
		default:
			<-ctx.Done()
		}
	}()
	return out, nil
}
func (m *fakeModel) SupportsTools() bool { return true }

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	meta := filepath.Join(dir, "meta")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"meta/progress.json":   `{"phase":"writing","current_chapter":1}`,
		"meta/run.json":        `{}`,
		"meta/user_rules.json": `{}`,
		"chapters/01.md":       "chapter",
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		rel, _ := filepath.Rel(dir, path)
		result[rel] = hex.EncodeToString(hash[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPreflightRejectsUnsafeLegacyStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(string)
	}{
		{"complete", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"complete"}`), 0o644)
		}},
		{"empty phase", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":""}`), 0o644)
		}},
		{"malformed progress", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":`), 0o644)
		}},
		{"unknown phase", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"future"}`), 0o644)
		}},
		{"pending steer", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "run.json"), []byte(`{"pending_steer":"x"}`), 0o644)
		}},
		{"pending commit", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "pending_commit.json"), []byte(`{"chapter":1}`), 0o644)
		}},
		{"missing rules", func(dir string) { os.Remove(filepath.Join(dir, "meta", "user_rules.json")) }},
		{"in-progress chapter", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"writing","in_progress_chapter":1}`), 0o644)
		}},
		{"completed scenes", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"writing","completed_scenes":[1]}`), 0o644)
		}},
		{"premise phase non-writing", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"premise"}`), 0o644)
		}},
		{"outline phase non-writing", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"outline"}`), 0o644)
		}},
		{"steering flow non-writing", func(dir string) {
			os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{"phase":"writing","flow":"steering"}`), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t)
			tc.edit(dir)
			before := tree(t, dir)
			if err := Preflight(dir); err == nil {
				t.Fatal("expected observe preflight rejection")
			}
			assertTreeEqual(t, before, tree(t, dir))
		})
	}
}

func TestDefaultDenyPolicyRejectsKnownAndUnknownTools(t *testing.T) {
	policy := DefaultDenyPolicy()
	mock := &countingTool{}
	for _, name := range []string{"check_consistency", "unknown_tool"} {
		tool := agentcore.Tool(mock)
		_, err := executeObserveTool(context.Background(), policy, tool, agentcore.ToolCall{Name: name})
		if err == nil {
			t.Fatalf("tool %q was not denied", name)
		}
	}
	if mock.calls != 0 {
		t.Fatalf("denied tool was executed %d times", mock.calls)
	}
	if err := policy.DispatchAuthorization(&flow.Instruction{Agent: "writer"}); err == nil {
		t.Fatal("observe policy should deny flow dispatch")
	}
}

func TestRunOneWithToolsDeniesStreamedToolBeforeExecute(t *testing.T) {
	tool := &countingTool{}
	result, err := runOneWithTools(context.Background(), time.Second, &fakeModel{kind: "tool"}, []agentcore.Tool{tool})
	if err != nil || result.Success {
		t.Fatalf("tool probe result=%+v err=%v", result, err)
	}
	if tool.calls != 0 {
		t.Fatalf("runtime guard allowed Execute: calls=%d", tool.calls)
	}
}

func TestRunTextAndThinkingAreSuccessfulAndWriteNothing(t *testing.T) {
	for _, kind := range []string{"text", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixture(t)
			before := tree(t, dir)
			result, err := Run(context.Background(), Options{Dir: dir, Timeout: time.Second, Model: &fakeModel{kind: kind}})
			if err != nil || !result.Success {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			assertTreeEqual(t, before, tree(t, dir))
		})
	}
}

func TestRunToolAndProviderErrorAreNonzeroAndNeverExecuteTools(t *testing.T) {
	for _, kind := range []string{"tool", "error", "retry"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixture(t)
			before := tree(t, dir)
			model := &fakeModel{kind: kind}
			result, _ := Run(context.Background(), Options{Dir: dir, Timeout: time.Second, Model: model})
			if result.Success {
				t.Fatalf("tool/error probe unexpectedly succeeded: %+v", result)
			}
			if kind == "retry" && model.calls.Load() != 1 {
				t.Fatalf("retry terminal invoked provider %d times", model.calls.Load())
			}
			if model.active.Load() != 0 {
				t.Fatalf("provider work remained after return: %d", model.active.Load())
			}
			assertTreeEqual(t, before, tree(t, dir))
		})
	}
}

func TestRunTimeoutIsBoundedAndNonzero(t *testing.T) {
	dir := fixture(t)
	before := tree(t, dir)
	start := time.Now()
	model := &fakeModel{kind: "stuck"}
	result, _ := Run(context.Background(), Options{Dir: dir, Timeout: 20 * time.Millisecond, Model: model})
	if result.Success || !strings.HasPrefix(result.Reason, "scope failure") || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("timeout probe result=%+v elapsed=%s", result, time.Since(start))
	}
	if model.calls.Load() != 1 {
		t.Fatalf("timeout invoked provider %d times", model.calls.Load())
	}
	assertTreeEqual(t, before, tree(t, dir))
}

func TestRunDeadlineWinsDeltaRace(t *testing.T) {
	dir := fixture(t)
	before := tree(t, dir)
	model := &fakeModel{kind: "race"}
	result, _ := Run(context.Background(), Options{Dir: dir, Timeout: 10 * time.Millisecond, Model: model})
	if result.Success || result.Reason != "timeout" {
		t.Fatalf("deadline race produced spurious result: %+v", result)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("race provider invoked %d times", model.calls.Load())
	}
	assertTreeEqual(t, before, tree(t, dir))
}

func assertTreeEqual(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("tree changed: before=%v after=%v", want, got)
	}
	for path, hash := range want {
		if got[path] != hash {
			t.Fatalf("tree file %q changed or disappeared", path)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Fatalf("tree file %q was created", path)
		}
	}
}

func TestCanonicalDirRequiresAbsoluteExistingDirectory(t *testing.T) {
	if _, err := CanonicalDir("relative"); err == nil {
		t.Fatal("relative observe directory should be rejected")
	}
	dir := fixture(t)
	got, err := CanonicalDir(dir + string(filepath.Separator) + ".")
	if err != nil || !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Fatalf("canonical dir=%q err=%v", got, err)
	}
}

func TestRunMissingProgressPreservesTree(t *testing.T) {
	dir := fixture(t)
	os.Remove(filepath.Join(dir, "meta", "progress.json"))
	before := tree(t, dir)
	if _, err := Run(context.Background(), Options{Dir: dir, Timeout: time.Second, Model: &fakeModel{kind: "text"}}); err == nil {
		t.Fatal("missing progress should be rejected")
	}
	assertTreeEqual(t, before, tree(t, dir))
}

func TestRunAbsoluteNonexistentDirPreservesParentTree(t *testing.T) {
	parent := t.TempDir()
	sentinel := filepath.Join(parent, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tree(t, parent)
	target := filepath.Join(parent, "does-not-exist")
	if _, err := Run(context.Background(), Options{Dir: target, Timeout: time.Second, Model: &fakeModel{kind: "text"}}); err == nil {
		t.Fatal("nonexistent directory should be rejected")
	}
	assertTreeEqual(t, before, tree(t, parent))
}

var _ = domain.PhaseWriting
