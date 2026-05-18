package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// TestLoad_TwoStageGenre 验证 genre 两阶段加载：
// 项目规则声明 genre=xianxia → loader 二次加载 assets/rules/genres/xianxia.md
func TestLoad_TwoStageGenre(t *testing.T) {
	// 模拟 embed FS：assets/rules/ 子树
	rulesFS := fstest.MapFS{
		"default.md": {Data: []byte("---\nchapter_words: 3000-6000\n---\n")},
		"genres/xianxia.md": {Data: []byte(
			"---\nforbidden_chars:\n  - \"——\"\n---\n# 仙侠偏好\n- 避免现代词\n")},
	}
	// 项目级 rules.md 声明 genre=xianxia
	tmp := t.TempDir()
	projectPath := filepath.Join(tmp, "rules.md")
	if err := os.WriteFile(projectPath, []byte("---\ngenre: xianxia\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layers := Load(LoadOptions{
		RulesFS:          rulesFS,
		ProjectRulesPath: projectPath,
	})

	// 期望加载：default + genre + project 三层
	if len(layers) != 3 {
		t.Fatalf("expected 3 layers, got %d: %+v", len(layers), layers)
	}
	// 顺序：default < genre < project
	expectKinds := []SourceKind{SourceDefault, SourceGenre, SourceProject}
	for i, want := range expectKinds {
		if layers[i].Kind != want {
			t.Errorf("layer[%d].Kind=%v, want %v", i, layers[i].Kind, want)
		}
	}
	// 仙侠 genre 应贡献 forbidden_chars
	b := Merge(layers)
	if len(b.Structured.ForbiddenChars) == 0 || b.Structured.ForbiddenChars[0] != "——" {
		t.Errorf("expected forbidden_chars from genre, got %v", b.Structured.ForbiddenChars)
	}
	if !strings.Contains(b.Preferences, "仙侠偏好") {
		t.Errorf("genre body missing: %q", b.Preferences)
	}
}

func TestLoad_NoGenreDeclared(t *testing.T) {
	rulesFS := fstest.MapFS{
		"default.md":        {Data: []byte("---\nchapter_words: 3000-6000\n---\n")},
		"genres/xianxia.md": {Data: []byte("---\nforbidden_chars:\n  - \"——\"\n---\n")},
	}
	layers := Load(LoadOptions{RulesFS: rulesFS})
	// 无 genre 声明 → 只加载 default，不触发 genre 文件
	if len(layers) != 1 || layers[0].Kind != SourceDefault {
		t.Errorf("expected only default layer, got %+v", layers)
	}
}

func TestLoad_GenreFileMissing(t *testing.T) {
	// 项目声明 genre=unknown，但 genres/unknown.md 不存在 → 静默跳过
	rulesFS := fstest.MapFS{
		"default.md": {Data: []byte("")},
	}
	tmp := t.TempDir()
	projectPath := filepath.Join(tmp, "rules.md")
	os.WriteFile(projectPath, []byte("---\ngenre: nonexistent\n---\n"), 0644)

	layers := Load(LoadOptions{
		RulesFS:          rulesFS,
		ProjectRulesPath: projectPath,
	})
	// 期望：default + project（genre 文件缺失，不加 layer，不报错）
	if len(layers) != 2 {
		t.Errorf("expected 2 layers (default + project), got %d", len(layers))
	}
	for _, p := range layers {
		if p.Kind == SourceGenre {
			t.Errorf("genre layer should be absent when file missing, got %+v", p)
		}
	}
}

func TestLoad_GenreFileCannotDeclareGenre(t *testing.T) {
	// genre 文件里再声明 genre 应被忽略，并写一条 conflict
	rulesFS := fstest.MapFS{
		"default.md":        {Data: []byte("")},
		"genres/xianxia.md": {Data: []byte("---\ngenre: another\n---\n")},
	}
	tmp := t.TempDir()
	projectPath := filepath.Join(tmp, "rules.md")
	os.WriteFile(projectPath, []byte("---\ngenre: xianxia\n---\n"), 0644)

	layers := Load(LoadOptions{
		RulesFS:          rulesFS,
		ProjectRulesPath: projectPath,
	})

	var genreLayer *Parsed
	for i := range layers {
		if layers[i].Kind == SourceGenre {
			genreLayer = &layers[i]
			break
		}
	}
	if genreLayer == nil {
		t.Fatal("expected genre layer")
	}
	if genreLayer.Structured.Genre != "" {
		t.Errorf("genre field in genre file should be cleared, got %q", genreLayer.Structured.Genre)
	}
	// 应有 conflict
	found := false
	for _, c := range genreLayer.Conflicts {
		if c.Field == "genre" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected conflict on genre field, got %+v", genreLayer.Conflicts)
	}
}

func TestLoad_LayerOrderProjectWinsGenreResolution(t *testing.T) {
	// global 说 genre=urban，project 说 genre=xianxia → 期望 xianxia 文件被加载
	rulesFS := fstest.MapFS{
		"default.md":        {Data: []byte("")},
		"genres/urban.md":   {Data: []byte("---\nforbidden_chars:\n  - 都市\n---\n")},
		"genres/xianxia.md": {Data: []byte("---\nforbidden_chars:\n  - 仙侠\n---\n")},
	}
	tmp := t.TempDir()
	globalPath := filepath.Join(tmp, "global.md")
	projectPath := filepath.Join(tmp, "project.md")
	os.WriteFile(globalPath, []byte("---\ngenre: urban\n---\n"), 0644)
	os.WriteFile(projectPath, []byte("---\ngenre: xianxia\n---\n"), 0644)

	layers := Load(LoadOptions{
		RulesFS:          rulesFS,
		HomeRulesPath:    globalPath,
		ProjectRulesPath: projectPath,
	})
	// 找到 genre 层，应是 xianxia
	var genrePath string
	for _, p := range layers {
		if p.Kind == SourceGenre {
			genrePath = p.Source
			break
		}
	}
	if !strings.Contains(genrePath, "xianxia") {
		t.Errorf("expected xianxia genre to win (project > global), got %q", genrePath)
	}
}

func TestLoad_NilFSDoesNotPanic(t *testing.T) {
	// 入参全空：不崩，返回空 layers
	layers := Load(LoadOptions{})
	if len(layers) != 0 {
		t.Errorf("expected 0 layers, got %d", len(layers))
	}
}
