package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// setupPolishGateStore 构造 pipeline commit gate 测试用 store：
// critic 模式 + 草稿 + consistency checkpoint + 可选的 terminal ledger 与 polish checkpoint。
// ledgerAt 控制账本结果条目时间（"critic pass"）；polishAt 控制 polish checkpoint 的
// OccurredAt——AppendPolish 内部固定用 time.Now()，helper 在追加后改写
// checkpoints.jsonl 中该条目的 occurred_at 并重建 store，让 polishAt 真实生效。
func setupPolishGateStore(t *testing.T, chapter int, draft string, ledgerAt, polishAt time.Time) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatalf("SetStyleReviewMode: %v", err)
	}
	if err := st.Drafts.SaveDraft(chapter, draft); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(chapter), "consistency_check", "a1", digest,
	); err != nil {
		t.Fatalf("Append consistency checkpoint: %v", err)
	}
	if !ledgerAt.IsZero() {
		basisDigest := ComputeBasisDigest(st, chapter, "test-v1")
		ledger := domain.StyleReviewLedger{
			SchemaVersion: 1, Chapter: chapter, Mode: domain.StyleQualityCritic,
			Cycles: []domain.StyleReviewEntry{
				{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: ledgerAt.Format(time.RFC3339),
					AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
					Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
				{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: ledgerAt.Format(time.RFC3339),
					AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
					Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
					Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
			},
		}
		if err := st.StyleReview.Save(ledger); err != nil {
			t.Fatalf("Save ledger: %v", err)
		}
	}
	if !polishAt.IsZero() {
		if _, err := st.Checkpoints.AppendPolish(
			domain.ChapterScope(chapter), "polish", "a1", digest,
			domain.PolishCheckpointMeta{
				InputDigest:   digest,
				PolisherModel: "mimo-polisher",
				Stage:         "draft",
				Changed:       false,
			},
		); err != nil {
			t.Fatalf("AppendPolish: %v", err)
		}
		if err := patchCheckpointOccurredAt(t, dir, polishAt); err != nil {
			t.Fatalf("patch checkpoint occurred_at: %v", err)
		}
		// checkpoint cache 是内存镜像：改写磁盘后重建 store 使其重新加载。
		st = store.NewStore(dir)
	}
	return st
}

// patchCheckpointOccurredAt 把 checkpoints.jsonl 最后一条 checkpoint 的 occurred_at
// 改写为指定时间（测试需精确控制 polish 与评审的墙钟时序）。
func patchCheckpointOccurredAt(t *testing.T, dir string, at time.Time) error {
	t.Helper()
	path := filepath.Join(dir, "meta", "checkpoints.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		return fmt.Errorf("empty checkpoints file")
	}
	var cp map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &cp); err != nil {
		return err
	}
	cp["occurred_at"] = at.Format(time.RFC3339Nano)
	out, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	lines[len(lines)-1] = string(out)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// ── 1. fresh checkpoint（polish 早于 critic pass）→ 通过 ──

func TestPolishGate_FreshCheckpointPasses(t *testing.T) {
	polishAt := time.Now()
	ledgerAt := polishAt.Add(2 * time.Second) // critic pass 在 polish 之后
	st := setupPolishGateStore(t, 1, "精修后的正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("fresh polish checkpoint should pass: %v", err)
	}
}

// ── 2. 缺失 checkpoint → 拒绝 ──

func TestPolishGate_MissingCheckpointRejects(t *testing.T) {
	ledgerAt := time.Now()
	st := setupPolishGateStore(t, 1, "正文。", ledgerAt, time.Time{}) // 无 polish checkpoint

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("missing polish checkpoint should be rejected")
	}
	if !strings.Contains(err.Error(), "polish_draft") {
		t.Errorf("expected polish_draft hint, got: %v", err)
	}
}

// ── 3. stale checkpoint（草稿在精修后又改动）→ 拒绝 ──

