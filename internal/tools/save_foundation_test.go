package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestSaveFoundationPersistsPlanningTier(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveFoundationTool(store)
	args, err := json.Marshal(map[string]any{
		"type":    "premise",
		"content": "# 测试书名\n\n## 题材和基调\n测试",
		"scale":   "long",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	meta, err := store.RunMeta.Load()
	if err != nil {
		t.Fatalf("LoadRunMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected run meta to exist")
	}
	if meta.PlanningTier != domain.PlanningTierLong {
		t.Fatalf("expected planning tier %q, got %q", domain.PlanningTierLong, meta.PlanningTier)
	}
}

func TestSaveFoundationPremiseSetsNovelName(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := store.Progress.Init("novel", 0); err != nil {
		t.Fatalf("Init progress: %v", err)
	}

	tool := NewSaveFoundationTool(store)
	args, err := json.Marshal(map[string]any{
		"type": "premise",
		"content": `# 长夜燃灯

## 题材和基调
东方玄幻，冷硬求生。`,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	progress, err := store.Progress.Load()
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if progress == nil {
		t.Fatal("expected progress")
	}
	if progress.NovelName != "长夜燃灯" {
		t.Fatalf("expected novel name set, got %q", progress.NovelName)
	}
}

func TestSaveFoundationOutlineClearsLayeredStateWhenDowngrading(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := store.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(store)

	layeredArgs, err := json.Marshal(map[string]any{
		"type":    "layered_outline",
		"content": `[{"index":1,"title":"第一卷","theme":"主题","arcs":[{"index":1,"title":"第一弧","goal":"目标","chapters":[{"chapter":1,"title":"第一章","core_event":"开局","hook":"继续"}]}]}]`,
		"scale":   "long",
	})
	if err != nil {
		t.Fatalf("Marshal layered args: %v", err)
	}
	if _, err := tool.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered outline: %v", err)
	}

	outlineArgs, err := json.Marshal(map[string]any{
		"type":    "outline",
		"content": `[{"chapter":1,"title":"第一章","core_event":"改为中篇","hook":"继续"}]`,
		"scale":   "mid",
	})
	if err != nil {
		t.Fatalf("Marshal outline args: %v", err)
	}
	if _, err := tool.Execute(context.Background(), outlineArgs); err != nil {
		t.Fatalf("Execute outline: %v", err)
	}

	progress, err := store.Progress.Load()
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if progress == nil {
		t.Fatal("expected progress to exist")
	}
	if progress.Layered {
		t.Fatal("expected layered mode to be disabled")
	}
	if progress.CurrentVolume != 0 || progress.CurrentArc != 0 {
		t.Fatalf("expected volume/arc reset, got volume=%d arc=%d", progress.CurrentVolume, progress.CurrentArc)
	}

	volumes, err := store.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if len(volumes) != 0 {
		t.Fatalf("expected layered outline cleared, got %d volumes", len(volumes))
	}

	meta, err := store.RunMeta.Load()
	if err != nil {
		t.Fatalf("LoadRunMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected run meta to exist")
	}
	if meta.PlanningTier != domain.PlanningTierMid {
		t.Fatalf("expected planning tier %q, got %q", domain.PlanningTierMid, meta.PlanningTier)
	}
}

func TestSaveFoundationAppendVolume(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	// 先创建初始 layered_outline（卷1）
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "第一卷", "theme": "起步",
			"arcs": []map[string]any{{
				"index": 1, "title": "首弧", "goal": "目标",
				"chapters": []map[string]any{{"title": "第一章", "core_event": "开局", "hook": "继续"}},
			}},
		}},
		"scale": "long",
	})
	if _, err := tool.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}

	// append_volume：追加卷2
	appendArgs, _ := json.Marshal(map[string]any{
		"type":   "append_volume",
		"reason": "主线仍有多条长线未收束，需继续第二卷",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "升级",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{{"title": "新章", "core_event": "推进", "hook": "钩子"}},
			}},
		},
	})
	res, err := tool.Execute(context.Background(), appendArgs)
	if err != nil {
		t.Fatalf("Execute append_volume: %v", err)
	}
	var result map[string]any
	json.Unmarshal(res, &result)
	if result["volume"] != float64(2) {
		t.Fatalf("expected volume=2, got %v", result["volume"])
	}

	// 验证大纲有 2 卷
	volumes, _ := s.Outline.LoadLayeredOutline()
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	if volumes[1].Title != "第二卷" {
		t.Fatalf("expected title '第二卷', got %q", volumes[1].Title)
	}

	// 卷末判定理由必须进裁定审计
	recs, _ := s.Decisions.Recent(1)
	if len(recs) != 1 || recs[0].Kind != "volume_end" || recs[0].Decider != "architect" {
		t.Fatalf("append_volume 应落一条 volume_end 裁定审计, got %+v", recs)
	}
	if recs[0].Reason == "" || !strings.Contains(string(recs[0].Decision), `"append_volume"`) {
		t.Fatalf("审计记录应含 reason 与 action, got %+v", recs[0])
	}
}

