package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func setupLayered(t *testing.T, volumes []domain.VolumeOutline) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Outline.SaveLayeredOutline(volumes); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := s.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}
	return s
}

func TestCheckArcBoundaryNeedsNewVolume(t *testing.T) {
	// 只有 1 卷 1 弧 1 章，且非 Final → 应触发 NeedsNewVolume
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "首弧", Goal: "目标",
			Chapters: []domain.OutlineEntry{{Title: "第一章", CoreEvent: "开局", Hook: "继续"}},
		}},
	}})

	b, err := s.Outline.CheckArcBoundary(1) // 第 1 章 = 弧/卷最后一章
	if err != nil {
		t.Fatalf("CheckArcBoundary: %v", err)
	}
	if b == nil {
		t.Fatal("expected boundary, got nil")
	}
	if !b.IsArcEnd || !b.IsVolumeEnd {
		t.Fatalf("expected arc+volume end, got arc=%v vol=%v", b.IsArcEnd, b.IsVolumeEnd)
	}
	if !b.NeedsNewVolume {
		t.Fatal("expected NeedsNewVolume=true")
	}
	if b.NextVolume != 0 || b.NextArc != 0 {
		t.Fatalf("expected no next, got vol=%d arc=%d", b.NextVolume, b.NextArc)
	}
}

func TestCheckArcBoundaryLastVolumeRequiresDecision(t *testing.T) {
	// 单卷最后一章 → 触发 NeedsNewVolume，让 Router 让架构师二选一：
	// append_volume 续写 / complete_book 收尾。
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "唯一卷", Theme: "主题",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "唯一弧", Goal: "收束",
			Chapters: []domain.OutlineEntry{{Title: "终章", CoreEvent: "结局", Hook: "无"}},
		}},
	}})

	b, err := s.Outline.CheckArcBoundary(1)
	if err != nil {
		t.Fatalf("CheckArcBoundary: %v", err)
	}
	if !b.NeedsNewVolume {
		t.Fatal("expected NeedsNewVolume=true at last expanded chapter")
	}
	if b.HasNextArc() {
		t.Fatal("expected no next arc")
	}
}

func TestCheckArcBoundaryNextArcInSameVolume(t *testing.T) {
	// 2 弧：第 1 弧结束应指向第 2 弧，不触发 NeedsNewVolume
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "目标", Chapters: []domain.OutlineEntry{{Title: "章一", CoreEvent: "事件", Hook: "钩子"}}},
			{Index: 2, Title: "次弧", Goal: "目标2", EstimatedChapters: 10},
		},
	}})

	b, err := s.Outline.CheckArcBoundary(1)
	if err != nil {
		t.Fatalf("CheckArcBoundary: %v", err)
	}
	if !b.IsArcEnd {
		t.Fatal("expected arc end")
	}
	if b.IsVolumeEnd {
		t.Fatal("expected not volume end (second arc exists)")
	}
	if b.NeedsNewVolume {
		t.Fatal("expected NeedsNewVolume=false")
	}
	if b.NextVolume != 1 || b.NextArc != 2 {
		t.Fatalf("expected next vol=1 arc=2, got vol=%d arc=%d", b.NextVolume, b.NextArc)
	}
	if !b.NeedsExpansion {
		t.Fatal("expected NeedsExpansion=true for skeleton arc")
	}
}

