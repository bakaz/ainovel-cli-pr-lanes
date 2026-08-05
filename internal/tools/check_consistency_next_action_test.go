package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── 纯函数 ComputeRequiredNextAction 测试 ──────────────────────────────
// 正常路径 → 非 nil action；异常/error/mismatch/exhausted → nil（字段缺省）

// ── Rewrite queue ───────────────────────────────────────────────────

func TestRWQ_DraftEqualsFinal_ReturnsEdit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, nil, true, d)
	assertAction(t, a, ActionEditChapter)
}

// C1：返工草稿已修改但账本无绑定当前草稿的 terminal 结果 → 必须先 review_style 终验。
func TestRWQ_DraftChanged_NoLedger_ReturnsReview(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft, nil, true, final)
	assertAction(t, a, ActionReviewStyle)
}

// C1：返工草稿已修改但账本只有旧 digest 的 terminal 结果（未绑定当前草稿）→ review_style。
func TestRWQ_DraftChanged_StaleLedgerDigest_ReturnsReview(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldDigest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft, mkLedger(domain.ReviewStatusAcceptedInitial, oldDigest), true, final)
	assertAction(t, a, ActionReviewStyle)
}

// C1：返工草稿已修改且账本存在绑定当前草稿的 terminal 结果（新 epoch 终验通过）→ commit。
func TestRWQ_DraftChanged_TerminalBinding_ReturnsCommit(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft, mkLedger(domain.ReviewStatusAcceptedInitial, draft), true, final)
	assertAction(t, a, ActionCommitChapter)
}

func TestRWQ_NoFinalDigest_EqualsUnchanged_ReturnsEdit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, nil, true, "")
	assertAction(t, a, ActionEditChapter)
}

func TestRWQ_HasErrors_ReturnsNil(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, d, nil, true, d)
	assertNil(t, a)
}

// ── D2：mode=off 时 rewrite 走旧规则（不进 review_style 分支） ─────────

func TestRWQ_OffMode_DraftChanged_ReturnsCommit(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, false, draft, nil, true, final)
	assertAction(t, a, ActionCommitChapter)
}

func TestRWQ_OffMode_DraftEqualsFinal_ReturnsEdit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, false, d, nil, true, d)
	assertAction(t, a, ActionEditChapter)
}

func TestRWQ_OffMode_NoFinalDigest_ReturnsEdit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, false, d, nil, true, "")
	assertAction(t, a, ActionEditChapter)
}

// 对照：critic 模式 + rewrite + draft 已改（无账本）→ review_style（三段式，不进 commit）
func TestRWQ_CriticMode_DraftChanged_ReturnsReview(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft, nil, true, final)
	assertAction(t, a, ActionReviewStyle)
}

// ── Critic off ───────────────────────────────────────────────────────

func TestOff_NoErrors_ReturnsCommit(t *testing.T) {
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, false, "d", nil, false, "")
	assertAction(t, a, ActionCommitChapter)
}

func TestOff_EmptyModeTreatedAsOff(t *testing.T) {
	a := ComputeRequiredNextAction("", 1, false, "d", nil, false, "")
	assertAction(t, a, ActionCommitChapter)
}

func TestOff_HasErrors_ReturnsNil(t *testing.T) {
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, true, "d", nil, false, "")
	assertNil(t, a)
}

// C1 决策：mode=off 时 C1 不生效——PendingRewrite 章节走旧规则（draft != final → 建议
// commit，不进 review_style）。
func TestOff_RewriteDraftChanged_ReturnsCommit(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// 即使没有任何账本（无 critic 终验），off 模式也建议 commit（旧规则）。
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, false, draft, nil, true, final)
	assertAction(t, a, ActionCommitChapter)
}

func TestOff_RewriteDraftEqualsFinal_ReturnsEdit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, false, d, nil, true, d)
	assertAction(t, a, ActionEditChapter)
}

func TestOff_Rewrite_HasErrors_ReturnsNil(t *testing.T) {
	a := ComputeRequiredNextAction(domain.StyleQualityOff, 1, true, "d", nil, true, "f")
	assertNil(t, a)
}

// ── Critic on + no ledger ────────────────────────────────────────────

func TestCritOn_NilLedger_ReturnsReview(t *testing.T) {
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, "d", nil, false, "")
	assertAction(t, a, ActionReviewStyle)
}

func TestCritOn_EmptyLedger_ReturnsReview(t *testing.T) {
	l := &domain.StyleReviewLedger{Chapter: 1, Mode: domain.StyleQualityCritic}
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, "d", l, false, "")
	assertAction(t, a, ActionReviewStyle)
}

