package model

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const defaultOpenAIBaseURL = "https://api.openai.com"

// OpenAIChatModel wraps the OpenAI Chat Completions API.
type OpenAIChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	defaultHeaders map[string]string

	httpClient *http.Client
}

// OpenAIConfig configures OpenAIChatModel.
type OpenAIConfig struct {
	APIKey       string
	SecretAPIKey SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL      string
	Model        string

	HTTPClient    *http.Client
	ClientOptions *ClientOptions
}

// NewOpenAIChatModel creates a ChatModel backed by OpenAI.
func NewOpenAIChatModel(cfg OpenAIConfig) (*OpenAIChatModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("openai: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &OpenAIChatModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements the ChatModel interface for OpenAI.
func (m *OpenAIChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("openai: msgs must not be empty")
	}
	if ctx == nil {
		return nil, fmt.Errorf("openai: context must not be nil")
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
		reqBody.MaxCompletionTokens = callOpts.MaxTokens
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
	if callOpts.Voice != nil {
		reqBody.Audio = &openAIAudioConfig{Voice: *callOpts.Voice, Format: "pcm16"}
		reqBody.Modalities = []string{"text", "audio"}
	}

	// Retry loop
	maxRetries := callOpts.MaxRetries
	retryDelay := callOpts.RetryDelay
	if retryDelay == 0 {
		retryDelay = time.Second
	}

	var parsed openAIChatResponse
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return nil, fmt.Errorf("openai: %w", err)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("openai: %w", err)
		}
		lastErr = httpx.DoJSONRequest(
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
		)
		if lastErr == nil {
			break
		}
		if !IsRetryableError(lastErr) {
			return nil, fmt.Errorf("openai: %w", lastErr)
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("openai: %w", lastErr)
	}

	return parseOpenAIResponse(&parsed, msgs)
}

// ChatStream implements streaming chat via SSE (OpenAI format).
func (m *OpenAIChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("openai: msgs must not be empty")
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
		reqBody.MaxCompletionTokens = callOpts.MaxTokens
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
	if callOpts.Voice != nil {
		reqBody.Audio = &openAIAudioConfig{Voice: *callOpts.Voice, Format: "pcm16"}
		reqBody.Modalities = []string{"text", "audio"}
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
		return nil, fmt.Errorf("openai: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processOpenAIStream(ctx, sseCh, outCh)
	return outCh, nil
}

// CountTokens estimates token count.
func (m *OpenAIChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// convertMessagesToOpenAI maps internal Msg instances to OpenAI messages.
// Uses the OpenAI formatter for full block-type support, then converts to typed structs.
func convertMessagesToOpenAI(msgs []*message.Msg) []openAIChatMessage {
	return convertMessagesWithFormatter(msgs, formatter.NewOpenAIFormatter())
}

func convertMessagesWithFormatter(msgs []*message.Msg, f formatter.Formatter) []openAIChatMessage {
	formatted, err := f.Format(msgs)
	if err != nil {
		return convertMessagesToOpenAIFallback(msgs)
	}

	out := make([]openAIChatMessage, 0, len(formatted))
	for _, m := range formatted {
		msg := openAIChatMessage{}
		if role, ok := m["role"].(string); ok {
			msg.Role = role
		}
		if name, ok := m["name"].(string); ok {
			msg.Name = name
		}
		if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
			msg.ReasoningContent = rc
		}
		if content, ok := m["content"].(string); ok {
			msg.Content = content
		} else if content, ok := m["content"].([]map[string]any); ok {
			// Preserve the structured multimodal array (text + image_url/input_audio
			// parts). Marshaling it to a string would send images/audio as an inert
			// text blob, silently disabling vision/audio input.
			msg.Content = content
		}
		if tcid, ok := m["tool_call_id"].(string); ok {
			msg.ToolCallID = tcid
		}
		if tcs, ok := m["tool_calls"].([]map[string]any); ok {
			for _, tc := range tcs {
				otc := openAIToolCall{}
				otc.ID, _ = tc["id"].(string)
				otc.Type, _ = tc["type"].(string)
				if fn, ok := tc["function"].(map[string]any); ok {
					otc.Function.Name, _ = fn["name"].(string)
					otc.Function.Arguments, _ = fn["arguments"].(string)
				}
				msg.ToolCalls = append(msg.ToolCalls, otc)
			}
		}
		out = append(out, msg)
	}
	return out
}

func convertMessagesToOpenAIFallback(msgs []*message.Msg) []openAIChatMessage {
	out := make([]openAIChatMessage, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		role := string(m.Role)
		if role == "" {
			role = "user"
		}
		msg := openAIChatMessage{Role: role, Name: m.Name}
		if txt := m.GetTextContent("\n"); txt != nil {
			msg.Content = *txt
		}
		out = append(out, msg)
	}
	return out
}
