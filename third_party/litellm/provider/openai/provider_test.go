package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/voocel/litellm"
	"github.com/voocel/litellm/internal/testgolden"
	"github.com/voocel/litellm/retry"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuildRequestTextImageToolsAndOptions(t *testing.T) {
	provider := mustProvider(t)
	maxTokens := 256
	temp := 0.2
	tool := mustTool(t, "lookup", "Lookup data.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
		"required": []string{"q"},
	})
	tool.Strict = litellm.StrictEnabled

	wire, err := provider.buildRequest(&litellm.Request{
		Model:       "gpt-4.1",
		MaxTokens:   &maxTokens,
		Temperature: &temp,
		Messages: []litellm.Message{
			litellm.System("You are helpful."),
			litellm.User(
				litellm.Text("describe"),
				litellm.ImageURL("https://example.test/image.png"),
			),
			litellm.Assistant(litellm.ToolUseBlock{
				ID:        "call_1",
				Name:      "lookup",
				Arguments: litellm.MustJSONRaw(map[string]any{"q": "x"}),
			}),
			litellm.ToolResultText("call_1", "result"),
		},
		Tools: []litellm.Tool{tool},
		ProviderOptions: litellm.ProviderOptions{
			"frequency_penalty": 0.4,
			"metadata":          map[string]any{"tenant": "acme"},
			"modalities":        []any{"text"},
		},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	testgolden.AssertJSON(t, "../../testdata/openai/chat_request_basic.golden.json", wire)

	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	jsonText := string(data)
	for _, want := range []string{
		`"max_tokens":256`,
		`"temperature":0.2`,
		`"type":"image_url"`,
		`"tool_calls"`,
		`"tool_call_id":"call_1"`,
		`"strict":true`,
		`"additionalProperties":false`,
		`"frequency_penalty":0.4`,
		`"metadata":{"tenant":"acme"}`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("wire JSON missing %s:\n%s", want, jsonText)
		}
	}
}

func TestBuildRequestOpenAIProviderOptions(t *testing.T) {
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "gpt-4o-audio-preview",
		Messages: []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{
			ProviderOptionAudio: map[string]any{
				"format": "mp3",
				"voice":  "alloy",
			},
			ProviderOptionModeration:        map[string]any{"model": "omni-moderation-latest"},
			ProviderOptionModalities:        []any{"text", "audio"},
			ProviderOptionParallelToolCalls: true,
			ProviderOptionSafetyIdentifier:  "safe-user",
			ProviderOptionVerbosity:         "low",
			ProviderOptionWebSearchOptions: map[string]any{
				"search_context_size": "low",
			},
		},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	jsonText := string(data)
	for _, want := range []string{
		`"audio":{"format":"mp3","voice":"alloy"}`,
		`"moderation":{"model":"omni-moderation-latest"}`,
		`"modalities":["text","audio"]`,
		`"parallel_tool_calls":true`,
		`"safety_identifier":"safe-user"`,
		`"verbosity":"low"`,
		`"web_search_options":{"search_context_size":"low"}`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("wire JSON missing %s:\n%s", want, jsonText)
		}
	}
}

func TestBuildRequestRoundTripsTextReasoningBlockHistory(t *testing.T) {
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model: "gpt-4.1",
		Messages: []litellm.Message{
			litellm.Assistant(
				litellm.ReasoningBlock{Text: "hidden state"},
				litellm.ToolUseBlock{ID: "call_1", Name: "lookup", Arguments: litellm.MustJSONRaw(map[string]any{})},
			),
		},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if len(wire.Messages) != 1 || wire.Messages[0].ReasoningContent != "hidden state" {
		t.Fatalf("messages = %#v", wire.Messages)
	}
}

