package diag

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// TestStaleForeshadow_ActiveAndEffectiveTouched 停滞判定覆盖全部活跃条目
// （planted/advanced/legacy 空状态；排除 resolved/retired），基准为
// effectiveTouchedAt（last_touched_at>0 用其，否则退回 planted_at），阈值固定 100。
func TestStaleForeshadow_ActiveAndEffectiveTouched(t *testing.T) {
	completed := make([]int, 0, 110)
	for ch := 1; ch <= 110; ch++ {
		completed = append(completed, ch)
	}
	snap := &Snapshot{
		Progress: &domain.Progress{CompletedChapters: completed},
		Foreshadow: []domain.ForeshadowEntry{
			// planted 于 ch1，从未推进：109 章 → stale
			{ID: "f1", Description: "上古封印", PlantedAt: 1, Status: "planted"},
			// planted ch5，最近推进 ch15：gap 95 < 100 → 不 stale
			{ID: "f2", Description: "断剑", PlantedAt: 5, Status: "advanced", LastTouchedAt: 15},
			// 已回收 / 已移除：排除
			{ID: "f3", Description: "信物", PlantedAt: 1, Status: "resolved", ResolvedAt: 20},
			{ID: "f4", Description: "弃线", PlantedAt: 1, Status: "retired", ClosedAt: 10},
			// legacy 空状态：视为活跃，gap 108 → stale
			{ID: "f5", Description: "旧账", PlantedAt: 2, Status: ""},
		},
	}

	findings := StaleForeshadow(snap)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Rule != "StaleForeshadow" {
		t.Fatalf("unexpected rule %q", f.Rule)
	}
	if !strings.Contains(f.Evidence, "f1") || !strings.Contains(f.Evidence, "f5") {
		t.Fatalf("stale evidence should include f1/f5, got %q", f.Evidence)
	}
	if strings.Contains(f.Evidence, "f2") || strings.Contains(f.Evidence, "f3") || strings.Contains(f.Evidence, "f4") {
		t.Fatalf("f2 (touched) / resolved / retired must not be stale, got %q", f.Evidence)
	}
	if !strings.Contains(f.Title, "100") {
		t.Fatalf("title should mention fixed 100-chapter threshold, got %q", f.Title)
	}

	// open 统计口径：排除 resolved/retired，按 active 计数；stale 按 effectiveTouchedAt。
	st := buildStats(snap)
	if st.ForeshadowOpen != 3 {
		t.Fatalf("ForeshadowOpen should count f1/f2/f5 (3), got %d", st.ForeshadowOpen)
	}
	if st.ForeshadowStale != 2 {
		t.Fatalf("ForeshadowStale should count f1/f5 (2), got %d", st.ForeshadowStale)
	}
}

// TestStaleForeshadow_Quiet 无停滞时不产出 finding。
func TestStaleForeshadow_Quiet(t *testing.T) {
	snap := &Snapshot{
		Progress: &domain.Progress{CompletedChapters: []int{1, 2, 3}},
		Foreshadow: []domain.ForeshadowEntry{
			{ID: "f1", Description: "近期", PlantedAt: 1, Status: "planted", LastTouchedAt: 3},
			{ID: "f2", Description: "已回收", PlantedAt: 1, Status: "resolved", ResolvedAt: 2},
		},
	}
	if findings := StaleForeshadow(snap); len(findings) != 0 {
		t.Fatalf("no stale expected, got %+v", findings)
	}
}
