package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildWriterPrompt_ByteIdenticalToPreSplit 是文风层验收标准 ①:
// 不放任何覆盖文件时,组装产物与拆分前的 writer.md 管线逐字节一致。
// golden 是拆分前 writer.md 的原始快照(testdata/writer-golden.md)。
func TestBuildWriterPrompt_ByteIdenticalToPreSplit(t *testing.T) {
	golden, err := os.ReadFile("testdata/writer-golden.md")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	protocol := mustRead(promptsFS, "prompts/writer.md")
	voice := mustRead(voiceFS, "voice.md")

	// 文件级:占位符回填 == 拆分前原文
	if got := strings.Replace(protocol, voicePlaceholder, strings.TrimSpace(voice), 1); got != string(golden) {
		t.Fatalf("占位符回填与拆分前不一致:\n--- 长度 golden=%d got=%d", len(golden), len(got))
	}

	// 管线级:新组装 == 旧管线(writer.md → simGuidance → style)
	const style = "## 某风格\n\n- 测试"
	old := WithSimulationGuidance(string(golden), "writer") + "\n\n" + style
	got := BuildWriterPrompt(WithSimulationGuidance(protocol, "writer"), voice, style)
	if got != old {
		t.Fatal("组装管线与拆分前不等价")
	}

	// 无风格追加时也等价
	if BuildWriterPrompt(WithSimulationGuidance(protocol, "writer"), voice, "") != WithSimulationGuidance(string(golden), "writer") {
		t.Fatal("无 style 时组装管线与拆分前不等价")
	}
}

// TestWriterPrompt_StyleGoalFieldKeysMatchSchema 验证 writer.md 中 style_goal
// 的五字段名与 Go schema 中的 JSON key 完全一致,避免 prompt 与实现不同步。
func TestWriterPrompt_StyleGoalFieldKeysMatchSchema(t *testing.T) {
	protocol := mustRead(promptsFS, "prompts/writer.md")
	requiredKeys := []string{
		"focal_filter",
		"prose_movement",
		"detail_strategy",
		"rhythm",
		"variation_from_recent",
	}
	for _, key := range requiredKeys {
		if !strings.Contains(protocol, key) {
			t.Errorf("writer.md 缺少 schema field key %q", key)
		}
	}
	// Verify the enclosing object name matches schema
	if !strings.Contains(protocol, "`style_goal`") {
		t.Error("writer.md 必须用 `style_goal` 引用 schema 对象名")
	}
}

// TestWriterPrompt_StylePrecedenceIsConsistent 验证文风优先级指引无矛盾
// (Gate 1 Oracle 跟进: writer_style_card > manual anchors 在优先级列表明确,
// style_anchors_manual 不含歧义的优先标记,抽象风格冲突规则限域到 simulation_profile)。
func TestWriterPrompt_StylePrecedenceIsConsistent(t *testing.T) {
	protocol := mustRead(promptsFS, "prompts/writer.md")

	// 显式优先级链必须存在且 writer_style_card 高于 manual anchors
	if !strings.Contains(protocol, "writer_style_card（如有）> manual anchors") {
		t.Error("优先级列表必须明确 writer_style_card > manual anchors")
	}

	// style_anchors_manual 不得带裸 "（优先）" 标签——这与 writer_style_card > manual anchors 矛盾
	if strings.Contains(protocol, "style_anchors_manual`**（优先）") {
		t.Error("style_anchors_manual 不得有裸 '（优先）' 标签 — 与优先级列表 writer_style_card > manual anchors 矛盾")
	}

	// 抽象风格冲突规则必须限域到 simulation_profile, 不能作为孤立的通用优先级声明
	if !strings.Contains(protocol, "`simulation_profile` 中的抽象风格冲突以 manual anchors 为准") {
		t.Error("抽象风格冲突规则必须明确限域到 `simulation_profile`，避免与优先级列表矛盾")
	}
}

