package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── --continue-prompt-file 显式性（阻断 5 ③）：空值不得静默落入普通 resume ──

func TestParseCLIOptions_ContinuePromptFileRecordsExplicitness(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"--headless", "--continue-prompt-file", ""})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.ContinuePromptSet {
		t.Fatal("--continue-prompt-file 显式出现（即使值为空）必须被记录")
	}
	if _, err := loadContinuePrompt(opts); err == nil {
		t.Fatal("显式空值必须被拒绝，不得被当成未指定而落入普通 resume")
	}
	if !strings.Contains(loadContinuePromptErr(opts), "缺少文件路径") {
		t.Fatalf("expected missing-path error, got %v", loadContinuePromptErr(opts))
	}
}

func TestLoadContinuePrompt_AbsentReturnsEmpty(t *testing.T) {
	got, err := loadContinuePrompt(cliOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestLoadContinuePrompt_EmptyFileContentRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := cliOptions{ContinuePromptSet: true, ContinuePromptFile: path}
	if _, err := loadContinuePrompt(opts); err == nil {
		t.Fatal("空内容必须被拒绝")
	}
	if !strings.Contains(loadContinuePromptErr(opts), "内容为空") {
		t.Fatalf("expected empty-content error, got %v", loadContinuePromptErr(opts))
	}
}

func TestLoadContinuePrompt_NonEmptyFileOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("继续写下一章\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := cliOptions{ContinuePromptSet: true, ContinuePromptFile: path}
	got, err := loadContinuePrompt(opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "继续写下一章" {
		t.Fatalf("got %q", got)
	}
}

func TestParseCLIOptions_ContinueConflictsWithPrompt(t *testing.T) {
	// 显式空值同样计入冲突（此前 ContinuePromptFile=="" 会静默放行）
	for _, argv := range [][]string{
		{"--continue-prompt-file", "x.txt", "--prompt", "hi"},
		{"--continue-prompt-file", "", "--prompt", "hi"},
	} {
		if _, _, err := parseCLIOptions(argv); err == nil || !strings.Contains(err.Error(), "不能同时使用") {
			t.Fatalf("argv=%v: expected conflict error, got %v", argv, err)
		}
	}
}

// ── 次要 1：--prompt / --prompt-file 显式性（显式空值拒绝 + 冲突校验） ──

func TestParseCLIOptions_PromptExplicitEmptyRejected(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"--headless", "--prompt", ""})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.PromptSet {
		t.Fatal("--prompt 显式出现（即使值为空）必须被记录")
	}
	if _, err := loadPrompt(opts); err == nil {
		t.Fatal("显式空 prompt 必须被拒绝，不得静默落入普通恢复")
	}
	if !strings.Contains(loadPromptErr(opts), "内容为空") {
		t.Fatalf("expected empty-prompt error, got %v", loadPromptErr(opts))
	}
}

func TestParseCLIOptions_PromptFileExplicitEmptyRejected(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"--headless", "--prompt-file", ""})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.PromptFileSet {
		t.Fatal("--prompt-file 显式出现（即使路径为空）必须被记录")
	}
	if _, err := loadPrompt(opts); err == nil {
		t.Fatal("显式空路径必须被拒绝")
	}
	if !strings.Contains(loadPromptErr(opts), "缺少文件路径") {
		t.Fatalf("expected missing-path error, got %v", loadPromptErr(opts))
	}
}

func TestParseCLIOptions_EmptyPromptCountsAsConflict(t *testing.T) {
	// --continue-prompt-file x --prompt "" → 冲突（此前 Prompt=="" 静默放行）
	if _, _, err := parseCLIOptions([]string{"--continue-prompt-file", "x.txt", "--prompt", ""}); err == nil || !strings.Contains(err.Error(), "不能同时使用") {
		t.Fatalf("expected conflict for empty --prompt with --continue-prompt-file, got %v", err)
	}
	// --prompt-file "" 与 --prompt y → 两者都显式 → 冲突
	if _, _, err := parseCLIOptions([]string{"--prompt", "y", "--prompt-file", ""}); err == nil || !strings.Contains(err.Error(), "不能同时使用") {
		t.Fatalf("expected conflict for explicit --prompt-file with --prompt, got %v", err)
	}
	// 双方都显式空值同样冲突（Set 标志语义，不因值空而放行）
	if _, _, err := parseCLIOptions([]string{"--headless", "--prompt", "", "--prompt-file", ""}); err == nil || !strings.Contains(err.Error(), "不能同时使用") {
		t.Fatalf("expected conflict for both-empty --prompt/--prompt-file, got %v", err)
	}
}

func TestLoadPrompt_AbsentReturnsEmpty(t *testing.T) {
	got, err := loadPrompt(cliOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestLoadPrompt_NonEmptyValueOK(t *testing.T) {
	opts := cliOptions{PromptSet: true, Prompt: "  写一个悬疑故事  "}
	got, err := loadPrompt(opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "写一个悬疑故事" {
		t.Fatalf("got %q, want trimmed prompt", got)
	}
}

func TestLoadPrompt_NonEmptyFileOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("继续写下一章\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := cliOptions{PromptFileSet: true, PromptFile: path}
	got, err := loadPrompt(opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "继续写下一章" {
		t.Fatalf("got %q", got)
	}
}

// loadContinuePromptErr 只取错误文本（测试辅助）。
func loadContinuePromptErr(opts cliOptions) string {
	_, err := loadContinuePrompt(opts)
	if err == nil {
		return ""
	}
	return err.Error()
}

// loadPromptErr 只取错误文本（测试辅助）。
func loadPromptErr(opts cliOptions) string {
	_, err := loadPrompt(opts)
	if err == nil {
		return ""
	}
	return err.Error()
}
