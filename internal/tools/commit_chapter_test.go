package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestCommitChapterSchemaDescribesFeedbackAsObject(t *testing.T) {
	st := store.NewStore(t.TempDir())
	t.Cleanup(st.Close)
	tool := NewCommitChapterTool(st)
	schema := tool.Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties missing: %#v", schema["properties"])
	}
	feedback, ok := props["feedback"].(map[string]any)
	if !ok {
		t.Fatalf("feedback schema missing: %#v", props["feedback"])
	}
	desc, _ := feedback["description"].(string)
	if !strings.Contains(desc, "JSON object") || !strings.Contains(desc, "字符串化 JSON") {
		t.Fatalf("feedback description should warn against stringified JSON, got %q", desc)
	}
	if got := feedback["type"]; got != "object" {
		t.Fatalf("feedback type = %v, want object", got)
	}
	finalTitle, ok := props["final_title"].(map[string]any)
	if !ok {
		t.Fatalf("final_title schema missing: %#v", props["final_title"])
	}
	if got := finalTitle["type"]; got != "string" {
		t.Fatalf("final_title type = %v, want string", got)
	}
}

func TestCommitChapterRejectsNonPendingRewrite(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := store.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := store.Progress.MarkChapterComplete(2, 3000, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := store.Progress.SetPendingRewrites([]int{2}, "测试重写"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := store.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	if err := store.Drafts.SaveDraft(3, "这是错误章节的正文。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewCommitChapterTool(store)
	args, err := json.Marshal(map[string]any{
		"chapter":         3,
		"summary":         "错误提交",
		"characters":      []string{"主角"},
		"key_events":      []string{"误提交"},
		"timeline_events": []any{},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected commit to be rejected during rewrite flow")
	}

	if _, err := os.Stat(dir + "/chapters/03.md"); !os.IsNotExist(err) {
		t.Fatalf("chapter should not be persisted, stat err=%v", err)
	}

	progress, err := store.Progress.Load()
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if len(progress.CompletedChapters) != 1 || progress.CompletedChapters[0] != 2 {
		t.Fatalf("completed chapters should only contain original chapter 2, got %v", progress.CompletedChapters)
	}
	if progress.CurrentChapter != 3 {
		t.Fatalf("current chapter should not advance beyond original progress, got %d", progress.CurrentChapter)
	}
}

func TestCommitChapterAllowsPendingRewrite(t *testing.T) {
	dir := t.TempDir()
	store := store.NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := store.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := store.Progress.MarkChapterComplete(2, 3000, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := store.Progress.SetPendingRewrites([]int{2}, "测试重写"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := store.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	if err := store.Drafts.SaveDraft(2, "这是正确待重写章节的正文。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewCommitChapterTool(store)
	args, err := json.Marshal(map[string]any{
		"chapter":          2,
		"summary":          "正确提交",
		"characters":       []string{"主角"},
		"key_events":       []string{"完成重写"},
		"world_state_mode": "preserve",
		"timeline_events":  []any{},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if _, err := os.Stat(dir + "/chapters/02.md"); err != nil {
		t.Fatalf("chapter should be persisted: %v", err)
	}

	progress, err := store.Progress.Load()
	if err != nil {
		t.Fatalf("LoadProgress: %v", err)
	}
	if len(progress.CompletedChapters) != 1 || progress.CompletedChapters[0] != 2 {
		t.Fatalf("unexpected completed chapters: %v", progress.CompletedChapters)
	}
	pending, err := store.Signals.LoadPendingCommit()
	if err != nil {
		t.Fatalf("LoadPendingCommit: %v", err)
	}
	if pending != nil {
		t.Fatalf("expected pending commit cleared, got %+v", pending)
	}
}

func TestCommitChapterFinalTitleNormalAndLegacyRepeat(t *testing.T) {
	s := preflightStore(t)
	t.Cleanup(s.Close)
	tool := NewCommitChapterTool(s)

	raw, err := tool.Execute(context.Background(), preflightCommitArgsJSON(map[string]any{
		"final_title": "  雨夜归人  ",
	}))
	if err != nil {
		t.Fatalf("Execute with final_title: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal result: %v", err)
	}
	if got := out["final_title"]; got != "雨夜归人" {
		t.Fatalf("result final_title = %v, want 雨夜归人", got)
	}
	if got, err := s.ChapterTitles.Load(1); err != nil || got != "雨夜归人" {
		t.Fatalf("stored final_title = %q, err=%v, want 雨夜归人", got, err)
	}

	// 旧调用不带 final_title；重复提交只能返回并保留已存事实。
	raw, err = tool.Execute(context.Background(), preflightCommitArgsJSON(nil))
	if err != nil {
		t.Fatalf("legacy repeated Execute: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal repeated result: %v", err)
	}
	if got := out["final_title"]; got != "雨夜归人" {
		t.Fatalf("repeated result final_title = %v, want 雨夜归人", got)
	}
}

func TestCommitChapterFinalTitleValidationHasNoSideEffects(t *testing.T) {
	cases := []struct {
		name  string
		title string
	}{
		{name: "whitespace", title: " \t\n "},
		{name: "overlong unicode", title: strings.Repeat("章", maxFinalTitleRunes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := preflightStore(t)
			t.Cleanup(s.Close)
			before := captureWorldSnapshot(t, s)
			_, err := NewCommitChapterTool(s).Execute(context.Background(), preflightCommitArgsJSON(map[string]any{
				"final_title": tc.title,
			}))
			if err == nil {
				t.Fatal("invalid final_title must be rejected")
			}
			if !errors.Is(err, errs.ErrToolArgs) {
				t.Fatalf("invalid final_title should be ErrToolArgs, got %v", err)
			}
			assertZeroSideEffects(t, s, before)
			if got, loadErr := s.ChapterTitles.Load(1); loadErr != nil || got != "" {
				t.Fatalf("invalid final_title must not persist title, got %q, err=%v", got, loadErr)
			}
		})
	}
}

func TestCommitChapterRewriteFinalTitleUpdatesAndKeepsOnEmpty(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	s := rewriteModeStore(t, final, "第一次重写正文。她心里骂自己丢人，真不要脸。")
	t.Cleanup(s.Close)
	if err := s.ChapterTitles.Save(2, "旧标题"); err != nil {
		t.Fatalf("seed chapter title: %v", err)
	}
	tool := NewCommitChapterTool(s)

	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写摘要", "characters": []string{"主角"}, "key_events": []string{"重写"},
		"world_state_mode": "preserve", "final_title": "新标题",
	})
	raw, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("rewrite with final_title: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal rewrite result: %v", err)
	}
	if got := out["final_title"]; got != "新标题" {
		t.Fatalf("rewrite result final_title = %v, want 新标题", got)
	}
	if got, err := s.ChapterTitles.Load(2); err != nil || got != "新标题" {
		t.Fatalf("rewritten stored title = %q, err=%v, want 新标题", got, err)
	}

	if err := s.Progress.SetPendingRewrites([]int{2}, "再次重写"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, "第二次重写正文。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft second rewrite: %v", err)
	}
	args, _ = json.Marshal(map[string]any{
		"chapter": 2, "summary": "再次重写摘要", "characters": []string{"主角"}, "key_events": []string{"再次重写"},
		"world_state_mode": "preserve", "final_title": "",
	})
	raw, err = tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("rewrite with empty final_title: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal empty-title rewrite result: %v", err)
	}
	if got := out["final_title"]; got != "新标题" {
		t.Fatalf("empty-title rewrite result = %v, want 新标题", got)
	}
	if got, err := s.ChapterTitles.Load(2); err != nil || got != "新标题" {
		t.Fatalf("empty-title rewrite stored title = %q, err=%v, want 新标题", got, err)
	}
}

// TestCommitChapterUpdatesCastLedger 验证：commit_chapter 把本章 characters 累加进 cast_ledger，
// cast_intros 提供的 brief_role 被采用，且 characters.json 中的核心角色不进入 ledger。
func TestCommitChapterUpdatesCastLedger(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	// 设定核心角色档案（这些不应进 cast_ledger）
	if err := s.Characters.Save([]domain.Character{
		{Name: "林墨", Role: "主角", Tier: "core"},
		{Name: "李清砚", Role: "导师", Tier: "important"},
	}); err != nil {
		t.Fatalf("Save core characters: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, "第一章正文，林墨遇到客栈老板老周与小厮阿云。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    1,
		"summary":    "林墨入住客栈",
		"characters": []string{"林墨", "李清砚", "老周", "阿云"},
		"key_events": []string{"入住"},
		"cast_intros": []any{
			map[string]any{"name": "老周", "brief_role": "客栈老板"},
			map[string]any{"name": "阿云", "brief_role": "客栈小厮"},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	entries, err := s.Cast.Load()
	if err != nil {
		t.Fatalf("Cast.Load: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 ledger entries (老周/阿云), got %d: %+v", len(entries), entries)
	}
	byName := map[string]domain.CastEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName["老周"]; !ok || e.BriefRole != "客栈老板" || e.FirstSeenChapter != 1 {
		t.Errorf("老周 entry wrong: %+v", e)
	}
	if e, ok := byName["阿云"]; !ok || e.BriefRole != "客栈小厮" || e.AppearanceCount != 1 {
		t.Errorf("阿云 entry wrong: %+v", e)
	}
	if _, ok := byName["林墨"]; ok {
		t.Errorf("核心角色 林墨 不应进 ledger")
	}
	if _, ok := byName["李清砚"]; ok {
		t.Errorf("核心角色 李清砚 不应进 ledger")
	}
}

func TestCommitChapterReplayAfterPartialCommitDoesNotDuplicateWorldState(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, "第一章正文，林墨遇到黑影并突破。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	timeline := []domain.TimelineEvent{{
		Chapter:    1,
		Time:       "清晨",
		Event:      "林墨遇到黑影",
		Characters: []string{"林墨"},
	}}
	stateChanges := []domain.StateChange{{
		Chapter:  1,
		Entity:   "林墨",
		Field:    "realm",
		OldValue: "凡人",
		NewValue: "练气期",
	}}
	foreshadow := []domain.ForeshadowUpdate{{
		ID:          "f1",
		Action:      "plant",
		Description: "黑影身份",
		Horizon:     "book",
	}}
	charState := []domain.CharacterStateUpdate{{
		Entity: "林墨", Field: "status.realm", Value: "练气期", Reason: "突破",
	}}

	// 模拟 commit_chapter 已写入世界状态，但尚未 MarkChapterComplete 时进程崩溃。
	if err := s.World.AppendTimelineEvents(timeline); err != nil {
		t.Fatalf("AppendTimelineEvents seed: %v", err)
	}
	if err := s.World.AppendStateChanges(stateChanges); err != nil {
		t.Fatalf("AppendStateChanges seed: %v", err)
	}
	if err := s.World.UpdateForeshadow(1, foreshadow); err != nil {
		t.Fatalf("UpdateForeshadow seed: %v", err)
	}
	if err := s.World.UpsertCharacterState(1, charState); err != nil {
		t.Fatalf("UpsertCharacterState seed: %v", err)
	}
	if err := s.Signals.SavePendingCommit(domain.PendingCommit{
		Chapter: 1,
		Stage:   domain.CommitStageStateApplied,
		Summary: "半提交摘要",
	}); err != nil {
		t.Fatalf("SavePendingCommit: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":                 1,
		"summary":                 "林墨遇到黑影并突破",
		"characters":              []string{"林墨"},
		"key_events":              []string{"遇到黑影", "突破"},
		"timeline_events":         timeline,
		"state_changes":           stateChanges,
		"foreshadow_updates":      foreshadow,
		"character_state_updates": charState,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute replay: %v", err)
	}

	events, _ := s.World.LoadTimeline()
	if len(events) != 1 {
		t.Fatalf("timeline duplicated after replay, got %d: %+v", len(events), events)
	}
	changes, _ := s.World.LoadStateChanges()
	// 预期 2 条：手工 seed 的 realm + UpsertCharacterState seed 派生的 status.realm；
	// 重放后不得再新增（realm 走 AppendStateChanges 去重、status.realm 走同值跳过派生）。
	if len(changes) != 2 {
		t.Fatalf("state changes duplicated after replay, got %d: %+v", len(changes), changes)
	}
	realmCount := 0
	for _, c := range changes {
		if c.Field == "status.realm" {
			realmCount++
		}
	}
	if realmCount != 1 {
		t.Fatalf("character state derived change duplicated after replay, got %+v", changes)
	}
	ledger, _ := s.World.LoadForeshadowLedger()
	if len(ledger) != 1 {
		t.Fatalf("foreshadow duplicated after replay, got %d: %+v", len(ledger), ledger)
	}
	// character_state 重放幂等：同值 upsert 不重复、派生 state_changes 不重复
	entries, _ := s.World.LoadCharacterState()
	if len(entries) != 1 || entries[0].Value != "练气期" {
		t.Fatalf("character state after replay wrong: %+v", entries)
	}
	pending, _ := s.Signals.LoadPendingCommit()
	if pending != nil {
		t.Fatalf("pending commit should be cleared, got %+v", pending)
	}
	if cp := s.Checkpoints.LatestByStep(domain.ChapterScope(1), "commit"); cp == nil {
		t.Fatal("commit checkpoint should be written")
	}
}

// TestCommitChapterRejectsPolishWithoutDraftChange 验证：已完成章节进入打磨/重写队列后，
// 若 writer 跳过 draft_chapter 直接 commit（drafts 与 chapters 内容完全相同），
// commit_chapter 必须拒绝，强制 writer 先调 draft_chapter 写入新版本。
// TestCommitChapterNonLayeredRecompletesAfterRework 验证非分层书完本后经 reopen 返工，
// 改完章节 commit、队列排空时能自动重新回到 complete（补 drain 后判完结的非分层分支）。
func TestCommitChapterNonLayeredRecompletesAfterRework(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 2); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 两章写完并完结。第 2 章备齐 drafts/chapters，供返工提交。
	ch2 := "第二章原始正文，用于模拟已提交终稿。她心里骂自己丢人，真不要脸。"
	if err := s.Drafts.SaveDraft(2, ch2); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(2, ch2); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(1, 100, "", ""); err != nil {
		t.Fatalf("MarkChapterComplete(1): %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, len([]rune(ch2)), "", ""); err != nil {
		t.Fatalf("MarkChapterComplete(2): %v", err)
	}
	if err := s.Progress.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// reopen 第 2 章 → phase 回 writing、PendingRewrites=[2]、flow=rewriting
	if err := s.Progress.Reopen([]int{2}, "返工"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// 返工提交（草稿需与终稿不同才放行）
	if err := s.Drafts.SaveDraft(2, ch2+"\n\n返工新增段落。"); err != nil {
		t.Fatalf("SaveDraft (reworked): %v", err)
	}
	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":          2,
		"summary":          "返工后摘要",
		"characters":       []string{"主角"},
		"key_events":       []string{"清理"},
		"world_state_mode": "preserve",
	})
	raw, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute rework commit: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if payload["book_complete"] != true {
		t.Errorf("book_complete = %v, want true", payload["book_complete"])
	}

	p, _ := s.Progress.Load()
	if p.Phase != domain.PhaseComplete {
		t.Errorf("phase = %s, want complete (应自动重新收尾)", p.Phase)
	}
	if len(p.PendingRewrites) != 0 {
		t.Errorf("PendingRewrites = %v, want empty", p.PendingRewrites)
	}
}

// TestCommitChapterLayeredReopenRecompletesDespiteOpenThread 验证收口：分层书经 reopen
// 返工后，即便 compass 仍有未收束长线（返工可能扰动），排空后也按"结构完整"重新完结——
// 不卡在 writing，杜绝终卷末越界续写死循环（§6.5 / known_outline_exhaustion 家族）。
// 反证：若 reopen 路径仍用质量级 layeredBookComplete，本例 open thread 会让其返 false、
// book_complete 为假，测试即失败。
func TestCommitChapterLayeredReopenRecompletesDespiteOpenThread(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 单卷单弧两章，全部展开
	foundation := NewSaveFoundationTool(s, testContract)
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "卷一", "theme": "主题",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{
					{"title": "首章", "core_event": "起", "hook": "续"},
					{"title": "次章", "core_event": "承", "hook": "终"},
				},
			}},
		}},
		"scale": "long",
	})
	if _, err := foundation.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}

	// 两章写完落盘并完结
	ch2 := "第二章原始正文，模拟已提交终稿。她心里骂自己丢人，真不要脸。"
	for ch, body := range map[int]string{1: "第一章正文。她心里骂自己丢人，真不要脸。", 2: ch2} {
		if err := s.Drafts.SaveDraft(ch, body); err != nil {
			t.Fatalf("SaveDraft %d: %v", ch, err)
		}
		if err := s.Drafts.SaveFinalChapter(ch, body); err != nil {
			t.Fatalf("SaveFinalChapter %d: %v", ch, err)
		}
		if err := s.Progress.MarkChapterComplete(ch, len([]rune(body)), "", ""); err != nil {
			t.Fatalf("MarkChapterComplete %d: %v", ch, err)
		}
	}
	if err := s.Progress.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// 模拟"返工扰动了长线"：compass 仍有未收束的 open thread
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "主角归乡", OpenThreads: []string{"宿敌未除"}}}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}

	// reopen 第 2 章 → 返工提交（草稿需与终稿不同才放行）
	if err := s.Progress.Reopen([]int{2}, "返工"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, ch2+"\n\n返工新增段落。"); err != nil {
		t.Fatalf("SaveDraft reworked: %v", err)
	}
	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "返工摘要", "characters": []string{"主角"}, "key_events": []string{"清理"},
		"world_state_mode": "preserve",
	})
	raw, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute rework commit: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if bc, _ := out["book_complete"].(bool); !bc {
		t.Error("reopen 返工排空后应按结构完整重新完结（即便长线未收束）")
	}
	p, _ := s.Progress.Load()
	if p.Phase != domain.PhaseComplete {
		t.Errorf("phase = %s, want complete", p.Phase)
	}
	if p.ReopenedFromComplete {
		t.Error("重新完结后 ReopenedFromComplete 应被清除")
	}
}

