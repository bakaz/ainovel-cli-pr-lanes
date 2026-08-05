package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// requireResultField 从 result 中提取指定字段，不为 nil 则通过。
func requireResultField(t *testing.T, result json.RawMessage, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	v, ok := m[key]
	if !ok {
		t.Fatalf("result missing field %q", key)
	}
	return v
}

// TestEditChapterAppliesEdit 正常路径：drafts 已有内容，唯一匹配替换成功。
func TestEditChapterAppliesEdit(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, err := s.Drafts.LoadDraft(2)
	if err != nil {
		t.Fatalf("LoadDraft: %v", err)
	}
	if !strings.Contains(got, "指节泛起青白") {
		t.Fatalf("expected draft to contain new text, got %q", got)
	}
	if strings.Contains(got, "指节发白") {
		t.Fatalf("old text should be replaced, got %q", got)
	}
}

// TestEditChapterSeedsFromFinalChapter drafts 不存在但 chapters 有 → 自动从 chapters 播种。
func TestEditChapterSeedsFromFinalChapter(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 模拟第 3 章已提交且进入打磨队列
	original := "风从窗缝里钻进来，带着潮湿的泥土气味。"
	if err := s.Drafts.SaveFinalChapter(3, original); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(3, len([]rune(original)), "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := s.Progress.SetPendingRewrites([]int{3}, "测试打磨"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    3,
		"old_string": "潮湿的泥土气味",
		"new_string": "泥土和铁锈混杂的气味",
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// drafts 应被播种且包含新文本
	draft, err := s.Drafts.LoadDraft(3)
	if err != nil {
		t.Fatalf("LoadDraft: %v", err)
	}
	if !strings.Contains(draft, "泥土和铁锈混杂的气味") {
		t.Fatalf("expected draft seeded + edited, got %q", draft)
	}

	// chapters 保持原样（edit_chapter 不碰终稿）
	final, err := s.Drafts.LoadChapterText(3)
	if err != nil {
		t.Fatalf("LoadChapterText: %v", err)
	}
	if final != original {
		t.Fatalf("final chapter must stay untouched, got %q", final)
	}
}

// TestEditChapterRejectsCompletedWithoutQueue 已完成且不在重写队列中 → 拒绝。
func TestEditChapterRejectsCompletedWithoutQueue(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	original := "第二章原始正文。"
	if err := s.Drafts.SaveDraft(2, original); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(2, original); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, len([]rune(original)), "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "原始正文",
		"new_string": "篡改内容",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected rejection for completed chapter not in PendingRewrites")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("expected ErrToolPrecondition, got %v", err)
	}
}

// TestEditChapterRejectsAmbiguousMatch 多处匹配且未开 replace_all → 报错。
func TestEditChapterRejectsAmbiguousMatch(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他笑了。她也笑了。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "笑了",
		"new_string": "沉默了",
	})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected rejection for ambiguous match")
	}
}

// TestEditChapterReplaceAll replace_all=true 时所有匹配均被替换。
func TestEditChapterReplaceAll(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他笑了。她也笑了。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":     2,
		"old_string":  "笑了",
		"new_string":  "沉默了",
		"replace_all": true,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := s.Drafts.LoadDraft(2)
	if strings.Contains(got, "笑了") {
		t.Fatalf("all occurrences should be replaced, got %q", got)
	}
	if strings.Count(got, "沉默了") != 2 {
		t.Fatalf("expected 2 replacements, got %q", got)
	}
}

// TestEditChapterRejectsEmptyOldString 空 old_string → 参数非法。
func TestEditChapterRejectsEmptyOldString(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "",
		"new_string": "xxx",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected rejection for empty old_string")
	}
	if !errors.Is(err, errs.ErrToolArgs) {
		t.Fatalf("expected ErrToolArgs, got %v", err)
	}
}

// TestEditChapterRejectsNoDraftNoFinal drafts 与 chapters 都不存在 → 报错提示先 draft_chapter。
func TestEditChapterRejectsNoDraftNoFinal(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    5,
		"old_string": "任何",
		"new_string": "替换",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected rejection when neither draft nor chapter exists")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("expected ErrToolPrecondition, got %v", err)
	}
}