func TestCritOn_NilLedger_HasErrors_ReturnsNil(t *testing.T) {
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, "d", nil, false, "")
	assertNil(t, a)
}

// ── initial_pending ──────────────────────────────────────────────────

func TestInitPend_DigestValid_ReturnsReview(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, mkLedger(domain.ReviewStatusInitialPending, d), false, "")
	assertAction(t, a, ActionReviewStyle)
}

func TestInitPend_DigestMismatch_ReturnsNil(t *testing.T) {
	ld := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cd := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, cd, mkLedger(domain.ReviewStatusInitialPending, ld), false, "")
	assertNil(t, a)
}

func TestInitPend_InvalidDigest_ReturnsNil(t *testing.T) {
	l := &domain.StyleReviewLedger{
		Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusInitialPending,
			AttemptID: "a1", DraftDigest: "bad", BasisDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Request: &domain.StyleReviewRequest{Prompt: "t", Model: "m"},
		}},
	}
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, "x", l, false, "")
	assertNil(t, a)
}

func TestInitPend_HasErrors_ReturnsNil(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, d, mkLedger(domain.ReviewStatusInitialPending, d), false, "")
	assertNil(t, a)
}

// ── revision_open ────────────────────────────────────────────────────

func TestRevOpen_Unchanged_ReturnsEdit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, mkLedger(domain.ReviewStatusRevisionOpen, d), false, "")
	assertAction(t, a, ActionEditChapter)
}

func TestRevOpen_Changed_ReturnsReview(t *testing.T) {
	old := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cur := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, cur, mkLedger(domain.ReviewStatusRevisionOpen, old), false, "")
	assertAction(t, a, ActionReviewStyle)
}

func TestRevOpen_NoCycleDigest_ReturnsReview(t *testing.T) {
	l := &domain.StyleReviewLedger{
		Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusRevisionOpen,
			DraftDigest: "", BasisDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Request: &domain.StyleReviewRequest{Prompt: "t", Model: "m"},
			Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "w", Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"}}},
		}},
	}
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, "x", l, false, "")
	assertAction(t, a, ActionReviewStyle)
}

// revision_open + changed + errors → nil
func TestRevOpen_Changed_HasErrors_ReturnsNil(t *testing.T) {
	old := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cur := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, cur, mkLedger(domain.ReviewStatusRevisionOpen, old), false, "")
	assertNil(t, a)
}

// revision_open + unchanged + errors → nil (mechanical error)
func TestRevOpen_Unchanged_HasErrors_ReturnsNil(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, d, mkLedger(domain.ReviewStatusRevisionOpen, d), false, "")
	assertNil(t, a)
}

// ── final_pending ────────────────────────────────────────────────────

func TestFinalPend_DigestValid_ReturnsReview(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, mkLedger(domain.ReviewStatusFinalPending, d), false, "")
	assertAction(t, a, ActionReviewStyle)
}

func TestFinalPend_DigestMismatch_ReturnsNil(t *testing.T) {
	ld := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cd := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, cd, mkLedger(domain.ReviewStatusFinalPending, ld), false, "")
	assertNil(t, a)
}

func TestFinalPend_HasErrors_ReturnsNil(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, d, mkLedger(domain.ReviewStatusFinalPending, d), false, "")
	assertNil(t, a)
}

// ── Terminal ─────────────────────────────────────────────────────────

type termCase struct {
	name string
	st   domain.StyleReviewStatus
}

func terminalStatuses() []termCase {
	return []termCase{
		{"accepted_initial", domain.ReviewStatusAcceptedInitial},
		{"accepted_revised", domain.ReviewStatusAcceptedRev},
		// degraded 是"评审调用故障"（可恢复），语义独立——见下方
		// TestDegraded_* 系列（候选变化 → review_style，不再返回 nil）。
		{"overridden", domain.ReviewStatusOverridden},
	}
}

func TestTerm_DigestValid_ReturnsCommit(t *testing.T) {
	for _, tc := range terminalStatuses() {
		d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, mkLedger(tc.st, d), false, "")
		if a == nil || a.Action != ActionCommitChapter {
			t.Fatalf("%s: expected commit_chapter, got %v", tc.name, a)
		}
	}
}

func TestTerm_DigestMismatch_ReturnsNil(t *testing.T) {
	for _, tc := range terminalStatuses() {
		ld := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		cd := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		if a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, cd, mkLedger(tc.st, ld), false, ""); a != nil {
			t.Fatalf("%s mismatch: expected nil, got %+v", tc.name, a)
		}
	}
}

