package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SessionSplitResult is the outcome of splitting a collapsed agent jsonl by chapter.
type SessionSplitResult struct {
	SourceLines    int
	ByChapter      map[int]int
	Unattributed   int
	UnattributedTo string
}

type sessionLinePeek struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (p sessionLinePeek) text() string {
	var b strings.Builder
	for _, c := range p.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// SplitSessionJSONLByChapter copies lines from src into per-chapter jsonl files
// next to it. Existing chapter files are appended. Unattributed lines go to
// <agent>-unattributed.jsonl. The source file is not renamed; the caller archives it.
func SplitSessionJSONLByChapter(srcPath, agent string) (SessionSplitResult, error) {
	out := SessionSplitResult{ByChapter: map[int]int{}}
	f, err := os.Open(srcPath)
	if err != nil {
		return out, err
	}
	defer f.Close()

	dir := filepath.Dir(srcPath)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)

	currentCh := 0
	writers := map[int]*os.File{}
	var unattr *os.File
	closeAll := func() {
		for _, w := range writers {
			_ = w.Close()
		}
		if unattr != nil {
			_ = unattr.Close()
		}
	}
	defer closeAll()

	openChapter := func(ch int) (*os.File, error) {
		if w, ok := writers[ch]; ok {
			return w, nil
		}
		name := fmt.Sprintf("%s-%s.jsonl", agent, fmt.Sprintf("ch%02d", ch))
		path := filepath.Join(dir, name)
		w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		writers[ch] = w
		return w, nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		out.SourceLines++
		var peek sessionLinePeek
		if err := json.Unmarshal(line, &peek); err != nil {
			return out, fmt.Errorf("parse line %d: %w", out.SourceLines, err)
		}
		if peek.Role == "user" {
			if suffix := extractChapter(peek.text()); suffix != "" {
				var n int
				fmt.Sscanf(suffix, "ch%d", &n)
				if n > 0 {
					currentCh = n
				}
			}
		}
		payload := append(append([]byte{}, line...), '\n')
		if currentCh <= 0 {
			if unattr == nil {
				p := filepath.Join(dir, agent+"-unattributed.jsonl")
				unattr, err = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					return out, err
				}
				out.UnattributedTo = p
			}
			if _, err := unattr.Write(payload); err != nil {
				return out, err
			}
			out.Unattributed++
			continue
		}
		w, err := openChapter(currentCh)
		if err != nil {
			return out, err
		}
		if _, err := w.Write(payload); err != nil {
			return out, err
		}
		out.ByChapter[currentCh]++
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	written := out.Unattributed
	for _, n := range out.ByChapter {
		written += n
	}
	if written != out.SourceLines {
		return out, fmt.Errorf("split count %d != source %d", written, out.SourceLines)
	}
	return out, nil
}