func TestPolishGate_StaleDigestRejects(t *testing.T) {
	st := setupPolishGateStore(t, 1, "精修后的正文。", time.Now(), time.Now())
	// 精修后正文又被改 → digest 与 checkpoint 不匹配
	if err := st.Drafts.SaveDraft(1, "被 critic revise 后又改动的正文。"); err != nil {
		t.Fatal(err)
	}

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("stale polish checkpoint should be rejected")
	}
	if !strings.Contains(err.Error(), "重新调用 polish_draft") {
		t.Errorf("expected re-polish hint, got: %v", err)
	}
}

// ── 4. polisher model 与配置不一致 → 拒绝 ──

func TestPolishGate_WrongModelRejects(t *testing.T) {
	st := setupPolishGateStore(t, 1, "正文。", time.Now().Add(2*time.Second), time.Now())

	err := CheckPolishPipelineGate(st, 1, "other-model")
	if err == nil {
		t.Fatal("model mismatch should be rejected")
	}
	if !strings.Contains(err.Error(), "模型") {
		t.Errorf("expected model mismatch error, got: %v", err)
	}
}

// ── 5. 时序：critic pass 先于 polish → 拒绝 ──

func TestPolishGate_OrderingRejects(t *testing.T) {
	ledgerAt := time.Now()
	polishAt := ledgerAt.Add(2 * time.Second) // 评审先于 polish 完成
	st := setupPolishGateStore(t, 1, "正文。", ledgerAt, polishAt)

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("critic-before-polish ordering should be rejected")
	}
	if !strings.Contains(err.Error(), "先于 polish") {
		t.Errorf("expected ordering error, got: %v", err)
	}
}

// ── 5b. legacy（R=0）wall-clock 时序的 1 秒容忍窗口（次要 2） ─────────────
//
// criticAt 来自 RFC3339（整秒精度，丢失小数秒），polishCP.OccurredAt 保留小数
// 秒——同秒内 Critic 实际晚于 polish 也可能被判更早，故容忍 1 秒：critic 比
// polish 早不超过 1 秒视为合法（同秒 + 截断边界），超过 1 秒才拒绝。
// 三个用例全部以整秒基准构造，消除 time.Now() 的秒边界竞态，结果确定。

// TestPolishGate_LegacySameSecondTolerated 同秒内（critic 与 polish 落在同一
// 整秒）：截断后 criticAt == 该秒起点，晚于 polish 的小数秒 → 放行。
func TestPolishGate_LegacySameSecondTolerated(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	ledgerAt := base.Add(400 * time.Millisecond) // critic pass 与 polish 同秒
	polishAt := base.Add(700 * time.Millisecond)
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("same-second critic/polish must be tolerated (RFC3339 truncation), got: %v", err)
	}
}

// TestPolishGate_LegacySubSecondGapTolerated critic 实际早于 polish 0.9 秒：
// 整秒截断后仍在 1 秒容忍窗口内 → 放行。
func TestPolishGate_LegacySubSecondGapTolerated(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	ledgerAt := base                             // critic 记录为 base（整秒）
	polishAt := base.Add(900 * time.Millisecond) // 实际间隔 0.9 秒
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("0.9s gap must be tolerated (within 1s window), got: %v", err)
	}
}

// TestPolishGate_LegacyOverSecondGapRejected critic 实际早于 polish 1.1 秒：
// 超出 1 秒容忍窗口 → 拒绝（评审对象不是精修后的正文）。
func TestPolishGate_LegacyOverSecondGapRejected(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	ledgerAt := base.Add(-time.Second)           // critic 记录为 base-1s
	polishAt := base.Add(100 * time.Millisecond) // 实际间隔 1.1 秒
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("critic 1.1s before polish must be rejected (outside 1s window)")
	}
	if !strings.Contains(err.Error(), "先于 polish") {
		t.Errorf("expected ordering error, got: %v", err)
	}
}

// ── 6. C1：重写/打磨队列同样适用时序校验（bypass 已删除） ──

