package model

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const defaultDashScopeBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

// DashScopeChatModel calls Alibaba Cloud DashScope (Qwen) models via the OpenAI-compatible API.
type DashScopeChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// DashScopeConfig configures DashScopeChatModel.
type DashScopeConfig struct {
	APIKey       string
	SecretAPIKey SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL      string    // Optional, defaults to DashScope compatible endpoint
	Model        string

	HTTPClient    *http.Client
	ClientOptions *ClientOptions
}

// NewDashScopeChatModel creates a ChatModel using the DashScope backend.
func NewDashScopeChatModel(cfg DashScopeConfig) (*DashScopeChatModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("dashscope: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("dashscope: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultDashScopeBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &DashScopeChatModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements ChatModel using the OpenAI-compatible chat/completions path.
func (m *DashScopeChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("dashscope: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	messages, err := convertMessagesWithFormatter(msgs, formatter.NewDashScopeFormatter())
	if err != nil {
		return nil, fmt.Errorf("dashscope: %w", err)
	}

	reqBody := openAIChatRequest{
		Model:    m.model,
		Messages: messages,
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
	if callOpts.ThinkingEnable != nil {
		reqBody.EnableThinking = callOpts.ThinkingEnable
	}
	if callOpts.Voice != nil {
		reqBody.Audio = &openAIAudioConfig{Voice: *callOpts.Voice, Format: "pcm16"}
		reqBody.Modalities = []string{"text", "audio"}
	}

	var parsed openAIChatResponse
	if err := httpx.DoJSONRequest(
		ctx,
		m.httpClient,
		http.MethodPost,
		m.baseURL+"/chat/completions",
		reqBody,
		&parsed,
		mergeHeaders(map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + m.apiKey,
		}, m.defaultHeaders),
	); err != nil {
		return nil, fmt.Errorf("dashscope: %w", err)
	}

	return parseOpenAIResponse(&parsed, msgs)
}

// ChatStream implements streaming chat via SSE (OpenAI-compatible format).
func (m *DashScopeChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("dashscope: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	messages, err := convertMessagesWithFormatter(msgs, formatter.NewDashScopeFormatter())
	if err != nil {
		return nil, fmt.Errorf("dashscope: %w", err)
	}

	reqBody := openAIChatRequest{
		Model:         m.model,
		Messages:      messages,
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
	if callOpts.ThinkingEnable != nil {
		reqBody.EnableThinking = callOpts.ThinkingEnable
	}
	if callOpts.Voice != nil {
		reqBody.Audio = &openAIAudioConfig{Voice: *callOpts.Voice, Format: "pcm16"}
		reqBody.Modalities = []string{"text", "audio"}
	}

	sseCh, err := httpx.DoSSERequest(
		ctx,
		m.httpClient,
		"POST",
		m.baseURL+"/chat/completions",
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + m.apiKey,
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("dashscope: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processOpenAIStream(ctx, sseCh, outCh)
	return outCh, nil
}

// DisableThinkingOptions implements ThinkingDisabler (upstream #2140).
func (m *DashScopeChatModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}

// CountTokens estimates token count by byte length / 4.
func (m *DashScopeChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}
