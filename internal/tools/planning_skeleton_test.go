package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── V3 initial layered_outline: skeleton arc acceptance ──

func TestPlanV3_InitialSkeletonArc(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{ // detailed arc
						"index": 1, "title": "首弧", "goal": "展开世界",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "第一章", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
					{ // skeleton arc
						"index": 2, "title": "发展弧", "goal": "推进主线",
						"estimated_chapters": 5,
					},
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("layered_outline with skeleton arc should succeed: %v", err)
	}

	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if len(volumes) != 1 || len(volumes[0].Arcs) != 2 {
		t.Fatalf("expected 2 arcs, got %d", len(volumes[0].Arcs))
	}
	// Arc 1: detailed (estimated_chapters=0)
	if !volumes[0].Arcs[0].IsExpanded() {
		t.Fatal("arc 1 should be expanded")
	}
	if volumes[0].Arcs[0].EstimatedChapters != 0 {
		t.Fatalf("arc 1 estimated_chapters should be 0, got %d", volumes[0].Arcs[0].EstimatedChapters)
	}
	// Arc 2: skeleton
	if volumes[0].Arcs[1].IsExpanded() {
		t.Fatal("arc 2 should NOT be expanded")
	}
	if volumes[0].Arcs[1].EstimatedChapters != 5 {
		t.Fatalf("arc 2 estimated_chapters should be 5, got %d", volumes[0].Arcs[1].EstimatedChapters)
	}
}

// ── V3 initial layered_outline: first arc skeleton rejected ──

func TestPlanV3_InitialFirstArcSkeletonRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{ // first arc skeleton → rejected
						"index": 1, "title": "首弧", "goal": "展开世界",
						"estimated_chapters": 3,
					},
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("first arc skeleton should be rejected")
	}
	if !strings.Contains(err.Error(), "first arc must be detailed") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// ── V3 layered_outline: skeleton arc with chapters:null or [] ──

func TestPlanV3_SkeletonChaptersNullOrEmpty(t *testing.T) {
	tests := []struct {
		name     string
		chapters any // nil for omitted field
	}{
		{"chapters_omitted", nil},
		{"chapters_null", nil}, // will be set as nil
		{"chapters_empty", []any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := store.NewStore(dir)
			if err := st.Init(); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := st.Progress.Init("test", 0); err != nil {
				t.Fatalf("InitProgress: %v", err)
			}

			volMap := map[string]any{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开世界",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "第一章", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
					{
						"index": 2, "title": "骨架弧", "goal": "推进主线",
						"estimated_chapters": 4,
					},
				},
			}
			// For null/empty tests, override the skeleton arc
			if tc.name == "chapters_null" {
				volMap["arcs"] = []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开世界",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "第一章", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
					{
						"index": 2, "title": "骨架弧", "goal": "推进主线",
						"estimated_chapters": 4,
						"chapters":           nil,
					},
				}
			} else if tc.name == "chapters_empty" {
				volMap["arcs"] = []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开世界",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "第一章", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
					{
						"index": 2, "title": "骨架弧", "goal": "推进主线",
						"estimated_chapters": 4,
						"chapters":           []any{},
					},
				}
			}

			tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
			args, _ := json.Marshal(map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{volMap},
			})
			if _, err := tool.Execute(context.Background(), args); err != nil {
				t.Fatalf("skeleton arc with %s should succeed: %v", tc.name, err)
			}

			volumes, err := st.Outline.LoadLayeredOutline()
			if err != nil {
				t.Fatalf("LoadLayeredOutline: %v", err)
			}
			if !volumes[0].Arcs[1].IsExpanded() && volumes[0].Arcs[1].EstimatedChapters != 4 {
				t.Fatalf("expected skeleton arc with estimated_chapters=4, got expanded=%v est=%d",
					volumes[0].Arcs[1].IsExpanded(), volumes[0].Arcs[1].EstimatedChapters)
			}
		})
	}
}

// ── V3 append_volume with skeleton arc ──

