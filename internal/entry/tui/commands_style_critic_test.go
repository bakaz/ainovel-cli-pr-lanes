package tui

import (
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/host"
)

func TestStyleCriticCommand_IsRegistered(t *testing.T) {
	registry := commandRegistryInstance()
	cmd, ok := registry.Find("style-critic")
	if !ok {
		t.Fatal("/style-critic should be registered")
	}
	if !cmd.NeedsIdle {
		t.Error("/style-critic should require idle state")
	}
	if cmd.Group != "writing" {
		t.Errorf("group = %q, want writing", cmd.Group)
	}
	// 验证在 palette 中可见
	items := builtinCommandItems()
	if !hasPaletteItem(items, "style-critic") {
		t.Error("/style-critic missing from command palette")
	}
}

func TestStyleCriticCommand_RejectsNoArgs(t *testing.T) {
	m := Model{
		mode:     modeRunning,
		snapshot: host.UISnapshot{RuntimeState: "paused"},
	}

	result, cmd := m.handleSlashCommand(slashCommand{name: "style-critic", args: []string{}})
	if cmd != nil {
		t.Fatal("expected nil cmd for error case")
	}
	evts := result.(Model).events
	if len(evts) == 0 || evts[len(evts)-1].Level != "error" {
		t.Fatal("expected error event for missing args")
	}
	if !strings.Contains(evts[len(evts)-1].Summary, "用法") {
		t.Errorf("expected usage hint, got %q", evts[len(evts)-1].Summary)
	}
}

func TestStyleCriticCommand_RejectsUnknownArg(t *testing.T) {
	m := Model{
		mode:     modeRunning,
		snapshot: host.UISnapshot{RuntimeState: "paused"},
	}

	result, cmd := m.handleSlashCommand(slashCommand{name: "style-critic", args: []string{"maybe"}})
	if cmd != nil {
		t.Fatal("expected nil cmd for error case")
	}
	evts := result.(Model).events
	if len(evts) == 0 || evts[len(evts)-1].Level != "error" {
		t.Fatal("expected error event for unknown arg")
	}
	if !strings.Contains(evts[len(evts)-1].Summary, "用法") {
		t.Errorf("expected usage hint, got %q", evts[len(evts)-1].Summary)
	}
}

func TestStyleCriticCommand_RejectsTooManyArgs(t *testing.T) {
	m := Model{
		mode:     modeRunning,
		snapshot: host.UISnapshot{RuntimeState: "paused"},
	}

	result, cmd := m.handleSlashCommand(slashCommand{name: "style-critic", args: []string{"on", "extra"}})
	if cmd != nil {
		t.Fatal("expected nil cmd for error case")
	}
	evts := result.(Model).events
	if len(evts) == 0 || evts[len(evts)-1].Level != "error" {
		t.Fatal("expected error event for too many args")
	}
	if !strings.Contains(evts[len(evts)-1].Summary, "用法") {
		t.Errorf("expected usage hint, got %q", evts[len(evts)-1].Summary)
	}
}

func TestStyleCriticCommand_NeedsIdleBlocksRunning(t *testing.T) {
	m := Model{
		mode:     modeRunning,
		snapshot: host.UISnapshot{IsRunning: true, RuntimeState: "running"},
	}

	result, cmd := m.handleSlashCommand(slashCommand{name: "style-critic", args: []string{"on"}})
	if cmd != nil {
		t.Fatal("expected nil cmd when engine is running")
	}
	evts := result.(Model).events
	if len(evts) == 0 || evts[len(evts)-1].Level != "error" {
		t.Fatal("expected error event for running state")
	}
	if !strings.Contains(evts[len(evts)-1].Summary, "空闲状态") {
		t.Errorf("expected idle-only error, got %q", evts[len(evts)-1].Summary)
	}
}

// TestStyleCriticCommand_SuccessBehaviorCoveredInHost 说明成功路径由 Host 层测试覆盖。
// TUI 层验证：命令注册、NeedsIdle 守卫、参数校验。
// Host 层测试已验证：模式持久化、off/critic 切换、无效值拒绝、事件发射。
func TestStyleCriticCommand_SuccessBehaviorCoveredInHost(t *testing.T) {
	// 标记占位符，明确成功路径的归属层
	t.Log("成功路径行为由 internal/host/style_review_mode_test.go 覆盖")
}
