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

const maxOverlayFileBytes = 1024 * 1024

// ResourceSource 描述某个运行时资源最终来自哪里。SHA256 对 file 来源计算原始
// 文件字节；核心 prompt 后续追加的 simulation/v3 guidance 不会伪装成文件内容。
type ResourceSource struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// OverlayReport 是启动时资源覆盖报告。缺失文件不是告警；越界、损坏或不支持的
// 文件会被拒绝并回退到上一层。
type OverlayReport struct {
	Applied  []ResourceSource `json:"applied"`
	Warnings []string         `json:"warnings,omitempty"`
}

var supportedPromptFiles = []string{
	"architect-short.md",
	"architect-long.md",
	"writer.md",
	"editor.md",
	"polisher.md",
	"import-segment.md",
	"import-foundation.md",
	"import-chapter-analyzer.md",
	"simulation-source.md",
	"simulation-merge.md",
	"arbiter-plan-start.md",
	"arbiter-intervention.md",
	"arbiter-failure.md",
	"style-critic.md",
}

// ApplyOverrides 依次应用资源根，后面的目录覆盖前面的目录。所有读取均使用固定
// 白名单；不会递归扫描 prompt/reference，也不会跟随逃出资源根的 symlink/junction。
func ApplyOverrides(bundle *Bundle, style string, roots []string) OverlayReport {
	var report OverlayReport
	if bundle == nil {
		report.Warnings = append(report.Warnings, "assets: nil bundle")
		return report
	}
	if bundle.Sources == nil {
		bundle.Sources = make(map[string]ResourceSource)
	}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		applyPromptOverrides(bundle, root, &report)
		warnUnsupportedPromptFiles(root, &report)
		applyReferenceOverrides(bundle, style, root, &report)
		applyStyleOverrides(bundle, root, &report)
	}
	return report
}

func applyPromptOverrides(bundle *Bundle, root string, report *OverlayReport) {
	for _, file := range supportedPromptFiles {
		rel := filepath.Join("prompts", file)
		raw, src, ok := readOverrideFile(root, rel, report)
		if !ok {
			continue
		}
		if _, core := promptRole[file]; core {
			if err := bundle.OverridePrompt(file, raw); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %v", src.Path, err))
				continue
			}
		} else {
			switch file {
			case "import-segment.md":
				bundle.Prompts.ImportSegment = raw
			case "import-foundation.md":
				bundle.Prompts.ImportFoundation = raw
				bundle.Prompts.ImportSynthesize = raw
			case "import-chapter-analyzer.md":
				bundle.Prompts.ImportAnalyzer = raw
				bundle.Prompts.ImportAnalyze = raw
			case "simulation-source.md":
				bundle.Prompts.SimulationSource = raw
			case "simulation-merge.md":
				bundle.Prompts.SimulationMerge = raw
			case "arbiter-plan-start.md":
				bundle.Prompts.ArbiterPlanStart = raw
			case "arbiter-intervention.md":
				bundle.Prompts.ArbiterIntervention = raw
			case "arbiter-failure.md":
				bundle.Prompts.ArbiterFailure = raw
			case "style-critic.md":
				bundle.Prompts.StyleCritic = raw
			}
		}
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
}

func warnUnsupportedPromptFiles(root string, report *OverlayReport) {
	dir, entries, ok := readOverrideDir(root, "prompts", report)
	if !ok {
		return
	}
	supported := make(map[string]bool, len(supportedPromptFiles))
	for _, name := range supportedPromptFiles {
		supported[strings.ToLower(name)] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(name), ".md") || supported[strings.ToLower(name)] {
			continue
		}
		report.Warnings = append(report.Warnings, fmt.Sprintf("忽略不支持的 prompt 覆盖 %s（当前 Engine 无对应运行时角色）", filepath.Join(dir, name)))
	}
}

func applyReferenceOverrides(bundle *Bundle, style, root string, report *OverlayReport) {
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
	names := make([]string, 0, len(setters))
	for name := range setters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rel := filepath.Join("references", name)
		raw, src, ok := readOverrideFile(root, rel, report)
		if !ok {
			continue
		}
		setters[name](raw)
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
	if style == "" || style == "default" || !styleNameRe.MatchString(style) {
		return
	}
	genreSetters := map[string]func(string){
		"style-references.md": func(v string) { bundle.References.StyleReference = v },
		"arc-templates.md":    func(v string) { bundle.References.ArcTemplates = v },
	}
	for _, name := range []string{"style-references.md", "arc-templates.md"} {
		rel := filepath.Join("references", "genres", style, name)
		raw, src, ok := readOverrideFile(root, rel, report)
		if !ok {
			continue
		}
		genreSetters[name](raw)
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
}

func applyStyleOverrides(bundle *Bundle, root string, report *OverlayReport) {
	dir, entries, ok := readOverrideDir(root, "styles", report)
	if !ok {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if !styleNameRe.MatchString(name) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("忽略非法 style 覆盖文件名 %s", filepath.Join(dir, entry.Name())))
			continue
		}
		rel := filepath.Join("styles", entry.Name())
		raw, src, ok := readOverrideFile(root, rel, report)
		if !ok {
			continue
		}
		bundle.Styles[name] = raw
		recordApplied(bundle, filepath.ToSlash(rel), src, report)
	}
}

