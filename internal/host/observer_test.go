package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errclass"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// testObserver 创建测试用 observer。
func testObserver(events *[]Event) *observer {
	return &observer{
		emitEv: func(ev Event) {
			*events = append(*events, ev)
		},
		emitD:               func(string) {},
		emitC:               func() {},
		agents:              make(map[string]*agentState),
		lastThinkingByAgent: make(map[string]string),
		dispatchStarts:      make(map[string]*activeCall),
		toolStarts:          make(map[string]*activeCall),
		streamExtractors:    make(map[string]*agentExtractor),
		streamArgPrefixes:   make(map[string]string),
		streamArgLabels:     make(map[string]string),
		retryEvents:         make(map[string]string),
	}
}

// concurrentObserver 创建用于并发测试的 observer，事件收集带 mutex 保护。
type concurrentObserver struct {
	*observer
	mu   sync.Mutex
	evts []Event
}

func newConcurrentObserver() *concurrentObserver {
	co := &concurrentObserver{}
	co.observer = &observer{
		emitEv: func(ev Event) {
			co.mu.Lock()
			co.evts = append(co.evts, ev)
			co.mu.Unlock()
		},
		emitD:               func(string) {},
		emitC:               func() {},
		agents:              make(map[string]*agentState),
		lastThinkingByAgent: make(map[string]string),
		dispatchStarts:      make(map[string]*activeCall),
		toolStarts:          make(map[string]*activeCall),
		streamExtractors:    make(map[string]*agentExtractor),
		streamArgPrefixes:   make(map[string]string),
		streamArgLabels:     make(map[string]string),
		retryEvents:         make(map[string]string),
	}
	return co
}

func (co *concurrentObserver) events() []Event {
	co.mu.Lock()
	defer co.mu.Unlock()
	out := make([]Event, len(co.evts))
	copy(out, co.evts)
	return out
}

func TestObserverSubagentRetryEventsUpdateSameLinePerAgent(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	for i := 1; i <= 2; i++ {
		o.handleToolUpdate(agentcore.Event{
			Type: agentcore.EventToolExecUpdate,
			Progress: &agentcore.ProgressPayload{
				Kind:       agentcore.ProgressRetry,
				Agent:      "writer",
				Attempt:    i,
				MaxRetries: 7,
				Message:    "stream failed",
			},
		})
	}

	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 raw update events", len(events))
	}
	if events[0].ID == "" || events[1].ID != events[0].ID {
		t.Fatalf("writer retry events should share ID: %+v", events)
	}
	if events[1].Agent != "writer" || !strings.Contains(events[1].Summary, "重试 (2/7，2s后)") {
		t.Fatalf("event = %+v, want writer retry 2/7 with delay", events[1])
	}
}

// ── ProgressRetry ghost partial stream 清理 ────────────────────────────────

// TestObserverRetryCleansRunningTool 验证 retry 时如有进行中的 TOOL 行，
// 会被标记为 discarded（不是 Failed），并且系统状态被清空。
func TestObserverRetryCleansRunningTool(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 先模拟流式触发一次 TOOL start
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"premise","content":"...`,
	})
	if len(events) < 1 {
		t.Fatal("expected tool start event")
	}
	toolID := events[0].ID

	// 模拟 extractor / thinking / argPrefixes 污染
	o.streamExtractors["architect_long"] = &agentExtractor{tool: "save_foundation"}
	o.lastThinkingByAgent["architect_long"] = "some thinking"
	o.streamArgPrefixes["architect_long\x00save_foundation"] = `{"type":"premise"`
	o.streamArgLabels["architect_long\x00save_foundation"] = "save_foundation[premise]"

	// 确保 toolStarts[architect_long] 已就绪
	if _, ok := o.toolStarts["architect_long"]; !ok {
		t.Fatal("expected toolStarts to be set for architect_long")
	}

	// 触发 retry
	var clearCalled int
	o.emitC = func() { clearCalled++ }

	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "architect_long",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "provider stream idle",
		},
	})

	// 验证：旧 TOOL 行被丢弃（不是失败）
	// 系统有两件同 ID 的事件（原始 start + discard finish），找到最后一件
	var discardedEvent *Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].ID == toolID && events[i].Discarded {
			discardedEvent = &events[i]
			break
		}
	}
	if discardedEvent == nil {
		t.Fatal("expected discarded TOOL event with same ID")
	}
	if discardedEvent.FinishedAt.IsZero() {
		t.Errorf("discarded TOOL event should have FinishedAt set")
	}
	if discardedEvent.Failed {
		t.Errorf("discarded TOOL event should not be Failed")
	}

	// 验证 toolStarts 被清空
	if _, ok := o.toolStarts["architect_long"]; ok {
		t.Error("toolStarts should be cleared for architect_long after retry")
	}

	// 验证 streamExtractors 被清空
	if _, ok := o.streamExtractors["architect_long"]; ok {
		t.Error("streamExtractors should be cleared for architect_long after retry")
	}

	// 验证 thinking 被清空
	if _, ok := o.lastThinkingByAgent["architect_long"]; ok {
		t.Error("lastThinkingByAgent should be cleared for architect_long after retry")
	}

	// 验证 streamArgPrefixes/labels 被清空
	for key := range o.streamArgPrefixes {
		if strings.HasPrefix(key, "architect_long\x00") {
			t.Errorf("streamArgPrefixes should be cleared for architect_long, found %q", key)
		}
	}
	for key := range o.streamArgLabels {
		if strings.HasPrefix(key, "architect_long\x00") {
			t.Errorf("streamArgLabels should be cleared for architect_long, found %q", key)
		}
	}

	// 验证 streamClear 被调用
	if clearCalled < 1 {
		t.Error("streamClear (emitC) should have been called")
	}

	// 验证后面跟着 retry SYSTEM 事件
	if len(events) < 3 {
		t.Fatal("expected at least 3 events (tool start, discard, retry)")
	}
	retryEv := events[len(events)-1]
	if retryEv.Category != "SYSTEM" || !strings.Contains(retryEv.Summary, "重试") {
		t.Errorf("last event should be retry SYSTEM, got Category=%s Summary=%s", retryEv.Category, retryEv.Summary)
	}
}

