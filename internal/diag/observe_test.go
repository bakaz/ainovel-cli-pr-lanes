package diag

import (
	"encoding/json"
	"testing"

	"github.com/voocel/agentcore"
)

// TestClassifyErrMsg_Nominal 验证各类错误消息能被正确分类。
func TestClassifyErrMsg_Nominal(t *testing.T) {
	tests := []struct {
		msg  string
		want ErrCategory
	}{
		{"", ""},
		{"InputValidationError: commit_chapter failed", CatToolSchemaValidation},
		{"The required parameter `chapter` is missing", CatToolSchemaValidation},
		{"type is expected as `integer`", CatToolSchemaValidation},
		{"ArgsParseError: unexpected token at offset 42", CatToolArgsMalformed},
		{"cannot parse json: invalid character 'x'", CatToolArgsMalformed},
		{"unexpected token at input", CatToolArgsMalformed},
		{"received malformed JSON arguments", CatToolArgsMalformed},
		{"received invalid JSON arguments", CatToolArgsMalformed},
		{"unexpected end of JSON input", CatToolArgsMalformed},
		{"semantic validation failed: chapter does not exist", CatToolSemanticVal},
		{"write before read: chapter 5 not yet drafted", CatToolSemanticVal},
		{"no such chapter: 99", CatToolSemanticVal},
		{"noop: no changes needed", CatToolNoop},
		{"no changes: nothing to update", CatToolNoop},
		{"nothing to update", CatToolNoop},
		{"nothing to do here", CatToolNoop},
		{"style review exhausted after max attempts", CatStyleReviewExhausted},
		{"style review max limit reached", CatStyleReviewExhausted},
		{"评审已耗尽", CatStyleReviewExhausted},
		{"max turns (100) reached", CatMaxTurns},
		{"provider stream idle timeout", CatStreamIdle},
		{"unknown random error", ""},
		// "invalid character" without "looking for" must NOT match (narrowing)
		{"some provider error: invalid character in response", ""},
	}
	for _, tt := range tests {
		t.Run(tt.msg[:min(len(tt.msg), 40)], func(t *testing.T) {
			got := ClassifyErrMsg(tt.msg)
			if got != tt.want {
				t.Errorf("ClassifyErrMsg(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

// TestClassifyErrMsg_Exhaustive 保证每个要求的类别至少有一个匹配用例。
func TestClassifyErrMsg_Exhaustive(t *testing.T) {
	required := []ErrCategory{
		CatToolArgsMalformed,
		CatToolSchemaValidation,
		CatToolSemanticVal,
		CatToolNoop,
		CatStyleReviewExhausted,
		CatMaxTurns,
	}
	covered := make(map[ErrCategory]bool)
	for _, msg := range []string{
		"cannot parse json",
		"received malformed JSON arguments",
		"InputValidationError: test",
		"semantic validation failed",
		"noop",
		"style review exhausted",
		"评审已耗尽",
		"max turns reached",
	} {
		covered[ClassifyErrMsg(msg)] = true
	}
	for _, cat := range required {
		if !covered[cat] {
			t.Errorf("category %q has no test coverage", cat)
		}
	}
}

// TestExtractSessionErrors_Basic 验证从会话消息中提取脱敏错误观测。
func TestExtractSessionErrors_Basic(t *testing.T) {
	msgs := buildTestToolErrorSession(t)
	obs := extractSessionErrors("writer-ch07", msgs)

	if len(obs) == 0 {
		t.Fatal("期望提取到错误观测，但结果为空")
	}

	// 验证第一个观测的字段
	o := obs[0]
	if o.Agent != "writer-ch07" {
		t.Errorf("Agent = %q, want %q", o.Agent, "writer-ch07")
	}
	if o.Category != CatToolSchemaValidation {
		t.Errorf("Category = %q, want %q", o.Category, CatToolSchemaValidation)
	}
	// Without tool_name metadata, Tool must default to "unknown"
	if o.Tool != "unknown" {
		t.Errorf("Tool = %q, want %q (metadata missing tool_name)", o.Tool, "unknown")
	}
}

// TestExtractSessionErrors_ToolNameFromMetadata 验证当 tool 结果消息携带 tool_name
// 元数据时，优先从元数据绑定工具名（而非从前的 assistant message 猜测）。
func TestExtractSessionErrors_ToolNameFromMetadata(t *testing.T) {
	msgs := []agentcore.Message{
		{
			Role: agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "commit_chapter",
					Args: json.RawMessage(`{"chapter":"7"}`),
				}),
			},
			StopReason: agentcore.StopReasonToolUse,
		},
		{
			Role:    agentcore.RoleTool,
			Content: []agentcore.ContentBlock{agentcore.TextBlock("InputValidationError: chapter must be int")},
			Metadata: map[string]any{
				"is_error":  true,
				"tool_name": "commit_chapter",
			},
		},
	}

	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 1 {
		t.Fatalf("期望 1 条错误观测，得到 %d", len(obs))
	}
	if obs[0].Tool != "commit_chapter" {
		t.Errorf("Tool = %q, want %q (should be from metadata)", obs[0].Tool, "commit_chapter")
	}
}

// TestExtractSessionErrors_ParallelCallsUnknownWithoutMetadata 验证并行工具调用时，
// 当 tool result 无 tool_name/tool_call_id 元数据时，Tool 标记为 unknown 且 args
// 指纹为空（不取第一个调用的参数）。
func TestExtractSessionErrors_ParallelCallsUnknownWithoutMetadata(t *testing.T) {
	// 单条 assistant 消息包含两个并行工具调用，但 tool result 没有 tool_name/tool_call_id 元数据
	msgs := []agentcore.Message{
		{
			Role: agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "commit_chapter",
					ID:   "call-1",
					Args: json.RawMessage(`{"chapter":1}`),
				}),
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "check_consistency",
					ID:   "call-2",
					Args: json.RawMessage(`{"chapter":1}`),
				}),
			},
			StopReason: agentcore.StopReasonToolUse,
		},
		// 第一个工具结果 — 无 tool_name/tool_call_id 元数据
		{
			Role:     agentcore.RoleTool,
			Content:  []agentcore.ContentBlock{agentcore.TextBlock("InputValidationError: chapter must be int")},
			Metadata: map[string]any{"is_error": true},
		},
		// 第二个工具结果 — 无 tool_name/tool_call_id 元数据
		{
			Role:     agentcore.RoleTool,
			Content:  []agentcore.ContentBlock{agentcore.TextBlock("ArgsParseError: cannot parse args")},
			Metadata: map[string]any{"is_error": true},
		},
	}

	obs := extractSessionErrors("writer", msgs)
	for i, o := range obs {
		if o.Tool != "unknown" {
			t.Errorf("obs[%d] Tool = %q, want %q (should be unknown without metadata)", i, o.Tool, "unknown")
		}
		// 并行调用且无 tool_call_id 时，args 必须为空（不取第一个调用）
		if o.ArgsHash != "" {
			t.Errorf("obs[%d] ArgsHash = %q, want empty (parallel call without tool_call_id)", i, o.ArgsHash)
		}
		if o.ArgsBytes != 0 {
			t.Errorf("obs[%d] ArgsBytes = %d, want 0 (parallel call without tool_call_id)", i, o.ArgsBytes)
		}
	}
}

