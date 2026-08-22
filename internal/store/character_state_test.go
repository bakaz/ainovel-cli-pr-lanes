package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
)

func csUpdate(entity, field, value string) domain.CharacterStateUpdate {
	return domain.CharacterStateUpdate{Entity: entity, Field: field, Value: value}
}

// TestCharacterState_LoadEmpty 文件不存在返回 (nil, nil)。
func TestCharacterState_LoadEmpty(t *testing.T) {
	s := newTestStore(t)
	entries, err := s.World.LoadCharacterState()
	if err != nil || entries != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", entries, err)
	}
}

// TestCharacterState_UpsertSemantics 新增 + 覆盖 + 证据/章节更新。
func TestCharacterState_UpsertSemantics(t *testing.T) {
	s := newTestStore(t)

	if err := s.World.UpsertCharacterState(1, []domain.CharacterStateUpdate{
		csUpdate("林墨", "body_device.left_hand", "断指"),
		csUpdate("林墨", "health.mind", "清醒"),
		csUpdate("老周", "location.current", "客栈"),
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// 覆盖已有 (entity, field)：新值 + updated_chapter 更新
	if err := s.World.UpsertCharacterState(3, []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "health.mind", Value: "恍惚", Evidence: "林墨盯着断指出神", Reason: "中毒"},
	}); err != nil {
		t.Fatalf("overwrite upsert: %v", err)
	}

	entries, err := s.World.LoadCharacterState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(entries), entries)
	}
	byKey := map[string]domain.CharacterStateEntry{}
	for _, e := range entries {
		byKey[e.Entity+"|"+e.Field] = e
	}
	got := byKey["林墨|health.mind"]
	if got.Value != "恍惚" || got.UpdatedChapter != 3 || got.Evidence != "林墨盯着断指出神" {
		t.Fatalf("overwrite not applied: %+v", got)
	}
	if byKey["林墨|body_device.left_hand"].UpdatedChapter != 1 {
		t.Fatalf("untouched entry chapter changed: %+v", byKey["林墨|body_device.left_hand"])
	}
}

// TestCharacterState_DerivedStateChanges 每次 upsert 派生 state_changes（old/new/reason）。
func TestCharacterState_DerivedStateChanges(t *testing.T) {
	s := newTestStore(t)

	if err := s.World.UpsertCharacterState(1, []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "status.realm", Value: "练气期", Reason: "突破"},
	}); err != nil {
		t.Fatalf("upsert ch1: %v", err)
	}
	if err := s.World.UpsertCharacterState(5, []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "status.realm", Value: "筑基期", Reason: "二次突破"},
	}); err != nil {
		t.Fatalf("upsert ch5: %v", err)
	}

	changes, err := s.World.LoadStateChanges()
	if err != nil {
		t.Fatalf("load changes: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("want 2 state changes, got %d: %+v", len(changes), changes)
	}
	if changes[0].OldValue != "" || changes[0].NewValue != "练气期" || changes[0].Reason != "突破" || changes[0].Chapter != 1 {
		t.Fatalf("first change: %+v", changes[0])
	}
	if changes[1].OldValue != "练气期" || changes[1].NewValue != "筑基期" || changes[1].Reason != "二次突破" || changes[1].Chapter != 5 {
		t.Fatalf("second change: %+v", changes[1])
	}
}

// TestCharacterState_DerivedStateChangesIdempotent 同章重复提交不重复 append（复用 stateChangeKey 去重）。
func TestCharacterState_DerivedStateChangesIdempotent(t *testing.T) {
	s := newTestStore(t)
	updates := []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "body_device.right_arm", Value: "骨折", Reason: "坠崖"},
	}
	if err := s.World.UpsertCharacterState(2, updates); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := s.World.UpsertCharacterState(2, updates); err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	// 同章同值重复提交：只应产生 1 条变化
	changes, _ := s.World.LoadStateChanges()
	if len(changes) != 1 {
		t.Fatalf("duplicate submit must not re-append, got %d: %+v", len(changes), changes)
	}
	entries, _ := s.World.LoadCharacterState()
	if len(entries) != 1 {
		t.Fatalf("upsert must not duplicate entries, got %d", len(entries))
	}
}