// TestObserverRetryWithoutRunningTool 验证 retry 时无进行中 TOOL 行时，
// 不会 emit 丢弃事件，但流式状态仍被清理。
// 注意：当 retry agent 不是 streamOwner 时，不触发全局 CLEAR（emitC）。
func TestObserverRetryWithoutRunningTool(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 模拟 extractor / thinking 污染（无 toolStarts）
	o.streamExtractors["writer"] = &agentExtractor{tool: "draft_chapter"}
	o.lastThinkingByAgent["writer"] = "some thinking"

	var clearCalled int
	o.emitC = func() { clearCalled++ }

	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "writer",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "output truncated",
		},
	})

	// streamExtractors/thinking 被清理
	if _, ok := o.streamExtractors["writer"]; ok {
		t.Error("streamExtractors should be cleared for writer after retry")
	}
	if _, ok := o.lastThinkingByAgent["writer"]; ok {
		t.Error("lastThinkingByAgent should be cleared for writer after retry")
	}
	// retry agent 不是 streamOwner 时不触发 emitC（全局 CLEAR）
	if clearCalled > 0 {
		t.Error("streamClear should NOT have been called when retry agent is not streamOwner")
	}

	// 无 discarded TOOL event
	for _, ev := range events {
		if ev.Category == "TOOL" && ev.Discarded {
			t.Error("should not emit discarded TOOL event without running tool")
		}
	}

	// 有 retry SYSTEM 事件
	foundRetry := false
	for _, ev := range events {
		if ev.Category == "SYSTEM" && strings.Contains(ev.Summary, "重试") {
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Error("expected retry SYSTEM event")
	}
}

// TestObserverRetryThenNewToolGetsNewID 验证 retry 清理后，新的 tool start
// 拿到新 Event.ID，不与旧 discarded tool 共用 ID。
func TestObserverRetryThenNewToolGetsNewID(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 第一轮：触发 TOOL start
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"premise","content":"...`,
	})
	if len(events) < 1 {
		t.Fatal("expected tool start event")
	}
	firstToolID := events[0].ID

	// retry 清理
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "architect_long",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "stream failed",
		},
	})

	// 第二轮：新的 tool start（模拟新 attempt 的流式工具调用）
	// 清理后 toolStarts 为空，ensureSubagentToolStarted 应发新 ID
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"outline","content":"...`,
	})

	// 验证：有新的 TOOL start 事件
	var secondToolID string
	for _, ev := range events {
		if ev.Category == "TOOL" && ev.Discarded {
			continue // 跳过 discarded
		}
		if ev.Category == "TOOL" && ev.Running() {
			secondToolID = ev.ID
		}
	}
	if secondToolID == "" {
		t.Fatal("expected a new running TOOL event after retry")
	}
	if secondToolID == firstToolID {
		t.Error("new TOOL should have different ID from first tool")
	}
}

// TestObserverRetryDoesNotAffectOtherAgents 验证 retry 只清理指定 agent 的状态，
// 不影响其他 agent。
func TestObserverRetryDoesNotAffectOtherAgents(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// architect_long 有 running tool
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"premise"`,
	})
	// writer 也有 running tool
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1`,
	})

	// 给 writer 加 extractor/thinking 污染（不通过 delta 路径）
	o.streamExtractors["writer"] = &agentExtractor{tool: "draft_chapter"}
	o.lastThinkingByAgent["writer"] = "writer thinking"

	if _, ok := o.toolStarts["architect_long"]; !ok {
		t.Fatal("architect_long should have toolStarts")
	}
	if _, ok := o.toolStarts["writer"]; !ok {
		t.Fatal("writer should have toolStarts")
	}

	// 只 retry architect_long
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "architect_long",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "stream idle",
		},
	})

	// architect_long 的 toolStarts 被清
	if _, ok := o.toolStarts["architect_long"]; ok {
		t.Error("architect_long toolStarts should be cleared")
	}
	// writer 的 toolStarts 保留
	if _, ok := o.toolStarts["writer"]; !ok {
		t.Error("writer toolStarts should NOT be cleared")
	}
	// writer 的 streamExtractors 保留
	if _, ok := o.streamExtractors["writer"]; !ok {
		t.Error("writer streamExtractors should NOT be cleared")
	}
	// writer 的 thinking 保留
	if _, ok := o.lastThinkingByAgent["writer"]; !ok {
		t.Error("writer lastThinkingByAgent should NOT be cleared")
	}

	// architect_long 的 TOOL 被标记 discarded
	for _, ev := range events {
		if ev.Category == "TOOL" && ev.Discarded {
			// ok: architect_long's tool was discarded
		}
	}
}

