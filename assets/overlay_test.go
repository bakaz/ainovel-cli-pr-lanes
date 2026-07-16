package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyOverridesPrecedenceAndFallback(t *testing.T) {
	b := Load("default", LoadOptions{})
	low := t.TempDir()
	high := t.TempDir()
	writeAsset(t, low, "prompts/writer.md", "low writer")
	writeAsset(t, low, "references/anti-ai-tone.md", "low reference")
	writeAsset(t, high, "prompts/writer.md", "high writer")
	writeAsset(t, high, "styles/custom.md", "custom style")
	writeAssetBytes(t, high, "prompts/editor.md", []byte{0xff, 0xfe})

	report := ApplyOverrides(&b, "default", []string{low, high})
	if !strings.HasPrefix(b.Prompts.Writer, "high writer") {
		t.Fatalf("writer override precedence failed: %q", b.Prompts.Writer)
	}
	if !strings.Contains(b.Prompts.Writer, "仿写画像") {
		t.Fatal("外置核心 prompt 必须与内置版走相同 simulation guidance 包装")
	}
	if b.References.AntiAITone != "low reference" {
		t.Fatalf("reference override missing: %q", b.References.AntiAITone)
	}
	if b.Styles["custom"] != "custom style" {
		t.Fatalf("style override missing: %q", b.Styles["custom"])
	}
	if len(report.Warnings) != 1 || len(report.Applied) != 4 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if src := b.Sources["prompts/writer.md"]; src.Kind != "file" || src.Key != "prompts/writer.md" || len(src.SHA256) != 64 {
		t.Fatalf("writer source not recorded: %+v", b.Sources["prompts/writer.md"])
	}
}

func TestLoadRecordsAllEmbeddedResourceClasses(t *testing.T) {
	b := Load("default", LoadOptions{})
	for _, key := range []string{
		"prompts/import-foundation.md",
		"references/chapter-guide.md", "references/anti-ai-tone.md",
		"styles/default.md",
	} {
		if src, ok := b.Sources[key]; !ok || src.Kind != "embedded" || src.Key != key || len(src.SHA256) != 64 {
			t.Fatalf("missing embedded source %s: %+v", key, src)
		}
	}
}

func TestApplyOverridesMissingDirectoryKeepsEmbedded(t *testing.T) {
	b := Load("default", LoadOptions{})
	want := b.Prompts.Editor
	report := ApplyOverrides(&b, "default", []string{filepath.Join(t.TempDir(), "missing")})
	if b.Prompts.Editor != want || len(report.Warnings) != 0 || len(report.Applied) != 0 {
		t.Fatalf("missing dir changed bundle: report=%+v", report)
	}
}

func writeAsset(t *testing.T, root, rel, content string) {
	t.Helper()
	writeAssetBytes(t, root, rel, []byte(content))
}

func writeAssetBytes(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
