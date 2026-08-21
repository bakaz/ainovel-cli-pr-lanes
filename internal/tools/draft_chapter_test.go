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
