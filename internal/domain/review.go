package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// TimelineEvent 时间线事件。
type TimelineEvent struct {
	Chapter    int      `json:"chapter"`
	Time       string   `json:"time"`
	Event      string   `json:"event"`
	Characters []string `json:"characters,omitempty"`
}

// ForeshadowEntry 伏笔条目。
type ForeshadowEntry struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	PlantedAt   int    `json:"planted_at"`
	Status      string `json:"status"` // planted / advanced / resolved / retired
	ResolvedAt  int    `json:"resolved_at,omitempty"`

	Horizon            string `json:"horizon,omitempty"`             // cross_arc | book（仅 plant 设置，后续不可改）
	LastTouchedAt      int    `json:"last_touched_at,omitempty"`     // 最近一次 advance/resolve 的章节
	LastEvidence       string `json:"last_evidence,omitempty"`       // 最近一次 advance 的正文引文
	ResolutionEvidence string `json:"resolution_evidence,omitempty"` // resolve 时的正文引文
	ClosedAt           int    `json:"closed_at,omitempty"`           // retire 章节
	CloseReason        string `json:"close_reason,omitempty"`        // retire 原因
}

// ForeshadowUpdate 伏笔增量操作。
type ForeshadowUpdate struct {
	ID          string `json:"id"`
	Action      string `json:"action"` // plant / advance / resolve / retire
	Description string `json:"description,omitempty"`
	Horizon     string `json:"horizon,omitempty"`  // 仅 plant 使用（cross_arc | book）
	Evidence    string `json:"evidence,omitempty"` // advance/resolve 必填（正文精确短引文）
	Reason      string `json:"reason,omitempty"`   // 仅 retire 必填
}

// MaxForeshadowEvidenceRunes advance/resolve 证据引文的长度上限（rune 数）。
const MaxForeshadowEvidenceRunes = 160

// ForeshadowTransitionAllowed 校验 action 对当前状态的转换合法性（§4.1 转换表）。
// currentStatus 为空串表示"不存在/空状态"。返回 nil 或带 ID/状态/action 信息的错误。
// 转换表语义（与 store.UpdateForeshadow 现状一致，供 preflight 与 store 共用）：
//
//	当前状态    plant            advance            resolve             retire
//	""(不存在)  允许             拒绝               拒绝                拒绝
//	planted    允许(幂等补空)    允许               允许                允许
//	advanced   拒绝             允许               允许                允许
//	resolved   拒绝             拒绝               允许(幂等返回 nil)   拒绝
//	retired    拒绝             拒绝               拒绝                允许(幂等返回 nil)
//
// 注意：store 对已存在的"空状态"（旧数据遗留）条目在调用本函数前会先归一化为
// planted（保持既有行为），因此本函数把 "" 视为"不存在/空状态"仅放行 plant。
func ForeshadowTransitionAllowed(currentStatus string, u ForeshadowUpdate) error {
	switch u.Action {
	case "plant":
		if currentStatus != "" && currentStatus != "planted" {
			return fmt.Errorf("foreshadow %q: cannot plant over status %q", u.ID, currentStatus)
		}
	case "advance":
		if currentStatus != "planted" && currentStatus != "advanced" {
			return fmt.Errorf("foreshadow %q: cannot advance from status %q", u.ID, currentStatus)
		}
	case "resolve":
		if currentStatus == "resolved" {
			return nil // 幂等：重复 resolve 允许
		}
		if currentStatus != "planted" && currentStatus != "advanced" {
			return fmt.Errorf("foreshadow %q: cannot resolve from status %q", u.ID, currentStatus)
		}
	case "retire":
		if currentStatus == "retired" {
			return nil // 幂等：重复 retire 允许
		}
		if currentStatus != "planted" && currentStatus != "advanced" {
			return fmt.Errorf("foreshadow %q: cannot retire from status %q", u.ID, currentStatus)
		}
	default:
		return fmt.Errorf("foreshadow %q: unknown action %q", u.ID, u.Action)
	}
	return nil
}

