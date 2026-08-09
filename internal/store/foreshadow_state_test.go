package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// mustForeshadow 断言 UpdateForeshadow 成功。
func mustForeshadow(t *testing.T, s *Store, chapter int, updates []domain.ForeshadowUpdate) {
	t.Helper()
	if err := s.World.UpdateForeshadow(chapter, updates); err != nil {
		t.Fatalf("UpdateForeshadow(ch%d, %+v): %v", chapter, updates, err)
	}
}

// seedForeshadowState 将账本推进到指定状态并返回条目标识。
func seedForeshadowState(t *testing.T, s *Store, state string) string {
	t.Helper()
	const id = "f1"
	switch state {
	case "不存在":
		// 不播种任何条目
	case "planted":
		mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{{ID: id, Action: "plant", Description: "黑影", Horizon: "book"}})
	case "advanced":
		seedForeshadowState(t, s, "planted")
		mustForeshadow(t, s, 2, []domain.ForeshadowUpdate{{ID: id, Action: "advance", Evidence: "黑影一闪而过"}})
	case "resolved":
		seedForeshadowState(t, s, "planted")
		mustForeshadow(t, s, 2, []domain.ForeshadowUpdate{{ID: id, Action: "resolve", Evidence: "黑影正是老周"}})
	case "retired":
		seedForeshadowState(t, s, "planted")
		mustForeshadow(t, s, 2, []domain.ForeshadowUpdate{{ID: id, Action: "retire", Reason: "大纲废弃该线索"}})
	default:
		t.Fatalf("unknown seed state %q", state)
	}
	return id
}

// validForeshadowUpdate 构造某 action 的合法载荷。
func validForeshadowUpdate(action, id string) domain.ForeshadowUpdate {
	switch action {
	case "plant":
		return domain.ForeshadowUpdate{ID: id, Action: "plant", Description: "新伏笔", Horizon: "cross_arc"}
	case "advance":
		return domain.ForeshadowUpdate{ID: id, Action: "advance", Evidence: "线索再次出现"}
	case "resolve":
		return domain.ForeshadowUpdate{ID: id, Action: "resolve", Evidence: "谜底揭晓"}
	case "retire":
		return domain.ForeshadowUpdate{ID: id, Action: "retire", Reason: "弃线"}
	}
	return domain.ForeshadowUpdate{ID: id, Action: action}
}

// TestForeshadow_StateMachineTable 覆盖 5×4 全部状态转换组合。
func TestForeshadow_StateMachineTable(t *testing.T) {
	tests := []struct {
		state, action string
		wantErr       bool
		wantStatus    string // 非空时断言目标状态
	}{
		// 不存在：仅 plant 允许
		{"不存在", "plant", false, "planted"},
		{"不存在", "advance", true, ""},
		{"不存在", "resolve", true, ""},
		{"不存在", "retire", true, ""},
		// planted：全 action 可用，plant 幂等
		{"planted", "plant", false, "planted"},
		{"planted", "advance", false, "advanced"},
		{"planted", "resolve", false, "resolved"},
		{"planted", "retire", false, "retired"},
		// advanced：拒绝 plant，允许 advance/resolve/retire
		{"advanced", "plant", true, "advanced"},
		{"advanced", "advance", false, "advanced"},
		{"advanced", "resolve", false, "resolved"},
		{"advanced", "retire", false, "retired"},
		// resolved：仅 resolve 幂等
		{"resolved", "plant", true, "resolved"},
		{"resolved", "advance", true, "resolved"},
		{"resolved", "resolve", false, "resolved"},
		{"resolved", "retire", true, "resolved"},
		// retired：仅 retire 幂等
		{"retired", "plant", true, "retired"},
		{"retired", "advance", true, "retired"},
		{"retired", "resolve", true, "retired"},
		{"retired", "retire", false, "retired"},
	}
	for _, tt := range tests {
		t.Run(tt.state+"/"+tt.action, func(t *testing.T) {
			s := newTestStore(t)
			id := seedForeshadowState(t, s, tt.state)
			before, err := s.World.LoadForeshadowLedger()
			if err != nil {
				t.Fatalf("load before: %v", err)
			}
			err = s.World.UpdateForeshadow(3, []domain.ForeshadowUpdate{validForeshadowUpdate(tt.action, id)})
			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got %v", tt.wantErr, err)
			}
			after, err := s.World.LoadForeshadowLedger()
			if err != nil {
				t.Fatalf("load after: %v", err)
			}
			// 失败的路径不允许增删条目；成功创建（不存在→plant）允许 +1
			if tt.wantErr && len(after) != len(before) {
				t.Fatalf("rejected %s must not change entry count: before %d, after %d", tt.action, len(before), len(after))
			}
			if tt.wantStatus != "" && len(after) == 1 && after[0].Status != tt.wantStatus {
				t.Errorf("want status %q, got %q", tt.wantStatus, after[0].Status)
			}
			// 失败路径必须保持账本原样
			if tt.wantErr && len(after) > 0 && len(before) > 0 && after[0].Status != before[0].Status {
				t.Errorf("failed %s should not mutate state: before %q, after %q", tt.action, before[0].Status, after[0].Status)
			}
		})
	}
}