func TestExpandArcCalibratesUnwrittenPlan(t *testing.T) {
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "旧弧", Goal: "造成计划外的决裂", Chapters: []domain.OutlineEntry{{Title: "决裂", CoreEvent: "同伴离队", Hook: "去向不明"}}},
			{Index: 2, Title: "原骨架", Goal: "按原计划同行", EstimatedChapters: 8},
		},
	}})

	expansion := domain.ArcExpansion{
		Title: "分途追索",
		Goal:  "承认决裂已经发生，让两条行动线分别逼近同一真相",
		Chapters: []domain.OutlineEntry{
			{Title: "两张地图", CoreEvent: "两队从不同线索出发", Hook: "线索指向同一地点"},
			{Title: "隔墙回声", CoreEvent: "双方隔空影响彼此选择", Hook: "重逢代价浮现"},
		},
	}
	if err := s.ExpandArc(1, 2, expansion); err != nil {
		t.Fatalf("ExpandArc: %v", err)
	}

	volumes, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	got := volumes[0].Arcs[1]
	if got.Title != expansion.Title || got.Goal != expansion.Goal {
		t.Fatalf("expected calibrated title/goal, got title=%q goal=%q", got.Title, got.Goal)
	}
	if got.EstimatedChapters != 0 || len(got.Chapters) != 2 {
		t.Fatalf("expected expanded arc, got estimated=%d chapters=%d", got.EstimatedChapters, len(got.Chapters))
	}
	flat, err := s.Outline.LoadOutline()
	if err != nil {
		t.Fatalf("LoadOutline: %v", err)
	}
	if len(flat) != 3 || flat[1].Chapter != 2 || flat[2].Chapter != 3 {
		t.Fatalf("expected continuous flattened outline, got %+v", flat)
	}
	progress, err := s.Progress.Load()
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if progress.TotalChapters != 3 {
		t.Fatalf("expected total chapters 3, got %d", progress.TotalChapters)
	}

	if err := s.ExpandArc(1, 2, expansion); err != nil {
		t.Fatalf("same expansion must be idempotent: %v", err)
	}
	changed := expansion
	changed.Goal = "事后改写已展开弧"
	if err := s.ExpandArc(1, 2, changed); err == nil {
		t.Fatal("expected a different expansion to reject overwriting the expanded arc")
	}
}

func TestAppendVolumeValidation(t *testing.T) {
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "首弧", Goal: "目标",
			Chapters: []domain.OutlineEntry{{Title: "章", CoreEvent: "事件", Hook: "钩子"}},
		}},
	}})

	validVol := domain.VolumeOutline{
		Index: 2, Title: "第二卷", Theme: "升级",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "弧一", Goal: "目标",
			Chapters: []domain.OutlineEntry{{Title: "新章", CoreEvent: "推进", Hook: "钩子"}},
		}},
	}

	// 正常追加应成功
	if err := s.AppendVolume(validVol); err != nil {
		t.Fatalf("AppendVolume valid: %v", err)
	}

	// Index 不递增 → 失败
	if err := s.AppendVolume(domain.VolumeOutline{
		Index: 1, Title: "重复", Theme: "x",
		Arcs: []domain.ArcOutline{{Index: 1, Title: "弧", Goal: "g", Chapters: []domain.OutlineEntry{{Title: "ch", CoreEvent: "e", Hook: "h"}}}},
	}); err == nil {
		t.Fatal("expected error for non-increasing index")
	}

	// 无弧 → 失败
	if err := s.AppendVolume(domain.VolumeOutline{Index: 3, Title: "空", Theme: "x"}); err == nil {
		t.Fatal("expected error for volume with no arcs")
	}

	// 首弧无章节 → 失败
	if err := s.AppendVolume(domain.VolumeOutline{
		Index: 3, Title: "骨架", Theme: "x",
		Arcs: []domain.ArcOutline{{Index: 1, Title: "弧", Goal: "g", EstimatedChapters: 10}},
	}); err == nil {
		t.Fatal("expected error for first arc without chapters")
	}
}

// 注：原先用 Final 卷拒绝 append 的语义已下沉到 save_foundation 层（Phase=Complete 拒绝），
// 见 save_foundation_test.go::TestSaveFoundationAppendVolumeRejectsAfterComplete。
// store 层只保留结构性校验（Index 递增 / 首弧含章节等）。

func TestSaveAndLoadCompass(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 空 direction 应失败
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{EstimatedScale: "3 卷"}}); err == nil {
		t.Fatal("expected error for empty ending_direction")
	}

	// 正常保存
	compass := domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "主角面对最终抉择",
			OpenThreads:     []string{"线索A", "关系B"},
			EstimatedScale:  "预计 4-6 卷",
			LastUpdated:     12,
		},
		Current: &domain.Compass{Direction: "先解决眼前困局", OpenThreads: []string{"短线C"}, LastUpdated: 13},
	}
	if err := s.Outline.SaveCompass(compass); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}

	loaded, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatalf("LoadCompass: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected compass, got nil")
	}
	if loaded.Long.EndingDirection != "主角面对最终抉择" {
		t.Fatalf("expected direction %q, got %q", "主角面对最终抉择", loaded.Long.EndingDirection)
	}
	if len(loaded.Long.OpenThreads) != 2 || loaded.Current == nil || len(loaded.Current.OpenThreads) != 1 {
		t.Fatalf("unexpected threads: %+v", loaded)
	}
}