// TestPolishGate_RewriteQueueLegacyOldResultRejected 验证返工章节的旧 epoch 评审
// （无 PolishCheckpointSeq 绑定、时间早于本次 polish）不再被放行——必须经新 epoch
// 的 critic 终验才能 commit。
func TestPolishGate_RewriteQueueLegacyOldResultRejected(t *testing.T) {
	draft := "重写后的终稿。"
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
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatal(err)
	}
	// 旧 critic pass（原始评审，早于本次 polish）——C1 下不再放行
	oldAt := time.Now().Add(-time.Hour)
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	if err := st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: oldAt.Format(time.RFC3339),
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: oldAt.Format(time.RFC3339),
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// fresh polish checkpoint（本次打磨，seq 晚于旧评审的 OccurredAt）
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: true},
	); err != nil {
		t.Fatal(err)
	}

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("rewrite-queue chapter with only legacy old result must be rejected (C1)")
	}
	if !strings.Contains(err.Error(), "polish_draft") && !strings.Contains(err.Error(), "先于 polish") {
		t.Errorf("expected re-polish/ordering rejection, got: %v", err)
	}
}

// TestPolishGate_RewriteQueueNewEpochBoundResultPasses 验证返工章节经新 epoch 评审
// （result 绑定当前 polish 的 seq）后可提交。
func TestPolishGate_RewriteQueueNewEpochBoundResultPasses(t *testing.T) {
	draft := "重写后的终稿。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	// 本次打磨的 polish checkpoint（seq P）
	polishCP, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 新 epoch（epoch 2）的终验结果，绑定本次 polish 的 seq（R = P）
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
			{Cycle: 3, Status: domain.ReviewStatusInitialPending, CreatedAt: now, Epoch: 2,
				AttemptID: "rw-1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: polishCP.Seq}},
			{Cycle: 4, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now, Epoch: 2,
				AttemptID: "rw-1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: polishCP.Seq},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("rewrite-queue chapter with new-epoch bound result should pass: %v", err)
	}
}

// TestPolishGate_SeqBindingRejectsOlderPolish 验证 seq 绑定：评审绑定的 polish seq
// （R）晚于当前最新 polish seq（P）→ 拒绝（当前 polish candidate 早于评审依据）。
func TestPolishGate_SeqBindingRejectsOlderPolish(t *testing.T) {
	draft := "正文。"
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
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	// 评审绑定虚构的更高 polish seq（100）——评审基于比当前 polish 更新的版本
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 100}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 100},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	// 当前 polish candidate（seq P < 100）
	pcp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pcp.Seq >= 100 {
		t.Skip("seq 分配已超过 100，无法构造 R > P 场景")
	}

	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("review bound to newer polish (R > P) must be rejected")
	}
	if !strings.Contains(err.Error(), "seq") {
		t.Errorf("expected seq-binding rejection, got: %v", err)
	}
}

// ── 7. 集成：pipeline 开启 + 全满足 → commit 成功；缺 polish checkpoint → 拒绝 ──

func TestCommitChapter_PipelineGateIntegration(t *testing.T) {
	draft := "精修后的正文。她心里骂自己丢人，真不要脸。"
	polishAt := time.Now()
	ledgerAt := polishAt.Add(2 * time.Second)
	st := setupPolishGateStore(t, 1, draft, ledgerAt, polishAt)

	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mimo-polisher"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("commit with fresh polish checkpoint should succeed: %v", err)
	}
}

func TestCommitChapter_PipelineGateBlocksWithoutCheckpoint(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
	st := setupPolishGateStore(t, 1, draft, time.Now(), time.Time{}) // 无 polish checkpoint

	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mimo-polisher"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	_, err := commitTool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("commit without polish checkpoint should be rejected when pipeline enabled")
	}
	if !strings.Contains(err.Error(), "polish_draft") {
		t.Errorf("expected polish_draft hint in commit error, got: %v", err)
	}
}

// ── 8. 集成：pipeline 关闭 → 不拦截（旧项目行为不变） ──

