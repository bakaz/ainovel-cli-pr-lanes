package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
)

// ── parseCLIOptions (version/update/prompt) ─────────────────

func TestParseCLIOptionsVersion(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"--version"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Version || len(args) != 0 {
		t.Fatalf("expected version, got: %+v args=%v", opts, args)
	}

	opts, args, err = parseCLIOptions([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Version || len(args) != 0 {
		t.Fatalf("expected version, got: %+v args=%v", opts, args)
	}
}

func TestParseCLIOptionsVersionRejectsArg(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"version", "x"})
	if err == nil {
		t.Fatal("expected error for version with arg")
	}
}

func TestParseCLIOptionsVersionConflict(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"version", "--headless"})
	if err == nil {
		t.Fatal("version should conflict with other flags")
	}
}

func TestParseCLIOptionsUpdate(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Update {
		t.Fatal("expected update")
	}
}

func TestParseCLIOptionsUpdateWithVersion(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"update", "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Update || opts.UpdateVersion != "v1.0.0" {
		t.Fatalf("unexpected: %+v", opts)
	}
}

func TestParseCLIOptionsUpdateRejectsExtraArg(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"update", "v1", "extra"})
	if err == nil {
		t.Fatal("expected error for extra arg")
	}
}

func TestParseCLIOptionsUpdateConflict(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"update", "--headless"})
	if err == nil {
		t.Fatal("update should conflict with other flags")
	}
}

func TestParseCLIOptionsConfig(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"--config", "/tmp/cfg.json"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ConfigPath != "/tmp/cfg.json" {
		t.Fatalf("unexpected config: %q", opts.ConfigPath)
	}
}

func TestParseCLIOptionsConfigMissingValue(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"--config"})
	if err == nil {
		t.Fatal("expected error for missing config value")
	}
}

func TestParseCLIOptionsHeadless(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"--headless"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Headless {
		t.Fatal("expected headless")
	}
}

func TestParseCLIOptionsPrompt(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"--headless", "--prompt", "write a story"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Prompt != "write a story" {
		t.Fatalf("unexpected prompt: %q", opts.Prompt)
	}
	if len(args) != 0 {
		t.Fatalf("expected no args, got: %v", args)
	}
}

func TestParseCLIOptionsPromptMissingValue(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"--headless", "--prompt"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseCLIOptionsPromptfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, _, err := parseCLIOptions([]string{"--headless", "--prompt-file", path})
	if err != nil {
		t.Fatal(err)
	}
	if opts.PromptFile != path {
		t.Fatalf("unexpected prompt-file: %q", opts.PromptFile)
	}
}

func TestParseCLIOptionsPromptAndPromptFileConflict(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"--headless", "--prompt", "x", "--prompt-file", "/tmp/p.txt"})
	if err == nil {
		t.Fatal("expected conflict error")
	}
}

// ── loadPrompt ─────────────────────────────────────────────

func TestLoadPromptNoFlagsReturnsEmpty(t *testing.T) {
	content, err := loadPrompt(cliOptions{})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if content != "" {
		t.Fatalf("expected empty, got: %q", content)
	}
}