func TestCommitChapterRejectsPolishWithoutDraftChange(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 模拟第 2 章已正常完成：drafts 与 chapters 内容相同。
	original := "第二章原始正文内容，用于模拟已提交终稿。她心里骂自己丢人，真不要脸。"
	if err := s.Drafts.SaveDraft(2, original); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(2, original); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, len([]rune(original)), "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}

	// 进入打磨队列：Flow=Polishing, PendingRewrites=[2]
	if err := s.Progress.SetPendingRewrites([]int{2}, "测试打磨"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.Progress.SetFlow(domain.FlowPolishing); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":          2,
		"summary":          "假装打磨了",
		"characters":       []string{"主角"},
		"key_events":       []string{"无改动"},
		"world_state_mode": "preserve",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected commit to be rejected when drafts equals final content")
	}

	// 再写一版不同的草稿 → 应该通过
	polished := original + "\n\n打磨后新增段落。"
	if err := s.Drafts.SaveDraft(2, polished); err != nil {
		t.Fatalf("SaveDraft (polished): %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute after real polish: %v", err)
	}
}

// TestCommitChapterLayeredRejectsOutOfRangeChapter 验证分层模式下，
// 章号越出 layered_outline 的 commit 必须硬失败，而不是 slog.Warn 放行。
// 这是阻止"裁定误判后 writer 一路裸跑"的物理刹车（《凡骨》ch204..347 案例）。
func TestCommitChapterLayeredRejectsOutOfRangeChapter(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 建一份 layered_outline，只有 1 卷 1 弧 1 章
	foundation := NewSaveFoundationTool(s, testContract)
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "卷一", "theme": "主题",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{
					{"title": "首章", "core_event": "起", "hook": "续"},
				},
			}},
		}},
		"scale": "long",
	})
	if _, err := foundation.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhaseWriting)

	// 越界章节 2 的 commit 必须硬失败
	if err := s.Drafts.SaveDraft(2, "越界章节正文，必须被拦下。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter":    2,
		"summary":    "越界章节",
		"characters": []string{"主角"},
		"key_events": []string{"不该被允许"},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected commit to fail when chapter out of layered outline range")
	}

	// 章节文件不应落盘、Progress 不应推进
	if _, statErr := os.Stat(dir + "/chapters/02.md"); !os.IsNotExist(statErr) {
		t.Fatalf("chapter 2 should not be persisted, stat err=%v", statErr)
	}
	progress, _ := s.Progress.Load()
	if len(progress.CompletedChapters) != 0 {
		t.Fatalf("CompletedChapters should stay empty, got %v", progress.CompletedChapters)
	}
}

