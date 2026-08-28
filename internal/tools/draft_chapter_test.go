package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestDraftChapterRejectsUnfinishedPendingRewrite(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 80); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	for ch := 1; ch <= 58; ch++ {
		if err := s.Progress.MarkChapterComplete(ch, 3000, "", ""); err != nil {
			t.Fatalf("MarkChapterComplete(%d): %v", ch, err)
		}
	}

	p, _ := s.Progress.Load()
	p.Flow = domain.FlowPolishing
	p.PendingRewrites = []int{65}
	if err := s.Progress.Save(p); err != nil {
		t.Fatalf("Save corrupt progress: %v", err)
	}

	tool := NewDraftChapterTool(s, testContract)
	args, err := json.Marshal(map[string]any{
		"chapter": 65,
		"content": "错误写入未来章节。",
		"mode":    "write",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "pending_rewrites 只能包含已完成章节") {
		t.Fatalf("expected invalid pending_rewrites rejection, got %v", err)
	}
	progress, _ := s.Progress.Load()
	if progress.InProgressChapter == 65 {
		t.Fatalf("future chapter should not become in progress")
	}
}

func TestDraftChapterRejectsUnexpandedLayeredChapter(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 5); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := s.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
		Index: 1,
		Title: "第一卷",
		Arcs: []domain.ArcOutline{{
			Index: 1,
			Title: "第一弧",
			Chapters: []domain.OutlineEntry{
				{Chapter: 1, Title: "一"},
				{Chapter: 2, Title: "二"},
			},
		}, {
			Index:             2,
			Title:             "第二弧",
			EstimatedChapters: 3,
		}},
	}}); err != nil {
		t.Fatalf("SaveLayeredOutline: %v", err)
	}
	if err := s.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}
	if err := s.Progress.SetLayered(true); err != nil {
		t.Fatalf("SetLayered: %v", err)
	}

	tool := NewDraftChapterTool(s, testContract)
	args, err := json.Marshal(map[string]any{
		"chapter": 3,
		"content": "越界正文。",
		"mode":    "write",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "expand_arc") {
		t.Fatalf("expected unexpanded chapter rejection, got %v", err)
	}
	progress, _ := s.Progress.Load()
	if progress.InProgressChapter == 3 {
		t.Fatalf("unexpanded chapter should not become in progress")
	}
}

// TestDraftChapterRejectsInvalidMode 数据安全回归：mode 只接受 write/append。
// 未知值（如 "overwrite"）或空串必须被 ErrToolArgs 拒绝，且不得写草稿、
// 不得把章节标记为进行中——绝不允许静默降级为 write 覆盖已有草稿。
func TestDraftChapterRejectsInvalidMode(t *testing.T) {
	for _, mode := range []string{"overwrite", "", "Write"} {
		t.Run("mode="+mode, func(t *testing.T) {
			s := store.NewStore(t.TempDir())
			if err := s.Init(); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := s.Progress.Init("test", 10); err != nil {
				t.Fatalf("Progress.Init: %v", err)
			}

			tool := NewDraftChapterTool(s, testContract)
			args, err := json.Marshal(map[string]any{
				"chapter": 1,
				"content": "非法 mode 的正文。",
				"mode":    mode,
			})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			if _, err := tool.Execute(context.Background(), args); err == nil || !errors.Is(err, errs.ErrToolArgs) {
				t.Fatalf("expected ErrToolArgs rejection, got %v", err)
			}

			// 不写草稿（数据安全核心断言）
			if content, _ := s.Drafts.LoadDraft(1); content != "" {
				t.Fatalf("draft must not be written for invalid mode, got %q", content)
			}
			// 不标记进行中（非法参数不得产生任何状态副作用）
			progress, _ := s.Progress.Load()
			if progress != nil && progress.InProgressChapter != 0 {
				t.Fatalf("chapter must not become in progress, got %d", progress.InProgressChapter)
			}
		})
	}
}
func TestDraftChapterUnderMinWriteRequiresAppend(t *testing.T) {
	st := fsmEnabledStore(t, 1, "")
	snap := rules.BuildSnapshot([]rules.Candidate{
		rules.SystemDefaults(),
		{
			Source: "test",
			Structured: rules.Structured{
				ChapterWords: &rules.WordRange{Min: 3000, Max: 6000},
			},
		},
	})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}

	tool := NewDraftChapterTool(st, testContract)
	tool.SetChapterFSMConfig(fsmEnabledCfg())
	short := "# 第一章\n他走到窗前，心里骂自己丢人，真不要脸。"
	writeArgs := func(content, mode string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{
			"chapter": 1, "content": content, "mode": mode,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, err := tool.Execute(context.Background(), writeArgs(short, "write")); err != nil {
		t.Fatalf("initial write must pass: %v", err)
	}

	_, err := tool.Execute(context.Background(), writeArgs("整章重写版本。", "write"))
	if err == nil || !errors.Is(err, errs.ErrToolPrecondition) || !strings.Contains(err.Error(), "mode=append") {
		t.Fatalf("under-min overwrite should require append, got %v", err)
	}
	got, loadErr := st.Drafts.LoadDraft(1)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got != short {
		t.Fatalf("guard must not overwrite draft: got %q, want %q", got, short)
	}

	if _, err := tool.Execute(context.Background(), writeArgs("续写一段。", "append")); err != nil {
		t.Fatalf("append should pass after guard: %v", err)
	}
}

// ── 重写队列守卫豁免收紧（850 循环修复） ──────────────────────────────
// 旧行为：hasActiveReviewOrRewrite 对重写队列章节无条件豁免"达标草稿防倒退"
// 守卫 → 模型反复 draft_chapter(mode=write) 输出固定短内容，把已达标草稿
// 打回不达标（850 循环）。修复后：重写队列仅当现有草稿未达标时豁免
// （重写刚开始，允许整章覆盖）；现有草稿已达标时守卫生效（防倒退）。

// saveWordRangeRules 保存指定章节字数下限的用户规则快照。
func saveWordRangeRules(t *testing.T, st *store.Store, min, max int) {
	t.Helper()
	snap := rules.BuildSnapshot([]rules.Candidate{
		rules.SystemDefaults(),
		{Source: "test", Structured: rules.Structured{ChapterWords: &rules.WordRange{Min: min, Max: max}}},
	})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}
}

