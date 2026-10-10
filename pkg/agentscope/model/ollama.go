package model

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const defaultOllamaBaseURL = "http://localhost:11434"

// OllamaChatModel wraps a local Ollama instance via the OpenAI-compatible API.
// No API key is required.
type OllamaChatModel struct {
	baseURL        string
	model          string
	contextSize    int
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// OllamaConfig configures OllamaChatModel.
type OllamaConfig struct {
	BaseURL       string
	Model         string
	HTTPClient    *http.Client
	ClientOptions *ClientOptions
	// ContextSize declares the context window, in tokens, that the Ollama
	// server uses for Model (its num_ctx). It is reported through
	// ContextSizer so context compression measures against that window.
	// It is not sent to the server: set num_ctx in a Modelfile or the
	// server configuration, because the OpenAI-compatible endpoint used by
	// this adapter cannot change it. Zero means unknown.
	ContextSize int
}

// NewOllamaChatModel creates a ChatModel backed by a local Ollama instance.
func NewOllamaChatModel(cfg OllamaConfig) (*OllamaChatModel, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("ollama: Model is required")
	}
	if cfg.ContextSize < 0 {
		return nil, fmt.Errorf("ollama: ContextSize must not be negative")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultOllamaBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &OllamaChatModel{
		baseURL:        base,
		model:          cfg.Model,
		contextSize:    cfg.ContextSize,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements the ChatModel interface.
func (m *OllamaChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("ollama: msgs must not be empty")
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
	if len(callOpts.Tools) > 0 {
		reqBody.Tools = callOpts.Tools
	}
	if callOpts.ToolChoice != nil {
		reqBody.ToolChoice = formatToolChoice(callOpts.ToolChoice)
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
			"Content-Type": "application/json",
		}, m.defaultHeaders),
	); err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}

	return parseOpenAIResponse(&parsed, msgs)
}

// ChatStream implements streaming chat via SSE.
func (m *OllamaChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("ollama: msgs must not be empty")
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
	if len(callOpts.Tools) > 0 {
		reqBody.Tools = callOpts.Tools
	}
	if callOpts.ToolChoice != nil {
		reqBody.ToolChoice = formatToolChoice(callOpts.ToolChoice)
	}

	sseCh, err := httpx.DoSSERequest(
		ctx,
		m.httpClient,
		"POST",
		m.baseURL+"/v1/chat/completions",
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type": "application/json",
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processOpenAIStream(ctx, sseCh, outCh)
	return outCh, nil
}

// CountTokens estimates token count.
func (m *OllamaChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// ContextSize implements ContextSizer with OllamaConfig.ContextSize; zero
// means unknown.
func (m *OllamaChatModel) ContextSize() int {
	return m.contextSize
}