// TestCommitChapterLayeredAutoCompletesWhenDone 验证分层模式确定性完结兜底：
// 大纲全部展开并写完 + 无骨架弧 + 无返工 + 活跃伏笔为零 + 指南针长线收束时，
// 最后一章 commit 自动推 Phase=Complete，不依赖架构师主动调 complete_book。
// 这是 9bf26a5 删掉分层自动完结后引入的 livelock 的修复（终卷末尾模型既不 append
// 也不 complete → 写手裸跑越界死循环）。
func TestCommitChapterLayeredAutoCompletesWhenDone(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	// 单卷单弧两章，全部展开（无骨架弧）
	foundation := NewSaveFoundationTool(s, testContract)
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "卷一", "theme": "主题",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{
					{"title": "首章", "core_event": "起", "hook": "续"},
					{"title": "次章", "core_event": "承", "hook": "终"},
				},
			}},
		}},
		"scale": "long",
	})
	if _, err := foundation.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}
	// 指南针长线已收束（OpenThreads 空）
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "主角归乡"}}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhaseWriting)

	tool := NewCommitChapterTool(s)
	commit := func(ch int) map[string]any {
		if err := s.Drafts.SaveDraft(ch, fmt.Sprintf("第 %d 章正文内容，用于测试确定性完结。她心里骂自己丢人，真不要脸。", ch)); err != nil {
			t.Fatalf("SaveDraft %d: %v", ch, err)
		}
		args, _ := json.Marshal(map[string]any{
			"chapter": ch, "summary": "摘要", "characters": []string{"主角"}, "key_events": []string{"事件"},
		})
		raw, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute ch%d: %v", ch, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("Unmarshal ch%d: %v", ch, err)
		}
		return out
	}

	// 第 1 章：未写完，不应完结
	if bc, _ := commit(1)["book_complete"].(bool); bc {
		t.Fatal("写完第 1 章不应触发完结")
	}
	if p, _ := s.Progress.Load(); p.Phase == domain.PhaseComplete {
		t.Fatal("写完第 1 章 phase 不应为 complete")
	}

	// 第 2 章（最后一章）：应自动完结
	if bc, _ := commit(2)["book_complete"].(bool); !bc {
		t.Fatal("写完最后一章应自动完结")
	}
	if p, _ := s.Progress.Load(); p.Phase != domain.PhaseComplete {
		t.Fatalf("expected phase=complete, got %s", p.Phase)
	}
}

