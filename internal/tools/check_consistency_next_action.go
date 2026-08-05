package tools

import (
	"github.com/voocel/ainovel-cli/internal/rules"
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
