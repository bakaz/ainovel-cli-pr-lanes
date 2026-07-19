package migratev3

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func renderOutline(entries []domain.OutlineEntry) string {
	var b strings.Builder
	b.WriteString("# 大纲\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "## 第 %d 章：%s\n\n", e.Chapter, e.Title)
		fmt.Fprintf(&b, "**核心事件**：%s\n\n", e.CoreEvent)
		if e.Hook != "" {
			fmt.Fprintf(&b, "**钩子**：%s\n\n", e.Hook)
		}
		if len(e.Scenes) > 0 {
			b.WriteString("**场景**：\n")
			for i, scene := range e.Scenes {
				fmt.Fprintf(&b, "%d. %s\n", i+1, scene.Text())
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func renderLayeredOutline(volumes []domain.VolumeOutline) string {
	var b strings.Builder
	b.WriteString("# 分层大纲\n\n")
	chapter := 1
	for _, volume := range volumes {
		fmt.Fprintf(&b, "## 第 %d 卷：%s\n\n", volume.Index, volume.Title)
		fmt.Fprintf(&b, "**主题**：%s\n\n", volume.Theme)
		for _, arc := range volume.Arcs {
			fmt.Fprintf(&b, "### 第 %d 弧：%s\n\n", arc.Index, arc.Title)
			fmt.Fprintf(&b, "**目标**：%s\n\n", arc.Goal)
			if !arc.IsExpanded() {
				fmt.Fprintf(&b, "*（待展开，预估 %d 章）*\n\n", arc.EstimatedChapters)
				continue
			}
			for _, entry := range arc.Chapters {
				fmt.Fprintf(&b, "#### 第 %d 章：%s\n\n", chapter, entry.Title)
				fmt.Fprintf(&b, "**核心事件**：%s\n\n", entry.CoreEvent)
				if entry.Hook != "" {
					fmt.Fprintf(&b, "**钩子**：%s\n\n", entry.Hook)
				}
				chapter++
			}
		}
	}
	return b.String()
}
