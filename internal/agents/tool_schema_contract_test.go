package agents

// agentcore v1.7.13 的真实 Runner/Tool Schema 契约测试。
// 锁死 Schema 层验证行为：类型/枚举不匹配时工具零执行，完整 schema 验证通过才执行。
//
// 所有测试经 subagent.Runner.Run 驱动的真实派发路径，不走工具 Execute 直调：
//   - 路由逻辑：schema → validateToolArgs → ToolValidationError → tool_result(IsError)，
//     不绕过 schema 直接点工具 fn。
//   - 使用真实生产工具的 Schema() 方法（除非工具本身 zero-value 不依赖 store），
//     不复制简化 schema。
//   - 通过 execCounter 包裹 Execute 来计数，不依赖完整业务前置条件。
//
// 限制：
//   - 不验证业务逻辑校验器（Validator 接口），仅 Schema 层。
//   - 不测试 StrictSchemaTool 接口（provider 侧 strict mode）。
//   - 不测试 ArgsInvalid 路径（上游 stream parse error 捕获）。

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/tools"
)

// ---------------------------------------------------------------------------
// schema 契约专用 mock 模型（内联到本文件以避免跨测试文件依赖）
// ---------------------------------------------------------------------------

type schemaTestModel struct {
	fn  func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error)
	idx int64
}

func (m *schemaTestModel) take(msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
	i := int(atomic.AddInt64(&m.idx, 1) - 1)
	return m.fn(i, msgs)
}

func (m *schemaTestModel) calls() int { return int(atomic.LoadInt64(&m.idx)) }

func (m *schemaTestModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return m.take(msgs)
}

func (m *schemaTestModel) GenerateStream(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	resp, err := m.take(msgs)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *schemaTestModel) SupportsTools() bool { return true }

// ---------------------------------------------------------------------------
// 内联助手（跨测试文件依赖去除：不引用 agentcore_contract_test.go 的符号）
// ---------------------------------------------------------------------------

// assistantToolCall 构造一条 assistant 消息，含一次工具调用。
func assistantToolCall(name string, args string) agentcore.Message {
	return agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.ToolCallBlock(agentcore.ToolCall{
			ID: "tc-" + name, Name: name, Args: json.RawMessage(args),
		})},
		StopReason: agentcore.StopReasonToolUse,
	}
}

// assistantText 构造一条纯文本 assistant 消息。
func assistantText(text string, stop agentcore.StopReason) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(text)},
		StopReason: stop,
	}
}

// ---------------------------------------------------------------------------
// 辅助：可计数执行的 tool wrapper
// ---------------------------------------------------------------------------

// execCounter 记录工具 Execute 被调用的次数。
type execCounter struct {
	count atomic.Int64
}

func (c *execCounter) load() int { return int(c.count.Load()) }
func (c *execCounter) reset()    { c.count.Store(0) }
func (c *execCounter) inc()      { c.count.Add(1) }

// makeTool 创建一个命名工具，其 Execute 调用会递增 counter。
// 测试通过 counter.load() 判断工具是否被实际执行。
func makeTool(name string, sch map[string]any, counter *execCounter) agentcore.Tool {
	return agentcore.NewFuncTool(name, "schema-contract: "+name, sch,
		func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			counter.inc()
			return json.RawMessage(`"ok"`), nil
		})
}

// runSchema 用给定配置经 subagent.Runner.Run 跑一次合约测试。
// 返回执行错误——期望正常结束的用例自行断言 nil。
func runSchema(t *testing.T, cfg subagent.Config) error {
	t.Helper()
	_, err := subagent.NewRunner(cfg).Run(context.Background(), cfg.Name, "schema-contract")
	return err
}

// toolCallTurn 构建第 0 轮模型响应：调用工具 + 期望模型在第 1 轮以 text end_turn 收尾。
// 这是大多数 schema 测试的双轮模式。
func toolCallThenEnd(name string, rawArgs string) *schemaTestModel {
	return &schemaTestModel{fn: func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		switch i {
		case 0:
			return &agentcore.LLMResponse{Message: assistantToolCall(name, rawArgs)}, nil
		default:
			return &agentcore.LLMResponse{Message: assistantText("done", agentcore.StopReasonStop)}, nil
		}
	}}
}

// ============================================================================
// 测试用例
// ============================================================================

