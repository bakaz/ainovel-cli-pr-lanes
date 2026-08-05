package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── review_style 精修流水线新鲜度闸门 ──

// TestReviewStyle_PipelineBlocksWithoutFreshPolish 验证 pipeline 启用时，
// review_style 要求存在与当前草稿匹配的 polish checkpoint（精修先于评审）。
func TestReviewStyle_PipelineBlocksWithoutFreshPolish(t *testing.T) {
	st := setupCriticStore(t, 1, "# 一\nabc她心里骂自己丢人，真不要脸。")
	tool := NewReviewStyleTool(st, newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		panic("critic must not be called")
	}), testCriticVersion)
	tool.SetPipelineEnabled(true) // pipeline 开启，但无 polish checkpoint

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("review_style must be blocked when polish checkpoint is missing")
	}
	if !strings.Contains(err.Error(), "polish_draft") {
		t.Errorf("expected polish_draft hint, got: %v", err)
	}
}

// TestReviewStyle_PipelineAllowsWithFreshPolish 验证 pipeline 启用 + fresh polish
// checkpoint（且 consistency 检查点 seq 晚于 polish）→ 评审正常进行。
func TestReviewStyle_PipelineAllowsWithFreshPolish(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := setupCriticStore(t, 1, draft)
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", domain.DigestDraft(draft),
		domain.PolishCheckpointMeta{InputDigest: domain.DigestDraft(draft), PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	// 顺序绑定：polish 之后必须重新 check_consistency（每次执行都追加新 seq），
	// 使 consistency seq > polish seq（setupCriticStore 早先追加的 consistency CP
	// 在 polish 之前，seq 更小——用 AppendAlways 追加新的 consistency CP）。
	if _, err := st.Checkpoints.AppendAlways(
		domain.ChapterScope(1), "consistency_check", "a2", domain.DigestDraft(draft),
	); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	tool.SetPipelineEnabled(true)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review with fresh polish should proceed: %v", err)
	}
	var output StyleReviewOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass", output.Verdict)
	}
}

// TestReviewStyle_PipelineAllowsWithDegradedPolish 验证 degraded polish checkpoint
// （polisher 失败降级记录，Digest=当前草稿）同样满足 review_style 的 fresh polish
// 前置检查（step 5：degraded 后允许 review）——顺序绑定（consistency seq > polish
// seq）原样生效，评审正常进行。
func TestReviewStyle_PipelineAllowsWithDegradedPolish(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := setupCriticStore(t, 1, draft)
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "", Stage: "draft", Changed: false, Degraded: true, ErrorCategory: "stream_idle"},
	); err != nil {
		t.Fatal(err)
	}
	// degraded 后必须重新 check_consistency（consistency seq > polish seq）
	if _, err := st.Checkpoints.AppendAlways(
		domain.ChapterScope(1), "consistency_check", "a2", digest,
	); err != nil {
		t.Fatal(err)
	}

	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	tool.SetPipelineEnabled(true)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review after degraded polish should proceed: %v", err)
	}
	var output StyleReviewOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass", output.Verdict)
	}
}

// TestReviewStyle_PipelineOrderingRejectsCheckBeforePolish 验证顺序绑定：
// consistency checkpoint 的 seq 不晚于 polish checkpoint（如 polish 之后未重新
// check_consistency）→ 评审被拒，引导先 check_consistency。
func TestReviewStyle_PipelineOrderingRejectsCheckBeforePolish(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := setupCriticStore(t, 1, draft)
	// polish 在 consistency 之后追加（seq 更大）→ 顺序违反
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", domain.DigestDraft(draft),
		domain.PolishCheckpointMeta{InputDigest: domain.DigestDraft(draft), PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	tool := NewReviewStyleTool(st, newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		panic("critic must not be called")
	}), testCriticVersion)
	tool.SetPipelineEnabled(true)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("review must be rejected when consistency seq <= polish seq")
	}
	if !strings.Contains(err.Error(), "check_consistency") {
		t.Errorf("expected check_consistency hint, got: %v", err)
	}
}

