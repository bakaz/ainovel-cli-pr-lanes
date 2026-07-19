package migratev3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	providerResponseLimit  = 8 << 20
	stage3bMaxOutputTokens = 4096
	providerFillSentinel   = "__FILL_NONEMPTY_CHINESE__"
)

const migrationSystemPrompt = `你是 SceneBeat v3 迁移助手。用户消息含 request、output_slots 与 output_template。
只返回 output_template 形状的 JSON：{"values":["中文值", ...]}。values 必须与 output_slots 等长且顺序一致，每个值对应同位置 slot 的 id 与 field。
只生成具体、非空的中文字符串值；禁止输出 id、字段键、slot 对象、额外数组项、重复 JSON 键或第二个 JSON。
不得返回、改写或省略 preserved_fields；legacy_action 必须作为既有 action 原文保留，不得改写。
根据该章标题、source、legacy_action 与 preserved_fields 补齐具体、可执行的场景节拍。erotic_charge 无性张力时明确写“无”。
不要 Markdown、代码围栏、解释、前后缀或第二个 JSON。`

const migrationRetryPrompt = migrationSystemPrompt + `
上一次回复未通过严格结构校验。本次必须直接复制 output_template 的 JSON 骨架，只替换 values 内的占位字符串；输出前数清 values 数量并与 output_slots 完全一致。`

type providerConfigFile struct {
	Provider  string                         `json:"provider"`
	Model     string                         `json:"model"`
	Providers map[string]providerConfigEntry `json:"providers"`
	Style     json.RawMessage                `json:"style,omitempty"`
}

type providerConfigEntry struct {
	Type              string         `json:"type,omitempty"`
	API               string         `json:"api,omitempty"`
	APIKey            string         `json:"api_key,omitempty"`
	BaseURL           string         `json:"base_url,omitempty"`
	Models            []string       `json:"models,omitempty"`
	ExtraBody         map[string]any `json:"extra_body,omitempty"`
	Extra             map[string]any `json:"extra,omitempty"`
	StreamIdleTimeout string         `json:"stream_idle_timeout,omitempty"`
}

type OpenAIChatGenerator struct {
	desc     GeneratorDescriptor
	apiKey   string
	endpoint string
	client   *http.Client
}

type openAIChatBody struct {
	Model          string              `json:"model"`
	Messages       []openAIChatMessage `json:"messages"`
	MaxTokens      int                 `json:"max_tokens"`
	Temperature    float64             `json:"temperature"`
	ResponseFormat openAIResponseFmt   `json:"response_format"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFmt struct {
	Type string `json:"type"`
}

type openAIUserPayload struct {
	Request        ChapterRequest        `json:"request"`
	OutputSlots    []providerOutputSlot  `json:"output_slots"`
	OutputTemplate providerValueEnvelope `json:"output_template"`
}

type providerOutputSlot struct {
	ID    string `json:"id"`
	Field string `json:"field"`
}

type providerValueEnvelope struct {
	Values []string `json:"values"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	} `json:"usage"`
}

