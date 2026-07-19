package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

// testContract 是测试用的 Core4 契约（向后兼容现有测试）。
var testContract = projectprofile.NewCore4Contract()

func setTestLongApproval(tool *SaveFoundationTool, answer string, timeout time.Duration) {
	ask := NewAskUserTool()
	ask.EnableTUILongApproval()
	ask.SetHandler(func(_ context.Context, questions []Question) (*AskUserResponse, error) {
		answers := make(map[string]string, len(questions))
		for _, q := range questions {
			answers[q.Question] = answer
		}
		return &AskUserResponse{Answers: answers, Notes: map[string]string{}}, nil
	})
	tool.SetLongApproval(ask, timeout)
}

func TestSaveFoundationPersistsPlanningTier(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveFoundationTool(store, testContract)
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

	tool := NewSaveFoundationTool(store, testContract)
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

	tool := NewSaveFoundationTool(store, testContract)

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

	tool := NewSaveFoundationTool(s, testContract)

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

	tool := NewSaveFoundationTool(s, testContract)
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

	tool := NewSaveFoundationTool(s, testContract)

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

	tool := NewSaveFoundationTool(s, testContract)
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

	tool := NewSaveFoundationTool(s, testContract)
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

	tool := NewSaveFoundationTool(s, testContract)
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

	tool := NewSaveFoundationTool(s, testContract)
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
	tool := NewSaveFoundationTool(s, testContract)
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
	setTestLongApproval(tool, "批准", time.Second)
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

func TestSaveFoundationLongProposalRejectedKeepsLongAndContinues(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress.Save(&domain.Progress{
		Phase: domain.PhaseWriting, CompletedChapters: []int{1, 2, 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{
		EndingDirection: "既定终局", OpenThreads: []string{"长线A"}, EstimatedScale: "5卷", LastUpdated: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	tool := NewSaveFoundationTool(s, testContract)
	setTestLongApproval(tool, "拒绝", time.Second)
	longRaw, _ := json.Marshal(map[string]any{
		"type": "update_compass", "reason": "阶段复盘误判",
		"content": map[string]any{
			"long":    map[string]any{"ending_direction": "错误的新终局", "open_threads": []string{"长线B"}},
			"current": map[string]any{"direction": "下一阶段追查", "open_threads": []string{"短线X"}},
		},
	})
	resultRaw, err := tool.Execute(context.Background(), longRaw)
	if err != nil {
		t.Fatalf("rejected proposal should be a normal result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatal(err)
	}
	if result["long_approval"] != "rejected" || result["continued"] != true || result["saved"] != true || result["current_saved"] != true {
		t.Fatalf("unexpected rejection result: %s", resultRaw)
	}
	got, err := s.Outline.LoadCompass()
	if err != nil {
		t.Fatal(err)
	}
	if got.Long.EndingDirection != "既定终局" || got.Long.EstimatedScale != "5卷" || strings.Join(got.Long.OpenThreads, "|") != "长线A" {
		t.Fatalf("rejected proposal changed long compass: %+v", got.Long)
	}
	if got.Current == nil || got.Current.Direction != "下一阶段追查" {
		t.Fatalf("rejected long proposal should still save current: %+v", got.Current)
	}
}

func TestSaveFoundationLongProposalTimeoutRejectsAndContinues(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	before := domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "既定终局", EstimatedScale: "5卷", LastUpdated: 8}}
	if err := s.Outline.SaveCompass(before); err != nil {
		t.Fatal(err)
	}
	ask := NewAskUserTool()
	ask.EnableTUILongApproval()
	ask.SetHandler(func(ctx context.Context, _ []Question) (*AskUserResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	tool := NewSaveFoundationTool(s, testContract)
	tool.SetLongApproval(ask, 25*time.Millisecond)
	raw, _ := json.Marshal(map[string]any{
		"type": "update_compass", "section": "long", "reason": "申请扩篇",
		"content": map[string]any{"estimated_scale": "8卷"},
	})
	started := time.Now()
	resultRaw, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("timeout should reject without failing the tool: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("test timeout took too long: %v", elapsed)
	}
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatal(err)
	}
	if result["long_approval"] != "timeout" || result["continued"] != true {
		t.Fatalf("unexpected timeout result: %s", resultRaw)
	}
	got, _ := s.Outline.LoadCompass()
	if !reflect.DeepEqual(got.Long, before.Long) {
		t.Fatalf("timeout changed long compass: before=%+v got=%+v", before.Long, got.Long)
	}
	if cp := s.Checkpoints.LatestByStep(domain.GlobalScope(), "update_compass_rejected"); cp == nil {
		t.Fatal("rejected long proposal must satisfy Architect StopGuard without retrying")
	}
}

func TestSaveFoundationLongProposalWithoutTUIRejectsImmediately(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	before := domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "既定终局", EstimatedScale: "5卷"}}
	if err := s.Outline.SaveCompass(before); err != nil {
		t.Fatal(err)
	}
	tool := NewSaveFoundationTool(s, testContract)
	raw, _ := json.Marshal(map[string]any{
		"type": "update_compass", "section": "long", "reason": "无人值守申请",
		"content": map[string]any{"estimated_scale": "8卷"},
	})
	resultRaw, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("missing TUI should reject without failing: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatal(err)
	}
	if result["long_approval"] != "unavailable" || result["continued"] != true {
		t.Fatalf("unexpected unavailable result: %s", resultRaw)
	}
	got, _ := s.Outline.LoadCompass()
	if !reflect.DeepEqual(got.Long, before.Long) {
		t.Fatalf("headless update changed long compass: before=%+v got=%+v", before.Long, got.Long)
	}
}

func TestSaveFoundationApprovedLongProposalRejectsStaleBase(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	base := domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "原终局", EstimatedScale: "5卷"}}
	if err := s.Outline.SaveCompass(base); err != nil {
		t.Fatal(err)
	}
	ask := NewAskUserTool()
	ask.EnableTUILongApproval()
	ask.SetHandler(func(_ context.Context, questions []Question) (*AskUserResponse, error) {
		// 模拟审批等待期间，用户在别处已经更新 long；旧提案即使获批也不能覆盖。
		newer := domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "用户的新终局", EstimatedScale: "6卷"}}
		if err := s.Outline.SaveCompass(newer); err != nil {
			return nil, err
		}
		return &AskUserResponse{Answers: map[string]string{questions[0].Question: "批准"}}, nil
	})
	tool := NewSaveFoundationTool(s, testContract)
	tool.SetLongApproval(ask, time.Second)
	raw, _ := json.Marshal(map[string]any{
		"type": "update_compass", "section": "long", "reason": "旧上下文提案",
		"content": map[string]any{"estimated_scale": "8卷"},
	})
	resultRaw, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("stale proposal should reject without failing: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatal(err)
	}
	if result["long_approval"] != "stale" || result["continued"] != true {
		t.Fatalf("unexpected stale result: %s", resultRaw)
	}
	got, _ := s.Outline.LoadCompass()
	if got.Long.EndingDirection != "用户的新终局" || got.Long.EstimatedScale != "6卷" {
		t.Fatalf("stale proposal overwrote newer long: %+v", got.Long)
	}
}

