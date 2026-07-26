package errclass

import "testing"

func TestClassifyMsg_Nominal(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"", ""},
		{"InputValidationError: commit_chapter failed", CatToolSchemaValidation},
		{"The required parameter `chapter` is missing", CatToolSchemaValidation},
		{"type is expected as `integer` but provided as `string`", CatToolSchemaValidation},
		{"ArgsParseError: unexpected token at offset 42", CatToolArgsMalformed},
		{"cannot parse json: invalid character 'x' looking for beginning of value", CatToolArgsMalformed},
		{"unexpected token at input", CatToolArgsMalformed},
		{`invalid character '"' looking for beginning of object key string`, CatToolArgsMalformed},
		// agentcore malformed-args patterns (must be tool_args_malformed even when
		// error chain is ErrToolValidation).
		{"tool validation: commit_chapter received malformed JSON arguments: unexpected end of JSON input\nraw args: {\"chapter\": 1", CatToolArgsMalformed},
		{"tool validation: commit_chapter received invalid JSON arguments: invalid character 'x' looking for beginning of value", CatToolArgsMalformed},
		{"unexpected end of JSON input", CatToolArgsMalformed},
		// "invalid character" without "looking for" must NOT match (narrowing)
		{"some provider error: invalid character in response", ""},
		{"semantic validation failed: chapter does not exist", CatToolSemanticVal},
		{"write before read: chapter 5 not yet drafted", CatToolSemanticVal},
		{"no such chapter: 99", CatToolSemanticVal},
		{"noop: no changes needed", CatToolNoop},
		{"no changes: nothing to update", CatToolNoop},
		{"nothing to update for chapter 7", CatToolNoop},
		{"nothing to do here", CatToolNoop},
		{"style review exhausted after max attempts", CatStyleReviewExhausted},
		{"style review max limit reached", CatStyleReviewExhausted},
		{"评审已耗尽", CatStyleReviewExhausted},
		{"critic 模式：章节 5 评审已耗尽（exhausted），不能 commit", CatStyleReviewExhausted},
		{"max turns (100) reached", CatMaxTurns},
		{"provider stream idle timeout", CatStreamIdle},
		{"stream idle timeout occurred", CatStreamIdle},
		{"unknown random error message", ""},
		{"some completely unrelated error", ""},
	}
	for _, tt := range tests {
		name := tt.msg
		if len(name) > 50 {
			name = name[:50]
		}
		t.Run(name, func(t *testing.T) {
			got := ClassifyMsg(tt.msg)
			if got != tt.want {
				t.Errorf("ClassifyMsg(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestClassifyMsg_CategoryIsStableLabelOnly(t *testing.T) {
	// Verify that every returned category is a short identifier string that
	// contains no body text, raw args, or provider SSE.
	sensitive := []string{"雪夜", "主角", "正文", "args:", "sse:", "prompt:", "thinking:"}
	for _, msg := range []string{
		"InputValidationError: commit_chapter failed",
		"cannot parse json",
		"noop",
		"max turns reached",
		"style review exhausted",
		"semantic validation failed",
		"provider stream idle timeout",
	} {
		cat := ClassifyMsg(msg)
		for _, s := range sensitive {
			if containsAny(cat, s) {
				t.Errorf("ClassifyMsg(%q) = %q contains sensitive substring %q", msg, cat, s)
			}
		}
	}
}

func TestClassifyMsg_Exhaustive(t *testing.T) {
	// Every required category must have at least one matching message.
	required := []string{
		CatToolArgsMalformed,
		CatToolSchemaValidation,
		CatToolSemanticVal,
		CatToolNoop,
		CatStyleReviewExhausted,
		CatMaxTurns,
		CatStreamIdle,
	}
	covered := make(map[string]bool)
	for _, msg := range []string{
		"cannot parse json",
		"received malformed JSON arguments",
		"InputValidationError: test",
		"semantic validation failed",
		"noop",
		"style review exhausted",
		"评审已耗尽",
		"max turns reached",
		"stream idle timeout",
	} {
		c := ClassifyMsg(msg)
		if c != "" {
			covered[c] = true
		}
	}
	for _, cat := range required {
		if !covered[cat] {
			t.Errorf("category %q has no test coverage", cat)
		}
	}
}