func TestSaveFoundationExpandArcCalibratesTarget(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"title": "裂盟之后",
			"goal":  "让分裂后的双方以不同选择推进同一主线",
			"chapters": []map[string]any{{
				"title": "各走一边", "core_event": "双方分别追索真相", "hook": "两条线索意外重合", "scenes": []string{"分道", "追索"},
			}},
		},
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute expand_arc: %v", err)
	}
	var facts map[string]any
	if err := json.Unmarshal(result, &facts); err != nil {
		t.Fatalf("Unmarshal result: %v", err)
	}
	if facts["title"] != "裂盟之后" || facts["goal"] != "让分裂后的双方以不同选择推进同一主线" {
		t.Fatalf("expected calibrated facts, got %+v", facts)
	}
	volumes, err := s.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if got := volumes[0].Arcs[1]; got.Title != "裂盟之后" || got.Goal != "让分裂后的双方以不同选择推进同一主线" || len(got.Chapters) != 1 {
		t.Fatalf("unexpected expanded arc: %+v", got)
	}
}

func TestSaveFoundationAppendVolumeValidation(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	// 初始卷
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "第一卷", "theme": "起步",
			"arcs": []map[string]any{{
				"index": 1, "title": "首弧", "goal": "目标",
				"chapters": []map[string]any{{"title": "第一章", "core_event": "开局", "hook": "继续"}},
			}},
		}},
		"scale": "long",
	})
	tool.Execute(context.Background(), layeredArgs)

	// Index 不递增 → 应失败（结构性校验）
	appendArgs, _ := json.Marshal(map[string]any{
		"type":   "append_volume",
		"reason": "测试理由",
		"content": map[string]any{
			"index": 1, "title": "重复 Index", "theme": "x",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{{"title": "章", "core_event": "事件", "hook": "钩子"}},
			}},
		},
	})
	_, err := tool.Execute(context.Background(), appendArgs)
	if err == nil {
		t.Fatal("expected error when appending volume with non-increasing index")
	}
}

// TestSaveFoundationAppendVolumeRejectsAfterComplete 验证 Phase=Complete 后不允许 append_volume。
// 取代旧的"Final 卷拒绝追加"语义（Final 字段已删除）。
func TestSaveFoundationAppendVolumeRejectsAfterComplete(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Progress.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	tool := NewSaveFoundationTool(s)
	appendArgs, _ := json.Marshal(map[string]any{
		"type":   "append_volume",
		"reason": "测试理由",
		"content": map[string]any{
			"index": 1, "title": "尝试续写", "theme": "x",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧", "goal": "g",
				"chapters": []map[string]any{{"title": "章", "core_event": "e", "hook": "h"}},
			}},
		},
	})
	if _, err := tool.Execute(context.Background(), appendArgs); err == nil {
		t.Fatal("expected error when appending after Phase=Complete")
	}
}

func TestSaveFoundationUpdateCompass(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type":    "update_compass",
		"section": "long",
		"reason":  "初始建立全书终局方向",
		"content": map[string]any{
			"ending_direction": "主角面对最终抉择",
			"open_threads":     []string{"线索A", "关系B"},
			"estimated_scale":  "预计 4-6 卷",
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute update_compass: %v", err)
	}

	compass, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatalf("LoadCompass: %v", err)
	}
	if compass == nil || compass.Long.EndingDirection != "主角面对最终抉择" {
		t.Fatalf("unexpected compass: %+v", compass)
	}
	if len(compass.Long.OpenThreads) != 2 {
		t.Fatalf("expected 2 open threads, got %d", len(compass.Long.OpenThreads))
	}
}

