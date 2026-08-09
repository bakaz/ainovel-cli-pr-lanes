package ctxpack

import (
	"context"
	"strings"
	"testing"

	"github.com/voocel/agentcore"
	corecontext "github.com/voocel/agentcore/context"
	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

func TestStoreSummaryCompactApplyUsesPersistentStoreData(t *testing.T) {
	s := seededWriterStore(t)
	strategy := NewStoreSummaryCompact(StoreSummaryCompactConfig{
		Store:              s,
		KeepRecentTokens:   80,
		SummaryTokenBudget: 2000,
	})

	msgs := []agentcore.AgentMessage{
		agentcore.UserMsg(strings.Repeat("旧上下文", 80)),
		agentcore.Message{
			Role:    agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{agentcore.TextBlock(strings.Repeat("旧回复", 80))},
		},
		agentcore.UserMsg("继续写第三章，注意承接第二章结尾。"),
		agentcore.Message{
			Role:    agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{agentcore.TextBlock("收到，我先梳理当前场景。")},
		},
	}

	out, result, err := strategy.Apply(context.Background(), msgs, msgs, corecontext.Budget{
		Tokens:    corecontext.EstimateTotal(msgs),
		Window:    128,
		Threshold: 32,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Applied {
		t.Fatal("expected store summary strategy to apply")
	}
	if result.Name != storeSummaryStrategyName {
		t.Fatalf("unexpected strategy name: %q", result.Name)
	}
	if len(out) < 2 {
		t.Fatalf("expected summary + kept messages, got %d", len(out))
	}
	summary, ok := out[0].(corecontext.ContextSummary)
	if !ok {
		t.Fatalf("expected ContextSummary, got %T", out[0])
	}
	if !strings.Contains(summary.Summary, "最近章节摘要") {
		t.Fatalf("expected persistent summaries in checkpoint, got %q", summary.Summary)
	}
	if !strings.Contains(summary.Summary, "当前章节计划") {
		t.Fatalf("expected chapter plan in checkpoint, got %q", summary.Summary)
	}
	if !strings.Contains(summary.Summary, "活跃伏笔") {
		t.Fatalf("expected foreshadow data in checkpoint, got %q", summary.Summary)
	}
	if !strings.Contains(summary.Summary, "待修审稿问题") {
		t.Fatalf("expected pending review section in checkpoint, got %q", summary.Summary)
	}
	if !strings.Contains(summary.Summary, "仓库线索需要再蓄压一拍") {
		t.Fatalf("expected pending review details in checkpoint, got %q", summary.Summary)
	}
	if result.Info == nil || result.Info.CompactedCount <= 0 {
		t.Fatalf("expected compaction info, got %+v", result.Info)
	}
}

func TestStoreSummaryCompactApplyFallsBackWhenStoreDataInsufficient(t *testing.T) {
	dir := t.TempDir()
	s := storepkg.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Save(&domain.Progress{
		Phase:             domain.PhaseWriting,
		CurrentChapter:    1,
		TotalChapters:     3,
		CompletedChapters: nil,
	}); err != nil {
		t.Fatalf("Save progress: %v", err)
	}

	strategy := NewStoreSummaryCompact(StoreSummaryCompactConfig{Store: s, KeepRecentTokens: 20})
	msgs := []agentcore.AgentMessage{
		agentcore.UserMsg(strings.Repeat("旧上下文", 40)),
		agentcore.Message{
			Role:    agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{agentcore.TextBlock(strings.Repeat("旧回复", 40))},
		},
	}

	out, result, err := strategy.Apply(context.Background(), msgs, msgs, corecontext.Budget{
		Tokens:    corecontext.EstimateTotal(msgs),
		Window:    64,
		Threshold: 16,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Applied {
		t.Fatal("expected no-op when persistent memory is insufficient")
	}
	if len(out) != len(msgs) {
		t.Fatalf("expected messages unchanged, got %d", len(out))
	}
}

func TestWriterRestorePackRefreshReusesStoreBuilder(t *testing.T) {
	s := seededWriterStore(t)
	pack := &WriterRestorePack{}
	pack.Refresh(s)

	msg, ok := pack.buildMessage(restoreBudgetTokens)
	if !ok {
		t.Fatal("expected restore pack message")
	}
	text := msg.TextContent()
	if !strings.Contains(text, "<post-compact-context>") {
		t.Fatalf("expected wrapped restore context, got %q", text)
	}
	if !strings.Contains(text, "待修审稿问题") {
		t.Fatalf("expected pending review section, got %q", text)
	}
	if !strings.Contains(text, "当前章节计划") {
		t.Fatalf("expected chapter plan section, got %q", text)
	}
}

func TestWriterStoreSectionsIncludeWorldRules(t *testing.T) {
	s := seededWriterStore(t)
	if err := s.World.SaveWorldRules([]domain.WorldRule{
		{Category: "magic", Rule: "灵气浓度随深度变化", Boundary: "禁止超过人体承受上限"},
		{Category: "society", Rule: "宗门弟子禁止私斗", Boundary: "违者逐出山门"},
	}); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林岚", Field: "body_device.collar", Value: "声控锁", UpdatedChapter: 2},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}

	summary, ok, err := buildWriterStoreSummaryText(s, defaultStoreSummaryBudgetTokens)
	if err != nil || !ok {
		t.Fatalf("buildWriterStoreSummaryText: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(summary, "世界规则") || !strings.Contains(summary, "灵气浓度随深度变化") {
		t.Fatalf("summary missing WorldRules section, got %q", summary)
	}
	if !strings.Contains(summary, "角色受控状态") || !strings.Contains(summary, "声控锁") {
		t.Fatalf("summary missing CharacterState section, got %q", summary)
	}

	restore, ok, err := buildWriterRestoreText(s, restoreBudgetTokens)
	if err != nil || !ok {
		t.Fatalf("buildWriterRestoreText: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(restore, "世界规则") || !strings.Contains(restore, "宗门弟子禁止私斗") {
		t.Fatalf("restore missing WorldRules section, got %q", restore)
	}
	if !strings.Contains(restore, "角色受控状态") || !strings.Contains(restore, "声控锁") {
		t.Fatalf("restore missing CharacterState section, got %q", restore)
	}
}

// 较小预算下，canon 段（计划/状态/伏笔）先于世界规则渲染且完整输出；
// 24KiB 的 WorldRules 排在后面，预算不足时被截断，不挤掉关键状态。
func TestWriterStoreSectionsSmallBudgetKeepsCanonBeforeWorldRules(t *testing.T) {
	s := seededWriterStore(t)
	var rules []domain.WorldRule
	for i := 0; i < 30; i++ {
		rules = append(rules, domain.WorldRule{
			Category: "magic",
			Rule:     strings.Repeat("规则边界描述文本填充内容", 20),
			Boundary: strings.Repeat("禁止越界", 10),
		})
	}
	if err := s.World.SaveWorldRules(rules); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林岚", Field: "body_device.collar", Value: "声控锁", UpdatedChapter: 2},
		{Entity: "林岚", Field: "health.injury", Value: "左臂旧伤", UpdatedChapter: 1},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}

	const smallBudget = 400
	summary, ok, err := buildWriterStoreSummaryText(s, smallBudget)
	if err != nil || !ok {
		t.Fatalf("buildWriterStoreSummaryText: ok=%v err=%v", ok, err)
	}
	restore, ok, err := buildWriterRestoreText(s, smallBudget)
	if err != nil || !ok {
		t.Fatalf("buildWriterRestoreText: ok=%v err=%v", ok, err)
	}

	for _, out := range []struct{ name, text string }{{"summary", summary}, {"restore", restore}} {
		for _, want := range []string{"当前章节计划", "角色受控状态", "活跃伏笔"} {
			if !strings.Contains(out.text, want) {
				t.Fatalf("%s: small budget dropped canon section %q, got %q", out.name, want, out.text)
			}
		}
		idxCS := strings.Index(out.text, "角色受控状态")
		idxFS := strings.Index(out.text, "活跃伏笔")
		idxWR := strings.Index(out.text, "世界规则")
		if idxCS < 0 || idxFS < 0 {
			t.Fatalf("%s: canon sections missing, got %q", out.name, out.text)
		}
		if idxWR >= 0 && (idxWR < idxCS || idxWR < idxFS) {
			t.Fatalf("%s: WorldRules rendered before canon sections (cs=%d fs=%d wr=%d), got %q",
				out.name, idxCS, idxFS, idxWR, out.text)
		}
	}
}

// 进度段压缩：completed_chapters 明细只保留最近 progressCompletedTail 章，
// completed_count 保留总量。
func TestWriterStoreProgressSectionCompressesCompletedChapters(t *testing.T) {
	completed := make([]int, 1000)
	for i := range completed {
		completed[i] = i + 1
	}
	state := &writerStoreSummaryState{chapter: 1001, progress: &domain.Progress{
		Phase:             domain.PhaseWriting,
		Flow:              domain.FlowWriting,
		CompletedChapters: completed,
	}}
	sec := writerStoreProgressSection(state)
	if sec == nil {
		t.Fatal("expected progress section")
	}
	if sec["completed_count"] != 1000 {
		t.Fatalf("completed_count = %v, want 1000", sec["completed_count"])
	}
	tail, ok := sec["completed_chapters"].([]int)
	if !ok {
		t.Fatalf("completed_chapters type %T", sec["completed_chapters"])
	}
	if len(tail) != progressCompletedTail {
		t.Fatalf("completed_chapters tail len = %d, want %d", len(tail), progressCompletedTail)
	}
	if tail[len(tail)-1] != 1000 {
		t.Fatalf("tail should end with latest chapter, got %v", tail)
	}
}

// 巨大 completed_chapters（1000 章）时：进度段被压缩（completed_count 保留、
// 明细受限），且 plan/outline/CharacterState/伏笔/WorldRules 全部渲染。
func TestWriterStoreSectionsHugeProgressKeepsAllCanonRendered(t *testing.T) {
	s := seededWriterStore(t)
	completed := make([]int, 1000)
	for i := range completed {
		completed[i] = i + 1
	}
	if err := s.Progress.Save(&domain.Progress{
		Phase:             domain.PhaseWriting,
		CurrentChapter:    1001,
		TotalChapters:     1200,
		CompletedChapters: completed,
		Flow:              domain.FlowWriting,
	}); err != nil {
		t.Fatalf("Save progress: %v", err)
	}
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{Chapter: 1001, Title: "第一千零一章", Goal: "推进主线"}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	if err := s.Summaries.SaveSummary(domain.ChapterSummary{
		Chapter:    1000,
		Summary:    "第一千章：主线推进到关键节点。",
		Characters: []string{"林岚"},
	}); err != nil {
		t.Fatalf("SaveSummary 1000: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1001, Title: "第一千零一章", CoreEvent: "推进主线"},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林岚", Field: "body_device.collar", Value: "声控锁", UpdatedChapter: 999},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{
		{Category: "magic", Rule: "灵气浓度随深度变化", Boundary: "禁止越界"},
	}); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}

	summary, ok, err := buildWriterStoreSummaryText(s, 600)
	if err != nil || !ok {
		t.Fatalf("buildWriterStoreSummaryText: ok=%v err=%v", ok, err)
	}
	restore, ok, err := buildWriterRestoreText(s, 600)
	if err != nil || !ok {
		t.Fatalf("buildWriterRestoreText: ok=%v err=%v", ok, err)
	}
	for _, out := range []struct{ name, text string }{{"summary", summary}, {"restore", restore}} {
		if !strings.Contains(out.text, `"completed_count":1000`) {
			t.Fatalf("%s: expected completed_count=1000 preserved, got %q", out.name, out.text)
		}
		for _, want := range []string{"当前章节计划", "当前章节大纲", "角色受控状态", "活跃伏笔", "世界规则"} {
			if !strings.Contains(out.text, want) {
				t.Fatalf("%s: canon section %q missing with huge progress, got %q", out.name, want, out.text)
			}
		}
	}
}

// 巨大 plan 使某 canon 段超限时：该段独立截断（[已截断]），后续 canon
// （大纲/状态/伏笔/世界规则）仍全部渲染——单段不能独占余量。
func TestWriterStoreSectionsCanonSectionOverLimitTruncatedIndependently(t *testing.T) {
	s := seededWriterStore(t)
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 3,
		Title:   "第三章",
		Goal:    strings.Repeat("规划填充文本", 2000), // ~12K 字，远超份额
		Hook:    "仓库中的录音",
	}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林岚", Field: "body_device.collar", Value: "声控锁", UpdatedChapter: 2},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{
		{Category: "magic", Rule: "灵气浓度随深度变化", Boundary: "禁止越界"},
	}); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}

	summary, ok, err := buildWriterStoreSummaryText(s, 400)
	if err != nil || !ok {
		t.Fatalf("buildWriterStoreSummaryText: ok=%v err=%v", ok, err)
	}
	restore, ok, err := buildWriterRestoreText(s, 400)
	if err != nil || !ok {
		t.Fatalf("buildWriterRestoreText: ok=%v err=%v", ok, err)
	}
	for _, out := range []struct{ name, text string }{{"summary", summary}, {"restore", restore}} {
		if !strings.Contains(out.text, "当前章节计划") || !strings.Contains(out.text, "[已截断]") {
			t.Fatalf("%s: expected truncated plan section, got %q", out.name, out.text)
		}
		for _, want := range []string{"当前章节大纲", "角色受控状态", "活跃伏笔", "世界规则"} {
			if !strings.Contains(out.text, want) {
				t.Fatalf("%s: canon section %q lost after over-limit plan, got %q", out.name, want, out.text)
			}
		}
	}
}

func seededWriterStore(t *testing.T) *storepkg.Store {
	t.Helper()

	s := storepkg.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Save(&domain.Progress{
		Phase:             domain.PhaseWriting,
		CurrentChapter:    3,
		TotalChapters:     6,
		CompletedChapters: []int{1, 2},
		Flow:              domain.FlowWriting,
	}); err != nil {
		t.Fatalf("Save progress: %v", err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{
		{Chapter: 1, Title: "第一章", CoreEvent: "开场"},
		{Chapter: 2, Title: "第二章", CoreEvent: "冲突升级"},
		{Chapter: 3, Title: "第三章", CoreEvent: "追查线索", Scenes: domain.SceneList{{Action: "主角追查失踪案"}, {Action: "发现旧仓库线索"}}},
	}); err != nil {
		t.Fatalf("SaveOutline: %v", err)
	}
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter:    3,
		Title:      "第三章",
		Goal:       "推进失踪案调查",
		Conflict:   "主角与搭档对调查方向分歧",
		Hook:       "仓库中发现可疑录音",
		EmotionArc: "怀疑到紧张",
	}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	if err := s.Summaries.SaveSummary(domain.ChapterSummary{
		Chapter:    1,
		Summary:    "主角接下委托，发现失踪案并不简单。",
		Characters: []string{"林岚", "周策"},
		KeyEvents:  []string{"委托成立"},
	}); err != nil {
		t.Fatalf("SaveSummary 1: %v", err)
	}
	if err := s.Summaries.SaveSummary(domain.ChapterSummary{
		Chapter:    2,
		Summary:    "两人追查旧码头，线索指向废弃仓库。",
		Characters: []string{"林岚", "周策", "沈叔"},
		KeyEvents:  []string{"旧码头冲突", "仓库线索出现"},
	}); err != nil {
		t.Fatalf("SaveSummary 2: %v", err)
	}
	if err := s.World.SaveForeshadowLedger([]domain.ForeshadowEntry{
		{ID: "tape", Description: "失踪者留下的录音带", PlantedAt: 2, Status: "planted"},
	}); err != nil {
		t.Fatalf("SaveForeshadowLedger: %v", err)
	}
	if err := s.World.SaveTimeline([]domain.TimelineEvent{
		{Chapter: 2, Time: "夜晚", Event: "旧码头交锋", Characters: []string{"林岚", "周策"}},
	}); err != nil {
		t.Fatalf("SaveTimeline: %v", err)
	}
	if err := s.World.SaveStyleRules(domain.WritingStyleRules{
		Prose:  []string{"句子偏短，保持压迫感"},
		Taboos: []string{"避免直白解释谜团"},
	}); err != nil {
		t.Fatalf("SaveStyleRules: %v", err)
	}
	if err := s.World.SaveReview(domain.ReviewEntry{
		Chapter: 2,
		Scope:   "chapter",
		Verdict: "polish",
		Summary: "第二章结尾铺垫偏急，需要补一拍仓库前的压迫感。",
		Issues: []domain.ConsistencyIssue{
			{
				Type:        "pacing",
				Severity:    "warning",
				Description: "仓库线索出现过快，悬疑蓄压不够。",
				Suggestion:  "在进入仓库前增加一段迟疑与环境压迫描写。",
			},
		},
		ContractMisses: []string{"章末钩子不够强"},
	}); err != nil {
		t.Fatalf("Save chapter review: %v", err)
	}
	if err := s.World.SaveReview(domain.ReviewEntry{
		Chapter: 2,
		Scope:   "global",
		Verdict: "polish",
		Summary: "第二章尾声节奏偏快，仓库线索需要再蓄压一拍。",
	}); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}
	return s
}