func TestSaveFoundationAcceptsDirectJSONArrayContent(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	tool := NewSaveFoundationTool(store, testContract)
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
	tool := NewSaveFoundationTool(s, testContract)
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
	tool := NewSaveFoundationTool(s, testContract)
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
	tool := NewSaveFoundationTool(s, testContract)
	args, _ := json.Marshal(map[string]any{
		"type": "complete_book", "content": map[string]any{},
		"reason": "测试理由",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("大纲内有未写章节的 complete_book 必须被拒")
	}
	if !strings.Contains(err.Error(), "未写章节") {
		t.Fatalf("应提示大纲内还有未写章节, got %v", err)
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
	tool := NewSaveFoundationTool(s, testContract)
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
	tool := NewSaveFoundationTool(s, testContract)
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
	tool := NewSaveFoundationTool(s, testContract)
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

	tool := NewSaveFoundationTool(s, testContract)

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
	tool2 := NewSaveFoundationTool(s2, testContract)
	args2, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": expansionWithMissing,
	})
	_, err := tool2.Execute(context.Background(), args2)
	if err == nil {
		t.Fatal("缺 goal 的 object scene 应拒绝")
	}
	if !strings.Contains(err.Error(), "goal is required") {
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
	tool3 := NewSaveFoundationTool(s3, testContract)
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
	tool4 := NewSaveFoundationTool(s4, testContract)
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
	} else if !strings.Contains(err.Error(), "goal is required") {
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

	tool := NewSaveFoundationTool(s, testContract)

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
	tool2 := NewSaveFoundationTool(s2, testContract)
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

	tool := NewSaveFoundationTool(s, testContract)

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
	if !strings.Contains(err.Error(), "conflict is required") {
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

	tool := NewSaveFoundationTool(s, testContract)

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

	tool := NewSaveFoundationTool(s, testContract)

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
	if !strings.Contains(err.Error(), "goal is required") {
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

	tool := NewSaveFoundationTool(s, testContract)

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

// anyOfBranch 是 content["anyOf"] 中对应 type 的预期索引。
// 保持与 Schema() 中 anyOf 分支顺序一致。
const (
	anyOfPremise        = 0
	anyOfOutline        = 1
	anyOfLayeredOutline = 2
	anyOfExpandArc      = 3
	anyOfAppendVolume   = 4
	anyOfLooseArray     = 5
	anyOfLooseObject    = 6
)

// getAnyOf 从 schema 的 content.anyOf 中返回第 i 个 branch。
func getAnyOf(schema map[string]any, i int) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	content, _ := props["content"].(map[string]any)
	anyOf, _ := content["anyOf"].([]any)
	if i < 0 || i >= len(anyOf) {
		return nil
	}
	branch, _ := anyOf[i].(map[string]any)
	return branch
}

// getNestedProp 沿 keys 路径递归获取 map 值。
func getNestedProp(m map[string]any, keys ...string) map[string]any {
	if m == nil {
		return nil
	}
	for _, k := range keys[:len(keys)-1] {
		v, ok := m[k]
		if !ok {
			return nil
		}
		m, ok = v.(map[string]any)
		if !ok {
			return nil
		}
	}
	v, ok := m[keys[len(keys)-1]]
	if !ok {
		return nil
	}
	out, _ := v.(map[string]any)
	return out
}

// TestSaveFoundationSchemaV3 验证 v3 契约下顶层 typed anyOf schema 包含全部约束。
func TestSaveFoundationSchemaV3(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	v3Contract := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3Contract)
	s := tool.Schema()

	// 顶层必须为 anyOf（不是 properties.type+properties.content）
	anyOf, ok := s["anyOf"].([]any)
	if !ok {
		t.Fatal("v3 schema top-level must be anyOf, got properties")
	}
	if len(anyOf) == 0 {
		t.Fatal("anyOf is empty")
	}

	// 验证每个分支都是完整参数对象 (type const, content, additionalProperties:false)
	for _, br := range anyOf {
		b, _ := br.(map[string]any)
		if b == nil {
			t.Errorf("anyOf branch is not an object")
			continue
		}
		if typ, _ := b["type"].(string); typ != "object" {
			t.Errorf("anyOf branch type = %q, want object", typ)
		}
		if ap, _ := b["additionalProperties"].(bool); ap != false {
			t.Errorf("anyOf branch should have additionalProperties: false")
		}
		props, _ := b["properties"].(map[string]any)
		if props == nil {
			t.Errorf("anyOf branch missing properties")
			continue
		}
		typeField, _ := props["type"].(map[string]any)
		if typeField == nil {
			t.Errorf("anyOf branch missing type discriminator")
			continue
		}
		if et, _ := typeField["type"].(string); et != "string" {
			t.Errorf("type discriminator should be string, got %q", et)
		}
		var typeName string
		if enum, ok := typeField["enum"].([]any); ok && len(enum) == 1 {
			typeName, _ = enum[0].(string)
		} else if es, ok := typeField["enum"].([]string); ok && len(es) == 1 {
			typeName = es[0]
		}
		if typeName == "" {
			t.Errorf("type discriminator enum not found or empty")
			continue
		}
		// validate required contains at least "type" and "content"
		hasType := false
		hasContent := false
		if req, ok := b["required"].([]any); ok {
			for _, r := range req {
				if rs, _ := r.(string); rs == "type" {
					hasType = true
				}
				if rs, _ := r.(string); rs == "content" {
					hasContent = true
				}
			}
		} else if reqs, ok := b["required"].([]string); ok {
			for _, r := range reqs {
				if r == "type" {
					hasType = true
				}
				if r == "content" {
					hasContent = true
				}
			}
		}
		if !hasType || !hasContent {
			t.Errorf("branch %q required should include type and content", typeName)
		}
	}

	// 验证 outline branch scenes items 含 erotic_charge
	for _, br := range anyOf {
		b, _ := br.(map[string]any)
		if b == nil {
			continue
		}
		props, _ := b["properties"].(map[string]any)
		if props == nil {
			continue
		}
		typeField, _ := props["type"].(map[string]any)
		enum, _ := typeField["enum"].([]any)
		if len(enum) != 1 || enum[0] != "outline" {
			continue
		}
		contentSchema, _ := props["content"].(map[string]any)
		if contentSchema == nil {
			continue
		}
		items, _ := contentSchema["items"].(map[string]any)
		if items == nil {
			continue
		}
		scenes, _ := items["properties"].(map[string]any)["scenes"].(map[string]any)
		if scenes == nil {
			continue
		}
		sceneItem, _ := scenes["items"].(map[string]any)
		if sceneItem == nil {
			continue
		}
		sceneProps, _ := sceneItem["properties"].(map[string]any)
		if sceneProps == nil {
			t.Fatal("scene item properties not found")
		}
		for _, f := range []string{"goal", "action", "conflict", "outcome", "sensory_anchor", "body_reaction", "emotion_reaction", "erotic_charge"} {
			if _, ok := sceneProps[f]; !ok {
				t.Errorf("v3 scene schema missing field: %s", f)
			}
		}
		reqCount := 0
		if req, ok := sceneItem["required"].([]any); ok {
			reqCount = len(req)
		} else if reqs, ok := sceneItem["required"].([]string); ok {
			reqCount = len(reqs)
		}
		if reqCount != 7 {
			t.Errorf("v3 scene required count = %d, want 7", reqCount)
		}
		if ap, _ := sceneItem["additionalProperties"].(bool); ap != false {
			t.Error("v3 scene schema should have additionalProperties: false")
		}
		break
	}
}

// TestSaveFoundationSchemaBranches 验证 anyOf 各分支的 type 和基本结构。
func TestSaveFoundationSchemaBranches(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	st.Init()
	tool := NewSaveFoundationTool(st, testContract)
	s := tool.Schema()

	// premise: string
	if typ, _ := getAnyOf(s, anyOfPremise)["type"].(string); typ != "string" {
		t.Errorf("premise branch type = %q, want string", typ)
	}

	// outline: array with items.properties.chapter/title/core_event/scenes
	outlineItems := getNestedProp(getAnyOf(s, anyOfOutline), "items")
	if typ, _ := outlineItems["type"].(string); typ != "object" {
		t.Errorf("outline items type = %q, want object", typ)
	}
	for _, f := range []string{"chapter", "title", "core_event", "hook", "scenes"} {
		if v := getNestedProp(outlineItems, "properties", f); v == nil && f != "hook" {
			t.Errorf("outline items.properties 缺少字段 %q", f)
		}
	}

	// layered_outline: array with items.properties.index/title/theme/arcs
	loItems := getNestedProp(getAnyOf(s, anyOfLayeredOutline), "items")
	if typ, _ := loItems["type"].(string); typ != "object" {
		t.Errorf("layered_outline items type = %q, want object", typ)
	}

	// expand_arc: object with properties.title/goal/chapters
	ea := getAnyOf(s, anyOfExpandArc)
	if typ, _ := ea["type"].(string); typ != "object" {
		t.Errorf("expand_arc type = %q, want object", typ)
	}

	// loose array branch
	la := getAnyOf(s, anyOfLooseArray)
	if typ, _ := la["type"].(string); typ != "array" {
		t.Errorf("loose array branch type = %q, want array", typ)
	}

	// loose object branch
	lo := getAnyOf(s, anyOfLooseObject)
	if typ, _ := lo["type"].(string); typ != "object" {
		t.Errorf("loose object branch type = %q, want object", typ)
	}
}

// TestSaveFoundationOutlineWithBodyEmotionReaction 验证 body_reaction / emotion_reaction
// 通过 save_foundation 工具落盘后正确持久化。同时验证 legacy string scenes 不受影响。
func TestSaveFoundationOutlineWithBodyEmotionReaction(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(s, testContract)

	// 场景 1：完整结构化 scene（含 body_reaction / emotion_reaction）
	// 场景 2：legacy string（验证兼容）
	args, _ := json.Marshal(map[string]any{
		"type": "outline",
		"content": []map[string]any{
			{
				"chapter": 1, "title": "第一章", "core_event": "开场", "hook": "悬念",
				"scenes": []map[string]any{
					{
						"goal":             "揭示真相",
						"action":           "质问对方",
						"conflict":         "对方否认",
						"outcome":          "发现新线索",
						"sensory_anchor":   "昏暗的灯光下，茶杯冒着热气",
						"body_reaction":    "握紧拳头，额角冒出冷汗",
						"emotion_reaction": "难以置信，继而转为愤怒",
					},
					{
						"goal":             "收集证据",
						"action":           "翻查档案",
						"conflict":         "档案室上锁",
						"outcome":          "找到突破口",
						"body_reaction":    "手指微微发抖",
						"emotion_reaction": "既兴奋又紧张",
					},
				},
			},
			{
				"chapter": 2, "title": "第二章", "core_event": "发展", "hook": "钩子",
				"scenes": []string{"遗留格式场景一", "遗留格式场景二"},
			},
		},
		"scale": "short",
	})
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute outline with body/emotion: %v", err)
	}

	// 从 store 读取大纲
	entries, err := s.Outline.LoadOutline()
	if err != nil {
		t.Fatalf("LoadOutline: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// 第一章：结构化 scenes
	ch1 := entries[0]
	if len(ch1.Scenes) != 2 {
		t.Fatalf("chapter 1: expected 2 scenes, got %d", len(ch1.Scenes))
	}
	sc0 := ch1.Scenes[0]
	if sc0.Goal != "揭示真相" {
		t.Errorf("sc0.Goal = %q", sc0.Goal)
	}
	if sc0.Action != "质问对方" {
		t.Errorf("sc0.Action = %q", sc0.Action)
	}
	if sc0.Conflict != "对方否认" {
		t.Errorf("sc0.Conflict = %q", sc0.Conflict)
	}
	if sc0.Outcome != "发现新线索" {
		t.Errorf("sc0.Outcome = %q", sc0.Outcome)
	}
	if sc0.SensoryAnchor != "昏暗的灯光下，茶杯冒着热气" {
		t.Errorf("sc0.SensoryAnchor = %q", sc0.SensoryAnchor)
	}
	if sc0.BodyReaction != "握紧拳头，额角冒出冷汗" {
		t.Errorf("sc0.BodyReaction = %q", sc0.BodyReaction)
	}
	if sc0.EmotionReaction != "难以置信，继而转为愤怒" {
		t.Errorf("sc0.EmotionReaction = %q", sc0.EmotionReaction)
	}
	if sc0.IsLegacy() {
		t.Errorf("结构化 scene 不应是 legacy")
	}

	// 场景 2：仅有必填字段 + body_reaction / emotion_reaction
	sc1 := ch1.Scenes[1]
	if sc1.BodyReaction != "手指微微发抖" {
		t.Errorf("sc1.BodyReaction = %q", sc1.BodyReaction)
	}
	if sc1.EmotionReaction != "既兴奋又紧张" {
		t.Errorf("sc1.EmotionReaction = %q", sc1.EmotionReaction)
	}
	if sc1.IsLegacy() {
		t.Errorf("场景 2 不应是 legacy")
	}

	// 第二章：legacy string scenes（兼容不受影响）
	ch2 := entries[1]
	if len(ch2.Scenes) != 2 {
		t.Fatalf("chapter 2: expected 2 scenes, got %d", len(ch2.Scenes))
	}
	if !ch2.Scenes[0].IsLegacy() {
		t.Errorf("string scene 应标记为 legacy")
	}
	if ch2.Scenes[0].Action != "遗留格式场景一" {
		t.Errorf("legacy scene Action = %q", ch2.Scenes[0].Action)
	}
	if ch2.Scenes[1].Action != "遗留格式场景二" {
		t.Errorf("legacy scene Action = %q", ch2.Scenes[1].Action)
	}
	// legacy 场景的 body/emotion 应为空
	if ch2.Scenes[0].BodyReaction != "" {
		t.Errorf("legacy scene BodyReaction 应为空, got %q", ch2.Scenes[0].BodyReaction)
	}
	if ch2.Scenes[0].EmotionReaction != "" {
		t.Errorf("legacy scene EmotionReaction 应为空, got %q", ch2.Scenes[0].EmotionReaction)
	}
}

// ── Snapshot helpers (store invariant) ──

type storeSnapshot map[string]string

func takeStoreSnapshot(dir string) storeSnapshot {
	snap := make(storeSnapshot)
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		h := sha256.Sum256(data)
		snap[filepath.ToSlash(rel)] = fmt.Sprintf("%x", h)
		return nil
	})
	return snap
}

func assertSnapshotUnchanged(t *testing.T, before, after storeSnapshot) {
	t.Helper()
	for path, hash := range before {
		if after[path] != hash {
			t.Errorf("file %q changed: before=%s after=%s", path, hash, after[path])
		}
	}
	if len(after) > len(before) {
		for path := range after {
			if _, ok := before[path]; !ok {
				t.Errorf("new file created: %s", path)
			}
		}
	}
}

// TestSaveFoundationV3_EmptyOutlineRejected 验证 V3 契约下空 outline 被拒且 snapshot 不变。
func TestSaveFoundationV3_EmptyOutlineRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type":    "outline",
		"content": []any{},
		"scale":   "short",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 empty outline should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_EmptyLayeredOutlineRejected 验证 V3 契约下空 layered_outline 被拒。
func TestSaveFoundationV3_EmptyLayeredOutlineRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type":    "layered_outline",
		"content": []any{},
		"scale":   "long",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 empty layered_outline should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_ExpandArcEmptyChaptersRejected 验证 V3 下 expand_arc 空 chapters 被拒。
func TestSaveFoundationV3_ExpandArcEmptyChaptersRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"title":    "新标题",
			"goal":     "新目标",
			"chapters": []any{},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 expand_arc empty chapters should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestSaveFoundationV3_ExpandArcAcceptsStringVolumeArc(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{v3SnapshotChapter(1, "old")}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 1},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": "1", "arc": "2",
		"content": domain.ArcExpansion{
			Title: "新标题", Goal: "新目标",
			Chapters: []domain.OutlineEntry{v3SnapshotChapter(2, "new")},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute expand_arc with string volume/arc: %v", err)
	}
	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	got := volumes[0].Arcs[1]
	if got.Title != "新标题" || got.Goal != "新目标" || len(got.Chapters) != 1 {
		t.Fatalf("unexpected expanded arc: %+v", got)
	}
}