func TestTerm_HasErrors_ReturnsNil(t *testing.T) {
	for _, tc := range terminalStatuses() {
		d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, true, d, mkLedger(tc.st, d), false, ""); a != nil {
			t.Fatalf("%s errors: expected nil, got %+v", tc.name, a)
		}
	}
}

// ── C2：degraded（评审调用故障，可恢复）的 next action ────────────────
// 候选未变（digest 匹配 + 绑定 seq == 最新 polish，或无绑定）→ commit；
// 候选已变化（digest 或 seq 不匹配）→ review_style（而非 nil）。

func TestDegraded_DigestValid_ReturnsCommit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, mkLedger(domain.ReviewStatusDegraded, d), false, "")
	assertAction(t, a, ActionCommitChapter)
}

func TestDegraded_DigestMismatch_ReturnsReview(t *testing.T) {
	ld := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cd := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, cd, mkLedger(domain.ReviewStatusDegraded, ld), false, "")
	assertAction(t, a, ActionReviewStyle)
}

func TestDegraded_PipelineSeqStale_ReturnsReview(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// digest 匹配但绑定 seq（R=5）落后于最新 polish（P=6）→ 候选已更新 → review_style
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d,
		mkLedgerBound(domain.ReviewStatusDegraded, d, 5), false, "",
		&PolishPipelineBinding{LatestPolishSeq: 6})
	assertAction(t, a, ActionReviewStyle)
}

func TestDegraded_PipelineSeqCurrent_ReturnsCommit(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// digest 匹配且绑定 seq（R=6）== 最新 polish（P=6）→ 候选未变 → commit
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d,
		mkLedgerBound(domain.ReviewStatusDegraded, d, 6), false, "",
		&PolishPipelineBinding{LatestPolishSeq: 6})
	assertAction(t, a, ActionCommitChapter)
}

// ── exhausted → nil ─────────────────────────────────────────────────

func TestExhausted_ReturnsNil(t *testing.T) {
	d := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, d, mkLedger(domain.ReviewStatusExhausted, d), false, "")
	assertNil(t, a)
}

// ── unknown status → nil ────────────────────────────────────────────

func TestUnknownStatus_ReturnsNil(t *testing.T) {
	l := &domain.StyleReviewLedger{
		Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: "alien", DraftDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BasisDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
	}
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, "x", l, false, "")
	assertNil(t, a)
}

// ── hasErrorViolations ───────────────────────────────────────────────

func TestHasErrorViolations(t *testing.T) {
	tests := []struct {
		name string
		vs   []rules.Violation
		want bool
	}{
		{"empty", nil, false},
		{"only warnings", []rules.Violation{{Severity: rules.SeverityWarning}}, false},
		{"one error", []rules.Violation{{Severity: rules.SeverityError}}, true},
		{"warning then error", []rules.Violation{{Severity: rules.SeverityWarning}, {Severity: rules.SeverityError}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasErrorViolations(tc.vs); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ── 集成测试：正常路径有字段，异常路径字段缺省 ─────────────────────

func savePermissiveUserRules(t *testing.T, st *store.Store) {
	t.Helper()
	snap := rules.BuildSnapshot([]rules.Candidate{
		rules.SystemDefaults(),
		{Source: "test", Structured: rules.Structured{ChapterWords: &rules.WordRange{Min: 0, Max: 100000}}},
	})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}
}

func TestInteg_CriticOff_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityOff}))
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; !ok {
		t.Fatal("expected required_next_action")
	}
}

func TestInteg_CriticOn_NoLedger_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; !ok {
		t.Fatal("expected required_next_action")
	}
}

func TestInteg_Terminal_DigestMatch_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	content := "# 一\n终稿她心里骂自己丢人，真不要脸。"
	must(t, st.Drafts.SaveDraft(1, content))
	digest := domain.DigestDraft(content)
	basis := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Now().Format(time.RFC3339)
	must(t, st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, AttemptID: "a1",
				DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"}, CreatedAt: now},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, AttemptID: "a1",
				DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}, CreatedAt: now},
		},
	}))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; !ok {
		t.Fatal("expected required_next_action")
	}
}

