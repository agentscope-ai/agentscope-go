package model

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const defaultVolcengineBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// VolcengineConfig configures the Ark Chat Completions adapter.
type VolcengineConfig struct {
	APIKey        string
	SecretAPIKey  SecretStr // Preferred over APIKey.
	Model         string
	BaseURL       string // Complete API prefix, including /api/v3; not a full chat URL.
	HTTPClient    *http.Client
	ClientOptions *ClientOptions
}

// VolcengineChatModel implements Ark text chat and tool calling using its
// OpenAI-compatible API. Model-specific thinking constraints are enforced by Ark.
type VolcengineChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// NewVolcengineChatModel creates an Ark adapter for a caller-supplied model ID.
func NewVolcengineChatModel(cfg *VolcengineConfig) (*VolcengineChatModel, error) {
	if cfg == nil {
		return nil, fmt.Errorf("volcengine: config is required")
	}
	key := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if key == "" {
		return nil, fmt.Errorf("volcengine: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("volcengine: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultVolcengineBaseURL
	}
	// Own the map so later caller configuration changes cannot race with calls.
	var headers map[string]string
	if cfg.ClientOptions != nil {
		headers = make(map[string]string, len(cfg.ClientOptions.DefaultHeaders))
		for k, v := range cfg.ClientOptions.DefaultHeaders {
			headers[http.CanonicalHeaderKey(k)] = v
		}
	}
	return &VolcengineChatModel{
		apiKey: key, model: cfg.Model, baseURL: strings.TrimRight(base, "/"),
		defaultHeaders: headers, httpClient: defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

func (m *VolcengineChatModel) request(msgs []*message.Msg, stream bool, opts []CallOption) (*openAIChatRequest, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("volcengine: msgs must not be empty")
	}
	var o CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.ThinkingBudget != nil && *o.ThinkingBudget > 0 {
		return nil, fmt.Errorf("volcengine: thinking token budgets are unsupported; use WithThinking(enable, 0)")
	}
	messages, err := convertMessagesToOpenAI(msgs)
	if err != nil {
		return nil, fmt.Errorf("volcengine: %w", err)
	}

	r := &openAIChatRequest{
		Model: m.model, Messages: messages, Stream: stream,
		MaxTokens: o.MaxTokens, Tools: o.Tools, ReasoningEffort: o.ReasoningEffort,
	}
	if o.Temperature != nil {
		v := float32(*o.Temperature)
		r.Temperature = &v
	}
	if o.TopP != nil {
		v := float32(*o.TopP)
		r.TopP = &v
	}
	if o.ToolChoice != nil {
		r.ToolChoice = formatToolChoice(o.ToolChoice)
	}
	if o.ThinkingEnable != nil {
		kind := "disabled"
		if *o.ThinkingEnable {
			kind = "enabled"
		}
		r.Thinking = &openAIThinking{Type: kind}
	}
	if stream {
		r.StreamOptions = &openAIStreamOpts{IncludeUsage: true}
	}
	return r, nil
}

func (m *VolcengineChatModel) headers() map[string]string {
	return mergeHeaders(map[string]string{
		"Content-Type": "application/json", "Authorization": "Bearer " + m.apiKey,
	}, m.defaultHeaders)
}

// Chat generates a buffered response. Positive thinking-token budgets are
// rejected; use zero for the toggle and WithReasoningEffort for reasoning depth.
func (m *VolcengineChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	r, err := m.request(msgs, false, opts)
	if err != nil {
		return nil, err
	}
	var parsed openAIChatResponse
	if err := httpx.DoJSONRequest(ctx, m.httpClient, http.MethodPost, m.baseURL+"/chat/completions", r, &parsed, m.headers()); err != nil {
		return nil, fmt.Errorf("volcengine: %w", err)
	}
	response, err := parseOpenAIResponse(&parsed, msgs)
	if err != nil {
		return nil, fmt.Errorf("volcengine: %w", err)
	}
	response.StopReason = normalizeStopReason(parsed.Choices[0].FinishReason)
	return response, nil
}

// ChatStream emits deltas followed by an assembled IsLast response. Consumers
// must check each response's Error and ctx.Err() when the channel closes.
func (m *VolcengineChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	r, err := m.request(msgs, true, opts)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	sse, err := httpx.DoSSERequest(streamCtx, m.httpClient, http.MethodPost, m.baseURL+"/chat/completions", r, m.headers())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("volcengine: %w", err)
	}
	out := make(chan ChatResponse, 16)
	go func() {
		// Release the HTTP reader even if processing ends before the server
		// closes its response (for example on [DONE] or malformed JSON).
		defer cancel()
		processOpenAIStream(streamCtx, sse, out)
	}()
	return out, nil
}

// CountTokens estimates input tokens; it does not report provider usage.
func (m *VolcengineChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// DisableThinkingOptions supplies the structured-output fallback toggle.
func (m *VolcengineChatModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}