// TestForeshadow_UnknownActionAndID 未知 action / 未知 ID 必须报错且账本不变。
func TestForeshadow_UnknownActionAndID(t *testing.T) {
	s := newTestStore(t)
	mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"}})

	// 未知 action
	err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{ID: "f1", Action: "weird"}})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("unknown action: want error containing %q, got %v", "unknown action", err)
	}
	// 未知 ID：advance / resolve / retire 全部拒绝
	for _, action := range []string{"advance", "resolve", "retire"} {
		err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{validForeshadowUpdate(action, "ghost")})
		if err == nil || !strings.Contains(err.Error(), "unknown id") {
			t.Fatalf("%s on unknown id: want error containing %q, got %v", action, "unknown id", err)
		}
	}
	// 账本应保持 1 条且未被污染
	all, err := s.World.LoadForeshadowLedger()
	if err != nil || len(all) != 1 || all[0].Status != "planted" {
		t.Fatalf("ledger mutated by rejected ops: %+v, %v", all, err)
	}
}

// TestForeshadow_RequiresEvidenceAndReason 缺失 evidence/reason 拒绝且不更新时间戳。
func TestForeshadow_RequiresEvidenceAndReason(t *testing.T) {
	s := newTestStore(t)
	mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"}})

	// advance 缺 evidence → 拒绝，状态与 last_touched_at 均不变
	if err := s.World.UpdateForeshadow(2, []domain.ForeshadowUpdate{{ID: "f1", Action: "advance"}}); err == nil {
		t.Fatal("advance without evidence should be rejected")
	}
	// resolve 缺 evidence → 拒绝
	if err := s.World.UpdateForeshadow(2, []domain.ForeshadowUpdate{{ID: "f1", Action: "resolve"}}); err == nil {
		t.Fatal("resolve without evidence should be rejected")
	}
	// retire 缺 reason → 拒绝
	if err := s.World.UpdateForeshadow(2, []domain.ForeshadowUpdate{{ID: "f1", Action: "retire"}}); err == nil {
		t.Fatal("retire without reason should be rejected")
	}
	all, _ := s.World.LoadForeshadowLedger()
	if all[0].Status != "planted" || all[0].LastTouchedAt != 0 || all[0].ResolvedAt != 0 || all[0].ClosedAt != 0 {
		t.Fatalf("rejected ops must not touch timestamps/status: %+v", all[0])
	}
}

// TestForeshadow_TimeInvariants 时间不变量：resolved 必设 resolved_at、advance 后无
// resolved_at、retire 必设 closed_at+close_reason、非 retired 清理 closed_at。
func TestForeshadow_TimeInvariants(t *testing.T) {
	s := newTestStore(t)
	mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"}})

	// advance → last_touched_at 设置、resolved_at 为 0
	mustForeshadow(t, s, 5, []domain.ForeshadowUpdate{{ID: "f1", Action: "advance", Evidence: "线索二"}})
	all, _ := s.World.LoadForeshadowLedger()
	if all[0].LastTouchedAt != 5 || all[0].LastEvidence != "线索二" || all[0].ResolvedAt != 0 || all[0].ClosedAt != 0 || all[0].CloseReason != "" {
		t.Fatalf("after advance: %+v", all[0])
	}

	// resolve → resolved_at / resolution_evidence / last_touched_at 全部设置
	mustForeshadow(t, s, 8, []domain.ForeshadowUpdate{{ID: "f1", Action: "resolve", Evidence: "谜底揭晓"}})
	all, _ = s.World.LoadForeshadowLedger()
	if all[0].Status != "resolved" || all[0].ResolvedAt != 8 || all[0].ResolutionEvidence != "谜底揭晓" || all[0].LastTouchedAt != 8 {
		t.Fatalf("after resolve: %+v", all[0])
	}
	if all[0].ClosedAt != 0 || all[0].CloseReason != "" {
		t.Fatalf("resolved must not carry retire fields: %+v", all[0])
	}

	// retire → closed_at + close_reason 设置、resolved_at 清理
	s2 := newTestStore(t)
	mustForeshadow(t, s2, 1, []domain.ForeshadowUpdate{{ID: "f2", Action: "plant", Description: "断剑", Horizon: "book"}})
	mustForeshadow(t, s2, 4, []domain.ForeshadowUpdate{{ID: "f2", Action: "retire", Reason: "线索并入主线"}})
	all2, _ := s2.World.LoadForeshadowLedger()
	if all2[0].Status != "retired" || all2[0].ClosedAt != 4 || all2[0].CloseReason != "线索并入主线" || all2[0].ResolvedAt != 0 {
		t.Fatalf("after retire: %+v", all2[0])
	}
}

