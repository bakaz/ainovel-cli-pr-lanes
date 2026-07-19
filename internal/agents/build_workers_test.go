package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// TestBuildWorkers_ToolComposition 验证 buildWorkerToolsets 各子代理的工具列表组成。
// 直接调用生产级 helper（BuildWorkers 也使用同一来源），不重复构造。
func TestBuildWorkers_ToolComposition(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	bundle := assets.Load("default", assets.LoadOptions{})
	contract := projectprofile.NewCore4Contract()
	ts := buildWorkerToolsets(st, bundle, "default", contract)

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

	// novel_context 指针身份：architect_short/long 共享同一实例，与 writer/editor 隔离。
	shortCtx := ts.ArchitectShort[0]
	longCtx := ts.ArchitectLong[0]
	writerCtx := ts.Writer[0]
	editorCtx := ts.Editor[0]
	if shortCtx != longCtx {
		t.Error("architect_short and architect_long should share the same novel_context instance")
	}
	if shortCtx == writerCtx {
		t.Error("architect and writer should use different novel_context instances")
	}
	if writerCtx == editorCtx {
		t.Error("writer and editor should use different novel_context instances")
	}

	t.Log("architect_short:", toolNames(ts.ArchitectShort))
	t.Log("architect_long:", toolNames(ts.ArchitectLong))
	t.Log("writer:", toolNames(ts.Writer))
	t.Log("editor:", toolNames(ts.Editor))
}

func TestBuildWorkerToolsetsWiresTUILongApprovalIntoArchitect(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{
		EndingDirection: "原终局", EstimatedScale: "5卷",
	}}); err != nil {
		t.Fatal(err)
	}
	ask := tools.NewAskUserTool()
	ask.EnableTUILongApproval()
	ask.SetHandler(func(_ context.Context, questions []tools.Question) (*tools.AskUserResponse, error) {
		return &tools.AskUserResponse{Answers: map[string]string{questions[0].Question: "批准"}}, nil
	})
	ts := buildWorkerToolsetsWithApproval(st, assets.Bundle{}, "default", projectprofile.NewCore4Contract(), ask)
	var save agentcore.Tool
	for _, candidate := range ts.ArchitectLong {
		if candidate.Name() == "save_foundation" {
			save = candidate
			break
		}
	}
	if save == nil {
		t.Fatal("architect_long missing save_foundation")
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "update_compass", "section": "long", "reason": "用户批准扩篇",
		"content": map[string]any{"estimated_scale": "6卷"},
	})
	if _, err := save.Execute(context.Background(), raw); err != nil {
		t.Fatalf("approved long proposal failed through production toolset: %v", err)
	}
	got, err := st.Outline.LoadCompass()
	if err != nil {
		t.Fatal(err)
	}
	if got.Long.EstimatedScale != "6卷" {
		t.Fatalf("approved long proposal was not saved: %+v", got.Long)
	}
}

