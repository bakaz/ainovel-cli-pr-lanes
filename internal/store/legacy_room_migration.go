package store

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ── compass raw JSON types ──

// compassRaw 是 compass JSON 的原始映射，保留所有未知字段。
type compassRaw struct {
	root map[string]json.RawMessage // 顶层
	long map[string]json.RawMessage // long 子对象
}

// parseCompassRaw 读取 compass 文件并解析为 raw maps。
// 返回 nil root 表示文件不存在。
func parseCompassRaw(io *IO) (*compassRaw, error) {
	raw, err := io.ReadFileUnlocked("meta/compass.json")
	if err != nil {
		return nil, fmt.Errorf("read compass: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("compass 不是合法 JSON: %v", err)
	}
	var long map[string]json.RawMessage
	if lr, ok := root["long"]; ok {
		if err := json.Unmarshal(lr, &long); err != nil {
			return nil, fmt.Errorf("compass.long 不是合法 JSON 对象: %v", err)
		}
	}
	return &compassRaw{root: root, long: long}, nil
}

// reference 返回 compass.long 中的 reference 原始 JSON。
func (c *compassRaw) reference() json.RawMessage {
	if c.long == nil {
		return nil
	}
	return c.long["reference"]
}

// updateReference 更新 compass.long.reference 为 cleanedRef。
// 若 cleanedRef 为 nil 则删除 reference 字段。
// 返回更新后的 compass 字节。
func (c *compassRaw) updateReference(cleanedRef json.RawMessage) ([]byte, error) {
	if c.long == nil {
		c.long = make(map[string]json.RawMessage)
	}
	if cleanedRef != nil {
		c.long["reference"] = cleanedRef
	} else {
		delete(c.long, "reference")
	}
	longData, err := json.Marshal(c.long)
	if err != nil {
		return nil, fmt.Errorf("序列化 compass.long 失败: %w", err)
	}
	c.root["long"] = longData
	out, err := json.MarshalIndent(c.root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 compass 失败: %w", err)
	}
	return out, nil
}

// MigrateLegacyRooms 将 compass.long.reference 中的 legacy
// detailed_plan.long_rooms[].room 迁移至 meta/planning_archive.json 的
// kind:"room" 条目。
//
// 行为契约：
//   - Archive 不存在且有 legacy rooms 时执行迁移：原子写 archive → 重读验证 →
//     从 Reference 删除 long_rooms。返回 Status="migrated"。
//   - Archive 已存在且 legacy rooms 仍存在于 Reference 中时：
//     先执行 canonical ID 唯一性/冲突检查（int 3 与 string "3" 视为冲突），
//     再逐条验证每个 legacy canonical ID 在 archive 中有正文等价的 entry，
//     全部匹配后才清理 legacy；任一不匹配则 fail closed（Status="conflict"）。
//     不覆盖 / 不合并 Archive。返回 Status="cleaned_up"。
//   - Archive 已存在且无 legacy rooms 时返回 Status="already_exists"。
//   - Archive 不存在且无 legacy rooms 时返回 Status="no_legacy_rooms"。
//   - Canonical ID 冲突（int 3 和 string "3"）时 fail closed → Status="conflict"。
//   - 幂等：重复调用安全；不会覆盖已有 Archive。
//   - 只删除 Reference 中的 long_rooms；其他 compass 未知字段通过原始 JSON 映射
//     原样保留（顶层、long、current 中的未知字段均不受影响）。
//   - 写入 archive 前 / 重读后均 Validate；invalid/v2/duplicate → fail closed。
//
// 此操作是跨 store 原子操作（crossMu + Outline.io + PlanningArchive.io）。
// 不触发 long 审批。
func (s *Store) MigrateLegacyRooms() (domain.MigrateLegacyRoomsResult, error) {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()

	s.Outline.io.mu.Lock()
	defer s.Outline.io.mu.Unlock()

	s.PlanningArchive.io.mu.Lock()
	defer s.PlanningArchive.io.mu.Unlock()

	return s.migrateLegacyRoomsLocked()
}

func (s *Store) migrateLegacyRoomsLocked() (domain.MigrateLegacyRoomsResult, error) {
	// ── 1. 读取 compass（原始 JSON，保留未知字段） ──
	cr, err := parseCompassRaw(s.Outline.io)
	if err != nil {
		return resultErr("parse compass: %v", err)
	}
	refRaw := cr.reference()

	// ── 2. 用可复用 extractor 解析 legacy rooms ──
	rooms, cleanedRef, found, err := domain.ExtractLegacyRoomsFromReference(refRaw)
	if err != nil {
		return resultErr("extract legacy rooms: %v", err)
	}

	// ── 3. 读取 archive（loadUnlocked 已内置 Validate） ──
	archive, aErr := s.PlanningArchive.loadUnlocked()
	if aErr != nil {
		return resultErr("load archive: %v", aErr)
	}

	// ── 4. 无 legacy rooms ──
	if !found || len(rooms) == 0 {
		// 即使没有 rooms，如果 cleanedRef 不同于原 reference（例如空/null long_rooms
		// 被清理），也需要保存更新后的 reference。
		if !bytes.Equal(cleanedRef, refRaw) {
			if err := s.saveCompassRawLocked(cr, cleanedRef); err != nil {
				return resultErr("save compass after cleaning empty long_rooms: %v", err)
			}
		}
		if archive != nil {
			return domain.MigrateLegacyRoomsResult{Status: "already_exists"}, nil
		}
		return domain.MigrateLegacyRoomsResult{Status: "no_legacy_rooms"}, nil
	}

	// ── 5. 有 legacy rooms + archive 已存在 ──
	if archive != nil {
		// 必须先执行 canonical ID 唯一性/冲突检查。
		if _, err := domain.ConvertLegacyRooms(rooms); err != nil {
			return domain.MigrateLegacyRoomsResult{
				Status:  "conflict",
				Message: fmt.Sprintf("canonical id 冲突，拒绝清理: %v", err),
			}, nil
		}
		return s.cleanupLegacyWithVerificationLocked(rooms, cleanedRef, cr, archive)
	}

	// ── 6. 有 legacy rooms + archive 不存在：执行迁移 ──
	entries, err := domain.ConvertLegacyRooms(rooms)
	if err != nil {
		return domain.MigrateLegacyRoomsResult{
			Status:  "conflict",
			Message: err.Error(),
		}, nil
	}

	newArchive := &domain.PlanningArchiveV1{
		Schema:  "ainovel.planning-archive",
		Version: 1,
		Entries: entries,
	}

	// 写入前 Validate
	if err := newArchive.Validate(); err != nil {
		return resultErr("validate new archive: %v", err)
	}

	// 原子写 archive
	if err := s.PlanningArchive.io.WriteJSONUnlocked(planningArchivePath, newArchive); err != nil {
		return resultErr("write archive: %v", err)
	}

	// 重读验证（完整性校验）
	var reread domain.PlanningArchiveV1
	if err := s.PlanningArchive.io.ReadJSONUnlocked(planningArchivePath, &reread); err != nil {
		return resultErr("verify archive after write: %v", err)
	}
	if err := reread.Validate(); err != nil {
		return resultErr("archive validation after write: %v", err)
	}
	if len(reread.Entries) != len(entries) {
		return resultErr("archive entry count mismatch after write: got %d, want %d",
			len(reread.Entries), len(entries))
	}

	// 更新 compass Reference（去掉 long_rooms）
	if err := s.saveCompassRawLocked(cr, cleanedRef); err != nil {
		return resultErr("save compass after migration: %v", err)
	}

	return domain.MigrateLegacyRoomsResult{
		Status:       "migrated",
		EntriesCount: len(entries),
		Message:      fmt.Sprintf("成功迁移 %d 个 legacy room 至 planning archive", len(entries)),
	}, nil
}

// cleanupLegacyWithVerificationLocked 在 archive 已存在时逐条验证每个 legacy room
// 已在 archive 中有正文等价的 entry，然后才清理 legacy。
// 调用前须确保 canonical ID 无冲突（已通过 ConvertLegacyRooms 检查）。
// 任一不匹配则 fail closed。
func (s *Store) cleanupLegacyWithVerificationLocked(
	rooms []domain.LegacyRoomRef,
	cleanedRef jsonRaw,
	cr *compassRaw,
	archive *domain.PlanningArchiveV1,
) (domain.MigrateLegacyRoomsResult, error) {
	// 验证 archive 自身完整性（loadUnlocked 已执行，此处二次确认）
	if err := archive.Validate(); err != nil {
		return resultErr("existing archive 校验失败: %v", err)
	}

	// 构建 archive 索引
	index := makeArchiveRoomIndex(archive)

	// 逐条验证（仅比较 Kind/ID/Data，忽略 Summary——派生逻辑不同不应导致冲突）
	for _, room := range rooms {
		want := domain.PlanningArchiveEntry{
			Kind: "room",
			ID:   room.ID,
			Data: room.Data,
		}
		got, ok := index[room.ID]
		if !ok {
			return domain.MigrateLegacyRoomsResult{
				Status:  "conflict",
				Message: fmt.Sprintf("legacy room %q 未在 archive 中找到对应 entry，拒绝清理", room.ID),
			}, nil
		}
		if !domain.LegacyEntryDataEqual(got, want) {
			return domain.MigrateLegacyRoomsResult{
				Status:  "conflict",
				Message: fmt.Sprintf("legacy room %q 的 archive entry 正文不匹配，拒绝清理", room.ID),
			}, nil
		}
	}

	// 全部匹配 → 清理 legacy（通过 raw compass 更新）
	if err := s.saveCompassRawLocked(cr, cleanedRef); err != nil {
		return resultErr("save compass after cleanup: %v", err)
	}

	return domain.MigrateLegacyRoomsResult{
		Status:       "cleaned_up",
		EntriesCount: len(archive.Entries),
		Message:      fmt.Sprintf("archive 已存在，已验证 %d 个 legacy room 覆盖一致并清理 long_rooms", len(rooms)),
	}, nil
}

// makeArchiveRoomIndex 构建 (kind="room", id) → entry 索引。
func makeArchiveRoomIndex(archive *domain.PlanningArchiveV1) map[string]domain.PlanningArchiveEntry {
	idx := make(map[string]domain.PlanningArchiveEntry, len(archive.Entries))
	for _, e := range archive.Entries {
		if e.Kind == "room" && e.ID != "" {
			idx[e.ID] = e
		}
	}
	return idx
}

// saveCompassRawLocked 将清理后的 reference 写回 compass（原始 JSON 映射方式，
// 保留所有未知字段）。
func (s *Store) saveCompassRawLocked(cr *compassRaw, newRef jsonRaw) error {
	out, err := cr.updateReference(newRef)
	if err != nil {
		return err
	}
	return s.Outline.io.WriteFileUnlocked("meta/compass.json", out)
}

// jsonRaw 是 json.RawMessage 的类型别名（减少 import 路径长度）。
type jsonRaw = []byte

func resultErr(format string, args ...any) (domain.MigrateLegacyRoomsResult, error) {
	msg := fmt.Sprintf(format, args...)
	return domain.MigrateLegacyRoomsResult{Status: "error", Message: msg}, fmt.Errorf("legacy room migration: %s", msg)
}