// TestReviewStyle_PipelineNoOpPolishRecheckProducesNewSeq 回归：polish no-op
// （输出与输入 digest 相同）后重新 check_consistency 必须产生新 seq 的 consistency
// checkpoint（AppendAlways 不做 digest 去重），review_style 能基于新顺序发起。
func TestReviewStyle_PipelineNoOpPolishRecheckProducesNewSeq(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := setupCriticStore(t, 1, draft)
	digest := domain.DigestDraft(draft)

	// 第一次 polish（no-op：输出与输入相同 → 同一 digest）
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	// 第二次 polish（no-op，同 digest）——AppendPolish 不再去重，seq 必须递增
	cp2, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cp2.Seq <= st.Checkpoints.LatestByStep(domain.ChapterScope(1), "consistency_check").Seq {
		t.Fatal("no-op polish 后 consistency seq 必须小于最新 polish seq（否则顺序绑定失效）")
	}
	// 重新 check_consistency：AppendAlways 产生新 seq（即使 digest 相同）
	cc, err := st.Checkpoints.AppendAlways(
		domain.ChapterScope(1), "consistency_check", "a3", digest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cc.Seq <= cp2.Seq {
		t.Fatalf("no-op polish 后重新 check_consistency 必须产生新 seq：cc=%d polish=%d", cc.Seq, cp2.Seq)
	}
	// 基于新顺序 review_style 可正常发起（顺序 polish → consistency → critic）
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	tool := NewReviewStyleTool(st, critic, testCriticVersion)
	tool.SetPipelineEnabled(true)
	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review after no-op polish + re-check should proceed: %v", err)
	}
	var output StyleReviewOutput
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass", output.Verdict)
	}
}

// TestReviewStyle_PipelineStalePolishBlocks 验证 pipeline 启用 + polish 后草稿又被改
// （stale checkpoint）→ 评审被拦截，要求重新 polish_draft（防"终态账本锁死"死结）。
func TestReviewStyle_PipelineStalePolishBlocks(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := setupCriticStore(t, 1, draft)
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", domain.DigestDraft(draft),
		domain.PolishCheckpointMeta{InputDigest: domain.DigestDraft(draft), PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
	); err != nil {
		t.Fatal(err)
	}
	// 精修后正文又被改（critic revise 后的修改）；同步新一致性 checkpoint
	// 让 review_style 通过一致性校验、命中 polish 新鲜度校验。
	newDraft := "# 一\nabc修改后的正文，她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check", "a2", domain.DigestDraft(newDraft),
	); err != nil {
		t.Fatal(err)
	}

	tool := NewReviewStyleTool(st, newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		panic("critic must not be called")
	}), testCriticVersion)
	tool.SetPipelineEnabled(true)

	_, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("review must be blocked when polish checkpoint is stale")
	}
	if !strings.Contains(err.Error(), "polish_draft") {
		t.Errorf("expected polish_draft hint, got: %v", err)
	}
}

// ── check_consistency required_next_action 提示 ──

// TestCheckConsistency_PipelineHintsPolishDraft 验证 pipeline 启用时，草稿缺少
// fresh polish checkpoint → required_next_action=polish_draft。
func TestCheckConsistency_PipelineHintsPolishDraft(t *testing.T) {
	st := setupCriticStore(t, 1, "# 一\nabc她心里骂自己丢人，真不要脸。")
	savePermissiveUserRules(t, st)
	tool := NewCheckConsistencyTool(st)
	tool.SetPipelineEnabled(true)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	action, ok := result["required_next_action"].(map[string]any)
	if !ok {
		t.Fatal("expected required_next_action")
	}
	if action["action"] != ActionPolishDraft {
		t.Fatalf("required_next_action.action = %v, want %s", action["action"], ActionPolishDraft)
	}
}