// TestOutlineFeedbackPool 反馈池闭环:commit 落盘 → 跨重启可读 → 结构操作消费清空。
func TestOutlineFeedbackPool(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Outline.AppendOutlineFeedback(ChapterFeedback{Chapter: 3, Deviation: "支线膨胀", Suggestion: "下一弧收线"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Outline.AppendOutlineFeedback(ChapterFeedback{Chapter: 4, Suggestion: "反派提前登场"}); err != nil {
		t.Fatalf("append2: %v", err)
	}

	// 跨重启(新 Store 实例)可读——不是内存态
	// 单写者语义（复核阻塞项 2 方案 A）：同一 workspace 同一进程只允许一个可写
	// Store，模拟重启前先释放（数据已持久化在磁盘）。
	s.Close()
	s2 := NewStore(dir)
	fbs := s2.Outline.LoadPendingOutlineFeedback()
	if len(fbs) != 2 || fbs[0].Chapter != 3 || fbs[1].Suggestion != "反派提前登场" {
		t.Fatalf("跨重启读取失败: %+v", fbs)
	}
	for _, fb := range fbs {
		if fb.At == "" {
			t.Fatal("At 应自动补齐")
		}
	}

	if err := s2.Outline.ClearOutlineFeedback(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if left := s2.Outline.LoadPendingOutlineFeedback(); len(left) != 0 {
		t.Fatalf("消费后应为空: %+v", left)
	}
	// 幂等清空
	if err := s2.Outline.ClearOutlineFeedback(); err != nil {
		t.Fatalf("clear idempotent: %v", err)
	}
}

// ── 分层大纲章节编号契约测试 ──

func TestLayeredOutlineChapterZeroAutoFill(t *testing.T) {
	// Chapter==0 → 自动补齐为结构位置 1
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "弧", Goal: "g",
			Chapters: []domain.OutlineEntry{
				{Chapter: 0, Title: "第一章"},
			},
		}},
	}})
	volumes, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if got := volumes[0].Arcs[0].Chapters[0].Chapter; got != 1 {
		t.Fatalf("Chapter==0 应自动补齐为 1，实际 %d", got)
	}
	// 扁平化一致性（domain.FlattenOutline 直接在加载的 volumes 上验证）
	flat := domain.FlattenOutline(volumes)
	if len(flat) != 1 || flat[0].Chapter != 1 {
		t.Fatalf("FlattenOutline 后 chapter 应为 1，实际 %+v", flat)
	}
}

func TestLayeredOutlineCorrectNonZeroPreserved(t *testing.T) {
	// Chapter==1 与预期一致 → 正常保存
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "弧", Goal: "g",
			Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "第一章"},
			},
		}},
	}})
	volumes, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if got := volumes[0].Arcs[0].Chapters[0].Chapter; got != 1 {
		t.Fatalf("正确的非零 Chapter 应保留，实际被改为 %d", got)
	}
}

func TestLayeredOutlineWrongNonZeroRejected(t *testing.T) {
	// Chapter==7 与预期位置 1 不一致 → 拒绝且零写入
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "弧", Goal: "g",
			Chapters: []domain.OutlineEntry{
				{Chapter: 7, Title: "第七章"},
			},
		}},
	}})
	if err == nil {
		t.Fatal("Chapter 7 != 预期位置 1 应拒绝保存")
	}
	checkNoOutlineFiles(t, dir)
}

func TestLayeredOutlineRejectsSkeletonThenExpandedTopology(t *testing.T) {
	// 骨架弧后不能出现已展开弧
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "骨架", Goal: "g", EstimatedChapters: 3},
			{Index: 2, Title: "已展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章"}}},
		},
	}})
	if err == nil {
		t.Fatal("骨架弧后出现已展开弧应拒绝")
	}
	checkNoOutlineFiles(t, dir)
}