func TestSaveFoundationV3_ExpandArcNormalizesMissingChapterNumber(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "旧标题", Goal: "旧目标", EstimatedChapters: 1},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	bad := v3SnapshotChapter(0, "zero")
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 1,
		"content": domain.ArcExpansion{
			Title:    "新标题",
			Goal:     "新目标",
			Chapters: []domain.OutlineEntry{bad},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute expand_arc with missing chapter number: %v", err)
	}
	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if got := volumes[0].Arcs[0].Chapters[0].Chapter; got != 1 {
		t.Fatalf("chapter number should be normalized, got %d", got)
	}
}

// TestSaveFoundationV3_ExpandArcMissingTitleRejected 验证 V3 下 expand_arc 缺 title 被拒。
func TestSaveFoundationV3_ExpandArcMissingTitleRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"goal":     "新目标",
			"chapters": []map[string]any{{"title": "章", "core_event": "事件"}},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 expand_arc missing title should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_ExpandArcMissingGoalRejected 验证 V3 下 expand_arc 缺 goal 被拒。
func TestSaveFoundationV3_ExpandArcMissingGoalRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "选择",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "已完成弧", Goal: "建立同盟", Chapters: []domain.OutlineEntry{{Title: "分裂", CoreEvent: "同盟意外破裂"}}},
			{Index: 2, Title: "旧标题", Goal: "维持同盟", EstimatedChapters: 4},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2,
		"content": map[string]any{
			"title":    "新标题",
			"chapters": []map[string]any{{"title": "章", "core_event": "事件"}},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 expand_arc missing goal should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_AppendVolumeEmptyArcsRejected 验证 V3 下 append_volume 空 arcs 被拒。
func TestSaveFoundationV3_AppendVolumeEmptyArcsRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "起步",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "首弧", Goal: "目标",
			Chapters: []domain.OutlineEntry{{Title: "第一章", CoreEvent: "开局"}},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "测试",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "升级",
			"arcs": []any{},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 append_volume empty arcs should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_CompleteBook_NotExactObjectRejected 验证 V3 下 complete_book content 不是 {} 被拒。
func TestSaveFoundationV3_CompleteBook_NotExactObjectRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 2); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	for ch := 1; ch <= 2; ch++ {
		if err := st.Progress.MarkChapterComplete(ch, 3000, "", ""); err != nil {
			t.Fatalf("MarkChapterComplete(%d): %v", ch, err)
		}
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())

	// 测试 content 不是 {} 的各种情况
	// 说明: content 为 "{}" (JSON string) 时, normalizeFoundationContent 会
	// 将其视为字符串并解出 "{}" → 等价于直接传 {} 对象。因此不在此测试中。
	tests := []struct {
		name    string
		content any
	}{
		{"array", []any{}},
		{"object_with_fields", map[string]any{"extra": "field"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := takeStoreSnapshot(dir)
			args, _ := json.Marshal(map[string]any{
				"type": "complete_book", "content": tc.content,
				"reason": "测试理由",
			})
			_, err := tool.Execute(context.Background(), args)
			if err == nil {
				t.Fatalf("complete_book with content=%v should be rejected", tc.content)
			}
			assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
		})
	}
}