func TestLoadPromptValidPrompt(t *testing.T) {
	content, err := loadPrompt(cliOptions{Prompt: "  写一本玄幻小说  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "写一本玄幻小说" {
		t.Fatalf("expected trimmed content, got: %q", content)
	}
}

func TestLoadPromptValidPromptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("  写一个故事  "), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := loadPrompt(cliOptions{PromptFile: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "写一个故事" {
		t.Fatalf("expected trimmed content, got: %q", content)
	}
}

// ── compile verification ──────────────────────────────────

func TestCompileCheck(t *testing.T) {
	// Compile-time verification that main package links correctly.
}

// ── observe helpers ────────────────────────────────────────

func writeMainFixture(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range map[string]string{
		"meta/progress.json":   `{"phase":"writing"}`,
		"meta/run.json":        `{}`,
		"meta/user_rules.json": `{}`,
		"chapters/01.md":       "chapter",
	} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func mainTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		result[rel] = fileHash(t, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertMainTreeEqual(t *testing.T, before map[string]string, dir string) {
	t.Helper()
	after := mainTree(t, dir)
	if len(before) != len(after) {
		t.Fatalf("observe changed tree: before=%d after=%d", len(before), len(after))
	}
	for path, hash := range before {
		if after[path] != hash {
			t.Fatalf("observe changed %q", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Fatalf("observe added %q", path)
		}
	}
}

// ── observe command validation ─────────────────────────────

func TestObserveCommandValidationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(string) []string
	}{
		{"missing dir", func(string) []string { return []string{"--timeout", "30s"} }},
		{"missing timeout", func(dir string) []string { return []string{"--dir", dir} }},
		{"missing timeout value", func(dir string) []string { return []string{"--dir", dir, "--timeout"} }},
		{"relative dir", func(string) []string { return []string{"--dir", "relative", "--timeout", "30s"} }},
		{"zero timeout", func(dir string) []string { return []string{"--dir", dir, "--timeout", "0s"} }},
		{"duplicate timeout", func(dir string) []string { return []string{"--dir", dir, "--timeout", "1s", "--timeout", "2s"} }},
		{"invalid duration", func(dir string) []string { return []string{"--dir", dir, "--timeout", "not-a-duration"} }},
		{"duplicate dir", func(dir string) []string { return []string{"--dir", dir, "--timeout", "30s", "--dir", dir} }},
		{"unknown flag", func(dir string) []string { return []string{"--dir", dir, "--timeout", "30s", "--unknown", "x"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMainFixture(t, dir)
			before := mainTree(t, dir)
			if code := observeCommand(tc.args(dir)); code == 0 {
				t.Fatalf("observe invalid invocation unexpectedly succeeded")
			}
			assertMainTreeEqual(t, before, dir)
		})
	}
}

func TestObserveCommandValidationRefusalsAbsoluteMissingTarget(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "does-not-exist")
	before := mainTree(t, parent)
	if code := observeCommand([]string{"--dir", target, "--timeout", "30s"}); code == 0 {
		t.Fatal("absolute nonexistent target unexpectedly succeeded")
	}
	assertMainTreeEqual(t, before, parent)
}

func TestObserveCommandRefusalMatrixPreservesTargetTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(string) []string
	}{
		{"dir missing value", func(string) []string { return []string{"--dir"} }},
		{"explicit empty dir", func(string) []string { return []string{"--dir", "", "--timeout", "30s"} }},
		{"explicit empty timeout", func(dir string) []string { return []string{"--dir", dir, "--timeout", ""} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMainFixture(t, dir)
			before := mainTree(t, dir)
			if code := observeCommand(tc.args(dir)); code == 0 {
				t.Fatal("observe refusal unexpectedly succeeded")
			}
			assertMainTreeEqual(t, before, dir)
		})
	}
}

func TestObserveCommandFailureTrees(t *testing.T) {
	for _, tc := range []struct {
		name  string
		load  func() (bootstrap.Config, error)
		build func(bootstrap.Config) (agentcore.ChatModel, error)
	}{
		{"config builder failure", func() (bootstrap.Config, error) {
			return bootstrap.Config{}, context.Canceled
		}, nil},
		{"model builder failure", func() (bootstrap.Config, error) {
			return bootstrap.Config{}, nil
		}, func(bootstrap.Config) (agentcore.ChatModel, error) { return nil, context.Canceled }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMainFixture(t, dir)
			before := mainTree(t, dir)
			originalLoader, originalBuilder := observeConfigLoader, observeModelBuilder
			defer func() { observeConfigLoader, observeModelBuilder = originalLoader, originalBuilder }()
			observeConfigLoader, observeModelBuilder = tc.load, tc.build
			if code := observeCommand([]string{"--dir", dir, "--timeout", "30s"}); code == 0 {
				t.Fatal("failure unexpectedly succeeded")
			}
			assertMainTreeEqual(t, before, dir)
		})
	}
}

