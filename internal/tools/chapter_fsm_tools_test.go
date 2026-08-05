package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── 工具入口强制（规格第 14.2 节） ────────────────────────────────────
// 六工具在 Execute 入口调用 RequireChapterAction；非法动作被拒且无任何
// 文件/checkpoint/ledger/模型调用副作用。

// fsmEnabledStore 构造启用 FSM 的测试 store：critic 模式 + permissive 用户规则
// + 机械干净的草稿（mechCleanDraft 包装避免与 C2 机械闸语义纠缠）。
func fsmEnabledStore(t *testing.T, chapter int, draft string) *store.Store {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	savePermissiveUserRules(t, st)
	draft = mechCleanDraft(draft)
	if draft != "" {
		if err := st.Drafts.SaveDraft(chapter, draft); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// fsmEnabledCfg 是测试用的生产等价 FSM 配置（与 BuildWorkers 注入一致）。
func fsmEnabledCfg() ChapterFSMConfig {
	return ChapterFSMConfig{Enabled: true, PipelineEnabled: true, ExpectedPolisherModel: "mock-polisher-model"}
}

// fsmTransitionErr 断言 err 是 chapter_fsm 拦截错误：包含 code/chapter/stage/
// attempted/required 且 unwrap 到 ErrToolPrecondition。
func fsmTransitionErr(t *testing.T, err error, wantStage, wantAttempted, wantRequired string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected FSM rejection (stage=%s attempted=%s), got nil", wantStage, wantAttempted)
	}
	msg := err.Error()
	for _, want := range []string{
		"code=chapter_fsm_transition_denied",
		"stage=" + wantStage,
		"attempted=" + wantAttempted,
		"required=" + wantRequired,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Errorf("FSM rejection must unwrap to ErrToolPrecondition, got %v", err)
	}
}

func commitArgs(chapter int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"chapter": chapter, "summary": "测试提交", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	return raw
}

// TestChapterFSM_CommitRejectedInDraftDirty 场景 1：commit 在 draft_dirty 被拒，
// 错误含 code/chapter/stage/attempted/required，且不创建 pending commit、不写 final。
func TestChapterFSM_CommitRejectedInDraftDirty(t *testing.T) {
	st := fsmEnabledStore(t, 1, "草稿内容。她心里骂自己丢人，真不要脸。")
	tool := NewCommitChapterTool(st)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	_, err := tool.Execute(t.Context(), commitArgs(1))
	fsmTransitionErr(t, err, "draft_dirty", "commit_chapter", "check_consistency")
	if !strings.Contains(err.Error(), "chapter=1") {
		t.Errorf("error %q missing chapter=1", err.Error())
	}

	if pending, _ := st.Signals.LoadPendingCommit(); pending != nil {
		t.Error("pending commit must not be created on FSM rejection")
	}
	if final, _ := st.Drafts.LoadChapterText(1); final != "" {
		t.Error("final chapter must not be written on FSM rejection")
	}
}

// TestChapterFSM_PolishRejectedBeforeFirstCheck 场景 2：polish 在首次 check 前被拒，
// mock polisher 调用次数必须为 0（不消耗模型调用）。
func TestChapterFSM_PolishRejectedBeforeFirstCheck(t *testing.T) {
	st := fsmEnabledStore(t, 1, "草稿内容。她心里骂自己丢人，真不要脸。")
	calls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: polisherText("精修后的草稿内容。她心里骂自己丢人，真不要脸。")}, nil
	})
	tool := newEnabledPolishTool(st, polisher)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	fsmTransitionErr(t, err, "draft_dirty", "polish_draft", "check_consistency")
	if calls != 0 {
		t.Errorf("polisher must not be called on FSM rejection, got %d calls", calls)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish"); cp != nil {
		t.Error("polish checkpoint must not be written on FSM rejection")
	}
}

// TestChapterFSM_ReviewRejectedBeforePostPolishCheck 场景 3：review 在 post-polish
// check 前被拒（consistency seq 未晚于 polish seq），mock critic 调用次数必须为 0。
func TestChapterFSM_ReviewRejectedBeforePostPolishCheck(t *testing.T) {
	st := fsmEnabledStore(t, 1, "草稿内容。她心里骂自己丢人，真不要脸。")
	digest := domain.DigestDraft(mustLoadDraft(t, st, 1))
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", digest); err != nil {
		t.Fatal(err)
	}
	// polish 在 consistency 之后追加（seq 更大）→ 缺少 post-polish check
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "p1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mock-polisher-model", Stage: "draft", Changed: true},
	); err != nil {
		t.Fatal(err)
	}

	calls := 0
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		calls++
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	fsmTransitionErr(t, err, "needs_post_polish_check", "review_style", "check_consistency")
	if calls != 0 {
		t.Errorf("critic must not be called on FSM rejection, got %d calls", calls)
	}
	if ledger, _ := st.StyleReview.Load(1); ledger != nil && !ledger.IsEmpty() {
		t.Error("ledger must not be mutated on FSM rejection")
	}
}