func TestPlanV3_AppendSkeletonArc(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	// existing volume with detailed arc
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "开端",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "展开", Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "章1", CoreEvent: "事件", Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}},
			}},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "继续",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "发展",
			"arcs": []map[string]any{
				{ // first arc must be detailed; use chapter=0 for global sequence
					"index": 1, "title": "新弧", "goal": "探索",
					"chapters": []map[string]any{
						{"chapter": 0, "title": "章1", "core_event": "事件",
							"scenes": []map[string]any{v3FullScene()}},
					},
				},
				{ // skeleton arc
					"index": 2, "title": "骨架", "goal": "推进",
					"estimated_chapters": 3,
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("append_volume with skeleton arc should succeed: %v", err)
	}

	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	if len(volumes[1].Arcs) != 2 {
		t.Fatalf("expected 2 arcs in new volume, got %d", len(volumes[1].Arcs))
	}
	if volumes[1].Arcs[1].IsExpanded() {
		t.Fatal("arc 2 should NOT be expanded")
	}
	if volumes[1].Arcs[1].EstimatedChapters != 3 {
		t.Fatalf("arc 2 estimated_chapters should be 3, got %d", volumes[1].Arcs[1].EstimatedChapters)
	}
}

// ── V3 append_volume first arc skeleton rejected ──

func TestPlanV3_AppendFirstArcSkeletonRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "开端",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "展开", Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "章1", CoreEvent: "事件", Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}},
			}},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "继续",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "发展",
			"arcs": []map[string]any{
				{ // first arc skeleton → rejected
					"index": 1, "title": "骨架", "goal": "推进",
					"estimated_chapters": 3,
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("append_volume first arc skeleton should be rejected")
	}
	if !strings.Contains(err.Error(), "first arc must contain expanded chapters") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// ── Schema-level proofs for skeleton arc acceptance ──
// These go through compileSaveSchema + validateAgainstSchema, the same path
// the JSON Schema library uses at runtime, NOT via Execute.

func TestPlanV3_Schema_SkeletonArcAcceptance(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)
	sch := compileSaveSchema(t, tool.Schema())

	scene := v3FullScene()

	tests := []struct {
		name    string
		payload map[string]any
	}{
		{
			// Skeleton arc with chapters field OMITTED entirely → must pass schema
			"layered_omitted_chapters",
			map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{
						map[string]any{
							"index": 1, "title": "首弧", "goal": "展开",
							"chapters": []any{map[string]any{
								"chapter": 1, "title": "章", "core_event": "事件",
								"scenes": []any{scene},
							}},
						},
						map[string]any{
							"index": 2, "title": "骨架", "goal": "推进",
							"estimated_chapters": 5,
							// chapters explicitly omitted
						},
					},
				}},
			},
		},
		{
			// Skeleton arc with chapters=null → must pass schema
			"layered_null_chapters",
			map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{
						map[string]any{
							"index": 1, "title": "首弧", "goal": "展开",
							"chapters": []any{map[string]any{
								"chapter": 1, "title": "章", "core_event": "事件",
								"scenes": []any{scene},
							}},
						},
						map[string]any{
							"index": 2, "title": "骨架", "goal": "推进",
							"estimated_chapters": 5,
							"chapters":           nil,
						},
					},
				}},
			},
		},
		{
			// Skeleton arc with chapters=[] (empty array) → must pass schema
			"layered_empty_chapters",
			map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{
						map[string]any{
							"index": 1, "title": "首弧", "goal": "展开",
							"chapters": []any{map[string]any{
								"chapter": 1, "title": "章", "core_event": "事件",
								"scenes": []any{scene},
							}},
						},
						map[string]any{
							"index": 2, "title": "骨架", "goal": "推进",
							"estimated_chapters": 5,
							"chapters":           []any{},
						},
					},
				}},
			},
		},
		{
			// Detailed arc with optional estimated_chapters → must pass schema
			"detailed_arc_with_estimate",
			map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{
						map[string]any{
							"index": 1, "title": "首弧", "goal": "展开",
							"chapters": []any{map[string]any{
								"chapter": 1, "title": "章", "core_event": "事件",
								"scenes": []any{scene},
							}},
							"estimated_chapters": 10, // allowed on detailed arc, zeroed later by toDomain()
						},
					},
				}},
			},
		},
		{
			// append_volume: skeleton second arc → must pass schema
			"append_omitted_chapters",
			map[string]any{
				"type": "append_volume", "reason": "继续",
				"content": map[string]any{
					"index": 2, "title": "卷2", "theme": "发展",
					"arcs": []any{
						map[string]any{
							"index": 1, "title": "首弧", "goal": "展开",
							"chapters": []any{map[string]any{
								"chapter": 1, "title": "章", "core_event": "事件",
								"scenes": []any{scene},
							}},
						},
						map[string]any{
							"index": 2, "title": "骨架", "goal": "推进",
							"estimated_chapters": 5,
						},
					},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAgainstSchema(t, sch, tc.payload); err != nil {
				t.Errorf("valid payload should pass schema, got: %v\npayload: %+v", err, tc.payload)
			}
		})
	}
}