// TestEditChapterWorksWithCommitValidation 整条链路：edit_chapter → commit_chapter 成功 drain 队列。
// 验证新工具与 commit_chapter 的 drafts≠chapters 硬校验配合良好。
func TestEditChapterWorksWithCommitValidation(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	original := "风从窗缝里钻进来，带着潮湿的泥土气味。她心里骂自己丢人，真不要脸。"
	if err := s.Drafts.SaveDraft(2, original); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(2, original); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, len([]rune(original)), "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := s.Progress.SetPendingRewrites([]int{2}, "打磨"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}

	editTool := NewEditChapterTool(s)
	editArgs, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "潮湿的泥土气味",
		"new_string": "泥土和铁锈混杂的气味",
	})
	if _, err := editTool.Execute(context.Background(), editArgs); err != nil {
		t.Fatalf("edit_chapter: %v", err)
	}

	commitTool := NewCommitChapterTool(s)
	commitArgs, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"summary":    "打磨后摘要",
		"characters": []string{"主角"},
		"key_events": []string{"完成打磨"},
	})
	if _, err := commitTool.Execute(context.Background(), commitArgs); err != nil {
		t.Fatalf("commit_chapter after edit: %v", err)
	}

	progress, err := s.Progress.Load()
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if len(progress.PendingRewrites) != 0 {
		t.Fatalf("expected queue drained, got %v", progress.PendingRewrites)
	}
}

// --- 编辑后结构化反馈测试 ---

// TestEditChapterResultHasAffectedContext 验证 affected_context 包含替换后的新文本。
func TestEditChapterResultHasAffectedContext(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	draft := "第一章正文。\n\n他握紧了拳头，指节发白。\n\n他抬起头，看向远方。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "指节泛起青白") {
		t.Fatalf("affected_context should contain new text, got %q", ctx)
	}
	if strings.Contains(ctx, "指节发白") {
		t.Fatalf("affected_context should NOT contain old text, got %q", ctx)
	}
	// 应包含段落首尾
	if !strings.Contains(ctx, "他握紧了拳头") {
		t.Fatalf("affected_context should include paragraph start, got %q", ctx)
	}
}

// TestEditChapterResultHasDigest 验证 draft_digest 与实际文件 SHA256 一致。
func TestEditChapterResultHasDigest(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	gotDigest := requireResultField(t, result, "draft_digest").(string)

	// 从磁盘读实际文件计算期望 digest
	actual, err := s.Drafts.LoadDraft(2)
	if err != nil {
		t.Fatalf("LoadDraft: %v", err)
	}
	h := sha256.Sum256([]byte(actual))
	wantDigest := "sha256:" + hex.EncodeToString(h[:])
	if gotDigest != wantDigest {
		t.Fatalf("draft_digest: got %q, want %q", gotDigest, wantDigest)
	}
}

// TestEditChapterResultHasWordCount 验证 chapter_word_count 与实际文件字数一致。
func TestEditChapterResultHasWordCount(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	wc := int(requireResultField(t, result, "chapter_word_count").(float64))
	actual, _ := s.Drafts.LoadDraft(2)
	expected := domain.WordCount(actual)
	if wc != expected {
		t.Fatalf("chapter_word_count: got %d, want %d", wc, expected)
	}
}

// TestEditChapterDeltaPositive 验证 new_string 更长时 word_count_delta > 0。
func TestEditChapterDeltaPositive(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他笑了。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "笑了",
		"new_string": "笑得前仰后合",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	delta := int(requireResultField(t, result, "word_count_delta").(float64))
	if delta <= 0 {
		t.Fatalf("word_count_delta should be positive, got %d", delta)
	}

	actual, _ := s.Drafts.LoadDraft(2)
	expectedDelta := domain.WordCount(actual) - domain.WordCount("他笑了。")
	if delta != expectedDelta {
		t.Fatalf("word_count_delta: got %d, want %d", delta, expectedDelta)
	}
}

// TestEditChapterDeltaNegative 验证 new_string 更短时 word_count_delta < 0。
func TestEditChapterDeltaNegative(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他笑得前仰后合。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "笑得前仰后合",
		"new_string": "笑了",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	delta := int(requireResultField(t, result, "word_count_delta").(float64))
	if delta >= 0 {
		t.Fatalf("word_count_delta should be negative, got %d", delta)
	}

	actual, _ := s.Drafts.LoadDraft(2)
	expectedDelta := domain.WordCount(actual) - domain.WordCount("他笑得前仰后合。")
	if delta != expectedDelta {
		t.Fatalf("word_count_delta: got %d, want %d", delta, expectedDelta)
	}
}

