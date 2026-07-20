package tools

import (
	"encoding/json"
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// ChapterInput 是工具输入层的章节结构，允许省略/0 序号。
// 仅用于 initial layered_outline 和 append_volume 的解码；expand_arc 保持 domain.ArcExpansion。
type ChapterInput struct {
	Chapter   int              `json:"chapter,omitempty"`
	Title     string           `json:"title,omitempty"`
	CoreEvent string           `json:"core_event,omitempty"`
	Hook      string           `json:"hook,omitempty"`
	Scenes    domain.SceneList `json:"scenes,omitempty"`
}

func (c ChapterInput) toDomain() domain.OutlineEntry {
	return domain.OutlineEntry{
		Chapter:   c.Chapter,
		Title:     c.Title,
		CoreEvent: c.CoreEvent,
		Hook:      c.Hook,
		Scenes:    c.Scenes,
	}
}

// ArcInput 是工具输入层的弧结构，支持骨架弧和详细弧。
// 骨架弧：EstimatedChapters > 0 且 Chapters 为空/null
// 详细弧：Chapters 非空（此时 EstimatedChapters 在转换到 domain 前归零）
type ArcInput struct {
	Index             int            `json:"index"`
	Title             string         `json:"title"`
	Goal              string         `json:"goal"`
	EstimatedChapters int            `json:"estimated_chapters,omitempty"`
	Chapters          []ChapterInput `json:"chapters,omitempty"`
}

// IsDetailed 返回 true 表示这是详细弧（非空 chapters）。
func (a ArcInput) IsDetailed() bool { return len(a.Chapters) > 0 }

// IsSkeleton 返回 true 表示这是骨架弧（estimated_chapters>0 且无 chapters）。
func (a ArcInput) IsSkeleton() bool { return !a.IsDetailed() && a.EstimatedChapters > 0 }

// toDomain 转换到领域模型。详细弧若有 estimated_chapters 则归零。
func (a ArcInput) toDomain() domain.ArcOutline {
	chs := make([]domain.OutlineEntry, len(a.Chapters))
	for i, c := range a.Chapters {
		chs[i] = c.toDomain()
	}
	est := a.EstimatedChapters
	if len(chs) > 0 {
		est = 0
	}
	return domain.ArcOutline{
		Index:             a.Index,
		Title:             a.Title,
		Goal:              a.Goal,
		EstimatedChapters: est,
		Chapters:          chs,
	}
}

// PlanningVolumeInput 是工具输入层的卷结构。仅用于 initial layered_outline 和 append_volume。
type PlanningVolumeInput struct {
	Index int        `json:"index"`
	Title string     `json:"title"`
	Theme string     `json:"theme"`
	Final bool       `json:"final,omitempty"`
	Arcs  []ArcInput `json:"arcs"`
}

func (p PlanningVolumeInput) toDomain() domain.VolumeOutline {
	arcs := make([]domain.ArcOutline, len(p.Arcs))
	for i, a := range p.Arcs {
		arcs[i] = a.toDomain()
	}
	return domain.VolumeOutline{
		Index: p.Index,
		Title: p.Title,
		Theme: p.Theme,
		Final: p.Final,
		Arcs:  arcs,
	}
}

// ── Phase 1 semantic error aggregation ──

// planValidationError 描述一个独立的语义验证错误。
type planValidationError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// planValidationErrors 聚合多个语义错误，供 Phase 1 返回。
// 它不会包装 Schema/JSON 类型/状态/Store/I/O 错误。
type planValidationErrors []planValidationError

func (e planValidationErrors) Error() string {
	raw, _ := json.Marshal(map[string]any{
		"validation_errors": []planValidationError(e),
	})
	return string(raw)
}

func (e planValidationErrors) Len() int { return len(e) }

// addError 向聚合追加一个语义错误。
func (e *planValidationErrors) addError(path, code, message string) {
	*e = append(*e, planValidationError{Path: path, Code: code, Message: message})
}

// addErrorf 格式化追加语义错误。
func (e *planValidationErrors) addErrorf(path, code, format string, args ...any) {
	*e = append(*e, planValidationError{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}
