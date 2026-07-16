package agents

import (
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/store"
)

// TestBuildWorkers_ToolComposition 验证 buildWorkerToolsets 各子代理的工具列表组成。
// 直接调用生产级 helper（BuildWorkers 也使用同一来源），不重复构造。
func TestBuildWorkers_ToolComposition(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	bundle := assets.Load("default", assets.LoadOptions{})
	ts := buildWorkerToolsets(st, bundle, "default")

	// ── architect_short 不应有 read_planning_reference ──
	if toolInList(ts.ArchitectShort, "read_planning_reference") {
		t.Error("architect_short should NOT have read_planning_reference")
	}

	// ── architect_long 必须有 read_planning_reference ──
	if !toolInList(ts.ArchitectLong, "read_planning_reference") {
		t.Error("architect_long MUST have read_planning_reference")
	}

	// 两种 architect 工具列表必须不同
	if len(ts.ArchitectLong) <= len(ts.ArchitectShort) {
		t.Error("architect_long should have more tools than architect_short")
	}

	// ── writer 工具 ──
	expectTools(t, "writer", ts.Writer, []string{
		"novel_context", "read_chapter", "plan_chapter", "draft_chapter",
		"edit_chapter", "check_consistency", "commit_chapter",
	})

	// ── editor 工具 ──
	expectTools(t, "editor", ts.Editor, []string{
		"novel_context", "read_chapter", "save_review",
		"save_arc_summary", "save_volume_summary",
	})

	// 确认上下文工具实例不同（角色隔离）
	novelCtxNames := make(map[string]int)
	for _, tools := range [][]agentcore.Tool{ts.ArchitectShort, ts.ArchitectLong, ts.Writer, ts.Editor} {
		for _, tool := range tools {
			if tool.Name() == "novel_context" {
				novelCtxNames["*"]++
			}
		}
	}
	// 每个子代理都应有 novel_context（architect_short 和 architect_long 共享同一实例）
	if len(ts.ArchitectShort) == 0 || len(ts.ArchitectLong) == 0 || len(ts.Writer) == 0 || len(ts.Editor) == 0 {
		t.Error("all agents must have tools")
	}

	shortInner := ts.ArchitectShort[0]
	longInner := ts.ArchitectLong[0]
	writerInner := ts.Writer[0]
	editorInner := ts.Editor[0]

	if shortInner != longInner {
		t.Error("architect_short and architect_long should share the same novel_context instance")
	}
	if shortInner == writerInner {
		t.Error("architect and writer should use different novel_context instances")
	}
	if writerInner == editorInner {
		t.Error("writer and editor should use different novel_context instances")
	}

	t.Log("architect_short:", toolNames(ts.ArchitectShort))
	t.Log("architect_long:", toolNames(ts.ArchitectLong))
	t.Log("writer:", toolNames(ts.Writer))
	t.Log("editor:", toolNames(ts.Editor))
}

func expectTools(t *testing.T, name string, tools []agentcore.Tool, want []string) {
	t.Helper()
	for _, w := range want {
		if !toolInList(tools, w) {
			t.Errorf("%s missing required tool: %s", name, w)
		}
	}
}

func toolInList(list []agentcore.Tool, name string) bool {
	for _, t := range list {
		if t.Name() == name {
			return true
		}
	}
	return false
}

func toolNames(list []agentcore.Tool) []string {
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = t.Name()
	}
	return names
}

