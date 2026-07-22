package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// SavePlanningArchiveEntryTool 让 ArchitectLong 精确写/删 meta/planning_archive.json
// 中的单条 archive 条目。upsert 无需 long approval；delete 时若 compass.long.open_threads
// 中仍以 [room:<id>] 或疑似 pattern 引用该 room，则拒绝（fail-closed）。
type SavePlanningArchiveEntryTool struct {
	store *store.Store
}

func NewSavePlanningArchiveEntryTool(store *store.Store) *SavePlanningArchiveEntryTool {
	return &SavePlanningArchiveEntryTool{store: store}
}

func (t *SavePlanningArchiveEntryTool) Name() string { return "save_planning_archive_entry" }
func (t *SavePlanningArchiveEntryTool) Description() string {
	return "写入/删除规划存档（meta/planning_archive.json）中的一条条目。kind 当前仅支持 room。action=upsert: 新增或替换，data 必须为 JSON 对象，summary 会持久化，reason 必填。action=delete: 移除条目，data 不可传，且 compass.long.open_threads 中无任何线程（含疑似 marker）引用该 room 时才可删除。"
}
func (t *SavePlanningArchiveEntryTool) Label() string { return "写入规划存档条目" }

func (t *SavePlanningArchiveEntryTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SavePlanningArchiveEntryTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *SavePlanningArchiveEntryTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("action", schema.Enum("操作类型", "upsert", "delete")).Required(),
		schema.Property("kind", schema.String("条目类型（当前仅支持 room）")).Required(),
		schema.Property("id", schema.String("条目 ID，大小写敏感，不 trim/规范化")).Required(),
		schema.Property("data", map[string]any{
			"oneOf": []any{
				map[string]any{"type": "object", "description": "条目数据（upsert 时必须为 JSON 对象，delete 时不可传）"},
				map[string]any{"type": "null"},
			},
		}),
		schema.Property("summary", schema.String("条目摘要，会随条目一起持久化")),
		schema.Property("reason", schema.String("操作理由，upsert/delete 均必填")).Required(),
	)
}

// SavePlanningArchiveEntryArgs 是 save_planning_archive_entry 的输入参数。
type SavePlanningArchiveEntryArgs struct {
	Action  string          `json:"action"`
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Data    json.RawMessage `json:"data,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Reason  string          `json:"reason"`
}

// SavePlanningArchiveEntryResult 是工具的返回结构。
type SavePlanningArchiveEntryResult struct {
	Saved   bool   `json:"saved"`
	Action  string `json:"action"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Summary string `json:"summary,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Status  string `json:"status,omitempty"` // upserted / deleted / rejected
}

// hasAnyWhitespace 检查字符串中是否有任何 Unicode 空白字符。
func hasAnyWhitespace(s string) bool {
	return strings.IndexFunc(s, unicode.IsSpace) >= 0
}

func (t *SavePlanningArchiveEntryTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a SavePlanningArchiveEntryArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}

	// ── Validation ──
	// 严格：不 TrimSpace/不大小写规范化；含多余空白的任何字段拒绝。
	if a.Action != "upsert" && a.Action != "delete" {
		return nil, fmt.Errorf("action must be exactly %q or %q: %w", "upsert", "delete", errs.ErrToolArgs)
	}
	if hasAnyWhitespace(a.Action) {
		return nil, fmt.Errorf("action must not contain whitespace: %w", errs.ErrToolArgs)
	}
	if a.Kind != "room" {
		return nil, fmt.Errorf("kind must be exactly %q: %w", "room", errs.ErrToolArgs)
	}
	if hasAnyWhitespace(a.Kind) {
		return nil, fmt.Errorf("kind must not contain whitespace: %w", errs.ErrToolArgs)
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required: %w", errs.ErrToolArgs)
	}
	if hasAnyWhitespace(a.ID) {
		return nil, fmt.Errorf("id must not contain whitespace: %w", errs.ErrToolArgs)
	}
	if strings.ContainsFunc(a.ID, func(r rune) bool { return r == 0 }) {
		return nil, fmt.Errorf("id must not contain NUL byte: %w", errs.ErrToolArgs)
	}
	if a.Reason == "" {
		return nil, fmt.Errorf("reason is required: %w", errs.ErrToolArgs)
	}

	switch a.Action {
	case "upsert":
		if len(a.Data) == 0 || string(a.Data) == "null" {
			return nil, fmt.Errorf("data is required for upsert: %w", errs.ErrToolArgs)
		}
		// data 必须为 JSON 对象
		var obj map[string]any
		if err := json.Unmarshal(a.Data, &obj); err != nil || obj == nil {
			return nil, fmt.Errorf("data must be a JSON object for upsert: %w", errs.ErrToolArgs)
		}
	case "delete":
		if len(a.Data) > 0 && string(a.Data) != "null" {
			return nil, fmt.Errorf("data must not be provided for delete: %w", errs.ErrToolArgs)
		}
		// 使用 Store 的跨域原子操作：使用 ParseOpenThreadMarkers 严格检查 + 删除
		if err := t.store.DeleteArchiveEntrySafe(a.Kind, a.ID, func(threads []string) error {
			for _, thread := range threads {
				parsed, pErr := ParseOpenThreadMarkers(thread)
				if pErr != nil {
					return fmt.Errorf("open_threads 包含无法解析的条目 %q: %w", thread, pErr)
				}
				for _, rid := range parsed.RoomIDs {
					if rid == a.ID {
						return fmt.Errorf("open_threads 仍有线程引用 %s %q", a.Kind, a.ID)
					}
				}
			}
			return nil
		}); err != nil {
			return json.Marshal(SavePlanningArchiveEntryResult{
				Saved: false, Action: a.Action, Kind: a.Kind, ID: a.ID,
				Summary: a.Summary, Reason: a.Reason,
				Status: "rejected",
			})
		}
		// ── Checkpoint ──
		if _, err := t.store.Checkpoints.AppendArtifact(domain.GlobalScope(), "planning_archive_"+a.Action, "meta/planning_archive.json"); err != nil {
			return nil, fmt.Errorf("checkpoint planning archive %s: %w: %w", a.Action, errs.ErrStoreWrite, err)
		}
		return json.Marshal(SavePlanningArchiveEntryResult{
			Saved: true, Action: a.Action, Kind: a.Kind, ID: a.ID,
			Summary: a.Summary, Reason: a.Reason,
			Status: "deleted",
		})
	}

	// ── Upsert: 持久化 summary ──
	// 当调用方未提供 summary 时保留旧值
	if a.Summary == "" {
		archive, ae := t.store.PlanningArchive.Load()
		if ae == nil && archive != nil {
			for _, e := range archive.Entries {
				if e.Kind == a.Kind && e.ID == a.ID {
					a.Summary = e.Summary
					break
				}
			}
		}
	}
	if err := t.store.PlanningArchive.UpsertEntryWithSummary(a.Kind, a.ID, a.Summary, a.Data); err != nil {
		return nil, fmt.Errorf("upsert planning archive entry: %w: %w", errs.ErrStoreWrite, err)
	}

	// ── Checkpoint ──
	if _, err := t.store.Checkpoints.AppendArtifact(domain.GlobalScope(), "planning_archive_"+a.Action, "meta/planning_archive.json"); err != nil {
		return nil, fmt.Errorf("checkpoint planning archive %s: %w: %w", a.Action, errs.ErrStoreWrite, err)
	}

	return json.Marshal(SavePlanningArchiveEntryResult{
		Saved: true, Action: a.Action, Kind: a.Kind, ID: a.ID,
		Summary: a.Summary, Reason: a.Reason,
		Status: "upserted",
	})
}