// TestCommitChapterFinaleVolumeCompletesDespiteOpenThreads 验证收官卷全链路：
// 已宣告收官卷（append_volume 带 final:true）后——
//  1. 末章 commit 不完结：完结不抢在卷末收尾三连（弧评审/弧摘要/卷摘要）之前，
//     结局必须过 editor 质量闸；
//  2. 三连齐备、卷摘要落盘（save_volume_summary 触发点）即完结，不再要求
//     伏笔/长线双归零——否则 estimated_scale 高估的书永远无法合法完本。
//
// 与下方 NoAutoCompleteWithOpenThreads 互为对照：同样带未收长线，未宣告不完结、已宣告完结。
func TestCommitChapterFinaleVolumeCompletesDespiteOpenThreads(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	foundation := NewSaveFoundationTool(s, testContract)
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "卷一", "theme": "主题",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{{"title": "首章", "core_event": "起", "hook": "续"}},
			}},
		}},
		"scale": "long",
	})
	if _, err := foundation.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}

	// 卷末宣告收官卷：append_volume 带 final:true
	appendArgs, _ := json.Marshal(map[string]any{
		"type":   "append_volume",
		"reason": "长线可在一卷内收完，宣告收官卷",
		"content": map[string]any{
			"index": 2, "title": "终卷", "theme": "收束", "final": true,
			"arcs": []map[string]any{{
				"index": 1, "title": "收官弧", "goal": "回收所有长线",
				"chapters": []map[string]any{{"title": "终章", "core_event": "合", "hook": "终"}},
			}},
		},
	})
	raw, err := foundation.Execute(context.Background(), appendArgs)
	if err != nil {
		t.Fatalf("Execute append_volume: %v", err)
	}
	var appendOut map[string]any
	if err := json.Unmarshal(raw, &appendOut); err != nil {
		t.Fatalf("Unmarshal append result: %v", err)
	}
	if appendOut["final_volume"] != true {
		t.Fatalf("append_volume 应返回 final_volume=true 事实, got %v", appendOut)
	}

	// 长线未收束（未宣告时这会阻止完结，见对照测试）
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "主角归乡", OpenThreads: []string{"宿敌未除"}}}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhaseWriting)

	tool := NewCommitChapterTool(s)
	commit := func(ch int) map[string]any {
		if err := s.Drafts.SaveDraft(ch, fmt.Sprintf("第 %d 章正文内容，用于收官卷完结测试。她心里骂自己丢人，真不要脸。", ch)); err != nil {
			t.Fatalf("SaveDraft %d: %v", ch, err)
		}
		args, _ := json.Marshal(map[string]any{
			"chapter": ch, "summary": "摘要", "characters": []string{"主角"}, "key_events": []string{"事件"},
		})
		raw, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute ch%d: %v", ch, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("Unmarshal ch%d: %v", ch, err)
		}
		return out
	}

	// 第 1 章（非终卷末章）：不应完结
	if bc, _ := commit(1)["book_complete"].(bool); bc {
		t.Fatal("收官卷尚未写完不应完结")
	}
	// 第 2 章（收官卷末章）：卷末收尾三连未齐，完结不得抢在 editor 评审/摘要之前
	if bc, _ := commit(2)["book_complete"].(bool); bc {
		t.Fatal("末章 commit 时三连未齐，不应完结")
	}
	if p, _ := s.Progress.Load(); p.Phase == domain.PhaseComplete {
		t.Fatal("完结不应发生在卷末评审与摘要之前")
	}

	// 卷末收尾三连：弧评审 + 弧摘要落盘后，卷摘要（save_volume_summary）是完结触发点
	if err := s.World.SaveReview(domain.ReviewEntry{Chapter: 2, Scope: "arc", Verdict: "accept", Summary: "末弧评审"}); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}
	if err := s.Summaries.SaveArcSummary(domain.ArcSummary{Volume: 2, Arc: 1, Title: "收官弧", Summary: "收束", KeyEvents: []string{"终局"}}); err != nil {
		t.Fatalf("SaveArcSummary: %v", err)
	}
	volTool := NewSaveVolumeSummaryTool(s)
	volArgs, _ := json.Marshal(map[string]any{
		"volume": 2, "title": "终卷", "summary": "全卷收束", "key_events": []string{"终局"},
	})
	volRaw, err := volTool.Execute(context.Background(), volArgs)
	if err != nil {
		t.Fatalf("Execute save_volume_summary: %v", err)
	}
	var volOut map[string]any
	if err := json.Unmarshal(volRaw, &volOut); err != nil {
		t.Fatalf("Unmarshal volume summary result: %v", err)
	}
	if volOut["book_complete"] != true {
		t.Fatalf("卷摘要落盘应触发收官完结并回显 book_complete, got %v", volOut)
	}
	if p, _ := s.Progress.Load(); p.Phase != domain.PhaseComplete {
		t.Fatalf("expected phase=complete, got %s", p.Phase)
	}
}

// TestCommitChapterFinaleSkeletonArcBlocksCompletion 验证收官完结的结构闸门：
// 收官卷仍有骨架弧（规划内容未写）时，即使三连齐备也不得完结——这是防止
// "过早完结"的唯一防线（layeredStructurallyComplete 条件 2）。
func TestCommitChapterFinaleSkeletonArcBlocksCompletion(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	foundation := NewSaveFoundationTool(s, testContract)
	// 收官卷：第一弧展开 1 章，第二弧仍是骨架
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "终卷", "theme": "收束", "final": true,
			"arcs": []map[string]any{
				{"index": 1, "title": "收官弧", "goal": "收线",
					"chapters": []map[string]any{{"title": "首章", "core_event": "起", "hook": "续"}}},
				{"index": 2, "title": "骨架弧", "goal": "待展开", "estimated_chapters": 5},
			},
		}},
		"scale": "long",
	})
	if _, err := foundation.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "归乡"}}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhaseWriting)

	tool := NewCommitChapterTool(s)
	if err := s.Drafts.SaveDraft(1, "第一章正文。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "摘要", "characters": []string{"主角"}, "key_events": []string{"事件"},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 三连齐备也不放行：骨架弧意味着规划内容还没写
	if err := s.World.SaveReview(domain.ReviewEntry{Chapter: 1, Scope: "arc", Verdict: "accept", Summary: "弧评审"}); err != nil {
		t.Fatalf("SaveReview: %v", err)
	}
	if err := s.Summaries.SaveArcSummary(domain.ArcSummary{Volume: 1, Arc: 1, Title: "收官弧", Summary: "s", KeyEvents: []string{"e"}}); err != nil {
		t.Fatalf("SaveArcSummary: %v", err)
	}
	volTool := NewSaveVolumeSummaryTool(s)
	volArgs, _ := json.Marshal(map[string]any{
		"volume": 1, "title": "终卷", "summary": "s", "key_events": []string{"e"},
	})
	volRaw, err := volTool.Execute(context.Background(), volArgs)
	if err != nil {
		t.Fatalf("Execute save_volume_summary: %v", err)
	}
	var volOut map[string]any
	_ = json.Unmarshal(volRaw, &volOut)
	if volOut["book_complete"] == true {
		t.Fatal("收官卷仍有骨架弧时不得完结")
	}
	if p, _ := s.Progress.Load(); p.Phase == domain.PhaseComplete {
		t.Fatal("骨架弧未展开，phase 不应为 complete")
	}
}

