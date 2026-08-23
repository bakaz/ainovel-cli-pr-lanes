package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/voocel/ainovel-cli/internal/store"
)

func main() {
	dir := `G:\opencode\ainovel-cli_0.6.3_Windows_x86_64\workspace\output\novel\meta\sessions\agents`
	jobs := []struct{ src, agent string }{
		{"polisher-ch03.jsonl", "polisher"},
		{"style_critic-ch03.jsonl", "style_critic"},
	}
	for _, job := range jobs {
		src := filepath.Join(dir, job.src)
		if _, err := os.Stat(src); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", src, err)
			continue
		}
		res, err := store.SplitSessionJSONLByChapter(src, job.agent)
		if err != nil {
			fmt.Fprintf(os.Stderr, "split %s: %v\n", src, err)
			os.Exit(1)
		}
		fmt.Printf("%s lines=%d chapters=%d unattributed=%d\n", job.src, res.SourceLines, len(res.ByChapter), res.Unattributed)
		archived := src + ".cpu-archive"
		if err := os.Rename(src, archived); err != nil {
			fmt.Fprintf(os.Stderr, "archive %s: %v\n", src, err)
			os.Exit(1)
		}
		fmt.Printf("archived %s\n", archived)
	}
}
