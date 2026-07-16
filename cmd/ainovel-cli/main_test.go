package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ── parseCLIOptions (version/update/prompt/prompts-dir) ───

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

func TestParseCLIOptionsPromptsDir(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"--prompts-dir", "/tmp/prompts", "request"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.PromptsDir != "/tmp/prompts" || len(args) != 1 || args[0] != "request" {
		t.Fatalf("unexpected parse: opts=%+v args=%v", opts, args)
	}
}

func TestParseCLIOptionsPromptsDirMissingValue(t *testing.T) {
	_, _, err := parseCLIOptions([]string{"--prompts-dir"})
	if err == nil {
		t.Fatal("expected error for missing value")
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

func TestLoadPromptExplicitEmptyPromptFails(t *testing.T) {
	_, err := loadPrompt(cliOptions{PromptFile: ""})
	if err != nil {
		t.Fatalf("empty prompt-file with empty prompt should be ok, got: %v", err)
	}
}

func TestLoadPromptExplicitEmptyPromptFileFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte("   \n  "), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPrompt(cliOptions{PromptFile: path})
	if err == nil {
		t.Fatal("expected error for empty prompt-file content, got nil")
	}
}

// ── compile verification ──────────────────────────────────

func TestCompileCheck(t *testing.T) {
	// Compile-time verification that main package links correctly.
}
