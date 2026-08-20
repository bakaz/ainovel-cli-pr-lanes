package imp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/voocel/agentcore"
)

// segmentMarker 是 import_segment 的最小输出协议。
// 模型只需指出每个章节标题所在的 1-based 行号，正文由 Go 从原文件重建，
// 避免模型改写或截断原文。
type segmentMarker struct {
	Title     string `json:"title"`
	StartLine int    `json:"start_line"`
}

// SegmentFile 使用显式配置的 import_segment 模型识别章节边界。
// 默认导入仍走 SplitFile；该函数只在用户主动配置 import_segment 时调用。
func SegmentFile(ctx context.Context, llm LLMChat, systemPrompt, path string) ([]Chapter, error) {
	if llm == nil {
		return nil, fmt.Errorf("segment llm is nil")
	}
	text, err := readSourceText(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("source file is empty: %s", path)
	}

	resp, err := llm.Generate(ctx, []agentcore.Message{
		agentcore.SystemMsg(systemPrompt),
		agentcore.UserMsg(buildSegmentUserPrompt(text)),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("segment llm generate: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("segment llm returned nil response")
	}

	markers, err := parseSegmentOutput(resp.Message.TextContent())
	if err != nil {
		return nil, err
	}
	return chaptersFromMarkers(text, markers)
}

func buildSegmentUserPrompt(text string) string {
	lines := strings.Split(text, "\n")
	var sb strings.Builder
	sb.WriteString("以下是待导入的小说原文。每行前有不可修改的 1-based 行号。请只识别章节起始行，不要改写正文。\n")
	sb.WriteString("输出每个章节的标题和标题所在行号；按正文顺序排列。\n\n")
	for i, line := range lines {
		fmt.Fprintf(&sb, "%06d | %s\n", i+1, line)
	}
	return sb.String()
}

func parseSegmentOutput(text string) ([]segmentMarker, error) {
	env := parseTaggedEnvelope(text)
	if env == nil {
		return nil, fmt.Errorf("no === CHAPTERS === envelope found in segment output")
	}
	body, ok := env["CHAPTERS"]
	if !ok {
		return nil, fmt.Errorf("segment output missing CHAPTERS tag")
	}
	var markers []segmentMarker
	if err := json.Unmarshal([]byte(stripFences(body)), &markers); err != nil {
		return nil, fmt.Errorf("decode segment chapters: %w", err)
	}
	if len(markers) == 0 {
		return nil, fmt.Errorf("segment output contains no chapters")
	}
	return markers, nil
}

func chaptersFromMarkers(text string, markers []segmentMarker) ([]Chapter, error) {
	lines := strings.Split(text, "\n")
	chapters := make([]Chapter, 0, len(markers))
	previous := 0
	for i, marker := range markers {
		title := strings.TrimSpace(marker.Title)
		if title == "" {
			return nil, fmt.Errorf("segment chapter %d has empty title", i+1)
		}
		if marker.StartLine < 1 || marker.StartLine > len(lines) {
			return nil, fmt.Errorf("segment chapter %d has invalid start_line %d", i+1, marker.StartLine)
		}
		if marker.StartLine <= previous {
			return nil, fmt.Errorf("segment chapter %d start_line %d is not strictly increasing", i+1, marker.StartLine)
		}
		previous = marker.StartLine

		start := marker.StartLine // omit the heading line itself
		end := len(lines)
		if i+1 < len(markers) {
			end = markers[i+1].StartLine - 1
		}
		if end <= start {
			return nil, fmt.Errorf("segment chapter %d has empty body", i+1)
		}
		body := strings.TrimSpace(stripTrailingNoise(strings.Join(lines[start:end], "\n")))
		if body == "" {
			return nil, fmt.Errorf("segment chapter %d has empty body", i+1)
		}
		chapters = append(chapters, Chapter{Title: title, Content: body})
	}
	return chapters, nil
}