// TestCheckConsistency_PipelineFreshPolishNoHint 验证 pipeline 启用 + fresh polish
// checkpoint → 不再提示 polish_draft（走正常评审/提交建议）。
func TestCheckConsistency_PipelineFreshPolishNoHint(t *testing.T) {
	draft := "# 一\nabc她心里骂自己丢人，真不要脸。"
	st := setupCriticStore(t, 1, draft)
	savePermissiveUserRules(t, st)
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", domain.DigestDraft(draft),
		domain.PolishCheckpointMeta{InputDigest: domain.DigestDraft(draft), PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	tool := NewCheckConsistencyTool(st)
	tool.SetPipelineEnabled(true)

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	action, ok := result["required_next_action"].(map[string]any)
	if !ok {
		t.Fatal("expected required_next_action")
	}
	if action["action"] != ActionReviewStyle {
		t.Fatalf("required_next_action.action = %v, want %s（fresh polish 后应进入评审）", action["action"], ActionReviewStyle)
	}
}

// TestCheckConsistency_PipelineOffNoHint 验证 pipeline 关闭时行为不变（无 polish 提示）。
func TestCheckConsistency_PipelineOffNoHint(t *testing.T) {
	st := setupCriticStore(t, 1, "# 一\nabc她心里骂自己丢人，真不要脸。")
	savePermissiveUserRules(t, st)
	tool := NewCheckConsistencyTool(st) // 默认 pipeline 关闭

	out, err := tool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	action, ok := result["required_next_action"].(map[string]any)
	if !ok {
		t.Fatal("expected required_next_action")
	}
	if action["action"] != ActionReviewStyle {
		t.Fatalf("required_next_action.action = %v, want %s（pipeline 关闭不提示 polish_draft）", action["action"], ActionReviewStyle)
	}
}

// ── 63 恢复链路：返工章节无 polish → polish_draft → check_consistency（新 seq）→
//    review_style（新 epoch）→ commit 通过 ───────────────────────────────
//
// 全链路使用真实工具调用（不手工构造 epoch-2 账本）：pending_rewrites 章节在旧
// epoch terminal 账本上重新精修，critic 终验后 commit 放行并 drain 队列。

func TestPipeline_RewriteQueueFullCycle63Recovery(t *testing.T) {
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
	savePermissiveUserRules(t, st)

	// 已完成章节进入重写队列：终稿 = 旧版本，草稿 = 返工版本（尚未精修）。
	final := "# 一\n原始终稿内容。她心里骂自己丢人，真不要脸。"
	rework := "# 一\n返工后的草稿内容。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, rework); err != nil {
		t.Fatalf("SaveDraft rework: %v", err)
	}
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "返工"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := st.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	// 旧 epoch（epoch 1）terminal 账本：绑定原始终稿 digest。
	now := time.Now().Format(time.RFC3339)
	basisDigest := ComputeBasisDigest(st, 1, testCriticVersion)
	originalDigest := domain.DigestDraft(final)
	oldLedger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: originalDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: originalDigest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: testCriticVersion, Model: "critic-model"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(oldLedger); err != nil {
		t.Fatalf("Save old ledger: %v", err)
	}

	// 1) 无 polish → check_consistency 建议 polish_draft。
	checkTool := NewCheckConsistencyTool(st)
	checkTool.SetPipelineEnabled(true)
	out1, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check_consistency #1: %v", err)
	}
	var res1 map[string]any
	if err := json.Unmarshal(out1, &res1); err != nil {
		t.Fatal(err)
	}
	act1, _ := res1["required_next_action"].(map[string]any)
	if act1 == nil || act1["action"] != ActionPolishDraft {
		t.Fatalf("required_next_action #1 = %v, want %s", act1, ActionPolishDraft)
	}

	// 2) polish_draft（真实调用，mock polisher）→ 新 polish checkpoint（stage=rewrite）。
	polished := "# 一\n返工草稿经精修后的内容。她心里骂自己丢人，真不要脸。"
	polisher := newMockPolisher(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: polisherText(polished)}, nil
	})
	polishTool := newEnabledPolishTool(st, polisher)
	pOut, err := polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("polish_draft: %v", err)
	}
	var pRes PolishDraftOutput
	if err := json.Unmarshal(pOut, &pRes); err != nil {
		t.Fatal(err)
	}
	if !pRes.Polished || pRes.Changed == false {
		t.Fatalf("polish result = %+v, want polished+changed", pRes)
	}
	polishCP := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "polish")
	if polishCP == nil || polishCP.Stage != "rewrite" {
		t.Fatalf("polish checkpoint = %+v, want stage=rewrite", polishCP)
	}

	// 3) 精修后 check_consistency（新 seq）→ 建议 review_style。
	out2, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check_consistency #2: %v", err)
	}
	var res2 map[string]any
	if err := json.Unmarshal(out2, &res2); err != nil {
		t.Fatal(err)
	}
	act2, _ := res2["required_next_action"].(map[string]any)
	if act2 == nil || act2["action"] != ActionReviewStyle {
		t.Fatalf("required_next_action #2 = %v, want %s", act2, ActionReviewStyle)
	}

	// 4) review_style（pipeline 开启，critic pass）→ 新 epoch（epoch 2）终验。
	critic := newMockCritic(func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})
	reviewTool := NewReviewStyleTool(st, critic, testCriticVersion)
	reviewTool.SetPipelineEnabled(true)
	rOut, err := reviewTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review_style: %v", err)
	}
	var rRes StyleReviewOutput
	if err := json.Unmarshal(rOut, &rRes); err != nil {
		t.Fatal(err)
	}
	if rRes.Verdict != "pass" || rRes.Status != string(domain.ReviewStatusAcceptedInitial) {
		t.Fatalf("review = %s/%s, want pass/accepted_initial", rRes.Verdict, rRes.Status)
	}
	ledger, err := st.StyleReview.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger.MaxEpoch(); got != 2 {
		t.Fatalf("MaxEpoch = %d, want 2（新 epoch 终验）", got)
	}
	last := ledger.CurrentCycle()
	if last.Request == nil || last.Request.PolishCheckpointSeq != polishCP.Seq {
		t.Fatalf("epoch-2 result 绑定 polish seq = %+v, want %d", last.Request, polishCP.Seq)
	}

	// 5) commit（pipeline 门控开启）→ 放行并 drain 队列。
	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mock-polisher-model"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "返工提交", "characters": []string{"主角"},
		"key_events":       []string{"事件"},
		"world_state_mode": "preserve",
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("commit after full 63 chain should pass: %v", err)
	}
	progress, err := st.Progress.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.PendingRewrites) != 0 {
		t.Fatalf("PendingRewrites = %v, want drained", progress.PendingRewrites)
	}
	finalText, _ := st.Drafts.LoadChapterText(1)
	if finalText == "" {
		t.Fatal("final chapter should have been overwritten")
	}
}