// Validate 校验伏笔增量操作（供 commit preflight 复用）。
// evidenceInDraft 非 nil 时，advance/resolve 的 evidence 必须能在正文草稿中找到
// （正文精确短引文）；chapter 参数供后续阶段使用，当前不参与校验。
func (u ForeshadowUpdate) Validate(chapter int, evidenceInDraft func(string) bool) error {
	if strings.TrimSpace(u.ID) == "" {
		return fmt.Errorf("foreshadow: id 不能为空")
	}
	switch u.Action {
	case "plant":
		if strings.TrimSpace(u.Description) == "" {
			return fmt.Errorf("foreshadow %q: action %q requires description", u.ID, u.Action)
		}
		if u.Horizon != "cross_arc" && u.Horizon != "book" {
			return fmt.Errorf("foreshadow %q: action %q requires horizon (cross_arc|book)", u.ID, u.Action)
		}
	case "advance", "resolve":
		if strings.TrimSpace(u.Evidence) == "" {
			return fmt.Errorf("foreshadow %q: action %q requires evidence", u.ID, u.Action)
		}
		if runes := utf8.RuneCountInString(u.Evidence); runes > MaxForeshadowEvidenceRunes {
			return fmt.Errorf("foreshadow %q: evidence 超过 %d 字符上限（当前 %d）", u.ID, MaxForeshadowEvidenceRunes, runes)
		}
		if evidenceInDraft != nil && !evidenceInDraft(u.Evidence) {
			return fmt.Errorf("foreshadow %q: evidence 未在正文草稿中出现", u.ID)
		}
	case "retire":
		if strings.TrimSpace(u.Reason) == "" {
			return fmt.Errorf("foreshadow %q: action %q requires reason", u.ID, u.Action)
		}
	default:
		return fmt.Errorf("foreshadow %q: unknown action %q", u.ID, u.Action)
	}
	return nil
}

// RelationshipEntry 人物关系条目。
type RelationshipEntry struct {
	CharacterA string `json:"character_a"`
	CharacterB string `json:"character_b"`
	Relation   string `json:"relation"`
	Chapter    int    `json:"chapter"`
}

// ConsistencyIssue 一致性问题。
type ConsistencyIssue struct {
	Type        string `json:"type"`     // consistency / character / pacing / continuity / foreshadow / hook / aesthetic
	Severity    string `json:"severity"` // critical / error / warning
	Description string `json:"description"`
	Evidence    string `json:"evidence,omitempty"` // 证据：原文片段、具体情节或状态数据
	Suggestion  string `json:"suggestion,omitempty"`
}

// DimensionScore 单维度评审评分。
type DimensionScore struct {
	Dimension string `json:"dimension"`         // consistency / character / pacing / continuity / foreshadow / hook / aesthetic
	Score     int    `json:"score"`             // 0-100
	Verdict   string `json:"verdict"`           // pass / warning / fail
	Comment   string `json:"comment,omitempty"` // 该维度的简要结论
}

// ReviewEntry Editor 的审阅条目。
type ReviewEntry struct {
	Chapter          int                `json:"chapter"`
	Scope            string             `json:"scope"` // chapter / global / arc
	Issues           []ConsistencyIssue `json:"issues"`
	Dimensions       []DimensionScore   `json:"dimensions,omitempty"`      // 分维度评分
	ContractStatus   string             `json:"contract_status,omitempty"` // met / partial / missed
	ContractMisses   []string           `json:"contract_misses,omitempty"` // 未达成的 contract 条目
	ContractNotes    string             `json:"contract_notes,omitempty"`  // 对 contract 履行情况的简述
	Verdict          string             `json:"verdict"`                   // accept / polish / rewrite
	Summary          string             `json:"summary"`
	AffectedChapters []int              `json:"affected_chapters,omitempty"` // 需要重写/打磨的章节号
}

// CriticalCount 返回 critical 级别问题数量。
func (r *ReviewEntry) CriticalCount() int {
	n := 0
	for _, issue := range r.Issues {
		if issue.Severity == "critical" {
			n++
		}
	}
	return n
}

// ErrorCount 返回 error 级别问题数量。
func (r *ReviewEntry) ErrorCount() int {
	n := 0
	for _, issue := range r.Issues {
		if issue.Severity == "error" {
			n++
		}
	}
	return n
}

// Dimension 返回指定维度的评分；不存在则返回 nil。
func (r *ReviewEntry) Dimension(name string) *DimensionScore {
	if r == nil {
		return nil
	}
	for i := range r.Dimensions {
		if r.Dimensions[i].Dimension == name {
			return &r.Dimensions[i]
		}
	}
	return nil
}