// TestExtractSessionErrors_ParallelCallsWithToolCallID 验证并行工具调用时，
// 通过 tool_call_id 精确匹配工具调用的 args 指纹，不取第一个调用的参数。
func TestExtractSessionErrors_ParallelCallsWithToolCallID(t *testing.T) {
	msgs := []agentcore.Message{
		{
			Role: agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "commit_chapter",
					ID:   "call-aaa",
					Args: json.RawMessage(`{"chapter":1,"summary":"first"}`),
				}),
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "check_consistency",
					ID:   "call-bbb",
					Args: json.RawMessage(`{"chapter":1,"summary":"second"}`),
				}),
			},
			StopReason: agentcore.StopReasonToolUse,
		},
		// 第一个工具结果 — 有精确的 tool_call_id
		{
			Role:    agentcore.RoleTool,
			Content: []agentcore.ContentBlock{agentcore.TextBlock("InputValidationError: chapter must be int")},
			Metadata: map[string]any{
				"is_error":     true,
				"tool_name":    "commit_chapter",
				"tool_call_id": "call-aaa",
			},
		},
		// 第二个工具结果 — 有精确的 tool_call_id
		{
			Role:    agentcore.RoleTool,
			Content: []agentcore.ContentBlock{agentcore.TextBlock("ArgsParseError: cannot parse args")},
			Metadata: map[string]any{
				"is_error":     true,
				"tool_name":    "check_consistency",
				"tool_call_id": "call-bbb",
			},
		},
	}

	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 2 {
		t.Fatalf("期望 2 条错误观测，得到 %d", len(obs))
	}

	// 验证第一条观测：commit_chapter 的 args 指纹应匹配其特有参数
	if obs[0].Tool != "commit_chapter" {
		t.Errorf("obs[0] Tool = %q, want %q", obs[0].Tool, "commit_chapter")
	}
	if obs[0].ArgsHash == "" {
		t.Error("obs[0] ArgsHash should be populated (matched via tool_call_id)")
	}
	if obs[0].ArgsBytes == 0 {
		t.Error("obs[0] ArgsBytes should be > 0")
	}

	// 验证第二条观测：check_consistency 的 args 指纹不应与第一条相同
	if obs[1].Tool != "check_consistency" {
		t.Errorf("obs[1] Tool = %q, want %q", obs[1].Tool, "check_consistency")
	}
	if obs[1].ArgsHash == "" {
		t.Error("obs[1] ArgsHash should be populated (matched via tool_call_id)")
	}
	if obs[0].ArgsHash == obs[1].ArgsHash {
		t.Error("两个并行调用的 ArgsHash 不应相同（参数不同但都被取了第一个调用）")
	}
}