func TestBuildRequestRejectsOpaqueReasoningBlockHistory(t *testing.T) {
	provider := mustProvider(t)
	_, err := provider.buildRequest(&litellm.Request{
		Model: "gpt-4.1",
		Messages: []litellm.Message{
			litellm.Assistant(litellm.ReasoningBlock{Text: "hidden state", Signature: "sig"}),
		},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "does not accept signed") {
		t.Fatalf("expected opaque reasoning block error, got %v", err)
	}
}

func TestBuildRequestRejectsUnknownProviderOption(t *testing.T) {
	provider := mustProvider(t)
	_, err := provider.buildRequest(&litellm.Request{
		Model:           "gpt-4.1",
		Messages:        []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{"unknown": true},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "unsupported provider option") {
		t.Fatalf("expected provider option error, got %v", err)
	}
}

func TestBuildRequestRejectsInvalidPromptCacheRetention(t *testing.T) {
	provider := mustProvider(t)
	_, err := provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{
			"prompt_cache_retention": "forever",
		},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "prompt_cache_retention") {
		t.Fatalf("expected prompt cache retention error, got %v", err)
	}
}

func TestBuildStreamRequestAcceptsStreamOptions(t *testing.T) {
	provider := mustProvider(t)
	includeObfuscation := false
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{
			ProviderOptionStreamOptions: map[string]any{
				"include_usage":       true,
				"include_obfuscation": includeObfuscation,
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.StreamOptions == nil || !wire.StreamOptions.IncludeUsage || wire.StreamOptions.IncludeObfuscation == nil || *wire.StreamOptions.IncludeObfuscation {
		t.Fatalf("stream_options = %#v", wire.StreamOptions)
	}

	_, err = provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{
			ProviderOptionStreamOptions: map[string]any{"include_obfuscation": false},
		},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "requires stream request") {
		t.Fatalf("expected non-stream stream_options error, got %v", err)
	}
}

func TestChatReturnsStructuredValidationError(t *testing.T) {
	provider := mustProvider(t)
	_, err := provider.Chat(context.Background(), &litellm.Request{
		Model:           "gpt-4.1",
		Messages:        []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{"unknown": true},
	})
	if err == nil || !litellm.IsValidationError(err) {
		t.Fatalf("expected structured validation error, got %v", err)
	}
}

func TestBuildRequestReasoningModelConstraints(t *testing.T) {
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingEnabled, Effort: "medium"},
	}, false)
	if err != nil {
		t.Fatalf("non-reasoning thinking enabled: %v", err)
	}
	if wire.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want empty for non-reasoning model", wire.ReasoningEffort)
	}
	thinkingJSON, err := json.Marshal(wire.Thinking)
	if err != nil {
		t.Fatalf("marshal thinking: %v", err)
	}
	if got := string(thinkingJSON); got != `{"effort":"medium","type":"enabled"}` {
		t.Fatalf("thinking body = %s", got)
	}

	wire, err = provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingDisabled},
	}, false)
	if err != nil {
		t.Fatalf("non-reasoning thinking disabled: %v", err)
	}
	thinkingJSON, err = json.Marshal(wire.Thinking)
	if err != nil {
		t.Fatalf("marshal thinking: %v", err)
	}
	if got := string(thinkingJSON); got != `{"type":"disabled"}` {
		t.Fatalf("thinking body = %s", got)
	}

	_, err = provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingEnabled, Effort: "minimal"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), `unsupported thinking effort "minimal"`) {
		t.Fatalf("expected minimal effort error, got %v", err)
	}

	temp := 1.0
	_, err = provider.buildRequest(&litellm.Request{
		Model:       "gpt-5.1",
		Temperature: &temp,
		Messages:    []litellm.Message{litellm.UserText("hi")},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "temperature is not supported") {
		t.Fatalf("expected temperature error, got %v", err)
	}

	_, err = provider.buildRequest(&litellm.Request{
		Model:    "gpt-5.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingEnabled, Effort: "minimal"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), `unsupported reasoning_effort "minimal"`) {
		t.Fatalf("expected minimal effort error, got %v", err)
	}

	wire, err = provider.buildRequest(&litellm.Request{
		Model:    "openai/gpt-5.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{
			Mode:   litellm.ThinkingEnabled,
			Effort: "medium",
		},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest reasoning model: %v", err)
	}
	if wire.ReasoningEffort != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium", wire.ReasoningEffort)
	}

	wire, err = provider.buildRequest(&litellm.Request{
		Model:    "gpt-5.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingDisabled},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest disabled reasoning model: %v", err)
	}
	if wire.ReasoningEffort != "none" {
		t.Fatalf("disabled reasoning_effort = %q, want none", wire.ReasoningEffort)
	}

	topP := 0.9
	wire, err = provider.buildRequest(&litellm.Request{
		Model:    "gpt-5.1",
		TopP:     &topP,
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{
			Mode:   litellm.ThinkingEnabled,
			Effort: "xhigh",
		},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest xhigh reasoning model: %v", err)
	}
	if wire.ReasoningEffort != "xhigh" || wire.TopP != nil {
		t.Fatalf("reasoning_effort/top_p = %q/%v", wire.ReasoningEffort, wire.TopP)
	}
}

func TestChatRetriesWhenRetryPolicyIsConfigured(t *testing.T) {
	var attempts int
	provider, err := New(Config{
		APIKey: "test-key",
		Retry:  &retry.Policy{MaxAttempts: 2, InitialDelay: 1},
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return jsonResponse(http.StatusTooManyRequests, `{"error":"retry"}`), nil
			}
			return jsonResponse(http.StatusOK, `{
				"model":"gpt-4.1",
				"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]
			}`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	resp, err := provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if attempts != 2 || resp.Text() != "ok" {
		t.Fatalf("attempts/text = %d/%q", attempts, resp.Text())
	}
}

func TestNewRejectsUnknownAPI(t *testing.T) {
	_, err := New(Config{APIKey: "test-key", API: "legacy"})
	if err == nil || !strings.Contains(err.Error(), "api must be chat or responses") {
		t.Fatalf("expected api validation error, got %v", err)
	}
}

func TestDefaultAPIRoutesChatToCompletionsEndpoint(t *testing.T) {
	var capturedPath string
	provider, err := New(Config{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			capturedPath = req.URL.Path
			return jsonResponse(http.StatusOK, `{
				"model":"gpt-4.1",
				"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]
			}`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if capturedPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", capturedPath)
	}
}

func TestChatSetsProviderHeaders(t *testing.T) {
	provider, err := New(Config{
		APIKey:    "test-key",
		BaseURL:   "https://example.test",
		UserAgent: "codex-cli/0.142.3",
		Headers: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "ainovel",
		},
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("User-Agent"); got != "codex-cli/0.142.3" {
				t.Fatalf("User-Agent = %q", got)
			}
			if got := req.Header.Get("Originator"); got != "Codex CLI" {
				t.Fatalf("Originator = %q", got)
			}
			if got := req.Header.Get("Session_id"); got != "ainovel" {
				t.Fatalf("Session_id = %q", got)
			}
			return jsonResponse(http.StatusOK, `{
				"model":"gpt-5.4",
				"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]
			}`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	resp, err := provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-5.4",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Text() != "ok" {
		t.Fatalf("Text = %q", resp.Text())
	}
}

func TestNewRejectsAmbiguousTransportConfig(t *testing.T) {
	_, err := New(Config{
		APIKey:     "test-key",
		HTTPClient: roundTripFunc(nil),
		Transport:  roundTripperFunc(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPClient and Transport are mutually exclusive") {
		t.Fatalf("expected HTTPClient/Transport error, got %v", err)
	}

	_, err = New(Config{
		APIKey:     "test-key",
		HTTPClient: roundTripFunc(nil),
		Retry:      retry.DefaultPolicy(),
	})
	if err == nil || !strings.Contains(err.Error(), "Retry cannot be used with a custom HTTPClient") {
		t.Fatalf("expected HTTPClient/Retry error, got %v", err)
	}
}

func TestChatConvertsResponseBlocks(t *testing.T) {
	var capturedAuth string
	provider, err := New(Config{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			capturedAuth = req.Header.Get("Authorization")
			return jsonResponse(200, `{
				"model":"gpt-4.1",
				"choices":[{
					"message":{
						"content":"hello",
						"tool_calls":[{
							"id":"call_1",
							"type":"function",
							"function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}
						}],
						"reasoning_content":"thought"
					},
					"finish_reason":"tool_calls"
				}],
				"usage":{
					"prompt_tokens":10,
					"completion_tokens":5,
					"total_tokens":15,
					"prompt_tokens_details":{"cached_tokens":3},
					"completion_tokens_details":{"reasoning_tokens":2}
				}
			}`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	resp, err := provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if capturedAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q", capturedAuth)
	}
	if resp.Text() != "hello" || resp.Reasoning() != "thought" {
		t.Fatalf("text/reasoning = %q/%q", resp.Text(), resp.Reasoning())
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if resp.FinishReason != litellm.FinishReasonToolCall {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.CacheReadTokens != 3 || resp.Usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestConvertResponseRejectsNil(t *testing.T) {
	_, err := convertResponse(nil, &litellm.Request{Model: "gpt-4.1"})
	if err == nil || !strings.Contains(err.Error(), "response cannot be nil") {
		t.Fatalf("expected nil response error, got %v", err)
	}
}

func TestConvertResponsePreservesRefusal(t *testing.T) {
	resp, err := convertResponse(&chatResponse{
		Model: "gpt-5.1",
		Choices: []choice{{
			Message:      responseMessage{Refusal: "I can't help."},
			FinishReason: "stop",
		}},
	}, nil)
	if err != nil {
		t.Fatalf("convertResponse: %v", err)
	}
	if resp.Refusal != "I can't help." || resp.Text() != resp.Refusal {
		t.Fatalf("refusal/text = %q/%q", resp.Refusal, resp.Text())
	}
	if resp.FinishReason != litellm.FinishReasonSafety || resp.FinishReasonRaw != "stop" {
		t.Fatalf("finish/raw = %q/%q", resp.FinishReason, resp.FinishReasonRaw)
	}
}

func TestChatRejectsUnsupportedResponseContent(t *testing.T) {
	provider, err := New(Config{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, `{
				"model":"gpt-4.1",
				"choices":[{
					"message":{"content":[{"type":"audio","url":"https://example.test/a.wav"}]},
					"finish_reason":"stop"
				}]
			}`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported content part type") {
		t.Fatalf("expected unsupported content error, got %v", err)
	}
}

func TestChatPreservesInvalidToolCallArguments(t *testing.T) {
	provider, err := New(Config{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, `{
				"model":"gpt-4.1",
				"choices":[{
					"message":{"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"lookup","arguments":"{\"q\":"}
					}]},
					"finish_reason":"tool_calls"
				}]
			}`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	resp, err := provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(calls))
	}
	if got := string(calls[0].Arguments); got != `{"q":` {
		t.Fatalf("arguments = %q, want raw malformed args", got)
	}
}

func TestChatDecodeErrorIsStructuredProviderError(t *testing.T) {
	provider, err := New(Config{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"choices":[`), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = provider.Chat(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err == nil || !strings.Contains(err.Error(), "decode response") || !litellm.IsProviderError(err) {
		t.Fatalf("expected structured decode provider error, got %v", err)
	}
}

func TestStreamEmitsTypedEvents(t *testing.T) {
	provider, err := New(Config{
		APIKey:  "test-key",
		BaseURL: "https://example.test",
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return streamResponse(testgolden.ReadFixtureString(t, "../../testdata/openai/chat_stream.sse")), nil
		}),
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.Stream(context.Background(), &litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	resp, err := litellm.Collect(stream)
	if err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	if resp.Text() != "hel" || resp.Reasoning() != "think " {
		t.Fatalf("text/reasoning = %q/%q", resp.Text(), resp.Reasoning())
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if resp.Usage.InputTokens != 4 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.FinishReason != litellm.FinishReasonToolCall {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
}

func TestStreamRejectsEOFBeforeDone(t *testing.T) {
	stream := newStream(streamResponse(`data: {"choices":[{"delta":{"content":"partial"}}]}`), &litellm.Request{Model: "gpt-4.1"})
	_, err := litellm.Collect(stream)
	if err == nil || !strings.Contains(err.Error(), "before [DONE]") || !litellm.IsProviderError(err) {
		t.Fatalf("expected truncated stream error, got %v", err)
	}
}

func TestStreamPreservesRefusal(t *testing.T) {
	stream := newStream(streamResponse(strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"refusal":"I can't help."}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")), &litellm.Request{Model: "gpt-5.1"})
	resp, err := litellm.Collect(stream)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Refusal != "I can't help." || resp.FinishReason != litellm.FinishReasonSafety {
		t.Fatalf("refusal/finish = %q/%q", resp.Refusal, resp.FinishReason)
	}
}

func TestBuildRequestDeepSeekV4FlashNoDefaultSampling(t *testing.T) {
	// Sampling parameters are config-driven via extra_body provider options,
	// not hardcoded: without them, no top_p/temperature is emitted.
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.TopP != nil {
		t.Fatalf("top_p = %v, want nil without extra_body", wire.TopP)
	}
	if wire.Temperature != nil {
		t.Fatalf("temperature = %v, want nil without extra_body", wire.Temperature)
	}

	// Explicit request-level top_p must still be preserved.
	topP := 0.7
	wire, err = provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		TopP:     &topP,
		Messages: []litellm.Message{litellm.UserText("hi")},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.TopP == nil || *wire.TopP != 0.7 {
		t.Fatalf("top_p = %v, want 0.7", wire.TopP)
	}
}

func TestBuildRequestExtraBodySamplingOptions(t *testing.T) {
	// extra_body temperature/top_p flow through ProviderOptions into the
	// wire request (config-driven sampling, no code hardcoding).
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{
			"temperature": 1.0,
			"top_p":       0.95,
		},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.Temperature == nil || *wire.Temperature != 1.0 {
		t.Fatalf("temperature = %v, want 1.0", wire.Temperature)
	}
	if wire.TopP == nil || *wire.TopP != 0.95 {
		t.Fatalf("top_p = %v, want 0.95", wire.TopP)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	jsonText := string(data)
	for _, want := range []string{`"temperature":1`, `"top_p":0.95`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("wire JSON missing %s:\n%s", want, jsonText)
		}
	}

	// Non-numeric values must be rejected.
	_, err = provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
		ProviderOptions: litellm.ProviderOptions{
			"top_p": "high",
		},
	}, false)
	if err == nil || !strings.Contains(err.Error(), `provider option "top_p" must be number`) {
		t.Fatalf("expected top_p type error, got %v", err)
	}
}

func TestBuildRequestDeepSeekV4FlashMapsThinkingToReasoningEffort(t *testing.T) {
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingEnabled, Effort: "high"},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", wire.ReasoningEffort)
	}
	if wire.Thinking != nil {
		t.Fatalf("thinking = %#v, want nil for deepseek-v4-flash", wire.Thinking)
	}

	wire, err = provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingDisabled},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.ReasoningEffort != "none" {
		t.Fatalf("reasoning_effort = %q, want none", wire.ReasoningEffort)
	}
	if wire.Thinking != nil {
		t.Fatalf("thinking = %#v, want nil for deepseek-v4-flash", wire.Thinking)
	}

	_, err = provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingEnabled, Effort: "minimal"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), `unsupported reasoning_effort "minimal"`) {
		t.Fatalf("expected minimal effort error, got %v", err)
	}
}

func TestBuildRequestNonDeepSeekV4FlashUnchanged(t *testing.T) {
	provider := mustProvider(t)
	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Thinking: &litellm.Thinking{Mode: litellm.ThinkingEnabled, Effort: "medium"},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	if wire.TopP != nil {
		t.Fatalf("top_p = %v, want nil for non-deepseek-v4-flash", wire.TopP)
	}
	if wire.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want empty for non-deepseek-v4-flash", wire.ReasoningEffort)
	}
	thinkingJSON, err := json.Marshal(wire.Thinking)
	if err != nil {
		t.Fatalf("marshal thinking: %v", err)
	}
	if got := string(thinkingJSON); got != `{"effort":"medium","type":"enabled"}` {
		t.Fatalf("thinking body = %s", got)
	}
}

func TestConvertToolsSkipsStrictForOllamaCloud(t *testing.T) {
	provider, err := New(Config{APIKey: "test-key", BaseURL: "https://ollama.com/v1"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	tool := mustTool(t, "lookup", "Lookup data.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
		"required": []string{"q"},
	})
	tool.Strict = litellm.StrictEnabled

	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "deepseek-v4-flash:0731",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Tools:    []litellm.Tool{tool},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	jsonText := string(data)
	if strings.Contains(jsonText, `"strict"`) {
		t.Fatalf("wire JSON must not contain strict for ollama.com:\n%s", jsonText)
	}
	if strings.Contains(jsonText, `"additionalProperties"`) {
		t.Fatalf("wire JSON must keep schema verbatim for ollama.com:\n%s", jsonText)
	}
}

func TestConvertToolsKeepsStrictForNonOllamaBaseURL(t *testing.T) {
	provider, err := New(Config{APIKey: "test-key", BaseURL: "https://example.test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	tool := mustTool(t, "lookup", "Lookup data.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
		"required": []string{"q"},
	})
	tool.Strict = litellm.StrictEnabled

	wire, err := provider.buildRequest(&litellm.Request{
		Model:    "gpt-4.1",
		Messages: []litellm.Message{litellm.UserText("hi")},
		Tools:    []litellm.Tool{tool},
	}, false)
	if err != nil {
		t.Fatalf("buildRequest returned error: %v", err)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	jsonText := string(data)
	for _, want := range []string{`"strict":true`, `"additionalProperties":false`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("wire JSON missing %s:\n%s", want, jsonText)
		}
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func streamResponse(body string) *http.Response {
	resp := jsonResponse(200, body)
	resp.Header.Set("Content-Type", "text/event-stream")
	return resp
}

func mustProvider(t *testing.T) *Provider {
	t.Helper()
	provider, err := New(Config{APIKey: "test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return provider
}

func mustTool(t *testing.T, name, description string, schema any) litellm.Tool {
	t.Helper()
	tool, err := litellm.NewTool(name, description, schema)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	return tool
}