func TestInteg_RevisionOpen_Unchanged_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	content := "# 一\n待改她心里骂自己丢人，真不要脸。"
	must(t, st.Drafts.SaveDraft(1, content))
	digest := domain.DigestDraft(content)
	basis := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	now := time.Now().Format(time.RFC3339)
	must(t, st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, AttemptID: "a1",
				DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"}, CreatedAt: now},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen, AttemptID: "a1",
				DraftDigest: digest, BasisDigest: basis,
				Request:   &domain.StyleReviewRequest{Prompt: "p", Model: "m"},
				Result:    &domain.StyleReviewResult{Verdict: domain.ReviewVerdictRevise, Evidence: "fix", Findings: []domain.StyleReviewFinding{{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"}}},
				CreatedAt: now},
		},
	}))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; !ok {
		t.Fatal("expected required_next_action")
	}
}

// ── 异常路径字段缺省 ────────────────────────────────────────────────

func TestInteg_NoRunMeta_FieldAbsent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; ok {
		t.Fatal("expected absent required_next_action when no run meta")
	}
}

func TestInteg_CorruptLedger_FieldAbsent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc"))
	writeFile(t, st.Dir(), "meta/style_review/01.json", `{corrupt!!!`)
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; ok {
		t.Fatal("expected absent required_next_action when ledger corrupt")
	}
}

func TestInteg_CorruptProgress_FieldAbsent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc"))
	writeFile(t, st.Dir(), "meta/progress.json", `{corrupt!!!`)
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; ok {
		t.Fatal("expected absent required_next_action when progress corrupt")
	}
}

func TestInteg_HasErrors_FieldAbsent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	// 不设宽松字数 → 短内容触发 chapter_words error
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	content := "# 一\n终稿"
	must(t, st.Drafts.SaveDraft(1, content))
	digest := domain.DigestDraft(content)
	basis := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Now().Format(time.RFC3339)
	must(t, st.StyleReview.Save(domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending, AttemptID: "a1",
				DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"}, CreatedAt: now},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial, AttemptID: "a1",
				DraftDigest: digest, BasisDigest: basis,
				Request: &domain.StyleReviewRequest{Prompt: "p", Model: "m"},
				Result:  &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "ok"}, CreatedAt: now},
		},
	}))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; ok {
		t.Fatal("expected absent required_next_action when has errors")
	}
}