// TestCharacterState_Validation 命名空间 / 长度 / 空 entity/field 校验。
func TestCharacterState_Validation(t *testing.T) {
	s := newTestStore(t)
	longValue := strings.Repeat("值", domain.MaxCharacterValueRunes+1)
	longEvidence := strings.Repeat("引", domain.MaxCharacterEvidenceRunes+1)

	cases := []struct {
		name   string
		update domain.CharacterStateUpdate
	}{
		{"empty entity", domain.CharacterStateUpdate{Field: "status.realm", Value: "x"}},
		{"empty field", domain.CharacterStateUpdate{Entity: "林墨", Field: "  ", Value: "x"}},
		{"field outside namespace", csUpdate("林墨", "自由文本", "x")},
		{"partial prefix not matched", csUpdate("林墨", "device.left_hand", "x")},
		{"value too long", csUpdate("林墨", "status.realm", longValue)},
		{"evidence too long", domain.CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: "x", Evidence: longEvidence}},
		{"empty value without reason", domain.CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.World.UpsertCharacterState(1, []domain.CharacterStateUpdate{tc.update})
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !errors.Is(err, errs.ErrToolArgs) {
				t.Fatalf("validation error should wrap errs.ErrToolArgs, got %v", err)
			}
			// 校验失败不得写入任何数据
			entries, _ := s.World.LoadCharacterState()
			if entries != nil {
				t.Fatalf("failed upsert must not write entries: %+v", entries)
			}
			changes, _ := s.World.LoadStateChanges()
			if changes != nil {
				t.Fatalf("failed upsert must not write state changes: %+v", changes)
			}
		})
	}
}

// TestCharacterState_MaxFieldsPerEntity 单实体字段数上限：50 允许，第 51 个拒绝。
func TestCharacterState_MaxFieldsPerEntity(t *testing.T) {
	s := newTestStore(t)
	var updates []domain.CharacterStateUpdate
	for i := 0; i < domain.MaxFieldsPerEntity; i++ {
		updates = append(updates, csUpdate("林墨", "body_device.item_"+string(rune('a'+i)), "值"))
	}
	if err := s.World.UpsertCharacterState(1, updates); err != nil {
		t.Fatalf("upsert %d fields: %v", domain.MaxFieldsPerEntity, err)
	}
	// 第 21 个字段 → 拒绝且不写入
	err := s.World.UpsertCharacterState(2, []domain.CharacterStateUpdate{
		csUpdate("林墨", "body_device.overflow", "值"),
	})
	if err == nil || !errors.Is(err, errs.ErrToolArgs) {
		t.Fatalf("21st field should be rejected with errs.ErrToolArgs, got %v", err)
	}
	entries, _ := s.World.LoadCharacterState()
	if len(entries) != domain.MaxFieldsPerEntity {
		t.Fatalf("entries must stay at %d, got %d", domain.MaxFieldsPerEntity, len(entries))
	}
	// 其他实体不受影响
	if err := s.World.UpsertCharacterState(2, []domain.CharacterStateUpdate{
		csUpdate("老周", "location.current", "客栈"),
	}); err != nil {
		t.Fatalf("other entity should be unaffected: %v", err)
	}
}