func TestCommitChapter_PipelineDisabledNotIntercepted(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
	st := setupPolishGateStore(t, 1, draft, time.Now(), time.Time{}) // 无 polish checkpoint

	// 未调用 SetPolishPipeline → 门控关闭，commit 正常
	commitTool := NewCommitChapterTool(st)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("commit without pipeline config must not be intercepted: %v", err)
	}
}

// ── 9. no-op checkpoint（changed=false）也算已处理 ──

func TestPolishGate_NoOpCheckpointCountsAsFresh(t *testing.T) {
	polishAt := time.Now()
	ledgerAt := polishAt.Add(2 * time.Second)
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)
	// setup 已写 changed=false 的 checkpoint（no-op）→ 仍应通过
	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("no-op polish checkpoint should count as handled: %v", err)
	}
}

// ── C1-H1：P > R 拒绝（M2-6：评审后新增 polish 未重新 review 就 commit 被拒） ──

func TestPolishGate_NewPolishAfterReviewRejected(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
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
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")

	// 第一次 polish → 评审绑定 P1
	p1, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: p1.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: p1.Seq},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	// 评审之后又跑了一次 polish_draft（no-op，同 digest）→ 最新 polish seq P2 > R
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}

	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("new polish after review without re-review must be rejected (P > R)")
	}
	if !strings.Contains(err.Error(), "seq") {
		t.Errorf("expected seq-binding rejection, got: %v", err)
	}
}

// ── C1-H1：BySeq 复核的 stage 负向校验（M2-7） ──
// 说明：P == R 严格相等下，BySeq(R) 即本章最新 polish checkpoint（scope/step/digest
// 天然一致，digest 另由 step 1 校验），因此可触发的负向路径是 stage 与场景不匹配、
// stage 非法、polisher_model 不一致（模型已由 step 2 覆盖）——这些是纵深防御。

// TestPolishGate_BoundStageMismatchRewriteRejected 验证重写队列章节绑定 stage="draft"
// 的 polish → 拒绝（期望 "rewrite"）。
func TestPolishGate_BoundStageMismatchRewriteRejected(t *testing.T) {
	draft := "返工草稿。她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	// polish CP stage="draft"（与重写场景不符）
	cp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("rewrite-queue chapter bound to stage=draft polish must be rejected")
	}
	if !strings.Contains(err.Error(), "stage") && !strings.Contains(err.Error(), "rewrite") {
		t.Errorf("expected stage mismatch rejection, got: %v", err)
	}
}

// TestPolishGate_BoundStageIllegalRejected 验证绑定 stage 非 rewrite/draft 的 polish
// → 拒绝。
func TestPolishGate_BoundStageIllegalRejected(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	cp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "garbage", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("bound polish with illegal stage must be rejected")
	}
	if !strings.Contains(err.Error(), "stage") {
		t.Errorf("expected stage rejection, got: %v", err)
	}
}

// TestPolishGate_BoundStageMismatchFreshRejected 验证非重写章节绑定 stage="rewrite"
// 的 polish → 拒绝（期望 "draft"）。
func TestPolishGate_BoundStageMismatchFreshRejected(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	cp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "rewrite", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: cp.Seq},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("non-rewrite chapter bound to stage=rewrite polish must be rejected")
	}
	if !strings.Contains(err.Error(), "stage") && !strings.Contains(err.Error(), "draft") {
		t.Errorf("expected stage mismatch rejection, got: %v", err)
	}
}

// ── BySeq 绑定复核：伪造/跨章/跨步骤的 R 一律拒绝 ──────────────────────

// TestPolishGate_BoundSeqWrongChapterRejected 验证评审绑定指向其他章节的
// checkpoint seq 时被拒绝（P == R 严格相等校验拦截）。
func TestPolishGate_BoundSeqWrongChapterRejected(t *testing.T) {
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
	// 章节 2 的 polish checkpoint（seq 会被章节 1 的账本伪造引用）
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(2), "polish", "a1", "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		domain.PolishCheckpointMeta{PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	draft := "正文。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}
	polishCP, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", digest,
		domain.PolishCheckpointMeta{PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 账本当前周期绑定章节 2 的 polish seq（= 1），而非本章最新 polish seq（= polishCP.Seq）
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now, AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now, AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if polishCP.Seq == 1 {
		t.Skip("seq 分配与章节 2 的 checkpoint 重合，无法构造跨章绑定场景")
	}
	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("bound seq pointing at another chapter must be rejected")
	}
	if !strings.Contains(err.Error(), "seq") {
		t.Errorf("expected seq-binding rejection, got: %v", err)
	}
}