func TestLayeredOutlineExpandedThenSkeletonAccepted(t *testing.T) {
	// 已展开弧在前、骨架弧在后 → 合法拓扑
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "第一章"}}},
			{Index: 2, Title: "骨架", Goal: "g2", EstimatedChapters: 5},
		},
	}})
	volumes, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if len(volumes) != 1 || len(volumes[0].Arcs) != 2 {
		t.Fatalf("预期 1 卷 2 弧，实际 %+v", volumes)
	}
	if got := volumes[0].Arcs[0].Chapters[0].Chapter; got != 1 {
		t.Fatalf("第一章编号应为 1，实际 %d", got)
	}
	// 扁平化一致性（domain.FlattenOutline 直接在加载的 volumes 上验证）
	flat := domain.FlattenOutline(volumes)
	if len(flat) != 1 || flat[0].Chapter != 1 {
		t.Fatalf("FlattenOutline 后应为 1 章且编号 1，实际 %+v", flat)
	}
}

func TestLayeredOutlineLocateAndGetChapterConsistent(t *testing.T) {
	// 多卷多弧多章场景，定位/读取/扁平化编号一致
	volumes := []domain.VolumeOutline{
		{
			Index: 1, Title: "卷一", Theme: "开始",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "弧一", Goal: "g1",
					Chapters: []domain.OutlineEntry{
						{Chapter: 0, Title: "开端"},
						{Chapter: 0, Title: "冲突"},
					}},
				{Index: 2, Title: "弧二", Goal: "g2",
					Chapters: []domain.OutlineEntry{
						{Chapter: 0, Title: "转折"},
					}},
			},
		},
		{
			Index: 2, Title: "卷二", Theme: "发展",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "弧三", Goal: "g3",
					Chapters: []domain.OutlineEntry{
						{Chapter: 0, Title: "深入"},
					}},
			},
		},
	}
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Outline.SaveLayeredOutline(volumes); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	// 验证 LocateChapter
	vol, arc, err := s.Outline.LocateChapter(1)
	if err != nil || vol != 1 || arc != 1 {
		t.Fatalf("LocateChapter(1) = vol=%d arc=%d err=%v", vol, arc, err)
	}
	vol, arc, err = s.Outline.LocateChapter(3)
	if err != nil || vol != 1 || arc != 2 {
		t.Fatalf("LocateChapter(3) = vol=%d arc=%d err=%v", vol, arc, err)
	}
	vol, arc, err = s.Outline.LocateChapter(4)
	if err != nil || vol != 2 || arc != 1 {
		t.Fatalf("LocateChapter(4) = vol=%d arc=%d err=%v", vol, arc, err)
	}

	// 验证 GetChapterFromLayered
	entry, err := s.Outline.GetChapterFromLayered(2)
	if err != nil || entry.Title != "冲突" {
		t.Fatalf("GetChapterFromLayered(2) = %+v err=%v", entry, err)
	}

	// 验证 FlattenOutline（直接从加载的 volumes 上验证）
	vols, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	flat := domain.FlattenOutline(vols)
	if len(flat) != 4 {
		t.Fatalf("扁平化应有 4 章，实际 %d", len(flat))
	}
	for i, e := range flat {
		if e.Chapter != i+1 {
			t.Fatalf("扁平化第 %d 项 Chapter=%d，预期 %d", i, e.Chapter, i+1)
		}
	}
}

// checkNoOutlineFiles 验证目录下不存在任何大纲文件（零写入）。
func checkNoOutlineFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"layered_outline.json", "layered_outline.md", "outline.json", "outline.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("拒绝后不应存在文件 %s", name)
		}
	}
}

// ── 跨卷 / 读取路径 / 扩展/追加边界测试 ──