// ── FSM 完整链路（规格第 14.2 节全链路单测） ──────────────────────────
// draft → check → polish → check → critic revise → edit → check → polish → check
// → critic pass → commit。每个节点先尝试非法后继动作：必须被 chapter_fsm 拦截
// （错误含 stage/attempted/required）且无副作用（checkpoint 数、账本周期数、
// 草稿、终稿均不变，mock 模型调用数不增），随后执行正确动作继续推进。

// fsmSnapshot 是"无副作用"断言的事实快照。
type fsmSnapshot struct {
	cpCount int
	cycles  int
	draft   string
	final   string
}

func snapshotChapterFSM(st *store.Store, chapter int) fsmSnapshot {
	draft, _ := st.Drafts.LoadDraft(chapter)
	final, _ := st.Drafts.LoadChapterText(chapter)
	cpCount := len(st.Checkpoints.All())
	cycles := 0
	if l, _ := st.StyleReview.Load(chapter); l != nil {
		cycles = len(l.Cycles)
	}
	return fsmSnapshot{cpCount: cpCount, cycles: cycles, draft: draft, final: final}
}

func assertFSMSnapshotEqual(t *testing.T, before, after fsmSnapshot, stage string) {
	t.Helper()
	if before != after {
		t.Errorf("side effects detected at stage %s: before=%+v after=%+v", stage, before, after)
	}
}

// rejectFSM 断言非法后继动作被 chapter_fsm 拦截且无副作用。
func rejectFSM(t *testing.T, st *store.Store, before fsmSnapshot, tool agentcore.Tool, args json.RawMessage, stage, attempted, required string, chapter int) {
	t.Helper()
	_, err := tool.Execute(t.Context(), args)
	if err == nil {
		t.Fatalf("stage %s: %s must be rejected, got nil error", stage, attempted)
	}
	msg := err.Error()
	for _, want := range []string{"code=chapter_fsm_transition_denied", "stage=" + stage, "attempted=" + attempted, "required=" + required} {
		if !strings.Contains(msg, want) {
			t.Errorf("stage %s: error %q missing %q", stage, msg, want)
		}
	}
	assertFSMSnapshotEqual(t, before, snapshotChapterFSM(st, chapter), stage+" / "+attempted)
}