// ── Skeleton arc with estimated_chapters missing or 0: schema rejection ──

func TestPlanV3_SkeletonEstimateMissingOrZero(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)
	sch := compileSaveSchema(t, tool.Schema())

	tests := []struct {
		name    string
		payload map[string]any
	}{
		{
			"layered_outline_arc_no_estimate",
			map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						// no chapters, no estimated_chapters → neither branch matches
					}},
				}},
			},
		},
		{
			"layered_outline_arc_estimate_zero",
			map[string]any{
				"type": "layered_outline", "scale": "long",
				"content": []any{map[string]any{
					"index": 1, "title": "卷", "theme": "主题",
					"arcs": []any{map[string]any{
						"index": 1, "title": "弧", "goal": "目标",
						"estimated_chapters": 0,
					}},
				}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAgainstSchema(t, sch, tc.payload); err == nil {
				t.Errorf("negative payload should be rejected by schema: %+v", tc.payload)
			}
		})
	}
}

// ── Chapter omitted / 0 / correct explicit / wrong explicit ──

func TestPlanV3_ChapterOmittedOrZero(t *testing.T) {
	// layered_outline: chapter omitted, 0, explicit
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())

	// chapter omitted (no field)
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开",
						"chapters": []map[string]any{
							{"title": "章1", "core_event": "开局", // no chapter field
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("layered_outline with omitted chapter should succeed: %v", err)
	}
}

func TestPlanV3_ChapterExplicitCorrect(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "章1", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
							{"chapter": 0, "title": "章2", "core_event": "发展", // chapter=0 accepted
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("layered_outline with chapter=0 should succeed: %v", err)
	}
	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	// chapter=0 should be normalized to 2
	if volumes[0].Arcs[0].Chapters[1].Chapter != 2 {
		t.Fatalf("expected chapter=2 after normalization, got %d", volumes[0].Arcs[0].Chapters[1].Chapter)
	}
}

func TestPlanV3_ChapterWrongExplicitRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开",
						"chapters": []map[string]any{
							{"chapter": 7, "title": "错号", "core_event": "X", // 7 != expected 1
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("wrong explicit chapter number should be rejected")
	}
	if !strings.Contains(err.Error(), "编号") && !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("unexpected error message: %v", err)
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// ── Expand_arc with chapter=0 (already tested in TestSaveFoundationV3_ExpandArcNormalizesMissingChapterNumber) ──

// ── Detailed arc estimate zeroed before domain conversion ──

func TestPlanV3_DetailedArcEstimateZeroed(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "首弧", "goal": "展开",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "章1", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
						},
						"estimated_chapters": 10, // should be zeroed
					},
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("detailed arc with estimate should succeed: %v", err)
	}

	volumes, err := st.Outline.LoadLayeredOutline()
	if err != nil {
		t.Fatalf("LoadLayeredOutline: %v", err)
	}
	if volumes[0].Arcs[0].EstimatedChapters != 0 {
		t.Fatalf("detailed arc estimated_chapters should be zeroed, got %d", volumes[0].Arcs[0].EstimatedChapters)
	}
}

// ── Append volume when existing store has skeleton arc ──

