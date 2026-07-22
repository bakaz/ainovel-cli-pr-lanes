package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ── Legacy room ref (post-extraction) ──

// LegacyRoomRef 是提取后的 legacy room 条目。
// ID 是 canonical string，Data 是原 room 对象去掉 .room key 后的剩余部分。
type LegacyRoomRef struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ── Reusable legacy extractor ──

// ExtractLegacyRoomsFromReference 解析 compass.long.reference 中的 legacy
// detailed_plan.long_rooms[].room 条目。每条 room 的 .room key（int 或 string）
// 转为 canonical string ID；原 room 对象剩余字段（不含 .room）作为 data 原样保留。
//
// 返回值：
//   - rooms：提取的 legacy rooms（无 legacy 时为空 nil）
//   - cleanedRef：去掉 long_rooms 后的 reference（无变化时等于原 ref）
//   - found：是否找到并提取了 legacy rooms
//   - err：解析错误
//
// 这是唯一 legacy 解析入口，tools 和 store 均应复用此函数。
func ExtractLegacyRoomsFromReference(ref json.RawMessage) (rooms []LegacyRoomRef, cleanedRef json.RawMessage, found bool, err error) {
	if len(ref) == 0 {
		return nil, ref, false, nil
	}

	var refMap map[string]json.RawMessage
	if err := json.Unmarshal(ref, &refMap); err != nil {
		return nil, nil, false, fmt.Errorf("reference 不是合法 JSON 对象: %w", err)
	}

	dpRaw, ok := refMap["detailed_plan"]
	if !ok {
		return nil, ref, false, nil // 无 detailed_plan，不是 legacy 格式
	}

	var dpMap map[string]json.RawMessage
	if err := json.Unmarshal(dpRaw, &dpMap); err != nil {
		return nil, nil, false, fmt.Errorf("detailed_plan 不是合法 JSON 对象: %w", err)
	}

	lrRaw, ok := dpMap["long_rooms"]
	if !ok {
		return nil, ref, false, nil // 无 long_rooms
	}

	// long_rooms 必须是数组或 null
	if string(lrRaw) == "null" {
		// null / 空 → 清理空字段，不返回 rooms
		cleaned, _ := removeLongRoomsFromMaps(refMap, dpMap)
		return nil, cleaned, false, nil
	}

	var rawRooms []json.RawMessage
	if err := json.Unmarshal(lrRaw, &rawRooms); err != nil {
		return nil, nil, false, fmt.Errorf("long_rooms 不是合法 JSON 数组: %w", err)
	}
	if len(rawRooms) == 0 {
		cleaned, _ := removeLongRoomsFromMaps(refMap, dpMap)
		return nil, cleaned, false, nil
	}

	// 解析每条 room
	rooms = make([]LegacyRoomRef, 0, len(rawRooms))
	for i, roomRaw := range rawRooms {
		var roomObj map[string]json.RawMessage
		if err := json.Unmarshal(roomRaw, &roomObj); err != nil {
			return nil, nil, false, fmt.Errorf("long_rooms[%d] 不是 JSON 对象: %w", i, err)
		}

		roomIDRaw, hasRoom := roomObj["room"]
		if !hasRoom {
			return nil, nil, false, fmt.Errorf("long_rooms[%d] 缺少 'room' key", i)
		}

		idStr, err := convertRawRoomID(roomIDRaw)
		if err != nil {
			return nil, nil, false, fmt.Errorf("long_rooms[%d].room: %w", i, err)
		}

		// 删除 .room key，剩余字段作为 data 原样保留
		delete(roomObj, "room")
		var dataRaw json.RawMessage
		if len(roomObj) > 0 {
			dataRaw, err = json.Marshal(roomObj)
			if err != nil {
				return nil, nil, false, fmt.Errorf("long_rooms[%d] 序列化 data 失败: %w", i, err)
			}
		}

		rooms = append(rooms, LegacyRoomRef{ID: idStr, Data: dataRaw})
	}

	// 生成 cleaned reference（去掉 long_rooms）
	cleanedRef, err = removeLongRoomsFromMaps(refMap, dpMap)
	if err != nil {
		return nil, nil, false, err
	}
	return rooms, cleanedRef, true, nil
}