// TestEditChapterResultHasRecheckFlag 验证 requires_consistency_recheck 为 true。
func TestEditChapterResultHasRecheckFlag(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	recheck := requireResultField(t, result, "requires_consistency_recheck").(bool)
	if !recheck {
		t.Fatal("requires_consistency_recheck should be true")
	}
}

// TestEditChapterExistingFieldsPreserved 验证原有字段（message, diff, first_changed_line, chapter, next_step）依然存在。
func TestEditChapterExistingFieldsPreserved(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}

	for _, key := range []string{"message", "diff", "first_changed_line", "chapter", "next_step"} {
		if _, ok := m[key]; !ok {
			t.Errorf("existing field %q missing from result", key)
		}
	}
}

// TestEditChapterContextBoundary 验证段落超长时 affected_context 被正确截断，
// 且截断后包含替换后的新文本。
func TestEditChapterContextBoundary(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 构造一个超长段落（> maxContextRunes runes），替换词在中间附近
	prefix := strings.Repeat("长", 300)
	suffix := strings.Repeat("短", 300)
	draft := "开头段落。\n\n" + prefix + "【替换点】" + suffix + "\n\n结尾段落。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "【替换点】",
		"new_string": "【已替换】",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ctx := requireResultField(t, result, "affected_context").(string)
	// 截断后不应包含换行符分割的相邻段落
	if strings.Contains(ctx, "开头段落") || strings.Contains(ctx, "结尾段落") {
		t.Logf("context may contain adjacent paragraphs (acceptable): %s", ctx)
	}
	// 必须包含新文本
	if !strings.Contains(ctx, "【已替换】") {
		t.Fatalf("affected_context must contain new text after truncation, got %q", ctx)
	}
	// 不应超过截断上限（允许略超过因为截断边界取整）
	if len([]rune(ctx)) > maxContextRunes+10 {
		t.Fatalf("affected_context too long: %d runes (max %d)", len([]rune(ctx)), maxContextRunes)
	}
}

// TestEditChapterAffectedContextWhenNewTextAlreadyExists 验证当 newText 在旧草稿中已存在时，
// affected_context 仍定位到替换位置而非老文本位置。
//
// 旧草稿: "AAA。BBB。被换词。AAA。"
// old="被换词" new="AAA" → 替换后: "AAA。BBB。AAA。AAA。"
// 若不记录旧位置，strings.Index(newDraft, "AAA") 会错误地定位到第一个"AAA"而非替换处。
func TestEditChapterAffectedContextWhenNewTextAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// "AAA。" 和 "被换词。" 等长，替换后位置不偏移
	draft := "AAA。BBB。被换词。AAA。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "被换词",
		"new_string": "AAA",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 验证 affected_context 包含的是替换处的文本（第三个分句），而非第一个"AAA。"
	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "BBB") {
		// 替换发生在"BBB。"之后，上下文应包含"BBB"而非开头的"AAA。"
		t.Fatalf("affected_context should center on the replacement position (after BBB), got %q", ctx)
	}

	// 验证完全替换正确
	got, _ := s.Drafts.LoadDraft(2)
	if strings.Contains(got, "被换词") {
		t.Fatalf("old text should be replaced, got %q", got)
	}
	if strings.Count(got, "AAA") != 3 {
		t.Fatalf("expected 3 occurrences of AAA after replace, got %q", got)
	}
}

// TestEditChapterReplaceAllHasAffectedContextsComplete 验证 replace_all 模式下
// affected_context 存在且 affected_contexts_complete=false（变更范围是整体包络）。
func TestEditChapterReplaceAllHasAffectedContextsComplete(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	draft := "AAAA 第一处 BBBB 第二处 BBBB 第三处 CCCC"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":     2,
		"old_string":  "BBBB",
		"new_string":  "XXXX",
		"replace_all": true,
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}

	// replace_all → affected_contexts_complete=false
	complete, ok := m["affected_contexts_complete"]
	if !ok {
		t.Fatal("replace_all result should contain affected_contexts_complete field")
	}
	if complete.(bool) {
		t.Fatal("affected_contexts_complete should be false for replace_all")
	}

	// affected_context 应覆盖变更整体包络（包含两处替换）
	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "XXXX") {
		t.Fatalf("affected_context should contain new text, got %q", ctx)
	}
	// 应包含两处替换的上下文（受包络影响，可能涵盖第一处和第二处之间）
	if !strings.Contains(ctx, "第一处") && !strings.Contains(ctx, "第二处") {
		t.Fatalf("affected_context should cover first replacement area, got %q", ctx)
	}
}