func TestLayeredOutlineCrossVolumeRejectsSkeletonThenExpanded(t *testing.T) {
	// 全书单一前沿：卷 1 已有骨架弧，卷 2 的展开弧必须拒绝
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	volumes := []domain.VolumeOutline{
		{
			Index: 1, Title: "V1", Theme: "t",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "已展开", Goal: "g",
					Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章1"}}},
				{Index: 2, Title: "骨架", Goal: "g2", EstimatedChapters: 3},
			},
		},
		{
			Index: 2, Title: "V2", Theme: "t2",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "新展开", Goal: "g3",
					Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章2"}}},
			},
		},
	}
	err := s.Outline.SaveLayeredOutline(volumes)
	if err == nil {
		t.Fatal("全书已有骨架弧，跨卷展开弧应被拒绝")
	}
	checkNoOutlineFiles(t, dir)
}

func TestLayeredOutlineLoadRejectsCrossVolumeSkeletonThenExpanded(t *testing.T) {
	// 直接写入跨卷坏拓扑数据到磁盘 → Load 拒绝
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bad := []domain.VolumeOutline{
		{
			Index: 1, Title: "V1", Theme: "t",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "已展开", Goal: "g",
					Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章1"}}},
				{Index: 2, Title: "骨架", Goal: "g2", EstimatedChapters: 3},
			},
		},
		{
			Index: 2, Title: "V2", Theme: "t2",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "新展开", Goal: "g3",
					Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章2"}}},
			},
		},
	}
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, "layered_outline.json"), data, 0o644); err != nil {
		t.Fatalf("write bad data: %v", err)
	}
	if _, err := s.Outline.LoadLayeredOutline(); err == nil {
		t.Fatal("Load 应拒绝跨卷骨架弧后出现展开弧")
	}
}

func TestLayeredOutlineLoadRejectsWrongNumber(t *testing.T) {
	// 手动写入错误编号的数据，验证 LoadLayeredOutline 拒绝
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 直接写入无效 JSON（Chapter=7 但预期是 1）
	bad := []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "弧", Goal: "g",
			Chapters: []domain.OutlineEntry{{Chapter: 7, Title: "错号"}},
		}},
	}}
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, "layered_outline.json"), data, 0o644); err != nil {
		t.Fatalf("write bad data: %v", err)
	}
	if _, err := s.Outline.LoadLayeredOutline(); err == nil {
		t.Fatal("LoadLayeredOutline 应拒绝错误编号数据")
	}
}

func TestLayeredOutlineLoadAcceptsZero(t *testing.T) {
	// Chapter==0 在内存中自动补齐，不写盘
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 写入带 Chapter=0 的数据
	zeroData := []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "弧", Goal: "g",
			Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章"}},
		}},
	}}
	data, _ := json.Marshal(zeroData)
	if err := os.WriteFile(filepath.Join(dir, "layered_outline.json"), data, 0o644); err != nil {
		t.Fatalf("write zero data: %v", err)
	}
	loaded, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline 应接受 Chapter=0: %v", err)
	}
	if loaded[0].Arcs[0].Chapters[0].Chapter != 1 {
		t.Fatalf("Chapter=0 应在内存补齐为 1，实际 %d", loaded[0].Arcs[0].Chapters[0].Chapter)
	}
	// 文件应保留原始 Chapter=0（不写盘）
	rawData, err := os.ReadFile(filepath.Join(dir, "layered_outline.json"))
	if err != nil {
		t.Fatalf("read raw file: %v", err)
	}
	var raw []domain.VolumeOutline
	if err := json.Unmarshal(rawData, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if raw[0].Arcs[0].Chapters[0].Chapter != 0 {
		t.Fatal("磁盘上的 Chapter=0 不应被修改")
	}
}

func TestLayeredOutlineLoadRejectsBadTopology(t *testing.T) {
	// 骨架弧在后有展开弧 → 拒绝
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bad := []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "骨架", Goal: "g", EstimatedChapters: 3},
			{Index: 2, Title: "展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章"}}},
		},
	}}
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, "layered_outline.json"), data, 0o644); err != nil {
		t.Fatalf("write bad data: %v", err)
	}
	if _, err := s.Outline.LoadLayeredOutline(); err == nil {
		t.Fatal("LoadLayeredOutline 应拒绝骨架弧在后有展开弧")
	}
}