// TestPolishGate_BoundSeqWrongStepRejected 验证评审绑定指向非 polish 步骤的
// checkpoint seq（如 consistency_check）时被拒绝。
func TestPolishGate_BoundSeqWrongStepRejected(t *testing.T) {
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
	draft := "正文。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	// consistency checkpoint（seq 1）先落，polish（seq 2）后落
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}
	polishCP, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", digest,
		domain.PolishCheckpointMeta{PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 账本绑定 consistency_check 的 seq（1）——不是 polish 步骤
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now, AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now, AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if polishCP.Seq == 1 {
		t.Skip("seq 分配与 consistency checkpoint 重合，无法构造跨步骤绑定场景")
	}
	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("bound seq pointing at a non-polish checkpoint must be rejected")
	}
	if !strings.Contains(err.Error(), "seq") {
		t.Errorf("expected seq-binding rejection, got: %v", err)
	}
}

// TestPolishGate_BoundSeqStaleDigestRejected 验证评审绑定指向旧 polish checkpoint
// （digest 已被新精修取代）时被拒绝：新增 polish 后未重新 review 就 commit。
func TestPolishGate_BoundSeqStaleDigestRejected(t *testing.T) {
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
	draft := "初版正文。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest1 := domain.DigestDraft(draft)
	// 第一次 polish（digest1）
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest1,
		domain.PolishCheckpointMeta{PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
	); err != nil {
		t.Fatal(err)
	}
	// 草稿变更 + 第二次 polish（digest2）→ 最新 polish seq 前进
	newDraft := "精修后的正文。"
	if err := st.Drafts.SaveDraft(1, newDraft); err != nil {
		t.Fatal(err)
	}
	digest2 := domain.DigestDraft(newDraft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a2", digest2); err != nil {
		t.Fatal(err)
	}
	polishCP2, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a3", digest2,
		domain.PolishCheckpointMeta{PolisherModel: "mimo-polisher", Stage: "draft", Changed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 账本绑定第一次 polish 的 seq（R < P）——评审对象不是当前精修版本
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now, AttemptID: "a1", DraftDigest: digest2, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now, AttemptID: "a1", DraftDigest: digest2, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if polishCP2.Seq == 1 {
		t.Skip("seq 分配与第一次 polish 重合，无法构造 R < P 场景")
	}
	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("bound seq older than latest polish (P != R) must be rejected")
	}
	if !strings.Contains(err.Error(), "seq") {
		t.Errorf("expected seq-binding rejection, got: %v", err)
	}
}

// ── 11. mode=off + pipeline：仅执行 pipeline 自身契约，不套用 critic 绑定（阻断 1） ──

// TestPolishGate_OffModeNoLedgerPasses 验证 off 模式 + pipeline + 无账本 + fresh
// polish：gate 通过且不 panic。回归：旧实现在 ledger 为 nil 时对 CurrentCycle()
// 解引用 nil receiver 直接 panic。
func TestPolishGate_OffModeNoLedgerPasses(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityOff); err != nil {
		t.Fatal(err)
	}
	draft := "正文。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("off + pipeline + no ledger + fresh polish must pass without panic: %v", err)
	}
}

// TestPolishGate_OffModeSkipsCriticSeqBinding 验证 off 模式 + pipeline + 历史 critic
// 账本（P ≠ R）：不应用 critic seq 绑定（D2：mode=off 走旧规则），gate 通过。
func TestPolishGate_OffModeSkipsCriticSeqBinding(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityOff); err != nil {
		t.Fatal(err)
	}
	draft := "正文。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	// 账本绑定虚构 seq 1，而最新 polish seq 为 2（P ≠ R）——off 模式不得拒绝
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	if err := st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: 1},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a2", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("off + pipeline + critic ledger with P≠R must not apply the critic seq gate: %v", err)
	}
}

