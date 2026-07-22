package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

const maxPlanningArchiveRefs = 8

// ReadPlanningArchiveTool 让 ArchitectLong 按需精确读取 meta/planning_archive.json
// 中的存档条目。当 archive 不存在时自动回退读取 compass.long.reference 中的
// legacy detailed_plan.long_rooms。单次最多 8 条 ref，去重保留顺序。
type ReadPlanningArchiveTool struct {
	store *store.Store
}

func NewReadPlanningArchiveTool(store *store.Store) *ReadPlanningArchiveTool {
	return &ReadPlanningArchiveTool{store: store}
}

func (t *ReadPlanningArchiveTool) Name() string { return "read_planning_archive" }
func (t *ReadPlanningArchiveTool) Description() string {
	return "读取规划存档（meta/planning_archive.json）中指定 kind+id 条目的详细数据。一次最多请求 8 条。返回 status 表示整体结果。"
}
func (t *ReadPlanningArchiveTool) Label() string { return "读取规划存档" }

func (t *ReadPlanningArchiveTool) ReadOnly(_ json.RawMessage) bool        { return true }
func (t *ReadPlanningArchiveTool) ConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *ReadPlanningArchiveTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("refs", schema.Array("要读取的存档条目引用列表，每条含 kind 和 id；最多 8 条，重复的 (kind,id) 自动去重",
			schema.Object(
				schema.Property("kind", schema.String("条目类型（如 room）")).Required(),
				schema.Property("id", schema.String("条目 ID，大小写敏感）")).Required(),
			),
		)),
	)
}

// ReadPlanningArchiveResult 是 read_planning_archive 的返回结构。
type ReadPlanningArchiveResult struct {
	Status string                    `json:"status"` // ok / partial / not_found / archive_absent / unsupported_version / invalid_archive
	Hint   string                    `json:"_hint,omitempty"`
	Refs   []PlanningRef             `json:"refs,omitempty"`   // 去重后的请求
	Found  []domain.PlanningArchiveEntry `json:"found,omitempty"` // 匹配的条目
	Missing []PlanningRef            `json:"missing,omitempty"`   // 未匹配的 ref
}

func (t *ReadPlanningArchiveTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Refs []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"refs"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}

	// 严格校验：kind/id 均不可为空/仅空白/含 NUL；任一无效拒绝整个请求。
	for i, r := range a.Refs {
		if r.Kind == "" {
			return nil, fmt.Errorf("refs[%d].kind 为空: %w", i, errs.ErrToolArgs)
		}
		if strings.TrimSpace(r.Kind) == "" {
			return nil, fmt.Errorf("refs[%d].kind 仅空白: %w", i, errs.ErrToolArgs)
		}
		if strings.ContainsRune(r.Kind, 0) {
			return nil, fmt.Errorf("refs[%d].kind 含 NUL 字节: %w", i, errs.ErrToolArgs)
		}
		if r.ID == "" {
			return nil, fmt.Errorf("refs[%d].id 为空: %w", i, errs.ErrToolArgs)
		}
		if strings.TrimSpace(r.ID) == "" {
			return nil, fmt.Errorf("refs[%d].id 仅空白: %w", i, errs.ErrToolArgs)
		}
		if strings.ContainsRune(r.ID, 0) {
			return nil, fmt.Errorf("refs[%d].id 含 NUL 字节: %w", i, errs.ErrToolArgs)
		}
	}

	// 去重保留顺序
	refs := dedupPlanningRefs(a.Refs)
	if len(refs) == 0 {
		return nil, fmt.Errorf("至少请求一条 ref: %w", errs.ErrToolArgs)
	}
	if len(refs) > maxPlanningArchiveRefs {
		return nil, fmt.Errorf("一次最多请求 %d 条 ref，当前 %d 条: %w", maxPlanningArchiveRefs, len(refs), errs.ErrToolArgs)
	}

	archive, err := t.store.PlanningArchive.Load()
	if err != nil {
		return nil, fmt.Errorf("load planning archive: %w", err)
	}

	// ── Archive 不存在 ──
	if archive == nil {
		return t.legacyFallback(refs)
	}

	// ── Archive 存在但无效/版本不匹配 ──
	// 严格 fail-closed：任何验证错误（schema/version/空kind/空id/duplicate）均拒绝读取。
	if archive.Schema != "ainovel.planning-archive" {
		return t.makeInvalidResult("unsupported_version",
			fmt.Sprintf("archive schema %q 不受支持", archive.Schema))
	}
	if archive.Version != 1 {
		return t.makeInvalidResult("unsupported_version",
			fmt.Sprintf("archive version %d 不受支持", archive.Version))
	}
	if err := archive.Validate(); err != nil {
		return t.makeInvalidResult("invalid_archive", err.Error())
	}

	// ── 在 entries 中精确查找 ──
	// 使用防碰撞键：kind + "\x00" + id 不会因 kind="" 或 id="" 产生误匹配
	result := t.searchArchive(archive.Entries, refs)
	return json.Marshal(result)
}

