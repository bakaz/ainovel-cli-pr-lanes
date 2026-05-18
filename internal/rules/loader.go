package rules

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LoadOptions 是 Load 的输入参数。
//
// 文件不存在不算错误，loader 静默跳过；解析失败不阻断，conflicts 由 parser 写入 Parsed.Conflicts。
type LoadOptions struct {
	// RulesFS 是 assets/rules 子树。约定根目录直接包含 default.md 与 genres/。
	// 通常通过 fs.Sub(embedFS, "rules") 得到；nil 表示跳过内置规则。
	RulesFS fs.FS

	// HomeRulesPath 是 ~/.ainovel/rules.md 的绝对路径；空表示跳过。
	HomeRulesPath string

	// ProjectRulesPath 是 ./rules.md（或调用方指定的项目根）；空表示跳过。
	ProjectRulesPath string

	// LearnedRulesPath 是 output/<novel>/meta/rules.learned.md；Phase 1 一般为空，Phase 2 启用。
	LearnedRulesPath string
}

// Load 按两阶段策略加载所有来源，返回按 SourceKind 升序排好的 Parsed 列表。
//
// 阶段 1：default / global / learned / project（按出现顺序加载）
// 阶段 2：从已加载的来源中解析 effective genre（就近优先），若非空则加载题材文件并插入
//
// merger 接收返回值后只需按列表顺序合并即可，后者覆盖前者。
func Load(opts LoadOptions) []Parsed {
	var layers []Parsed

	// 阶段 1：加载非 genre 来源
	if p, ok := readFromFS(opts.RulesFS, "default.md", SourceDefault, "assets/rules/default.md"); ok {
		layers = append(layers, p)
	}
	if p, ok := readFromDisk(opts.HomeRulesPath, SourceGlobal); ok {
		layers = append(layers, p)
	}
	if p, ok := readFromDisk(opts.LearnedRulesPath, SourceLearned); ok {
		layers = append(layers, p)
	}
	if p, ok := readFromDisk(opts.ProjectRulesPath, SourceProject); ok {
		layers = append(layers, p)
	}

	// 阶段 2：解析 effective genre（项目 > learned > global > default，就近优先）
	genre := resolveGenre(layers)
	if genre != "" {
		genrePath := "genres/" + genre + ".md"
		if p, ok := readFromFS(opts.RulesFS, genrePath, SourceGenre, "assets/rules/"+genrePath); ok {
			// 题材文件不允许再声明 genre，避免递归歧义
			if p.Structured.Genre != "" {
				p.Conflicts = append(p.Conflicts, Conflict{
					Source: p.Source,
					Kind:   ConflictUnknownField,
					Field:  "genre",
					Detail: "题材文件不能再声明 genre，已忽略",
				})
				p.Structured.Genre = ""
			}
			// 插入到 default 之后、global 之前（保持 SourceKind 升序）
			layers = insertByKind(layers, p)
		}
	}

	return layers
}

// resolveGenre 按优先级（高 → 低）扫描 layers，返回首个非空 Genre 字段。
func resolveGenre(layers []Parsed) string {
	highest := SourceKind(-1)
	chosen := ""
	for _, p := range layers {
		if p.Structured.Genre == "" {
			continue
		}
		if p.Kind > highest {
			highest = p.Kind
			chosen = p.Structured.Genre
		}
	}
	return chosen
}

// insertByKind 按 SourceKind 升序插入 layers，保持顺序稳定。
func insertByKind(layers []Parsed, p Parsed) []Parsed {
	idx := 0
	for ; idx < len(layers); idx++ {
		if layers[idx].Kind > p.Kind {
			break
		}
	}
	return slices.Insert(layers, idx, p)
}

// readFromFS 从 fs.FS 读取并解析；文件不存在返回 (Parsed{}, false)。
// displayPath 用于 Parsed.Source（便于在 sources/conflicts 里显示为 "assets/rules/..."）。
func readFromFS(fsys fs.FS, name string, kind SourceKind, displayPath string) (Parsed, bool) {
	if fsys == nil {
		return Parsed{}, false
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		// 文件不存在静默跳过；其他错误也不阻断（loader 设计上不报错）
		if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
			return Parsed{}, false
		}
		// 极少数 IO 错误：作为 parse_error 暴露，避免静默
		return Parsed{
			Source: displayPath,
			Kind:   kind,
			Conflicts: []Conflict{{
				Source: displayPath,
				Kind:   ConflictParseError,
				Detail: "读取失败: " + err.Error(),
			}},
		}, true
	}
	return Parse(displayPath, kind, data), true
}

// readFromDisk 从绝对路径读取并解析；空路径或文件不存在返回 (Parsed{}, false)。
func readFromDisk(absPath string, kind SourceKind) (Parsed, bool) {
	if strings.TrimSpace(absPath) == "" {
		return Parsed{}, false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Parsed{}, false
		}
		return Parsed{
			Source: absPath,
			Kind:   kind,
			Conflicts: []Conflict{{
				Source: absPath,
				Kind:   ConflictParseError,
				Detail: "读取失败: " + err.Error(),
			}},
		}, true
	}
	return Parse(absPath, kind, data), true
}

// DefaultProjectRulesPath 拼出 ./rules.md 的绝对路径（基于给定项目目录）。
// 调用方传入项目根，避免在 loader 内部依赖 cwd。
func DefaultProjectRulesPath(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	return filepath.Join(projectDir, "rules.md")
}

// DefaultHomeRulesPath 拼出 ~/.ainovel/rules.md 的绝对路径。
// home 解析失败返回空串（调用方据此跳过该来源）。
func DefaultHomeRulesPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ainovel", "rules.md")
}

// DefaultLearnedRulesPath 拼出 output/<novel>/meta/rules.learned.md 的绝对路径。
// outputDir 应为当前 novel 的 output 目录（如 output/西游/）。
func DefaultLearnedRulesPath(outputDir string) string {
	if outputDir == "" {
		return ""
	}
	return filepath.Join(outputDir, "meta", "rules.learned.md")
}

// DefaultOptions 根据当前工作目录与 output 目录构造常用 LoadOptions。
//
// 适合 Host 启动时调用一次，让 ContextTool / CommitChapterTool 复用同一份配置。
// 解析 cwd 失败时 ProjectRulesPath 留空（loader 会跳过该来源）。
//
// 路径语义（Phase 1 决策）：
//
//   - ProjectRulesPath 绑定 **当前工作目录（cwd）** 而非 outputDir。
//     符合项目"按 cwd 隔离多本书"的现有约定（参见 README 多本书切换说明）：
//     用户 cd 到不同目录启动写不同的书，./rules.md 自然跟着 cwd 走。
//
//   - LearnedRulesPath 绑定 **outputDir**（output/<novel>/meta/rules.learned.md）。
//     Phase 2 save_rule 写入这里，跟具体小说绑定，跨书不共享。
//
//   - 多本书并行的边缘场景：用户从同一 cwd 用 -o 切换 outputDir 启动多本书时，
//     这些书会共享 ./rules.md。如需严格"本书规则"隔离，应把偏好放 ~/.ainovel/rules.md
//     全局层，或 cd 到独立目录启动。Phase 1 不引入 BookLocalRulesPath 这层抽象。
func DefaultOptions(rulesFS fs.FS, outputDir string) LoadOptions {
	cwd, _ := os.Getwd()
	return LoadOptions{
		RulesFS:          rulesFS,
		HomeRulesPath:    DefaultHomeRulesPath(),
		ProjectRulesPath: DefaultProjectRulesPath(cwd),
		LearnedRulesPath: DefaultLearnedRulesPath(outputDir),
	}
}
