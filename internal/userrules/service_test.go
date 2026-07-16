package userrules

import (
	"errors"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// nil 模型 + 空规则目录：归一化全降级，但快照仍可产出（system_defaults 兜底）并落盘。
// LoadOptions{} 的两个目录为空串，RawFileSources 返回 nil，测试不触碰真实磁盘。
func newDegradedService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st := store.NewStore(t.TempDir())
	return NewService(st, nil, rules.LoadOptions{}), st
}

func TestService_Build_DegradesButPersists(t *testing.T) {
	svc, st := newDegradedService(t)

	snap, err := svc.Build(t.Context(), "每章1200字，主角冷静克制")
	if err != nil {
		t.Fatalf("Build 不应报错（降级而非阻断）：%v", err)
	}
	if snap.Status != rules.StatusDegraded {
		t.Fatalf("无模型应降级，status=%q", snap.Status)
	}
	// system_defaults 始终兜底机械基线。
	if snap.Structured.ChapterWords == nil || snap.Structured.ChapterWords.Min != 3000 {
		t.Fatalf("应保留 system_defaults 字数基线，got %+v", snap.Structured.ChapterWords)
	}
	// 启动 prompt 降级为 raw preferences，原文不丢。
	if snap.Preferences.AllText() == "" {
		t.Fatal("降级应把启动 prompt 原文记入 preferences")
	}

	// 已落盘：GetOrBuild 读回同一份而非重建。
	reloaded, err := st.UserRules.Load()
	if err != nil || reloaded == nil {
		t.Fatalf("快照应已落盘：err=%v snap=%v", err, reloaded)
	}
	if reloaded.Preferences.AllText() != snap.Preferences.AllText() {
		t.Fatal("落盘内容与返回不一致")
	}
}

func TestService_GetOrBuild_LazyForOldBook(t *testing.T) {
	svc, st := newDegradedService(t)

	if cur, _ := st.UserRules.Load(); cur != nil {
		t.Fatal("初始应无快照")
	}
	snap, err := svc.GetOrBuild(t.Context())
	if err != nil {
		t.Fatalf("GetOrBuild 不应报错：%v", err)
	}
	if snap.Structured.ChapterWords == nil {
		t.Fatal("惰性生成应含 system_defaults")
	}
	if cur, _ := st.UserRules.Load(); cur == nil {
		t.Fatal("GetOrBuild 应顺带落盘")
	}
}

func TestService_AddRuntimeRule_PersistsAndReturnsCandidate(t *testing.T) {
	svc, st := newDegradedService(t)

	const text = "以后少用比喻"
	merged, cand, err := svc.AddRuntimeRule(t.Context(), text)
	if err != nil {
		t.Fatalf("AddRuntimeRule 不应报错：%v", err)
	}
	// 候选用于回显：无模型时降级，原文进 preferences。
	if !cand.Degraded {
		t.Fatal("无模型时本次候选应降级")
	}
	if cand.Preferences != text {
		t.Fatalf("候选应保留原文，got %q", cand.Preferences)
	}
	// 叠加后快照含该条且已落盘。
	if merged.Preferences.AllText() == "" {
		t.Fatal("叠加后 preferences 不应为空")
	}
	reloaded, err := st.UserRules.Load()
	if err != nil || reloaded == nil {
		t.Fatalf("叠加后应落盘：err=%v", err)
	}
	if reloaded.Status != rules.StatusDegraded {
		t.Fatalf("含降级来源，status 应为 degraded，got %q", reloaded.Status)
	}
}

func TestService_ValidatedPreferencePatches(t *testing.T) {
	svc, _ := newDegradedService(t)
	added, err := svc.ApplyPatch(t.Context(), RulePatch{Action: PatchAdd, Text: "少用比喻", Scope: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added.Snapshot.Preferences.Writer) != 1 {
		t.Fatalf("应进入 writer bucket: %+v", added.Snapshot.Preferences)
	}
	id := added.Snapshot.Preferences.Writer[0].ID

	moved, err := svc.ApplyPatch(t.Context(), RulePatch{Action: PatchReclassify, RuleID: id, Scope: "architect"})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Snapshot.Preferences.Writer) != 0 || len(moved.Snapshot.Preferences.Architect) != 1 {
		t.Fatalf("reclassify failed: %+v", moved.Snapshot.Preferences)
	}

	revised, err := svc.ApplyPatch(t.Context(), RulePatch{Action: PatchRevise, RuleID: id, Text: "规划时避免重复升级", Scope: "architect"})
	if err != nil {
		t.Fatal(err)
	}
	if got := revised.Snapshot.Preferences.Architect; len(got) != 1 || got[0].Text != "规划时避免重复升级" || got[0].ID == id {
		t.Fatalf("revise failed: %+v", got)
	}
	newID := revised.Snapshot.Preferences.Architect[0].ID
	removed, err := svc.ApplyPatch(t.Context(), RulePatch{Action: PatchRemove, RuleID: newID})
	if err != nil {
		t.Fatal(err)
	}
	if removed.Snapshot.Preferences.AllText() != "" {
		t.Fatalf("remove failed: %+v", removed.Snapshot.Preferences)
	}
	if _, err := svc.ApplyPatch(t.Context(), RulePatch{Action: PatchRemove, RuleID: "missing"}); !errors.Is(err, ErrInvalidPatch) {
		t.Fatalf("missing id 应是 validated patch error, got %v", err)
	}
}

func TestForceCandidateScopeCollapsesNormalizerBuckets(t *testing.T) {
	cand := rules.Candidate{ScopedPreferences: map[rules.RuleScope]string{
		rules.ScopeDefault: "共同规则",
		rules.ScopeWriter:  "正文规则",
	}}
	got := forceCandidateScope(cand, rules.ScopeEditor)
	if got.Scope != rules.ScopeEditor || got.ScopedPreferences != nil || !strings.Contains(got.Preferences, "共同规则") || !strings.Contains(got.Preferences, "正文规则") {
		t.Fatalf("forced scope failed: %+v", got)
	}
}