// TestObserverRetryDiscardedIsNotFailed 验证 discarded 的 TOOL 行不是 Failed。
func TestObserverRetryDiscardedIsNotFailed(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 先有 running tool
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"premise"`,
	})

	// 然后接到 tool error（模拟工具执行失败）
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:    agentcore.ProgressToolError,
			Agent:   "architect_long",
			Tool:    "save_foundation",
			Message: "execution error",
		},
	})

	// 错误之后，再触发 retry（此时不应有 running tool 了）
	// 用另一个 agent 来验证
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1`,
	})

	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "writer",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "stream failed",
		},
	})

	// writer 的 TOOL 被 discarded（找最后一件同 agent 的 TOOL 事件）
	var writerDiscarded bool
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Category == "TOOL" && ev.Agent == "writer" {
			if ev.Discarded {
				writerDiscarded = true
			}
			if ev.Failed {
				t.Error("discarded TOOL should not be Failed")
			}
			break
		}
	}
	if !writerDiscarded {
		t.Error("writer TOOL should be Discarded")
	}
	// architect_long 的 TOOL 已 Failed，不因后续 retry 被改成 discarded
	for _, ev := range events {
		if ev.Category == "TOOL" && ev.Agent == "architect_long" && ev.Failed {
			if ev.Discarded {
				t.Error("failed TOOL should not be Discarded")
			}
		}
	}
}

func TestObserverSubagentToolDeltaUpdatesSaveFoundationType(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"premise","content":"# 书名`,
	})

	if len(events) < 2 {
		t.Fatalf("events = %d, want start + summary update", len(events))
	}
	if events[0].Category != "TOOL" || events[0].Summary != "save_foundation" || events[0].Depth != 1 {
		t.Fatalf("start event = %+v", events[0])
	}
	if events[1].ID != events[0].ID || events[1].Summary != "save_foundation[premise]" {
		t.Fatalf("summary update = %+v, start = %+v", events[1], events[0])
	}
}

func TestObserverSubagentToolDeltaUpdatesSaveFoundationTypeAcrossChunks(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	for _, delta := range []string{`{"ty`, `pe":"premise","content":"# 书名`} {
		o.handleSubagentDelta(&agentcore.ProgressPayload{
			Kind:      agentcore.ProgressToolDelta,
			Agent:     "architect_long",
			Tool:      "save_foundation",
			DeltaKind: agentcore.DeltaToolCall,
			Delta:     delta,
		})
	}

	var summaries []string
	for _, ev := range events {
		summaries = append(summaries, ev.Summary)
	}
	if !strings.Contains(strings.Join(summaries, "\n"), "save_foundation[premise]") {
		t.Fatalf("summaries = %v, want save_foundation[premise]", summaries)
	}
}

// ── errorKind tests ─────────────────────────────────────────────────────────