// TestExtractSessionErrors_SingletonCall 验证单工具调用且无 tool_call_id 时，
// 正确 fallback 到该唯一调用（不返回空 args）。
func TestExtractSessionErrors_SingletonCall(t *testing.T) {
	msgs := []agentcore.Message{
		{
			Role: agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "commit_chapter",
					ID:   "call-1",
					Args: json.RawMessage(`{"chapter":1,"summary":"solo"}`),
				}),
			},
			StopReason: agentcore.StopReasonToolUse,
		},
		{
			Role:    agentcore.RoleTool,
			Content: []agentcore.ContentBlock{agentcore.TextBlock("InputValidationError: chapter must be int")},
			Metadata: map[string]any{
				"is_error":  true,
				"tool_name": "commit_chapter",
				// 无 tool_call_id — 但 assistant 只有单调用，应 fallback
			},
		},
	}

	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 1 {
		t.Fatalf("期望 1 条错误观测，得到 %d", len(obs))
	}
	if obs[0].ArgsHash == "" {
		t.Error("singleton call without tool_call_id should still get args via fallback")
	}
	if obs[0].ArgsBytes == 0 {
		t.Error("singleton call should have positive ArgsBytes")
	}
}

// TestExtractSessionErrors_SensitiveNotLeaked 验证提取结果不含敏感正文。
func TestExtractSessionErrors_SensitiveNotLeaked(t *testing.T) {
	msgs := buildTestToolErrorSession(t)
	obs := extractSessionErrors("writer-ch07", msgs)

	if ErrObsContainsSensitive(obs) {
		t.Error("提取的错误观测包含敏感内容")
	}

	// 额外检查：不允许任何字段包含小说正文
	sentinel := "雪夜里主角揭穿了反派的惊天阴谋这是机密正文"
	for _, o := range obs {
		if containsSensitiveField(o, sentinel) {
			t.Errorf("观测包含敏感正文: %+v", o)
		}
	}
}

