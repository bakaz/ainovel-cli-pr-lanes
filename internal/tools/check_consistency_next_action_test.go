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

// ── 集成测试：check_consistency 输出的 required_next_action 与 FSM 一致 ──
// required_next_action 的唯一来源是 (ChapterStageDecision) RequiredNextAction()
// （规格第 11 节）。以下场景验证"建议字段"与 FSM 判定一致：
// disabled/blocked → 字段缺省；其余阶段 → 字段存在且 action 正确。

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

// critic off + pipeline 关 → disabled → required_next_action 缺省。
func TestInteg_CriticOff_NoPipeline_FieldAbsent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityOff}))
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	if _, ok := m["required_next_action"]; ok {
		t.Fatal("expected absent required_next_action when FSM disabled (off mode + pipeline off)")
	}
}

func TestInteg_CriticOn_NoLedger_FieldPresent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	savePermissiveUserRules(t, st)
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Drafts.SaveDraft(1, "# 一\nabc她心里骂自己丢人，真不要脸。"))
	m := mustUnmarshal(t, mustExecute(t, st, 1))
	assertPresentAction(t, m, ActionReviewStyle)
}

func TestInteg_UnderMin_FieldSuggestsAppend(t *testing.T) {
	st := store.NewStore(t.TempDir())
	snap := rules.BuildSnapshot([]rules.Candidate{
		rules.SystemDefaults(),
		{Source: "test", Structured: rules.Structured{
			ChapterWords: &rules.WordRange{Min: 3000, Max: 6000},
		}},
	})
	must(t, st.UserRules.Save(&snap))
	must(t, st.RunMeta.Save(domain.RunMeta{StyleReviewMode: domain.StyleQualityCritic}))
	must(t, st.Drafts.SaveDraft(1, "# 一\n她走到窗前，心里骂自己丢人，真不要脸。"))

	m := mustUnmarshal(t, mustExecute(t, st, 1))
	action, ok := m["required_next_action"].(map[string]any)
	if !ok {
		t.Fatalf("expected append required_next_action, got %v", m)
	}
	if action["action"] != ActionDraftChapter || action["mode"] != "append" {
		t.Fatalf("required_next_action = %v, want draft_chapter mode=append", action)
	}
	guidance, ok := m["word_count_guidance"].(map[string]any)
	if !ok || guidance["status"] != "under_min" || guidance["recommended_mode"] != "append" {
		t.Fatalf("word_count_guidance = %v, want under_min/append", m["word_count_guidance"])
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
	assertPresentAction(t, m, ActionCommitChapter)
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
	assertPresentAction(t, m, ActionEditChapter)
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

// 机械 error + 已终审候选 → blocked（禁止静默改写）→ 字段缺省。
// 注意：新 FSM 不允许 hasErrors→nil 的旧语义——非终态候选的机械 error 会
// 建议 draft/edit（见 chapter_stage_test.go 用例 3）。
func TestInteg_TerminalWithErrors_FieldAbsent(t *testing.T) {
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
		t.Fatal("expected absent required_next_action when terminal candidate has mechanical errors (blocked)")
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
	assertPresentAction(t, m, ActionReviewStyle)
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
	assertPresentAction(t, m, ActionEditChapter)
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

// assertPresentAction 断言 required_next_action 存在且 action 为期望值。
func assertPresentAction(t *testing.T, m map[string]any, wantAction string) {
	t.Helper()
	action, ok := m["required_next_action"].(map[string]any)
	if !ok {
		t.Fatalf("expected required_next_action, got %v", m["required_next_action"])
	}
	if action["action"] != wantAction {
		t.Fatalf("required_next_action.action = %v, want %s", action["action"], wantAction)
	}
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
