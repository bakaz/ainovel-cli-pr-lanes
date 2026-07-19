package imp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
	"github.com/voocel/ainovel-cli/internal/store"
)

type mockLLM struct {
	out string
	err error
	got []agentcore.Message
}

func (m *mockLLM) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	m.got = msgs
	if m.err != nil {
		return nil, m.err
	}
	return &agentcore.LLMResponse{
		Message: agentcore.Message{
			Role:      agentcore.RoleAssistant,
			Content:   []agentcore.ContentBlock{agentcore.TextBlock(m.out)},
			Timestamp: time.Now(),
		},
	}, nil
}

const validEnvelope = `=== PREMISE ===
# 测试书名

## 题材和基调
现代都市悬疑

## 核心冲突
新闻记者追查连环失踪案

## 主角目标
找出真凶并自证清白

## 结局方向
真相大白，主角抉择

## 写作禁区
血腥猎奇，跳脱现实

## 差异化卖点
- 双线叙事
- 女性视角

## 差异化钩子
失踪者全部姓"陈"

## 核心兑现承诺
追完能体验完整悬疑解谜

=== CHARACTERS ===
[
  {"name":"林晚","role":"主角","description":"独立记者","arc":"前期被动追案，后期主动出击","traits":["敏锐","固执"]},
  {"name":"陈沉","role":"反派","description":"幕后凶手","arc":"前期隐蔽，后期暴露","traits":["冷静","残忍"]}
]

=== WORLD_RULES ===
[
  {"category":"society","rule":"现代都市背景，警力体系完备","boundary":"不超自然"}
]

=== LAYERED_OUTLINE ===
[
  {
    "index":1,
    "title":"失踪疑云",
    "theme":"记者追查连环失踪案",
    "arcs":[
      {
        "index":1,
        "title":"初查",
        "goal":"林晚接案并锁定陈姓线索",
        "chapters":[
          {"title":"初遇","core_event":"林晚收到匿名爆料","hook":"线索指向陈姓家族","scenes":["编辑部","咖啡馆"]},
          {"title":"循迹","core_event":"林晚走访失踪者家属","hook":"发现共同祭品符号","scenes":["旧宅","档案馆"]}
        ]
      }
    ]
  }
]

=== COMPASS ===
{
  "ending_direction":"真相大白，主角在揭露与自保间抉择",
  "open_threads":["陈姓家族的祭品仪式真相","林晚的清白指控"],
  "estimated_scale":"预计 20-40 章"
}
`

func TestReverseFoundation_ParsesValid(t *testing.T) {
	llm := &mockLLM{out: validEnvelope}
	chapters := []Chapter{
		{Title: "初遇", Content: "林晚翻开匿名信..."},
		{Title: "循迹", Content: "她敲响那栋旧宅的门..."},
	}
	got, err := ReverseFoundation(context.Background(), llm, "system prompt with ${chapter_count}", chapters, nil)
	if err != nil {
		t.Fatalf("ReverseFoundation: %v", err)
	}
	if !strings.HasPrefix(got.Premise, "# 测试书名") {
		t.Errorf("premise head: %q", got.Premise[:20])
	}
	if len(got.Characters) != 2 || got.Characters[0].Name != "林晚" {
		t.Errorf("characters wrong: %+v", got.Characters)
	}
	if len(got.Volumes) != 1 || len(domain.FlattenOutline(got.Volumes)) != 2 {
		t.Errorf("volumes wrong: %+v", got.Volumes)
	}
	if got.Compass == nil || len(got.Compass.Long.OpenThreads) == 0 {
		t.Errorf("compass should be parsed with open_threads: %+v", got.Compass)
	}
	if !strings.Contains(llm.got[0].TextContent(), "with 2") {
		t.Errorf("system prompt expected ${chapter_count}=2 substituted, got: %q",
			llm.got[0].TextContent())
	}
	if !strings.Contains(llm.got[1].TextContent(), "林晚翻开匿名信") {
		t.Errorf("user prompt should contain chapter 1 content")
	}
}

func TestReverseFoundation_RejectsLengthMismatch(t *testing.T) {
	llm := &mockLLM{out: validEnvelope}
	chapters := []Chapter{
		{Title: "ch1", Content: "..."},
		{Title: "ch2", Content: "..."},
		{Title: "ch3", Content: "..."},
	}
	_, err := ReverseFoundation(context.Background(), llm, "x", chapters, nil)
	if err == nil || !strings.Contains(err.Error(), "chapter count mismatch") {
		t.Fatalf("want chapter-count-mismatch error, got %v", err)
	}
}