func TestExpandArcRejectsBadNumberExpansion(t *testing.T) {
	// 展开一个骨架弧，但 expansion 中的 chapter 使用了错误编号 → 拒绝
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "已写章"}}},
			{Index: 2, Title: "待展开", Goal: "g2", EstimatedChapters: 2},
		},
	}})
	// 第 2 章预期编号为 2（因为第 1 章占用了 1），传入 Chapter=7 应拒绝
	badExp := domain.ArcExpansion{
		Title: "展开", Goal: "新目标",
		Chapters: []domain.OutlineEntry{
			{Chapter: 7, Title: "错号章", CoreEvent: "事件"},
		},
	}
	if err := s.ExpandArc(1, 2, badExp); err == nil {
		t.Fatal("ExpandArc 应拒绝 expansion 中错误编号的 chapter")
	}
}

func TestExpandArcAcceptsZeroNumberExpansion(t *testing.T) {
	// Chapter==0 的 expansion 应正常通过并被自动补齐
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "已写章"}}},
			{Index: 2, Title: "待展开", Goal: "g2", EstimatedChapters: 2},
		},
	}})
	goodExp := domain.ArcExpansion{
		Title: "展开", Goal: "新目标",
		Chapters: []domain.OutlineEntry{
			{Chapter: 0, Title: "新章", CoreEvent: "事件"},
		},
	}
	if err := s.ExpandArc(1, 2, goodExp); err != nil {
		t.Fatalf("Chapter==0 的 expansion 应通过: %v", err)
	}
	vols, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if len(vols) != 1 || len(vols[0].Arcs) != 2 {
		t.Fatalf("预期 1 卷 2 弧，实际 %+v", vols)
	}
	if len(vols[0].Arcs[1].Chapters) != 1 || vols[0].Arcs[1].Chapters[0].Chapter != 2 {
		t.Fatalf("新章节编号应为 2，实际 %d", vols[0].Arcs[1].Chapters[0].Chapter)
	}
}

func TestAppendVolumeRejectsWhenPriorVolHasSkeleton(t *testing.T) {
	// 全书单一前沿：已有骨架弧，不可追加展开卷
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 先用 SaveLayeredOutline 创建含骨架弧的数据
	vols := []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章"}}},
			{Index: 2, Title: "骨架", Goal: "g2", EstimatedChapters: 5},
		},
	}}
	if err := s.Outline.SaveLayeredOutline(vols); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := s.Outline.SaveOutline(domain.FlattenOutline(vols)); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	// 记录当前状态快照用于零持久化校验
	snapLayered, _ := s.Outline.LoadLayeredOutline()
	snapFlat, _ := s.Outline.LoadOutline()

	newVol := domain.VolumeOutline{
		Index: 2, Title: "新卷", Theme: "t2",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "展开弧", Goal: "g3",
			Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "新章"}},
		}},
	}
	err := s.AppendVolume(newVol)
	if err == nil {
		t.Fatal("已有骨架弧时追加展开卷应拒绝")
	}
	// 验证零持久化：layered/flat 不变
	afterLayered, _ := s.Outline.LoadLayeredOutline()
	afterFlat, _ := s.Outline.LoadOutline()
	if len(snapLayered) != len(afterLayered) || len(snapFlat) != len(afterFlat) {
		t.Fatal("拒绝后大纲应不变")
	}
}

func TestAppendVolumeAcceptsAllExpanded(t *testing.T) {
	// 所有既有弧已展开 → 可正常追加展开卷
	s := setupLayered(t, []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "已展开", Goal: "g",
			Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章"}},
		}},
	}})
	newVol := domain.VolumeOutline{
		Index: 2, Title: "新卷", Theme: "t2",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "展开弧", Goal: "g3",
			Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "新章"}},
		}},
	}
	if err := s.AppendVolume(newVol); err != nil {
		t.Fatalf("所有弧已展开时应可追加: %v", err)
	}
	vols, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("预期 2 卷，实际 %d", len(vols))
	}
}

