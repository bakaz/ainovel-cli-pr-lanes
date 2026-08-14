package rules

import (
	"strings"
	"testing"
)

// TestPolisherScope 验证 polisher 规则分区：polisher 视图 = default+writer+polisher；
// writer 视图不受 polisher 规则影响。
func TestPolisherScope(t *testing.T) {
	buckets := PreferenceBuckets{
		Default:   []PreferenceRule{{ID: "d1", Text: "默认规则"}},
		Writer:    []PreferenceRule{{ID: "w1", Text: "写作规则"}},
		Polisher:  []PreferenceRule{{ID: "p1", Text: "精修规则"}},
		Architect: []PreferenceRule{{ID: "a1", Text: "规划规则"}},
		Editor:    []PreferenceRule{{ID: "e1", Text: "评审规则"}},
	}

	pText := buckets.TextForRole("polisher")
	for _, want := range []string{"默认规则", "写作规则", "精修规则"} {
		if !strings.Contains(pText, want) {
			t.Errorf("polisher 视图应包含 %q，实际: %q", want, pText)
		}
	}
	if strings.Contains(pText, "规划规则") || strings.Contains(pText, "评审规则") {
		t.Errorf("polisher 视图不应包含 architect/editor 规则，实际: %q", pText)
	}

	wText := buckets.TextForRole("writer")
	if !strings.Contains(wText, "写作规则") {
		t.Errorf("writer 视图应包含 writer 规则，实际: %q", wText)
	}
	if strings.Contains(wText, "精修规则") {
		t.Errorf("writer 视图不应包含 polisher 规则，实际: %q", wText)
	}
}

func TestParsePolisherScope(t *testing.T) {
	if _, ok := ParseRuleScope("polisher"); !ok {
		t.Fatal("polisher 应为合法 scope")
	}
	if _, ok := ParseRuleScope("Polisher"); !ok {
		t.Fatal("Polisher 大小写应归一化后合法")
	}
}
