package domain

import (
	"fmt"
	"time"
)

// ScopeKind 标识 checkpoint 的作用域类型。
type ScopeKind string

const (
	ScopeChapter ScopeKind = "chapter"
	ScopeArc     ScopeKind = "arc"
	ScopeVolume  ScopeKind = "volume"
	ScopeGlobal  ScopeKind = "global"
)

// Scope 定位一条 checkpoint 所属的创作范围。
type Scope struct {
	Kind    ScopeKind `json:"kind"`
	Chapter int       `json:"chapter,omitempty"`
	Volume  int       `json:"volume,omitempty"`
	Arc     int       `json:"arc,omitempty"`
}

// ChapterScope 构造一个章节级 Scope。
func ChapterScope(chapter int) Scope {
	return Scope{Kind: ScopeChapter, Chapter: chapter}
}

// ArcScope 构造一个弧级 Scope。
func ArcScope(volume, arc int) Scope {
	return Scope{Kind: ScopeArc, Volume: volume, Arc: arc}
}

// VolumeScope 构造一个卷级 Scope。
func VolumeScope(volume int) Scope {
	return Scope{Kind: ScopeVolume, Volume: volume}
}

// GlobalScope 构造一个全局 Scope。
func GlobalScope() Scope {
	return Scope{Kind: ScopeGlobal}
}

func (s Scope) String() string {
	switch s.Kind {
	case ScopeChapter:
		return fmt.Sprintf("chapter:%d", s.Chapter)
	case ScopeArc:
		return fmt.Sprintf("arc:v%da%d", s.Volume, s.Arc)
	case ScopeVolume:
		return fmt.Sprintf("volume:%d", s.Volume)
	default:
		return "global"
	}
}

// Matches 判断两个 Scope 是否相同。
func (s Scope) Matches(other Scope) bool {
	if s.Kind != other.Kind {
		return false
	}
	switch s.Kind {
	case ScopeChapter:
		return s.Chapter == other.Chapter
	case ScopeArc:
		return s.Volume == other.Volume && s.Arc == other.Arc
	case ScopeVolume:
		return s.Volume == other.Volume
	default:
		return true
	}
}

// Checkpoint 记录某个 step 成功完成的事实。
// 由工具在原子落盘后追加到 JSONL，是恢复和观察的唯一事实来源。
type Checkpoint struct {
	Seq        int64     `json:"seq"`
	Scope      Scope     `json:"scope"`
	Step       string    `json:"step"`
	Artifact   string    `json:"artifact,omitempty"`
	Digest     string    `json:"digest,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	// 以下字段仅 polish 步骤（polish_draft 工具）使用，其余步骤为零值。
	InputDigest   string `json:"input_digest,omitempty"`   // 精修前的草稿 digest（DS 初稿）
	PolisherModel string `json:"polisher_model,omitempty"` // 执行精修的 polisher 模型名
	Stage         string `json:"stage,omitempty"`          // "draft"（初稿精修）或 "rewrite"（返工队列精修）
	Changed       bool   `json:"changed,omitempty"`        // 精修是否实际改动正文（false=no-op，允许）
	// Degraded=true 表示这是一条"降级精修记录"：polisher 经有限重试仍失败
	// （stream idle / provider timeout / network 类 / MaxTurns 等可恢复类错误），
	// 正文未变、Digest 绑定当前草稿。degraded 记录是"精修不可用"的留痕——
	// FSM/commit gate 将其视为合法 polish 记录以推进 post-polish check → review，
	// 消除"polish 必须成功才可推进"导致的永久 needs_polish 死锁。默认 false 兼容
	// 既有数据。
	Degraded bool `json:"degraded,omitempty"`
	// ErrorCategory 是降级原因的稳定分类（stream_idle/max_turns/timeout/network/
	// rate_limit/overloaded，与 agentcore.ErrorKind 同义），仅审计用，不影响判定。
	ErrorCategory string `json:"error_category,omitempty"`
	// Method 记录精修执行方式："edit_list"（结构化 edit 列表原子应用，ora-1 形态 2）
	// 或 "full_text"（整章重输出，旧协议回退路径）；degraded 记录可能为空。仅审计用。
	Method string `json:"method,omitempty"`
	// EditCount 是 edit_list 路径实际应用的 edit 条数（空 edits=0）；
	// full_text/degraded 路径为零。仅审计用，不影响判定。
	EditCount int `json:"edit_count,omitempty"`
	// 以下字段是 edit_list 路径的部分接受/归一化匹配审计（ora-1 ④）：只含计数与
	// 原因分类，绝不含正文/old_string/new_string 内容。仅审计用，不影响判定。
	// ProposedEditCount 是 polisher 提出的 edit 条数（可能超过上限/含无效条）。
	ProposedEditCount int `json:"proposed_edit_count,omitempty"`
	// DroppedEditCount 是被丢弃（未应用）的 edit 条数。
	DroppedEditCount int `json:"dropped_edit_count,omitempty"`
	// DropReasons 是被丢弃 edit 的原因分类（anchor_missing/anchor_ambiguous/
	// overlap_lower_priority/coverage_limit/output_too_short/output_too_long/
	// count_limit/noop/empty_old_string/old_too_long/mechanical），按原 plan 下标序。
	DropReasons []string `json:"drop_reasons,omitempty"`
	// NormalizedMatchCount 是实际应用中经归一化（白名单等价）定位的 edit 条数。
	NormalizedMatchCount int `json:"normalized_match_count,omitempty"`
	// Partial=true 表示部分接受：≥1 条应用且 ≥1 条被丢弃（无需模型二次纠错）。
	Partial bool `json:"partial,omitempty"`
	// MatchModes 是实际应用 edit 的匹配模式（"exact"/"normalized"，按应用序）。
	MatchModes []string `json:"match_modes,omitempty"`
}

// PolishCheckpointMeta 是 polish 步骤 checkpoint 的附加元数据。
// Digest（主字段）即 output_digest。
type PolishCheckpointMeta struct {
	InputDigest   string
	PolisherModel string
	Stage         string
	Changed       bool
	Degraded      bool
	ErrorCategory string
	Method        string
	EditCount     int
	// 审计字段（edit_list 路径部分接受/归一化匹配，见 Checkpoint 同名注释）。
	ProposedEditCount    int
	DroppedEditCount     int
	DropReasons          []string
	NormalizedMatchCount int
	Partial              bool
	MatchModes           []string
}