// TestCharacterState_ValidationFailureRollback 批量中一个校验失败 → 全部不落盘。
func TestCharacterState_ValidationFailureRollback(t *testing.T) {
	s := newTestStore(t)
	err := s.World.UpsertCharacterState(1, []domain.CharacterStateUpdate{
		csUpdate("林墨", "status.realm", "练气期"),
		csUpdate("林墨", "bad.field", "x"),
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	entries, _ := s.World.LoadCharacterState()
	if entries != nil {
		t.Fatalf("batch must be atomic on validation failure: %+v", entries)
	}
	changes, _ := s.World.LoadStateChanges()
	if changes != nil {
		t.Fatalf("no state changes should be derived from failed batch: %+v", changes)
	}
}

// TestCharacterState_SaveFullWrite SaveCharacterState 全量写（迁移工具用）。
func TestCharacterState_SaveFullWrite(t *testing.T) {
	s := newTestStore(t)
	entries := []domain.CharacterStateEntry{
		{Entity: "林墨", Field: "status.realm", Value: "筑基期", UpdatedChapter: 5},
	}
	if err := s.World.SaveCharacterState(entries); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := s.World.LoadCharacterState()
	if err != nil || len(loaded) != 1 || loaded[0].Value != "筑基期" {
		t.Fatalf("roundtrip: %+v, %v", loaded, err)
	}
	// 全量覆盖：清空旧数据
	if err := s.World.SaveCharacterState(nil); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	loaded, _ = s.World.LoadCharacterState()
	if loaded != nil {
		t.Fatalf("want empty after full overwrite, got %+v", loaded)
	}
}

// TestCharacterState_CrossStorePersistence 跨 Store 实例可读。
func TestCharacterState_CrossStorePersistence(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := s.World.UpsertCharacterState(1, []domain.CharacterStateUpdate{
		csUpdate("林墨", "health.mind", "清醒"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s2 := NewStore(dir)
	entries, err := s2.World.LoadCharacterState()
	if err != nil || len(entries) != 1 || entries[0].Entity != "林墨" {
		t.Fatalf("cross-store load: %+v, %v", entries, err)
	}
}

func TestCharacterState_ClearRemovesKey(t *testing.T) {
	s := newTestStore(t)
	if err := s.World.UpsertCharacterState(1, []domain.CharacterStateUpdate{
		csUpdate("林墨", "status.realm", "练气期"),
		csUpdate("林墨", "body_device.ring", "在"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.World.UpsertCharacterState(2, []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "status.realm", Value: "", Reason: "不再约束"},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.World.LoadCharacterState()
	if err != nil || len(entries) != 1 || entries[0].Field != "body_device.ring" {
		t.Fatalf("after clear want only ring, got %+v err=%v", entries, err)
	}
	changes, _ := s.World.LoadStateChanges()
	var found bool
	for _, c := range changes {
		if c.Field == "status.realm" && c.NewValue == "" && c.OldValue == "练气期" && c.Reason == "不再约束" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing clear state_change: %+v", changes)
	}
	if err := s.World.UpsertCharacterState(3, []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "status.gone", Value: "", Reason: "幂等"},
	}); err != nil {
		t.Fatalf("clear missing key should be idempotent: %v", err)
	}
}

func TestCharacterState_ClearFreesSlotForNewField(t *testing.T) {
	s := newTestStore(t)
	var updates []domain.CharacterStateUpdate
	for i := 0; i < domain.MaxFieldsPerEntity; i++ {
		updates = append(updates, csUpdate("林墨", fmt.Sprintf("status.item_%d", i), "值"))
	}
	if err := s.World.UpsertCharacterState(1, updates); err != nil {
		t.Fatal(err)
	}
	err := s.World.UpsertCharacterState(2, []domain.CharacterStateUpdate{
		{Entity: "林墨", Field: "status.item_0", Value: "", Reason: "结束"},
		csUpdate("林墨", "body_device.new", "新"),
	})
	if err != nil {
		t.Fatalf("clear then add in same batch: %v", err)
	}
	entries, _ := s.World.LoadCharacterState()
	if len(entries) != domain.MaxFieldsPerEntity {
		t.Fatalf("want %d entries, got %d", domain.MaxFieldsPerEntity, len(entries))
	}
}