func TestSaveFoundationUpdateCompassOverridesLastUpdated(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Save(&domain.Progress{
		NovelName:         "光斑",
		Phase:             domain.PhaseWriting,
		CompletedChapters: []int{1, 2, 3, 5, 4}, // 乱序，验证取 max 而非 len
	}); err != nil {
		t.Fatalf("Save progress: %v", err)
	}

	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type":    "update_compass",
		"section": "long",
		"reason":  "用户改变长期方向",
		"content": map[string]any{
			"ending_direction": "主角面对最终抉择",
			"open_threads":     []string{"线索A"},
			"last_updated":     0, // LLM 通常忘填或留 0
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute update_compass: %v", err)
	}

	compass, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatalf("LoadCompass: %v", err)
	}
	if compass.Long.LastUpdated != 5 {
		t.Fatalf("expected LastUpdated=5 (max of CompletedChapters), got %d", compass.Long.LastUpdated)
	}
}

func TestSaveFoundationUpdateCompassRequiresDirection(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type":    "update_compass",
		"section": "long",
		"reason":  "初始建立",
		"content": map[string]any{"estimated_scale": "3 卷"},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when ending_direction is empty")
	}
}

func TestSaveFoundationCompassMergesSectionsAndRequiresLongReason(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	tool := NewSaveFoundationTool(s)
	call := func(input map[string]any) error {
		raw, _ := json.Marshal(input)
		_, err := tool.Execute(context.Background(), raw)
		return err
	}
	if err := call(map[string]any{
		"type": "update_compass", "section": "long", "reason": "初始建立",
		"content": map[string]any{"ending_direction": "终局", "open_threads": []string{"长线"}, "estimated_scale": "5卷"},
	}); err != nil {
		t.Fatal(err)
	}
	seed, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatal(err)
	}
	seed.Long.Reference = json.RawMessage(`{"schema":"long-reference.v1"}`)
	if err := s.Outline.SaveCompass(*seed); err != nil {
		t.Fatal(err)
	}
	if err := call(map[string]any{
		"type": "update_compass", "section": "current",
		"content": map[string]any{"direction": "先追查失踪案", "open_threads": []string{"短线"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := call(map[string]any{
		"type": "update_compass", "section": "long", "content": map[string]any{"estimated_scale": "6卷"},
	}); !errors.Is(err, errs.ErrToolArgs) {
		t.Fatalf("long without reason should fail, got %v", err)
	}
	if err := call(map[string]any{
		"type": "update_compass", "section": "long", "reason": "用户要求扩篇",
		"content": map[string]any{"estimated_scale": "6卷"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Outline.LoadCompass()
	if got.Long.EndingDirection != "终局" || got.Long.EstimatedScale != "6卷" || len(got.Long.OpenThreads) != 1 || got.Current == nil || got.Current.Direction != "先追查失踪案" {
		t.Fatalf("section merge failed: %+v", got)
	}
	var reference map[string]any
	if err := json.Unmarshal(got.Long.Reference, &reference); err != nil || reference["schema"] != "long-reference.v1" {
		t.Fatalf("long reference should survive partial updates: %s (err=%v)", got.Long.Reference, err)
	}
}

func TestSaveFoundationAcceptsDirectJSONArrayContent(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveFoundationTool(store)
	args, err := json.Marshal(map[string]any{
		"type": "outline",
		"content": []map[string]any{
			{
				"chapter":    1,
				"title":      "第一章",
				"core_event": "主角登场",
				"hook":       "继续",
				"scenes":     []string{"场景一", "场景二"},
			},
		},
		"scale": "short",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	outline, err := store.Outline.LoadOutline()
	if err != nil {
		t.Fatalf("LoadOutline: %v", err)
	}
	if len(outline) != 1 || outline[0].Title != "第一章" {
		t.Fatalf("unexpected outline: %+v", outline)
	}
}

// completeBookSetup 建一份处于 writing 阶段、共 2 章的最小 Store,用于 complete_book
// 系列测试。工具层校验(全部可枚举,进代码不进提示词):progress 已初始化、
// PendingRewrites 为空、至少写完一章、大纲内无未写章节。
func completeBookSetup(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 2); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhaseWriting)
	return s
}

func TestSaveFoundationCompleteBookPushesPhaseComplete(t *testing.T) {
	s := completeBookSetup(t)
	for ch := 1; ch <= 2; ch++ {
		if err := s.Progress.MarkChapterComplete(ch, 3000, "", ""); err != nil {
			t.Fatalf("MarkChapterComplete(%d): %v", ch, err)
		}
	}
	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type": "complete_book", "content": map[string]any{},
		"reason": "两章大纲全部写完，终局命题已回答",
	})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute complete_book: %v", err)
	}
	var result map[string]any
	_ = json.Unmarshal(res, &result)
	if result["book_complete"] != true {
		t.Fatalf("expected book_complete=true, got %+v", result)
	}
	if result["phase"] != string(domain.PhaseComplete) {
		t.Fatalf("expected phase=complete, got %v", result["phase"])
	}
	progress, _ := s.Progress.Load()
	if progress.Phase != domain.PhaseComplete {
		t.Fatalf("expected progress.Phase=complete, got %s", progress.Phase)
	}

	// 完结判定的理由必须进裁定审计（事实快照取判定时刻）
	recs, _ := s.Decisions.Recent(1)
	if len(recs) != 1 || recs[0].Kind != "volume_end" || recs[0].Decider != "architect" {
		t.Fatalf("complete_book 应落一条 volume_end 裁定审计, got %+v", recs)
	}
	if recs[0].Reason == "" || !strings.Contains(string(recs[0].Decision), `"complete_book"`) {
		t.Fatalf("审计记录应含 reason 与 action, got %+v", recs[0])
	}
	if !strings.Contains(string(recs[0].Facts), `"completed_chapters":2`) {
		t.Fatalf("审计 facts 应含判定时刻进度, got %s", recs[0].Facts)
	}
}

// TestSaveFoundationCompleteBookRejectsZeroChapters 复现真实事故:规划刚落盘
// phase 自动翻到 writing,弱模型顺手误调 complete_book——一章未写必须拒绝,
// 否则整本书被跳过(0/68 章标记完本)。
func TestSaveFoundationCompleteBookRejectsZeroChapters(t *testing.T) {
	s := completeBookSetup(t)
	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type": "complete_book", "content": map[string]any{},
		"reason": "测试理由",
	})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("一章未写的 complete_book 必须被拒")
	}
	progress, _ := s.Progress.Load()
	if progress.Phase != domain.PhaseWriting {
		t.Fatalf("phase 应保持 writing, got %s", progress.Phase)
	}
}