// TestPipeline_FSMFullCycle 是启用 FSM 的完整链路（mock critic/polisher）。
func TestPipeline_FSMFullCycle(t *testing.T) {
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

	cfg := ChapterFSMConfig{Enabled: true, PipelineEnabled: true, ExpectedPolisherModel: "mock-polisher-model"}

	// 真实工具装配（mock critic/polisher），六工具全部注入 FSM 配置。
	polisherCalls, criticCalls := 0, 0
	polisher := newMockPolisher(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		polisherCalls++
		switch i {
		case 0:
			return &agentcore.LLMResponse{Message: polisherText("第一章初稿正文已经过精修，句子更凝练。她心里骂自己丢人，真不要脸。")}, nil
		default:
			return &agentcore.LLMResponse{Message: polisherText("第一章初稿正文二轮修改后再度精修，节奏明快。她心里骂自己丢人，真不要脸。")}, nil
		}
	})
	critic := newMockCritic(func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		criticCalls++
		if i == 0 {
			return &agentcore.LLMResponse{Message: criticText(productionReviseJSON())}, nil
		}
		return &agentcore.LLMResponse{Message: criticText(productionPassJSON())}, nil
	})

	draftTool := NewDraftChapterTool(st, testContract)
	checkTool := NewCheckConsistencyTool(st)
	polishTool := newEnabledPolishTool(st, polisher)
	reviewTool := NewReviewStyleTool(st, critic, testCriticVersion)
	editTool := NewEditChapterTool(st)
	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mock-polisher-model"})
	for _, tl := range []ChapterFSMConfigurable{draftTool, checkTool, polishTool, reviewTool, editTool, commitTool} {
		tl.SetChapterFSMConfig(cfg)
	}

	d1 := "第一章初稿正文。她心里骂自己丢人，真不要脸。"

	// ── 节点 0：needs_draft — commit 非法，draft 合法 ──
	snap := snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "needs_draft", "commit_chapter", "draft_chapter", 1)
	if _, err := draftTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"content":"`+d1+`","mode":"write"}`)); err != nil {
		t.Fatalf("draft (needs_draft) must pass: %v", err)
	}

	// ── 节点 1：draft_dirty — polish/commit 非法（polisher 0 调用），check 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, polishTool, json.RawMessage(`{"chapter":1}`), "draft_dirty", "polish_draft", "check_consistency", 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "draft_dirty", "commit_chapter", "check_consistency", 1)
	if polisherCalls != 0 {
		t.Fatalf("polisher must not be called in draft_dirty, got %d", polisherCalls)
	}
	out1, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("check (draft_dirty) must pass: %v", err)
	}
	var res1 map[string]any
	if err := json.Unmarshal(out1, &res1); err != nil {
		t.Fatal(err)
	}
	act1, _ := res1["required_next_action"].(map[string]any)
	if act1 == nil || act1["action"] != ActionPolishDraft {
		t.Fatalf("required_next_action after clean check = %v, want %s", act1, ActionPolishDraft)
	}

	// ── 节点 2：needs_polish — draft/edit/check/commit/review 非法，polish 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, draftTool, json.RawMessage(`{"chapter":1,"content":"越权重写","mode":"write"}`), "needs_polish", "draft_chapter", "polish_draft", 1)
	rejectFSM(t, st, snap, editTool, json.RawMessage(`{"chapter":1,"old_string":"初稿正文","new_string":"越权修改"}`), "needs_polish", "edit_chapter", "polish_draft", 1)
	rejectFSM(t, st, snap, checkTool, json.RawMessage(`{"chapter":1}`), "needs_polish", "check_consistency", "polish_draft", 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "needs_polish", "commit_chapter", "polish_draft", 1)
	rejectFSM(t, st, snap, reviewTool, json.RawMessage(`{"chapter":1}`), "needs_polish", "review_style", "polish_draft", 1)
	if criticCalls != 0 {
		t.Fatalf("critic must not be called in needs_polish, got %d", criticCalls)
	}
	if _, err := polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("polish (needs_polish) must pass: %v", err)
	}
	if polisherCalls != 1 {
		t.Fatalf("polisher calls = %d, want 1", polisherCalls)
	}

	// ── 节点 3：polish 已产生新候选（digest 变 → consistency stale → draft_dirty）
	// — review/commit 非法（critic 0 调用），check 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, reviewTool, json.RawMessage(`{"chapter":1}`), "draft_dirty", "review_style", "check_consistency", 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "draft_dirty", "commit_chapter", "check_consistency", 1)
	if criticCalls != 0 {
		t.Fatalf("critic must not be called before post-polish check, got %d", criticCalls)
	}
	if _, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("check (post-polish) must pass: %v", err)
	}

	// ── 节点 4：needs_review — commit 非法，review 合法（revise → revision_open） ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "needs_review", "commit_chapter", "review_style", 1)
	outRev, err := reviewTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("review (needs_review) must pass: %v", err)
	}
	var rev StyleReviewOutput
	if err := json.Unmarshal(outRev, &rev); err != nil {
		t.Fatal(err)
	}
	if rev.Verdict != "revise" || rev.Status != string(domain.ReviewStatusRevisionOpen) {
		t.Fatalf("first review = %s/%s, want revise/revision_open", rev.Verdict, rev.Status)
	}

	// ── 节点 5：revision_open — check/polish/review/commit 非法，edit 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, checkTool, json.RawMessage(`{"chapter":1}`), "revision_open", "check_consistency", "edit_chapter", 1)
	rejectFSM(t, st, snap, polishTool, json.RawMessage(`{"chapter":1}`), "revision_open", "polish_draft", "edit_chapter", 1)
	rejectFSM(t, st, snap, reviewTool, json.RawMessage(`{"chapter":1}`), "revision_open", "review_style", "edit_chapter", 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "revision_open", "commit_chapter", "edit_chapter", 1)
	if _, err := editTool.Execute(t.Context(), json.RawMessage(`{"chapter":1,"old_string":"已经过精修","new_string":"已经过二轮修改"}`)); err != nil {
		t.Fatalf("edit (revision_open) must pass: %v", err)
	}

	// ── 节点 6：draft_dirty（修订后 digest 变）— review/polish/commit 非法，check 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, reviewTool, json.RawMessage(`{"chapter":1}`), "draft_dirty", "review_style", "check_consistency", 1)
	rejectFSM(t, st, snap, polishTool, json.RawMessage(`{"chapter":1}`), "draft_dirty", "polish_draft", "check_consistency", 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "draft_dirty", "commit_chapter", "check_consistency", 1)
	if criticCalls != 1 {
		t.Fatalf("critic calls = %d, want 1（修订后未复检不得再评审）", criticCalls)
	}
	if _, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("check (draft_dirty after edit) must pass: %v", err)
	}

	// ── 节点 7：needs_polish（polish stale）→ polish 合法（P2） ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, reviewTool, json.RawMessage(`{"chapter":1}`), "needs_polish", "review_style", "polish_draft", 1)
	if _, err := polishTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("polish #2 must pass: %v", err)
	}
	if polisherCalls != 2 {
		t.Fatalf("polisher calls = %d, want 2", polisherCalls)
	}

	// ── 节点 8：polish #2 后 digest 再变 → draft_dirty；commit 非法，check 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "draft_dirty", "commit_chapter", "check_consistency", 1)
	if _, err := checkTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`)); err != nil {
		t.Fatalf("check #4 must pass: %v", err)
	}

	// ── 节点 9：needs_review → review 合法（pass → accepted_revised → needs_commit） ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, commitTool, commitArgs(1), "needs_review", "commit_chapter", "review_style", 1)
	outPass, err := reviewTool.Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatalf("final review must pass: %v", err)
	}
	var pass StyleReviewOutput
	if err := json.Unmarshal(outPass, &pass); err != nil {
		t.Fatal(err)
	}
	if pass.Verdict != "pass" || pass.Status != string(domain.ReviewStatusAcceptedRev) {
		t.Fatalf("final review = %s/%s, want pass/accepted_revised", pass.Verdict, pass.Status)
	}

	// ── 节点 10：needs_commit — draft/edit/polish/check 非法，commit 合法 ──
	snap = snapshotChapterFSM(st, 1)
	rejectFSM(t, st, snap, draftTool, json.RawMessage(`{"chapter":1,"content":"越权重写","mode":"write"}`), "needs_commit", "draft_chapter", "commit_chapter", 1)
	rejectFSM(t, st, snap, editTool, json.RawMessage(`{"chapter":1,"old_string":"再度精修","new_string":"越权修改"}`), "needs_commit", "edit_chapter", "commit_chapter", 1)
	rejectFSM(t, st, snap, polishTool, json.RawMessage(`{"chapter":1}`), "needs_commit", "polish_draft", "commit_chapter", 1)
	rejectFSM(t, st, snap, checkTool, json.RawMessage(`{"chapter":1}`), "needs_commit", "check_consistency", "commit_chapter", 1)
	if polisherCalls != 2 || criticCalls != 2 {
		t.Fatalf("mock calls drifted: polisher=%d critic=%d", polisherCalls, criticCalls)
	}
	if _, err := commitTool.Execute(t.Context(), commitArgs(1)); err != nil {
		t.Fatalf("commit (needs_commit) must pass: %v", err)
	}

	// 终态：final 已写、章节完成、账本 terminal。
	finalText, _ := st.Drafts.LoadChapterText(1)
	if finalText == "" {
		t.Fatal("final chapter must be written")
	}
	if !st.Progress.IsChapterCompleted(1) {
		t.Fatal("chapter must be completed after commit")
	}
	ledger, _ := st.StyleReview.Load(1)
	if ledger == nil || !ledger.CurrentStatus().IsTerminal() {
		t.Fatalf("ledger must be terminal after full cycle, got %v", ledger)
	}
}
