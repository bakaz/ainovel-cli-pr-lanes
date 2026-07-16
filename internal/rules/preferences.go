package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// RuleScope 是自然语言规则的可见分区。
type RuleScope string

const (
	ScopeDefault   RuleScope = "default"
	ScopeArchitect RuleScope = "architect"
	ScopeWriter    RuleScope = "writer"
	ScopeEditor    RuleScope = "editor"
)

func ParseRuleScope(v string) (RuleScope, bool) {
	s := RuleScope(strings.ToLower(strings.TrimSpace(v)))
	switch s {
	case ScopeDefault, ScopeArchitect, ScopeWriter, ScopeEditor:
		return s, true
	default:
		return "", false
	}
}

// PreferenceRule 是一条可被 Coordinator 定点管理的自然语言规则。
type PreferenceRule struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

// PreferenceBuckets 保持四个简单分区；UnmarshalJSON 同时兼容 v1 字符串。
type PreferenceBuckets struct {
	Default   []PreferenceRule `json:"default,omitempty"`
	Architect []PreferenceRule `json:"architect,omitempty"`
	Writer    []PreferenceRule `json:"writer,omitempty"`
	Editor    []PreferenceRule `json:"editor,omitempty"`
}

func (b *PreferenceBuckets) UnmarshalJSON(data []byte) error {
	if b == nil {
		return fmt.Errorf("nil PreferenceBuckets")
	}
	var legacy string
	if err := json.Unmarshal(data, &legacy); err == nil {
		if text := strings.TrimSpace(legacy); text != "" {
			b.Default = []PreferenceRule{newPreferenceRule("legacy_v1", text)}
		}
		return nil
	}
	type alias PreferenceBuckets
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = PreferenceBuckets(decoded)
	b.Normalize()
	return nil
}

func (b *PreferenceBuckets) Normalize() {
	b.Default = normalizePreferenceRules(b.Default)
	b.Architect = normalizePreferenceRules(b.Architect)
	b.Writer = normalizePreferenceRules(b.Writer)
	b.Editor = normalizePreferenceRules(b.Editor)
}

func (b *PreferenceBuckets) Append(scope RuleScope, source, text string) PreferenceRule {
	if b == nil {
		return PreferenceRule{}
	}
	if _, ok := ParseRuleScope(string(scope)); !ok {
		scope = ScopeDefault
	}
	rule := newPreferenceRule(source, text)
	if rule.Text == "" {
		return PreferenceRule{}
	}
	list := b.list(scope)
	for _, existing := range *list {
		if existing.ID == rule.ID {
			return existing
		}
	}
	*list = append(*list, rule)
	return rule
}

func (b *PreferenceBuckets) Remove(id string) (PreferenceRule, RuleScope, bool) {
	id = strings.TrimSpace(id)
	for _, scope := range allRuleScopes() {
		list := b.list(scope)
		for i, rule := range *list {
			if rule.ID == id {
				*list = append((*list)[:i], (*list)[i+1:]...)
				return rule, scope, true
			}
		}
	}
	return PreferenceRule{}, "", false
}

func (b *PreferenceBuckets) Move(id string, target RuleScope) (PreferenceRule, RuleScope, bool) {
	if _, ok := ParseRuleScope(string(target)); !ok {
		return PreferenceRule{}, "", false
	}
	rule, old, ok := b.Remove(id)
	if !ok {
		return PreferenceRule{}, "", false
	}
	*b.list(target) = append(*b.list(target), rule)
	return rule, old, true
}

func (b PreferenceBuckets) TextForRole(role string) string {
	var scopes []RuleScope
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "architect", "architect_short", "architect_long":
		scopes = []RuleScope{ScopeDefault, ScopeArchitect}
	case "writer":
		scopes = []RuleScope{ScopeDefault, ScopeWriter}
	case "editor":
		scopes = []RuleScope{ScopeDefault, ScopeWriter, ScopeEditor}
	default:
		scopes = []RuleScope{ScopeDefault}
	}
	return b.textForScopes(scopes)
}

func (b PreferenceBuckets) AllText() string { return b.textForScopes(allRuleScopes()) }

func (b PreferenceBuckets) textForScopes(scopes []RuleScope) string {
	var sections []string
	for _, scope := range scopes {
		for _, rule := range *b.list(scope) {
			text := strings.TrimSpace(rule.Text)
			if text == "" {
				continue
			}
			if src := strings.TrimSpace(rule.Source); src != "" {
				text = fmt.Sprintf("## [%s]\n\n%s", src, text)
			}
			sections = append(sections, text)
		}
	}
	return strings.Join(sections, "\n\n")
}

func (b *PreferenceBuckets) list(scope RuleScope) *[]PreferenceRule {
	switch scope {
	case ScopeArchitect:
		return &b.Architect
	case ScopeWriter:
		return &b.Writer
	case ScopeEditor:
		return &b.Editor
	default:
		return &b.Default
	}
}

func allRuleScopes() []RuleScope {
	return []RuleScope{ScopeDefault, ScopeArchitect, ScopeWriter, ScopeEditor}
}

func newPreferenceRule(source, text string) PreferenceRule {
	source = strings.TrimSpace(source)
	text = strings.TrimSpace(text)
	if text == "" {
		return PreferenceRule{}
	}
	sum := sha256.Sum256([]byte(source + "\x00" + text))
	return PreferenceRule{ID: "rule_" + hex.EncodeToString(sum[:6]), Text: text, Source: source}
}

func normalizePreferenceRules(in []PreferenceRule) []PreferenceRule {
	out := make([]PreferenceRule, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, rule := range in {
		rule.Text = strings.TrimSpace(rule.Text)
		rule.Source = strings.TrimSpace(rule.Source)
		if rule.Text == "" {
			continue
		}
		if strings.TrimSpace(rule.ID) == "" {
			rule = newPreferenceRule(rule.Source, rule.Text)
		}
		if _, ok := seen[rule.ID]; ok {
			continue
		}
		seen[rule.ID] = struct{}{}
		out = append(out, rule)
	}
	return out
}