// TestSaveFoundationCompleteBookRejectsUnwrittenChapters 大纲内还有未写章节时
// 不可完本(提前收束的正规路径是 final 收官卷)。
func TestSaveFoundationCompleteBookRejectsUnwrittenChapters(t *testing.T) {
	s := completeBookSetup(t)
	if err := s.Progress.MarkChapterComplete(1, 3000, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type": "complete_book", "content": map[string]any{},
		"reason": "测试理由",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("大纲内有未写章节的 complete_book 必须被拒")
	}
	if !strings.Contains(err.Error(), "final") {
		t.Fatalf("拒绝文案应引导 final 收官卷路径, got %v", err)
	}
	progress, _ := s.Progress.Load()
	if progress.Phase != domain.PhaseWriting {
		t.Fatalf("phase 应保持 writing, got %s", progress.Phase)
	}
}

func TestSaveFoundationCompleteBookRejectsBeforeWriting(t *testing.T) {
	// 规划阶段误调 complete_book 必须被拒，否则会直接跳过整本写作。
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhasePremise)
	_ = s.Progress.UpdatePhase(domain.PhaseOutline)
	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type": "complete_book", "content": map[string]any{},
		"reason": "测试理由",
	})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected error when phase != writing")
	}
	progress, _ := s.Progress.Load()
	if progress.Phase != domain.PhaseOutline {
		t.Fatalf("phase should remain outline, got %s", progress.Phase)
	}
}

// TestSaveFoundationVolumeEndRequiresReason 卷末三选一必须带判定理由——
// 它是全书最重的语义判断，理由要成为审计事实而不是散在会话日志里。
func TestSaveFoundationVolumeEndRequiresReason(t *testing.T) {
	s := completeBookSetup(t)
	tool := NewSaveFoundationTool(s)
	for _, typ := range []string{"append_volume", "complete_book"} {
		args, _ := json.Marshal(map[string]any{
			"type": typ, "content": map[string]any{},
		})
		_, err := tool.Execute(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "reason") {
			t.Fatalf("%s 缺 reason 必须被拒且文案提及 reason, got %v", typ, err)
		}
	}
	if recs, _ := s.Decisions.Recent(1); len(recs) != 0 {
		t.Fatalf("被拒调用不应产生审计记录, got %+v", recs)
	}
}

