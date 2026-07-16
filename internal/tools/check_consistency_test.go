package tools

import (
	"encoding/json"
	"testing"

	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestCheckConsistencyReturnsDigestAndRuleFactsWithoutContent(t *testing.T) {
	st := store.NewStore(t.TempDir())
	if err := st.Drafts.SaveDraft(1, "# 第一章\n\n某种程度上，他看见了 TEST。\n\n## 多余标题"); err != nil {
		t.Fatal(err)
	}
	snap := rules.BuildSnapshot([]rules.Candidate{rules.SystemDefaults()})
	if err := st.UserRules.Save(&snap); err != nil {
		t.Fatal(err)
	}
	out, err := NewCheckConsistencyTool(st).Execute(t.Context(), json.RawMessage(`{"chapter":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["content"]; ok {
		t.Fatal("check_consistency 不应重复返回草稿全文")
	}
	if got["content_digest"] == "" || got["word_count"] == nil {
		t.Fatalf("应返回摘要事实: %+v", got)
	}
	violations, ok := got["rule_violations"].([]any)
	if !ok || len(violations) < 3 {
		t.Fatalf("应同时运行 Lint + Check，got %+v", got["rule_violations"])
	}
}