// TestCommitChapter_OffModePipelinePasses 集成回归：off + pipeline + 无账本 +
// fresh polish → commit 不 panic 且通过（阻断 1 的端到端路径）。
func TestCommitChapter_OffModePipelinePasses(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityOff); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "consistency_check", "a1", digest,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}

	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mimo-polisher"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("off + pipeline + no ledger commit must pass without panic: %v", err)
	}
}

// ── 12. 显式配置模型 + checkpoint 未记录模型（空）→ 拒绝（低级 4） ────────

func TestPolishGate_EmptyCheckpointModelRejected(t *testing.T) {
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
	draft := "正文。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	// checkpoint 未记录 PolisherModel（空）
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("checkpoint with empty polisher model must be rejected when a model is configured")
	}
	if !strings.Contains(err.Error(), "未记录 polisher 模型") {
		t.Errorf("expected empty-model rejection, got: %v", err)
	}
}

// ── 13. legacy 墙钟比较的 1 秒容忍窗口（次要 2） ────────────────────────
// criticAt 来自 RFC3339（整秒精度），polishCP.OccurredAt 保留小数秒——同秒内
// Critic 实际晚于 polish 也可能被判更早。修复后容忍 1 秒窗口：critic 比 polish
// 早不超过 1 秒视为合法。helper 的 polishAt 参数真实改写 occurred_at。

// TestPolishGate_LegacySameSecondTolerance 验证同秒场景（critic 实际晚于
// polish，但整秒截断后 criticAt 早于带小数秒的 OccurredAt）→ 放行。
func TestPolishGate_LegacySameSecondTolerance(t *testing.T) {
	base := time.Date(2026, 8, 2, 10, 0, 0, 0, time.Local)
	polishAt := base.Add(200 * time.Millisecond) // polish 在 10:00:00.200
	ledgerAt := base.Add(700 * time.Millisecond) // critic 在 10:00:00.700（晚于 polish，同秒）
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("same-second critic-after-polish must pass within the 1s tolerance window: %v", err)
	}
}

// TestPolishGate_LegacyBeyondSecondWindowRejects 验证超出 1 秒窗口仍拒绝
// （critic 早于 polish 1.5s → 评审对象确实不是精修后的正文）。
func TestPolishGate_LegacyBeyondSecondWindowRejects(t *testing.T) {
	base := time.Date(2026, 8, 2, 10, 0, 0, 0, time.Local)
	polishAt := base.Add(1500 * time.Millisecond) // polish 在 10:00:01.500
	ledgerAt := base                              // critic 在 10:00:00.000（早 1.5s）
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。", ledgerAt, polishAt)

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("critic 1.5s before polish must still be rejected (beyond tolerance window)")
	}
	if !strings.Contains(err.Error(), "先于 polish") {
		t.Errorf("expected ordering rejection, got: %v", err)
	}
}

// TestPolishGate_OrderingRejectsUsesPatchedOccurredAt 验证 polishAt 参数真实
// 生效：评审时间未变、仅推迟 polish checkpoint 的 occurred_at → 从放行变为拒绝
// （回归：此前 polishAt 只控制是否创建 checkpoint，时间恒为 time.Now()）。
func TestPolishGate_OrderingRejectsUsesPatchedOccurredAt(t *testing.T) {
	base := time.Date(2026, 8, 2, 10, 0, 0, 0, time.Local)
	// 同样 ledgerAt（critic 10:00:00），polish 从 10:00:00.5（同秒放行）推到
	// 10:00:03（超过 1s 窗口拒绝）——只有 occurred_at 补丁真实生效才能区分。
	st := setupPolishGateStore(t, 1, "正文。她心里骂自己丢人，真不要脸。",
		base.Add(500*time.Millisecond), base.Add(3*time.Second))

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("critic before polish by 3s must be rejected")
	}
	if !strings.Contains(err.Error(), "先于 polish") {
		t.Errorf("expected ordering rejection, got: %v", err)
	}
}