// TestChapterFSM_CommitRejectedBeforeReview 场景 4：commit 在 review 前被拒
// （post-polish check 完成、critic 模式、无账本 → needs_review），
// 不创建 pending commit、不写 final。
func TestChapterFSM_CommitRejectedBeforeReview(t *testing.T) {
	st := fsmEnabledStore(t, 1, "草稿内容。她心里骂自己丢人，真不要脸。")
	digest := domain.DigestDraft(mustLoadDraft(t, st, 1))
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "p1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mock-polisher-model", Stage: "draft", Changed: true},
	); err != nil {
		t.Fatal(err)
	}
	// post-polish check：consistency seq 晚于 polish seq
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "c2", digest); err != nil {
		t.Fatal(err)
	}

	tool := NewCommitChapterTool(st)
	tool.SetChapterFSMConfig(fsmEnabledCfg())
	_, err := tool.Execute(t.Context(), commitArgs(1))
	fsmTransitionErr(t, err, "needs_review", "commit_chapter", "review_style")

	if pending, _ := st.Signals.LoadPendingCommit(); pending != nil {
		t.Error("pending commit must not be created on FSM rejection")
	}
	if final, _ := st.Drafts.LoadChapterText(1); final != "" {
		t.Error("final chapter must not be written on FSM rejection")
	}
}

// TestChapterFSM_TerminalCurrentLocksBody 场景 5：terminal 当前候选（accepted_initial
// 绑定当前 digest + 绑定最新 polish seq）→ draft/edit/polish 被拒，commit 允许。
func TestChapterFSM_TerminalCurrentLocksBody(t *testing.T) {
	st := fsmEnabledStore(t, 1, "终审通过的草稿。她心里骂自己丢人，真不要脸。")
	digest := domain.DigestDraft(mustLoadDraft(t, st, 1))
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", digest); err != nil {
		t.Fatal(err)
	}
	polishCP, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "p1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mock-polisher-model", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendAlways(domain.ChapterScope(1), "consistency_check", "c2", digest); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	basis := ComputeBasisDigest(st, 1, "v1")
	if err := st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "v1", Model: "m", PolishCheckpointSeq: polishCP.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "v1", Model: "m", PolishCheckpointSeq: polishCP.Seq},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// draft_chapter 被拒（stage=needs_commit）
	draftTool := NewDraftChapterTool(st, testContract)
	draftTool.SetChapterFSMConfig(fsmEnabledCfg())
	_, err = draftTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"越权重写","mode":"write"}`))
	fsmTransitionErr(t, err, "needs_commit", "draft_chapter", "commit_chapter")

	// edit_chapter 被拒
	editTool := NewEditChapterTool(st)
	editTool.SetChapterFSMConfig(fsmEnabledCfg())
	_, err = editTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"old_string":"终审通过的草稿","new_string":"越权修改"}`))
	fsmTransitionErr(t, err, "needs_commit", "edit_chapter", "commit_chapter")

	// polish_draft 被拒，polisher 不调用
	polishCalls := 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		polishCalls++
		return &agentcore.LLMResponse{Message: polisherText("x")}, nil
	})
	polishTool := newEnabledPolishTool(st, polisher)
	polishTool.SetChapterFSMConfig(fsmEnabledCfg())
	_, err = polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	fsmTransitionErr(t, err, "needs_commit", "polish_draft", "commit_chapter")
	if polishCalls != 0 {
		t.Errorf("polisher must not be called on terminal lock, got %d calls", polishCalls)
	}

	// commit 允许（FSM 放行 + 现有 commit gates 也通过）
	commitTool := NewCommitChapterTool(st)
	commitTool.SetChapterFSMConfig(fsmEnabledCfg())
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mock-polisher-model"})
	if _, err := commitTool.Execute(t.Context(), commitArgs(1)); err != nil {
		t.Fatalf("commit must pass for terminal current candidate: %v", err)
	}
	final, _ := st.Drafts.LoadChapterText(1)
	if final == "" {
		t.Fatal("final chapter must be written after allowed commit")
	}
}