// TestForeshadow_RepeatResolveRetireIdempotent 重复 resolve/retire 返回 nil 且不重复记录。
func TestForeshadow_RepeatResolveRetireIdempotent(t *testing.T) {
	s := newTestStore(t)
	mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"}})
	mustForeshadow(t, s, 2, []domain.ForeshadowUpdate{{ID: "f1", Action: "resolve", Evidence: "谜底"}})
	if err := s.World.UpdateForeshadow(3, []domain.ForeshadowUpdate{{ID: "f1", Action: "resolve", Evidence: "谜底"}}); err != nil {
		t.Fatalf("repeat resolve should be idempotent, got %v", err)
	}
	all, _ := s.World.LoadForeshadowLedger()
	if len(all) != 1 || all[0].ResolvedAt != 2 {
		t.Fatalf("repeat resolve mutated ledger: %+v", all)
	}

	s2 := newTestStore(t)
	mustForeshadow(t, s2, 1, []domain.ForeshadowUpdate{{ID: "f2", Action: "plant", Description: "断剑", Horizon: "book"}})
	mustForeshadow(t, s2, 2, []domain.ForeshadowUpdate{{ID: "f2", Action: "retire", Reason: "弃线"}})
	if err := s2.World.UpdateForeshadow(3, []domain.ForeshadowUpdate{{ID: "f2", Action: "retire", Reason: "弃线"}}); err != nil {
		t.Fatalf("repeat retire should be idempotent, got %v", err)
	}
	all2, _ := s2.World.LoadForeshadowLedger()
	if len(all2) != 1 || all2[0].ClosedAt != 2 {
		t.Fatalf("repeat retire mutated ledger: %+v", all2)
	}
}

// TestForeshadow_HorizonImmutable 后续 action 携带 horizon 一律忽略。
func TestForeshadow_HorizonImmutable(t *testing.T) {
	s := newTestStore(t)
	mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"}})
	mustForeshadow(t, s, 2, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "advance", Evidence: "线索", Horizon: "cross_arc"},
	})
	mustForeshadow(t, s, 3, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "resolve", Evidence: "谜底", Horizon: "cross_arc"},
	})
	all, _ := s.World.LoadForeshadowLedger()
	if all[0].Horizon != "book" {
		t.Fatalf("horizon must be immutable after plant, got %q", all[0].Horizon)
	}
}

// TestForeshadow_LegacyDataCompatibility 旧数据（无新字段）加载兼容：
// 零值字段 + 可继续推进 + 空 horizon 可由重复 plant 补填。
func TestForeshadow_LegacyDataCompatibility(t *testing.T) {
	s := newTestStore(t)
	legacy := `[{"id":"f1","description":"旧数据伏笔","planted_at":1,"status":"planted"}]`
	ledgerPath := filepath.Join(s.Dir(), "foreshadow_ledger.json")
	if err := os.WriteFile(ledgerPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy ledger: %v", err)
	}

	all, err := s.World.LoadForeshadowLedger()
	if err != nil || len(all) != 1 {
		t.Fatalf("legacy load: %+v, %v", all, err)
	}
	if all[0].Horizon != "" || all[0].LastTouchedAt != 0 || all[0].ResolutionEvidence != "" || all[0].ClosedAt != 0 {
		t.Fatalf("legacy entry should have zero new fields: %+v", all[0])
	}

	// 旧条目可直接 advance（不要求 horizon）
	mustForeshadow(t, s, 4, []domain.ForeshadowUpdate{{ID: "f1", Action: "advance", Evidence: "旧线索推进"}})
	all, _ = s.World.LoadForeshadowLedger()
	if all[0].Status != "advanced" || all[0].LastTouchedAt != 4 {
		t.Fatalf("legacy entry advance failed: %+v", all[0])
	}
}