// TestBuildWorkers_ExternalPromptStyleAndV3FinalAssembly 走生产装配的关键边界：
// 外置 prompt/style -> Bundle -> BuildWorkers -> agentcore subagent.Config.SystemPrompt。
// 该字段就是 agentcore 每次 Provider 请求所用的 system prompt。
func TestBuildWorkers_ExternalPromptStyleAndV3FinalAssembly(t *testing.T) {
	bookDir := t.TempDir()
	st := store.NewStore(bookDir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	overlayRoot := t.TempDir()
	writeOverlay := func(rel, content string) {
		t.Helper()
		path := filepath.Join(overlayRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeOverlay("prompts/architect-short.md", "ARCH_SHORT_EXTERNAL_CANARY")
	writeOverlay("prompts/architect-long.md", "ARCH_LONG_EXTERNAL_CANARY")
	writeOverlay("prompts/writer.md", "WRITER_EXTERNAL_CANARY\n{{VOICE}}")
	writeOverlay("prompts/editor.md", "EDITOR_EXTERNAL_CANARY")
	writeOverlay("styles/default.md", "STYLE_EXTERNAL_CANARY")

	bundle := assets.Load("default", assets.LoadOptions{})
	bundle.Voice = "VOICE_EXTERNAL_CANARY"
	report := assets.ApplyOverrides(&bundle, "default", []string{overlayRoot})
	if len(report.Warnings) != 0 {
		t.Fatalf("unexpected overlay warnings: %+v", report.Warnings)
	}

	cfg := bootstrap.Config{
		Provider: "ollama", ModelName: "dummy-model", Style: "default",
		Providers: map[string]bootstrap.ProviderConfig{"ollama": {BaseURL: "http://0.0.0.0:0"}},
	}
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tool, _, _, _ := BuildWorkers(cfg, st, models, bundle, nil, nil, projectprofile.NewSceneBeatV3Contract())
	want := map[string]string{
		"architect_short": "ARCH_SHORT_EXTERNAL_CANARY",
		"architect_long":  "ARCH_LONG_EXTERNAL_CANARY",
		"writer":          "WRITER_EXTERNAL_CANARY",
		"editor":          "EDITOR_EXTERNAL_CANARY",
	}
	for role, canary := range want {
		ac, ok := tool.AgentConfig(role)
		if !ok {
			t.Fatalf("agent %s missing", role)
		}
		if !strings.Contains(ac.SystemPrompt, canary) {
			t.Errorf("%s external prompt did not reach final SystemPrompt", role)
		}
		if !strings.Contains(ac.SystemPrompt, "body_reaction") || !strings.Contains(ac.SystemPrompt, "emotion_reaction") {
			t.Errorf("%s lost embedded v3 contract guidance", role)
		}
	}
	writer, _ := tool.AgentConfig("writer")
	positions := []int{
		strings.Index(writer.SystemPrompt, "WRITER_EXTERNAL_CANARY"),
		strings.Index(writer.SystemPrompt, "VOICE_EXTERNAL_CANARY"),
		strings.Index(writer.SystemPrompt, "STYLE_EXTERNAL_CANARY"),
		strings.Index(writer.SystemPrompt, "body_reaction"),
	}
	if positions[0] < 0 || !(positions[0] < positions[1] && positions[1] < positions[2] && positions[2] < positions[3]) {
		t.Fatalf("writer assembly order must be external prompt < voice < style < v3 guidance, positions=%v", positions)
	}
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

// getNestedProp 沿 keys 路径递归获取 schema map 值。
func getNestedProp(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys[:len(keys)-1] {
		v, ok := m[k]
		if !ok {
			return nil
		}
		m, ok = v.(map[string]any)
		if !ok {
			return nil
		}
	}
	v, ok := m[keys[len(keys)-1]]
	if !ok {
		return nil
	}
	out, _ := v.(map[string]any)
	return out
}

// getAnyOfBranch 从 schema 的 content.anyOf 中返回第 i 个 branch。
func getAnyOfBranch(schema map[string]any, idx int) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	content, _ := props["content"].(map[string]any)
	anyOf, _ := content["anyOf"].([]any)
	if idx < 0 || idx >= len(anyOf) {
		return nil
	}
	branch, _ := anyOf[idx].(map[string]any)
	return branch
}

// getOutlineSceneSchema 从 schema 的 outline anyOf 分支提取 scenes.items（sceneBeatSchema）。
// V3 schema 使用顶层 anyOf（遍历查找 properties.type.enum 包含 "outline" 的分支）。
// Core4 schema 使用 content-level anyOf（索引 1 = outline）。
func getOutlineSceneSchema(schema map[string]any) map[string]any {
	// 尝试顶层 anyOf（v3 schema）
	anyOf, _ := schema["anyOf"].([]any)
	if len(anyOf) > 0 {
		for _, br := range anyOf {
			b, _ := br.(map[string]any)
			if b == nil {
				continue
			}
			bProps, _ := b["properties"].(map[string]any)
			if bProps == nil {
				continue
			}
			typeField, _ := bProps["type"].(map[string]any)
			if typeField == nil {
				continue
			}
			enum, ok := typeField["enum"].([]any)
			if !ok {
				if es, ok2 := typeField["enum"].([]string); ok2 {
					enum = make([]any, len(es))
					for i, v := range es {
						enum[i] = v
					}
				}
			}
			if len(enum) != 1 || enum[0] != "outline" {
				continue
			}
			contentSchema, _ := bProps["content"].(map[string]any)
			if contentSchema == nil {
				continue
			}
			return getNestedProp(contentSchema, "items", "properties", "scenes", "items")
		}
		return nil
	}
	// 回落 Core4 方式（content.anyOf[1].items.properties.scenes.items）
	return getNestedProp(getAnyOfBranch(schema, 1), "items", "properties", "scenes", "items")
}

// getBodyReactionDesc 从 sceneBeatSchema 中提取 body_reaction 的 description。
func getBodyReactionDesc(sceneSchema map[string]any) string {
	props, _ := sceneSchema["properties"].(map[string]any)
	br, _ := props["body_reaction"].(map[string]any)
	d, _ := br["description"].(string)
	return d
}

// getEmotionReactionDesc 从 sceneBeatSchema 中提取 emotion_reaction 的 description。
func getEmotionReactionDesc(sceneSchema map[string]any) string {
	props, _ := sceneSchema["properties"].(map[string]any)
	er, _ := props["emotion_reaction"].(map[string]any)
	d, _ := er["description"].(string)
	return d
}

// TestBuildWorkers_V3GuidanceInSaveFoundationSchema 验证 v3 契约下的 guidance 注入
// 到 save_foundation 工具的 schema 中。
func TestBuildWorkers_V3GuidanceInSaveFoundationSchema(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	bundle := assets.Load("default", assets.LoadOptions{})

	// v3 契约 → 内建 guidance
	v3Contract := projectprofile.NewSceneBeatV3Contract()
	ts := buildWorkerToolsets(st, bundle, "default", v3Contract)
	for name, tools := range map[string][]agentcore.Tool{
		"architect_short": ts.ArchitectShort,
		"architect_long":  ts.ArchitectLong,
	} {
		t.Run(name, func(t *testing.T) {
			var sf agentcore.Tool
			for _, tool := range tools {
				if tool.Name() == "save_foundation" {
					sf = tool
					break
				}
			}
			if sf == nil {
				t.Fatal("missing save_foundation tool")
			}
			sceneSchema := getOutlineSceneSchema(sf.Schema())
			if sceneSchema == nil {
				t.Fatal("outline branch scenes.items not found")
			}
			// v3 契约应有 body_reaction description
			if d := getBodyReactionDesc(sceneSchema); d == "" {
				t.Error("body_reaction.description should not be empty for v3 contract")
			}
			// v3 契约应包含 erotic_charge field
			if props, ok := sceneSchema["properties"].(map[string]any); ok {
				if _, hasEC := props["erotic_charge"]; !hasEC {
					t.Error("v3 scene schema should have erotic_charge property")
				}
			}
		})
	}

	// Core4 契约 → 不包含 erotic_charge
	core4Contract := projectprofile.NewCore4Contract()
	ts2 := buildWorkerToolsets(st, bundle, "default", core4Contract)
	for _, tools := range [][]agentcore.Tool{ts2.ArchitectShort, ts2.ArchitectLong} {
		for _, tool := range tools {
			if tool.Name() == "save_foundation" {
				sceneSchema := getOutlineSceneSchema(tool.Schema())
				if props, ok := sceneSchema["properties"].(map[string]any); ok {
					if _, hasEC := props["erotic_charge"]; hasEC {
						t.Error("Core4 scene schema should NOT have erotic_charge property")
					}
				}
			}
		}
	}
}

// TestAppendSceneGuidance 验证 appendSceneGuidance 的行为。
func TestAppendSceneGuidance(t *testing.T) {
	// 空 guidance 返回原 prompt
	if got := appendSceneGuidance("原始提示", ""); got != "原始提示" {
		t.Errorf("空 guidance 应返回原 prompt, got %q", got)
	}
	// 非空 guidance 追加在末尾
	got := appendSceneGuidance("原始提示", "场景指导块")
	want := "原始提示\n\n场景指导块"
	if got != want {
		t.Errorf("appendSceneGuidance = %q, want %q", got, want)
	}
	// writer 场景：模拟 voice/style 组装后的 prompt 再追加 guidance
	basePrompt := "writer 协议模板\n{{VOICE}}\n风格预设"
	voiceStyleApplied := strings.Replace(basePrompt, "{{VOICE}}", "文风段", 1) + "\n\n### 风格\n风格详细内容"
	result := appendSceneGuidance(voiceStyleApplied, "场景指导块")
	if !strings.HasSuffix(result, "场景指导块") {
		t.Errorf("guidance 应在 prompt 最末尾, got: %s", result)
	}
	if !strings.Contains(result, "文风段") {
		t.Errorf("guidance 追加不应影响 voice 段")
	}
	if !strings.Contains(result, "风格详细内容") {
		t.Errorf("guidance 追加不应影响 style 段")
	}
}

// TestBuildWorkers_V3GuidanceProductionWiring 验证 v3 契约下 BuildWorkers 的 SystemPrompt
// 及 save_foundation Schema 使用内建 guidance。
func TestBuildWorkers_V3GuidanceProductionWiring(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bundle := assets.Load("default", assets.LoadOptions{})
	bundle.Voice = "###VOICE_SENTINEL###"
	bundle.Styles["default"] = "\n###STYLE_SENTINEL###"

	cfg := bootstrap.Config{
		Provider:  "ollama",
		ModelName: "dummy-model",
		Providers: map[string]bootstrap.ProviderConfig{
			"ollama": {BaseURL: "http://0.0.0.0:0"},
		},
		Style: "default",
	}
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}

	v3Contract := projectprofile.NewSceneBeatV3Contract()
	subagentTool, _, _, _ := BuildWorkers(cfg, st, models, bundle, nil, nil, v3Contract)

	for _, name := range []string{"architect_short", "architect_long", "writer", "editor"} {
		t.Run(name, func(t *testing.T) {
			ac, ok := subagentTool.AgentConfig(name)
			if !ok {
				t.Fatalf("agent %q not found", name)
			}
			prompt := ac.SystemPrompt

			// v3 契约的 guidance 片段应包含 body_reaction/emotion_reaction/erotic_charge 指引
			if !strings.Contains(prompt, "body_reaction") {
				t.Errorf("v3 prompt 应含 body_reaction 字段指导")
			}
			if !strings.Contains(prompt, "emotion_reaction") {
				t.Errorf("v3 prompt 应含 emotion_reaction 字段指导")
			}
			if !strings.Contains(prompt, "erotic_charge") {
				t.Errorf("v3 prompt 应含 erotic_charge 字段指导")
			}
		})
	}

	// writer 特检：guidance 在 voice/style 之后（硬断言）
	t.Run("writer_ordering", func(t *testing.T) {
		ac, ok := subagentTool.AgentConfig("writer")
		if !ok {
			t.Fatal("writer not found")
		}
		prompt := ac.SystemPrompt

		voicePos := strings.Index(prompt, "###VOICE_SENTINEL###")
		stylePos := strings.Index(prompt, "###STYLE_SENTINEL###")
		guidancePos := strings.Index(prompt, "body_reaction")

		if voicePos < 0 {
			t.Fatal("writer prompt 应包含 voice sentinel")
		}
		if stylePos < 0 {
			t.Fatal("writer prompt 应包含 style sentinel")
		}
		if guidancePos < 0 {
			t.Fatal("writer prompt 应包含 body_reaction 字段")
		}

		// 硬断言：voice < style < guidance
		if !(voicePos < stylePos && stylePos < guidancePos) {
			t.Errorf("writer prompt 顺序错误: voicePos=%d, stylePos=%d, guidancePos=%d; 期望 voice < style < guidance",
				voicePos, stylePos, guidancePos)
		}
	})

	// Core4 契约应不含 erotic_charge 指导
	t.Run("core4_no_erotic_charge", func(t *testing.T) {
		core4Contract := projectprofile.NewCore4Contract()
		subagentTool2, _, _, _ := BuildWorkers(cfg, st, models, bundle, nil, nil, core4Contract)
		ac, ok := subagentTool2.AgentConfig("architect_short")
		if !ok {
			t.Fatal("architect_short not found")
		}
		if strings.Contains(ac.SystemPrompt, "erotic_charge") {
			t.Error("Core4 prompt should not contain erotic_charge guidance")
		}
	})
}