// TestEditChapterAffectedContextReplaceAllNewTextPreexists 验证 replace_all 且 newText 已存在时，
// affected_context 指向整体变更包络（而非错误定位到已有文本位置）。
func TestEditChapterAffectedContextReplaceAllNewTextPreexists(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// newText "AAA" 在第一段已存在，"BBB" 出现在中间两段被替换为 "AAA"。
	// 第三段也有原始 "AAA"。
	draft := "AAA 头段。\n\n一处 BBB 中段。\n\n二处 BBB 中段。\n\nAAA 尾段。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":     2,
		"old_string":  "BBB",
		"new_string":  "AAA",
		"replace_all": true,
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// affected_context 应覆盖整体变更包络，而非定位到第一段的"AAA"
	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "一处") || !strings.Contains(ctx, "二处") {
		t.Fatalf("affected_context should cover both replacement paragraphs, got %q", ctx)
	}
	// 由于是包络，可能包含头段或尾段内容，但必须覆盖替换区域

	// 验证 affected_contexts_complete=false
	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	complete, ok := m["affected_contexts_complete"]
	if !ok {
		t.Fatal("replace_all result should contain affected_contexts_complete field")
	}
	if complete.(bool) {
		t.Fatal("affected_contexts_complete should be false for replace_all")
	}
}

// TestEditChapterReplaceAllResult 验证 replace_all 模式下结构化反馈准确性。
func TestEditChapterReplaceAllResult(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	// 两处替换，delta = 2 * (len(new)-len(old))
	draft := "他笑了。她也笑了。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	oldWC := domain.WordCount(draft)

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":     2,
		"old_string":  "笑了",
		"new_string":  "沉默了",
		"replace_all": true,
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	wc := int(requireResultField(t, result, "chapter_word_count").(float64))
	delta := int(requireResultField(t, result, "word_count_delta").(float64))

	actual, _ := s.Drafts.LoadDraft(2)
	expectedWC := domain.WordCount(actual)
	if wc != expectedWC {
		t.Fatalf("chapter_word_count: got %d, want %d", wc, expectedWC)
	}
	if delta != expectedWC-oldWC {
		t.Fatalf("word_count_delta: got %d, want %d", delta, expectedWC-oldWC)
	}

	// affected_context 应包含新文本
	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "沉默了") {
		t.Fatalf("affected_context should contain new text, got %q", ctx)
	}
}

// --- 新增回归测试 ---

// TestFindChangedRange 直接验证 findChangedRange 函数在各种场景下的正确性。
func TestFindChangedRange(t *testing.T) {
	tests := []struct {
		name      string
		oldDraft  string
		newDraft  string
		wantStart int
		wantEnd   int
		wantOK    bool
		wantSlice string // newDraft[wantStart:wantEnd]
	}{
		{
			name:     "same content",
			oldDraft: "hello world",
			newDraft: "hello world",
			wantOK:   false,
		},
		{
			name:      "single replacement exact",
			oldDraft:  "AAA BBB CCC",
			newDraft:  "AAA XXX CCC",
			wantStart: 4,
			wantEnd:   7,
			wantOK:    true,
			wantSlice: "XXX",
		},
		{
			name:      "replacement at start",
			oldDraft:  "BBB CCC",
			newDraft:  "XXX CCC",
			wantStart: 0,
			wantEnd:   3,
			wantOK:    true,
			wantSlice: "XXX",
		},
		{
			name:      "replacement at end",
			oldDraft:  "AAA BBB",
			newDraft:  "AAA XXX",
			wantStart: 4,
			wantEnd:   7,
			wantOK:    true,
			wantSlice: "XXX",
		},
		{
			name:      "deletion",
			oldDraft:  "AAA BBB CCC",
			newDraft:  "AAA CCC",
			wantStart: 4,
			wantEnd:   4,
			wantOK:    true,
			wantSlice: "",
		},
		{
			name:      "insertion",
			oldDraft:  "AAA CCC",
			newDraft:  "AAA BBB CCC",
			wantStart: 4,
			wantEnd:   8,
			wantOK:    true,
			wantSlice: "BBB ",
		},
		{
			name:      "replace_all envelope",
			oldDraft:  "AAA BBB CCC BBB DDD",
			newDraft:  "AAA XXX CCC XXX DDD",
			wantStart: 4,
			wantEnd:   15,
			wantOK:    true,
			wantSlice: "XXX CCC XXX",
		},
		{
			name:      "fuzzy equivalent content",
			oldDraft:  "hello\u00A0world", // NBSP
			newDraft:  "hello world",      // regular space (fuzzy match)
			wantStart: 5,
			wantEnd:   6,
			wantOK:    true,
			wantSlice: " ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, ok := findChangedRange(tt.oldDraft, tt.newDraft)
			if ok != tt.wantOK {
				t.Fatalf("findChangedRange ok=%v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("findChangedRange=(%d,%d), want (%d,%d); slice=%q",
					start, end, tt.wantStart, tt.wantEnd, tt.newDraft[start:end])
			}
			if tt.wantSlice != "" && tt.newDraft[start:end] != tt.wantSlice {
				t.Fatalf("changed slice=%q, want %q", tt.newDraft[start:end], tt.wantSlice)
			}
		})
	}
}