// TestSaveFoundationV3_AppendVolume_NoReasonRejected 验证 V3 下 append_volume 无 reason 被拒。
func TestSaveFoundationV3_AppendVolume_NoReasonRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume",
		"content": map[string]any{
			"index": 1, "title": "第一卷", "theme": "起步",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{{"title": "章", "core_event": "事件"}},
			}},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("append_volume without reason should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_CompleteBook_NoReasonRejected 验证 V3 下 complete_book 无 reason 被拒。
func TestSaveFoundationV3_CompleteBook_NoReasonRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 2); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	for ch := 1; ch <= 2; ch++ {
		if err := st.Progress.MarkChapterComplete(ch, 3000, "", ""); err != nil {
			t.Fatalf("MarkChapterComplete(%d): %v", ch, err)
		}
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type":    "complete_book",
		"content": map[string]any{},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("complete_book without reason should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_EmptyScenesRejected 验证 V3 契约下包含空 scenes 数组的 outline 被拒。
func TestSaveFoundationV3_EmptyScenesRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "outline",
		"content": []map[string]any{
			{
				"chapter": 1, "title": "第一章", "core_event": "开场",
				"scenes": []any{},
			},
		},
		"scale": "short",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("V3 outline with empty scenes array should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_Premise_NoHeaderRejected V3 下 premise 缺 # 标题被拒 snapshot 不变。
func TestSaveFoundationV3_Premise_NoHeaderRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "premise", "content": "not starting with hash",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("premise without # should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_Characters_InvalidJSON 验证 characters 解析失败 snapshot 不变。
func TestSaveFoundationV3_Characters_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "characters", "content": "not-json",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("characters with invalid JSON should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_WorldRules_InvalidJSON 验证 world_rules 解析失败 snapshot 不变。
func TestSaveFoundationV3_WorldRules_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "world_rules", "content": "not-array",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("world_rules with invalid content should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_UpdateCompass_NoLongReason 验证 update_compass long 无 reason 被拒 snapshot 不变。
func TestSaveFoundationV3_UpdateCompass_NoLongReason(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "update_compass", "section": "long",
		"content": map[string]any{"ending_direction": "终局"},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("update_compass long without reason should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_InvalidTypeRejected 验证未知 type 被拒 snapshot 不变。
func TestSaveFoundationV3_InvalidTypeRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "nonexistent", "content": "x",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("invalid type should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_MissingTypeRejected 验证缺 type 被拒 snapshot 不变。
func TestSaveFoundationV3_MissingTypeRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"content": "x",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("missing type should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// TestSaveFoundationV3_CompleteBook_StringWrappedRejected 验证 complete_book
// content 为 JSON 字符串 "{}" 被拒（必须是真正空对象）。
func TestSaveFoundationV3_CompleteBook_StringWrappedRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	// 构造 raw JSON：content 为 JSON 字符串 "{}" 而不是对象 {}
	rawJSON := []byte(`{"type":"complete_book","content":"{}","reason":"test"}`)
	_, err := tool.Execute(context.Background(), rawJSON)
	if err == nil {
		t.Fatal("complete_book with string-wrapped {} should be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// ── Real JSON Schema validation tests (jsonschema v6) ──

// compileSaveSchema 把 schema map 编译为 jsonschema.Schema。
func compileSaveSchema(t *testing.T, schemaMap map[string]any) *jsonschema.Schema {
	t.Helper()
	raw, err := json.Marshal(schemaMap)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft7) // agentcore schema uses draft-07 features
	if err := c.AddResource("save.json", doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile("save.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

// validateAgainstSchema 验证 payload 是否符合 schema。
func validateAgainstSchema(t *testing.T, sch *jsonschema.Schema, payload any) error {
	t.Helper()
	return sch.Validate(payload)
}

// TestV3Schema_ValidPayloads 验证四个 V3 路径的正常 payload 通过 schema 验证。
func TestV3Schema_ValidPayloads(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)
	sch := compileSaveSchema(t, tool.Schema())

	tests := []struct {
		name    string
		payload map[string]any
	}{
		{
			"outline",
			map[string]any{
				"type": "outline",
				"content": []any{map[string]any{
					"chapter": 1, "title": "章", "core_event": "事件",
					"scenes": []any{map[string]any{
						"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
						"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
					}},
				}},
				"scale": "short",
			},
		},
		{
			"layered_outline",
			map[string]any{
				"type": "layered_outline",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"chapters": []any{map[string]any{
							"chapter": 1, "title": "章", "core_event": "事件",
							"scenes": []any{map[string]any{
								"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
								"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
							}},
						}},
					}},
				}},
				"scale": "long",
			},
		},
		{
			"expand_arc",
			map[string]any{
				"type": "expand_arc", "volume": 1, "arc": 1,
				"content": map[string]any{
					"title": "弧标题", "goal": "弧目标",
					"chapters": []any{map[string]any{
						"chapter": 1, "title": "章", "core_event": "事件",
						"scenes": []any{map[string]any{
							"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
							"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
						}},
					}},
				},
			},
		},
		{
			"append_volume",
			map[string]any{
				"type": "append_volume", "reason": "测试",
				"content": map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"chapters": []any{map[string]any{
							"chapter": 1, "title": "章", "core_event": "事件",
							"scenes": []any{map[string]any{
								"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
								"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
							}},
						}},
					}},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAgainstSchema(t, sch, tc.payload); err != nil {
				t.Fatalf("valid payload should pass schema: %v", err)
			}
		})
	}
}

// TestV3Schema_NegativePayloads 验证四个 V3 路径的负例被 schema 拒绝。
func TestV3Schema_NegativePayloads(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)
	sch := compileSaveSchema(t, tool.Schema())

	tests := []struct {
		name    string
		payload map[string]any
	}{
		{
			"outline_empty_array",
			map[string]any{
				"type": "outline", "content": []any{}, "scale": "short",
			},
		},
		{
			"outline_scene_missing_field",
			map[string]any{
				"type": "outline",
				"content": []any{map[string]any{
					"chapter": 1, "title": "章", "core_event": "事件",
					"scenes": []any{map[string]any{
						"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
						"body_reaction": "b", "emotion_reaction": "e", // missing erotic_charge
					}},
				}},
				"scale": "short",
			},
		},
		{
			"expand_arc_missing_title",
			map[string]any{
				"type": "expand_arc", "volume": 1, "arc": 1,
				"content": map[string]any{
					"goal": "目标",
					"chapters": []any{map[string]any{
						"chapter": 1, "title": "章", "core_event": "事件",
					}},
				},
			},
		},
		{
			"expand_arc_empty_chapters",
			map[string]any{
				"type": "expand_arc", "volume": 1, "arc": 1,
				"content": map[string]any{"title": "t", "goal": "g", "chapters": []any{}},
			},
		},
		{
			"layered_outline_empty_volumes",
			map[string]any{
				"type": "layered_outline", "content": []any{}, "scale": "long",
			},
		},
		{
			"layered_outline_volume_empty_arcs",
			map[string]any{
				"type": "layered_outline",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题", "arcs": []any{},
				}},
				"scale": "long",
			},
		},
		{
			"layered_outline_arc_empty_chapters",
			map[string]any{
				"type": "layered_outline",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标", "chapters": []any{},
					}},
				}},
				"scale": "long",
			},
		},
		{
			"layered_outline_scene_missing_required_field",
			map[string]any{
				"type": "layered_outline",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"chapters": []any{map[string]any{
							"chapter": 1, "title": "章", "core_event": "事件",
							"scenes": []any{map[string]any{
								"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
								"body_reaction": "b", "emotion_reaction": "e",
								// missing erotic_charge
							}},
						}},
					}},
				}},
				"scale": "long",
			},
		},
		{
			"append_volume_empty_arcs",
			map[string]any{
				"type": "append_volume", "reason": "test",
				"content": map[string]any{
					"index": 2, "title": "卷", "theme": "主题", "arcs": []any{},
				},
			},
		},
		{
			"append_volume_arc_empty_chapters",
			map[string]any{
				"type": "append_volume", "reason": "test",
				"content": map[string]any{
					"index": 2, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标", "chapters": []any{},
					}},
				},
			},
		},
		{
			"append_volume_scene_missing_required_field",
			map[string]any{
				"type": "append_volume", "reason": "test",
				"content": map[string]any{
					"index": 2, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"chapters": []any{map[string]any{
							"chapter": 1, "title": "章", "core_event": "事件",
							"scenes": []any{map[string]any{
								"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
								"body_reaction": "b", "emotion_reaction": "e",
								// missing erotic_charge
							}},
						}},
					}},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAgainstSchema(t, sch, tc.payload); err == nil {
				t.Errorf("negative payload should be rejected by schema: %+v", tc.payload)
			}
		})
	}
}

