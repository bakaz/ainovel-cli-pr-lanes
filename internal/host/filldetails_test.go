package host

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// TestFillDetailsTimelineFiltersBeyondMaxCompleted 验证时间线只显示已完成章节以内的条目:
// CompletedChapters 乱序([3,1,2])时 maxCompleted 必须遍历求最大值,ch4/ch518 等
// 旧版残留的高章号条目被过滤,且结果按章号倒序。
func TestFillDetailsTimelineFiltersBeyondMaxCompleted(t *testing.T) {
	st := newFillDetailsStore(t)
	p, err := st.Progress.Load()
	if err != nil {
		t.Fatal(err)
	}
	p.CompletedChapters = []int{3, 1, 2} // 乱序,模拟只 append 不排序的写入
	if err := st.Progress.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := st.World.SaveTimeline([]domain.TimelineEvent{
		{Chapter: 1, Time: "2026-01-01T00:00:00Z", Event: "开局"},
		{Chapter: 2, Time: "2026-01-02T00:00:00Z", Event: "铺垫"},
		{Chapter: 3, Time: "2026-01-03T00:00:00Z", Event: "高潮"},
		{Chapter: 4, Time: "2026-01-04T00:00:00Z", Event: "第四章残留"},
		{Chapter: 518, Time: "2026-01-05T00:00:00Z", Event: "旧版518残留"},
	}); err != nil {
		t.Fatal(err)
	}

	progress, err := st.Progress.Load()
	if err != nil {
		t.Fatal(err)
	}
	snap := UISnapshot{}
	(&Host{store: st}).fillDetails(&snap, progress)

	if len(snap.RecentTimeline) != 3 {
		t.Fatalf("应只显示 3 条已完成章时间线,得 %d 条: %v", len(snap.RecentTimeline), snap.RecentTimeline)
	}
	for _, want := range []string{"第3章", "第2章", "第1章"} {
		found := false
		for _, line := range snap.RecentTimeline {
			if strings.HasPrefix(line, want+":") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("时间线应含 %q,实际: %v", want, snap.RecentTimeline)
		}
	}
	if got := snap.RecentTimeline[0]; !strings.HasPrefix(got, "第3章:") {
		t.Errorf("首条应为第3章(章号倒序),得 %q", got)
	}
	for _, bad := range []string{"第4章", "第518章", "残留"} {
		for _, line := range snap.RecentTimeline {
			if strings.Contains(line, bad) {
				t.Errorf("时间线不应含 %q,实际: %v", bad, snap.RecentTimeline)
			}
		}
	}
}

// TestFillDetailsTimelineHiddenWithoutCompleted 验证 progress 为 nil 或零完成章时
// 时间线不显示(不回退成全量)。
func TestFillDetailsTimelineHiddenWithoutCompleted(t *testing.T) {
	st := newFillDetailsStore(t)
	if err := st.World.SaveTimeline([]domain.TimelineEvent{
		{Chapter: 1, Time: "2026-01-01T00:00:00Z", Event: "开局"},
		{Chapter: 518, Time: "2026-01-05T00:00:00Z", Event: "旧版518残留"},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("progress为nil", func(t *testing.T) {
		snap := UISnapshot{}
		(&Host{store: st}).fillDetails(&snap, nil)
		if len(snap.RecentTimeline) != 0 {
			t.Errorf("progress 为 nil 时时间线不应显示,得 %v", snap.RecentTimeline)
		}
	})

	t.Run("空CompletedChapters", func(t *testing.T) {
		p, err := st.Progress.Load()
		if err != nil {
			t.Fatal(err)
		}
		p.CompletedChapters = nil
		if err := st.Progress.Save(p); err != nil {
			t.Fatal(err)
		}
		progress, err := st.Progress.Load()
		if err != nil {
			t.Fatal(err)
		}
		snap := UISnapshot{}
		(&Host{store: st}).fillDetails(&snap, progress)
		if len(snap.RecentTimeline) != 0 {
			t.Errorf("无完成章时时间线不应显示,得 %v", snap.RecentTimeline)
		}
	})
}

func TestFillDetailsOutlineWindowKeepsPlannedCount(t *testing.T) {
	st := newFillDetailsStore(t)
	vol1 := make([]domain.OutlineEntry, 30)
	vol2 := make([]domain.OutlineEntry, 8)
	for i := range vol1 {
		vol1[i] = domain.OutlineEntry{Title: "v1", CoreEvent: "e", Hook: "h"}
	}
	for i := range vol2 {
		vol2[i] = domain.OutlineEntry{Title: "v2", CoreEvent: "e", Hook: "h"}
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{
		{Index: 1, Title: "第一卷", Theme: "起步", Arcs: []domain.ArcOutline{{Index: 1, Title: "弧1", Goal: "g", Chapters: vol1}}},
		{Index: 2, Title: "第二卷", Theme: "推进", Arcs: []domain.ArcOutline{{Index: 1, Title: "弧1", Goal: "g", Chapters: vol2}}},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := st.Progress.Load()
	if err != nil {
		t.Fatal(err)
	}
	p.Layered = true
	p.CurrentVolume = 2
	p.CurrentArc = 1
	p.InProgressChapter = 32
	if err := st.Progress.Save(p); err != nil {
		t.Fatal(err)
	}
	progress, err := st.Progress.Load()
	if err != nil {
		t.Fatal(err)
	}

	snap := UISnapshot{}
	(&Host{store: st}).fillDetails(&snap, progress)

	if snap.OutlinePlanned != 38 {
		t.Fatalf("OutlinePlanned=%d, want 38", snap.OutlinePlanned)
	}
	if len(snap.Outline) != 8 {
		t.Fatalf("right-panel outline len=%d, want current volume 8: %+v", len(snap.Outline), snap.Outline)
	}
	if snap.Outline[0].Chapter != 31 || snap.Outline[7].Chapter != 38 {
		t.Fatalf("volume window %d-%d, want 31-38", snap.Outline[0].Chapter, snap.Outline[7].Chapter)
	}
}

func newFillDetailsStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("测试小说", 100); err != nil {
		t.Fatal(err)
	}
	return st
}