func TestObserveIsAnEarlyProductionDispatch(t *testing.T) {
	if got := earlyCommand([]string{"ainovel-cli", "observe", "--dir", "x"}); got != "observe" {
		t.Fatalf("early command = %q, want observe", got)
	}
	if got := earlyCommand([]string{"ainovel-cli", "start"}); got != "" {
		t.Fatalf("normal startup unexpectedly classified as early command %q", got)
	}
}

// ── observe fake model and execution tests ─────────────────

type observeCommandFakeModel struct {
	fail  bool
	calls *int
}

func (m observeCommandFakeModel) Generate(context.Context, []agentcore.Message, []agentcore.ToolSpec, ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return &agentcore.LLMResponse{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock("ok")}}}, nil
}
func (m observeCommandFakeModel) GenerateStream(context.Context, []agentcore.Message, []agentcore.ToolSpec, ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	if m.calls != nil {
		*m.calls++
	}
	ch := make(chan agentcore.StreamEvent, 2)
	if m.fail {
		ch <- agentcore.StreamEvent{Type: agentcore.StreamEventError, Err: context.Canceled}
	} else {
		ch <- agentcore.StreamEvent{Type: agentcore.StreamEventTextDelta, Delta: "ok"}
		ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone}
	}
	close(ch)
	return ch, nil
}
func (observeCommandFakeModel) SupportsTools() bool { return true }

func TestObserveCommandFakeModelExitMappingAndTreePreservation(t *testing.T) {
	dir := t.TempDir()
	writeMainFixture(t, dir)
	before := mainTree(t, dir)
	originalLoader, originalBuilder := observeConfigLoader, observeModelBuilder
	defer func() { observeConfigLoader, observeModelBuilder = originalLoader, originalBuilder }()
	observeConfigLoader = func() (bootstrap.Config, error) { return bootstrap.Config{}, nil }
	observeModelBuilder = func(bootstrap.Config) (agentcore.ChatModel, error) { return observeCommandFakeModel{}, nil }
	if code := observeCommand([]string{"--dir", dir, "--timeout", "1s"}); code != 0 {
		t.Fatalf("fake-model success returned exit %d", code)
	}
	assertMainTreeEqual(t, before, dir)

	observeModelBuilder = func(bootstrap.Config) (agentcore.ChatModel, error) { return observeCommandFakeModel{fail: true}, nil }
	if code := observeCommand([]string{"--dir", dir, "--timeout", "1s"}); code == 0 {
		t.Fatal("fake-model provider failure returned success")
	}
	assertMainTreeEqual(t, before, dir)
}

func TestObserveModelBuilderUsesDefaultModel(t *testing.T) {
	dir := t.TempDir()
	writeMainFixture(t, dir)
	before := mainTree(t, dir)
	var calls int
	originalLoader, originalFactory := observeConfigLoader, observeModelSetFactory
	defer func() { observeConfigLoader, observeModelSetFactory = originalLoader, originalFactory }()
	observeConfigLoader = func() (bootstrap.Config, error) {
		return bootstrap.Config{
			Provider:  "test-provider",
			ModelName: "test-model",
			Providers: map[string]bootstrap.ProviderConfig{
				"test-provider": {Type: "openai", APIKey: "sk-test"},
			},
		}, nil
	}
	observeModelSetFactory = func(cfg bootstrap.Config) (observeModelSet, error) {
		return &fakeModelSet{model: observeCommandFakeModel{calls: &calls}}, nil
	}
	if code := observeCommand([]string{"--dir", dir, "--timeout", "1s"}); code != 0 {
		t.Fatalf("default model config returned exit %d", code)
	}
	if calls < 1 {
		t.Fatalf("observe probe made %d provider requests; want at least one", calls)
	}
	assertMainTreeEqual(t, before, dir)
}

type fakeModelSet struct {
	model agentcore.ChatModel
}

func (s *fakeModelSet) ForRole(role string) agentcore.ChatModel {
	return s.model
}
