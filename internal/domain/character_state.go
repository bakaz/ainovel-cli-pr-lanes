package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// CharacterStateEntry 角色/实体受控状态条目。(entity, field) 构成唯一键。
// field 使用受控命名空间（如 body_device.xxx），防止自由文本污染状态层。
type CharacterStateEntry struct {
	Entity         string `json:"entity"`
	Field          string `json:"field"` // 受控命名空间，如 body_device.xxx
	Value          string `json:"value"`
	UpdatedChapter int    `json:"updated_chapter"`
	Evidence       string `json:"evidence,omitempty"`
}

// CharacterStateUpdate 角色状态更新操作（upsert；value 为空且带 reason 时清键）。
type CharacterStateUpdate struct {
	Entity   string `json:"entity"`
	Field    string `json:"field"`
	Value    string `json:"value"`
	Reason   string `json:"reason,omitempty"`   // 状态变化原因（派生到 state_changes）；清键必填
	Evidence string `json:"evidence,omitempty"` // 正文引文，最长 MaxCharacterEvidenceRunes
}

// Clears reports whether this update removes the (entity, field) from current state.
func (u CharacterStateUpdate) Clears() bool {
	return strings.TrimSpace(u.Value) == ""
}

const (
	// MaxCharacterValueRunes 单个状态值的最大长度（字符数）。
	MaxCharacterValueRunes = 800
	// MaxCharacterEvidenceRunes 状态证据引文的最大长度（字符数）。
	MaxCharacterEvidenceRunes = 300
	// MaxFieldsPerEntity 单个实体允许的字段数上限。
	MaxFieldsPerEntity = 100
	// MaxNewStatusFieldsPerCommit 单次提交允许新增的 status.* 字段数。
	// 覆盖已有 status 不计入；防止把章节进度写成角色状态。
	MaxNewStatusFieldsPerCommit = 2
)

// CharacterStateFieldPrefixes field 命名空间前缀白名单。
var CharacterStateFieldPrefixes = []string{
	"body_device.", "health.", "location.", "capability.",
	"resource.", "inventory.", "status.", "knowledge.",
}

// ValidCharacterStateField 校验 field 是否属于受控命名空间。
func ValidCharacterStateField(field string) bool {
	for _, p := range CharacterStateFieldPrefixes {
		if strings.HasPrefix(field, p) {
			return true
		}
	}
	return false
}

// ValidateCharacterStateUpdate 校验角色状态更新：entity/field 非空、field 在受控
// 命名空间内、value 长度 ≤ MaxCharacterValueRunes、evidence 长度 ≤ MaxCharacterEvidenceRunes。
func ValidateCharacterStateUpdate(u CharacterStateUpdate) error {
	if strings.TrimSpace(u.Entity) == "" {
		return fmt.Errorf("character state: entity 不能为空")
	}
	if strings.TrimSpace(u.Field) == "" {
		return fmt.Errorf("character state: field 不能为空")
	}
	if !ValidCharacterStateField(u.Field) {
		return fmt.Errorf("character state: field %q 不在受控命名空间内（允许前缀：%v）", u.Field, CharacterStateFieldPrefixes)
	}
	if u.Clears() {
		if strings.TrimSpace(u.Reason) == "" {
			return fmt.Errorf("character state: 清空 %s.%s 必须提供 reason", u.Entity, u.Field)
		}
	} else if runes := utf8.RuneCountInString(u.Value); runes > MaxCharacterValueRunes {
		return fmt.Errorf("character state: %s.%s 值长度 %d 超过上限 %d 个字符", u.Entity, u.Field, runes, MaxCharacterValueRunes)
	}
	if runes := utf8.RuneCountInString(u.Evidence); runes > MaxCharacterEvidenceRunes {
		return fmt.Errorf("character state: %s.%s evidence 长度 %d 超过上限 %d 个字符", u.Entity, u.Field, runes, MaxCharacterEvidenceRunes)
	}
	return nil
}
