package tools

import (
	"fmt"

	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── Action 常量别名（guard 允许的工具名） ────────────────────────────
// 旧 FSM（ComputeRequiredNextAction / rewriteQueueAction /
// polishSeqBoundToLatest / PolishPipelineBinding）已删除；RequiredNextAction
// 类型与"下一步建议"的唯一生成逻辑已迁移至 chapter_stage.go
// （(ChapterStageDecision) RequiredNextAction()）。
// 以下别名保留旧调用点（check_consistency、测试）的兼容，直接引用
// chapter_stage.go 的 ChapterAction 枚举（唯一权威）——string() 转换使别名
// 保持 string 类型：既有调用点既有 string 字段赋值，也有 map[string]any 的
// 接口比较，typed ChapterAction 常量会破坏二者。

const (
	ActionDraftChapter  = string(ChapterActionDraft)
	ActionEditChapter   = string(ChapterActionEdit)
	ActionReviewStyle   = string(ChapterActionReview)
	ActionCommitChapter = string(ChapterActionCommit)
	ActionPolishDraft   = string(ChapterActionPolish)
)

// hasErrorViolations 检查违规列表中是否存在 error 级的违规。
func hasErrorViolations(violations []rules.Violation) bool {
	for _, v := range violations {
		if v.Severity == rules.SeverityError {
			return true
		}
	}
	return false
}
func chapterWordRange(st *store.Store) *rules.WordRange {
	structured := rules.SystemDefaults().Structured
	if snap, err := st.UserRules.Load(); err == nil && snap != nil {
		structured = snap.Structured
	}
	return structured.ChapterWords
}

// onlyUnderMinChapterWordsError is intentionally narrow: it recognizes the
// safe continuation case only when the sole error-level violation is the
// chapter lower bound. Other errors, rewrite work, and review decisions must
// retain their existing recovery semantics.
func onlyUnderMinChapterWordsError(st *store.Store, content string, wordCount int) bool {
	rng := chapterWordRange(st)
	if rng == nil || rng.Min <= 0 || wordCount >= rng.Min {
		return false
	}
	errors := 0
	for _, violation := range computeMechanicalViolations(st, content, wordCount) {
		if violation.Severity != rules.SeverityError {
			continue
		}
		errors++
		if violation.Rule != "chapter_words" {
			return false
		}
	}
	return errors == 1
}

func underMinWordCountGuidance(st *store.Store, wordCount int) (map[string]any, bool) {
	rng := chapterWordRange(st)
	if rng == nil || rng.Min <= 0 || wordCount >= rng.Min {
		return nil, false
	}
	deficit := rng.Min - wordCount
	return map[string]any{
		"status":           "under_min",
		"actual":           wordCount,
		"minimum":          rng.Min,
		"deficit":          deficit,
		"recommended_mode": "append",
		"instruction": fmt.Sprintf(
			"当前草稿 %d 字，低于最小值 %d（还差 %d 字）。这是当前唯一 error；已有正文请调用 draft_chapter(mode=\"append\") 续写，不要用 mode=\"write\" 整章重写。追加后再 read_chapter(source=\"draft\")，然后 check_consistency。",
			wordCount, rng.Min, deficit,
		),
	}, true
}

// shouldForceAppendUnderMin narrows the hard guard to an initial, non-review
// draft. Review/rewrite paths remain governed by their existing FSM actions.
func shouldForceAppendUnderMin(st *store.Store, chapter int, content string, wordCount int) bool {
	ledger, err := st.StyleReview.Load(chapter)
	if err != nil {
		return false
	}
	if ledger != nil && ledger.CurrentStatus() != "" {
		return false
	}
	return onlyUnderMinChapterWordsError(st, content, wordCount)
}
