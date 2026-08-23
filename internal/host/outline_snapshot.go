package host

import "github.com/voocel/ainovel-cli/internal/domain"

// maxOutlineSnapshotChapters 是 TUI 右栏一次携带的大纲上限。
// 长篇把 800+ 章全量塞进 UISnapshot 会让每次 Snapshot / Model 拷贝都分配
// 大切片，详情面板重渲也要走完整网格。当前卷通常远小于这个数；超长卷再按
// 写作锚点取窗口。
const maxOutlineSnapshotChapters = 80

func selectOutlineForSnapshot(outline []domain.OutlineEntry, progress *domain.Progress, volumes []domain.VolumeOutline) []OutlineSnapshot {
	selected := outline
	if progress != nil && progress.Layered && progress.CurrentVolume > 0 && len(volumes) > 0 {
		if start, end, ok := volumeChapterRange(volumes, progress.CurrentVolume); ok {
			selected = filterOutlineByChapterRange(outline, start, end)
		}
	}
	selected = windowOutlineEntries(selected, outlineAnchor(progress), maxOutlineSnapshotChapters)
	out := make([]OutlineSnapshot, len(selected))
	for i, e := range selected {
		out[i] = OutlineSnapshot{Chapter: e.Chapter, Title: e.Title, CoreEvent: e.CoreEvent}
	}
	return out
}

func volumeChapterRange(volumes []domain.VolumeOutline, volume int) (start, end int, ok bool) {
	ch := 1
	for _, v := range volumes {
		n := 0
		for _, a := range v.Arcs {
			n += len(a.Chapters)
		}
		if v.Index == volume {
			if n == 0 {
				return 0, 0, false
			}
			return ch, ch + n - 1, true
		}
		ch += n
	}
	return 0, 0, false
}

func filterOutlineByChapterRange(entries []domain.OutlineEntry, start, end int) []domain.OutlineEntry {
	out := make([]domain.OutlineEntry, 0, end-start+1)
	for _, e := range entries {
		if e.Chapter >= start && e.Chapter <= end {
			out = append(out, e)
		}
	}
	return out
}

func outlineAnchor(progress *domain.Progress) int {
	if progress == nil {
		return 1
	}
	if progress.InProgressChapter > 0 {
		return progress.InProgressChapter
	}
	maxCh := 0
	for _, ch := range progress.CompletedChapters {
		if ch > maxCh {
			maxCh = ch
		}
	}
	if maxCh > 0 {
		return maxCh
	}
	return 1
}

func windowOutlineEntries(entries []domain.OutlineEntry, anchor, limit int) []domain.OutlineEntry {
	n := len(entries)
	if n <= limit || limit <= 0 {
		return entries
	}
	idx := 0
	for i, e := range entries {
		if e.Chapter <= anchor {
			idx = i
			continue
		}
		break
	}
	start := idx - limit/2
	if start < 0 {
		start = 0
	}
	end := start + limit
	if end > n {
		end = n
		start = end - limit
		if start < 0 {
			start = 0
		}
	}
	return entries[start:end]
}
