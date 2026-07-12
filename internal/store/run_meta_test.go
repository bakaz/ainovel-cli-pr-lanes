package store

import (
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestSaveAndLoadRunMeta(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	meta := domain.RunMeta{
		StartedAt: "2026-03-07T10:00:00+08:00",
		Provider:  "openrouter",
		Style:     "fantasy",
		Model:     "gpt-4o",
	}
	if err := store.RunMeta.Save(meta); err != nil {
		t.Fatalf("SaveRunMeta: %v", err)
	}

	loaded, err := store.RunMeta.Load()
	if err != nil {
		t.Fatalf("LoadRunMeta: %v", err)
	}
	if loaded.Style != "fantasy" {
		t.Errorf("style mismatch: %s", loaded.Style)
	}
	if loaded.Provider != "openrouter" {
		t.Errorf("provider mismatch: %s", loaded.Provider)
	}
	if loaded.Model != "gpt-4o" {
		t.Errorf("model mismatch: %s", loaded.Model)
	}
}

func TestLoadRunMeta_Empty(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	meta, err := store.RunMeta.Load()
	if err != nil {
		t.Fatalf("LoadRunMeta on empty: %v", err)
	}
	if meta != nil {
		t.Fatalf("expected nil, got %+v", meta)
	}
}

func TestInitRunMeta_PreservesHistory(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// 先建立带运行意图的 RunMeta
	_ = store.RunMeta.Save(domain.RunMeta{
		StartedAt:    "old",
		Provider:     "openai",
		Style:        "fantasy",
		Model:        "old-model",
		PendingSteer: "待处理",
	})

	// Init 应保留 PendingSteer 等运行意图事实
	_ = store.RunMeta.Init("suspense", "openrouter", "new-model")

	meta, _ := store.RunMeta.Load()
	if meta.Style != "suspense" {
		t.Errorf("style should be updated, got %s", meta.Style)
	}
	if meta.Provider != "openrouter" {
		t.Errorf("provider should be updated, got %s", meta.Provider)
	}
	if meta.Model != "new-model" {
		t.Errorf("model should be updated, got %s", meta.Model)
	}
	if meta.PendingSteer != "待处理" {
		t.Errorf("pending steer should be preserved, got %s", meta.PendingSteer)
	}
}

func TestSetAndClearPendingSteer(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// 设置 PendingSteer
	if err := store.RunMeta.SetPendingSteer("主角改成女性"); err != nil {
		t.Fatalf("SetPendingSteer: %v", err)
	}
	meta, _ := store.RunMeta.Load()
	if meta.PendingSteer != "主角改成女性" {
		t.Errorf("expected pending steer, got %s", meta.PendingSteer)
	}

	// 清除
	if err := store.RunMeta.ClearPendingSteer(); err != nil {
		t.Fatalf("ClearPendingSteer: %v", err)
	}
	meta, _ = store.RunMeta.Load()
	if meta.PendingSteer != "" {
		t.Errorf("expected empty pending steer, got %s", meta.PendingSteer)
	}
}

func TestSetPlanningTier(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	if err := store.RunMeta.SetPlanningTier(domain.PlanningTierLong); err != nil {
		t.Fatalf("SetPlanningTier: %v", err)
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

func TestClearPendingSteer_Noop(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// 空 meta 上调用不报错
	if err := store.RunMeta.ClearPendingSteer(); err != nil {
		t.Fatalf("ClearPendingSteer on empty: %v", err)
	}
}

func TestSetAndClearPausePoint(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	pp := domain.PausePoint{After: domain.PauseAfterRewritesDrained, Reason: "重写第3章", SetAt: "ts"}
	if err := store.RunMeta.SetPausePoint(pp); err != nil {
		t.Fatalf("SetPausePoint: %v", err)
	}
	meta, _ := store.RunMeta.Load()
	if meta.PausePoint == nil || meta.PausePoint.After != domain.PauseAfterRewritesDrained || meta.PausePoint.Reason != "重写第3章" {
		t.Errorf("pause point round trip: %+v", meta.PausePoint)
	}

	if err := store.RunMeta.ClearPausePoint(); err != nil {
		t.Fatalf("ClearPausePoint: %v", err)
	}
	meta, _ = store.RunMeta.Load()
	if meta.PausePoint != nil {
		t.Errorf("expected nil pause point, got %+v", meta.PausePoint)
	}

	// 空/无点时清除幂等
	if err := store.RunMeta.ClearPausePoint(); err != nil {
		t.Fatalf("ClearPausePoint idempotent: %v", err)
	}
}

func TestInitRunMeta_PreservesPausePoint(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	_ = store.RunMeta.SetPausePoint(domain.PausePoint{After: domain.PauseAfterRewritesDrained, Reason: "验收"})
	// 进程重启路径：Host.New 每次都会调 Init，停靠点必须存活
	_ = store.RunMeta.Init("fantasy", "openrouter", "m")

	meta, _ := store.RunMeta.Load()
	if meta.PausePoint == nil || meta.PausePoint.Reason != "验收" {
		t.Fatalf("pause point should survive Init, got %+v", meta.PausePoint)
	}
}

// TestRunMetaInit_PreservesPlanStart 规划期(裁定已落盘、首个 foundation 未落盘)
// 崩溃重启时,Host.New 的 RunMeta.Init 不得清掉 PlanStart——它是恢复规划师身份的
// 唯一依据(engine.planStartFallback)。
func TestRunMetaInit_PreservesPlanStart(t *testing.T) {
	store := NewStore(t.TempDir())
	rec := domain.PlanStartRecord{RawPrompt: "写个悬疑短篇", Planner: "architect_short", PlannerTask: "任务全文", DecisionID: "dec-x"}
	if err := store.RunMeta.SetPlanStart(rec); err != nil {
		t.Fatalf("set plan start: %v", err)
	}
	// 模拟进程重启:Host.New 会再次 Init
	if err := store.RunMeta.Init("default", "openrouter", "m"); err != nil {
		t.Fatalf("init: %v", err)
	}
	meta, err := store.RunMeta.Load()
	if err != nil || meta == nil {
		t.Fatalf("load: %v", err)
	}
	if meta.PlanStart == nil || meta.PlanStart.Planner != "architect_short" {
		t.Fatalf("Init 必须保留 PlanStart, got %+v", meta.PlanStart)
	}
}