// TestChapterFSM_ReadToolsNotGuarded 场景 6：读取类工具不接入 guard，正常执行。
func TestChapterFSM_ReadToolsNotGuarded(t *testing.T) {
	st := fsmEnabledStore(t, 1, "草稿内容。她心里骂自己丢人，真不要脸。")
	readTool := NewReadChapterTool(st)
	out, err := readTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"source":"draft"}`))
	if err != nil {
		t.Fatalf("read_chapter must not be guarded by FSM: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("read_chapter returned empty result")
	}
}

// TestChapterFSM_DraftAppendAllowedRepeatedlyInDraftDirty 场景 7：draft(mode=append)
// 在 draft_dirty 允许多次追加（分批写作语义）。
func TestChapterFSM_DraftAppendAllowedRepeatedlyInDraftDirty(t *testing.T) {
	st := fsmEnabledStore(t, 1, "")
	tool := NewDraftChapterTool(st, testContract)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	// 首次 write 创建草稿
	if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"第一段。","mode":"write"}`)); err != nil {
		t.Fatalf("initial draft write must pass: %v", err)
	}
	// draft_dirty 下多次 append 均允许
	for i, chunk := range []string{"第二段。", "第三段。", "第四段。"} {
		if _, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"`+chunk+`","mode":"append"}`)); err != nil {
			t.Fatalf("append #%d must pass in draft_dirty: %v", i+1, err)
		}
	}
	draft := mustLoadDraft(t, st, 1)
	for _, want := range []string{"第一段。", "第二段。", "第三段。", "第四段。"} {
		if !strings.Contains(draft, want) {
			t.Errorf("draft %q missing appended chunk %q", draft, want)
		}
	}
}

// TestChapterFSM_AfterCleanCheckDraftEditRejected 场景 8：clean check 后进入
// needs_polish，再次 draft/edit 被拒（先精修，不允许跳过 polish）。
func TestChapterFSM_AfterCleanCheckDraftEditRejected(t *testing.T) {
	st := fsmEnabledStore(t, 1, "干净的草稿。她心里骂自己丢人，真不要脸。")
	checkTool := NewCheckConsistencyTool(st)
	checkTool.SetChapterFSMConfig(fsmEnabledCfg())

	out, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("first check_consistency must pass in draft_dirty: %v", err)
	}
	// 追加 checkpoint 后阶段为 needs_polish → required_next_action=polish_draft
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	action, _ := result["required_next_action"].(map[string]any)
	if action == nil || action["action"] != ActionPolishDraft {
		t.Fatalf("required_next_action = %v, want %s", action, ActionPolishDraft)
	}

	// needs_polish：draft/edit 被拒
	draftTool := NewDraftChapterTool(st, testContract)
	draftTool.SetChapterFSMConfig(fsmEnabledCfg())
	_, err = draftTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"越权重写","mode":"write"}`))
	fsmTransitionErr(t, err, "needs_polish", "draft_chapter", "polish_draft")

	editTool := NewEditChapterTool(st)
	editTool.SetChapterFSMConfig(fsmEnabledCfg())
	_, err = editTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"old_string":"干净的草稿","new_string":"越权修改"}`))
	fsmTransitionErr(t, err, "needs_polish", "edit_chapter", "polish_draft")

	// 重复 check 也被拒（clean check 后不允许重复 check）
	_, err = checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	fsmTransitionErr(t, err, "needs_polish", "check_consistency", "polish_draft")
}

func mustLoadDraft(t *testing.T, st *store.Store, chapter int) string {
	t.Helper()
	d, err := st.Drafts.LoadDraft(chapter)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestChapterFSM_CommitCrashRecoveryClearsPendingCommit 场景 9：崩溃恢复窄路径。
// 崩溃发生在 MarkChapterComplete 之后、ClearPendingCommit 之前：本章已完成且
// 残留匹配 PendingCommit，FSM 会判 complete 拒绝一切动作——重试 commit 必须走
// 既有恢复（追加 commit checkpoint + 清除残留信号 + skip 结果），而不是被 FSM
// 永久卡死（发布阻断 1）。
func TestChapterFSM_CommitCrashRecoveryClearsPendingCommit(t *testing.T) {
	st := fsmEnabledStore(t, 1, "已提交的正文。她心里骂自己丢人，真不要脸。")
	final := mustLoadDraft(t, st, 1)
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, len([]rune(final)), "mystery", "quest"); err != nil {
		t.Fatal(err)
	}
	if err := st.Signals.SavePendingCommit(domain.PendingCommit{
		Chapter: 1, Stage: domain.CommitStageProgressMarked, Summary: "半提交摘要",
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewCommitChapterTool(st)
	tool.SetChapterFSMConfig(fsmEnabledCfg())

	out, err := tool.Execute(t.Context(), commitArgs(1))
	if err != nil {
		t.Fatalf("崩溃恢复重试不得被 FSM 拒绝: %v", err)
	}
	if pending, _ := st.Signals.LoadPendingCommit(); pending != nil {
		t.Fatalf("重试后残留 PendingCommit 应被清除，got %+v", pending)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "commit"); cp == nil {
		t.Fatal("恢复路径应补齐 commit checkpoint")
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result["committed"] != true {
		t.Fatalf("恢复结果应含 committed=true（skip 语义），got %v", result)
	}

	// 负向控制：已完成章节但无残留 PendingCommit → FSM 仍拒绝（窄路径不越权）。
	if _, err := tool.Execute(t.Context(), commitArgs(1)); err == nil {
		t.Fatal("已完成且无残留 PendingCommit 的 commit 仍应被 FSM 拒绝")
	}
}