// stripLastCheckpointOccurredAt 把 checkpoints.jsonl 最后一条 checkpoint 的
// occurred_at 字段删掉（模拟 legacy checkpoint 无完成时间），重建 store 使其
// 从磁盘重新加载（time.Time 缺省为零值）。
func stripLastCheckpointOccurredAt(t *testing.T, dir string) *store.Store {
	t.Helper()
	path := filepath.Join(dir, "meta", "checkpoints.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var cp map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &cp); err != nil {
		t.Fatal(err)
	}
	delete(cp, "occurred_at")
	out, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	lines[len(lines)-1] = string(out)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return store.NewStore(dir)
}

// TestPolishGate_LegacyMissingTimeTolerated legacy（R=0）绑定时间缺失时按已绑定
// 放行（fail-open，与 FSM 的 reviewBindsPolish 共用同一 helper，规格 §4）：
// 不因无法比较墙钟时间而拒绝旧账本提交（避免 legacy 账本突然死锁）。
// 说明：ledger 存储层强制 created_at 为合法 RFC3339（无法构造缺失时间的账本），
// 因此缺失侧通过 polish checkpoint 的零 OccurredAt 模拟——helper 的 fail-open
// 分支（CreatedAt=="" 或 OccurredAt 零值）两侧共享，FSM 侧缺失 CreatedAt 的
// 场景由 chapter_stage_test.go 的 25c 用例覆盖。
func TestPolishGate_LegacyMissingTimeTolerated(t *testing.T) {
	draft := "正文。她心里骂自己丢人，真不要脸。"
	dir := t.TempDir()
	st := store.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.SetStyleReviewMode(domain.StyleQualityCritic); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	// legacy 条目：R=0（Request 无 PolishCheckpointSeq），时间戳存在但墙钟比较
	// 因 polish 侧时间缺失而无法进行。
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"}},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "mimo-polisher", Stage: "draft", Changed: false},
	); err != nil {
		t.Fatal(err)
	}
	// polish checkpoint 的 occurred_at 缺失 → 墙钟比较无法进行，按已绑定放行。
	st = stripLastCheckpointOccurredAt(t, dir)

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("legacy 绑定时间缺失应按已绑定放行（与 FSM 一致），got: %v", err)
	}
}

// ── 14. degraded polish checkpoint（Oracle 方案第 3 步） ────────────────
//
// degraded 记录（polisher 失败降级，正文未变、Digest=当前草稿）在 commit gate 中
// 同样满足 fresh 校验；模型一致性检查被跳过（未调用模型）；digest/stage/seq 绑定
// 校验原样执行——degraded 后 review 绑定的正是该记录，R == P 自然成立。

// TestPolishGate_DegradedCheckpointPasses 验证 degraded polish checkpoint +
// 绑定评审（R==P）→ gate 通过。checkpoint 未记录 polisher 模型（空）也不拒绝——
// degraded 跳过模型一致性检查（对照 TestPolishGate_EmptyCheckpointModelRejected）。
func TestPolishGate_DegradedCheckpointPasses(t *testing.T) {
	draft := "精修失败但正文未变的草稿。她心里骂自己丢人，真不要脸。"
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
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}
	// degraded polish checkpoint：Digest=当前草稿、Degraded=true、ErrorCategory=stream_idle、
	// PolisherModel 为空（未调用模型）
	dcp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "", Stage: "draft", Changed: false, Degraded: true, ErrorCategory: "stream_idle"},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 评审绑定该 degraded 记录的 seq（R == P）
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: dcp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: dcp.Seq},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	if err := CheckPolishPipelineGate(st, 1, "mimo-polisher"); err != nil {
		t.Fatalf("degraded polish checkpoint must pass the gate (model check skipped): %v", err)
	}
}