// TestCommitChapterLayeredNoAutoCompleteWithOpenThreads 验证保守性：仍有活跃长线时
// 即使章节写满也不自动完结，把"是否继续"的裁定权留给架构师。

// TestCommitChapterLayeredNoAutoCompleteWithOpenThreads 验证保守性：仍有活跃长线时
// 即使章节写满也不自动完结，把"是否继续"的裁定权留给架构师。
func TestCommitChapterLayeredNoAutoCompleteWithOpenThreads(t *testing.T) {
	dir := t.TempDir()
	s := store.NewStore(dir)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 0); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}

	foundation := NewSaveFoundationTool(s, testContract)
	layeredArgs, _ := json.Marshal(map[string]any{
		"type": "layered_outline",
		"content": []map[string]any{{
			"index": 1, "title": "卷一", "theme": "主题",
			"arcs": []map[string]any{{
				"index": 1, "title": "弧一", "goal": "目标",
				"chapters": []map[string]any{{"title": "首章", "core_event": "起", "hook": "续"}},
			}},
		}},
		"scale": "long",
	})
	if _, err := foundation.Execute(context.Background(), layeredArgs); err != nil {
		t.Fatalf("Execute layered: %v", err)
	}
	// 仍有未收束的活跃长线
	if err := s.Outline.SaveCompass(domain.StoryCompass{Long: domain.LongCompass{EndingDirection: "主角归乡", OpenThreads: []string{"宿敌未除"}}}); err != nil {
		t.Fatalf("SaveCompass: %v", err)
	}
	_ = s.Progress.UpdatePhase(domain.PhaseWriting)

	if err := s.Drafts.SaveDraft(1, "唯一一章的正文，但长线未收束。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 1, "summary": "摘要", "characters": []string{"主角"}, "key_events": []string{"事件"},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if p, _ := s.Progress.Load(); p.Phase == domain.PhaseComplete {
		t.Fatal("活跃长线未收束时不应自动完结")
	}
}

// ── 批次 4：重写提交 world_state_mode 安全闸门 ───────────────────────────
//
// 背景：剧情级重写（PendingRewrites 路径）曾静默丢弃 5 组世界状态变更
// （TimelineEvents/ForeshadowUpdates/RelationshipChanges/StateChanges），
// 正文与世界账本永久失配且无报错。本批次强制重写提交显式声明
// world_state_mode（preserve/replace）：
//   - 缺失或非法值 → Precondition 拒绝（不写终稿、不 drain 队列）；
//   - preserve → 现有 executeRewriteCommit 行为（5 组世界状态变更一律不应用）；
//   - replace → 无可重放历史时显式拒绝（世界状态重放能力尚未就绪）。

// rewriteModeStore 构造重写队列基础 store：第 2 章已完成并入队，草稿已改为
// draftText（≠ 终稿 finalText）。
func rewriteModeStore(t *testing.T, finalText, draftText string) *store.Store {
	t.Helper()
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, finalText); err != nil {
		t.Fatalf("SaveDraft(final): %v", err)
	}
	if err := s.Drafts.SaveFinalChapter(2, finalText); err != nil {
		t.Fatalf("SaveFinalChapter: %v", err)
	}
	if err := s.Progress.MarkChapterComplete(2, len([]rune(finalText)), "mystery", "quest"); err != nil {
		t.Fatalf("MarkChapterComplete: %v", err)
	}
	if err := s.Progress.SetPendingRewrites([]int{2}, "测试重写"); err != nil {
		t.Fatalf("SetPendingRewrites: %v", err)
	}
	if err := s.Progress.SetFlow(domain.FlowRewriting); err != nil {
		t.Fatalf("SetFlow: %v", err)
	}
	if err := s.Drafts.SaveDraft(2, draftText); err != nil {
		t.Fatalf("SaveDraft(rework): %v", err)
	}
	return s
}

// TestCommitChapterRewritePreserveKeepsWorldState 验证 preserve 重写提交后
// 四类世界账本（Timeline/Foreshadow/Relationship/State）一律不更新——
// 即使模型在参数里附带 5 组世界状态字段（纯文风重写模型可能产出的噪声）。
func TestCommitChapterRewritePreserveKeepsWorldState(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	s := rewriteModeStore(t, final, "返工后的草稿。她心里骂自己丢人，真不要脸。")

	// 世界账本已有第 2 章的原始记录（章节首次提交时写入）。
	seededTimeline := []domain.TimelineEvent{{Chapter: 2, Time: "午后", Event: "林墨遇袭", Characters: []string{"林墨"}}}
	seededState := []domain.StateChange{{Chapter: 2, Entity: "林墨", Field: "境界", OldValue: "凡人", NewValue: "练气"}}
	seededForeshadow := []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影身份", Horizon: "book"}}
	seededRelationships := []domain.RelationshipEntry{{CharacterA: "林墨", CharacterB: "李清砚", Relation: "师从", Chapter: 2}}
	if err := s.World.AppendTimelineEvents(seededTimeline); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}
	if err := s.World.AppendStateChanges(seededState); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := s.World.UpdateForeshadow(2, seededForeshadow); err != nil {
		t.Fatalf("seed foreshadow: %v", err)
	}
	if err := s.World.UpdateRelationships(seededRelationships); err != nil {
		t.Fatalf("seed relationships: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写摘要", "characters": []string{"林墨"}, "key_events": []string{"重写"},
		"world_state_mode":     "preserve",
		"timeline_events":      []domain.TimelineEvent{{Chapter: 2, Time: "子夜", Event: "重写后事件", Characters: []string{"林墨"}}},
		"state_changes":        []domain.StateChange{{Chapter: 2, Entity: "林墨", Field: "境界", OldValue: "练气", NewValue: "金丹"}},
		"foreshadow_updates":   []domain.ForeshadowUpdate{{ID: "f2", Action: "plant", Description: "重写伏笔", Horizon: "book"}},
		"relationship_changes": []domain.RelationshipEntry{{CharacterA: "林墨", CharacterB: "李清砚", Relation: "决裂", Chapter: 2}},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute preserve rewrite: %v", err)
	}

	events, _ := s.World.LoadTimeline()
	if len(events) != 1 || events[0].Event != "林墨遇袭" {
		t.Fatalf("preserve 后 timeline 应保持原状，got %+v", events)
	}
	changes, _ := s.World.LoadStateChanges()
	if len(changes) != 1 || changes[0].NewValue != "练气" {
		t.Fatalf("preserve 后 state changes 应保持原状，got %+v", changes)
	}
	ledger, _ := s.World.LoadForeshadowLedger()
	if len(ledger) != 1 || ledger[0].ID != "f1" {
		t.Fatalf("preserve 后 foreshadow 账本应保持原状，got %+v", ledger)
	}
	rels, _ := s.World.LoadRelationships()
	if len(rels) != 1 || rels[0].Relation != "师从" {
		t.Fatalf("preserve 后 relationship 账本应保持原状，got %+v", rels)
	}
	progress, _ := s.Progress.Load()
	if len(progress.PendingRewrites) != 0 {
		t.Fatalf("preserve 提交应 drain 队列，got %v", progress.PendingRewrites)
	}
}