// TestWriterStoreRestoreBudgetRealisticChinese 验证 R1：大段中文 canon 内容在
// 小预算下，完整 restore 文本（含 wrapper）的真实估算 ≤ 预算，且 buildMessage 接受
// （防止截断器 byte 估算与正式 CJK 估算器分裂导致恢复包被整体拒绝）。
func TestWriterStoreRestoreBudgetRealisticChinese(t *testing.T) {
	s := seededWriterStore(t)
	bigPlan := strings.Repeat("这一卷的规划目标是深入探索宗门禁地与上古遗迹的秘密，", 60)
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 3, Title: "禁地", Goal: bigPlan,
	}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{
		{Category: "magic", Rule: strings.Repeat("灵气浓度随深度指数增长，深层禁地禁止使用灵力，违者经脉逆行，", 50), Boundary: strings.Repeat("禁止超过人体承受上限，违规者即刻废弃，", 30)},
	}); err != nil {
		t.Fatalf("SaveWorldRules: %v", err)
	}
	if err := s.World.SaveCharacterState([]domain.CharacterStateEntry{
		{Entity: "林岚", Field: "body_device.collar", Value: strings.Repeat("声控锁，需要持续供能，断电即收缩，", 20), UpdatedChapter: 3},
	}); err != nil {
		t.Fatalf("SaveCharacterState: %v", err)
	}

	const budget = 900
	restore, ok, err := buildWriterRestoreText(s, budget)
	if err != nil || !ok {
		t.Fatalf("buildWriterRestoreText: ok=%v err=%v", ok, err)
	}
	if got := corecontext.EstimateTokens(agentcore.UserMsg(restore)); got > budget {
		t.Fatalf("restore text exceeds budget: %d > %d", got, budget)
	}
	for _, want := range []string{"当前章节计划", "角色受控状态", "世界规则"} {
		if !strings.Contains(restore, want) {
			t.Fatalf("restore missing canon section %q", want)
		}
	}
	pack := &WriterRestorePack{}
	pack.text = restore // 直接用 900 预算构建的文本（Refresh 会用默认预算重建，不适用本场景）
	msg, ok2 := pack.buildMessage(budget)
	if !ok2 {
		t.Fatal("buildMessage should accept budget-fitting restore")
	}
	if !strings.Contains(msg.TextContent(), "<post-compact-context>") {
		t.Fatalf("expected wrapped restore, got %q", msg.TextContent())
	}
}

