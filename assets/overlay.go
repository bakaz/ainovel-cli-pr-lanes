package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// ResourceSource 描述某个运行时资源最终来自哪里。
type ResourceSource struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// OverlayReport 是启动时资源覆盖报告。缺失文件不是告警；损坏文件会回退到上一层。
type OverlayReport struct {
	Applied  []ResourceSource
	Warnings []string
}

// ApplyOverrides 依次应用资源根，后面的目录覆盖前面的目录。导出供测试和嵌入方使用。
func ApplyOverrides(bundle *Bundle, style string, dirs []string) OverlayReport {
	var report OverlayReport
	if bundle == nil {
		report.Warnings = append(report.Warnings, "assets: nil bundle")
		return report
	}
	if bundle.Sources == nil {
		bundle.Sources = make(map[string]ResourceSource)
	}
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		applyPromptOverrides(bundle, dir, &report)
		applyReferenceOverrides(bundle, style, dir, &report)
		applyStyleOverrides(bundle, dir, &report)
	}
	return report
}

func applyPromptOverrides(bundle *Bundle, dir string, report *OverlayReport) {
	files := []string{
		"architect-short.md", "architect-long.md", "writer.md", "editor.md",
		"import-foundation.md", "import-chapter-analyzer.md", "simulation-source.md", "simulation-merge.md",
	}
	for _, file := range files {
		rel := filepath.Join("prompts", file)
		raw, src, ok := readOverrideFile(dir, rel, report)
		if !ok {
			continue
		}
		if role, core := promptRole[file]; core {
			_ = role
			if err := bundle.OverridePrompt(file, raw); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %v", src.Path, err))
				continue
			}
		} else {
			switch file {
			case "import-foundation.md":
				bundle.Prompts.ImportFoundation = raw
			case "import-chapter-analyzer.md":
				bundle.Prompts.ImportAnalyzer = raw
			case "simulation-source.md":
				bundle.Prompts.SimulationSource = raw
			case "simulation-merge.md":
				bundle.Prompts.SimulationMerge = raw
			}
		}
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
}

func applyReferenceOverrides(bundle *Bundle, style, dir string, report *OverlayReport) {
	setters := map[string]func(string){
		"chapter-guide.md":      func(v string) { bundle.References.ChapterGuide = v },
		"hook-techniques.md":    func(v string) { bundle.References.HookTechniques = v },
		"quality-checklist.md":  func(v string) { bundle.References.QualityChecklist = v },
		"outline-template.md":   func(v string) { bundle.References.OutlineTemplate = v },
		"character-template.md": func(v string) { bundle.References.CharacterTemplate = v },
		"chapter-template.md":   func(v string) { bundle.References.ChapterTemplate = v },
		"consistency.md":        func(v string) { bundle.References.Consistency = v },
		"content-expansion.md":  func(v string) { bundle.References.ContentExpansion = v },
		"dialogue-writing.md":   func(v string) { bundle.References.DialogueWriting = v },
		"longform-planning.md":  func(v string) { bundle.References.LongformPlanning = v },
		"differentiation.md":    func(v string) { bundle.References.Differentiation = v },
		"anti-ai-tone.md":       func(v string) { bundle.References.AntiAITone = v },
	}
	keys := make([]string, 0, len(setters))
	for key := range setters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, file := range keys {
		rel := filepath.Join("references", file)
		raw, src, ok := readOverrideFile(dir, rel, report)
		if !ok {
			continue
		}
		setters[file](raw)
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
	if style == "" || style == "default" {
		return
	}
	genreSetters := map[string]func(string){
		"style-references.md": func(v string) { bundle.References.StyleReference = v },
		"arc-templates.md":    func(v string) { bundle.References.ArcTemplates = v },
	}
	for _, file := range []string{"style-references.md", "arc-templates.md"} {
		rel := filepath.Join("references", "genres", style, file)
		raw, src, ok := readOverrideFile(dir, rel, report)
		if !ok {
			continue
		}
		genreSetters[file](raw)
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
}

func applyStyleOverrides(bundle *Bundle, dir string, report *OverlayReport) {
	stylesDir := filepath.Join(dir, "styles")
	entries, err := os.ReadDir(stylesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("读取 %s: %v", stylesDir, err))
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		rel := filepath.Join("styles", entry.Name())
		raw, src, ok := readOverrideFile(dir, rel, report)
		if !ok {
			continue
		}
		bundle.Styles[strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))] = raw
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
}

func readOverrideFile(root, rel string, report *OverlayReport) (string, ResourceSource, bool) {
	path := filepath.Join(root, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("读取 %s: %v", path, err))
		}
		return "", ResourceSource{}, false
	}
	if !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		report.Warnings = append(report.Warnings, fmt.Sprintf("忽略无效覆盖文件 %s（必须是非空 UTF-8 文本）", path))
		return "", ResourceSource{}, false
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	return string(data), sourceFor("file", path, data), true
}

func recordApplied(bundle *Bundle, key string, src ResourceSource, report *OverlayReport) {
	src.Key = key
	bundle.Sources[key] = src
	report.Applied = append(report.Applied, src)
}

func sourceFor(kind, path string, data []byte) ResourceSource {
	sum := sha256.Sum256(data)
	return ResourceSource{Kind: kind, Path: path, SHA256: hex.EncodeToString(sum[:])}
}