func TestSaveFoundationCompleteBookRejectsWithPendingRewrites(t *testing.T) {
	s := completeBookSetup(t)
	if err := s.Progress.MarkChapterComplete(2, 3000, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := s.Progress.SetPendingRewrites([]int{2}, "尾章节奏过快"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	tool := NewSaveFoundationTool(s)
	args, _ := json.Marshal(map[string]any{
		"type": "complete_book", "content": map[string]any{},
		"reason": "测试理由",
	})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected error when PendingRewrites non-empty")
	}
	progress, _ := s.Progress.Load()
	if progress.Phase == domain.PhaseComplete {
		t.Fatalf("phase should not be Complete with PendingRewrites: %s", progress.Phase)
	}
}

// TestSaveFoundationExpandArcValidatesScenes 验证 expand_arc 对场景节拍的校验。
func TestSaveFoundationExpandArcValidatesScenes(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	// 1. 完整 object scenes → 成功
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"title": "裂盟之后",
			"goal":  "让分裂后的双方以不同选择推进同一主线",
			"chapters": []map[string]any{{
				"title": "各走一边", "core_event": "追索", "hook": "意外重合",
				"scenes": []map[string]any{
					{"goal": "分道", "action": "主角独自上路", "conflict": "遭遇追兵", "outcome": "逃脱"},
					{"goal": "追索", "action": "寻找线索", "conflict": "线索中断", "outcome": "发现新方向"},
				},
			}},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("完整 object scenes 应成功: %v", err)
	}

	// 2. 缺 goal 的 object scene → 拒绝
	s2 := store.NewStore(dir) // 复用同一目录重新加载
	s2.Progress.Init("test", 5)
	expansionWithMissing := map[string]any{
		"title": "裂盟之后",
		"goal":  "让分裂后的双方以不同选择推进同一主线",
		"chapters": []map[string]any{{
			"title": "各走一边", "core_event": "追索", "hook": "意外重合",
			"scenes": []map[string]any{
				{"action": "主角独自上路", "conflict": "遭遇追兵", "outcome": "逃脱"}, // 缺 goal
			},
		}},
	}
	// 需要先恢复骨架弧状态（前一个 expand_arc 已展开）
	if err := s2.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	tool2 := NewSaveFoundationTool(s2)
	args2, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": expansionWithMissing,
	})
	_, err := tool2.Execute(context.Background(), args2)
	if err == nil {
		t.Fatal("缺 goal 的 object scene 应拒绝")
	}
	if !strings.Contains(err.Error(), "goal: required") {
		t.Fatalf("应提示 goal: required，实际: %v", err)
	}

	// 3. 旧 string scenes 兼容通过
	s3 := store.NewStore(dir)
	s3.Progress.Init("test", 5)
	if err := s3.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	tool3 := NewSaveFoundationTool(s3)
	args3, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"title": "裂盟之后",
			"goal":  "让分裂后的双方以不同选择推进同一主线",
			"chapters": []map[string]any{{
				"title": "各走一边", "core_event": "追索", "hook": "意外重合",
				"scenes": []string{"分道", "追索"}, // 旧 string 格式
			}},
		},
	})
	if _, err := tool3.Execute(context.Background(), args3); err != nil {
		t.Fatalf("旧 string scenes 应兼容通过: %v", err)
	}

	// 4. action-only object 不是 legacy，应拒绝
	s4 := store.NewStore(dir)
	s4.Progress.Init("test", 5)
	if err := s4.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	tool4 := NewSaveFoundationTool(s4)
	args4, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"title": "裂盟之后",
			"goal":  "让分裂后的双方以不同选择推进同一主线",
			"chapters": []map[string]any{{
				"title": "各走一边", "core_event": "追索", "hook": "意外重合",
				"scenes": []map[string]any{
					{"action": "只有行动没有其它字段"},
				},
			}},
		},
	})
	if _, err := tool4.Execute(context.Background(), args4); err == nil {
		t.Fatal("action-only object scene 应拒绝")
	} else if !strings.Contains(err.Error(), "goal: required") {
		t.Fatalf("应提示 goal: required，实际: %v", err)
	}
}

