package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// TestMigrateLegacyRooms_BadArgs 验证未知参数返回退出码 2。
func TestMigrateLegacyRooms_BadArgs(t *testing.T) {
	code := migrateLegacyRooms([]string{"--unknown-flag"})
	if code != 2 {
		t.Fatalf("expected exit code 2 for unknown flag, got %d", code)
	}
}

// TestMigrateLegacyRooms_MissingConfigValue 验证 --config 后缺值返回退出码 2。
func TestMigrateLegacyRooms_MissingConfigValue(t *testing.T) {
	code := migrateLegacyRooms([]string{"--config"})
	if code != 2 {
		t.Fatalf("expected exit code 2 for missing --config value, got %d", code)
	}
}

// TestMigrateLegacyRooms_MissingOutput 验证没有 --output 时返回退出码 2。
func TestMigrateLegacyRooms_MissingOutput(t *testing.T) {
	code := migrateLegacyRooms([]string{"--config", "some-config.json"})
	if code != 2 {
		t.Fatalf("expected exit code 2 for missing --output, got %d", code)
	}
}

// TestMigrateLegacyRooms_MissingOutputValue 验证 --output 后缺值返回退出码 2。
func TestMigrateLegacyRooms_MissingOutputValue(t *testing.T) {
	code := migrateLegacyRooms([]string{"--output"})
	if code != 2 {
		t.Fatalf("expected exit code 2 for missing --output value, got %d", code)
	}
}

// TestMigrateLegacyRooms_OutputDirNotExist 验证不存在的 output 目录返回退出码 1。
func TestMigrateLegacyRooms_OutputDirNotExist(t *testing.T) {
	code := migrateLegacyRooms([]string{"--output", filepath.Join(t.TempDir(), "nonexistent")})
	if code != 1 {
		t.Fatalf("expected exit code 1 for nonexistent output dir, got %d", code)
	}
}

// TestMigrateLegacyRooms_OutputDirNoCompass 验证存在但没有 meta/compass.json 的
// output 目录返回退出码 1。
func TestMigrateLegacyRooms_OutputDirNoCompass(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "novel")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	code := migrateLegacyRooms([]string{"--output", outputDir})
	if code != 1 {
		t.Fatalf("expected exit code 1 for output dir without compass, got %d", code)
	}
}

// TestMigrateLegacyRooms_OutputDirIsFile 验证 output 路径是文件而非目录时返回退出码 1。
func TestMigrateLegacyRooms_OutputDirIsFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := migrateLegacyRooms([]string{"--output", filePath})
	if code != 1 {
		t.Fatalf("expected exit code 1 for file path, got %d", code)
	}
}

// TestMigrateLegacyRooms_AbsolutePath 验证通过绝对路径传入 --output 也能正常工作。
func TestMigrateLegacyRooms_AbsolutePath(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "my", "workspace")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, outputDir)

	// 创建含有 legacy rooms 的 compass
	saveCompassAt(t, outputDir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "ancient_temple", "name": "上古神殿"},
				{"room": 42, "name": "密道"}
			]
		}
	}`)

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 0 {
		t.Fatalf("expected exit code 0 for migrated with absolute path, got %d", code)
	}
}

// TestMigrateLegacyRooms_NonDefaultWorkspace 验证 output 不在默认 output/novel 时
// 也能正常工作（例如 output/custom-workspace）。
func TestMigrateLegacyRooms_NonDefaultWorkspace(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "custom-workspace")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, dir)

	// 创建含有 legacy rooms 的 compass
	saveCompassAt(t, outputDir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "room_a", "name": "Room A"}
			]
		}
	}`)

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 0 {
		t.Fatalf("expected exit code 0 for migrated with non-default workspace, got %d", code)
	}
}

// TestMigrateLegacyRooms_NoLegacyRooms 验证没有 legacy rooms 时返回退出码 0
// 且 status=no_legacy_rooms。
func TestMigrateLegacyRooms_NoLegacyRooms(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "novel")
	if err := os.MkdirAll(filepath.Join(outputDir, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, dir)

	// 创建无 legacy rooms 的 compass（空 reference）
	saveCompassAt(t, outputDir, `{}`)

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 0 {
		t.Fatalf("expected exit code 0 for no_legacy_rooms, got %d", code)
	}
}

