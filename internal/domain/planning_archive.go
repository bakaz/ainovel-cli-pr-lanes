package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ── v1 Archive ──

// PlanningArchiveV1 是 meta/planning_archive.json v1 的领域模型。
// Schema 固定为 "ainovel.planning-archive"，Version 固定为 1。
// 未知顶层字段通过 Extra 保留。
type PlanningArchiveV1 struct {
	Schema  string                `json:"schema"`
	Version int                   `json:"version"`
	Entries []PlanningArchiveEntry `json:"entries"`
	Extra   json.RawMessage       `json:"-"` // 未知顶层字段
}

// entryKey 是 (Kind, ID) 的无碰撞复合键。
type entryKey struct{ Kind, ID string }

// Validate 校验 archive 的完整性与一致性：
//   - Schema 必须为 "ainovel.planning-archive"
//   - Version 必须为 1
//   - 所有 entry 的 Kind 和 ID 非空
//   - (Kind, ID) 在全 archive 内唯一
func (a *PlanningArchiveV1) Validate() error {
	if a.Schema != "ainovel.planning-archive" {
		return fmt.Errorf("planning archive: invalid schema %q", a.Schema)
	}
	if a.Version != 1 {
		return fmt.Errorf("planning archive: invalid version %d", a.Version)
	}
	seen := make(map[entryKey]bool)
	for i, e := range a.Entries {
		if e.Kind == "" {
			return fmt.Errorf("planning archive: entry %d has empty kind", i)
		}
		if e.ID == "" {
			return fmt.Errorf("planning archive: entry %d has empty id", i)
		}
		key := entryKey{Kind: e.Kind, ID: e.ID}
		if seen[key] {
			return fmt.Errorf("planning archive: duplicate entry (kind=%q, id=%q)", e.Kind, e.ID)
		}
		seen[key] = true
	}
	return nil
}

// ── custom JSON for top-level Extra preservation ──

type planningArchiveWire struct {
	Schema  string                `json:"schema"`
	Version int                   `json:"version"`
	Entries []PlanningArchiveEntry `json:"entries"`
}

// LegacyEntryEqual 比较两个 archive entry 正文是否等价（忽略 Extra）。
// 两个 entry 的 Kind、ID、Summary、Data 经 JSON 规范化后比较。
func LegacyEntryEqual(a, b PlanningArchiveEntry) bool {
	if a.Kind != b.Kind || a.ID != b.ID || a.Summary != b.Summary {
		return false
	}
	return jsonBytesEqual(a.Data, b.Data)
}

// jsonBytesEqual 比较两段 JSON 的语义等价性（规范化后逐字节比较）。
// 使用 json.Decoder.UseNumber 避免 >53 位整数精度丢失。
func jsonBytesEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var va, vb any
	decA := json.NewDecoder(strings.NewReader(string(a)))
	decA.UseNumber()
	if err := decA.Decode(&va); err != nil {
		return false
	}
	decB := json.NewDecoder(strings.NewReader(string(b)))
	decB.UseNumber()
	if err := decB.Decode(&vb); err != nil {
		return false
	}
	normA, _ := json.Marshal(va)
	normB, _ := json.Marshal(vb)
	return string(normA) == string(normB)
}

func (a *PlanningArchiveV1) MarshalJSON() ([]byte, error) {
	base := planningArchiveWire{
		Schema:  a.Schema,
		Version: a.Version,
		Entries: a.Entries,
	}
	baseData, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	if len(a.Extra) == 0 {
		return baseData, nil
	}
	// Merge base with Extra (Extra fields should not override known fields)
	var baseMap map[string]json.RawMessage
	if err := json.Unmarshal(baseData, &baseMap); err != nil {
		return nil, err
	}
	var extraMap map[string]json.RawMessage
	if err := json.Unmarshal(a.Extra, &extraMap); err != nil {
		return nil, err
	}
	for k, v := range extraMap {
		if _, exists := baseMap[k]; !exists {
			baseMap[k] = v
		}
	}
	return json.Marshal(baseMap)
}

func (a *PlanningArchiveV1) UnmarshalJSON(data []byte) error {
	var wire planningArchiveWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("planning archive: unmarshal wire: %w", err)
	}
	a.Schema = wire.Schema
	a.Version = wire.Version
	a.Entries = wire.Entries

	// Detect unknown top-level fields
	var full map[string]json.RawMessage
	if err := json.Unmarshal(data, &full); err != nil {
		return fmt.Errorf("planning archive: unmarshal full: %w", err)
	}
	known := map[string]bool{"schema": true, "version": true, "entries": true}
	extra := make(map[string]json.RawMessage)
	for k, v := range full {
		if !known[k] {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		raw, err := json.Marshal(extra)
		if err != nil {
			return fmt.Errorf("planning archive: marshal extra: %w", err)
		}
		a.Extra = raw
	}
	return nil
}

// ── Entry ──

// PlanningArchiveEntry 是 PlanningArchiveV1 中的一个条目。
// (Kind, ID) 构成唯一标识，两者均不可为空。
// Summary 是条目的便捷摘要，从原始数据派生（不覆盖原始数据中的同名字段）。
// Data 存放条目的具体负载，未改动条目通过 RawMessage 原样保留。
// 未知 entry-level 字段通过 Extra 保留。
type PlanningArchiveEntry struct {
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Summary string          `json:"summary,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Extra   json.RawMessage `json:"-"` // 未知 entry-level 字段
}

type planningArchiveEntryWire struct {
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Summary string          `json:"summary,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *PlanningArchiveEntry) MarshalJSON() ([]byte, error) {
	base := planningArchiveEntryWire{Kind: e.Kind, ID: e.ID, Summary: e.Summary, Data: e.Data}
	baseData, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	if len(e.Extra) == 0 {
		return baseData, nil
	}
	var baseMap map[string]json.RawMessage
	if err := json.Unmarshal(baseData, &baseMap); err != nil {
		return nil, err
	}
	var extraMap map[string]json.RawMessage
	if err := json.Unmarshal(e.Extra, &extraMap); err != nil {
		return nil, err
	}
	for k, v := range extraMap {
		if _, exists := baseMap[k]; !exists {
			baseMap[k] = v
		}
	}
	return json.Marshal(baseMap)
}

func (e *PlanningArchiveEntry) UnmarshalJSON(data []byte) error {
	var wire planningArchiveEntryWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("planning archive entry: unmarshal wire: %w", err)
	}
	e.Kind = wire.Kind
	e.ID = wire.ID
	e.Summary = wire.Summary
	e.Data = wire.Data

	var full map[string]json.RawMessage
	if err := json.Unmarshal(data, &full); err != nil {
		return fmt.Errorf("planning archive entry: unmarshal full: %w", err)
	}
	known := map[string]bool{"kind": true, "id": true, "summary": true, "data": true}
	extra := make(map[string]json.RawMessage)
	for k, v := range full {
		if !known[k] {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		raw, err := json.Marshal(extra)
		if err != nil {
			return fmt.Errorf("planning archive entry: marshal extra: %w", err)
		}
		e.Extra = raw
	}
	return nil
}