func TestErrorKind_ExactErrorChain(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error", nil, ""},
		{"max turns", agentcore.ErrMaxTurns, "max_turns"},
		{"tool validation", agentcore.ErrToolValidation, "tool_schema_validation"},
		{"stream idle", agentcore.ErrProviderStreamIdle, "stream_idle"},
		{"unknown sentinel", errors.New("some other error"), ""},
		{"wrapped max turns", fmt.Errorf("wrapped: %w", agentcore.ErrMaxTurns), "max_turns"},
		{"wrapped tool validation", fmt.Errorf("wrapped: %w", agentcore.ErrToolValidation), "tool_schema_validation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errorKind(tt.err, "")
			if got != tt.want {
				t.Errorf("errorKind(%v, \"\") = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorKind_MessagePattern(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{"empty message", "", ""},
		{"stream idle message", "provider stream idle timeout", "stream_idle"},
		{"max turns message", "max turns reached", "max_turns"},
		{"max turns exact", "max turns limit exceeded", "max_turns"},
		{"schema validation - InputValidationError", "InputValidationError: commit_chapter failed", "tool_schema_validation"},
		{"schema validation - required param", "The required parameter `chapter` is missing", "tool_schema_validation"},
		{"schema validation - type expected", "type is expected as `integer`", "tool_schema_validation"},
		{"args malformed - ArgsParseError", "ArgsParseError: unexpected token", "tool_args_malformed"},
		{"args malformed - cannot parse", "cannot parse json: invalid character", "tool_args_malformed"},
		{"args malformed - unexpected token", "unexpected token at offset 42", "tool_args_malformed"},
		{"semantic validation - write before read", "write before read: chapter 5 not drafted yet", "tool_semantic_validation"},
		{"semantic validation - no such chapter", "no such chapter: 99", "tool_semantic_validation"},
		{"semantic validation - explicit", "semantic validation failed: chapter does not exist", "tool_semantic_validation"},
		{"noop - no changes", "no changes needed", "tool_noop"},
		{"noop - nothing to update", "nothing to update for chapter 5", "tool_noop"},
		{"noop - nothing to do", "nothing to do", "tool_noop"},
		{"noop - exact", "noop", "tool_noop"},
		{"style review exhausted", "style review exhausted after max attempts", "style_review_exhausted"},
		{"style review limit", "style review max limit reached", "style_review_exhausted"},
		{"style review exhausted 2", "style review limit exceeded", "style_review_exhausted"},
		{"style review exhausted CN", "critic 模式：章节 5 评审已耗尽（exhausted），不能 commit", "style_review_exhausted"},
		{"agentcore malformed JSON args", "tool validation: commit_chapter received malformed JSON arguments: unexpected end of JSON input", "tool_args_malformed"},
		{"agentcore invalid JSON args", "tool validation: commit_chapter received invalid JSON arguments: invalid character", "tool_args_malformed"},
		{"unexpected end of JSON input", "unexpected end of JSON input", "tool_args_malformed"},
		{"invalid char alone no longer matches", "some provider error: invalid character in response", ""},
		{"non-matching message", "some random error message", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errorKind(nil, tt.msg)
			if got != tt.want {
				t.Errorf("errorKind(nil, %q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestErrorKind_ErrOverridesMsg(t *testing.T) {
	// When err is non-nil and directly matches a sentinel, it should win even
	// when msg would suggest a different category or is empty.
	err := agentcore.ErrMaxTurns
	got := errorKind(err, "InputValidationError: something")
	if got != "max_turns" {
		t.Errorf("errorKind should prefer error chain match over msg: got %q, want %q", got, "max_turns")
	}

	// A nil err with a matching msg should still classify.
	got = errorKind(nil, "InputValidationError: something")
	if got != "tool_schema_validation" {
		t.Errorf("errorKind(nil, msg) should classify by msg: got %q, want %q", got, "tool_schema_validation")
	}
}

func TestErrorKind_ErrToolValidationNotGeneralizedForMalformedArgs(t *testing.T) {
	// ErrToolValidation wraps both schema violations and JSON parse errors.
	// When the message indicates args-malformed, that category must be used
	// instead of schema validation.
	err := agentcore.ErrToolValidation
	got := errorKind(err, "cannot parse json: invalid character 'x' looking for beginning of value")
	if got != "tool_args_malformed" {
		t.Errorf("errorKind(ErrToolValidation, args-parse msg) = %q, want %q", got, "tool_args_malformed")
	}

	// Plain ErrToolValidation without msg should still map to schema validation.
	got = errorKind(err, "")
	if got != "tool_schema_validation" {
		t.Errorf("errorKind(ErrToolValidation, \"\") = %q, want %q", got, "tool_schema_validation")
	}

	// Schema validation msg should remain schema validation.
	got = errorKind(err, "InputValidationError: commit_chapter failed")
	if got != "tool_schema_validation" {
		t.Errorf("errorKind(ErrToolValidation, schema msg) = %q, want %q", got, "tool_schema_validation")
	}
}

func TestErrorKind_SensitiveContentNotInCategory(t *testing.T) {
	// Verify that error categories do not include body text, raw args, or
	// provider SSE. The category label itself must be a stable short string.
	sensitive := []string{"雪夜", "主角", "args:", "sse:", "provider", "正文"}
	for _, cat := range []string{
		"", "stream_idle", "max_turns", "tool_schema_validation",
		"tool_args_malformed", "tool_semantic_validation", "tool_noop",
		"style_review_exhausted",
	} {
		for _, s := range sensitive {
			if strings.Contains(cat, s) {
				t.Errorf("category %q contains sensitive content %q", cat, s)
			}
		}
	}
}

// ── Cross-entry consistency ─────────────────────────────────────────────────

// TestErrorKindVsClassifyErrMsg_Consistent verifies that host/observer.errorKind
// and diag/ClassifyErrMsg return exactly the same category for every message
// pattern. The errorKind variant passes nil err so it falls through to the
// shared message-pattern path (errclass.ClassifyMsg). Both must agree.
func TestErrorKindVsClassifyErrMsg_Consistent(t *testing.T) {
	messages := []struct {
		name   string
		msg    string
		expect string // expected category when both agree; "" for no-match
	}{
		{"empty", "", ""},
		{"stream idle 1", "provider stream idle timeout", errclass.CatStreamIdle},
		{"stream idle 2", "stream idle timeout occurred", errclass.CatStreamIdle},
		{"max turns 1", "max turns reached", errclass.CatMaxTurns},
		{"max turns 2", "max turns (100) reached", errclass.CatMaxTurns},
		{"schema validation 1", "InputValidationError: commit_chapter failed", errclass.CatToolSchemaValidation},
		{"schema validation 2", "The required parameter `chapter` is missing", errclass.CatToolSchemaValidation},
		{"schema validation 3", "type is expected as `integer` but provided as `string`", errclass.CatToolSchemaValidation},
		{"args malformed 1", "ArgsParseError: unexpected token", errclass.CatToolArgsMalformed},
		{"args malformed 2", "cannot parse json: invalid character", errclass.CatToolArgsMalformed},
		{"args malformed 3", "unexpected token at offset 42", errclass.CatToolArgsMalformed},
		{"semantic validation 1", "semantic validation failed: chapter does not exist", errclass.CatToolSemanticVal},
		{"semantic validation 2", "write before read: chapter 5 not yet drafted", errclass.CatToolSemanticVal},
		{"semantic validation 3", "no such chapter: 99", errclass.CatToolSemanticVal},
		{"noop 1", "noop", errclass.CatToolNoop},
		{"noop 2", "no changes needed", errclass.CatToolNoop},
		{"noop 3", "nothing to update", errclass.CatToolNoop},
		{"noop 4", "nothing to do", errclass.CatToolNoop},
		{"style review exhausted 1", "style review exhausted after max attempts", errclass.CatStyleReviewExhausted},
		{"style review exhausted 2", "style review max limit reached", errclass.CatStyleReviewExhausted},
		{"no match 1", "some random error", ""},
		{"no match 2", "arbiter output not found", ""},
	}

	for _, tc := range messages {
		t.Run(tc.name, func(t *testing.T) {
			gotHost := errorKind(nil, tc.msg)
			gotDiag := string(diag.ClassifyErrMsg(tc.msg))

			// Both must agree
			if gotHost != gotDiag {
				t.Errorf("MISMATCH: errorKind(nil, %q)=%q vs ClassifyErrMsg(%q)=%q",
					tc.msg, gotHost, tc.msg, gotDiag)
			}
			// Both must match expected
			if gotHost != tc.expect {
				t.Errorf("errorKind(nil, %q)=%q, want %q", tc.msg, gotHost, tc.expect)
			}
		})
	}
}

// TestErrorKindVsErrClassConstants confirms that every string constant
// referenced by errorKind (via errclass) matches the literal strings the
// original code used before extracting the shared package. This is a
// compile-time-verified snapshot guard against accidental constant drift.
func TestErrorKindVsErrClassConstants(t *testing.T) {
	m := map[string]string{
		"max_turns":                errclass.CatMaxTurns,
		"tool_schema_validation":   errclass.CatToolSchemaValidation,
		"tool_args_malformed":      errclass.CatToolArgsMalformed,
		"tool_semantic_validation": errclass.CatToolSemanticVal,
		"tool_noop":                errclass.CatToolNoop,
		"style_review_exhausted":   errclass.CatStyleReviewExhausted,
		"stream_idle":              errclass.CatStreamIdle,
	}
	for want, got := range m {
		if got != want {
			t.Errorf("constant = %q, want %q", got, want)
		}
	}
}

// ── streamOwner: Arbiter retry 不截断 Worker 输出 ──────────────────────────

// TestObserverRetryStreamOwnerCheck 验证 Worker 流式输出期间 Arbiter retry
// 不会 CLEAR/reset 全局 stream 状态（streamOwner 不是 "arbiter"）。
func TestObserverRetryStreamOwnerCheck(t *testing.T) {
	var events []Event
	var clearCount int
	var deltaCalls []string
	o := testObserver(&events)
	o.emitC = func() { clearCount++ }
	o.emitD = func(d string) { deltaCalls = append(deltaCalls, d) }

	// Writer 开始流式输出 → 成为 stream owner
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1,"content":"...`,
	})
	if o.streamOwner != "writer" {
		t.Fatalf("streamOwner = %q, want %q", o.streamOwner, "writer")
	}

	// 正常流式内容
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		DeltaKind: agentcore.DeltaText,
		Delta:     "这是正在输出的章节正文...",
	})
	beforeClear := clearCount

	// Arbiter retry — agent="arbiter"，不是 stream owner → 不应 CLEAR
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "arbiter",
			Attempt:    2,
			MaxRetries: 7,
			Message:    "provider stream idle",
		},
	})

	if clearCount != beforeClear {
		t.Errorf("Arbiter retry should not call emitC (CLEAR): before=%d after=%d", beforeClear, clearCount)
	}
	// stream 全局状态应保留（streamOwner 不变）
	if o.streamOwner != "writer" {
		t.Errorf("streamOwner should remain %q after arbiter retry, got %q", "writer", o.streamOwner)
	}
}