// TestLoad_NoOverrides 零覆盖时 Voice/AntiAITone 与内置逐字节一致。
func TestLoad_NoOverrides(t *testing.T) {
	b := Load("default", LoadOptions{})
	if b.Voice != mustRead(voiceFS, "voice.md") {
		t.Fatal("无覆盖时 Voice 应与内置逐字节一致")
	}
	if b.References.AntiAITone != mustRead(referencesFS, "references/anti-ai-tone.md") {
		t.Fatal("无覆盖时 AntiAITone 应与内置逐字节一致")
	}
	if _, ok := b.Styles["default"]; !ok {
		t.Fatal("内置风格集应含 default")
	}
}

// TestStyleCriticPrompt_Loaded 验证 style-critic.md 嵌入并可通过 Load 正常访问。
func TestStyleCriticPrompt_Loaded(t *testing.T) {
	b := Load("default", LoadOptions{})
	if b.Prompts.StyleCritic == "" {
		t.Fatal("StyleCritic prompt 应非空")
	}
	if !contains(t, b.Prompts.StyleCritic, "verdict") || !contains(t, b.Prompts.StyleCritic, "pass|revise") {
		t.Fatal("StyleCritic 须包含 verdict 字段和 pass|revise 枚举")
	}
	if !contains(t, b.Prompts.StyleCritic, "findings") {
		t.Fatal("StyleCritic 须包含 findings 字段")
	}
	if !contains(t, b.Prompts.StyleCritic, "dimension") {
		t.Fatal("StyleCritic 须包含 dimension 字段")
	}
	if !contains(t, b.Prompts.StyleCritic, "severity") {
		t.Fatal("StyleCritic 须包含 severity 字段")
	}
	if !contains(t, b.Prompts.StyleCritic, "strength") {
		t.Fatal("StyleCritic 须包含 strength 正面评价")
	}
	// 须包含版本标识
	if !contains(t, b.Prompts.StyleCritic, "v1") && !contains(t, b.Prompts.StyleCritic, "version") {
		t.Fatal("StyleCritic 须含版本标识")
	}
	// 禁止给出改写文本
	if !contains(t, b.Prompts.StyleCritic, "不重写") && !contains(t, b.Prompts.StyleCritic, "不得给出具体措辞") {
		t.Fatal("StyleCritic 须禁止给出改写文本")
	}
}

// TestStyleCriticPrompt_DomainEnumContract 验证 style-critic.md 的有效维度/类别/severity
// 与 domain 包中的枚举完全对齐。
func TestStyleCriticPrompt_DomainEnumContract(t *testing.T) {
	b := Load("default", LoadOptions{})
	p := b.Prompts.StyleCritic

	// 所有有效维度必须出现
	for _, dim := range []string{"consistency", "character", "pacing", "continuity", "foreshadow", "hook", "aesthetic"} {
		if !contains(t, p, dim) {
			t.Errorf("style-critic.md 须包含有效 dimension %q", dim)
		}
	}

	// 所有有效类别必须出现
	for _, cat := range []string{"plot", "style", "logic", "tone", "grammar"} {
		if !contains(t, p, cat) {
			t.Errorf("style-critic.md 须包含有效 category %q", cat)
		}
	}

	// 所有有效 severity 必须出现
	for _, sev := range []string{"critical", "error", "warning", "info"} {
		if !contains(t, p, sev) {
			t.Errorf("style-critic.md 须包含有效 severity %q", sev)
		}
	}

	// verdict 必须是严格枚举
	if !contains(t, p, `"pass"`) || !contains(t, p, `"revise"`) {
		t.Error("style-critic.md verdict 枚举必须含 pass 和 revise")
	}

	// strength 必须存在且必须含 evidence
	if !contains(t, p, "\"evidence\"") {
		t.Error("style-critic.md strength 必须含 evidence 字段")
	}

	// 最多 3 条 findings
	if !contains(t, p, "最多 3 条") && !contains(t, p, "最多 3") {
		t.Error("style-critic.md 必须约束 findings 最多 3 条")
	}

	// 必须声明格式不合法 = invalid
	if !contains(t, p, "不合法") && !contains(t, p, "格式不合法") && !contains(t, p, "视为格式不合法") {
		t.Error("style-critic.md 必须声明超出有效列表的值为格式不合法")
	}

	// 不得声称接收了未提供的输入（basis 中的字段是合法输入——critic只收到 draft + basis
	// 两个顶层输入，其中 basis 包含 style_goal / chapter_contract / compass_* /
	// anchor_excerpts / user_rules / factual_outline / critic_version）。
	if contains(t, p, "chapter_plan") || contains(t, p, "style_rules") {
		t.Error("style-critic.md 不应声称接收未在 basis 中定义的独立输入 chapter_plan / style_rules")
	}
}