// removeLongRoomsFromMaps 从 dpMap 删除 long_rooms，更新 refMap。
// dpMap long_rooms 删除后若为空则从 refMap 删除 detailed_plan。
// refMap 空时返回 nil reference。
func removeLongRoomsFromMaps(refMap, dpMap map[string]json.RawMessage) (json.RawMessage, error) {
	delete(dpMap, "long_rooms")
	if len(dpMap) == 0 {
		delete(refMap, "detailed_plan")
	} else {
		newDP, err := json.Marshal(dpMap)
		if err != nil {
			return nil, fmt.Errorf("序列化 detailed_plan 失败: %w", err)
		}
		refMap["detailed_plan"] = newDP
	}
	if len(refMap) == 0 {
		return nil, nil
	}
	out, err := json.Marshal(refMap)
	if err != nil {
		return nil, fmt.Errorf("序列化 reference 失败: %w", err)
	}
	return out, nil
}

// ── Room ID conversion ──

// convertRawRoomID 将 .room 的原始 JSON 值（int 或 string）转为 canonical string。
// string 值不得包含任何 Unicode whitespace（TrimSpace 静默清理改为显式拒绝）。
// 空值、不支持的类型返回错误。
func convertRawRoomID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("room id 为空")
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", fmt.Errorf("room id 为空字符串")
		}
		// 拒绝任何 Unicode whitespace（首尾或中间均不允许）
		for _, r := range s {
			if unicode.IsSpace(r) {
				return "", fmt.Errorf("room id 包含空白字符: %q", s)
			}
		}
		return s, nil
	}
	// Try int
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatInt(n, 10), nil
	}
	// 其他类型必须显式拒绝（非对象/未知值绝不能静默置空）
	return "", fmt.Errorf("room id 必须是 int 或 string，实际值: %s", string(raw))
}

// ── Summary extraction ──

// ExtractSummaryFromRoomData 从 room data 中提取摘要字符串。
// 优先用 title，其次 name；都不存在时返回空字符串。
// 不修改 data，不覆盖已有字段。
func ExtractSummaryFromRoomData(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	for _, key := range []string{"title", "name"} {
		if raw, ok := m[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// LegacyEntryDataEqual 比较两条 archive entry 的正文是否等价（忽略 Summary）。
// 用于 archive 已存在时的 legacy 覆盖验证——由不同派生逻辑产生的 summary
// 差异不应当阻止 legacy 清理。
func LegacyEntryDataEqual(a, b PlanningArchiveEntry) bool {
	if a.Kind != b.Kind || a.ID != b.ID {
		return false
	}
	return jsonBytesEqual(a.Data, b.Data)
}

// ── Conversion ──

// ConvertLegacyRoom 将一条 LegacyRoomRef 转为 archive entry。
// entry Kind="room"，Summary 从 data 的 title/name 派生，Data 原样保留。
func ConvertLegacyRoom(ref LegacyRoomRef) (PlanningArchiveEntry, error) {
	if ref.ID == "" {
		return PlanningArchiveEntry{}, fmt.Errorf("legacy room id 为空")
	}
	summary := ExtractSummaryFromRoomData(ref.Data)
	return PlanningArchiveEntry{
		Kind:    "room",
		ID:      ref.ID,
		Summary: summary,
		Data:    ref.Data, // 原样保留，不修改
	}, nil
}

// ConvertLegacyRooms 批量转换 legacy rooms。
// 检查 canonical ID 唯一性：int 3 与 string "3" 视为冲突 → fail closed。
// 同时检查 (Kind, ID) 重复。
func ConvertLegacyRooms(refs []LegacyRoomRef) ([]PlanningArchiveEntry, error) {
	seen := make(map[string]int)
	entries := make([]PlanningArchiveEntry, 0, len(refs))
	for i, ref := range refs {
		entry, err := ConvertLegacyRoom(ref)
		if err != nil {
			return nil, fmt.Errorf("legacy room at index %d: %w", i, err)
		}
		if prevIdx, ok := seen[entry.ID]; ok {
			return nil, fmt.Errorf("canonical id conflict: index %d and %d both produce id %q", prevIdx, i, entry.ID)
		}
		seen[entry.ID] = i
		entries = append(entries, entry)
	}
	return entries, nil
}

// ── Result ──

// MigrateLegacyRoomsResult 描述迁移结果。
type MigrateLegacyRoomsResult struct {
	Status       string `json:"status"`                 // migrated / already_exists / no_legacy_rooms / cleaned_up / conflict / error
	EntriesCount int    `json:"entries_count,omitempty"` // 仅 migrated/cleaned_up 时有值
	Message      string `json:"message,omitempty"`       // 人类可读详情
}