// 1) commit_chapter.feedback: schema 定义 type=object。
// 使用真实 commit_chapter 生产 Schema() 验证。
// 传 string → validateToolArgs 拒绝 → 工具零执行。
// 传合法 object → 验证通过 → 工具执行。
func TestSchemaContract_CommitChapterFeedback_StringRejected_ObjectAccepted(t *testing.T) {
	sch := (*tools.CommitChapterTool)(nil).Schema()

	t.Run("feedback_as_string_rejected", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("commit_chapter", sch, &counter)
		model := toolCallThenEnd("commit_chapter", `{
			"chapter": 1, "summary": "s", "characters": ["a"], "key_events": ["b"],
			"feedback": "i am a string, not an object"
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "writer", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 0 {
			t.Fatalf("feedback=string 时工具必须零执行，got %d 次执行", n)
		}
	})

	t.Run("feedback_as_object_executes", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("commit_chapter", sch, &counter)
		model := toolCallThenEnd("commit_chapter", `{
			"chapter": 1, "summary": "s", "characters": ["a"], "key_events": ["b"],
			"feedback": {"deviation": "偏离了", "suggestion": "建议调整"}
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "writer", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 1 {
			t.Fatalf("feedback=object 时工具应执行 1 次，got %d", n)
		}
	})
}

// 2) save_volume_summary.key_events: 使用真实生产 Schema()。
// 传 string → 拒绝；传合法 array → 执行。
func TestSchemaContract_VolumeSummaryKeyEvents_StringRejected_ArrayAccepted(t *testing.T) {
	sch := (*tools.SaveVolumeSummaryTool)(nil).Schema()

	t.Run("key_events_as_string_rejected", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("save_volume_summary", sch, &counter)
		model := toolCallThenEnd("save_volume_summary", `{
			"volume": 1, "title": "t", "summary": "s",
			"key_events": "this should be an array, not a string"
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "editor", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 0 {
			t.Fatalf("key_events=string 时工具必须零执行，got %d 次执行", n)
		}
	})

	t.Run("key_events_as_array_executes", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("save_volume_summary", sch, &counter)
		model := toolCallThenEnd("save_volume_summary", `{
			"volume": 1, "title": "t", "summary": "s",
			"key_events": ["event1", "event2"]
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "editor", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 1 {
			t.Fatalf("key_events=array 时工具应执行 1 次，got %d", n)
		}
	})
}

// 3) save_review.issues[].type / issues[].severity: 使用真实生产 Schema()。
//   - issues[i].type 是 enum（consistency/character/...aesthetic）→ 缺 type 时 IssueMissing
//   - issues[i].severity 是 enum（critical/error/warning）→ 非法值触发 IssueValue
//
// 两种非法 schema 都使工具零执行。
func TestSchemaContract_SaveReviewIssues_RejectsBadNestedEnum(t *testing.T) {
	sch := (*tools.SaveReviewTool)(nil).Schema()

	t.Run("issue_missing_type_field_rejected", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("save_review", sch, &counter)
		// issues[0] 缺少必需的 type 字段
		model := toolCallThenEnd("save_review", `{
			"chapter": 1, "scope": "chapter", "verdict": "accept", "summary": "ok",
			"dimensions": [
				{"dimension": "consistency", "score": 85, "comment": "good"},
				{"dimension": "character", "score": 85, "comment": "good"},
				{"dimension": "pacing", "score": 85, "comment": "good"},
				{"dimension": "continuity", "score": 85, "comment": "good"},
				{"dimension": "foreshadow", "score": 85, "comment": "good"},
				{"dimension": "hook", "score": 85, "comment": "good"},
				{"dimension": "aesthetic", "score": 85, "comment": "good"}
			],
			"issues": [
				{"severity": "error", "description": "desc", "evidence": "ev"}
			]
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "editor", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 0 {
			t.Fatalf("issue 缺 type 字段时工具必须零执行，got %d 次执行", n)
		}
	})

	t.Run("issue_invalid_severity_rejected", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("save_review", sch, &counter)
		// issues[0].severity 非法值，不在 enum 中
		model := toolCallThenEnd("save_review", `{
			"chapter": 1, "scope": "chapter", "verdict": "accept", "summary": "ok",
			"dimensions": [
				{"dimension": "consistency", "score": 85, "comment": "good"},
				{"dimension": "character", "score": 85, "comment": "good"},
				{"dimension": "pacing", "score": 85, "comment": "good"},
				{"dimension": "continuity", "score": 85, "comment": "good"},
				{"dimension": "foreshadow", "score": 85, "comment": "good"},
				{"dimension": "hook", "score": 85, "comment": "good"},
				{"dimension": "aesthetic", "score": 85, "comment": "good"}
			],
			"issues": [
				{"type": "consistency", "severity": "illegal_value", "description": "desc", "evidence": "ev"}
			]
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "editor", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 0 {
			t.Fatalf("severity 非法 enum 值时工具必须零执行，got %d 次执行", n)
		}
	})

	t.Run("issue_valid_entries_executes", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("save_review", sch, &counter)
		model := toolCallThenEnd("save_review", `{
			"chapter": 1, "scope": "chapter", "verdict": "accept", "summary": "ok",
			"dimensions": [
				{"dimension": "consistency", "score": 85, "comment": "g"},
				{"dimension": "character", "score": 85, "comment": "g"},
				{"dimension": "pacing", "score": 85, "comment": "g"},
				{"dimension": "continuity", "score": 85, "comment": "g"},
				{"dimension": "foreshadow", "score": 85, "comment": "g"},
				{"dimension": "hook", "score": 85, "comment": "g"},
				{"dimension": "aesthetic", "score": 85, "comment": "g"}
			],
			"issues": [
				{"type": "consistency", "severity": "error", "description": "desc", "evidence": "ev"}
			]
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "editor", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 1 {
			t.Fatalf("合法 issue 时工具应执行 1 次，got %d", n)
		}
	})
}

//  4. 字符串整数（如 chapter="46"）按 v1.7.13 schema 验证被拒：
//     schema type=integer，JSON 实际类型为 string → matchesSchemaType 返回 false，
//     且 v1.7.13 移除了 coerceArg（string→integer 自动转换），故工具零执行。
//
//     业务层 commit_chapter.Execute 有 normalizeIntegerStringFields 做安全网，
//     但 schema 验证在它之前已拦截。
func TestSchemaContract_StringIntegerRejected(t *testing.T) {
	sch := (*tools.CommitChapterTool)(nil).Schema()

	t.Run("chapter_as_string_rejected", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("commit_chapter", sch, &counter)
		model := toolCallThenEnd("commit_chapter", `{
			"chapter": "46", "summary": "test", "characters": ["a"], "key_events": ["b"]
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "writer", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 0 {
			t.Fatalf("chapter=string 时工具必须零执行，got %d 次执行", n)
		}
	})

	t.Run("chapter_as_integer_executes", func(t *testing.T) {
		var counter execCounter
		tool := makeTool("commit_chapter", sch, &counter)
		model := toolCallThenEnd("commit_chapter", `{
			"chapter": 46, "summary": "test", "characters": ["a"], "key_events": ["b"]
		}`)
		if err := runSchema(t, subagent.Config{
			Name: "writer", Description: "schema-contract",
			Model: model, SystemPrompt: "test", MaxTurns: 5,
			Tools: []agentcore.Tool{tool},
		}); err != nil {
			t.Fatalf("run should not fail fatally: %v", err)
		}
		if n := counter.load(); n != 1 {
			t.Fatalf("chapter=integer 时工具应执行 1 次，got %d", n)
		}
	})
}

// 5) 验证测试使用的确实是真实生产工具的 Schema()。
// 如果手写复制简化 schema 与实际 Schema() 产生偏差，本测试捕获差异。
func TestSchemaContract_UsesRealProductionSchema(t *testing.T) {
	t.Run("commit_chapter_has_feedback_object", func(t *testing.T) {
		sch := (*tools.CommitChapterTool)(nil).Schema()
		props, _ := sch["properties"].(map[string]any)
		if props == nil {
			t.Fatal("production schema must have properties")
		}
		fb, ok := props["feedback"]
		if !ok {
			t.Fatal("production schema must include feedback property")
		}
		fbObj, ok := fb.(map[string]any)
		if !ok {
			t.Fatal("feedback must be an object schema")
		}
		if fbObj["type"] != "object" {
			t.Fatalf("feedback type = %v, want object", fbObj["type"])
		}
	})

	t.Run("save_volume_summary_required_fields", func(t *testing.T) {
		sch := (*tools.SaveVolumeSummaryTool)(nil).Schema()
		raw, ok := sch["required"]
		if !ok {
			t.Fatal("production schema must have required field")
		}
		req, ok := raw.([]string)
		if !ok {
			t.Fatalf("required is %T, want []string", raw)
		}
		hasVolume, hasTitle, hasSummary, hasEvents := false, false, false, false
		for _, s := range req {
			switch s {
			case "volume":
				hasVolume = true
			case "title":
				hasTitle = true
			case "summary":
				hasSummary = true
			case "key_events":
				hasEvents = true
			}
		}
		if !hasVolume || !hasTitle || !hasSummary || !hasEvents {
			t.Fatalf("production save_volume_summary must require volume/title/summary/key_events, got %v", req)
		}
	})

	t.Run("save_review_has_dimensions_and_issues", func(t *testing.T) {
		sch := (*tools.SaveReviewTool)(nil).Schema()
		props, _ := sch["properties"].(map[string]any)
		if props == nil {
			t.Fatal("production schema must have properties")
		}
		for _, field := range []string{"dimensions", "issues", "verdict", "summary", "scope"} {
			if _, ok := props[field]; !ok {
				t.Fatalf("production save_review schema must include %q", field)
			}
		}
	})
}
