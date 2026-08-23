package domain

import (
	"strings"
	"testing"
)

// TestValidCharacterStateField 命名空间前缀白名单。
func TestValidCharacterStateField(t *testing.T) {
	valid := []string{
		"body_device.left_hand", "health.mind", "location.current",
		"capability.sword", "resource.qi", "inventory.pills",
		"status.realm", "knowledge.map",
	}
	for _, f := range valid {
		if !ValidCharacterStateField(f) {
			t.Errorf("%q should be valid", f)
		}
	}
	invalid := []string{
		"", "自由文本", "device.left_hand", "body", "body_", "BODY_DEVICE.x", ".x",
	}
	for _, f := range invalid {
		if ValidCharacterStateField(f) {
			t.Errorf("%q should be invalid", f)
		}
	}
}

// TestValidateCharacterStateUpdate 合法/非法更新。
func TestValidateCharacterStateUpdate(t *testing.T) {
	ok := CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: "练气期"}
	if err := ValidateCharacterStateUpdate(ok); err != nil {
		t.Fatalf("valid update rejected: %v", err)
	}
	clear := CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: "", Reason: "不再约束"}
	if err := ValidateCharacterStateUpdate(clear); err != nil {
		t.Fatalf("clear with reason rejected: %v", err)
	}
	// 上限边界：恰好 Max 长度通过
	boundary := CharacterStateUpdate{
		Entity:   "林墨",
		Field:    "status.realm",
		Value:    strings.Repeat("值", MaxCharacterValueRunes),
		Evidence: strings.Repeat("引", MaxCharacterEvidenceRunes),
	}
	if err := ValidateCharacterStateUpdate(boundary); err != nil {
		t.Fatalf("boundary update rejected: %v", err)
	}

	cases := []struct {
		name   string
		update CharacterStateUpdate
	}{
		{"empty entity", CharacterStateUpdate{Field: "status.realm", Value: "x"}},
		{"whitespace entity", CharacterStateUpdate{Entity: "  ", Field: "status.realm", Value: "x"}},
		{"empty field", CharacterStateUpdate{Entity: "林墨", Value: "x"}},
		{"field outside namespace", CharacterStateUpdate{Entity: "林墨", Field: "freeform", Value: "x"}},
		{"value over limit", CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: strings.Repeat("值", MaxCharacterValueRunes+1)}},
		{"evidence over limit", CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: "x", Evidence: strings.Repeat("引", MaxCharacterEvidenceRunes+1)}},
		{"empty value without reason", CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: ""}},
		{"whitespace value without reason", CharacterStateUpdate{Entity: "林墨", Field: "status.realm", Value: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCharacterStateUpdate(tc.update); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// TestForeshadowUpdateValidate plant/advance/resolve/retire 的必填字段与草稿引文校验。
func TestForeshadowUpdateValidate(t *testing.T) {
	// 合法操作
	valid := []ForeshadowUpdate{
		{ID: "f1", Action: "plant", Description: "黑影", Horizon: "cross_arc"},
		{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"},
		{ID: "f1", Action: "advance", Evidence: "黑影一闪而过"},
		{ID: "f1", Action: "resolve", Evidence: "谜底揭晓"},
		{ID: "f1", Action: "retire", Reason: "弃线"},
	}
	for _, u := range valid {
		if err := u.Validate(1, nil); err != nil {
			t.Errorf("%s should pass: %v", u.Action, err)
		}
	}

	invalid := []struct {
		name   string
		update ForeshadowUpdate
	}{
		{"empty id", ForeshadowUpdate{Action: "plant", Description: "x", Horizon: "book"}},
		{"unknown action", ForeshadowUpdate{ID: "f1", Action: "weird"}},
		{"plant without description", ForeshadowUpdate{ID: "f1", Action: "plant", Horizon: "book"}},
		{"plant without horizon", ForeshadowUpdate{ID: "f1", Action: "plant", Description: "黑影"}},
		{"plant bad horizon", ForeshadowUpdate{ID: "f1", Action: "plant", Description: "黑影", Horizon: "arc"}},
		{"advance without evidence", ForeshadowUpdate{ID: "f1", Action: "advance"}},
		{"resolve without evidence", ForeshadowUpdate{ID: "f1", Action: "resolve"}},
		{"retire without reason", ForeshadowUpdate{ID: "f1", Action: "retire"}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.update.Validate(1, nil); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}

	// evidenceInDraft：advance/resolve 的 evidence 必须能在草稿中找到
	draft := func(s string) bool { return strings.Contains("黑影一闪而过", s) }
	if err := (ForeshadowUpdate{ID: "f1", Action: "advance", Evidence: "黑影一闪而过"}).Validate(1, draft); err != nil {
		t.Fatalf("evidence in draft should pass: %v", err)
	}
	if err := (ForeshadowUpdate{ID: "f1", Action: "advance", Evidence: "不在草稿中"}).Validate(1, draft); err == nil {
		t.Fatal("evidence not in draft should be rejected")
	}
	// plant/retire 不受 evidenceInDraft 影响
	if err := (ForeshadowUpdate{ID: "f1", Action: "plant", Description: "黑影", Horizon: "book"}).Validate(1, draft); err != nil {
		t.Fatalf("plant should ignore evidenceInDraft: %v", err)
	}
}