func TestPlanV3_AppendAfterExistingSkeletonRejected(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	// existing volume with skeleton arc
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "开端",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "展开", Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "章1", CoreEvent: "事件", Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}},
			}},
			{Index: 2, Title: "骨架弧", Goal: "推进", EstimatedChapters: 5},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "继续",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "发展",
			"arcs": []map[string]any{
				{
					"index": 1, "title": "新弧", "goal": "探索",
					"chapters": []map[string]any{
						{"chapter": 1, "title": "章1", "core_event": "事件",
							"scenes": []map[string]any{v3FullScene()}},
					},
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("append_volume after existing skeleton should be rejected")
	}
	if !strings.Contains(err.Error(), "骨架") && !strings.Contains(err.Error(), "skeleton") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// ── Core4 skeleton arc compat ──

func TestPlanCore4_SkeletonArcCompat(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	// existing volume with all expanded arcs
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "开端",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "展开", Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "章1", CoreEvent: "事件", Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}},
			}},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	// Core4 append_volume with skeleton second arc
	tool := NewSaveFoundationTool(st, projectprofile.NewCore4Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "继续", "scale": "long",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "发展",
			"arcs": []map[string]any{
				{
					"index": 1, "title": "新弧", "goal": "探索",
					"chapters": []map[string]any{
						{"chapter": 0, "title": "章1", "core_event": "事件",
							"scenes": []map[string]any{{"goal": "g", "action": "a", "conflict": "c", "outcome": "o"}}},
					},
				},
				{
					"index": 2, "title": "骨架", "goal": "推进",
					"estimated_chapters": 3,
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Core4 append_volume with skeleton arc should succeed: %v", err)
	}
}

// ── Phase 1 multi semantic error aggregation ──

func TestPlanV3_MultiSemanticError(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewSceneBeatV3Contract())
	before := takeStoreSnapshot(dir)
	// First arc detailed (required), then two arcs that are neither detailed nor skeleton
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "第一卷", "theme": "开端",
				"arcs": []map[string]any{
					{ // first arc must be detailed
						"index": 1, "title": "首弧", "goal": "展开",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "章1", "core_event": "开局",
								"scenes": []map[string]any{v3FullScene()}},
						},
					},
					{ // empty: neither detailed nor skeleton
						"index": 2, "title": "空弧1", "goal": "无内容",
					},
					{ // empty: neither detailed nor skeleton
						"index": 3, "title": "空弧2", "goal": "也无内容",
					},
				},
			},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("multiple empty arcs should be rejected")
	}
	// Check aggregated error format
	if !strings.Contains(err.Error(), "validation_errors") {
		t.Fatalf("expected aggregated validation_errors, got: %v", err)
	}
	if !strings.Contains(err.Error(), "EMPTY_ARC") {
		t.Fatalf("expected EMPTY_ARC code, got: %v", err)
	}
	if !strings.Contains(err.Error(), "volumes[0].arcs[1]") {
		t.Fatalf("expected path volumes[0].arcs[1], got: %v", err)
	}
	if !strings.Contains(err.Error(), "volumes[0].arcs[2]") {
		t.Fatalf("expected path volumes[0].arcs[2], got: %v", err)
	}
	// No writes
	assertSnapshotUnchanged(t, before, takeStoreSnapshot(dir))
}

// ── Core4 append/expand scale compatibility ──

func TestPlanCore4_AppendExpandScaleCompat(t *testing.T) {
	// Core4 append_volume with scale
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "开端",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "展开", Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "章1", CoreEvent: "事件", Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}},
			}},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewCore4Contract())
	// append_volume with scale
	args, _ := json.Marshal(map[string]any{
		"type": "append_volume", "reason": "继续", "scale": "long",
		"content": map[string]any{
			"index": 2, "title": "第二卷", "theme": "发展",
			"arcs": []map[string]any{
				{
					"index": 1, "title": "新弧", "goal": "探索",
					"chapters": []map[string]any{
						{"chapter": 0, "title": "章1", "core_event": "事件",
							"scenes": []map[string]any{{"goal": "g", "action": "a", "conflict": "c", "outcome": "o"}}},
					},
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Core4 append_volume with scale should succeed: %v", err)
	}

	// Verify scale was saved
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatalf("LoadRunMeta: %v", err)
	}
	if meta.PlanningTier != domain.PlanningTierLong {
		t.Fatalf("expected planning tier long, got %q", meta.PlanningTier)
	}
}

func TestPlanCore4_ExpandArcScaleCompat(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 5); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1, Title: "第一卷", Theme: "开端",
		Arcs: []domain.ArcOutline{
			{Index: 1, Title: "首弧", Goal: "展开", Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "章1", CoreEvent: "事件", Scenes: domain.SceneList{{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}}},
			}},
			{Index: 2, Title: "骨架", Goal: "推进", EstimatedChapters: 3},
		},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewCore4Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "expand_arc", "volume": 1, "arc": 2, "scale": "long",
		"content": map[string]any{
			"title": "展开弧", "goal": "推进剧情",
			"chapters": []map[string]any{
				{"chapter": 0, "title": "新章", "core_event": "事件",
					"scenes": []map[string]any{{"goal": "g", "action": "a", "conflict": "c", "outcome": "o"}}},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Core4 expand_arc with scale should succeed: %v", err)
	}
}

