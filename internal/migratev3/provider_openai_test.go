package migratev3

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestOpenAIChatGenerator_PreparedBodyUsageAndPrivacy(t *testing.T) {
	secret := "sk-stage3b-local-test-secret"
	var calls atomic.Int32
	server := newMigrationProviderServer(t, secret, &calls)
	defer server.Close()
	config := writeProviderConfig(t, server.URL+"/v1", secret, "mimo-v2.5", false)
	gen, err := NewOpenAIChatGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	req := ChapterRequest{Chapter: 1, Title: "测试", Scenes: []SceneRequest{{
		ID: "ch-01/s-01", Source: sceneForProviderTest(), MissingFields: []string{"body_reaction", "emotion_reaction", "erotic_charge"},
		PreservedFields: map[string]string{"goal": "目标", "action": "动作", "conflict": "冲突", "outcome": "结果"},
	}}}
	prepared, err := gen.Prepare(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prepared.OutboundBody, secret) || len(prepared.OutboundBody) == 0 {
		t.Fatal("prepared body is empty or contains credential")
	}
	result, err := gen.Generate(context.Background(), req, prepared, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Usage.Known || result.Usage.InputTokens <= 0 || result.Usage.OutputTokens <= 0 || result.FinishReason != "stop" || !json.Valid(result.RawResponse) {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := gen.Generate(context.Background(), req, prepared, 2); err == nil {
		t.Fatal("attempt 2 accepted the attempt 1 outbound body")
	}
	retryPrepared, err := gen.PrepareRetry(req, 2, "duplicate proposal id ch-01/s-01")
	if err != nil {
		t.Fatal(err)
	}
	if retryPrepared.OutboundBody == prepared.OutboundBody || !strings.Contains(retryPrepared.OutboundBody, "上一次回复未通过严格结构校验") || strings.Contains(retryPrepared.OutboundBody, secret) {
		t.Fatal("retry body is not strengthened or is unsafe")
	}
	if _, err := gen.Generate(context.Background(), req, retryPrepared, 2); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}
}

func TestOpenAIChatGenerator_FullDraftVerifyNoCredentialLeak(t *testing.T) {
	secret := "sk-stage3b-full-draft-secret"
	var calls atomic.Int32
	server := newMigrationProviderServer(t, secret, &calls)
	defer server.Close()
	config := writeProviderConfig(t, server.URL+"/v1", secret, "mimo-v2.5", false)
	gen, err := NewOpenAIChatGenerator(config)
	if err != nil {
		t.Fatal(err)
	}
	book, expected := makeFixtureBook(t)
	before := mustSnapshot(t, book)
	preflight, err := Preflight(DraftOptions{
		BookDir: book, Generator: gen, ProviderConfigPath: config, AllowProviderCalls: true,
		ExpectedEnrolled: &expected, MaxCostUSD: MaxApprovedCost,
	})
	if err != nil || preflight.LogicalBatches != 42 || preflight.WorstCaseCostUSD <= 0 || calls.Load() != 0 {
		t.Fatalf("preflight=%+v err=%v calls=%d", preflight, err, calls.Load())
	}
	assertSnapshot(t, book, before)
	result, err := Draft(context.Background(), DraftOptions{
		BookDir: book, Generator: gen, ProviderConfigPath: config, AllowProviderCalls: true,
		ExpectedEnrolled: &expected, MaxCostUSD: MaxApprovedCost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 42 || result.CostUSD <= 0 || result.CostUSD > MaxApprovedCost {
		t.Fatalf("calls=%d result=%+v", calls.Load(), result)
	}
	if err := noSecretLeak(result.RunDir, []string{secret, "Authorization", "Bearer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(VerifyOptions{BookDir: book, RunDir: result.RunDir, ExpectedEnrolled: &expected}); err != nil {
		t.Fatal(err)
	}
	var manifest ArtifactManifest
	if err := readStrictFile(filepath.Join(result.RunDir, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	exchangePath := filepath.Join(result.RunDir, "provider", "exchanges", "ch-01-attempt-01.json")
	var exchange providerExchange
	if err := readStrictFile(exchangePath, &exchange); err != nil {
		t.Fatal(err)
	}
	var wire providerValueEnvelope
	if err := strictJSON(exchange.ProviderResponse, &wire); err != nil {
		t.Fatal(err)
	}
	wire.Values[0] += "篡改"
	exchange.ProviderResponse, _ = json.Marshal(wire)
	exchangeData, _ := marshalJSON(exchange)
	mustWrite(t, exchangePath, exchangeData)
	resignManifest(t, result.RunDir, &manifest)
	if _, err := Verify(VerifyOptions{BookDir: book, RunDir: result.RunDir, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("verify accepted a re-signed Provider wire-response forgery")
	}
	assertSnapshot(t, book, before)
}

func TestNormalizeProviderValueResponseFailsClosed(t *testing.T) {
	req := ChapterRequest{Chapter: 1, Scenes: []SceneRequest{{
		ID: "ch-01/s-01", MissingFields: []string{"goal", "conflict"},
	}}}
	valid, err := normalizeProviderValueResponse(req, []byte(`{"values":["目标","冲突"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var proposal proposalEnvelope
	if err := strictJSON(valid, &proposal); err != nil || len(proposal.Items) != 1 || proposal.Items[0].Fields["goal"] != "目标" || proposal.Items[0].Fields["conflict"] != "冲突" {
		t.Fatalf("bad deterministic normalization: %s err=%v", valid, err)
	}
	invalid := []string{
		`{"values":["目标","冲突"],"values":["目标","冲突"]}`,
		`{"values":["目标"]}`,
		`{"values":["目标","__FILL_NONEMPTY_CHINESE__"]}`,
		`{"values":["目标","冲突"],"extra":true}`,
		`{"values":["目标","冲突"]} {}`,
	}
	for _, raw := range invalid {
		if _, err := normalizeProviderValueResponse(req, []byte(raw)); err == nil {
			t.Fatalf("accepted invalid Provider value response: %s", raw)
		}
	}
}

func TestOpenAIChatGenerator_ConfigFailsClosed(t *testing.T) {
	tests := map[string]struct {
		baseURL string
		model   string
		extra   bool
	}{
		"query-url":       {baseURL: "https://example.test/v1?token=secret", model: "mimo-v2.5"},
		"userinfo-url":    {baseURL: "https://user:pass@example.test/v1", model: "mimo-v2.5"},
		"unknown-pricing": {baseURL: "https://example.test/v1", model: "unknown-model"},
		"extra-secrets":   {baseURL: "https://example.test/v1", model: "mimo-v2.5", extra: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			config := writeProviderConfig(t, tc.baseURL, "sk-secret", tc.model, tc.extra)
			if _, err := NewOpenAIChatGenerator(config); err == nil {
				t.Fatal("unsafe/unknown Provider config was accepted")
			}
		})
	}
}

func TestOpenAIChatGenerator_RejectsConfigThroughReparseDirectory(t *testing.T) {
	target := t.TempDir()
	config := filepath.Join(target, "config.json")
	data := []byte(`{"provider":"p","model":"mimo-v2.5","providers":{"p":{"type":"openai","api_key":"sk","base_url":"https://example.test/v1"}}}`)
	mustWrite(t, config, data)
	link := filepath.Join(t.TempDir(), "config-link")
	if err := createDirectoryLink(link, target); err != nil {
		t.Skipf("directory reparse creation unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if _, err := NewOpenAIChatGenerator(filepath.Join(link, "config.json")); err == nil {
		t.Fatal("Provider config through reparse directory was accepted")
	}
}

func newMigrationProviderServer(t *testing.T, secret string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("unexpected request path/auth")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var body openAIChatBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if body.Model != "mimo-v2.5" || body.MaxTokens != stage3bMaxOutputTokens || len(body.Messages) != 2 {
			t.Errorf("unexpected outbound body: %+v", body)
		}
		var payload openAIUserPayload
		if err := json.Unmarshal([]byte(body.Messages[1].Content), &payload); err != nil {
			t.Errorf("decode chapter request: %v", err)
			http.Error(w, "bad chapter", http.StatusBadRequest)
			return
		}
		req := payload.Request
		slots := buildProviderOutputSlots(req)
		if len(payload.OutputSlots) != len(slots) || len(payload.OutputTemplate.Values) != len(slots) {
			t.Errorf("output template/slot count drift")
			http.Error(w, "bad template", http.StatusBadRequest)
			return
		}
		for i, slot := range payload.OutputSlots {
			if slot != slots[i] {
				t.Errorf("output template structure drift")
				http.Error(w, "bad template", http.StatusBadRequest)
				return
			}
			if payload.OutputTemplate.Values[i] != providerFillSentinel {
				t.Errorf("output template sentinel drift")
				http.Error(w, "bad template", http.StatusBadRequest)
				return
			}
		}
		values := providerValueEnvelope{Values: make([]string, len(slots))}
		for i, slot := range slots {
			values.Values[i] = fmt.Sprintf("本地测试值-%s-%s", slot.ID, slot.Field)
		}
		wire, _ := json.Marshal(values)
		response := map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": string(wire)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
}

func writeProviderConfig(t *testing.T, baseURL, secret, model string, extra bool) string {
	t.Helper()
	entry := map[string]any{"type": "openai", "api_key": secret, "base_url": baseURL}
	if extra {
		entry["extra"] = map[string]any{"headers": map[string]any{"X-Secret": secret}}
	}
	config := map[string]any{
		"provider": "stage3b-test", "model": model,
		"providers": map[string]any{"stage3b-test": entry}, "style": "test",
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("provider-%s.json", strings.ReplaceAll(model, "/", "_")))
	mustWrite(t, path, data)
	return path
}

func sceneForProviderTest() domain.SceneBeat {
	return domain.SceneBeat{Goal: "目标", Action: "动作", Conflict: "冲突", Outcome: "结果"}
}