// TestTruncateSectionToBudgetMarkerEdge 覆盖二分边界：heading 可容纳但
// heading+截断标记不可容纳时，必须降级 heading-only，不得返回超预算文本。
func TestTruncateSectionToBudgetMarkerEdge(t *testing.T) {
	heading := "测试段"
	prefix := "## " + heading + "\n"
	body := strings.Repeat("这是一段很长的中文正文内容，", 200)
	marker := " [已截断]"
	budget := corecontext.EstimateTokens(agentcore.UserMsg(prefix))
	out, used := truncateSectionToBudget(heading, body, marker, budget)
	if out == "" {
		t.Fatal("expected non-empty truncation")
	}
	if used > budget {
		t.Fatalf("used %d exceeds budget %d", used, budget)
	}
	if got := corecontext.EstimateTokens(agentcore.UserMsg(out)); got > budget {
		t.Fatalf("output %d exceeds budget %d", got, budget)
	}
}

// TestWriterStoreRestoreFinalBudgetMixedDominance 覆盖完整文本预算兜底：
// 大段中文 canon 与较大 ASCII optional 混合（估算器按完整文本判断 dominance，
// 分项估算不可加），最终 restore 文本仍必须落入预算。
func TestWriterStoreRestoreFinalBudgetMixedDominance(t *testing.T) {
	s := seededWriterStore(t)
	bigPlan := strings.Repeat("这一卷的规划目标是深入探索宗门禁地与上古遗迹的秘密，", 60)
	if err := s.Drafts.SaveChapterPlan(domain.ChapterPlan{
		Chapter: 3, Title: "禁地", Goal: bigPlan,
	}); err != nil {
		t.Fatalf("SaveChapterPlan: %v", err)
	}
	bigASCII := strings.Repeat("the ancient gate hums with forbidden power, ", 80)
	if err := s.World.AppendTimelineEvents([]domain.TimelineEvent{
		{Chapter: 2, Time: "午夜", Event: bigASCII, Characters: []string{"林岚"}},
	}); err != nil {
		t.Fatalf("AppendTimelineEvents: %v", err)
	}

	const budget = 1000
	restore, ok, err := buildWriterRestoreText(s, budget)
	if err != nil {
		t.Fatalf("buildWriterRestoreText: %v", err)
	}
	if !ok {
		t.Fatal("expected restore text under final budget check")
	}
	if got := corecontext.EstimateTokens(agentcore.UserMsg(restore)); got > budget {
		t.Fatalf("restore text exceeds budget: %d > %d", got, budget)
	}
	pack := &WriterRestorePack{}
	pack.text = restore
	msg, ok2 := pack.buildMessage(budget)
	if !ok2 {
		t.Fatal("buildMessage should accept budget-fitting restore")
	}
	if !strings.Contains(msg.TextContent(), "当前章节计划") {
		t.Fatalf("expected chapter plan section, got %q", msg.TextContent())
	}
}