// ── Archive 缺失时的 legacy 回退（复用 domain.ExtractLegacyRoomsFromReference）──

func (t *ReadPlanningArchiveTool) legacyFallback(refs []PlanningRef) (json.RawMessage, error) {
	// 只对 kind=="room" 的 ref 做 legacy 回退
	var roomRefs []PlanningRef
	for _, r := range refs {
		if r.Kind == "room" {
			roomRefs = append(roomRefs, r)
		}
	}
	if len(roomRefs) == 0 {
		return t.makeStatusResult("archive_absent",
			"archive 不存在且无 room 类型 ref 可回退 legacy")
	}

	compass, err := t.store.Outline.LoadCompass()
	if err != nil || compass == nil {
		return t.makeStatusResult("archive_absent",
			"archive 不存在且 compass 不可用，无法回退 legacy")
	}

	rooms, _, found, err := domain.ExtractLegacyRoomsFromReference(compass.Long.Reference)
	if err != nil || !found || len(rooms) == 0 {
		return t.makeStatusResult("archive_absent",
			"archive 不存在且 compass.long.reference 无 legacy room 数据")
	}

	// 构建 roomID → data 索引
	roomIndex := make(map[string]domain.LegacyRoomRef, len(rooms))
	for _, room := range rooms {
		if room.ID != "" {
			roomIndex[room.ID] = room
		}
	}

	result := ReadPlanningArchiveResult{
		Status:  "archive_absent",
		Refs:    refs,
		Found:   make([]domain.PlanningArchiveEntry, 0, len(roomRefs)),
		Missing: make([]PlanningRef, 0),
	}

	for _, r := range roomRefs {
		if room, ok := roomIndex[r.ID]; ok {
			result.Found = append(result.Found, domain.PlanningArchiveEntry{
				Kind: r.Kind,
				ID:   r.ID,
				Data: room.Data,
			})
		} else {
			result.Missing = append(result.Missing, r)
		}
	}

	// 升级 status：roomRefs 中有些 found 有些 missing
	if len(result.Found) > 0 && len(result.Missing) > 0 {
		result.Status = "partial"
		result.Hint = fmt.Sprintf("从 legacy fallback 找到 %d 条 room，%d 条未找到", len(result.Found), len(result.Missing))
	} else if len(result.Found) > 0 {
		result.Status = "ok"
		result.Hint = "从 legacy fallback 读取"
	} else {
		result.Status = "not_found"
		result.Hint = "legacy fallback 中未找到任何请求的 room"
	}

	return json.Marshal(result)
}

// ── Archive 存在时的精确查找 ──

func (t *ReadPlanningArchiveTool) searchArchive(entries []domain.PlanningArchiveEntry, refs []PlanningRef) ReadPlanningArchiveResult {
	// 建立防碰撞索引键：kind + "\x00" + id 能可靠区分空 kind/id
	index := make(map[string]domain.PlanningArchiveEntry, len(entries))
	for _, e := range entries {
		key := archiveEntryKey(e.Kind, e.ID)
		index[key] = e
	}

	result := ReadPlanningArchiveResult{
		Status:  "ok",
		Refs:    refs,
		Found:   make([]domain.PlanningArchiveEntry, 0, len(refs)),
		Missing: make([]PlanningRef, 0),
	}

	for _, r := range refs {
		key := archiveEntryKey(r.Kind, r.ID)
		if e, ok := index[key]; ok {
			result.Found = append(result.Found, e)
		} else {
			result.Missing = append(result.Missing, r)
		}
	}

	if len(result.Found) > 0 && len(result.Missing) > 0 {
		result.Status = "partial"
	} else if len(result.Found) == 0 {
		result.Status = "not_found"
	}

	return result
}

// archiveEntryKey 生成防碰撞索引键：kind + NUL + id。
// NUL 字节不能出现在合法 kind/id 中，因此 (kind,id)=(room,"a/b") 与 (kind,"room/a","b") 不会碰撞。
func archiveEntryKey(kind, id string) string {
	return kind + "\x00" + id
}

// ── Helpers ──

func dedupPlanningRefs(raw []struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}) []PlanningRef {
	seen := make(map[string]bool)
	result := make([]PlanningRef, 0, len(raw))
	for _, r := range raw {
		// 不 trim kind/id——已经过 Execute 严格校验非空/非空白/无 NUL
		key := archiveEntryKey(r.Kind, r.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, PlanningRef{Kind: r.Kind, ID: r.ID})
	}
	return result
}

func (t *ReadPlanningArchiveTool) makeStatusResult(status, hint string) (json.RawMessage, error) {
	out, err := json.Marshal(ReadPlanningArchiveResult{Status: status, Hint: hint})
	return out, err
}

func (t *ReadPlanningArchiveTool) makeInvalidResult(status, hint string) (json.RawMessage, error) {
	out, err := json.Marshal(ReadPlanningArchiveResult{Status: status, Hint: hint})
	return out, err
}
