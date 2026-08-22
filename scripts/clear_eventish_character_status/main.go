package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

var eventishNeedles = []string{
	"完成阶段", "首轮完成", "落地", "预告", "完成认领", "完成移送",
	"完成等待", "完成（", "完成(", "核完成", "收束完成",
}

var keepStatus = map[string]bool{
	"status.驯顺因果":  true,
	"status.幻觉触碰":  true,
	"status.默认姿态":  true,
}

// staleProgress is a second-round explicit list: one-shot chapter-progress
// keys that did not match eventishNeedles (or belong to entity AI).
var staleProgress = map[string]bool{
	"女孩\x00status.移送阶段":            true,
	"女孩\x00status.密室14上架阶段":        true,
	"女孩\x00status.密室14接口定位阶段":      true,
	"女孩\x00status.密室14排队灯列阶段":      true,
	"AI\x00status.密室14第一枚乳导管阀接管":   true,
	"AI\x00status.密室14宫阀日常化接管":     true,
	"女孩\x00status.密室14抽灌循环第二轮封死完成": true,
	"女孩\x00status.密室14四路接管终态确认完成":  true,
	"女孩\x00status.密室15第一轮多人操阀完成":   true,
	"女孩\x00status.夜班第三班笑腔之夜完成":     true,
	"女孩\x00status.夜班结算与晨间交接完成":     true,
	"女孩\x00status.白天直播评分解说循环深化完成":  true,
	"女孩\x00status.白天自由使用时段深化完成":    true,
	"女孩\x00status.白天直播乳税弹幕段完成":     true,
	"女孩\x00status.白天直播同场峰值段完成":     true,
}

func isEventish(e domain.CharacterStateEntry) bool {
	if e.Entity != "女孩" {
		return false
	}
	if !strings.HasPrefix(e.Field, "status.") {
		return false
	}
	if keepStatus[e.Field] {
		return false
	}
	blob := e.Field + e.Value
	for _, n := range eventishNeedles {
		if strings.Contains(blob, n) {
			return true
		}
	}
	return false
}

func shouldDrop(e domain.CharacterStateEntry) bool {
	if staleProgress[e.Entity+"\x00"+e.Field] {
		return true
	}
	return isEventish(e)
}

func main() {
	apply := flag.Bool("apply", false, "actually write character_state.json (default dry-run)")
	path := flag.String("path", `G:\opencode\ainovel-cli_0.6.3_Windows_x86_64\workspace\output\novel\meta\character_state.json`, "character_state.json path")
	flag.Parse()

	raw, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	var entries []domain.CharacterStateEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		os.Exit(1)
	}

	var drop, keep []domain.CharacterStateEntry
	for _, e := range entries {
		if shouldDrop(e) {
			drop = append(drop, e)
			continue
		}
		keep = append(keep, e)
	}

	fmt.Printf("total=%d drop=%d keep=%d apply=%v\n", len(entries), len(drop), len(keep), *apply)
	for _, e := range drop {
		fmt.Printf("DROP %s %s ch=%d\n", e.Entity, e.Field, e.UpdatedChapter)
	}
	if !*apply {
		fmt.Println("dry-run only; pass -apply to write")
		return
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(*path), ".."))
	st := store.NewStore(dir)
	defer st.Close()
	if err := st.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "store init: %v\n", err)
		os.Exit(1)
	}
	chapter := 831
	if p, err := st.Progress.Load(); err == nil && p != nil && p.CurrentChapter > 0 {
		chapter = p.CurrentChapter
	}
	updates := make([]domain.CharacterStateUpdate, 0, len(drop))
	for _, e := range drop {
		updates = append(updates, domain.CharacterStateUpdate{
			Entity: e.Entity,
			Field:  e.Field,
			Value:  "",
			Reason: "bulk_clear_eventish_status",
		})
	}
	if err := st.World.UpsertCharacterState(chapter, updates); err != nil {
		fmt.Fprintf(os.Stderr, "upsert: %v\n", err)
		os.Exit(1)
	}
	left, err := st.World.LoadCharacterState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reload: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("applied chapter=%d remaining=%d\n", chapter, len(left))
}