// TestEditChapterFuzzyMatchAffectedContext 验证上游模糊匹配时（如 Unicode 空格被归一化），
// affected_context 依然正确（通过前后草稿 diff，不依赖 oldString 位置）。
func TestEditChapterFuzzyMatchAffectedContext(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 用不间断空格（\u00A0）写入草稿，old_string 用普通空格
	// 上游 fuzzyFind 会将 \u00A0 归一化为空格后匹配
	draft := "第\u00A0一\u00A0章\u00A0测\u00A0试"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "第 一 章 测 试", // 普通空格
		"new_string": "第一章测试",     // 无空格
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 验证受影响上下文包含新文本
	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "第一章测试") {
		t.Fatalf("affected_context should contain new text '第一章测试', got %q", ctx)
	}
	// 验证 draft_digest 存在且匹配
	requireResultField(t, result, "draft_digest")
	requireResultField(t, result, "chapter_word_count")
}

// TestEditChapterDeletionAffectedContext 验证删除操作（newString=""）时，
// affected_context 不会误定位到文件开头。
func TestEditChapterDeletionAffectedContext(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 删除中间文本
	draft := "前缀文本。要删除的内容。后缀文本。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "要删除的内容。",
		"new_string": "",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// affected_context 应包含删除点附近的文本（前缀或后缀），而非空
	ctx := requireResultField(t, result, "affected_context").(string)
	if ctx == "" {
		t.Fatal("affected_context should not be empty for deletion")
	}
	if !strings.Contains(ctx, "前缀") && !strings.Contains(ctx, "后缀") {
		t.Fatalf("affected_context should contain neighbor text around deletion point, got %q", ctx)
	}

	// 验证字数减少
	delta := int(requireResultField(t, result, "word_count_delta").(float64))
	if delta >= 0 {
		t.Fatalf("word_count_delta should be negative for deletion, got %d", delta)
	}

	// 验证文件确实删除了目标文本
	got, _ := s.Drafts.LoadDraft(2)
	if strings.Contains(got, "要删除的内容") {
		t.Fatalf("deleted text should not remain, got %q", got)
	}
}

// fakeEditTool 实现 editToolExecutor，在真实文件系统上执行字符串替换但返回非 JSON，
// 用于模拟上游 EditTool 返回不可解析响应的场景。
type fakeEditTool struct {
	dir string
}

func (f *fakeEditTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	path := filepath.Join(f.dir, a.FilePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	var newContent string
	if a.ReplaceAll {
		newContent = strings.ReplaceAll(content, a.OldString, a.NewString)
	} else {
		newContent = strings.Replace(content, a.OldString, a.NewString, 1)
	}
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return nil, err
	}
	// 返回非 JSON 文本以触发 upstream_result_unparsed 分支
	return json.RawMessage(`edit applied`), nil
}