func TestReverseFoundation_MissingTagFails(t *testing.T) {
	llm := &mockLLM{out: "=== PREMISE ===\n# x\n"}
	_, err := ReverseFoundation(context.Background(), llm,
		"x", []Chapter{{Title: "a", Content: "b"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "missing required tags") {
		t.Fatalf("want missing-tags error, got %v", err)
	}
}

func TestReverseFoundationV3_RejectsTrueLegacyStringScenes(t *testing.T) {
	llm := &mockLLM{out: validEnvelope} // envelope 中 scenes 是真实 JSON string 数组
	chapters := []Chapter{
		{Title: "初遇", Content: "林晚翻开匿名信..."},
		{Title: "循迹", Content: "她敲响那栋旧宅的门..."},
	}

	_, err := ReverseFoundation(context.Background(), llm, "system", chapters, projectprofile.NewSceneBeatV3Contract())
	if err == nil || !strings.Contains(err.Error(), "legacy string scene") {
		t.Fatalf("v3 ReverseFoundation must reject parsed legacy string scenes, got %v", err)
	}
}

func TestParseFoundation_FencedJSONStripped(t *testing.T) {
	src := strings.ReplaceAll(validEnvelope,
		`=== CHARACTERS ===
[`,
		"=== CHARACTERS ===\n```json\n[",
	)
	src = strings.ReplaceAll(src, `]

=== WORLD_RULES ===`, "]\n```\n\n=== WORLD_RULES ===")
	got, err := parseFoundationOutput(src, 2)
	if err != nil {
		t.Fatalf("fenced parse: %v", err)
	}
	if len(got.Characters) != 2 {
		t.Errorf("characters: %+v", got.Characters)
	}
}

func TestPersistFoundation_PromotesPhaseToWriting(t *testing.T) {
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Progress.Init("import-test", 0); err != nil {
		t.Fatalf("init progress: %v", err)
	}

	fr := mustParse(t, validEnvelope, 2)
	if err := PersistFoundation(context.Background(), st, domain.PlanningTierShort, fr, nil); err != nil {
		t.Fatalf("PersistFoundation: %v", err)
	}

	prog, err := st.Progress.Load()
	if err != nil {
		t.Fatalf("load progress: %v", err)
	}
	if prog.Phase != domain.PhaseWriting {
		t.Errorf("phase: got %q want writing", prog.Phase)
	}
	if prog.TotalChapters != 2 {
		t.Errorf("total chapters: %d", prog.TotalChapters)
	}
	if !prog.Layered {
		t.Errorf("imported book must be layered so it can be continued/extended")
	}
	if c, _ := st.Outline.LoadCompass(); c == nil {
		t.Errorf("compass must be saved for continuation")
	}
	if prog.NovelName != "测试书名" {
		t.Errorf("novel name: %q", prog.NovelName)
	}
	if got := st.FoundationMissing(); len(got) != 0 {
		t.Errorf("foundation should be complete, missing: %v", got)
	}
}

func mustParse(t *testing.T, raw string, expect int) *FoundationResult {
	t.Helper()
	fr, err := parseFoundationOutput(raw, expect)
	if err != nil {
		t.Fatalf("parse helper: %v", err)
	}
	return fr
}

// ── V3 Import dual-boundary tests (Phase 2 terminal blocker 3) ──

type fsSnapshot map[string]string

func takeFSSnapshot(dir string) fsSnapshot {
	snap := make(fsSnapshot)
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		h := sha256.Sum256(data)
		snap[filepath.ToSlash(rel)] = fmt.Sprintf("%x", h)
		return nil
	})
	return snap
}

func assertFSSnapshotUnchanged(t *testing.T, before, after fsSnapshot) {
	t.Helper()
	for p, hash := range before {
		if after[p] != hash {
			t.Errorf("file %q changed: before=%s after=%s", p, hash, after[p])
		}
	}
	if len(after) > len(before) {
		for p := range after {
			if _, ok := before[p]; !ok {
				t.Errorf("new file created: %s", p)
			}
		}
	}
}

// validV3Scene 返回一个合法的 V3 SceneBeat（七字段非空）。
func validV3Scene() domain.SceneBeat {
	return domain.SceneBeat{
		Goal: "g", Action: "a", Conflict: "c", Outcome: "o",
		BodyReaction: "b", EmotionReaction: "e", EroticCharge: "ec",
	}
}

