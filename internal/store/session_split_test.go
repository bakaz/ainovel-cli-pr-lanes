package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractChapterPrefersDraftSection(t *testing.T) {
	task := "规则里提到第 3 章只是例子\n### 章节与草稿（字数：100）\n第 120 章\n\n正文"
	if got := extractChapter(task); got != "ch120" {
		t.Fatalf("extractChapter = %q, want ch120", got)
	}
}

func TestExtractChapterUsesLastMention(t *testing.T) {
	task := "第 3 章示例\n后面才是第 88 章"
	if got := extractChapter(task); got != "ch88" {
		t.Fatalf("extractChapter = %q, want ch88", got)
	}
}

func TestSplitSessionJSONLByChapter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "polisher-ch03.jsonl")
	lines := []string{
		`{"role":"user","content":[{"type":"text","text":"### 章节与草稿（字数：10）\n第 12 章\n\na"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"edit-12"}]}`,
		`{"role":"user","content":[{"type":"text","text":"### 章节与草稿（字数：10）\n第 13 章\n\nb"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"edit-13"}]}`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SplitSessionJSONLByChapter(src, "polisher")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceLines != 4 || got.ByChapter[12] != 2 || got.ByChapter[13] != 2 || got.Unattributed != 0 {
		t.Fatalf("split result = %+v", got)
	}
	ch12, err := os.ReadFile(filepath.Join(dir, "polisher-ch12.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ch12), "第 12 章") || strings.Contains(string(ch12), "第 13 章") {
		t.Fatalf("ch12 file = %s", ch12)
	}
}

func TestSessionLoggerRoutesDraftSectionToChapterFile(t *testing.T) {
	dir := t.TempDir()
	s := NewSessionStore(newIO(dir))
	logger := s.SubAgentLogger(nil)
	task := "第 3 章只是规则示例\n### 章节与草稿（字数：10）\n第 120 章\n\n正文"
	logger("polisher", task, makeAssistantWithUsage())
	if _, err := os.Stat(filepath.Join(dir, "meta/sessions/agents/polisher-ch120.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta/sessions/agents/polisher-ch03.jsonl")); err == nil {
		t.Fatal("should not write collapsed ch03 file")
	}
}
