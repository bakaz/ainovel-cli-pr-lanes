package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// dirtyProse 是典型"散文腔"正文：否定修正句 ×3 + 升华收尾 ×2 + 物化句式 ×2，
// 足以同时触发 3 条硬闸规则。
const dirtyProse = "他不是怕死，而是怕疼。他不是退缩，而是等待。他不是沉默，而是蓄力。这便是宿命，便已足够。她把身体交给对方，把自己封进黑暗。"

const cleanProse = "他怕死，也怕疼，但他还是握住了剑。林墨把药丸收进袖中，退到窗边。她心里骂自己丢人，真不要脸。"

// TestLiteraryProseGateBlocksCommit 验证：文学腔句式超阈值 → commit 被 error 级
// 打回；终稿/摘要/进度均不落盘；命中片段写入 rule_violations 供审计。
func TestLiteraryProseGateBlocksCommit(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, dirtyProse); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    1,
		"summary":    "测试",
		"characters": []string{"林墨"},
		"key_events": []string{"事件"},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("文学腔超阈值 commit 应被拦截")
	}
	if !strings.Contains(err.Error(), "文学腔句式硬闸拦截") {
		t.Errorf("错误应说明硬闸拦截，got: %v", err)
	}

	// 终稿不落盘、进度不推进
	if _, statErr := os.Stat(dir + "/chapters/01.md"); !os.IsNotExist(statErr) {
		t.Fatalf("终稿不应落盘, stat err=%v", statErr)
	}
	progress, _ := s.Progress.Load()
	if len(progress.CompletedChapters) != 0 {
		t.Fatalf("CompletedChapters 应为空, got %v", progress.CompletedChapters)
	}

	// 命中句已写入 rule_violations 供审计
	violations := s.World.LoadRuleViolations(1)
	if len(violations) == 0 {
		t.Fatal("被拦下的命中句应写入 rule_violations")
	}
	foundDirty := false
	for _, v := range violations {
		if v.Rule != "literary_prose" || v.Severity != "error" {
			continue
		}
		if strings.Contains(v.Target, "不是怕死，而是") {
			foundDirty = true
		}
		if v.Limit == nil {
			t.Errorf("literary_prose 违例应带阈值 limit, got %+v", v)
		}
	}
	if !foundDirty {
		t.Errorf("rule_violations 应包含命中原文片段, got %+v", violations)
	}
}

// TestLiteraryProseGateAllowsCleanCommit 验证：干净正文不受影响，commit 正常通过。
func TestLiteraryProseGateAllowsCleanCommit(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, cleanProse); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    1,
		"summary":    "测试",
		"characters": []string{"林墨"},
		"key_events": []string{"事件"},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("干净正文 commit 不应被拦: %v", err)
	}
	if _, statErr := os.Stat(dir + "/chapters/01.md"); statErr != nil {
		t.Fatalf("终稿应正常落盘: %v", statErr)
	}
}

// TestLiteraryProseGateSkipsDuplicateCommit 验证：已完成章节的重复提交（skip 结果）
// 不提交新正文，硬闸不拦——即使草稿是脏文本也不影响已提交的终稿。
func TestLiteraryProseGateSkipsDuplicateCommit(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, cleanProse); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "首次提交", "characters": []string{"林墨"}, "key_events": []string{"事件"},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("首次 commit: %v", err)
	}

	// 章节已完成：重复提交跳过硬闸（不提交新正文）
	if err := s.Drafts.SaveDraft(1, dirtyProse); err != nil {
		t.Fatalf("SaveDraft (dirty): %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("已完成章节的重复提交不应被硬闸拦截: %v", err)
	}
}

// TestLiteraryProseGateBlocksRewriteCommit 验证：重写/打磨提交同样算"新正文"，
// 脏文本会被硬闸拦下；修正后可通过。
func TestLiteraryProseGateBlocksRewriteCommit(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, cleanProse); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(2, cleanProse); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, len([]rune(cleanProse)), "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := s.Progress.SetPendingRewrites([]int{2}, "测试重写"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写", "characters": []string{"林墨"}, "key_events": []string{"重写"},
	})

	// 脏的重写文本 → 拦下
	if err := s.Drafts.SaveDraft(2, dirtyProse); err != nil {
		t.Fatalf("SaveDraft (dirty rewrite): %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("脏的重写提交应被硬闸拦截")
	}

	// 修正后的重写文本 → 通过
	if err := s.Drafts.SaveDraft(2, cleanProse+"重写后新增段落。"); err != nil {
		t.Fatalf("SaveDraft (clean rewrite): %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("干净的重写提交不应被拦: %v", err)
	}
}

// TestCheckConsistencyReportsLiteraryGateFacts 验证：check_consistency（draft/check
// 阶段）同样运行硬闸并返回 error 级事实，但不阻断工具本身。
func TestCheckConsistencyReportsLiteraryGateFacts(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Drafts.SaveDraft(1, dirtyProse); err != nil {
		t.Fatal(err)
	}
	out, err := NewCheckConsistencyTool(s).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check_consistency 不应被硬闸阻断: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	violations, ok := got["rule_violations"].([]any)
	if !ok {
		t.Fatalf("应返回 rule_violations, got %+v", got["rule_violations"])
	}
	found := false
	for _, item := range violations {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["rule"] == "literary_prose" && m["severity"] == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("check_consistency 应报告 literary_prose error 级事实, got %+v", violations)
	}
}