// TestV3Import_ValidateFoundationResult 验证 V3 契约下 ValidateFoundationResult 的
// parse 边界：legacy/missing/empty 均拒绝。
func TestV3Import_ValidateFoundationResult(t *testing.T) {
	v3 := projectprofile.NewSceneBeatV3Contract()

	validFR := &FoundationResult{
		Premise:    "# 书名\n\n正文",
		Characters: []domain.Character{{Name: "主角", Role: "main"}},
		WorldRules: []domain.WorldRule{{Category: "magic", Rule: "规则"}},
		Volumes: []domain.VolumeOutline{{
			Index: 1, Title: "第一卷", Theme: "主题",
			Arcs: []domain.ArcOutline{{
				Index: 1, Title: "弧", Goal: "目标",
				Chapters: []domain.OutlineEntry{{
					Chapter: 1, Title: "章", CoreEvent: "事件",
					Scenes: []domain.SceneBeat{validV3Scene()},
				}},
			}},
		}},
		Compass: &domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "终局"}},
	}

	// 正常 V3 通过
	if err := ValidateFoundationResult(validFR, v3); err != nil {
		t.Fatalf("valid V3 FR should pass: %v", err)
	}

	// legacy scene → 拒绝
	legacyFR := copyFR(validFR)
	legacyFR.Volumes[0].Arcs[0].Chapters[0].Scenes[0] = domain.SceneBeat{Action: "legacy"}
	if err := ValidateFoundationResult(legacyFR, v3); err == nil {
		t.Error("V3 should reject legacy scene")
	}

	// no ending_direction → 拒绝
	noDirFR := copyFR(validFR)
	noDirFR.Compass.Long.EndingDirection = ""
	if err := ValidateFoundationResult(noDirFR, v3); err == nil {
		t.Error("V3 should reject missing ending_direction")
	}

	// empty scenes → 拒绝（bypass: 有 arcs 但 chapter 无 scenes）
	emptyScenesFR := copyFR(validFR)
	emptyScenesFR.Volumes[0].Arcs[0].Chapters[0].Scenes = nil
	if err := ValidateFoundationResult(emptyScenesFR, v3); err == nil {
		t.Error("V3 should reject empty scenes")
	}
}

// TestV3Import_PersistFoundation 验证 V3 契约下 PersistFoundation 的持久化边界：
// legacy/missing/empty 均不落盘（snapshot 不变）。
func TestV3Import_PersistFoundation(t *testing.T) {
	v3 := projectprofile.NewSceneBeatV3Contract()
	validFR := &FoundationResult{
		Premise:    "# 书名\n\n正文",
		Characters: []domain.Character{{Name: "主角", Role: "main"}},
		WorldRules: []domain.WorldRule{{Category: "magic", Rule: "规则"}},
		Volumes: []domain.VolumeOutline{{
			Index: 1, Title: "第一卷", Theme: "主题",
			Arcs: []domain.ArcOutline{{
				Index: 1, Title: "弧", Goal: "目标",
				Chapters: []domain.OutlineEntry{{
					Chapter: 1, Title: "章", CoreEvent: "事件",
					Scenes: []domain.SceneBeat{validV3Scene()},
				}},
			}},
		}},
		// 基线 Compass 必须合法，保证每个负例确实在 scene 边界被拒绝。
		Compass: &domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "终局"}},
	}

	tests := []struct {
		name string
		mut  func(*FoundationResult)
	}{
		{"legacy_scene", func(fr *FoundationResult) {
			fr.Volumes[0].Arcs[0].Chapters[0].Scenes[0] = domain.SceneBeat{Action: "legacy"}
		}},
		{"missing_field", func(fr *FoundationResult) {
			fr.Volumes[0].Arcs[0].Chapters[0].Scenes[0] = domain.SceneBeat{Goal: "g", Action: "a", Conflict: "c", Outcome: "o"}
		}},
		{"empty_scenes", func(fr *FoundationResult) {
			fr.Volumes[0].Arcs[0].Chapters[0].Scenes = nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := store.NewStore(dir)
			if err := st.Init(); err != nil {
				t.Fatalf("init: %v", err)
			}
			if err := st.RunMeta.Init("default", "test", "test"); err != nil {
				t.Fatalf("run meta: %v", err)
			}
			fr := copyFR(validFR)
			tc.mut(fr)

			before := takeFSSnapshot(dir)
			err := PersistFoundation(context.Background(), st, domain.PlanningTierShort, fr, v3)
			if err == nil {
				t.Fatal("V3 persist should fail for invalid FR")
			}
			assertFSSnapshotUnchanged(t, before, takeFSSnapshot(dir))
		})
	}
}

func copyFR(fr *FoundationResult) *FoundationResult {
	data, _ := json.Marshal(fr)
	var cp FoundationResult
	_ = json.Unmarshal(data, &cp)
	return &cp
}