// TestExtractSessionErrors_MultipleErrors 验证多个错误消息会被正确收集。
func TestExtractSessionErrors_MultipleErrors(t *testing.T) {
	var msgs []agentcore.Message
	// 追加 assistant message（带工具调用）
	msgs = append(msgs, commitCallMsg(`"7"`))
	// 追加 tool error
	msgs = append(msgs, errResultMsg("InputValidationError: chapter must be int"))
	// 第二条 assistant（带另一个工具调用）
	msgs = append(msgs, commitCallMsg(`"abc"`))
	// 第二条 tool error
	msgs = append(msgs, errResultMsg("ArgsParseError: cannot parse chapter"))

	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 2 {
		t.Fatalf("期望 2 条错误观测，得到 %d: %+v", len(obs), obs)
	}

	// 第一条应为 schema validation
	if obs[0].Category != CatToolSchemaValidation {
		t.Errorf("第一条错误类别 = %q, want %q", obs[0].Category, CatToolSchemaValidation)
	}
	// 第二条应为 args malformed
	if obs[1].Category != CatToolArgsMalformed {
		t.Errorf("第二条错误类别 = %q, want %q", obs[1].Category, CatToolArgsMalformed)
	}
}

// TestAggregateErrObs_Dedup 验证同类错误会被聚合计数。
func TestAggregateErrObs_Dedup(t *testing.T) {
	base := ErrObs{
		Agent:    "writer",
		Tool:     "commit_chapter",
		Category: CatToolSchemaValidation,
		ArgsHash: "a1b2c3d4e5f6a7b8",
	}
	obs := []ErrObs{base, base, base}
	agg := aggregateErrObs(obs)
	if len(agg) != 1 {
		t.Fatalf("聚合后应有 1 条，得到 %d", len(agg))
	}
	if agg[0].Count != 3 {
		t.Errorf("Count = %d, want 3", agg[0].Count)
	}
}

func TestAggregateErrObs_SortByCount(t *testing.T) {
	obs := []ErrObs{
		{Agent: "a", Tool: "t1", Category: CatToolArgsMalformed, ArgsHash: "h1"},
		{Agent: "a", Tool: "t1", Category: CatToolArgsMalformed, ArgsHash: "h1"},
		{Agent: "a", Tool: "t2", Category: CatToolSchemaValidation, ArgsHash: "h2"},
		{Agent: "a", Tool: "t2", Category: CatToolSchemaValidation, ArgsHash: "h2"},
		{Agent: "a", Tool: "t2", Category: CatToolSchemaValidation, ArgsHash: "h2"},
	}
	agg := aggregateErrObs(obs)
	if len(agg) != 2 {
		t.Fatalf("聚合后应有 2 条，得到 %d", len(agg))
	}
	// 第一条应是最多的
	if agg[0].Count != 3 || agg[0].Tool != "t2" {
		t.Errorf("第一个应是最频繁的错误: %+v", agg[0])
	}
}

// TestErrObsContainsSensitive_DetectsProse 验证敏感检测助手能发现正文。
func TestErrObsContainsSensitive_DetectsProse(t *testing.T) {
	safe := []ErrObs{
		{Agent: "writer", Tool: "commit_chapter", Category: CatToolSchemaValidation},
	}
	if ErrObsContainsSensitive(safe) {
		t.Error("安全观测不应报告敏感内容")
	}
	unsafe := []ErrObs{
		{Agent: "writer", Tool: "commit_chapter", Category: CatToolSchemaValidation, ArgsHash: "雪夜正文"},
	}
	if !ErrObsContainsSensitive(unsafe) {
		t.Error("含有敏感内容的观测应被检测到")
	}
}