// ── V3 scene 7-field and Core4 4-field differences ──

func TestPlanV3_Scene7FieldStrictInLayered(t *testing.T) {
	// V3 layered_outline with scene missing erotic_charge should be schema-rejected
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)
	sch := compileSaveSchema(t, tool.Schema())

	payload := map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []any{map[string]any{
			"index": 1, "title": "卷", "theme": "主题",
			"arcs": []any{map[string]any{
				"index": 1, "title": "弧", "goal": "目标",
				"chapters": []any{map[string]any{
					"chapter": 1, "title": "章", "core_event": "事件",
					"scenes": []any{map[string]any{
						"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
						"body_reaction": "b", "emotion_reaction": "e", // missing erotic_charge
					}},
				}},
			}},
		}},
	}
	if err := validateAgainstSchema(t, sch, payload); err == nil {
		t.Error("V3 layered_outline scene missing erotic_charge should be rejected by schema")
	}
}

func TestPlanCore4_Scene4FieldAcceptsLegacy(t *testing.T) {
	// Core4 layered_outline with legacy string scenes
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewSaveFoundationTool(st, projectprofile.NewCore4Contract())
	args, _ := json.Marshal(map[string]any{
		"type": "layered_outline", "scale": "long",
		"content": []map[string]any{
			{
				"index": 1, "title": "卷1", "theme": "开端",
				"arcs": []map[string]any{
					{
						"index": 1, "title": "弧1", "goal": "目标",
						"chapters": []map[string]any{
							{"chapter": 1, "title": "章1", "core_event": "事件",
								"scenes": []any{"legacy string scene"}},
						},
					},
				},
			},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Core4 layered_outline with legacy string scenes should succeed: %v", err)
	}
}

// ── Flat outline chapter still required? ──
// Schema-level proof: V3 flat outline (outline type) entries MUST have a "chapter" field.
// Because outline and the three planning paths share the same chapterOutlineSchema helper,
// and we removed "chapter" from its required list to allow planning paths to omit it,
// the flat outline SCHEMA no longer rejects missing chapter either — a regression.
//
// This test proves the schema no longer rejects what it should reject.
// Fix: restore "chapter" in a flat-outline-specific schema or add runtime validation.

func TestPlanV3_FlatOutlineChapterRequired(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v3 := projectprofile.NewSceneBeatV3Contract()
	tool := NewSaveFoundationTool(st, v3)

	sch := compileSaveSchema(t, tool.Schema())
	payload := map[string]any{
		"type": "outline",
		"content": []any{map[string]any{
			// no "chapter" field — flat outline should require it
			"title": "章", "core_event": "事件",
			"scenes": []any{map[string]any{
				"goal": "g", "action": "a", "conflict": "c", "outcome": "o",
				"body_reaction": "b", "emotion_reaction": "e", "erotic_charge": "ec",
			}},
		}},
		"scale": "short",
	}

	// Schema-level proof: flat outline must still require chapter
	if err := validateAgainstSchema(t, sch, payload); err == nil {
		t.Fatal("REGRESSION: V3 flat outline without chapter field PASSES schema validation but should be rejected")
	}
	// If we reach here, schema correctly rejected it — flat outline chapter still required
}

// ── v3FullScene helper: returns a complete V3 scene with all 7 required fields ──

func v3FullScene() map[string]any {
	return map[string]any{
		"goal":             "目标",
		"action":           "行动",
		"conflict":         "冲突",
		"outcome":          "结果",
		"body_reaction":    "身体反应",
		"emotion_reaction": "心理反应",
		"erotic_charge":    "色气",
	}
}
