package userrules

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// Service 编排用户规则快照的生成与更新：归一化各来源 → 确定性合并 → 落盘。
//
// 两个调用方共用同一套逻辑：
//   - 开书/刷新（启动侧，确定性）：Build / GetOrBuild，由 Host 直接调用，不经 Coordinator。
//   - 运行中更新（Coordinator 工具）：AddRuntimeRule，save_user_rules 工具壳复用。
type Service struct {
	store     *store.Store
	norm      *Normalizer
	rulesOpts rules.LoadOptions
}

// NewService 构造服务。model 用于归一化（应为能力较强的模型）；model 为 nil 时
// 所有来源降级为 raw preferences（仍可产出快照，机械检查由 system_defaults 兜底）。
func NewService(st *store.Store, model agentcore.ChatModel, opts rules.LoadOptions) *Service {
	return &Service{store: st, norm: NewNormalizer(model), rulesOpts: opts}
}

// Build 从静态来源（system_defaults + rules 文件 + 启动 prompt）归一化生成快照并落盘。
// 开书/刷新时调用。startupPrompt 可空。
func (s *Service) Build(ctx context.Context, startupPrompt string) (*rules.Snapshot, error) {
	cands := []rules.Candidate{rules.SystemDefaults()}
	for _, rs := range rules.RawFileSources(s.rulesOpts) {
		cands = append(cands, s.norm.Normalize(ctx, rs.Label, rs.Text))
	}
	if strings.TrimSpace(startupPrompt) != "" {
		cands = append(cands, s.norm.Normalize(ctx, "startup_prompt", startupPrompt))
	}
	snap := rules.BuildSnapshot(cands)
	if err := s.store.UserRules.Save(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetOrBuild 返回当前快照；老书无快照时惰性生成（无启动 prompt 原文，故只含
// system_defaults + rules 文件）。运行时读取路径统一走这里。
func (s *Service) GetOrBuild(ctx context.Context) (*rules.Snapshot, error) {
	cur, err := s.store.UserRules.Load()
	if err != nil {
		return nil, err
	}
	if cur != nil {
		return cur, nil
	}
	return s.Build(ctx, "")
}

// AddRuntimeRule 归一化一条运行中长期规则，以最高优先级叠加到当前快照并落盘。
// 永不因归一化失败而报错——失败时该条降级为 raw preferences。
// 返回叠加后的快照与本次的归一化候选（后者供 save_user_rules 回显"理解成了什么"给用户确认）。
func (s *Service) AddRuntimeRule(ctx context.Context, text string) (*rules.Snapshot, rules.Candidate, error) {
	result, err := s.ApplyPatch(ctx, RulePatch{Action: PatchAdd, Text: text})
	return result.Snapshot, result.Candidate, err
}

type PatchAction string

var ErrInvalidPatch = errors.New("invalid user rule patch")

const (
	PatchAdd        PatchAction = "add"
	PatchRemove     PatchAction = "remove"
	PatchRevise     PatchAction = "revise"
	PatchReclassify PatchAction = "reclassify"
	PatchRebuild    PatchAction = "rebuild"
)

// RulePatch 是 Coordinator 能提交的受限变更，不允许用整份 JSON 覆盖快照。
type RulePatch struct {
	Action PatchAction
	Text   string
	RuleID string
	Scope  string
}

type PatchResult struct {
	Snapshot      *rules.Snapshot
	Candidate     rules.Candidate
	Changed       *rules.PreferenceRule
	PreviousScope rules.RuleScope
}

// ApplyPatch 对单条规则执行校验后的增删改/重分类；rebuild 仅规范化现有快照，
// 不接受 LLM 提供整份替换数据。
func (s *Service) ApplyPatch(ctx context.Context, patch RulePatch) (PatchResult, error) {
	cur, err := s.GetOrBuild(ctx)
	if err != nil {
		return PatchResult{}, err
	}
	if patch.Action == "" {
		patch.Action = PatchAdd
	}
	var forcedScope rules.RuleScope
	if strings.TrimSpace(patch.Scope) != "" {
		var ok bool
		forcedScope, ok = rules.ParseRuleScope(patch.Scope)
		if !ok {
			return PatchResult{}, fmt.Errorf("无效 scope %q: %w", patch.Scope, ErrInvalidPatch)
		}
	}
	out := *cur
	result := PatchResult{Snapshot: &out}

	switch patch.Action {
	case PatchAdd:
		text := strings.TrimSpace(patch.Text)
		if text == "" {
			return PatchResult{}, fmt.Errorf("add 需要 text: %w", ErrInvalidPatch)
		}
		cand := s.norm.Normalize(ctx, "runtime_update", text)
		if forcedScope != "" {
			cand = forceCandidateScope(cand, forcedScope)
		}
		out = rules.OverlaySnapshot(out, cand)
		result.Snapshot, result.Candidate = &out, cand
	case PatchRemove:
		rule, old, ok := out.Preferences.Remove(patch.RuleID)
		if !ok {
			return PatchResult{}, fmt.Errorf("未找到 rule_id %q: %w", patch.RuleID, ErrInvalidPatch)
		}
		result.Changed, result.PreviousScope = &rule, old
	case PatchRevise:
		text := strings.TrimSpace(patch.Text)
		if text == "" {
			return PatchResult{}, fmt.Errorf("revise 需要 text: %w", ErrInvalidPatch)
		}
		oldRule, oldScope, ok := out.Preferences.Remove(patch.RuleID)
		if !ok {
			return PatchResult{}, fmt.Errorf("未找到 rule_id %q: %w", patch.RuleID, ErrInvalidPatch)
		}
		cand := s.norm.Normalize(ctx, "runtime_revision", text)
		if forcedScope != "" {
			cand = forceCandidateScope(cand, forcedScope)
		} else if cand.Degraded || (len(cand.ScopedPreferences) == 0 && cand.Scope == rules.ScopeDefault) {
			// 旧 Normalizer 回复若没有 scope 会落到 default；修订时保留原桶比静默搬家更安全。
			cand.Scope = oldScope
		}
		out = rules.OverlaySnapshot(out, cand)
		result.Snapshot, result.Candidate = &out, cand
		result.Changed, result.PreviousScope = &oldRule, oldScope
	case PatchReclassify:
		if forcedScope == "" {
			return PatchResult{}, fmt.Errorf("reclassify 需要 scope: %w", ErrInvalidPatch)
		}
		rule, old, ok := out.Preferences.Move(patch.RuleID, forcedScope)
		if !ok {
			return PatchResult{}, fmt.Errorf("未找到 rule_id %q: %w", patch.RuleID, ErrInvalidPatch)
		}
		result.Changed, result.PreviousScope = &rule, old
	case PatchRebuild:
		out.Migrate()
	default:
		return PatchResult{}, fmt.Errorf("无效 action %q: %w", patch.Action, ErrInvalidPatch)
	}

	result.Snapshot.Migrate()
	if err := s.store.UserRules.Save(result.Snapshot); err != nil {
		return PatchResult{}, err
	}
	return result, nil
}

func forceCandidateScope(cand rules.Candidate, scope rules.RuleScope) rules.Candidate {
	var parts []string
	if text := strings.TrimSpace(cand.Preferences); text != "" {
		parts = append(parts, text)
	}
	for _, s := range []rules.RuleScope{rules.ScopeDefault, rules.ScopeArchitect, rules.ScopeWriter, rules.ScopeEditor} {
		if text := strings.TrimSpace(cand.ScopedPreferences[s]); text != "" {
			parts = append(parts, text)
		}
	}
	cand.Preferences = strings.Join(parts, "\n\n")
	cand.ScopedPreferences = nil
	cand.Scope = scope
	return cand
}