// TestV3Schema_BypassRejected 验证无法通过额外字段或松散分支绕过 V3 schema。
func TestV3Schema_BypassRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)
	sch := compileSaveSchema(t, tool.Schema())

	tests := []struct {
		name    string
		payload map[string]any
	}{
		{
			"outline_with_extra_prop",
			map[string]any{
				"type": "outline",
				"content": []any{map[string]any{
					"chapter": 1, "title": "章", "core_event": "事件",
					"scenes": []any{map[string]any{
						"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
						"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
						"extra_field": "should_fail",
					}},
				}},
				"scale": "short",
			},
		},
		{
			"expand_arc_extra_prop",
			map[string]any{
				"type": "expand_arc", "volume": 1, "arc": 1,
				"content": map[string]any{
					"title": "t", "goal": "g",
					"extra": "bypass", // additionalProperties 应拒绝
					"chapters": []any{map[string]any{
						"chapter": 1, "title": "章", "core_event": "事件",
						"scenes": []any{map[string]any{
							"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
							"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
						}},
					}},
				},
			},
		},
		{
			"outline_legacy_string_scene_in_v3",
			map[string]any{
				"type": "outline",
				"content": []any{map[string]any{
					"chapter": 1, "title": "章", "core_event": "事件",
					"scenes": []any{"legacy string should be rejected in v3"},
				}},
				"scale": "short",
			},
		},
		{
			"layered_outline_extra_prop_in_volume",
			map[string]any{
				"type": "layered_outline",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题", "extra": "bypass",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"chapters": []any{map[string]any{
							"chapter": 1, "title": "章", "core_event": "事件",
							"scenes": []any{map[string]any{
								"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
								"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
							}},
						}},
					}},
				}},
				"scale": "long",
			},
		},
		{
			"append_volume_extra_prop",
			map[string]any{
				"type": "append_volume", "reason": "test",
				"content": map[string]any{
					"index": 2, "title": "卷", "theme": "主题", "extra": "bypass",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"chapters": []any{map[string]any{
							"chapter": 1, "title": "章", "core_event": "事件",
							"scenes": []any{map[string]any{
								"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
								"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
							}},
						}},
					}},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAgainstSchema(t, sch, tc.payload); err == nil {
				t.Errorf("bypass payload should be rejected: %+v", tc.payload)
			}
		})
	}
}