// TestCommitChapterRewriteMissingModeRejected 验证缺失 world_state_mode 的
// 重写提交被 Precondition 拒绝：不写终稿、不 drain PendingRewrites、不切 flow。
func TestCommitChapterRewriteMissingModeRejected(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	s := rewriteModeStore(t, final, "返工后的草稿。她心里骂自己丢人，真不要脸。")

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写摘要", "characters": []string{"主角"}, "key_events": []string{"重写"},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("缺失 world_state_mode 的重写提交必须被拒绝")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("应为 Precondition 错误，got %v", err)
	}
	if !strings.Contains(err.Error(), "world_state_mode") || !strings.Contains(err.Error(), "第 2 章") {
		t.Fatalf("错误消息应包含 chapter 与 world_state_mode，got %v", err)
	}
	if text, _ := s.Drafts.LoadChapterText(2); text != final {
		t.Fatalf("拒绝后终稿不应被覆盖，got %q", text)
	}
	progress, _ := s.Progress.Load()
	if len(progress.PendingRewrites) != 1 || progress.PendingRewrites[0] != 2 {
		t.Fatalf("拒绝后 PendingRewrites 应保持 [2]，got %v", progress.PendingRewrites)
	}
	if progress.Flow != domain.FlowRewriting {
		t.Fatalf("拒绝后 flow 应保持 rewriting，got %s", progress.Flow)
	}
}

// TestCommitChapterRewriteInvalidModeRejected 验证非法 world_state_mode 值
// 被 Precondition 拒绝（消息含 mode 值），且无任何写入。
func TestCommitChapterRewriteInvalidModeRejected(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	s := rewriteModeStore(t, final, "返工后的草稿。她心里骂自己丢人，真不要脸。")

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写摘要", "characters": []string{"主角"}, "key_events": []string{"重写"},
		"world_state_mode": "merge",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("非法 world_state_mode 的重写提交必须被拒绝")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("应为 Precondition 错误，got %v", err)
	}
	if !strings.Contains(err.Error(), `world_state_mode="merge"`) {
		t.Fatalf("错误消息应包含 world_state_mode 值，got %v", err)
	}
	if text, _ := s.Drafts.LoadChapterText(2); text != final {
		t.Fatalf("拒绝后终稿不应被覆盖，got %q", text)
	}
	if progress, _ := s.Progress.Load(); len(progress.PendingRewrites) != 1 {
		t.Fatalf("拒绝后 PendingRewrites 应保持 [2]，got %v", progress.PendingRewrites)
	}
}

// TestCommitChapterRewriteMissingModeZeroSideEffects 验证缺失 world_state_mode 的
// 重写提交失败时零副作用（发布阻断 2）：不写入 rule_violations（literary gate
// 未执行）、不追加 commit checkpoint、不清除残留 PendingCommit（恢复信号保持
// 原样）。回归：mode 校验曾位于 literary gate 与 PendingCommit 清理之后
// （commit_chapter.go 183-188），缺失 mode 时已产生部分写入；修复后校验提前
// 到一切写副作用之前。
func TestCommitChapterRewriteMissingModeZeroSideEffects(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	// 草稿用 dirtyProse：若 literary gate 先于 mode 校验执行，会把命中句落盘。
	s := rewriteModeStore(t, final, dirtyProse)
	// 残留 PendingCommit：若 mode 校验被推迟到 IsChapterCompleted 分支之后，
	// 会被清除并追加 commit checkpoint。
	if err := s.Signals.SavePendingCommit(domain.PendingCommit{
		Chapter: 2, Stage: domain.CommitStageProgressMarked, Summary: "半提交摘要",
	}); err != nil {
		t.Fatal(err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写摘要", "characters": []string{"主角"}, "key_events": []string{"重写"},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("缺失 world_state_mode 的重写提交必须被拒绝")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("应为 Precondition 错误，got %v", err)
	}
	if !strings.Contains(err.Error(), "world_state_mode") {
		t.Fatalf("错误消息应包含 world_state_mode，got %v", err)
	}

	// 零副作用断言
	if v := s.World.LoadRuleViolations(2); len(v) != 0 {
		t.Fatalf("mode 缺失失败不得写入 rule_violations，got %+v", v)
	}
	if cp := s.Checkpoints.LatestByStep(domain.ChapterScope(2), "commit"); cp != nil {
		t.Fatal("mode 缺失失败不得追加 commit checkpoint")
	}
	if pending, _ := s.Signals.LoadPendingCommit(); pending == nil || pending.Chapter != 2 {
		t.Fatal("mode 缺失失败不得清除残留 PendingCommit（恢复信号应保持原样）")
	}
	if text, _ := s.Drafts.LoadChapterText(2); text != final {
		t.Fatalf("拒绝后终稿不应被覆盖，got %q", text)
	}
	progress, _ := s.Progress.Load()
	if len(progress.PendingRewrites) != 1 || progress.PendingRewrites[0] != 2 {
		t.Fatalf("拒绝后 PendingRewrites 应保持 [2]，got %v", progress.PendingRewrites)
	}
}

// TestCommitChapterRewriteReplaceRejected 验证 world_state_mode="replace" 在
// 无可重放历史时被显式拒绝：不产生部分写入（终稿不变、队列不 drain、
// 时间线不被替换/追加）。
func TestCommitChapterRewriteReplaceRejected(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	s := rewriteModeStore(t, final, "返工后的草稿。她心里骂自己丢人，真不要脸。")

	// 重写前账本已有第 2 章的原始记录。
	seededTimeline := []domain.TimelineEvent{{Chapter: 2, Time: "午后", Event: "林墨遇袭", Characters: []string{"林墨"}}}
	if err := s.World.AppendTimelineEvents(seededTimeline); err != nil {
		t.Fatalf("seed timeline: %v", err)
	}

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "剧情重写摘要", "characters": []string{"林墨"}, "key_events": []string{"剧情变化"},
		"world_state_mode":     "replace",
		"timeline_events":      []domain.TimelineEvent{{Chapter: 2, Time: "子夜", Event: "重写后事件", Characters: []string{"林墨"}}},
		"state_changes":        []domain.StateChange{{Chapter: 2, Entity: "林墨", Field: "境界", OldValue: "练气", NewValue: "金丹"}},
		"foreshadow_updates":   []domain.ForeshadowUpdate{{ID: "f2", Action: "plant", Description: "重写伏笔", Horizon: "book"}},
		"relationship_changes": []domain.RelationshipEntry{{CharacterA: "林墨", CharacterB: "李清砚", Relation: "决裂", Chapter: 2}},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("replace（无可重放历史）必须被显式拒绝")
	}
	if !errors.Is(err, errs.ErrToolPrecondition) {
		t.Fatalf("应为 Precondition 错误，got %v", err)
	}
	for _, want := range []string{"第 2 章", `world_state_mode="replace"`, "重放", "preserve"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误消息应包含 %q，got %v", want, err)
		}
	}

	// 不产生部分写入：终稿不变、队列不 drain、时间线不被替换/追加。
	if text, _ := s.Drafts.LoadChapterText(2); text != final {
		t.Fatalf("拒绝后终稿不应被覆盖，got %q", text)
	}
	if progress, _ := s.Progress.Load(); len(progress.PendingRewrites) != 1 {
		t.Fatalf("拒绝后 PendingRewrites 应保持 [2]，got %v", progress.PendingRewrites)
	}
	events, _ := s.World.LoadTimeline()
	if len(events) != 1 || events[0].Event != "林墨遇袭" {
		t.Fatalf("拒绝后 timeline 不得被替换或追加，got %+v", events)
	}
}