func TestInteg_Rewrite_DraftVsFinal_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Progress.Init("t", 10))
	must(t, st.Drafts.SaveDraft(1, "# 一\n修改后她心里骂自己丢人，真不要脸。"))
	must(t, st.Drafts.SaveFinalChapter(1, "# 一\n原始终稿她心里骂自己丢人，真不要脸。"))
	must(t, st.Progress.MarkChapterComplete(1, 10, "crisis", "quest"))
	must(t, st.Progress.SetPendingRewrites([]int{1}, "重写"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; !ok {
		t.Fatal("expected required_next_action in rewrite")
	}
}

func TestInteg_Rewrite_DraftEqualsFinal_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Progress.Init("t", 10))
	content := "# 一\n未改她心里骂自己丢人，真不要脸。"
	must(t, st.Drafts.SaveDraft(1, content))
	must(t, st.Drafts.SaveFinalChapter(1, content))
	must(t, st.Progress.MarkChapterComplete(1, 10, "crisis", "quest"))
	must(t, st.Progress.SetPendingRewrites([]int{1}, "重写"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; !ok {
		t.Fatal("expected required_next_action in unchanged rewrite")
	}
}

func TestInteg_Rewrite_NoContentReturnsError(t *testing.T) {
	st := store.NewStore(t.TempDir())
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	_, err := NewCheckConsistencyTool(st).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err == nil {
		t.Fatal("expected error for missing content")
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func assertAction(t *testing.T, a *RequiredNextAction, want string) {
	t.Helper()
	if a == nil {
		t.Fatalf("expected non-nil action %q", want)
	}
	if a.Action != want {
		t.Fatalf("action=%q, want=%q; reason=%q", a.Action, want, a.Reason)
	}
	if a.Reason == "" {
		t.Fatal("reason must not be empty")
	}
}

func assertNil(t *testing.T, a *RequiredNextAction) {
	t.Helper()
	if a != nil {
		t.Fatalf("expected nil, got action=%q reason=%q", a.Action, a.Reason)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustExecute(t *testing.T, st *store.Store, ch int) []byte {
	t.Helper()
	out, err := NewCheckConsistencyTool(st).Execute(t.Context(), json.RawMessage(`{"chapter":`+fmt.Sprintf(`%d`, ch)+`}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out
}

func mustUnmarshal(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func writeFile(t *testing.T, storeDir, relPath, content string) {
	t.Helper()
	abs := filepath.Join(storeDir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkLedger(status domain.StyleReviewStatus, digest string) *domain.StyleReviewLedger {
	return &domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: buildCycles(status, digest),
	}
}

func buildCycles(target domain.StyleReviewStatus, digest string) []domain.StyleReviewEntry {
	req := &domain.StyleReviewRequest{Prompt: "p", Model: "m"}
	basis := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	switch target {
	case domain.ReviewStatusInitialPending:
		return []domain.StyleReviewEntry{{
			Cycle: 1, Status: domain.ReviewStatusInitialPending,
			AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
		}}
	case domain.ReviewStatusRevisionOpen:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "r",
					Findings: []domain.StyleReviewFinding{
						{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"},
					},
				}},
		}
	case domain.ReviewStatusFinalPending:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "r",
					Findings: []domain.StyleReviewFinding{
						{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"},
					},
				}},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
		}
	case domain.ReviewStatusAcceptedInitial:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "g"}},
		}
	case domain.ReviewStatusAcceptedRev:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "r",
					Findings: []domain.StyleReviewFinding{
						{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"},
					},
				}},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 4, Status: domain.ReviewStatusAcceptedRev,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "g"}},
		}
	case domain.ReviewStatusDegraded:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusDegraded,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req, Error: "API"},
		}
	case domain.ReviewStatusOverridden:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusOverridden,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Override: &domain.StyleReviewOverride{
					Actor: "u", Reason: "manual",
					DraftDigest: digest, BasisDigest: basis,
					OverriddenAt: "2026-07-26T00:00:00Z",
				}},
		}
	case domain.ReviewStatusExhausted:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "r",
					Findings: []domain.StyleReviewFinding{
						{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"},
					},
				}},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 4, Status: domain.ReviewStatusExhausted,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req,
				Result: &domain.StyleReviewResult{
					Verdict: domain.ReviewVerdictRevise, Evidence: "stagnant",
					Findings: []domain.StyleReviewFinding{
						{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"},
					},
				}},
		}
	default:
		return nil
	}
}

// ── C1-H1：pipeline 启用时 next action 绑定最新 polish seq（中等 2） ──────
// 场景：Critic 已评审 P1 → 生成同 digest 的 no-op polish P2 → gate 因 P2 != R1
// 拒绝 commit；next action 必须建议 review_style 而非 commit（控制面一致）。

func TestRWQ_PipelineBinding_SeqBoundLatest_ReturnsCommit(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// terminal 绑定 R=5 == 最新 polish P=5 → commit
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft,
		mkLedgerBound(domain.ReviewStatusAcceptedInitial, draft, 5), true, final,
		&PolishPipelineBinding{LatestPolishSeq: 5})
	assertAction(t, a, ActionCommitChapter)
}

func TestRWQ_PipelineBinding_NoOpPolish_ReturnsReview(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// Critic 已评审 P1（R=5），随后 no-op polish P2（最新 P=6，同 digest）→ review_style
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft,
		mkLedgerBound(domain.ReviewStatusAcceptedInitial, draft, 5), true, final,
		&PolishPipelineBinding{LatestPolishSeq: 6})
	assertAction(t, a, ActionReviewStyle)
}

func TestRWQ_PipelineBinding_LegacyZeroSeq_ReturnsReview(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// pipeline 启用但 terminal 无 seq 绑定（R=0，legacy）→ 建议重新终验
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft,
		mkLedger(domain.ReviewStatusAcceptedInitial, draft), true, final,
		&PolishPipelineBinding{LatestPolishSeq: 5})
	assertAction(t, a, ActionReviewStyle)
}

func TestRWQ_PipelineBinding_NilBindingPreservesOldBehavior(t *testing.T) {
	draft := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	final := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// pipeline 关闭（binding nil）→ 不做 seq 绑定，terminal + digest 匹配即 commit
	a := ComputeRequiredNextAction(domain.StyleQualityCritic, 1, false, draft,
		mkLedgerBound(domain.ReviewStatusAcceptedInitial, draft, 1), true, final)
	assertAction(t, a, ActionCommitChapter)
}

// mkLedgerBound 构造 terminal（accepted_initial）账本，并把全部请求的
// PolishCheckpointSeq 设为 seq。
func mkLedgerBound(status domain.StyleReviewStatus, digest string, seq int64) *domain.StyleReviewLedger {
	l := mkLedger(status, digest)
	for i := range l.Cycles {
		if l.Cycles[i].Request != nil {
			l.Cycles[i].Request.PolishCheckpointSeq = seq
		}
	}
	return l
}
