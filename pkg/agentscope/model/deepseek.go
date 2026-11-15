package model

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const defaultDeepSeekBaseURL = "https://api.deepseek.com"

// DeepSeekChatModel wraps the DeepSeek Chat API (OpenAI-compatible with reasoning_content).
type DeepSeekChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// DeepSeekConfig configures DeepSeekChatModel.
type DeepSeekConfig struct {
	APIKey        string
	SecretAPIKey  SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL       string
	Model         string
	HTTPClient    *http.Client
	ClientOptions *ClientOptions
}

// NewDeepSeekChatModel creates a ChatModel backed by DeepSeek.
func NewDeepSeekChatModel(cfg DeepSeekConfig) (*DeepSeekChatModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("deepseek: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("deepseek: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultDeepSeekBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &DeepSeekChatModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements the ChatModel interface.
func (m *DeepSeekChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("deepseek: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody := openAIChatRequest{
		Model:    m.model,
		Messages: convertMessagesToOpenAI(msgs),
	}
	if callOpts.Temperature != nil {
		t := float32(*callOpts.Temperature)
		reqBody.Temperature = &t
	}
	if callOpts.MaxTokens != nil {
		reqBody.MaxTokens = callOpts.MaxTokens
	}
	if callOpts.TopP != nil {
		p := float32(*callOpts.TopP)
		reqBody.TopP = &p
	}
	if callOpts.Seed != nil {
		reqBody.Seed = callOpts.Seed
	}
	if len(callOpts.Tools) > 0 {
		reqBody.Tools = callOpts.Tools
	}
	if callOpts.ToolChoice != nil {
		reqBody.ToolChoice = formatToolChoice(callOpts.ToolChoice)
	}
	if callOpts.ThinkingEnable != nil && !*callOpts.ThinkingEnable {
		// Upstream #2140: DeepSeek/Moonshot disable thinking via
		// {"thinking":{"type":"disabled"}}, not enable_thinking.
		reqBody.Thinking = &openAIThinking{Type: "disabled"}
	}

	var parsed openAIChatResponse
	if err := httpx.DoJSONRequest(
		ctx,
		m.httpClient,
		http.MethodPost,
		m.baseURL+"/v1/chat/completions",
		reqBody,
		&parsed,
		mergeHeaders(map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + m.apiKey,
		}, m.defaultHeaders),
	); err != nil {
		return nil, fmt.Errorf("deepseek: %w", err)
	}

	return parseOpenAIResponse(&parsed, msgs)
}

// ChatStream implements streaming chat via SSE.
func (m *DeepSeekChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("deepseek: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody := openAIChatRequest{
		Model:         m.model,
		Messages:      convertMessagesToOpenAI(msgs),
		Stream:        true,
		StreamOptions: &openAIStreamOpts{IncludeUsage: true},
	}
	if callOpts.Temperature != nil {
		t := float32(*callOpts.Temperature)
		reqBody.Temperature = &t
	}
	if callOpts.MaxTokens != nil {
		reqBody.MaxTokens = callOpts.MaxTokens
	}
	if callOpts.TopP != nil {
		p := float32(*callOpts.TopP)
		reqBody.TopP = &p
	}
	if callOpts.Seed != nil {
		reqBody.Seed = callOpts.Seed
	}
	if len(callOpts.Tools) > 0 {
		reqBody.Tools = callOpts.Tools
	}
	if callOpts.ToolChoice != nil {
		reqBody.ToolChoice = formatToolChoice(callOpts.ToolChoice)
	}
	if callOpts.ThinkingEnable != nil && !*callOpts.ThinkingEnable {
		// Upstream #2140: DeepSeek/Moonshot disable thinking via
		// {"thinking":{"type":"disabled"}}, not enable_thinking.
		reqBody.Thinking = &openAIThinking{Type: "disabled"}
	}

	sseCh, err := httpx.DoSSERequest(
		ctx,
		m.httpClient,
		"POST",
		m.baseURL+"/v1/chat/completions",
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + m.apiKey,
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("deepseek: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processOpenAIStream(ctx, sseCh, outCh)
	return outCh, nil
}

// DisableThinkingOptions implements ThinkingDisabler (upstream #2140).
func (m *DeepSeekChatModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}

// CountTokens estimates token count.
func (m *DeepSeekChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}
