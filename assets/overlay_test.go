package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyOverridesWhitelistPrecedenceAndSources(t *testing.T) {
	low := t.TempDir()
	high := t.TempDir()
	writeFile(t, filepath.Join(low, "prompts", "writer.md"), "LOW_WRITER\n{{VOICE}}")
	writeFile(t, filepath.Join(high, "prompts", "writer.md"), "HIGH_WRITER\n{{VOICE}}")
	writeFile(t, filepath.Join(high, "prompts", "arbiter-failure.md"), "ARBITER_FAILURE_CANARY")
	writeFile(t, filepath.Join(high, "prompts", "style-critic.md"), "CRITIC_v1")
	writeFile(t, filepath.Join(high, "prompts", "coordinator.md"), "UNSUPPORTED_CANARY")
	writeFile(t, filepath.Join(high, "styles", "default.md"), "STYLE_CANARY")

	bundle := Load("default", LoadOptions{})
	report := ApplyOverrides(&bundle, "default", []string{low, high})
	if !strings.HasPrefix(bundle.Prompts.Writer, "HIGH_WRITER") {
		t.Fatalf("project/high prompt should win: %q", bundle.Prompts.Writer)
	}
	if !strings.Contains(bundle.Prompts.Writer, "仿写画像") {
		t.Fatal("external core prompt must retain production simulation wrapper")
	}
	if bundle.Prompts.ArbiterFailure != "ARBITER_FAILURE_CANARY" {
		t.Fatalf("arbiter prompt override missing: %q", bundle.Prompts.ArbiterFailure)
	}
	if bundle.Prompts.StyleCritic != "CRITIC_v1" {
		t.Fatalf("style-critic prompt override missing: %q", bundle.Prompts.StyleCritic)
	}
	if bundle.Styles["default"] != "STYLE_CANARY" {
		t.Fatalf("style override missing: %q", bundle.Styles["default"])
	}
	if strings.Contains(bundle.Prompts.Writer, "UNSUPPORTED_CANARY") {
		t.Fatal("unsupported prompt must never enter a runtime prompt")
	}
	if src := bundle.Sources["prompts/writer.md"]; src.Kind != "file" || src.Key != "prompts/writer.md" || len(src.SHA256) != 64 {
		t.Fatalf("writer source not auditable: %+v", src)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "coordinator.md") {
		t.Fatalf("unsupported file should be surfaced exactly once: %+v", report.Warnings)
	}
}

func TestStyleCriticOverlay_RejectedOnInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "prompts", "style-critic.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := Load("default", LoadOptions{})
	want := bundle.Prompts.StyleCritic
	report := ApplyOverrides(&bundle, "default", []string{root})
	if bundle.Prompts.StyleCritic != want {
		t.Fatal("invalid style-critic overlay must fall back to embedded prompt")
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "UTF-8") {
		t.Fatalf("invalid UTF-8 warning missing: %+v", report.Warnings)
	}
}

func TestApplyOverridesInvalidUTF8FallsBack(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "prompts", "editor.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := Load("default", LoadOptions{})
	want := bundle.Prompts.Editor
	report := ApplyOverrides(&bundle, "default", []string{root})
	if bundle.Prompts.Editor != want {
		t.Fatal("invalid overlay must fail closed to embedded prompt")
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "UTF-8") {
		t.Fatalf("invalid UTF-8 warning missing: %+v", report.Warnings)
	}
}

func TestLoadRecordsEmbeddedPromptAndStyleSources(t *testing.T) {
	bundle := Load("default", LoadOptions{})
	for _, key := range []string{"prompts/writer.md", "prompts/arbiter-plan-start.md", "styles/default.md", "voice.md"} {
		src, ok := bundle.Sources[key]
		if !ok || src.Kind != "embedded" || len(src.SHA256) != 64 {
			t.Fatalf("missing embedded source %q: %+v", key, src)
		}
	}
}

func TestResolvedPathWithinRootRejectsOutside(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "prompts")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if ok, err := resolvedPathWithinRoot(root, inside); err != nil || !ok {
		t.Fatalf("inside path rejected: ok=%v err=%v", ok, err)
	}
	if ok, err := resolvedPathWithinRoot(root, outside); err != nil || ok {
		t.Fatalf("outside path accepted: ok=%v err=%v", ok, err)
	}
}
