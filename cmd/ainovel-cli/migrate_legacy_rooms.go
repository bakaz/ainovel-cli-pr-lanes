package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/store"
)

// migrateLegacyRooms 是 `ainovel-cli migrate-legacy-rooms` 子命令的实现。
//
// 用法：ainovel-cli migrate-legacy-rooms --output <workspace-output-dir> [--config <path>]
//
// --output（必填）指定 output workspace 根目录（包含 meta/compass.json）。
// --config（可选）指定配置文件路径，不影响迁移目标。
//
// 退出码约定：
//   0 — migrated / cleaned_up / already_exists / no_legacy_rooms（成功无操作）
//   1 — 系统级错误（IO、config 解析失败、无效 workspace、store 错误）
//   2 — 参数用法错误
func migrateLegacyRooms(argv []string) int {
	var configPath, outputDir string
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--config":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: --config 缺少值")
				return 2
			}
			i++
			configPath = argv[i]
		case "--output":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: --output 缺少值")
				return 2
			}
			i++
			outputDir = argv[i]
		default:
			fmt.Fprintf(os.Stderr, "error: migrate-legacy-rooms 不支持参数 %q\n", argv[i])
			return 2
		}
	}

	if outputDir == "" {
		fmt.Fprintln(os.Stderr, "error: --output <workspace-output-dir> 是必填参数")
		return 2
	}

	// 在加载配置或创建 Store 之前验证目标 workspace 合法。
	// 非法目标直接拒绝，保证不写任何文件。
	if err := validateOutputWorkspace(outputDir); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// --config 可选；加载失败不影响迁移（仅提供额外上下文）。
	if _, err := bootstrap.LoadConfig(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: config: %v\n", err)
	}

	st := store.NewStore(outputDir)
	result, err := st.MigrateLegacyRooms()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: migrate legacy rooms: %v\n", err)
		return 1
	}

	// 输出 JSON 结果
	output := struct {
		Status       string `json:"status"`
		EntriesCount int    `json:"entries_count,omitempty"`
		Message      string `json:"message,omitempty"`
	}{
		Status:       result.Status,
		EntriesCount: result.EntriesCount,
		Message:      result.Message,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		fmt.Fprintf(os.Stderr, "error: encode result: %v\n", err)
		return 1
	}

	// 成功无操作状态
	switch result.Status {
	case "migrated", "cleaned_up", "already_exists", "no_legacy_rooms":
		return 0
	default:
		// conflict、error 等视为失败
		return 1
	}
}

// validateOutputWorkspace 验证 outputDir 是一个合法的 output workspace：
// 路径存在、是目录、包含 meta/compass.json。非法目标在写文件之前被拒绝。
func validateOutputWorkspace(outputDir string) error {
	info, err := os.Stat(outputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("output 目录 %q 不存在", outputDir)
		}
		return fmt.Errorf("output 目录 %q: %w", outputDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("output 目录 %q 不是目录", outputDir)
	}
	compassPath := filepath.Join(outputDir, "meta", "compass.json")
	if _, err := os.Stat(compassPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("output 目录 %q 不是合法的 workspace（meta/compass.json 不存在）", outputDir)
		}
		return fmt.Errorf("output 目录 %q: %w", outputDir, err)
	}
	return nil
}