func v3SnapshotScene(action string) domain.SceneBeat {
	return domain.SceneBeat{
		Goal: "goal", Action: action, Conflict: "conflict", Outcome: "outcome",
		BodyReaction: "body", EmotionReaction: "emotion", EroticCharge: "charge",
	}
}

func v3SnapshotChapter(chapter int, action string) domain.OutlineEntry {
	return domain.OutlineEntry{
		Chapter: chapter, Title: fmt.Sprintf("第%d章", chapter), CoreEvent: "event",
		Scenes: domain.SceneList{v3SnapshotScene(action)},
	}
}

func TestSaveFoundationV3_CompleteBookNullRejectedWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 2); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"type":"complete_book","content":null,"reason":"done"
	}`))
	if err == nil {
		t.Fatal("complete_book content=null must be rejected")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestSaveFoundationV3_UnknownRootFieldRejectedWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"type":"premise","content":"# 书名","unexpected":true
	}`))
	if err == nil {
		t.Fatal("v3 full argument decode must reject unknown root fields")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestSaveFoundationV3_BranchIrrelevantFieldRejectedWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{
		"type":"premise","content":"# 书名","reason":"belongs only to another branch"
	}`))
	if err == nil {
		t.Fatal("v3 runtime must enforce the selected branch's additionalProperties=false shape")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestSaveFoundationV3_ExpandArcCorruptProgressRejectedWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "vol", Theme: "theme",
		Arcs: []domain.ArcOutline{{Index: 1, Title: "skeleton", Goal: "goal"}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{broken`), 0o644); err != nil {
		t.Fatalf("write corrupt progress: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 1, "scale": "long",
		"content": domain.ArcExpansion{Title: "expanded", Goal: "goal", Chapters: []domain.OutlineEntry{v3SnapshotChapter(1, "new")}},
	})

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expand_arc must fail closed when progress is unreadable")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestSaveFoundationV3_ExpandArcConflictRejectedBeforePlanningTierWrite(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "vol", Theme: "theme",
		Arcs: []domain.ArcOutline{{
			Index: 1, Title: "already", Goal: "old goal",
			Chapters: []domain.OutlineEntry{v3SnapshotChapter(1, "old")},
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 1, "scale": "long",
		"content": domain.ArcExpansion{Title: "different", Goal: "new goal", Chapters: []domain.OutlineEntry{v3SnapshotChapter(1, "new")}},
	})

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expand_arc must reject a conflicting already-expanded target")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func TestSaveFoundationV3_AppendVolumeUnreadableStateRejectedWithoutWrites(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{
			name: "progress",
			corrupt: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte(`{broken`), 0o644); err != nil {
					t.Fatalf("write corrupt progress: %v", err)
				}
			},
		},
		{
			name: "layered_outline",
			corrupt: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "layered_outline.json"), []byte(`{broken`), 0o644); err != nil {
					t.Fatalf("write corrupt layered outline: %v", err)
				}
			},
		},
		{
			name: "missing_layered_outline",
			corrupt: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(dir, "layered_outline.json")); err != nil {
					t.Fatalf("remove layered outline: %v", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := store.NewStore(dir)
			if err := st.Init(); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := st.Progress.Init("test", 1); err != nil {
				t.Fatalf("Progress.Init: %v", err)
			}
			if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
				Index: 1, Title: "first", Theme: "theme",
				Arcs: []domain.ArcOutline{{Index: 1, Title: "arc", Goal: "goal", Chapters: []domain.OutlineEntry{v3SnapshotChapter(1, "old")}}},
			}}); err != nil {
				t.Fatalf("SaveLayeredOutline: %v", err)
			}
			tc.corrupt(t, dir)
			tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
			before := takeStoreSnapshot(dir)
			args, _ := json.Marshal(map[string]any{
				"type": "append_volume", "reason": "continue", "scale": "long",
				"content": domain.VolumeOutline{
					Index: 2, Title: "second", Theme: "theme",
					Arcs: []domain.ArcOutline{{Index: 1, Title: "arc", Goal: "goal", Chapters: []domain.OutlineEntry{v3SnapshotChapter(2, "new")}}},
				},
			})

			if _, err := tool.Execute(context.Background(), args); err == nil {
				t.Fatalf("append_volume must fail closed when %s is unreadable", tc.name)
			}
			assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
		})
	}
}

func TestSaveFoundationCore4_AppendVolumeSkeletonFirstArcRejectedBeforePlanningTierWrite(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 1); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "first", Theme: "theme",
		Arcs: []domain.ArcOutline{{Index: 1, Title: "arc", Goal: "goal", Chapters: []domain.OutlineEntry{{Chapter: 1, Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}}}}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	tool := NewSaveFoundationTool(st, projectprofile.NewCore4Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "continue", "scale": "long",
		"content": domain.VolumeOutline{
			Index: 2, Title: "second", Theme: "theme",
			Arcs: []domain.ArcOutline{{Index: 1, Title: "skeleton", Goal: "goal"}},
		},
	})

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("append_volume must reject a skeleton first arc before any write")
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

func extractContentDesc(schema map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	content, _ := props["content"].(map[string]any)
	desc, _ := content["description"].(string)
	return desc
}