func NewOpenAIChatGenerator(configPath string) (*OpenAIChatGenerator, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("--provider-config is required for Stage 3b")
	}
	configAbs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	if err := ensureNoReparseComponents(configAbs); err != nil {
		return nil, fmt.Errorf("explicit provider config must not cross a symlink/reparse path")
	}
	data, err := readPlainFile(configAbs)
	if err != nil {
		return nil, fmt.Errorf("read explicit provider config: %w", err)
	}
	if err := validateJSONLexically(data); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	var cfg providerConfigFile
	if err := strictJSON(data, &cfg); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	providerName := strings.TrimSpace(cfg.Provider)
	model := strings.TrimSpace(cfg.Model)
	if providerName == "" || model == "" || sanitizeLabel(providerName) != providerName || sanitizeLabel(model) != model {
		return nil, fmt.Errorf("provider config has unsafe/missing provider or model")
	}
	selected, ok := cfg.Providers[providerName]
	if !ok {
		return nil, fmt.Errorf("selected provider is absent from explicit config")
	}
	if selected.Type != "openai" || (selected.API != "" && selected.API != "chat") {
		return nil, fmt.Errorf("Stage 3b adapter requires type=openai and api=chat")
	}
	if selected.APIKey == "" || selected.BaseURL == "" {
		return nil, fmt.Errorf("selected provider requires api_key and base_url")
	}
	if len(selected.Extra) != 0 || len(selected.ExtraBody) != 0 {
		return nil, fmt.Errorf("Stage 3b rejects provider extra/extra_body to prevent secret or request drift")
	}
	endpoint, err := chatCompletionsEndpoint(selected.BaseURL)
	if err != nil {
		return nil, err
	}
	pricing, err := reviewedPricing(model)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("provider redirects are forbidden")
		},
	}
	return &OpenAIChatGenerator{
		desc:   GeneratorDescriptor{Provider: providerName, Model: model, Pricing: pricing, RealProvider: true},
		apiKey: selected.APIKey, endpoint: endpoint, client: client,
	}, nil
}

func reviewedPricing(model string) (Pricing, error) {
	switch model {
	case "mimo-v2.5":
		// Conservative full-input pricing; cached-input discounts are ignored.
		return Pricing{Known: true, InputUSDPerMillion: 0.14, OutputUSDPerMillion: 0.28, MaxOutputTokens: stage3bMaxOutputTokens}, nil
	default:
		return Pricing{}, fmt.Errorf("model %q has no reviewed Stage 3b pricing", model)
	}
}

func chatCompletionsEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse provider base_url: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return "", fmt.Errorf("provider base_url must not contain userinfo, query, or fragment")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if u.Scheme != "http" || !(strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())) {
			return "", fmt.Errorf("provider base_url must use https (http is test-only loopback)")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/chat/completions") {
		u.Path = path.Join(u.Path, "chat/completions")
		if !strings.HasPrefix(u.Path, "/") {
			u.Path = "/" + u.Path
		}
	}
	return u.String(), nil
}

func (g *OpenAIChatGenerator) Descriptor() GeneratorDescriptor { return g.desc }

func (g *OpenAIChatGenerator) Prepare(req ChapterRequest) (PreparedRequest, error) {
	return g.prepare(req, migrationSystemPrompt)
}

func (g *OpenAIChatGenerator) PrepareRetry(req ChapterRequest, attempt int, feedback string) (PreparedRequest, error) {
	if attempt <= 1 {
		return PreparedRequest{}, fmt.Errorf("retry attempt must be greater than one")
	}
	if feedback == "" || len([]byte(feedback)) > maxRetryFeedbackBytes || sanitizeRetryFeedback(feedback) != feedback {
		return PreparedRequest{}, fmt.Errorf("retry feedback is empty or unsafe")
	}
	prepared, err := g.prepare(req, migrationRetryPrompt+"\n上次校验错误："+feedback)
	prepared.retryFeedback = feedback
	return prepared, err
}

func (g *OpenAIChatGenerator) prepare(req ChapterRequest, systemPrompt string) (PreparedRequest, error) {
	slots := buildProviderOutputSlots(req)
	template := providerValueEnvelope{Values: make([]string, len(slots))}
	for i := range template.Values {
		template.Values[i] = providerFillSentinel
	}
	payload := openAIUserPayload{Request: req, OutputSlots: slots, OutputTemplate: template}
	requestJSON, err := json.Marshal(payload)
	if err != nil {
		return PreparedRequest{}, err
	}
	body := openAIChatBody{
		Model: g.desc.Model,
		Messages: []openAIChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: string(requestJSON)},
		},
		MaxTokens: g.desc.Pricing.MaxOutputTokens, Temperature: 0,
		ResponseFormat: openAIResponseFmt{Type: "json_object"},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return PreparedRequest{}, err
	}
	return PreparedRequest{OutboundBody: string(data)}, nil
}

