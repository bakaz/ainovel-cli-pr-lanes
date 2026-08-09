package diag

import (
	"fmt"
	"sort"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// ── 诊断阈值 ─────────────────────────────────────────────

const (
	ThresholdDimScoreLow      = 70  // ChronicLowDimension: 维度均分低于此值告警
	ThresholdContractMissRate = 0.3 // ContractMissPattern: 合同未达成率上限
	ThresholdRewriteRate      = 0.5 // ExcessiveRewrites: 改写率上限
	ThresholdWordShortRatio   = 0.4 // WordCountAnomaly: 字数低于均值此比例视为异常
	ThresholdWordLongRatio    = 2.5 // WordCountAnomaly: 字数高于均值此比例视为异常
	ThresholdHookWeakScore    = 75  // HookWeakChain: hook 低于此分视为偏弱
	ThresholdHookWeakChain    = 3   // HookWeakChain: 连续偏弱章数阈值
	ThresholdPayoffMissRate   = 0.4 // PayoffMissPattern: payoff 未兑现率上限
	ThresholdCompassDrift     = 15  // CompassDrift: 指南针未更新章数上限
	ThresholdTimelineGapRate  = 0.3 // TimelineGaps: 缺失率容忍上限
	ThresholdForeshadowStale  = 100 // StaleForeshadow: 伏笔停滞固定阈值（章）
)

// allRules 按 flow → quality → planning → context 排列。
var allRules = []RuleFunc{
	// Flow
	InvalidPendingRewrites,
	RewritePendingPressure,
	OrphanedSteer,
	PhaseFlowMismatch,
	ChapterGaps,
	// Quality
	ChronicLowDimension,
	ContractMissPattern,
	HookWeakChain,
	PayoffMissPattern,
	ExcessiveRewrites,
	WordCountAnomaly,
	// Planning
	StaleForeshadow,
	CompassDrift,
	OutlineExhausted,
	MissingSummaries,
	// Context
	GhostCharacter,
	TimelineGaps,
	RelationshipStagnation,
}

// Analyze 是诊断系统的唯一入口。
func Analyze(s *store.Store) Report {
	snap := Load(s)

	var findings []Finding
	for _, e := range snap.LoadErrors {
		findings = append(findings, Finding{
			Rule:       "LoadError",
			Category:   CatFlow,
			Severity:   SevWarning,
			Confidence: ConfHigh,
			AutoLevel:  AutoNone,
			Target:     "runtime.flow",
			Title:      fmt.Sprintf("工件加载失败: %s", e),
			Suggestion: "文件可能损坏或权限不足，相关诊断规则的结果可能不完整。",
		})
	}
	for _, rule := range allRules {
		findings = append(findings, rule(&snap)...)
	}
	sortFindings(findings)

	return Report{
		Stats:    buildStats(&snap),
		Findings: findings,
		Actions:  PlanActions(findings),
	}
}

func buildStats(snap *Snapshot) Stats {
	st := Stats{}
	if snap.Progress == nil {
		return st
	}
	p := snap.Progress
	st.CompletedChapters = len(p.CompletedChapters)
	st.TotalChapters = p.TotalChapters
	st.TotalWords = p.TotalWordCount
	st.Phase = string(p.Phase)
	st.Flow = string(p.Flow)

	if st.CompletedChapters > 0 {
		st.AvgWordsPerCh = st.TotalWords / st.CompletedChapters
	}

	if snap.RunMeta != nil {
		st.PlanningTier = string(snap.RunMeta.PlanningTier)
	}

	// 评审统计
	st.ReviewCount = len(snap.Reviews)
	var totalScore float64
	var dimCount int
	for _, r := range snap.Reviews {
		if r.Verdict == "rewrite" {
			st.RewriteCount++
		}
		for _, d := range r.Dimensions {
			totalScore += float64(d.Score)
			dimCount++
		}
	}
	if dimCount > 0 {
		st.AvgReviewScore = totalScore / float64(dimCount)
	}

	// 伏笔统计：open 按活跃列表口径（排除 resolved + retired；legacy 空状态视为活跃），
	// stale 按 effectiveTouchedAt（last_touched_at>0 用其，否则退回 planted_at）判定，
	// 与 novel_context 的 agingForeshadow 共用固定 100 章阈值，保证诊断与召回口径一致。
	latest := snap.LatestCompleted()
	for _, f := range snap.Foreshadow {
		if f.Status == "resolved" || f.Status == "retired" {
			continue
		}
		st.ForeshadowOpen++
		if touched := effectiveForeshadowTouched(f); touched > 0 && latest-touched >= ThresholdForeshadowStale {
			st.ForeshadowStale++
		}
	}
	return st
}

// effectiveForeshadowTouched 返回伏笔的"最近推进"基准章节：
// 有 LastTouchedAt（advance/resolve 过）用它，否则退回 PlantedAt（旧数据兼容）。
func effectiveForeshadowTouched(f domain.ForeshadowEntry) int {
	if f.LastTouchedAt > 0 {
		return f.LastTouchedAt
	}
	return f.PlantedAt
}

// sortFindings 按严重程度排序：critical > warning > info。
func sortFindings(findings []Finding) {
	order := map[Severity]int{SevCritical: 0, SevWarning: 1, SevInfo: 2}
	sort.SliceStable(findings, func(i, j int) bool {
		return order[findings[i].Severity] < order[findings[j].Severity]
	})
}
