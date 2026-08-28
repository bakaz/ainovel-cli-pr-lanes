package tools

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── 纯 FSM 表驱动测试（规格第 14.1 节，25+ 用例） ─────────────────────

// dig 生成一个合法的 sha256 digest（确定性）。
func dig(s string) string { return domain.DigestDraft(s) }

// consistencyCP 构造 consistency_check checkpoint。
func consistencyCP(seq int64, digest string) *domain.Checkpoint {
	return &domain.Checkpoint{Seq: seq, Scope: domain.ChapterScope(1), Step: "consistency_check", Digest: digest, OccurredAt: time.Now()}
}

// polishCP 构造 polish checkpoint（OccurredAt = 当前时刻）。
func polishCP(seq int64, digest, stage, model string) *domain.Checkpoint {
	return &domain.Checkpoint{Seq: seq, Scope: domain.ChapterScope(1), Step: "polish", Digest: digest, Stage: stage, PolisherModel: model, OccurredAt: time.Now()}
}

// polishCPAt 构造 OccurredAt 受控的 polish checkpoint（legacy 容差测试用）。
func polishCPAt(seq int64, digest, stage string, at time.Time) *domain.Checkpoint {
	return &domain.Checkpoint{Seq: seq, Scope: domain.ChapterScope(1), Step: "polish", Digest: digest, Stage: stage, OccurredAt: at}
}

// degradedPolishCP 构造 degraded polish checkpoint（polisher 失败降级记录，
// Degraded=true + ErrorCategory=stream_idle）。
func degradedPolishCP(seq int64, digest, stage, model string) *domain.Checkpoint {
	cp := polishCP(seq, digest, stage, model)
	cp.Degraded = true
	cp.ErrorCategory = "stream_idle"
	return cp
}

// mkLedger 构造指定终态的单章 critic 账本（循环结构合法）。
func mkLedger(status domain.StyleReviewStatus, digest string) *domain.StyleReviewLedger {
	return &domain.StyleReviewLedger{
		SchemaVersion: 1, Chapter: 1, Mode: domain.StyleQualityCritic,
		Cycles: buildCycles(status, digest),
	}
}

// mkLedgerBound 构造 terminal 账本，并把全部请求的 PolishCheckpointSeq 设为 seq。
func mkLedgerBound(status domain.StyleReviewStatus, digest string, seq int64) *domain.StyleReviewLedger {
	l := mkLedger(status, digest)
	for i := range l.Cycles {
		if l.Cycles[i].Request != nil {
			l.Cycles[i].Request.PolishCheckpointSeq = seq
		}
	}
	return l
}

// mkLedgerAt 构造 terminal 账本并把全部条目 CreatedAt 设为给定时刻
// （legacy seq==0 的 wall-clock 绑定测试用）。
func mkLedgerAt(status domain.StyleReviewStatus, digest, createdAt string) *domain.StyleReviewLedger {
	l := mkLedger(status, digest)
	for i := range l.Cycles {
		l.Cycles[i].CreatedAt = createdAt
	}
	return l
}

// mkLedgerNilRequest 构造 terminal 账本并把全部条目 Request 置 nil
// （legacy 条目可能缺失 Request；pipeline 关闭时不得因此拒绝绑定）。
func mkLedgerNilRequest(status domain.StyleReviewStatus, digest string) *domain.StyleReviewLedger {
	l := mkLedger(status, digest)
	for i := range l.Cycles {
		l.Cycles[i].Request = nil
	}
	return l
}

func buildCycles(target domain.StyleReviewStatus, digest string) []domain.StyleReviewEntry {
	req := &domain.StyleReviewRequest{Prompt: "p", Model: "m"}
	basis := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revise := &domain.StyleReviewResult{
		Verdict: domain.ReviewVerdictRevise, Evidence: "r",
		Findings: []domain.StyleReviewFinding{
			{Dimension: "pacing", Severity: "warning", Category: "style", Evidence: "s"},
		},
	}
	pass := &domain.StyleReviewResult{Verdict: domain.ReviewVerdictPass, Evidence: "g"}
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
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req, Result: revise},
		}
	case domain.ReviewStatusFinalPending:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req, Result: revise},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
		}
	case domain.ReviewStatusAcceptedInitial:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusAcceptedInitial,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req, Result: pass},
		}
	case domain.ReviewStatusAcceptedRev:
		return []domain.StyleReviewEntry{
			{Cycle: 1, Status: domain.ReviewStatusInitialPending,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 2, Status: domain.ReviewStatusRevisionOpen,
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req, Result: revise},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 4, Status: domain.ReviewStatusAcceptedRev,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req, Result: pass},
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
				AttemptID: "a1", DraftDigest: digest, BasisDigest: basis, Request: req, Result: revise},
			{Cycle: 3, Status: domain.ReviewStatusFinalPending,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req},
			{Cycle: 4, Status: domain.ReviewStatusExhausted,
				AttemptID: "a2", DraftDigest: digest, BasisDigest: basis, Request: req, Result: revise},
		}
	default:
		return nil
	}
}

// assertAllowedSet 断言 allowed 集合与期望完全一致。
func assertAllowedSet(t *testing.T, got []ChapterAction, want ...ChapterAction) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("allowed = %v, want %v", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("allowed = %v, want contains %s", got, w)
		}
	}
}

// assertNextAction 断言 RequiredNextAction 的 action（wantAction 空 = 期望 nil）。
func assertNextAction(t *testing.T, d ChapterStageDecision, wantAction string) {
	t.Helper()
	na := d.RequiredNextAction()
	if wantAction == "" {
		if na != nil {
			t.Fatalf("RequiredNextAction = %+v, want nil", na)
		}
		return
	}
	if na == nil {
		t.Fatalf("RequiredNextAction = nil, want action %q (stage=%s)", wantAction, d.Stage)
	}
	if na.Action != wantAction {
		t.Fatalf("RequiredNextAction.action = %q, want %q; reason=%q", na.Action, wantAction, na.Reason)
	}
	if na.Reason == "" {
		t.Fatal("RequiredNextAction.reason must not be empty")
	}
}