// TestPolishGate_DegradedStaleDigestRejects 验证 degraded checkpoint 的 fresh
// 校验仍然生效：Digest 与当前草稿不匹配 → 拒绝并要求重新 polish_draft。
func TestPolishGate_DegradedStaleDigestRejects(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, "当前草稿。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}
	currentDigest := domain.DigestDraft("当前草稿。她心里骂自己丢人，真不要脸。")
	// degraded checkpoint 绑定的是旧 digest（草稿在 degraded 后又被修改）
	if _, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", "sha256:9999999999999999999999999999999999999999999999999999999999999999",
		domain.PolishCheckpointMeta{InputDigest: currentDigest, PolisherModel: "", Stage: "draft", Changed: false, Degraded: true, ErrorCategory: "stream_idle"},
	); err != nil {
		t.Fatal(err)
	}

	err := CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("degraded checkpoint with stale digest must be rejected (fresh check still applies)")
	}
	if !strings.Contains(err.Error(), "重新调用 polish_draft") {
		t.Errorf("expected re-polish hint, got: %v", err)
	}
}

// TestPolishGate_DegradedStageMismatchRejects 验证 degraded checkpoint 的 stage
// 检查仍然生效：重写队列章节的 degraded 记录 stage=draft → 拒绝（期望 rewrite）。
func TestPolishGate_DegradedStageMismatchRejects(t *testing.T) {
	draft := "重写草稿。她心里骂自己丢人，真不要脸。"
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, "旧终稿。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "打磨"); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	dcp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "", Stage: "draft", Changed: false, Degraded: true, ErrorCategory: "max_turns"},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 评审绑定该 degraded 记录（stage=draft 与重写场景不匹配）
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: dcp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: dcp.Seq},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	err = CheckPolishPipelineGate(st, 1, "mimo-polisher")
	if err == nil {
		t.Fatal("degraded checkpoint with wrong stage must be rejected (stage check still applies)")
	}
	if !strings.Contains(err.Error(), "stage") && !strings.Contains(err.Error(), "rewrite") {
		t.Errorf("expected stage mismatch rejection, got: %v", err)
	}
}

// TestCommitChapter_DegradedPipelinePasses 集成：degraded polish → check →
// degraded 账本绑定 → commit 工具整体放行（账本留痕，允许跳过 polish 提交）。
func TestCommitChapter_DegradedPipelinePasses(t *testing.T) {
	draft := "精修失败但正文未变的草稿。她心里骂自己丢人，真不要脸。"
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
	if err := st.Drafts.SaveDraft(1, draft); err != nil {
		t.Fatal(err)
	}
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "a1", digest); err != nil {
		t.Fatal(err)
	}
	dcp, err := st.Checkpoints.AppendPolish(
		domain.ChapterScope(1), "polish", "a1", digest,
		domain.PolishCheckpointMeta{InputDigest: digest, PolisherModel: "", Stage: "draft", Changed: false, Degraded: true, ErrorCategory: "stream_idle"},
	)
	if err != nil {
		t.Fatal(err)
	}
	basisDigest := ComputeBasisDigest(st, 1, "test-v1")
	now := time.Now().Format(time.RFC3339)
	ledger := domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: dcp.Seq}},
			{Cycle: 2, Status: domain.ReviewStatusDegraded, CreatedAt: now,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basisDigest,
				Request: &domain.StyleReviewRequest{Prompt: "test-v1", Model: "critic-model", PolishCheckpointSeq: dcp.Seq},
				Error:   "critic call failed"},
		},
	}
	if err := st.StyleReview.Save(ledger); err != nil {
		t.Fatal(err)
	}

	commitTool := NewCommitChapterTool(st)
	commitTool.SetPolishPipeline(&PolishPipelineConfig{ExpectedModel: "mimo-polisher"})
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "测试", "characters": []string{"主角"},
		"key_events": []string{"事件"},
	})
	if _, err := commitTool.Execute(t.Context(), args); err != nil {
		t.Fatalf("commit with degraded polish checkpoint should succeed (账本留痕放行): %v", err)
	}
}