// TestSaveFoundationAppendVolumeValidatesScenes 验证 append_volume 对场景节拍的校验。
func TestSaveFoundationAppendVolumeValidatesScenes(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 初始 cluster
	if err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "首弧", Goal: "目标",
			Chapters: []domain.OutlineEntry{{Title: "第一章", CoreEvent: "开局"}},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	// 1. 完整 object scenes → 成功
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "需要新卷",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "升级",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{{
					"title": "新章", "core_event": "推进",
					"scenes": []map[string]any{
						{"goal": "探索", "action": "进入新区域", "conflict": "遇到怪物", "outcome": "击败怪物"},
					},
				}},
			}},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("完整 object scenes 应成功: %v", err)
	}

	// 2. 旧 string scenes 兼容通过
	s2 := store.NewStore(dir)
	s2.Progress.Init("test", 0)
	if err := s2.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}
	s2.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "首弧", Goal: "目标",
			Chapters: []domain.OutlineEntry{{Title: "第一章", CoreEvent: "开局"}},
		}},
	}})
	tool2 := NewSaveFoundationTool(s2)
	args2, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "测试兼容",
		"content": map[string]any{
			"index": 3, "title": "第三卷", "theme": "过渡",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧", "goal": "g",
				"chapters": []map[string]any{{
					"title": "章", "core_event": "e",
					"scenes": []string{"旧格式场景"},
				}},
			}},
		},
	})
	if _, err := tool2.Execute(context.Background(), args2); err != nil {
		t.Fatalf("旧 string scenes 应兼容通过: %v", err)
	}
}

// TestSaveFoundationOutlineRejectsIncompleteObject 验证 outline 初始保存拒绝残缺 object scene。
func TestSaveFoundationOutlineRejectsIncompleteObject(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	// 残缺 object（缺少 conflict）
	args, _ := json.Marshal(map[string]any{
		"type": "outline",
		"content": []map[string]any{
			{
				"chapter": 1, "title": "第一章", "core_event": "开场", "hook": "钩子",
				"scenes": []map[string]any{
					{"goal": "g", "action": "a", "outcome": "o"}, // 缺 conflict
				},
			},
		},
		"scale": "short",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("outline 初始保存应拒绝残缺 object scene")
	}
	if !strings.Contains(err.Error(), "conflict: required") {
		t.Fatalf("应提示 conflict: required，实际: %v", err)
	}
}

// TestSaveFoundationOutlineAcceptsLegacyString 验证 outline 初始保存接受 legacy string scenes。
func TestSaveFoundationOutlineAcceptsLegacyString(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	args, _ := json.Marshal(map[string]any{
		"type": "outline",
		"content": []map[string]any{
			{
				"chapter": 1, "title": "第一章", "core_event": "开场", "hook": "钩子",
				"scenes": []string{"旧格式场景一", "旧格式场景二"},
			},
		},
		"scale": "short",
	})
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("outline 初始保存应接受 legacy string scenes: %v", err)
	}
}

// TestSaveFoundationLayeredOutlineRejectsIncompleteObject 验证 layered_outline 初始保存拒绝残缺 object scene。
func TestSaveFoundationLayeredOutlineRejectsIncompleteObject(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	// 残缺 object 场景（缺 goal）
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "起步",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "弧一", "goal": "目标",
						"chapters": []map[string]any{
							{
								"title": "第一章", "core_event": "开场",
								"scenes": []map[string]any{
									{"action": "只有 action"}, // 缺 goal
								},
							},
						},
					},
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("layered_outline 初始保存应拒绝残缺 object scene")
	}
	if !strings.Contains(err.Error(), "goal: required") {
		t.Fatalf("应提示 goal: required，实际: %v", err)
	}
}

// TestSaveFoundationLayeredOutlineAcceptsLegacyString 验证 layered_outline 初始保存接受 legacy string scenes。
func TestSaveFoundationLayeredOutlineAcceptsLegacyString(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewSaveFoundationTool(s)

	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "起步",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "弧一", "goal": "目标",
						"chapters": []map[string]any{
							{
								"title": "第一章", "core_event": "开场",
								"scenes": []string{"旧格式场景"},
							},
						},
					},
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("layered_outline 初始保存应接受 legacy string scenes: %v", err)
	}
}