func TestComputeChapterStage(t *testing.T) {
	const (
		critic = domain.StyleQualityCritic
		off    = domain.StyleQualityOff
	)
	d := dig("draft-content")
	d2 := dig("draft-v2")
	final := dig("final-content")
	now := time.Now()
	nowRFC := now.Format(time.RFC3339)

	tests := []struct {
		name       string
		in         ChapterStageInput
		want       ChapterStage
		allowed    []ChapterAction
		required   ChapterAction
		nextAction string // RequiredNextAction().Action；空 = 期望 nil
	}{
		// 1. 新章无 draft → needs_draft
		{
			name:       "new chapter no draft -> needs_draft",
			in:         ChapterStageInput{StyleReviewMode: critic},
			want:       ChapterStageNeedsDraft,
			allowed:    []ChapterAction{ChapterActionDraft},
			required:   ChapterActionDraft,
			nextAction: "draft_chapter",
		},
		// 2. draft 存在无 check → draft_dirty
		{
			name:       "draft exists no check -> draft_dirty",
			in:         ChapterStageInput{StyleReviewMode: critic, DraftExists: true, DraftDigest: d},
			want:       ChapterStageDraftDirty,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionCheck},
			required:   ChapterActionCheck,
			nextAction: "check_consistency",
		},
		// 3. fresh check + mechanical error → needs_edit
		{
			name: "fresh check with mechanical error -> needs_edit",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				HasMechanicalErrors: true, LatestConsistency: consistencyCP(1, d),
			},
			want:       ChapterStageNeedsEdit,
			allowed:    []ChapterAction{ChapterActionDraft},
			required:   ChapterActionDraft,
			nextAction: "draft_chapter",
		},
		// 4. fresh clean check + 无 polish → needs_polish
		{
			name: "fresh clean check without polish -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d, LatestConsistency: consistencyCP(1, d),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 5. fresh polish + consistency seq 较旧 → needs_post_polish_check
		{
			name: "fresh polish with stale consistency seq -> needs_post_polish_check",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(5, d), LatestPolish: polishCP(6, d, "draft", ""),
			},
			want:       ChapterStageNeedsPostPolishCheck,
			allowed:    []ChapterAction{ChapterActionCheck},
			required:   ChapterActionCheck,
			nextAction: "check_consistency",
		},
		// 6. no-op polish 同 digest 新 seq → 仍需 post-polish check
		{
			name: "no-op polish same digest new seq -> needs_post_polish_check",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(7, d), LatestPolish: polishCP(8, d, "draft", ""),
			},
			want:       ChapterStageNeedsPostPolishCheck,
			allowed:    []ChapterAction{ChapterActionCheck},
			required:   ChapterActionCheck,
			nextAction: "check_consistency",
		},
		// 7. post-polish check 完成无 ledger → needs_review
		{
			name: "post-polish check done without ledger -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 8. initial_pending 当前 digest → needs_review
		{
			name: "initial_pending with current digest -> needs_review",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				ReviewLedger: mkLedger(domain.ReviewStatusInitialPending, d),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 9. final_pending 当前 digest → needs_review
		{
			name: "final_pending with current digest -> needs_review",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				ReviewLedger: mkLedger(domain.ReviewStatusFinalPending, d),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 10. pending digest mismatch → blocked
		{
			name: "pending digest mismatch -> blocked",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d2,
				ReviewLedger: mkLedger(domain.ReviewStatusInitialPending, d),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// 11. revision_open digest 未变 → revision_open
		{
			name: "revision_open digest unchanged -> revision_open",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				ReviewLedger: mkLedger(domain.ReviewStatusRevisionOpen, d),
			},
			want:       ChapterStageRevisionOpen,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionEdit},
			required:   ChapterActionEdit,
			nextAction: "edit_chapter",
		},
		// 12. revision_open digest 已变 + check stale → draft_dirty
		{
			name: "revision_open digest changed with stale check -> draft_dirty",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d2,
				ReviewLedger: mkLedger(domain.ReviewStatusRevisionOpen, d),
			},
			want:       ChapterStageDraftDirty,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionEdit, ChapterActionCheck},
			required:   ChapterActionCheck,
			nextAction: "check_consistency",
		},
		// 13. 修订后 check→polish→check → needs_review
		{
			name: "revised then check/polish/check -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				ReviewLedger:      mkLedger(domain.ReviewStatusRevisionOpen, d),
				LatestConsistency: consistencyCP(9, d2), LatestPolish: polishCP(8, d2, "draft", ""),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 14. terminal digest + polish seq 匹配 → needs_commit
		{
			name: "terminal digest with bound polish seq -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				ReviewLedger:      mkLedgerBound(domain.ReviewStatusAcceptedInitial, d, 7),
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 15. rewrite old terminal digest stale → needs_review
		{
			name: "rewrite stale old terminal digest -> needs_review",
			in: ChapterStageInput{
				StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency: consistencyCP(1, d2),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 16. rewrite terminal 当前 digest 匹配 → needs_commit
		{
			name: "rewrite terminal current digest -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:      mkLedgerBound(domain.ReviewStatusAcceptedInitial, d2, 7),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d2, "rewrite", ""),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 17. rewrite draft==final → rewrite_not_started
		{
			name: "rewrite draft equals final -> rewrite_not_started",
			in: ChapterStageInput{
				StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: final, FinalExists: true, FinalDigest: final,
			},
			want:       ChapterStageRewriteNotStarted,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionEdit},
			required:   ChapterActionEdit,
			nextAction: "edit_chapter",
		},
		// 18. exhausted → blocked
		{
			name: "exhausted -> blocked",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				ReviewLedger: mkLedger(domain.ReviewStatusExhausted, d),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// 19. completed 且不在 rewrite queue → complete
		{
			name:       "completed not in rewrite queue -> complete",
			in:         ChapterStageInput{StyleReviewMode: critic, Completed: true},
			want:       ChapterStageComplete,
			nextAction: "",
		},
		// 20. pipeline on + critic off：check→polish→check→commit
		{
			name: "pipeline on critic off full chain -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: off,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 21. pipeline off + critic on：check→review→commit
		{
			name: "pipeline off critic on fresh check -> needs_review",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(1, d),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 22. pipeline off + critic off → disabled
		{
			name:       "pipeline off critic off -> disabled",
			in:         ChapterStageInput{StyleReviewMode: off},
			want:       ChapterStageDisabled,
			nextAction: "",
		},
		// 23a. polish model mismatch（非 terminal）→ needs_polish
		{
			name: "polish model mismatch -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d, ExpectedPolisherModel: "new-model",
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", "old-model"),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 23b. polish model mismatch（terminal 当前候选）→ blocked
		{
			name: "polish model mismatch on terminal candidate -> blocked",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d, ExpectedPolisherModel: "new-model",
				ReviewLedger:      mkLedgerBound(domain.ReviewStatusAcceptedInitial, d, 7),
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", "old-model"),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// 24a. polish stage mismatch（rewrite 队列期望 rewrite，实际 draft）→ needs_polish
		{
			name: "polish stage mismatch in rewrite queue -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d2, "draft", ""),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 24b. polish stage mismatch（非重写队列期望 draft，实际 rewrite）→ needs_polish
		{
			name: "polish stage mismatch outside rewrite queue -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "rewrite", ""),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 24c. degraded polish + digest 匹配 + consistency 未晚于 polish →
		// needs_post_polish_check（degraded 视为合法 polish 记录，不再 needs_polish）
		{
			name: "degraded polish stale consistency -> needs_post_polish_check",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(6, d), LatestPolish: degradedPolishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsPostPolishCheck,
			allowed:    []ChapterAction{ChapterActionCheck},
			required:   ChapterActionCheck,
			nextAction: "check_consistency",
		},
		// 24d. degraded polish + 后续 check 已通过 → needs_review（推进链继续）
		{
			name: "degraded polish fresh post check -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				LatestConsistency: consistencyCP(8, d), LatestPolish: degradedPolishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 24e. degraded polish 跳过 ExpectedPolisherModel 检查（根本没调模型）：
		// 即使模型字段与配置不一致也不判 needs_polish（对照 23a）
		{
			name: "degraded polish skips model check -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d, ExpectedPolisherModel: "new-model",
				LatestConsistency: consistencyCP(8, d), LatestPolish: degradedPolishCP(7, d, "draft", "old-model"),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 24f. degraded polish 的 stage 检查仍然生效（重写队列期望 rewrite，
		// 实际 draft）→ needs_polish（重新精修新候选）
		{
			name: "degraded polish stage mismatch -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				LatestConsistency: consistencyCP(8, d2), LatestPolish: degradedPolishCP(7, d2, "draft", ""),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 24g. degraded polish 的 digest 与当前草稿不匹配（degraded 后草稿又被
		// 修改）→ needs_polish（对"新 digest"重新精修是合法新周期，非死锁）
		{
			name: "degraded polish stale digest -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				LatestConsistency: consistencyCP(8, d2), LatestPolish: degradedPolishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 24h. 端到端：degraded polish + degraded 账本绑定（R==P）→ needs_commit
		{
			name: "degraded polish degraded ledger bound -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				ReviewLedger:      mkLedgerBound(domain.ReviewStatusDegraded, d, 7),
				LatestConsistency: consistencyCP(8, d), LatestPolish: degradedPolishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 25a. legacy review binding（seq==0，同秒）→ needs_commit
		{
			name: "legacy binding same second -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:      mkLedgerAt(domain.ReviewStatusAcceptedInitial, d2, nowRFC),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCPAt(7, d2, "rewrite", now),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 25b. legacy review binding（critic 早于 polish >1s）→ 未绑定 → needs_review
		{
			name: "legacy binding critic too early -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:      mkLedgerAt(domain.ReviewStatusAcceptedInitial, d2, now.Add(-2*time.Second).Format(time.RFC3339)),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCPAt(7, d2, "rewrite", now),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 25c. legacy review binding（时间缺失 → fail-open 视为已绑定，与
		// CheckPolishPipelineGate 语义一致）→ needs_commit
		{
			name: "legacy binding missing timestamp tolerated -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				// mkLedger 的条目 CreatedAt==""（legacy 账本可能缺失时间字段）
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedInitial, d2),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d2, "rewrite", ""),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 附加：pipeline 关闭 + critic 开启 + terminal 周期 Request==nil → needs_commit
		// （pipeline 关闭不要求 polish 绑定，legacy 条目缺失 Request 不得被拒绝）
		{
			name: "pipeline off critic on terminal nil request -> needs_commit",
			in: ChapterStageInput{
				StyleReviewMode: critic, DraftExists: true, DraftDigest: d,
				ReviewLedger:      mkLedgerNilRequest(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency: consistencyCP(8, d),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 附加：rewrite 队列无草稿 → rewrite_not_started（未播种）
		{
			name: "rewrite queue no draft seeded -> rewrite_not_started",
			in: ChapterStageInput{
				StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
			},
			want:       ChapterStageRewriteNotStarted,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionEdit},
			required:   ChapterActionEdit,
			nextAction: "edit_chapter",
		},
		// 附加：degraded 候选未变化（绑定匹配）→ needs_commit
		{
			name: "degraded unchanged candidate -> needs_commit",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d,
				ReviewLedger:      mkLedgerBound(domain.ReviewStatusDegraded, d, 7),
				LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsCommit,
			allowed:    []ChapterAction{ChapterActionCommit},
			required:   ChapterActionCommit,
			nextAction: "commit_chapter",
		},
		// 附加：degraded 候选已变化（digest 不匹配）→ needs_review
		{
			name: "degraded changed candidate -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				ReviewLedger:      mkLedgerBound(domain.ReviewStatusDegraded, d, 7),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d2, "draft", ""),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// 附加：重写队列 + revision_open 已修改 + check 新鲜 + 有 error → needs_edit(edit)
		{
			name: "rewrite revision_open with mechanical error -> needs_edit edit",
			in: ChapterStageInput{
				StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:        mkLedger(domain.ReviewStatusRevisionOpen, d),
				HasMechanicalErrors: true, LatestConsistency: consistencyCP(9, d2),
			},
			want:       ChapterStageNeedsEdit,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionEdit},
			required:   ChapterActionEdit,
			nextAction: "edit_chapter",
		},
		// ── P1-5：terminal ledger 与当前草稿 digest 不匹配（非返工章节）→ blocked ──
		// ch450 死锁根因：accepted_revised + digest 不匹配 + polish 陈旧时，pipeline
		// freshness 分支先返回 needs_polish，FSM 允许 polish_draft 但 mutation guard
		// 对 terminal 锁定拒绝修改 → 模型照 required 调用必被拒，无限重派。
		// 修复后必须早于 pipeline freshness 返回 blocked。
		{
			name: "accepted_revised digest mismatch stale polish -> blocked",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedRev, d),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		{
			name: "accepted_initial digest mismatch stale polish -> blocked",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// overridden 同样是 terminal 锁定（guard 拒绝修改）→ blocked。
		{
			name: "overridden digest mismatch stale polish -> blocked",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				ReviewLedger:      mkLedger(domain.ReviewStatusOverridden, d),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// P1-6：mismatch 必须早于 consistency/机械违规分支——terminal 锁定的章节
		// 返回 draft_dirty/needs_edit（允许 draft/edit）同样会被 guard 拒绝。
		{
			name: "accepted_initial digest mismatch stale consistency -> blocked",
			in: ChapterStageInput{
				StyleReviewMode: critic,
				DraftExists:     true, DraftDigest: d2,
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency: consistencyCP(1, d),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		{
			name: "accepted_initial digest mismatch mech errors -> blocked",
			in: ChapterStageInput{
				StyleReviewMode: critic,
				DraftExists:     true, DraftDigest: d2,
				ReviewLedger:        mkLedger(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency:   consistencyCP(8, d2),
				HasMechanicalErrors: true,
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// 返工章节（InRewriteQueue）terminal mismatch → 行为不变：guard 允许修改，
		// stale polish → needs_polish（合法新周期，不是死锁）。
		{
			name: "rewrite terminal mismatch stale polish -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d, "rewrite", ""),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// 返工章节 + terminal mismatch + polish 新鲜 → 行为不变（needs_review 开新 epoch）。
		{
			name: "rewrite terminal mismatch fresh polish -> needs_review",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				DraftExists: true, DraftDigest: d2, FinalExists: true, FinalDigest: final,
				ReviewLedger:      mkLedger(domain.ReviewStatusAcceptedInitial, d),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d2, "rewrite", ""),
			},
			want:       ChapterStageNeedsReview,
			allowed:    []ChapterAction{ChapterActionReview},
			required:   ChapterActionReview,
			nextAction: "review_style",
		},
		// degraded 不被 P1-5 误伤：degraded 是评审调用故障（非锁定），guard 允许
		// 修改，digest 不匹配 + stale polish → needs_polish（合法新周期）。
		{
			name: "degraded digest mismatch stale polish -> needs_polish",
			in: ChapterStageInput{
				PipelineEnabled: true, StyleReviewMode: critic,
				DraftExists: true, DraftDigest: d2,
				ReviewLedger:      mkLedger(domain.ReviewStatusDegraded, d),
				LatestConsistency: consistencyCP(8, d2), LatestPolish: polishCP(7, d, "draft", ""),
			},
			want:       ChapterStageNeedsPolish,
			allowed:    []ChapterAction{ChapterActionPolish},
			required:   ChapterActionPolish,
			nextAction: "polish_draft",
		},
		// ── 阻塞项 8：非返工 terminal ledger + 草稿丢失 → blocked（guard 因
		// terminal 拒绝 draft_chapter，needs_draft 会破坏 FSM/guard 不变量）──
		{
			name: "terminal ledger no draft -> blocked",
			in: ChapterStageInput{
				StyleReviewMode: critic,
				ReviewLedger:    mkLedger(domain.ReviewStatusAcceptedInitial, d),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		{
			name: "accepted_revised ledger no draft -> blocked",
			in: ChapterStageInput{
				StyleReviewMode: critic,
				ReviewLedger:    mkLedger(domain.ReviewStatusAcceptedRev, d),
			},
			want:       ChapterStageBlocked,
			nextAction: "",
		},
		// 返工队列 terminal + 无草稿 → 保持 rewrite_not_started（guard 允许
		// 重写开始：rewriteNotStarted + terminal → 放行）。
		{
			name: "rewrite terminal no draft -> rewrite_not_started",
			in: ChapterStageInput{
				StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
				ReviewLedger: mkLedger(domain.ReviewStatusAcceptedInitial, d),
			},
			want:       ChapterStageRewriteNotStarted,
			allowed:    []ChapterAction{ChapterActionDraft, ChapterActionEdit},
			required:   ChapterActionEdit,
			nextAction: "edit_chapter",
		},
		// degraded + 无草稿 → 保持允许起草（guard 放行 degraded 的修改）。
		{
			name: "degraded no draft -> needs_draft",
			in: ChapterStageInput{
				StyleReviewMode: critic,
				ReviewLedger:    mkLedger(domain.ReviewStatusDegraded, d),
			},
			want:       ChapterStageNeedsDraft,
			allowed:    []ChapterAction{ChapterActionDraft},
			required:   ChapterActionDraft,
			nextAction: "draft_chapter",
		},
		// 无账本 + 无草稿 → 保持 needs_draft（新章节首次起草不受影响）。
		{
			name: "no ledger no draft -> needs_draft",
			in: ChapterStageInput{
				StyleReviewMode: critic,
			},
			want:       ChapterStageNeedsDraft,
			allowed:    []ChapterAction{ChapterActionDraft},
			required:   ChapterActionDraft,
			nextAction: "draft_chapter",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeChapterStage(tc.in)
			if got.Stage != tc.want {
				t.Fatalf("stage = %s, want %s; reason=%q recovery=%q", got.Stage, tc.want, got.Reason, got.Recovery)
			}
			if tc.allowed != nil {
				assertAllowedSet(t, got.Allowed, tc.allowed...)
			}
			if got.Required != tc.required {
				t.Fatalf("required = %s, want %s", got.Required, tc.required)
			}
			assertNextAction(t, got, tc.nextAction)
			if got.Stage == ChapterStageBlocked && got.Recovery == "" {
				t.Fatal("blocked decision must carry recovery guidance")
			}
		})
	}
}

// ── Allows 判定 ──────────────────────────────────────────────────────

func TestChapterStageDecision_Allows(t *testing.T) {
	d := ComputeChapterStage(ChapterStageInput{
		StyleReviewMode: domain.StyleQualityCritic, DraftExists: true, DraftDigest: dig("x"),
	})
	if !d.Allows(ChapterActionDraft) || !d.Allows(ChapterActionCheck) {
		t.Fatalf("draft_dirty should allow draft+check, got %v", d.Allowed)
	}
	if d.Allows(ChapterActionCommit) || d.Allows(ChapterActionPolish) || d.Allows(ChapterActionReview) {
		t.Fatalf("draft_dirty must not allow commit/polish/review, got %v", d.Allowed)
	}
}

// ── 绑定 helper 语义（规格 §4 一致性） ────────────────────────────────

// TestReviewBindsPolish_MissingTimeFailOpen 验证 reviewBindsPolish 的时间缺失/
// 解析失败 fail-open 语义（与 CheckPolishPipelineGate 的 legacy 回退一致）：
// 无法比较墙钟时按"已绑定"处理，避免旧账本死锁；nil 与超窗仍拒绝。
func TestReviewBindsPolish_MissingTimeFailOpen(t *testing.T) {
	polish := polishCPAt(7, dig("x"), "draft", time.Now())
	entry := &domain.StyleReviewEntry{Cycle: 1, DraftDigest: dig("x")} // CreatedAt==""

	if !reviewBindsPolish(entry, polish) {
		t.Fatal("缺失 CreatedAt 的 legacy 条目应视为已绑定（fail-open）")
	}
	entry.CreatedAt = "not-a-time"
	if !reviewBindsPolish(entry, polish) {
		t.Fatal("无法解析的 CreatedAt 应视为已绑定（fail-open）")
	}
	if reviewBindsPolish(nil, polish) || reviewBindsPolish(entry, nil) {
		t.Fatal("nil 条目/polish 不应视为已绑定")
	}
	// 超窗（>1s）仍拒绝绑定（与 25b 用例一致）
	entry.CreatedAt = time.Now().Add(-2 * time.Second).Format(time.RFC3339)
	if reviewBindsPolish(entry, polish) {
		t.Fatal("critic 早于 polish 超过 1 秒不应视为已绑定")
	}
}

// TestReviewBindingValid_PipelineOffIgnoresNilRequest 直接验证 reviewBindingValid：
// pipeline 关闭时不检查 Request/polish（只要求存在评审周期，旧行为）；pipeline
// 开启时 nil Request 仍拒绝（绑定缺失）。
func TestReviewBindingValid_PipelineOffIgnoresNilRequest(t *testing.T) {
	cycle := &domain.StyleReviewEntry{Cycle: 1, DraftDigest: dig("x")}

	inOff := ChapterStageInput{PipelineEnabled: false}
	if !reviewBindingValid(inOff, cycle) {
		t.Fatal("pipeline off 时 cycle.Request==nil 不得被拒绝")
	}
	if reviewBindingValid(inOff, nil) {
		t.Fatal("pipeline off 时 cycle==nil 仍应拒绝")
	}

	inOn := ChapterStageInput{PipelineEnabled: true, LatestPolish: polishCP(7, dig("x"), "draft", "")}
	if reviewBindingValid(inOn, cycle) {
		t.Fatal("pipeline on 时 cycle.Request==nil 应拒绝（绑定缺失）")
	}
}

// ── RequiredNextAction 边界（disabled/complete/blocked → nil） ────────
func TestRequiredNextAction_NilForTerminalStages(t *testing.T) {
	cases := []ChapterStageInput{
		{StyleReviewMode: domain.StyleQualityOff},                                                                    // disabled
		{StyleReviewMode: domain.StyleQualityCritic, Completed: true},                                                // complete
		{StyleReviewMode: domain.StyleQualityCritic, ReviewLedger: mkLedger(domain.ReviewStatusExhausted, dig("x"))}, // blocked
	}
	for i, in := range cases {
		if na := ComputeChapterStage(in).RequiredNextAction(); na != nil {
			t.Fatalf("case %d: expected nil RequiredNextAction, got %+v", i, na)
		}
	}
}

// ── ChapterTransitionError 消息格式 ──────────────────────────────────

func TestChapterTransitionError_Format(t *testing.T) {
	err := &ChapterTransitionError{
		Chapter:   56,
		Stage:     ChapterStageNeedsPostPolishCheck,
		Attempted: ChapterActionCommit,
		Required:  ChapterActionCheck,
		Allowed:   []ChapterAction{ChapterActionCheck},
		Reason:    "polish 已产生新候选，必须重新检查",
	}
	msg := err.Error()
	for _, want := range []string{
		"code=chapter_fsm_transition_denied", "chapter=56", "stage=needs_post_polish_check",
		"attempted=commit_chapter", "required=check_consistency", "allowed=[check_consistency]",
		"下一步：调用 check_consistency({\"chapter\":56})",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q:\n%s", want, msg)
		}
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("ChapterTransitionError must unwrap to ErrToolPrecondition")
	}

	blocked := &ChapterTransitionError{
		Chapter:   56,
		Stage:     ChapterStageBlocked,
		Attempted: ChapterActionEdit,
		Reason:    "评审已耗尽",
		Recovery:  "先执行 /style-override",
	}
	bmsg := blocked.Error()
	for _, want := range []string{
		"code=chapter_fsm_blocked", "chapter=56", "stage=blocked", "attempted=edit_chapter",
		"required=none", "reason=评审已耗尽", "recovery=先执行 /style-override",
	} {
		if !strings.Contains(bmsg, want) {
			t.Fatalf("blocked message missing %q:\n%s", want, bmsg)
		}
	}
}

// ── ResolveChapterStage / RequireChapterAction 快照加载 ──────────────

func TestRequireChapterAction_DraftDirtyBlocksCommit(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	if err := st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}

	cfg := ChapterFSMConfig{Enabled: true}
	// draft 允许、commit 拒绝。
	if err := RequireChapterAction(st, 1, ChapterActionDraft, cfg); err != nil {
		t.Fatalf("draft should be allowed in draft_dirty: %v", err)
	}
	err := RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	if err == nil {
		t.Fatal("commit must be denied in draft_dirty")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("denial must unwrap to ErrToolPrecondition, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"chapter=1", "stage=draft_dirty", "attempted=commit_chapter", "required=check_consistency",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("denial message missing %q:\n%s", want, msg)
		}
	}

	// cfg.Enabled=false → 不拦截（standalone 旧行为）。
	if err := RequireChapterAction(st, 1, ChapterActionCommit, ChapterFSMConfig{}); err != nil {
		t.Fatalf("disabled FSM must not intercept: %v", err)
	}
}

func TestRequireChapterAction_DisabledWhenOffMode(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityOff}); err != nil {
		t.Fatal(err)
	}
	cfg := ChapterFSMConfig{Enabled: true}
	if err := RequireChapterAction(st, 1, ChapterActionCommit, cfg); err != nil {
		t.Fatalf("off mode + pipeline off must be disabled (no interception): %v", err)
	}
}

func TestResolveChapterStage_StoreReadError(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, st.Dir(), "meta/progress.json", `{corrupt!!!`)

	cfg := ChapterFSMConfig{Enabled: true}
	err := RequireChapterAction(st, 1, ChapterActionDraft, cfg)
	if err == nil {
		t.Fatal("store read error must surface, not fake a stage")
	}
	if !errors.Is(err, errs.ErrStoreRead) {
		t.Fatalf("expected ErrStoreRead in chain, got %v", err)
	}
}

// ── 改动 2：polish 后 edit（digest 变化）的专门 reason ─────────────────

// TestComputeChapterStage_PostPolishEditReason 验证核心纠错场景：polish 后
// writer 又 edit 草稿导致 digest 与最后一次 polish checkpoint 不一致时，
// needs_polish 的 reason 必须给出专门提示（精修后已被修改 + 当前唯一动作是
// polish），而不是笼统的"需要精修当前草稿"，也不得再引导"先 check"（此刻
// check 会被 FSM 拒绝）；对照场景（无 polish/模型不符/阶段不符）不得误报。
func TestComputeChapterStage_PostPolishEditReason(t *testing.T) {
	const critic = domain.StyleQualityCritic
	d := dig("draft-content")     // polish checkpoint 记录的 output digest
	d3 := dig("draft-after-edit") // polish 后又被编辑的当前草稿 digest

	// 核心场景：LatestPolish.OutputDigest(=Digest)=d != 当前草稿 d3。
	in := ChapterStageInput{
		PipelineEnabled: true, StyleReviewMode: critic,
		Chapter: 1, DraftExists: true, DraftDigest: d3,
		LatestConsistency: consistencyCP(8, d3), LatestPolish: polishCP(7, d, "draft", ""),
	}
	got := ComputeChapterStage(in)
	if got.Stage != ChapterStageNeedsPolish {
		t.Fatalf("stage = %s, want needs_polish", got.Stage)
	}
	if !strings.Contains(got.Reason, "精修后已被修改") {
		t.Fatalf("reason 必须标注 polish 后被修改，got %q", got.Reason)
	}
	// 新语义：当前唯一动作 = polish，成功后 check 一次；禁止 edit/commit。
	for _, want := range []string{
		"当前唯一动作：调用 polish_draft(chapter=1)",
		"成功后调用一次 check_consistency",
		"禁止 edit_chapter / commit_chapter",
	} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("reason 必须含 %q，got %q", want, got.Reason)
		}
	}
	// 不得再引导"先 check"（此刻 check 会被 FSM 拒绝，浪费 turn）。
	for _, banned := range []string{"先 check_consistency", "必须重新 check_consistency"} {
		if strings.Contains(got.Reason, banned) {
			t.Fatalf("reason 不得含 %q（不得引导先 check），got %q", banned, got.Reason)
		}
	}
	// RequiredNextAction 传播同一 reason（模型决策依据唯一来源）。
	na := got.RequiredNextAction()
	if na == nil || na.Action != "polish_draft" || !strings.Contains(na.Reason, "精修后已被修改") {
		t.Fatalf("RequiredNextAction 必须携带专门 reason，got %+v", na)
	}

	// 对照 1：尚无 polish 记录（首次精修）→ 常规 reason，不得误报。
	inFirst := ChapterStageInput{
		PipelineEnabled: true, StyleReviewMode: critic,
		DraftExists: true, DraftDigest: d,
		LatestConsistency: consistencyCP(1, d),
	}
	if g := ComputeChapterStage(inFirst); g.Stage != ChapterStageNeedsPolish ||
		strings.Contains(g.Reason, "精修后已被修改") {
		t.Fatalf("首次精修不得误报 post-polish edit：stage=%s reason=%q", g.Stage, g.Reason)
	}

	// 对照 2：digest 匹配但 polisher 模型不符 → needs_polish 且不得误报。
	inModel := ChapterStageInput{
		PipelineEnabled: true, StyleReviewMode: critic,
		DraftExists: true, DraftDigest: d, ExpectedPolisherModel: "new-model",
		LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", "old-model"),
	}
	if g := ComputeChapterStage(inModel); g.Stage != ChapterStageNeedsPolish ||
		strings.Contains(g.Reason, "精修后已被修改") {
		t.Fatalf("模型不符不得误报 post-polish edit：stage=%s reason=%q", g.Stage, g.Reason)
	}

	// 对照 3：digest 匹配但 polish stage 不符（rewrite 队列实际记录 draft）→ 不得误报。
	inStage := ChapterStageInput{
		PipelineEnabled: true, StyleReviewMode: critic, Completed: true, InRewriteQueue: true,
		DraftExists: true, DraftDigest: d, FinalExists: true, FinalDigest: dig("final-content"),
		LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", ""),
	}
	if g := ComputeChapterStage(inStage); g.Stage != ChapterStageNeedsPolish ||
		strings.Contains(g.Reason, "精修后已被修改") {
		t.Fatalf("stage 不符不得误报 post-polish edit：stage=%s reason=%q", g.Stage, g.Reason)
	}
}

// TestComputeChapterStage_PostPolishEditBansEditAndCommit 补充验证：post-polish
// edit 场景的 needs_polish reason 必须明确禁止 edit_chapter / commit_chapter
// （两者正是生产日志中 digest 失效的最大来源），且不允许集只有 polish_draft。
func TestComputeChapterStage_PostPolishEditBansEditAndCommit(t *testing.T) {
	d := dig("draft-content")
	d3 := dig("draft-after-edit")
	in := ChapterStageInput{
		PipelineEnabled: true, StyleReviewMode: domain.StyleQualityCritic,
		Chapter: 1, DraftExists: true, DraftDigest: d3,
		LatestConsistency: consistencyCP(8, d3), LatestPolish: polishCP(7, d, "draft", ""),
	}
	got := ComputeChapterStage(in)
	if got.Stage != ChapterStageNeedsPolish {
		t.Fatalf("stage = %s, want needs_polish", got.Stage)
	}
	for _, banned := range []string{"禁止 edit_chapter", "禁止 edit_chapter / commit_chapter", "commit_chapter"} {
		if !strings.Contains(got.Reason, banned) {
			t.Fatalf("reason 必须明确禁止 %q，got %q", banned, got.Reason)
		}
	}
	// 允许集只有 polish：edit/commit/check/review 全部不在列。
	for _, a := range []ChapterAction{
		ChapterActionEdit, ChapterActionCommit, ChapterActionCheck, ChapterActionReview, ChapterActionDraft,
	} {
		if got.Allows(a) {
			t.Fatalf("needs_polish(post-polish edit) 不得允许 %s，allowed=%v", a, got.Allowed)
		}
	}
	if !got.Allows(ChapterActionPolish) {
		t.Fatalf("needs_polish(post-polish edit) 必须允许 polish_draft，allowed=%v", got.Allowed)
	}
}

// ── 改动 1：action-specific recovery 文案 ─────────────────────────────

// TestChapterTransitionError_ActionSpecificRecovery 验证 recovery 按 required
// action 区分：check/polish/review 给完整调用示例；edit_chapter 必须引导先
// read_chapter 取逐字 old_string；draft_chapter 必须引导先取上下文再给完整正文
// （不得输出缺参的不完整调用示例）。
func TestChapterTransitionError_ActionSpecificRecovery(t *testing.T) {
	base := func(required ChapterAction) *ChapterTransitionError {
		return &ChapterTransitionError{
			Chapter: 3, Stage: ChapterStageNeedsPolish,
			Attempted: ChapterActionCommit, Required: required,
			Allowed: []ChapterAction{required}, Reason: "r",
		}
	}
	// 单参直达工具：完整调用示例。
	for _, a := range []ChapterAction{ChapterActionCheck, ChapterActionPolish, ChapterActionReview} {
		msg := base(a).Error()
		want := fmt.Sprintf("下一步：调用 %s({\"chapter\":3})", a)
		if !strings.Contains(msg, want) {
			t.Fatalf("%s recovery 缺少 %q：\n%s", a, want, msg)
		}
		if !strings.Contains(msg, "required_next_action") {
			t.Fatalf("%s recovery 必须保留 required_next_action 指引：\n%s", a, msg)
		}
	}
	// edit_chapter：先读草稿取逐字 old_string。
	emsg := base(ChapterActionEdit).Error()
	for _, want := range []string{"read_chapter(chapter=3, source='draft')", "old_string", "逐字一致", "edit_chapter"} {
		if !strings.Contains(emsg, want) {
			t.Fatalf("edit recovery 缺少 %q：\n%s", want, emsg)
		}
	}
	if strings.Contains(emsg, "调用 edit_chapter({\"chapter\":3})，然后严格执行") {
		t.Fatalf("edit_chapter 不得给出缺参的不完整调用示例：\n%s", emsg)
	}
	// draft_chapter：先取上下文再提供完整正文。
	dmsg := base(ChapterActionDraft).Error()
	for _, want := range []string{"read_chapter", "novel_context", "draft_chapter", "content", "mode"} {
		if !strings.Contains(dmsg, want) {
			t.Fatalf("draft recovery 缺少 %q：\n%s", want, dmsg)
		}
	}
}

// ── 改动 3：needs_commit reason 携带 commit 参数提示 ──────────────────

// TestComputeChapterStage_NeedsCommitArgsHint 验证 needs_commit 的 reason
// 附带 commit_chapter 必传参数提示，帮助模型一次提交成功。
func TestComputeChapterStage_NeedsCommitArgsHint(t *testing.T) {
	const critic = domain.StyleQualityCritic
	d := dig("draft-content")
	in := ChapterStageInput{
		PipelineEnabled: true, StyleReviewMode: critic,
		DraftExists: true, DraftDigest: d,
		ReviewLedger:      mkLedgerBound(domain.ReviewStatusAcceptedInitial, d, 7),
		LatestConsistency: consistencyCP(8, d), LatestPolish: polishCP(7, d, "draft", ""),
	}
	got := ComputeChapterStage(in)
	if got.Stage != ChapterStageNeedsCommit {
		t.Fatalf("stage = %s, want needs_commit", got.Stage)
	}
	for _, want := range []string{"summary", "characters", "key_events", "world_state_mode"} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("needs_commit reason 缺少 %q：%q", want, got.Reason)
		}
	}
	// RequiredNextAction 传播同一 reason。
	na := got.RequiredNextAction()
	if na == nil || na.Action != "commit_chapter" || !strings.Contains(na.Reason, "world_state_mode") {
		t.Fatalf("RequiredNextAction 必须携带 commit 参数提示，got %+v", na)
	}
}
func TestComputeChapterStage_UnderMinSuggestsAppend(t *testing.T) {
	d := dig("short-draft")
	got := ComputeChapterStage(ChapterStageInput{
		StyleReviewMode:     domain.StyleQualityCritic,
		DraftExists:         true,
		DraftDigest:         d,
		HasMechanicalErrors: true,
		OnlyUnderMinError:   true,
		LatestConsistency:   consistencyCP(1, d),
	})
	if got.Stage != ChapterStageNeedsEdit {
		t.Fatalf("stage = %s, want needs_edit", got.Stage)
	}
	next := got.RequiredNextAction()
	if next == nil || next.Action != ActionDraftChapter || next.Mode != "append" {
		t.Fatalf("required_next_action = %+v, want draft_chapter mode=append", next)
	}
	if !strings.Contains(next.Reason, "mode=append") {
		t.Fatalf("append reason missing: %q", next.Reason)
	}

	rewrite := ComputeChapterStage(ChapterStageInput{
		StyleReviewMode: domain.StyleQualityCritic, DraftExists: true, DraftDigest: d,
		HasMechanicalErrors: true, OnlyUnderMinError: true, InRewriteQueue: true,
		LatestConsistency: consistencyCP(1, d),
	})
	if next := rewrite.RequiredNextAction(); next != nil && next.Mode == "append" {
		t.Fatalf("rewrite queue must not receive append-only mode: %+v", next)
	}
}

// ── FSM 拒绝计数升级（同一章节同一拒绝码连续 ≥2 次追加强制指令） ────────

// TestRequireChapterAction_DeniedCountEscalation 验证拒绝计数升级机制：
// 首次拒绝不升级；连续第 2/3 次拒绝升级并携带计数；不同章节的拒绝不打断
// 本章拒绝链；拒绝码（required）变化打断拒绝链、计数从 1 重新开始；
// code= 前缀保持稳定。
func TestRequireChapterAction_DeniedCountEscalation(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	if err := st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}
	cfg := ChapterFSMConfig{Enabled: true}

	// 第 1 次拒绝：DeniedCount=1，不升级。
	err := RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	if err == nil {
		t.Fatal("commit must be denied in draft_dirty")
	}
	var te *ChapterTransitionError
	if !errors.As(err, &te) || te.DeniedCount != 1 {
		t.Fatalf("first denial DeniedCount = %+v, want 1", te)
	}
	if strings.Contains(err.Error(), "你已被同一原因") {
		t.Fatalf("first denial must not escalate:\n%s", err.Error())
	}

	// 第 2 次拒绝：DeniedCount=2，升级。
	err = RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	if !errors.As(err, &te) || te.DeniedCount != 2 {
		t.Fatalf("second denial DeniedCount = %+v, want 2", te)
	}
	msg := err.Error()
	for _, want := range []string{
		"你已被同一原因连续拒绝 2 次",
		"必须立即调用 check_consistency",
		"不要再次调用 novel_context / read_chapter / check_consistency 获取上下文",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("escalated message missing %q:\n%s", want, msg)
		}
	}
	if !strings.HasPrefix(msg, "code=chapter_fsm_transition_denied") {
		t.Fatalf("code prefix must stay stable:\n%s", msg)
	}

	// 第 3 次拒绝：DeniedCount=3。
	err = RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	if !errors.As(err, &te) || te.DeniedCount != 3 {
		t.Fatalf("third denial DeniedCount = %+v, want 3", te)
	}
	if !strings.Contains(err.Error(), "连续拒绝 3 次") {
		t.Fatalf("third denial must escalate with count 3:\n%s", err.Error())
	}

	// 不同章节的拒绝（不同 key）打断本章拒绝链：严格"连续"语义——
	// 中间夹了其它章节的拒绝，本章计数从 1 重新开始。
	if err := st.Drafts.SaveDraft(2, "# 二\nabc她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}
	if err := RequireChapterAction(st, 2, ChapterActionCommit, cfg); err == nil {
		t.Fatal("chapter 2 commit must be denied")
	}
	err = RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	if !errors.As(err, &te) || te.DeniedCount != 1 {
		t.Fatalf("denial after other-chapter denial DeniedCount = %+v, want 1 (chain broken)", te)
	}
	if strings.Contains(err.Error(), "你已被同一原因") {
		t.Fatalf("chain-broken denial must not escalate:\n%s", err.Error())
	}

	// 拒绝码（required）变化打断拒绝链：追加匹配 digest 的 consistency
	// checkpoint → 阶段推进到 needs_review（required=review_style），
	// 新拒绝码计数从 1 重新开始、不升级。
	draft, _ := st.Drafts.LoadDraft(1)
	digest := domain.DigestDraft(draft)
	if _, err := st.Checkpoints.Append(domain.ChapterScope(1), "consistency_check", "c1", digest); err != nil {
		t.Fatal(err)
	}
	err = RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	if !errors.As(err, &te) || te.DeniedCount != 1 {
		t.Fatalf("denial with new required must restart count, DeniedCount = %+v, want 1", te)
	}
	if strings.Contains(err.Error(), "你已被同一原因") {
		t.Fatalf("new denial code must not escalate:\n%s", err.Error())
	}
}

// TestRequireChapterAction_DeniedCountIgnoresOtherRecords 验证引擎旁路记录
// （worker_failure/plan_start 等非 chapter_fsm_denied 审计）不打断拒绝链——
// 生产流程中两次 FSM 拒绝之间必然夹着失败裁定等记录，若它们打断链，
// 升级机制将永远无法触发。
func TestRequireChapterAction_DeniedCountIgnoresOtherRecords(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	if err := st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatal(err)
	}
	cfg := ChapterFSMConfig{Enabled: true}

	// 第 1 次拒绝。
	if err := RequireChapterAction(st, 1, ChapterActionCommit, cfg); err == nil {
		t.Fatal("commit must be denied")
	}
	// 引擎在两次拒绝之间插入的旁路审计（worker_failure 裁定等）。
	if _, err := st.Decisions.Append(store.DecisionRecord{
		Kind: "worker_failure", Decider: "arbiter", Input: "writer: 写第 1 章", Reason: "重试",
	}); err != nil {
		t.Fatal(err)
	}
	// 第 2 次拒绝：旁路记录不打断链 → 升级触发。
	err := RequireChapterAction(st, 1, ChapterActionCommit, cfg)
	var te *ChapterTransitionError
	if !errors.As(err, &te) || te.DeniedCount != 2 {
		t.Fatalf("DeniedCount = %+v, want 2 (interleaved records must not break chain)", te)
	}
	if !strings.Contains(err.Error(), "你已被同一原因连续拒绝 2 次") {
		t.Fatalf("escalation must trigger despite interleaved records:\n%s", err.Error())
	}
}

// TestRequireChapterAction_DeniedCountRewriteQueue 复现生产死循环场景：
// 重写队列章节（rewrite_not_started，required=edit_chapter）反复调用
// check_consistency 被拒 → 连续 ≥2 次后错误升级为强制调用 edit_chapter。
func TestRequireChapterAction_DeniedCountRewriteQueue(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("重写测试", 10); err != nil {
		t.Fatal(err)
	}
	final := "原终稿。她心里骂自己丢人，真不要脸。"
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveDraft(1, final); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.SetPendingRewrites([]int{1}, "重写测试"); err != nil {
		t.Fatal(err)
	}
	cfg := ChapterFSMConfig{Enabled: true}

	// 第 1 次：check_consistency 被拒（required=edit_chapter），不升级。
	err := RequireChapterAction(st, 1, ChapterActionCheck, cfg)
	if err == nil {
		t.Fatal("check_consistency must be denied in rewrite_not_started")
	}
	var te *ChapterTransitionError
	if !errors.As(err, &te) || te.Required != ChapterActionEdit || te.DeniedCount != 1 {
		t.Fatalf("first denial = %+v, want required=edit_chapter DeniedCount=1", te)
	}
	if strings.Contains(err.Error(), "你已被同一原因") {
		t.Fatalf("first denial must not escalate:\n%s", err.Error())
	}

	// 第 2 次：升级，强制调用 edit_chapter。
	err = RequireChapterAction(st, 1, ChapterActionCheck, cfg)
	if !errors.As(err, &te) || te.DeniedCount != 2 {
		t.Fatalf("second denial DeniedCount = %+v, want 2", te)
	}
	msg := err.Error()
	for _, want := range []string{
		"你已被同一原因连续拒绝 2 次",
		"必须立即调用 edit_chapter",
		"不要再次调用 novel_context / read_chapter / check_consistency 获取上下文",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("escalated message missing %q:\n%s", want, msg)
		}
	}
	if !strings.HasPrefix(msg, "code=chapter_fsm_transition_denied") {
		t.Fatalf("code prefix must stay stable:\n%s", msg)
	}
}
