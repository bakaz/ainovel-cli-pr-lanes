package store

import (
	"fmt"
	"os"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
)

// characterStateFile 角色受控状态文件路径。
const characterStateFile = "meta/character_state.json"

// ── 角色受控状态 ──

// LoadCharacterState 读取角色受控状态；文件不存在返回 (nil, nil)。
func (s *WorldStore) LoadCharacterState() ([]domain.CharacterStateEntry, error) {
	var entries []domain.CharacterStateEntry
	if err := s.io.ReadJSON(characterStateFile, &entries); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// SaveCharacterState 全量写入角色受控状态（迁移工具用）。
func (s *WorldStore) SaveCharacterState(entries []domain.CharacterStateEntry) error {
	return s.io.WriteJSON(characterStateFile, entries)
}

// UpsertCharacterState 批量 upsert 角色受控状态（原子写入）：
//   - 每个 update 先经 domain.ValidateCharacterStateUpdate 校验；
//   - (entity, field) 唯一键 upsert：新值覆盖旧值，updated_chapter=chapter，evidence 更新；
//   - 新增 (entity, field) 时单实体字段数不得超过 MaxFieldsPerEntity；
//   - 对每个 upsert 派生 StateChange（old_value=旧值，new_value=新值，reason=update.Reason）
//     追加到 meta/state_changes.json，复用 stateChangeKey 幂等去重——同章重复提交不重复 append；
//   - 写入顺序：先追加派生 state_changes（幂等 key 保证重试不重复），再写
//     character_state.json——任何单点失败后重试可收敛（见函数内注释）。
func (s *WorldStore) UpsertCharacterState(chapter int, updates []domain.CharacterStateUpdate) error {
	return s.io.WithWriteLock(func() error {
		var entries []domain.CharacterStateEntry
		if err := s.io.ReadJSONUnlocked(characterStateFile, &entries); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("load character state: %w", err)
			}
		}
		idx := make(map[string]int, len(entries))
		fieldCount := make(map[string]int, len(entries))
		for i, e := range entries {
			idx[e.Entity+"\x00"+e.Field] = i
			fieldCount[e.Entity]++
		}
		var derived []domain.StateChange
		for _, u := range updates {
			if err := domain.ValidateCharacterStateUpdate(u); err != nil {
				return fmt.Errorf("%w: %v", errs.ErrToolArgs, err)
			}
			key := u.Entity + "\x00" + u.Field
			oldValue := ""
			if i, ok := idx[key]; ok {
				oldValue = entries[i].Value
				entries[i].Value = u.Value
				entries[i].UpdatedChapter = chapter
				entries[i].Evidence = u.Evidence
			} else {
				if fieldCount[u.Entity] >= domain.MaxFieldsPerEntity {
					return fmt.Errorf("%w: %s 字段数已达上限 %d，拒绝新增 %s", errs.ErrToolArgs, u.Entity, domain.MaxFieldsPerEntity, u.Field)
				}
				idx[key] = len(entries)
				entries = append(entries, domain.CharacterStateEntry{
					Entity:         u.Entity,
					Field:          u.Field,
					Value:          u.Value,
					UpdatedChapter: chapter,
					Evidence:       u.Evidence,
				})
				fieldCount[u.Entity]++
			}
			if oldValue != u.Value {
				derived = append(derived, domain.StateChange{
					Chapter:  chapter,
					Entity:   u.Entity,
					Field:    u.Field,
					OldValue: oldValue,
					NewValue: u.Value,
					Reason:   u.Reason,
				})
			}
		}
		// 写入顺序依赖（已决策，防部分写入不可收敛）：
		// 1) 先写派生 state_changes（stateChangeKey 幂等去重，同章同值重试不重复追加）；
		// 2) 再写 character_state.json。
		// 收敛性：流水写失败 → 状态文件未动 → 重试全量重做；状态写失败 → 流水已写
		// （重试时 key 去重不重复）+ 状态重写。任何单点失败后重试都能收敛到一致。
		if err := s.appendStateChangesUnlocked(derived); err != nil {
			return fmt.Errorf("append derived state changes: %w", err)
		}
		if err := s.io.WriteJSONUnlocked(characterStateFile, entries); err != nil {
			return fmt.Errorf("save character state: %w", err)
		}
		return nil
	})
}