func TestExpandArcRejectsCrossVolumeRearward(t *testing.T) {
	// 跨卷后方 ExpandArc：卷 2 在卷 1 骨架弧之后，不可展开
	s := setupLayered(t, []domain.VolumeOutline{
		{
			Index: 1, Title: "V1", Theme: "t",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "已展开", Goal: "g",
					Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章1"}}},
				{Index: 2, Title: "骨架", Goal: "g2", EstimatedChapters: 3},
			},
		},
		{
			Index: 2, Title: "V2", Theme: "t2",
			Arcs: []domain.ArcOutline{
				{Index: 1, Title: "待展开", Goal: "g3", EstimatedChapters: 2},
			},
		},
	})
	// 记录快照
	snapLayered, _ := s.Outline.LoadLayeredOutline()
	snapFlat, _ := s.Outline.LoadOutline()
	snapProgress, _ := s.Progress.Load()

	exp := domain.ArcExpansion{
		Title: "新弧", Goal: "新目标",
		Chapters: []domain.OutlineEntry{
			{Chapter: 0, Title: "新章", CoreEvent: "事件"},
		},
	}
	err := s.ExpandArc(2, 1, exp)
	if err == nil {
		t.Fatal("跨卷后方展开弧（骨架弧之后）应拒绝")
	}
	// 验证完整状态不变：layered/flat/progress 均应不变
	afterLayered, _ := s.Outline.LoadLayeredOutline()
	afterFlat, _ := s.Outline.LoadOutline()
	afterProgress, _ := s.Progress.Load()
	if len(snapLayered) != len(afterLayered) || len(snapFlat) != len(afterFlat) {
		t.Fatal("拒绝后大纲文件应不变")
	}
	if snapProgress.TotalChapters != afterProgress.TotalChapters {
		t.Fatalf("Progress.TotalChapters 不应变: before=%d after=%d", snapProgress.TotalChapters, afterProgress.TotalChapters)
	}
}

func TestAppendVolumeRejectStrengthened(t *testing.T) {
	// 增强版：已有骨架弧 → AppendVolume 拒绝且完整文件/Progress 不变
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	vols := []domain.VolumeOutline{{
		Index: 1, Title: "卷", Theme: "t",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已展开", Goal: "g",
				Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "章"}}},
			{Index: 2, Title: "骨架", Goal: "g2", EstimatedChapters: 5},
		},
	}}
	if err := s.Outline.SaveLayeredOutline(vols); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := s.Outline.SaveOutline(domain.FlattenOutline(vols)); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	snapLayered, _ := s.Outline.LoadLayeredOutline()
	snapFlat, _ := s.Outline.LoadOutline()
	snapProgress, _ := s.Progress.Load()
	snapFiles := takeOutlineFileHashes(t, dir)

	newVol := domain.VolumeOutline{
		Index: 2, Title: "新卷", Theme: "t2",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "展开弧", Goal: "g3",
			Chapters: []domain.OutlineEntry{{Chapter: 0, Title: "新章"}},
		}},
	}
	err := s.AppendVolume(newVol)
	if err == nil {
		t.Fatal("已有骨架弧时追加展开卷应拒绝")
	}

	afterLayered, _ := s.Outline.LoadLayeredOutline()
	afterFlat, _ := s.Outline.LoadOutline()
	afterProgress, _ := s.Progress.Load()
	if len(snapLayered) != len(afterLayered) || len(snapFlat) != len(afterFlat) {
		t.Fatal("拒绝后大纲应不变")
	}
	if snapProgress.TotalChapters != afterProgress.TotalChapters {
		t.Fatalf("Progress.TotalChapters 不应变: before=%d after=%d", snapProgress.TotalChapters, afterProgress.TotalChapters)
	}
	if !compareOutlineFileHashes(t, dir, snapFiles) {
		t.Fatal("拒绝后大纲文件哈希应不变")
	}
}

// takeOutlineFileHashes 返回大纲相关文件的 SHA256 哈希快照。
func takeOutlineFileHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	hashes := map[string]string{}
	names := []string{"layered_outline.json", "layered_outline.md", "outline.json", "outline.md"}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			hashes[name] = "" // 不存在
			continue
		}
		h := sha256.Sum256(data)
		hashes[name] = fmt.Sprintf("%x", h)
	}
	return hashes
}

func compareOutlineFileHashes(t *testing.T, dir string, before map[string]string) bool {
	t.Helper()
	after := takeOutlineFileHashes(t, dir)
	for name, hash := range before {
		if after[name] != hash {
			t.Errorf("文件 %s 哈希变化: before=%s after=%s", name, hash, after[name])
			return false
		}
	}
	return true
}