func buildProviderOutputSlots(req ChapterRequest) []providerOutputSlot {
	var slots []providerOutputSlot
	for _, scene := range req.Scenes {
		for _, field := range scene.MissingFields {
			slots = append(slots, providerOutputSlot{ID: scene.ID, Field: field})
		}
	}
	return slots
}

func (g *OpenAIChatGenerator) Generate(ctx context.Context, req ChapterRequest, prepared PreparedRequest, attempt int) (GenerateResult, error) {
	want, err := prepareGeneratorAttempt(g, req, attempt, prepared.retryFeedback)
	if err != nil {
		return GenerateResult{}, err
	}
	if prepared.OutboundBody != want.OutboundBody {
		return GenerateResult{}, fmt.Errorf("prepared Provider request drift")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, strings.NewReader(prepared.OutboundBody))
	if err != nil {
		return GenerateResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "ainovel-migrate/scene-beat-v3")
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("Provider request failed")
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, providerResponseLimit+1)
	responseData, err := io.ReadAll(limited)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("read Provider response: %w", err)
	}
	if len(responseData) > providerResponseLimit {
		return GenerateResult{}, fmt.Errorf("Provider response exceeds size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GenerateResult{}, fmt.Errorf("Provider returned HTTP status %d", resp.StatusCode)
	}
	if err := validateJSONLexically(responseData); err != nil {
		return GenerateResult{}, fmt.Errorf("Provider envelope is invalid JSON: %w", err)
	}
	var decoded openAIChatResponse
	if err := json.Unmarshal(responseData, &decoded); err != nil {
		return GenerateResult{}, fmt.Errorf("decode Provider envelope: %w", err)
	}
	if decoded.Usage.PromptTokens == nil || decoded.Usage.CompletionTokens == nil {
		return GenerateResult{}, fmt.Errorf("Provider response has unknown usage")
	}
	usage := Usage{Known: true, InputTokens: *decoded.Usage.PromptTokens, OutputTokens: *decoded.Usage.CompletionTokens}
	if len(decoded.Choices) != 1 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return GenerateResult{Usage: usage}, fmt.Errorf("Provider response has no single non-empty content choice")
	}
	content := []byte(decoded.Choices[0].Message.Content)
	result := GenerateResult{ProviderResponse: bytes.Clone(content), Usage: usage, FinishReason: decoded.Choices[0].FinishReason}
	normalized, err := normalizeProviderValueResponse(req, content)
	if err != nil {
		return result, fmt.Errorf("Provider value response: %w", err)
	}
	result.RawResponse = normalized
	return result, nil
}

func normalizeProviderValueResponse(req ChapterRequest, raw []byte) (json.RawMessage, error) {
	if err := validateJSONLexically(raw); err != nil {
		return nil, err
	}
	var values providerValueEnvelope
	if err := strictJSON(raw, &values); err != nil {
		return nil, err
	}
	slots := buildProviderOutputSlots(req)
	if len(values.Values) != len(slots) {
		return nil, fmt.Errorf("values count got %d want %d", len(values.Values), len(slots))
	}
	byID := make(map[string]map[string]string, len(req.Scenes))
	for i, slot := range slots {
		value := values.Values[i]
		if strings.TrimSpace(value) == "" || strings.Contains(value, providerFillSentinel) {
			return nil, fmt.Errorf("value %d for %s/%s is empty or retained the sentinel", i, slot.ID, slot.Field)
		}
		if byID[slot.ID] == nil {
			byID[slot.ID] = map[string]string{}
		}
		byID[slot.ID][slot.Field] = value
	}
	items := make([]ProposalItem, 0, len(req.Scenes))
	for _, scene := range req.Scenes {
		items = append(items, ProposalItem{ID: scene.ID, Fields: byID[scene.ID]})
	}
	data, err := json.Marshal(proposalEnvelope{Items: items})
	return json.RawMessage(data), err
}