// TestStyleCritic_OverlaySupportsReplacement 验证 style-critic.md 可通过 overlay 替换。
func TestStyleCritic_OverlaySupportsReplacement(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "prompts", "style-critic.md"), "CUSTOM_CRITIC_v1")
	bundle := Load("default", LoadOptions{})
	report := ApplyOverrides(&bundle, "default", []string{root})
	if bundle.Prompts.StyleCritic != "CUSTOM_CRITIC_v1" {
		t.Fatalf("overlay 替换失败: got %q", bundle.Prompts.StyleCritic)
	}
	if len(report.Warnings) > 0 {
		t.Fatalf("不应有警告: %v", report.Warnings)
	}
	src, ok := bundle.Sources["prompts/style-critic.md"]
	if !ok || src.Kind != "file" || len(src.SHA256) != 64 {
		t.Fatalf("source 记录缺失或格式错误: %+v", src)
	}
}

func contains(t *testing.T, s, sub string) bool {
	t.Helper()
	return strings.Contains(s, sub)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestLoad_ThreeTierAppendAndReplace 覆盖三层优先级与逐资产语义(验收标准 ②)。
func TestLoad_ThreeTierAppendAndReplace(t *testing.T) {
	home := t.TempDir()
	book := t.TempDir()
	opts := LoadOptions{HomeStyleDir: home, BookStyleDir: book}

	// voice / anti-ai-tone:追加语义,全局在前、本书在后,带边界标记
	writeFile(t, filepath.Join(home, "voice.md"), "全局:少用成语")
	writeFile(t, filepath.Join(book, "voice.md"), "本书:多写对话")
	writeFile(t, filepath.Join(book, "anti-ai-tone.md"), "本书判据:禁排比")

	// styles:同名整文件替换 + 新名新增;非法名忽略
	writeFile(t, filepath.Join(home, "styles", "fantasy.md"), "全局改写的奇幻")
	writeFile(t, filepath.Join(book, "styles", "xianxia.md"), "自定义仙侠")
	writeFile(t, filepath.Join(book, "styles", "Bad Name!.md"), "非法")

	// 题材参考:同名整文件替换,本书 > 全局
	writeFile(t, filepath.Join(home, "genres", "fantasy", "style-references.md"), "全局参考")
	writeFile(t, filepath.Join(book, "genres", "fantasy", "style-references.md"), "本书参考")

	b := Load("fantasy", opts)

	builtinVoice := mustRead(voiceFS, "voice.md")
	if !strings.HasPrefix(b.Voice, builtinVoice) {
		t.Fatal("追加语义必须保留内置原文为前缀")
	}
	giIdx := strings.Index(b.Voice, "## 用户全局文风覆盖")
	bkIdx := strings.Index(b.Voice, "## 本书文风覆盖")
	if giIdx < 0 || bkIdx < 0 || giIdx > bkIdx {
		t.Fatalf("追加段顺序错误:global=%d book=%d", giIdx, bkIdx)
	}
	if !strings.Contains(b.Voice, "全局:少用成语") || !strings.Contains(b.Voice, "本书:多写对话") {
		t.Fatal("覆盖内容缺失")
	}
	if !strings.Contains(b.References.AntiAITone, "本书判据:禁排比") {
		t.Fatal("anti-ai-tone 本书追加缺失")
	}

	if b.Styles["fantasy"] != "全局改写的奇幻" {
		t.Fatal("styles 同名应整文件替换")
	}
	if b.Styles["xianxia"] != "自定义仙侠" {
		t.Fatal("新增自定义风格应即放即用")
	}
	if _, ok := b.Styles["Bad Name!"]; ok {
		t.Fatal("非法风格名必须被忽略")
	}

	if b.References.StyleReference != "本书参考" {
		t.Fatalf("题材参考应为本书覆盖优先,got %q", b.References.StyleReference)
	}
}

// TestLoad_BookOverridesHomeOnStyles 本书 styles 覆盖全局同名。
func TestLoad_BookOverridesHomeOnStyles(t *testing.T) {
	home := t.TempDir()
	book := t.TempDir()
	writeFile(t, filepath.Join(home, "styles", "romance.md"), "全局版")
	writeFile(t, filepath.Join(book, "styles", "romance.md"), "本书版")
	b := Load("default", LoadOptions{HomeStyleDir: home, BookStyleDir: book})
	if b.Styles["romance"] != "本书版" {
		t.Fatalf("本书应覆盖全局,got %q", b.Styles["romance"])
	}
}

// TestOverrideVoice_SharesAssemblyPath eval 的 voice A/B 与生产同组装路径(验收标准 ④)。
func TestOverrideVoice_SharesAssemblyPath(t *testing.T) {
	b := Load("default", LoadOptions{})
	b.OverrideVoice("## 实验文风\n\n- 一句话")
	got := BuildWriterPrompt(b.Prompts.Writer, b.Voice, "")
	if !strings.Contains(got, "## 实验文风") {
		t.Fatal("OverrideVoice 未生效")
	}
	if strings.Contains(got, voicePlaceholder) {
		t.Fatal("占位符必须被消耗")
	}
	// 协议部分不受 voice 覆盖影响
	if !strings.Contains(got, "## 执行协议") {
		t.Fatal("协议模板不得被 voice 覆盖破坏")
	}
}

// TestWriterPrompt_NoUnconditionalDirectCommit 验证 writer.md 不含"没有硬伤直接 commit"
// 类无条件提交措辞——协议必须按 mode 决定 commit 时机（Gate 2 Oracle：
// off 模式待 check→commit 才可提交，critic 模式必须经 review_style→terminal 后才可 commit）。
func TestWriterPrompt_NoUnconditionalDirectCommit(t *testing.T) {
	protocol := mustRead(promptsFS, "prompts/writer.md")
	forbidden := []string{
		"没有硬伤直接 commit_chapter",
		"没有硬伤直接 `commit_chapter`",
	}
	for _, phrase := range forbidden {
		if strings.Contains(protocol, phrase) {
			t.Errorf("writer.md 禁止出现无条件提交措辞: %q", phrase)
		}
	}
	// 必须包含 mode 感知的提交指引
	if !strings.Contains(protocol, "required_next_action: \"commit_chapter\"") &&
		!strings.Contains(protocol, "required_next_action: `commit_chapter`") {
		t.Error("writer.md 必须引用 required_next_action 作为 commit 的准入条件")
	}
	if !strings.Contains(protocol, "review_style") || !strings.Contains(protocol, "terminal") {
		t.Error("writer.md critic 模式指引必须要求 review_style→terminal→commit")
	}
}

// TestWriterPrompt_RequiredNextActionIsMandatory 验证 required_next_action 被描述为
// 强制操作而非建议（Gate 2 Oracle：非空时下一次工具调用必须执行该 action）。
func TestWriterPrompt_RequiredNextActionIsMandatory(t *testing.T) {
	protocol := mustRead(promptsFS, "prompts/writer.md")
	// 不得再用"建议"来描述 required_next_action
	if strings.Contains(protocol, "作为流程建议") {
		t.Error("required_next_action 不应再描述为'流程建议'")
	}
	// 必须包含"必须执行"语义
	if !strings.Contains(protocol, "必须执行") {
		t.Error("required_next_action 必须明确'必须执行'语义")
	}
	// 必须说明缺失不代表可 commit
	if !strings.Contains(protocol, "不代表可 commit") && !strings.Contains(protocol, "不代表可") {
		t.Error("required_next_action 字段缺失说明必须声明'不代表可 commit'")
	}
}