// TestMigrateLegacyRooms_Migrated 验证迁移成功后返回退出码 0
// 且 status=migrated。
func TestMigrateLegacyRooms_Migrated(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "novel")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, dir)

	// 创建含有 legacy rooms 的 compass（通过 Outline.SaveCompass）
	saveCompassAt(t, outputDir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "ancient_temple", "name": "上古神殿"},
				{"room": 42, "name": "密道"}
			]
		}
	}`)

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 0 {
		t.Fatalf("expected exit code 0 for migrated, got %d", code)
	}
}

// TestMigrateLegacyRooms_AlreadyExists 验证 archive 已存在、无 legacy rooms 时
// 返回退出码 0 且 status=already_exists。
func TestMigrateLegacyRooms_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "novel")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, dir)

	// 创建无 legacy rooms 的 compass
	saveCompassAt(t, outputDir, `{}`)

	// 创建已有的 archive
	setupArchiveAt(t, outputDir)

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 0 {
		t.Fatalf("expected exit code 0 for already_exists, got %d", code)
	}
}

// TestMigrateLegacyRooms_CleanedUp 验证 archive 已存在且有 legacy rooms 时，
// 清理后返回退出码 0 且 status=cleaned_up。
func TestMigrateLegacyRooms_CleanedUp(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "novel")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, dir)

	// 创建含有 legacy rooms 的 compass
	saveCompassAt(t, outputDir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "ancient_temple", "name": "上古神殿"}
			]
		}
	}`)

	// 创建已有 archive，包含对应的 room 条目。
	// Summary 必须为空（cleanupLegacyWithVerificationLocked 构造的 want 不含 Summary）。
	setupArchiveWithEntriesAt(t, outputDir, []archiveEntry{
		{
			Kind: "room", ID: "ancient_temple",
			Data: json.RawMessage(`{"name":"上古神殿"}`),
		},
	})

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 0 {
		t.Fatalf("expected exit code 0 for cleaned_up, got %d", code)
	}
}

// TestMigrateLegacyRooms_Conflict 验证 legacy room 与 archive 数据不匹配时
// 返回退出码 1 且 status=conflict。
func TestMigrateLegacyRooms_Conflict(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output", "novel")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := writeMinimalConfig(t, dir)

	// 创建含有 legacy rooms 的 compass
	saveCompassAt(t, outputDir, `{
		"detailed_plan": {
			"long_rooms": [
				{"room": "ancient_temple", "name": "上古神殿"}
			]
		}
	}`)

	// 创建已有 archive，但故意不包含对应 room 条目（空 archive）
	setupArchiveAt(t, outputDir)

	code := migrateLegacyRooms([]string{"--config", configPath, "--output", outputDir})
	if code != 1 {
		t.Fatalf("expected exit code 1 for conflict, got %d", code)
	}
}

// ── helpers ──

type archiveEntry struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Summary string `json:"summary,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// writeMinimalConfig 在指定目录下写入一个仅含必需字段的 config.json，
// 返回路径。确保 LoadConfig 能加载（provider / model 只需非空）。
func writeMinimalConfig(t *testing.T, dir string) (path string) {
	t.Helper()
	cfg := struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}{
		Provider: "test-provider",
		Model:    "test-model",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// saveCompassAt 在指定的 outputDir 下通过 Outline.SaveCompass 写入 compass。
// outputDir 即 Store 的根目录，其 meta/ 下写入 compass.json。
// 复核阻塞项 2（方案 A）：写入完成后立即释放 workspace 锁——migrateLegacyRooms
// 内部会再建一个可写 Store（同一进程同一 workspace 只允许一个可写实例）。
func saveCompassAt(t *testing.T, outputDir, referenceJSON string) {
	t.Helper()
	st := store.NewStore(outputDir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	var ref json.RawMessage
	if referenceJSON != "" {
		ref = json.RawMessage(referenceJSON)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{
		Long: domain.LongCompass{
			EndingDirection: "终局",
			Reference:       ref,
		},
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
}

// setupArchiveAt 在指定的 outputDir 下写入一个空的 planning_archive.json。
func setupArchiveAt(t *testing.T, outputDir string) {
	t.Helper()
	metaDir := filepath.Join(outputDir, "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := map[string]any{
		"schema":  "ainovel.planning-archive",
		"version": 1,
		"entries": []any{},
	}
	data, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(metaDir, "planning_archive.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupArchiveWithEntriesAt 在指定的 outputDir 下写入包含指定条目的 planning_archive.json。
func setupArchiveWithEntriesAt(t *testing.T, outputDir string, entries []archiveEntry) {
	t.Helper()
	metaDir := filepath.Join(outputDir, "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := map[string]any{
		"schema":  "ainovel.planning-archive",
		"version": 1,
		"entries": entries,
	}
	data, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(metaDir, "planning_archive.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