// rewriteQueueDraftStore 构造 critic 模式 + 已完成 + 重写队列 + 草稿 != 终稿
// 的基础 store（重写已开始，非 rewrite_not_started）。
func rewriteQueueDraftStore(t *testing.T, draft string) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	saveWordRangeRules(t, st, 3000, 6000)
	final := "原始终稿内容。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "重写测试"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	return st
}

// qualifiedDraft 构造 >= 下限（3000）且机械干净的草稿（含自评口吻关键词）。
func qualifiedDraft() string {
	return strings.Repeat("她站在窗前，望着远处的灯火。", 250) + "她心里骂自己丢人，真不要脸。"
}

// TestDraftChapter_RewriteQueueQualifiedDraftShortWriteRejected 850 场景回归：
// 重写队列 + 现有草稿已达标 + 本次 write 内容不达标 → 拒绝覆盖
// （ErrToolPrecondition），草稿不被覆盖。
func TestDraftChapter_RewriteQueueQualifiedDraftShortWriteRejected(t *testing.T) {
	st := rewriteQueueDraftStore(t, qualifiedDraft())
	tool := NewDraftChapterTool(st, testContract)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"chapter":1,"content":"整章重写但字数不足的版本。","mode":"write"}`))
	if err == nil || !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("rewrite-queue qualified draft must reject short write, got %v", err)
	}
	if !strings.Contains(err.Error(), "已达标") {
		t.Fatalf("rejection must mention qualified draft, got: %v", err)
	}
	// 草稿未被覆盖
	got, _ := st.Drafts.LoadDraft(1)
	if got != qualifiedDraft() {
		t.Fatalf("draft must stay untouched after rejection, got %d runes", len([]rune(got)))
	}
}

// TestDraftChapter_RewriteQueueUnderMinDraftWriteAllowed 重写刚开始豁免：
// 重写队列 + 现有草稿未达标 → write 放行（整章覆盖合法，不得按字数误伤）。
// 草稿同时带非字数机械 error（自评口吻不足），确保不被第一道 under-min
// 守卫（onlyUnderMinChapterWordsError）拦截，走到"达标防倒退"守卫验证豁免。
func TestDraftChapter_RewriteQueueUnderMinDraftWriteAllowed(t *testing.T) {
	st := rewriteQueueDraftStore(t, "重写刚开始的短草稿。")
	tool := NewDraftChapterTool(st, testContract)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"chapter":1,"content":"整章重写版本。","mode":"write"}`))
	if err != nil {
		t.Fatalf("rewrite-queue under-min draft must allow write, got %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res["written"] != true {
		t.Fatalf("expected written=true, got %+v", res)
	}
	got, _ := st.Drafts.LoadDraft(1)
	if got != "整章重写版本。" {
		t.Fatalf("draft must be overwritten, got %q", got)
	}
}

// TestDraftChapter_ActiveReviewLedgerQualifiedDraftShortWriteAllowed 活跃评审
// 豁免：revision_open 账本 + 现有草稿已达标 + 短 write → 放行（评审修订需要
// 整章覆盖，不得按字数误伤）。
func TestDraftChapter_ActiveReviewLedgerQualifiedDraftShortWriteAllowed(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 10); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	saveWordRangeRules(t, st, 3000, 6000)
	draft := qualifiedDraft()
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st.StyleReview.Save(rewriteLedger(domain.ReviewStatusRevisionOpen, domain.DigestDraft(draft), 0)); err != nil {
		t.Fatalf("Save ledger: %v", err)
	}

	tool := NewDraftChapterTool(st, testContract)
	tool.SetChapterFSMConfig(fsmEnabledCfg())
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"chapter":1,"content":"评审修订的整章覆盖。","mode":"write"}`))
	if err != nil {
		t.Fatalf("active review ledger must exempt short write, got %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res["written"] != true {
		t.Fatalf("expected written=true, got %+v", res)
	}
}
