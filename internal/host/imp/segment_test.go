package imp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
)

type segmentTestLLM struct {
	response string
	calls    int
}

func (m *segmentTestLLM) Generate(_ context.Context, _ []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	m.calls++
	return &agentcore.LLMResponse{Message: agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock(m.response)},
	}}, nil
}

func TestChaptersFromMarkersRebuildsOriginalBodies(t *testing.T) {
	text := "书名\n前言\n第一章 风起\n甲\n乙\n第二章 夜行\n丙\n"
	chapters, err := chaptersFromMarkers(text, []segmentMarker{
		{Title: "风起", StartLine: 3},
		{Title: "夜行", StartLine: 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 {
		t.Fatalf("chapters=%d, want 2", len(chapters))
	}
	if chapters[0].Title != "风起" || chapters[0].Content != "甲\n乙" {
		t.Fatalf("first chapter=%+v", chapters[0])
	}
	if chapters[1].Title != "夜行" || chapters[1].Content != "丙" {
		t.Fatalf("second chapter=%+v", chapters[1])
	}
}

func TestSegmentFileUsesTaggedMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "novel.txt")
	text := "第一章 风起\n甲\n第二章 夜行\n乙\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &segmentTestLLM{response: "=== CHAPTERS ===\n[{\"title\":\"风起\",\"start_line\":1},{\"title\":\"夜行\",\"start_line\":3}]"}
	chapters, err := SegmentFile(context.Background(), m, "segment", path)
	if err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 || len(chapters) != 2 || !strings.Contains(chapters[1].Content, "乙") {
		t.Fatalf("calls=%d chapters=%+v", m.calls, chapters)
	}
}