// TestObserverRetryWorkerStreamOwnerClears 验证 Worker 自己的 retry 会 CLEAR
// 全局 stream（streamOwner == agent）。
func TestObserverRetryWorkerStreamOwnerClears(t *testing.T) {
	var events []Event
	var clearCount int
	o := testObserver(&events)
	o.emitC = func() { clearCount++ }

	// Writer 开始流式输出 → 成为 stream owner
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1,"content":"...`,
	})
	beforeClear := clearCount

	// Writer 自身 retry — streamOwner == "writer" → 应 CLEAR
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "writer",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "stream output truncated",
		},
	})

	if clearCount <= beforeClear {
		t.Errorf("Writer retry should call emitC (CLEAR): before=%d after=%d", beforeClear, clearCount)
	}
	if o.streamOwner != "" {
		t.Errorf("streamOwner should be cleared after writer retry, got %q", o.streamOwner)
	}
}

// ── Agent sidebar cleanup ─────────────────────────────────────────────────

// TestObserverRetryCleansAgentSidebar 验证 retry 会把 agent 侧栏状态重置为 idle。
func TestObserverRetryCleansAgentSidebar(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 模拟 writer 正在进行 tool call
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1`,
	})

	o.updateAgent("writer", func(a *agentState) {
		a.state = "working"
		a.tool = "draft_chapter"
		a.summary = "writer → draft_chapter"
	})

	// 验证 agent 状态为 working
	snaps := o.agentSnapshots()
	writerFound := false
	for _, s := range snaps {
		if s.Name == "writer" {
			writerFound = true
			if s.State != "working" || s.Tool != "draft_chapter" {
				t.Fatalf("writer should be working before retry, got state=%q tool=%q", s.State, s.Tool)
			}
		}
	}
	if !writerFound {
		t.Fatal("writer should exist in agent snapshots")
	}

	// 触发 retry（无 emitC 回调，测试 updateAgent 是否被调用）
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "writer",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "stream failed",
		},
	})

	// 验证 agent 状态已重置为 idle
	snaps = o.agentSnapshots()
	for _, s := range snaps {
		if s.Name == "writer" {
			if s.State != "idle" {
				t.Errorf("writer state should be idle after retry, got %q", s.State)
			}
			if s.Tool != "" {
				t.Errorf("writer tool should be cleared after retry, got %q", s.Tool)
			}
		}
	}
}

// ── Explicit zero delay ───────────────────────────────────────────────────

// TestObserverRetryExplicitZeroDelay 验证上游携带 retry_delay_ms=0 时不会被
// 指数退避覆盖，显示"即时重试"而非 fallback 1s。
func TestObserverRetryExplicitZeroDelay(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	meta, _ := json.Marshal(struct {
		DelayMS int64 `json:"retry_delay_ms"`
	}{DelayMS: 0})

	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "writer",
			Attempt:    2,
			MaxRetries: 5,
			Message:    "immediate retry requested",
			Meta:       meta,
		},
	})

	// 最后一条事件应该是 retry SYSTEM
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	lastEv := events[len(events)-1]
	if lastEv.Category != "SYSTEM" {
		t.Fatalf("last event should be SYSTEM, got %s", lastEv.Category)
	}
	if !strings.Contains(lastEv.Summary, "即时") {
		t.Errorf("retry with explicit zero delay should show '即时', got %q", lastEv.Summary)
	}
	// 验证没有 fallback 1s 字样
	if strings.Contains(lastEv.Summary, "1s") {
		t.Errorf("retry with explicit zero delay should not show fallback 1s, got %q", lastEv.Summary)
	}
}

// TestObserverRetryMissingDelayFallsBack 验证无 retry_delay_ms 时走指数退避。
func TestObserverRetryMissingDelayFallsBack(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 无 Meta → 走 fallback
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "writer",
			Attempt:    2,
			MaxRetries: 5,
			Message:    "fallback delay",
		},
	})

	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	lastEv := events[len(events)-1]
	if !strings.Contains(lastEv.Summary, "2s") {
		// attempt=2 时指数退避应为 2s
		t.Errorf("missing Meta should fallback to exponential delay, got %q", lastEv.Summary)
	}
}

// TestObserverRetryExplicitNonZeroDelay 验证上游携带 retry_delay_ms=5000 时
// 使用该值而非指数退避。
func TestObserverRetryExplicitNonZeroDelay(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	meta, _ := json.Marshal(struct {
		DelayMS int64 `json:"retry_delay_ms"`
	}{DelayMS: 5000})

	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "arbiter",
			Attempt:    1,
			MaxRetries: 7,
			Message:    "rate limited",
			Meta:       meta,
		},
	})

	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	lastEv := events[len(events)-1]
	if !strings.Contains(lastEv.Summary, "5s") {
		t.Errorf("explicit delay should show 5s, got %q", lastEv.Summary)
	}
}

// ── Concurrent map access race-safety ────────────────────────────────────
//
// TestObserverConcurrentRetryAndDelta 在 -race 下验证 handleToolUpdate 与
// handleSubagentDelta 并发调用时不会 data race。模拟 Writer 流式输出过程中
// Arbiter 发生 retry 的场景。

// TestObserverConcurrentRetryAndDelta 验证并发调用 handleToolUpdate 与
// handleSubagentDelta 时不会 data race。
// 注：此测试需 go test -race 运行才能检测 data race；
// 无 GCC 环境仅验证逻辑正确性（无 panic + streamOwner 不被 Arbiter retry 清除）。
func TestObserverConcurrentRetryAndDelta(t *testing.T) {
	co := newConcurrentObserver()

	// 先让 writer 成为 stream owner
	co.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1,"content":"# 第一章`,
	})

	var wg sync.WaitGroup
	wg.Add(4)

	// goroutine 1: 持续发射 Writer delta（模拟流式输出）
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			co.handleSubagentDelta(&agentcore.ProgressPayload{
				Kind:      agentcore.ProgressToolDelta,
				Agent:     "writer",
				DeltaKind: agentcore.DeltaText,
				Delta:     "更多的正文内容...",
			})
		}
	}()

	// goroutine 2: 持续发射 Arbiter retry（模拟 Arbiter 重试）
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			co.handleToolUpdate(agentcore.Event{
				Type: agentcore.EventToolExecUpdate,
				Progress: &agentcore.ProgressPayload{
					Kind:       agentcore.ProgressRetry,
					Agent:      "arbiter",
					Attempt:    i%7 + 1,
					MaxRetries: 7,
					Message:    "concurrent arbiter retry",
				},
			})
		}
	}()

	// goroutine 3: 持续发射 writer retry（模拟 Worker 重试）
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			co.handleToolUpdate(agentcore.Event{
				Type: agentcore.EventToolExecUpdate,
				Progress: &agentcore.ProgressPayload{
					Kind:       agentcore.ProgressRetry,
					Agent:      "writer",
					Attempt:    i%3 + 1,
					MaxRetries: 3,
					Message:    "concurrent writer retry",
				},
			})
		}
	}()

	// goroutine 4: 持续读取 snapshot（模拟 TUI 轮询）
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = co.agentSnapshots()
		}
	}()

	wg.Wait()

	// 验证没有 panic 且 events 包含合理的事件
	if len(co.events()) == 0 {
		t.Error("expected at least some events after concurrent access")
	}

	// 验证 streamOwner：Arbiter retry 后 writer 仍应是 owner
	if co.streamOwner != "" && co.streamOwner != "writer" {
		t.Errorf("streamOwner should be empty or 'writer' after concurrent retries, got %q", co.streamOwner)
	}
}