// ── 批次 4b：foreshadow/character_state preflight（commit 新通道）─────────
//
// preflight 是 commit_chapter 新增的写入前参数闸门：foreshadow 语义校验
// （含 evidence 草稿引文、未知 ID、冲突 action）+ character_state 校验 +
// 双写冲突检测。任一失败整体拒绝，pending/终稿/摘要/账本全部零变化。

// preflightStore 构造 preflight 测试基础 store：第 1 章草稿已写入，
// 草稿正文包含引文 "黑影一闪而过"（供 advance/resolve 的 evidence 校验）。
func preflightStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 10); err != nil {
		t.Fatalf("InitProgress: %v", err)
	}
	if err := s.Drafts.SaveDraft(1, "第一章正文。黑影一闪而过，林墨警觉起来。她心里骂自己丢人，真不要脸。"); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	return s
}

// seedPlantedF1 预埋已 planted 的伏笔 f1（advance/resolve 用例需要）。
func seedPlantedF1(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{
		ID: "f1", Action: "plant", Description: "黑影身份", Horizon: "book",
	}}); err != nil {
		t.Fatalf("seed foreshadow f1: %v", err)
	}
}

// seedForeshadowF1 把伏笔 f1 推进到指定状态（planted/advanced/resolved/retired）。
func seedForeshadowF1(t *testing.T, s *store.Store, state string) {
	t.Helper()
	seedPlantedF1(t, s)
	switch state {
	case "planted":
	case "advanced":
		if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{
			ID: "f1", Action: "advance", Evidence: "黑影一闪而过",
		}}); err != nil {
			t.Fatalf("seed advanced f1: %v", err)
		}
	case "resolved":
		if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{
			ID: "f1", Action: "resolve", Evidence: "黑影一闪而过",
		}}); err != nil {
			t.Fatalf("seed resolved f1: %v", err)
		}
	case "retired":
		if err := s.World.UpdateForeshadow(1, []domain.ForeshadowUpdate{{
			ID: "f1", Action: "retire", Reason: "弃线",
		}}); err != nil {
			t.Fatalf("seed retired f1: %v", err)
		}
	default:
		t.Fatalf("unknown state %q", state)
	}
}

// preflightCommitArgsJSON 构造 commit_chapter 参数（chapter=1 + 最小必填 + extra 合并）。
func preflightCommitArgsJSON(extra map[string]any) json.RawMessage {
	args := map[string]any{
		"chapter":    1,
		"summary":    "测试提交",
		"characters": []string{"林墨"},
		"key_events": []string{"测试"},
	}
	for k, v := range extra {
		args[k] = v
	}
	raw, _ := json.Marshal(args)
	return raw
}

// captureWorldSnapshot 捕获提交前的世界账本快照（伏笔/角色状态/state_changes）。
func captureWorldSnapshot(t *testing.T, s *store.Store) map[string]any {
	t.Helper()
	ledger, err := s.World.LoadForeshadowLedger()
	if err != nil {
		t.Fatalf("LoadForeshadowLedger: %v", err)
	}
	charState, err := s.World.LoadCharacterState()
	if err != nil {
		t.Fatalf("LoadCharacterState: %v", err)
	}
	changes, err := s.World.LoadStateChanges()
	if err != nil {
		t.Fatalf("LoadStateChanges: %v", err)
	}
	return map[string]any{"ledger": ledger, "character_state": charState, "state_changes": changes}
}

// assertZeroSideEffects 断言 commit 失败后所有写入目标零变化。
func assertZeroSideEffects(t *testing.T, s *store.Store, before map[string]any) {
	t.Helper()
	if pending, _ := s.Signals.LoadPendingCommit(); pending != nil {
		t.Fatalf("失败后不得留下 pending commit: %+v", pending)
	}
	if text, _ := s.Drafts.LoadChapterText(1); text != "" {
		t.Fatalf("失败后不得写终稿: %q", text)
	}
	if sum, _ := s.Summaries.LoadSummary(1); sum != nil {
		t.Fatalf("失败后不得写摘要: %+v", sum)
	}
	after := captureWorldSnapshot(t, s)
	for _, key := range []string{"ledger", "character_state", "state_changes"} {
		if !reflect.DeepEqual(after[key], before[key]) {
			t.Fatalf("失败后 %s 发生变化: before=%+v after=%+v", key, before[key], after[key])
		}
	}
	if progress, _ := s.Progress.Load(); len(progress.CompletedChapters) != 0 {
		t.Fatalf("失败后不得推进进度: %+v", progress.CompletedChapters)
	}
}