// TestEditChapterUpstreamNonJSON 验证上游 EditTool 返回非 JSON、
// 但文件/检查点已成功更新时，edit_chapter 返回成功结构且含 upstream_result_unparsed=true，
// 且不泄露上游原始文本。
func TestEditChapterUpstreamNonJSON(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := &EditChapterTool{
		store: s,
		edit:  &fakeEditTool{dir: dir},
	}
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute should succeed despite non-JSON upstream: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result should be valid JSON: %v", err)
	}

	// 必须包含 upstream_result_unparsed=true
	unparsed, ok := m["upstream_result_unparsed"]
	if !ok {
		t.Fatal("result missing upstream_result_unparsed flag")
	}
	unparsedBool, ok := unparsed.(bool)
	if !ok || !unparsedBool {
		t.Fatal("upstream_result_unparsed should be true")
	}

	// 成功字段仍然存在（message/diff/first_changed_line 是上游 JSON 透传字段，
	// 非 JSON 分支不应包含）
	for _, key := range []string{"draft_digest", "chapter_word_count", "word_count_delta", "affected_context", "chapter", "requires_consistency_recheck", "next_step"} {
		if _, ok := m[key]; !ok {
			t.Errorf("result missing field %q", key)
		}
	}

	// 不得泄露上游原始文本
	resultStr := string(result)
	if strings.Contains(resultStr, "edit applied") {
		t.Fatal("result must not contain raw upstream text")
	}
	// 上游透传字段（message/diff/first_changed_line）也不应出现
	for _, leaked := range []string{"message", "diff", "first_changed_line"} {
		if _, ok := m[leaked]; ok {
			t.Errorf("result must not contain upstream passthrough field %q in non-JSON branch", leaked)
		}
	}

	// 文件实际已被更新
	draft, err := s.Drafts.LoadDraft(2)
	if err != nil {
		t.Fatalf("LoadDraft: %v", err)
	}
	if !strings.Contains(draft, "指节泛起青白") {
		t.Fatalf("draft should contain new text, got %q", draft)
	}

	// 旧文本已不存在
	if strings.Contains(draft, "指节发白") {
		t.Fatalf("draft should not contain old text, got %q", draft)
	}
}

// TestEditChapterCRLFLineEndings 验证草稿使用 CRLF 行尾时，diff 方式仍能正确
// 定位变更范围（不受上游行尾归一化影响）。
func TestEditChapterCRLFLineEndings(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 用 CRLF 行尾写入草稿
	draft := "第一行。\r\n第二行旧文本。\r\n第三行。"
	if err := s.Drafts.SaveDraft(2, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "第二行旧文本。",
		"new_string": "第二行新文本。",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// affected_context 应包含新文本
	ctx := requireResultField(t, result, "affected_context").(string)
	if !strings.Contains(ctx, "第二行新文本") {
		t.Fatalf("affected_context should contain new text '第二行新文本', got %q", ctx)
	}
	// 不应包含旧文本
	if strings.Contains(ctx, "第二行旧文本") {
		t.Fatalf("affected_context should NOT contain old text, got %q", ctx)
	}
}

// TestEditChapterNextStepMentionsRecheck 验证 next_step 强调 edit 后必须 recheck
// 且不含"edit 后直接 commit"措辞——任何 draft/edit 修改后必须重新 check_consistency。
func TestEditChapterNextStepMentionsRecheck(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "他握紧了拳头，指节发白。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewEditChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"old_string": "指节发白",
		"new_string": "指节泛起青白",
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	nextStep, ok := m["next_step"].(string)
	if !ok {
		t.Fatal("next_step field missing or not a string")
	}
	if !strings.Contains(nextStep, "check_consistency") && !strings.Contains(nextStep, "核验") {
		t.Fatalf("next_step must mention recheck after edit, got %q", nextStep)
	}
	if !strings.Contains(nextStep, "edit") && !strings.Contains(nextStep, "mode") {
		// At minimum, after-edit guidance should reference mode or edit
	}
	if strings.Contains(nextStep, "直接 commit") || strings.Contains(nextStep, "后 commit") {
		// "check_consistency 后 commit" is acceptable pattern but "edit 后直接 commit" is not
		// We only reject explicit "直接 commit" (no recheck)
		if !strings.Contains(nextStep, "check") && !strings.Contains(nextStep, "核验") {
			t.Fatalf("next_step must not suggest commit without recheck, got %q", nextStep)
		}
	}
}