// TestForeshadow_LegacyReplantFillsHorizon 遗留空 horizon 条目在 planted 状态重复 plant 可补填。
func TestForeshadow_LegacyReplantFillsHorizon(t *testing.T) {
	s := newTestStore(t)
	legacy := `[{"id":"f1","description":"旧数据伏笔","planted_at":1,"status":"planted"}]`
	ledgerPath := filepath.Join(s.Dir(), "foreshadow_ledger.json")
	if err := os.WriteFile(ledgerPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy ledger: %v", err)
	}
	// 仍处于 planted 时重复 plant → 补填空 horizon，不覆盖 planted_at/description
	mustForeshadow(t, s, 5, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "旧数据伏笔", Horizon: "cross_arc"}})
	all, _ := s.World.LoadForeshadowLedger()
	if all[0].Horizon != "cross_arc" || all[0].PlantedAt != 1 || all[0].Status != "planted" {
		t.Fatalf("replant should only fill empty horizon: %+v", all[0])
	}
}

// TestForeshadow_LoadActiveExcludesResolvedAndRetired LoadActiveForeshadow 排除 resolved+retired。
func TestForeshadow_LoadActiveExcludesResolvedAndRetired(t *testing.T) {
	s := newTestStore(t)
	mustForeshadow(t, s, 1, []domain.ForeshadowUpdate{
		{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"},
		{ID: "f2", Action: "plant", Description: "断剑", Horizon: "book"},
		{ID: "f3", Action: "plant", Description: "玉佩", Horizon: "cross_arc"},
		{ID: "f4", Action: "plant", Description: "密信", Horizon: "book"},
	})
	mustForeshadow(t, s, 2, []domain.ForeshadowUpdate{
		{ID: "f2", Action: "advance", Evidence: "断剑出鞘"},
		{ID: "f3", Action: "resolve", Evidence: "玉佩认主"},
		{ID: "f4", Action: "retire", Reason: "弃线"},
	})

	active, err := s.World.LoadActiveForeshadow()
	if err != nil {
		t.Fatalf("LoadActiveForeshadow: %v", err)
	}
	var ids []string
	for _, e := range active {
		ids = append(ids, e.ID)
	}
	if len(ids) != 2 || ids[0] != "f1" || ids[1] != "f2" {
		t.Fatalf("active should be [f1 f2] (planted+advanced), got %v", ids)
	}
}

// TestForeshadow_PlantRequiresHorizonAndDescription plant 缺 description/horizon 拒绝。
func TestForeshadow_PlantRequiresHorizonAndDescription(t *testing.T) {
	s := newTestStore(t)
	if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影"}}); err == nil {
		t.Fatal("plant without horizon should be rejected")
	}
	if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Horizon: "book"}}); err == nil {
		t.Fatal("plant without description should be rejected")
	}
	if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影", Horizon: "bad"}}); err == nil {
		t.Fatal("plant with invalid horizon should be rejected")
	}
	all, err := s.World.LoadForeshadowLedger()
	if err != nil || all != nil {
		t.Fatalf("rejected plants must not create entries: %+v, %v", all, err)
	}
}

// TestForeshadow_UpdateErrorWrapped 状态机拒绝类错误应可被 errs.ErrToolArgs 识别。
func TestForeshadow_UpdateErrorWrapped(t *testing.T) {
	s := newTestStore(t)
	err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{ID: "ghost", Action: "resolve", Evidence: "x"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errs.ErrToolArgs) {
		t.Fatalf("UpdateForeshadow rejection should be wrapped in errs.ErrToolArgs, got %v", err)
	}
}

// TestRenderForeshadow_ExtendedFields 渲染镜像必须展示 horizon / last_touched_at /
// retired 的 closed_at+close_reason / resolved 的 resolution_evidence。
func TestRenderForeshadow_ExtendedFields(t *testing.T) {
	md := renderForeshadow([]domain.ForeshadowEntry{
		{ID: "f1", Description: "黑影", PlantedAt: 1, Status: "planted", Horizon: "book"},
		{ID: "f2", Description: "断剑", PlantedAt: 2, Status: "advanced", Horizon: "cross_arc", LastTouchedAt: 8, LastEvidence: "断剑再现"},
		{ID: "f3", Description: "信物", PlantedAt: 1, Status: "resolved", Horizon: "book", ResolvedAt: 20, ResolutionEvidence: "信物正是玉牌", LastTouchedAt: 20},
		{ID: "f4", Description: "弃线", PlantedAt: 3, Status: "retired", ClosedAt: 9, CloseReason: "大纲废弃"},
	})
	for _, want := range []string{
		"跨度：book",
		"跨度：cross_arc",
		"最近推进：第 8 章",
		"已回收（第 20 章）",
		"证据：信物正是玉牌",
		"已移除",
		"第 9 章移除：大纲废弃",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderForeshadow 缺少 %q\n%s", want, md)
		}
	}
}