// TestCommitChapterPreflightRejectsWithZeroSideEffects 表驱动验证 preflight
// 各类失败（缺 horizon/缺 evidence/未知 ID/evidence 不在草稿/非法 character_state/
// 双写冲突/同 ID 冲突 action）都整体拒绝且零副作用。
func TestCommitChapterPreflightRejectsWithZeroSideEffects(t *testing.T) {
	cases := []struct {
		name string
		seed func(*testing.T, *store.Store)
		// commitArgs 的 extra（不传则只提交最小必填）
		extra map[string]any
		want  string // 错误消息必须包含的关键子串
	}{
		{
			name: "plant 缺 horizon",
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "黑影身份"}},
			},
			want: "horizon",
		},
		{
			name: "advance 缺 evidence",
			seed: seedPlantedF1,
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "f1", Action: "advance"}},
			},
			want: "requires evidence",
		},
		{
			name: "advance 未知 ID",
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "ghost", Action: "advance", Evidence: "黑影一闪而过"}},
			},
			want: "不存在",
		},
		{
			name: "evidence 不在草稿",
			seed: seedPlantedF1,
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "f1", Action: "advance", Evidence: "正文中没有的引文"}},
			},
			want: "未在正文草稿中出现",
		},
		{
			name: "character_state 非法 field",
			extra: map[string]any{
				"character_state_updates": []domain.CharacterStateUpdate{{Entity: "林墨", Field: "freeform", Value: "x"}},
			},
			want: "受控命名空间",
		},
		{
			name: "双写冲突",
			extra: map[string]any{
				"character_state_updates": []domain.CharacterStateUpdate{{Entity: "林墨", Field: "status.realm", Value: "练气期"}},
				"state_changes":           []domain.StateChange{{Entity: "林墨", Field: "status.realm", OldValue: "凡人", NewValue: "练气期"}},
			},
			want: "双写冲突",
		},
		{
			name: "同 ID 冲突 action",
			seed: seedPlantedF1,
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{
					{ID: "f1", Action: "advance", Evidence: "黑影一闪而过"},
					{ID: "f1", Action: "resolve", Evidence: "黑影一闪而过"},
				},
			},
			want: "冲突操作",
		},
		{
			name: "resolved 伏笔上 advance 拒绝",
			seed: func(t *testing.T, s *store.Store) { seedForeshadowF1(t, s, "resolved") },
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "f1", Action: "advance", Evidence: "黑影一闪而过"}},
			},
			want: "cannot advance",
		},
		{
			name: "retired 伏笔上 resolve 拒绝",
			seed: func(t *testing.T, s *store.Store) { seedForeshadowF1(t, s, "retired") },
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "f1", Action: "resolve", Evidence: "黑影一闪而过"}},
			},
			want: "cannot resolve",
		},
		{
			name: "advanced 伏笔上 plant 拒绝",
			seed: func(t *testing.T, s *store.Store) { seedForeshadowF1(t, s, "advanced") },
			extra: map[string]any{
				"foreshadow_updates": []domain.ForeshadowUpdate{{ID: "f1", Action: "plant", Description: "新描述", Horizon: "book"}},
			},
			want: "cannot plant over status",
		},
		{
			name: "character_state 批内同 key 不同 value 拒绝",
			extra: map[string]any{
				"character_state_updates": []domain.CharacterStateUpdate{
					{Entity: "林墨", Field: "status.realm", Value: "练气期"},
					{Entity: "林墨", Field: "status.realm", Value: "金丹期"},
				},
			},
			want: "重复且 value 不同",
		},
		{
			name: "character_state 第 51 个字段拒绝",
			seed: func(t *testing.T, s *store.Store) {
				entries := make([]domain.CharacterStateEntry, 0, domain.MaxFieldsPerEntity)
				for i := 0; i < domain.MaxFieldsPerEntity; i++ {
					entries = append(entries, domain.CharacterStateEntry{
						Entity: "林墨", Field: fmt.Sprintf("status.f%02d", i), Value: "v",
					})
				}
				if err := s.World.SaveCharacterState(entries); err != nil {
					t.Fatalf("seed 50 fields: %v", err)
				}
			},
			extra: map[string]any{
				"character_state_updates": []domain.CharacterStateUpdate{{Entity: "林墨", Field: "status.overflow", Value: "x"}},
			},
			want: "字段数已达上限",
		},
		{
			name: "[world_rule] 提案缺规则描述",
			extra: map[string]any{
				"feedback": map[string]any{
					"suggestion": "[world_rule]  ",
				},
			},
			want: "[world_rule] 提案须包含规则描述",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := preflightStore(t)
			if tc.seed != nil {
				tc.seed(t, s)
			}
			before := captureWorldSnapshot(t, s)
			tool := NewCommitChapterTool(s)
			_, err := tool.Execute(context.Background(), preflightCommitArgsJSON(tc.extra))
			if err == nil {
				t.Fatal("preflight 必须拒绝")
			}
			if !errors.Is(err, errs.ErrToolArgs) {
				t.Fatalf("应为 ErrToolArgs，got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误消息应包含 %q，got %v", tc.want, err)
			}
			assertZeroSideEffects(t, s, before)
		})
	}
}

// TestCommitChapterPreflightAcceptsWorldRuleProposal 验证合法 [world_rule] 提案
// （suggestion 去除前缀后非空且 ≥10 字符的规则描述）通过 preflight。
// preflight 本身只读（不产生任何写入），直接调用即天然零副作用。
func TestCommitChapterPreflightAcceptsWorldRuleProposal(t *testing.T) {
	s := preflightStore(t)
	tool := NewCommitChapterTool(s)
	draft := "第一章正文。黑影一闪而过，林墨警觉起来。她心里骂自己丢人，真不要脸。"
	if err := tool.preflightCommitArgs(1, draft, nil, nil, nil, &domain.OutlineFeedback{
		Suggestion: "[world_rule] 奶税转型：官府对奶税的态度从宽松转向严苛，需要新增规则约束。",
	}); err != nil {
		t.Fatalf("合法 [world_rule] 提案应通过 preflight，got %v", err)
	}
	// 非 [world_rule] 前缀的普通建议不受格式校验约束。
	if err := tool.preflightCommitArgs(1, draft, nil, nil, nil, &domain.OutlineFeedback{
		Suggestion: "短",
	}); err != nil {
		t.Fatalf("非 [world_rule] 提案不应受规则描述长度约束，got %v", err)
	}
}

// TestCommitChapterCharacterStateUpserts 验证 character_state_updates 正常落库：
// (entity,field) 唯一键 upsert + 派生 state_changes + 完成提交。
func TestCommitChapterCharacterStateUpserts(t *testing.T) {
	s := preflightStore(t)
	tool := NewCommitChapterTool(s)
	raw, err := tool.Execute(context.Background(), preflightCommitArgsJSON(map[string]any{
		"character_state_updates": []domain.CharacterStateUpdate{
			{Entity: "林墨", Field: "status.realm", Value: "练气期", Reason: "突破", Evidence: "黑影一闪而过"},
			{Entity: "林墨", Field: "location.city", Value: "云州城"},
		},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out["committed"] != true {
		t.Fatalf("committed = %v, want true", out["committed"])
	}

	entries, err := s.World.LoadCharacterState()
	if err != nil {
		t.Fatalf("LoadCharacterState: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 character state entries, got %+v", entries)
	}
	byField := map[string]domain.CharacterStateEntry{}
	for _, e := range entries {
		byField[e.Field] = e
	}
	if e := byField["status.realm"]; e.Value != "练气期" || e.UpdatedChapter != 1 || e.Evidence != "黑影一闪而过" {
		t.Errorf("status.realm entry wrong: %+v", e)
	}
	if e := byField["location.city"]; e.Value != "云州城" || e.UpdatedChapter != 1 {
		t.Errorf("location.city entry wrong: %+v", e)
	}
	// 派生 state_changes：每 upsert 一条（含 reason）
	changes, _ := s.World.LoadStateChanges()
	if len(changes) != 2 {
		t.Fatalf("expected 2 derived state changes, got %+v", changes)
	}
	got := map[string]domain.StateChange{}
	for _, c := range changes {
		got[c.Field] = c
	}
	if c := got["status.realm"]; c.Entity != "林墨" || c.Reason != "突破" || c.NewValue != "练气期" {
		t.Errorf("derived state change status.realm wrong: %+v", c)
	}
	if c := got["location.city"]; c.NewValue != "云州城" {
		t.Errorf("derived state change location.city wrong: %+v", c)
	}
	if pending, _ := s.Signals.LoadPendingCommit(); pending != nil {
		t.Fatalf("pending commit should be cleared, got %+v", pending)
	}
}

// TestCommitChapterRewritePreserveSkipsCharacterState 验证 preserve 重写提交
// 不应用 character_state_updates（与 timeline/foreshadow/relationships/state 一致），
// 即使参数里携带该字段。
func TestCommitChapterRewritePreserveSkipsCharacterState(t *testing.T) {
	const final = "已提交的终稿。她心里骂自己丢人，真不要脸。"
	s := rewriteModeStore(t, final, "返工后的草稿。她心里骂自己丢人，真不要脸。")

	tool := NewCommitChapterTool(s)
	args, _ := json.Marshal(map[string]any{
		"chapter": 2, "summary": "重写摘要", "characters": []string{"林墨"}, "key_events": []string{"重写"},
		"world_state_mode":        "preserve",
		"character_state_updates": []domain.CharacterStateUpdate{{Entity: "林墨", Field: "status.realm", Value: "金丹期"}},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute preserve rewrite: %v", err)
	}
	entries, _ := s.World.LoadCharacterState()
	if len(entries) != 0 {
		t.Fatalf("preserve 后 character_state 应保持原状，got %+v", entries)
	}
	if changes, _ := s.World.LoadStateChanges(); len(changes) != 0 {
		t.Fatalf("preserve 后不得派生 state_changes，got %+v", changes)
	}
}