// TestExtractSessionErrors_FinishReason 验证非错误消息不会产生观测且 StopReason 被正确记录。
func TestExtractSessionErrors_FinishReason(t *testing.T) {
	var msgs []agentcore.Message
	// 正常助理消息（无工具调用，不应影响）
	msgs = append(msgs, agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{
			{Type: agentcore.ContentText, Text: "I'll check the chapter"},
		},
		StopReason: agentcore.StopReasonStop,
	})
	// 助理消息带工具调用
	msgs = append(msgs, agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{
			agentcore.ToolCallBlock(agentcore.ToolCall{
				Name: "commit_chapter",
				Args: json.RawMessage(`{"chapter":7}`),
			}),
		},
		StopReason: agentcore.StopReasonToolUse,
	})
	// tool error
	msgs = append(msgs, agentcore.Message{
		Role:     agentcore.RoleTool,
		Content:  []agentcore.ContentBlock{agentcore.TextBlock("InputValidationError: chapter must be int")},
		Metadata: map[string]any{"is_error": true},
	})

	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 1 {
		t.Fatalf("期望 1 条错误，得到 %d", len(obs))
	}
	o := obs[0]
	if o.FinishReason != "toolUse" {
		t.Errorf("FinishReason = %q, want %q", o.FinishReason, "toolUse")
	}
	if !o.ToolUseDone {
		t.Error("ToolUseDone 应为 true")
	}
}

func TestExtractSessionErrors_SkipsNonError(t *testing.T) {
	var msgs []agentcore.Message
	msgs = append(msgs, agentcore.Message{
		Role:    agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.TextBlock("hello")},
	})
	msgs = append(msgs, agentcore.Message{
		Role:    agentcore.RoleTool,
		Content: []agentcore.ContentBlock{agentcore.TextBlock("ok, done")},
	})
	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 0 {
		t.Errorf("非错误消息不应产生观测，得到 %d", len(obs))
	}
}

func TestExtractSessionErrors_SkipsUnclassified(t *testing.T) {
	var msgs []agentcore.Message
	msgs = append(msgs, agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{
			agentcore.ToolCallBlock(agentcore.ToolCall{
				Name: "some_tool",
				Args: json.RawMessage(`{}`),
			}),
		},
	})
	msgs = append(msgs, agentcore.Message{
		Role:     agentcore.RoleTool,
		Content:  []agentcore.ContentBlock{agentcore.TextBlock("generic error")},
		Metadata: map[string]any{"is_error": true},
	})
	obs := extractSessionErrors("writer", msgs)
	if len(obs) != 0 {
		t.Errorf("未分类错误不应产生观测，得到 %d", len(obs))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// buildTestToolErrorSession 构造一个典型会话：助理要求提交章节 → 校验失败。
func buildTestToolErrorSession(t *testing.T) []agentcore.Message {
	t.Helper()
	return []agentcore.Message{
		{
			Role: agentcore.RoleAssistant,
			Content: []agentcore.ContentBlock{
				agentcore.ToolCallBlock(agentcore.ToolCall{
					Name: "commit_chapter",
					Args: json.RawMessage(`{"chapter":"7","content":"# test"}`),
				}),
			},
			StopReason: agentcore.StopReasonToolUse,
		},
		{
			Role:     agentcore.RoleTool,
			Content:  []agentcore.ContentBlock{agentcore.TextBlock("InputValidationError: commit_chapter failed due to the following issues:\nThe required parameter `chapter` is missing")},
			Metadata: map[string]any{"is_error": true},
		},
	}
}

func commitCallMsg(chapterRaw string) agentcore.Message {
	return agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{
			agentcore.ToolCallBlock(agentcore.ToolCall{
				Name: "commit_chapter",
				Args: json.RawMessage(`{"chapter":` + chapterRaw + `,"content":"placeholder"}`),
			}),
		},
		StopReason: agentcore.StopReasonToolUse,
	}
}

func errResultMsg(msg string) agentcore.Message {
	return agentcore.Message{
		Role:     agentcore.RoleTool,
		Content:  []agentcore.ContentBlock{agentcore.TextBlock(msg)},
		Metadata: map[string]any{"is_error": true},
	}
}

func containsSensitiveField(o ErrObs, sentinel string) bool {
	if o.Agent == sentinel ||
		o.Tool == sentinel ||
		string(o.Category) == sentinel ||
		o.SchemaPath == sentinel ||
		o.Expected == sentinel ||
		o.Received == sentinel ||
		o.FinishReason == sentinel ||
		o.ArgsHash == sentinel {
		return true
	}
	return false
}