func readOverrideFile(root, rel string, report *OverlayReport) (string, ResourceSource, bool) {
	path := filepath.Join(root, rel)
	if _, err := os.Lstat(path); err != nil {
		if !os.IsNotExist(err) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("检查 %s: %v", path, err))
		}
		return "", ResourceSource{}, false
	}
	if ok, err := resolvedPathWithinRoot(root, path); err != nil || !ok {
		report.Warnings = append(report.Warnings, fmt.Sprintf("忽略越出资源根的覆盖文件 %s", path))
		return "", ResourceSource{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("读取 %s: %v", path, err))
		return "", ResourceSource{}, false
	}
	if len(data) > maxOverlayFileBytes {
		report.Warnings = append(report.Warnings, fmt.Sprintf("忽略过大的覆盖文件 %s（上限 %d 字节）", path, maxOverlayFileBytes))
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

func readOverrideDir(root, rel string, report *OverlayReport) (string, []os.DirEntry, bool) {
	dir := filepath.Join(root, rel)
	if _, err := os.Lstat(dir); err != nil {
		if !os.IsNotExist(err) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("检查 %s: %v", dir, err))
		}
		return dir, nil, false
	}
	if ok, err := resolvedPathWithinRoot(root, dir); err != nil || !ok {
		report.Warnings = append(report.Warnings, fmt.Sprintf("忽略越出资源根的覆盖目录 %s", dir))
		return dir, nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("读取 %s: %v", dir, err))
		return dir, nil, false
	}
	return dir, entries, true
}

func resolvedPathWithinRoot(root, path string) (bool, error) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	pathResolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
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

func (b *Bundle) recordEmbeddedSources(style string) {
	record := func(key, value string) {
		src := sourceFor("embedded", "embedded:"+key, []byte(value))
		src.Key = key
		b.Sources[key] = src
	}
	prompts := map[string]string{
		"architect-short.md": b.Prompts.ArchitectShort, "architect-long.md": b.Prompts.ArchitectLong,
		"writer.md": b.Prompts.Writer, "editor.md": b.Prompts.Editor, "polisher.md": b.Prompts.Polisher,
		"import-segment.md":    b.Prompts.ImportSegment,
		"import-foundation.md": b.Prompts.ImportFoundation, "import-chapter-analyzer.md": b.Prompts.ImportAnalyzer,
		"simulation-source.md": b.Prompts.SimulationSource, "simulation-merge.md": b.Prompts.SimulationMerge,
		"arbiter-plan-start.md": b.Prompts.ArbiterPlanStart, "arbiter-intervention.md": b.Prompts.ArbiterIntervention,
		"arbiter-failure.md": b.Prompts.ArbiterFailure,
		"style-critic.md":    b.Prompts.StyleCritic,
	}
	for name, value := range prompts {
		record("prompts/"+name, value)
	}
	record("voice.md", b.Voice)
	refs := map[string]string{
		"chapter-guide.md": b.References.ChapterGuide, "hook-techniques.md": b.References.HookTechniques,
		"quality-checklist.md": b.References.QualityChecklist, "outline-template.md": b.References.OutlineTemplate,
		"character-template.md": b.References.CharacterTemplate, "chapter-template.md": b.References.ChapterTemplate,
		"consistency.md": b.References.Consistency, "content-expansion.md": b.References.ContentExpansion,
		"dialogue-writing.md": b.References.DialogueWriting, "longform-planning.md": b.References.LongformPlanning,
		"differentiation.md": b.References.Differentiation, "anti-ai-tone.md": b.References.AntiAITone,
	}
	for name, value := range refs {
		record("references/"+name, value)
	}
	if style != "" && style != "default" {
		if b.References.StyleReference != "" {
			record("references/genres/"+style+"/style-references.md", b.References.StyleReference)
		}
		if b.References.ArcTemplates != "" {
			record("references/genres/"+style+"/arc-templates.md", b.References.ArcTemplates)
		}
	}
	for name, value := range b.Styles {
		record("styles/"+name+".md", value)
	}
}

// EffectiveResource 返回覆盖和固定包装后的 bundle 文本，供只读诊断计算最终哈希。
func (b Bundle) EffectiveResource(key string) (string, bool) {
	switch filepath.ToSlash(key) {
	case "prompts/architect-short.md":
		return b.Prompts.ArchitectShort, true
	case "prompts/architect-long.md":
		return b.Prompts.ArchitectLong, true
	case "prompts/writer.md":
		return b.Prompts.Writer, true
	case "prompts/editor.md":
		return b.Prompts.Editor, true
	case "prompts/polisher.md":
		return b.Prompts.Polisher, true
	case "prompts/import-segment.md":
		return b.Prompts.ImportSegment, true
	case "prompts/import-foundation.md":
		return b.Prompts.ImportFoundation, true
	case "prompts/import-chapter-analyzer.md":
		return b.Prompts.ImportAnalyzer, true
	case "prompts/simulation-source.md":
		return b.Prompts.SimulationSource, true
	case "prompts/simulation-merge.md":
		return b.Prompts.SimulationMerge, true
	case "prompts/arbiter-plan-start.md":
		return b.Prompts.ArbiterPlanStart, true
	case "prompts/arbiter-intervention.md":
		return b.Prompts.ArbiterIntervention, true
	case "prompts/arbiter-failure.md":
		return b.Prompts.ArbiterFailure, true
	case "prompts/style-critic.md":
		return b.Prompts.StyleCritic, true
	}
	return "", false
}