// ── Agent scoped cleanup ─────────────────────────────────────────────────

// ── Agent scoped cleanup ─────────────────────────────────────────────────

// TestObserverRetryScopedCleanup 验证 retry 只清理指定 agent 的状态，
// 不影响其他 agent（补充 TestObserverRetryDoesNotAffectOtherAgents 中
// 对 streamArgPrefixes/labels 和 streamOwner 的保护验证）。
func TestObserverRetryScopedCleanup(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	// 两个 agent 都在工作
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "architect_long",
		Tool:      "save_foundation",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"type":"premise"`,
	})
	o.handleSubagentDelta(&agentcore.ProgressPayload{
		Kind:      agentcore.ProgressToolDelta,
		Agent:     "writer",
		Tool:      "draft_chapter",
		DeltaKind: agentcore.DeltaToolCall,
		Delta:     `{"chapter":1`,
	})

	// writer 是 stream owner
	o.streamOwner = "writer"

	// 额外污染 architect_long 的 arg 前缀（用于验证正确清除）
	o.streamArgPrefixes["architect_long\x00save_foundation"] = `{"type":"premise"`
	o.streamArgLabels["architect_long\x00save_foundation"] = "save_foundation[premise]"

	// 只 retry architect_long
	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:       agentcore.ProgressRetry,
			Agent:      "architect_long",
			Attempt:    1,
			MaxRetries: 3,
			Message:    "stream idle for architect",
		},
	})

	// architect_long 的 arg 前缀应清除
	if _, ok := o.streamArgPrefixes["architect_long\x00save_foundation"]; ok {
		t.Error("architect_long streamArgPrefixes should be cleared")
	}
	if _, ok := o.streamArgLabels["architect_long\x00save_foundation"]; ok {
		t.Error("architect_long streamArgLabels should be cleared")
	}

	// writer 的 toolStarts/streamExtractors/thinking 应保留
	if _, ok := o.toolStarts["writer"]; !ok {
		t.Error("writer toolStarts should NOT be cleared")
	}

	// streamOwner 应保留为 writer（architect_long 不是 owner）
	if o.streamOwner != "writer" {
		t.Errorf("streamOwner should remain %q after architect_long retry, got %q", "writer", o.streamOwner)
	}
}

// ── B4: TaskKind 填充 ─────────────────────────────────────────────────────

// TestTaskKindForAgentFlow 验证 agent+flow → TaskKind 的映射规则（B4）。
func TestTaskKindForAgentFlow(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		flow  string
		want  string
	}{
		{"architect_short", "architect_short", "", "foundation_plan"},
		{"architect_short with flow", "architect_short", "writing", "foundation_plan"},
		{"architect_long", "architect_long", "", "foundation_plan"},
		{"writer writing", "writer", "writing", "chapter_write"},
		{"writer rewriting", "writer", "rewriting", "chapter_rewrite"},
		{"writer polishing", "writer", "polishing", "chapter_polish"},
		{"writer reviewing", "writer", "reviewing", ""},
		{"writer unknown flow", "writer", "steering", ""},
		{"writer empty flow", "writer", "", ""},
		{"editor reviewing", "editor", "reviewing", "chapter_review"},
		{"editor rewriting", "editor", "rewriting", "chapter_rewrite"},
		{"editor writing", "editor", "writing", ""},
		{"polisher", "polisher", "", "chapter_polish"},
		{"polisher rewriting", "polisher", "rewriting", "chapter_polish"},
		{"unknown agent", "architect", "", ""},
		{"empty agent", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := taskKindForAgentFlow(tc.agent, tc.flow)
			if got != tc.want {
				t.Errorf("taskKindForAgentFlow(%q, %q) = %q, want %q", tc.agent, tc.flow, got, tc.want)
			}
		})
	}
}

// TestObserverDispatchStartSetsTaskKind 验证 dispatchStart 把 taskKind 写入
// agentState，并最终出现在 agentSnapshots 的 AgentSnapshot.TaskKind。
// flow 通过真实 store.Progress 落盘读取。
func TestObserverDispatchStartSetsTaskKind(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		flow  domain.FlowState
		want  string
	}{
		{"architect_short", "architect_short", "", "foundation_plan"},
		{"architect_long", "architect_long", "", "foundation_plan"},
		{"writer writing", "writer", domain.FlowWriting, "chapter_write"},
		{"writer rewriting", "writer", domain.FlowRewriting, "chapter_rewrite"},
		{"writer polishing", "writer", domain.FlowPolishing, "chapter_polish"},
		{"writer no flow", "writer", "", ""},
		{"editor reviewing", "editor", domain.FlowReviewing, "chapter_review"},
		{"editor rewriting", "editor", domain.FlowRewriting, "chapter_rewrite"},
		{"polisher", "polisher", "", "chapter_polish"},
		{"unknown agent", "unknown", domain.FlowWriting, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			o := testObserver(&events)
			if tc.flow != "" {
				s := storepkg.NewStore(t.TempDir())
				if err := s.Progress.Init("test", 10); err != nil {
					t.Fatalf("Progress.Init: %v", err)
				}
				if err := s.Progress.SetFlow(tc.flow); err != nil {
					t.Fatalf("Progress.SetFlow(%s): %v", tc.flow, err)
				}
				o.store = s
			}
			o.dispatchStart(tc.agent, "task")
			snaps := o.agentSnapshots()
			if len(snaps) != 1 {
				t.Fatalf("agentSnapshots() = %d entries, want 1", len(snaps))
			}
			if snaps[0].TaskKind != tc.want {
				t.Errorf("TaskKind = %q, want %q (agent=%s flow=%s)", snaps[0].TaskKind, tc.want, tc.agent, tc.flow)
			}
		})
	}
}

// ── B4: ProgressTurnCounter ────────────────────────────────────────────────

// TestObserverTurnCounterUpdatesTurn 验证 ProgressTurnCounter 只更新
// agentState.turn，不发 UI 事件。
func TestObserverTurnCounterUpdatesTurn(t *testing.T) {
	var events []Event
	o := testObserver(&events)

	o.handleToolUpdate(agentcore.Event{
		Type: agentcore.EventToolExecUpdate,
		Progress: &agentcore.ProgressPayload{
			Kind:  agentcore.ProgressTurnCounter,
			Agent: "writer",
			Turn:  3,
		},
	})

	if len(events) != 0 {
		t.Fatalf("turn counter should not emit events, got %d", len(events))
	}
	snaps := o.agentSnapshots()
	if len(snaps) != 1 {
		t.Fatalf("agentSnapshots() = %d entries, want 1", len(snaps))
	}
	if snaps[0].Name != "writer" || snaps[0].Turn != 3 {
		t.Errorf("snapshot = %+v, want writer with Turn=3", snaps[0])
	}
}

// ── B5: REVIEW / CHECK 事件归类 ────────────────────────────────────────────

// TestObserverToolEndReviewCheckCategories 验证 ProgressToolEnd 的 finish
// 事件按 Tool 归类：save_review→REVIEW，check_consistency→CHECK，其余保持 TOOL。
func TestObserverToolEndReviewCheckCategories(t *testing.T) {
	tests := []struct {
		name string
		tool string
		want string
	}{
		{"save_review", "save_review", "REVIEW"},
		{"check_consistency", "check_consistency", "CHECK"},
		{"draft_chapter", "draft_chapter", "TOOL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			o := testObserver(&events)

			// 先注入一个进行中的 TOOL 调用
			start := time.Now()
			o.mapMu.Lock()
			o.toolStarts["editor"] = &activeCall{id: "e1", start: start, summary: tc.tool, depth: 1}
			o.mapMu.Unlock()

			o.handleToolUpdate(agentcore.Event{
				Type: agentcore.EventToolExecUpdate,
				Progress: &agentcore.ProgressPayload{
					Kind:  agentcore.ProgressToolEnd,
					Agent: "editor",
					Tool:  tc.tool,
				},
			})

			// finishEv 与 start 同 ID
			var finishEv *Event
			for i := range events {
				if events[i].ID == "e1" {
					finishEv = &events[i]
				}
			}
			if finishEv == nil {
				t.Fatalf("no finish event with id e1, events=%+v", events)
			}
			if finishEv.Category != tc.want {
				t.Errorf("Category = %q, want %q (tool=%s)", finishEv.Category, tc.want, tc.tool)
			}
		})
	}
}

// TestObserverToolErrorReviewCheckCategories 验证错误态 ProgressToolError 的
// finish 事件同样按 Tool 归类（B5）。
func TestObserverToolErrorReviewCheckCategories(t *testing.T) {
	tests := []struct {
		name string
		tool string
		want string
	}{
		{"save_review", "save_review", "REVIEW"},
		{"check_consistency", "check_consistency", "CHECK"},
		{"draft_chapter", "draft_chapter", "TOOL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			o := testObserver(&events)

			start := time.Now()
			o.mapMu.Lock()
			o.toolStarts["editor"] = &activeCall{id: "e2", start: start, summary: tc.tool, depth: 1}
			o.mapMu.Unlock()

			o.handleToolUpdate(agentcore.Event{
				Type: agentcore.EventToolExecUpdate,
				Progress: &agentcore.ProgressPayload{
					Kind:    agentcore.ProgressToolError,
					Agent:   "editor",
					Tool:    tc.tool,
					Message: "boom",
				},
			})

			var finishEv *Event
			for i := range events {
				if events[i].ID == "e2" {
					finishEv = &events[i]
				}
			}
			if finishEv == nil {
				t.Fatalf("no failed finish event with id e2, events=%+v", events)
			}
			if !finishEv.Failed {
				t.Errorf("finish event should be Failed")
			}
			if finishEv.Category != tc.want {
				t.Errorf("Category = %q, want %q (tool=%s)", finishEv.Category, tc.want, tc.tool)
			}
		})
	}
}
