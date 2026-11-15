package model

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const (
	defaultMiniMaxBaseURL         = "https://api.minimax.io/anthropic"
	defaultMiniMaxMaxOutputTokens = 4096
)

// MiniMaxConfig configures MiniMax's Anthropic-compatible Messages endpoint.
type MiniMaxConfig struct {
	APIKey          string
	SecretAPIKey    SecretStr // Preferred over APIKey.
	Model           string
	BaseURL         string // Complete prefix ending in /anthropic, not a full Messages URL.
	MaxOutputTokens int    // Defaults to 4096; a per-call WithMaxTokens overrides it.
	HTTPClient      *http.Client
	ClientOptions   *ClientOptions
}

// MiniMaxChatModel supports MiniMax text, thinking and tool-use messages.
// Thinking/effort constraints depend on the selected model and are enforced by
// the provider. Positive thinking-token budgets are rejected before dispatch.
type MiniMaxChatModel struct {
	model           string
	baseURL         string
	maxOutputTokens int
	headers         map[string]string
	httpClient      *http.Client
}

// NewMiniMaxChatModel creates a directly constructed MiniMax adapter.
func NewMiniMaxChatModel(cfg *MiniMaxConfig) (*MiniMaxChatModel, error) {
	if cfg == nil {
		return nil, fmt.Errorf("minimax: config is required")
	}
	key := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if key == "" {
		return nil, fmt.Errorf("minimax: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("minimax: Model is required")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("minimax: MaxOutputTokens must not be negative")
	}
	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMiniMaxMaxOutputTokens
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultMiniMaxBaseURL
	}
	headers := make(map[string]string)
	if cfg.ClientOptions != nil {
		for k, v := range cfg.ClientOptions.DefaultHeaders {
			headers[http.CanonicalHeaderKey(k)] = v
		}
	}
	headers["Authorization"] = "Bearer " + key
	headers["Content-Type"] = "application/json"
	return &MiniMaxChatModel{model: cfg.Model, baseURL: strings.TrimRight(base, "/"), maxOutputTokens: maxTokens, headers: headers, httpClient: defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions)}, nil
}

func (m *MiniMaxChatModel) request(ctx context.Context, msgs []*message.Msg, stream bool, opts []CallOption) (*anthropicRequest, error) {
	if ctx == nil {
		return nil, fmt.Errorf("minimax: context must not be nil")
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("minimax: msgs must not be empty")
	}
	var o CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.ThinkingBudget != nil && *o.ThinkingBudget > 0 {
		return nil, fmt.Errorf("minimax: thinking token budgets are unsupported; use WithThinking(enable, 0)")
	}
	maxTokens := m.maxOutputTokens
	if o.MaxTokens != nil {
		maxTokens = *o.MaxTokens
	}
	if maxTokens <= 0 {
		return nil, fmt.Errorf("minimax: max_tokens must be positive")
	}
	r := baseAnthropicRequest(m.model, maxTokens, miniMaxHistory(msgs), &o)
	r.Stream = stream
	if o.ThinkingEnable != nil {
		kind := "disabled"
		if *o.ThinkingEnable {
			kind = "adaptive"
		}
		r.Thinking = &anthropicThinking{Type: kind}
	}
	if o.ReasoningEffort != nil {
		r.OutputConfig = &anthropicOutputConfig{Effort: *o.ReasoningEffort}
	}
	return r, nil
}

// UnifiedAgent stores multiple rounds in one assistant message. Messages API
// tool results belong to the user role, so expand only the MiniMax wire view;
// the stored messages and their content blocks remain untouched.
func miniMaxHistory(msgs []*message.Msg) []*message.Msg {
	var out []*message.Msg
	for _, msg := range msgs {
		if msg == nil || msg.Role != message.RoleAssistant {
			out = append(out, msg)
			continue
		}
		blocks := msg.GetContentBlocks()
		start := 0
		role := message.RoleAssistant
		for i, block := range blocks {
			next := message.RoleAssistant
			if _, ok := block.(message.ToolResultBlock); ok {
				next = message.RoleUser
			}
			if i == 0 {
				role = next
			}
			if next != role {
				segment := *msg
				segment.Role, segment.Content = role, blocks[start:i]
				out = append(out, &segment)
				start, role = i, next
			}
		}
		segment := *msg
		segment.Role, segment.Content = role, blocks[start:]
		out = append(out, &segment)
	}
	return out
}

// Chat generates a buffered reply. WithMaxTokens overrides the configured limit;
// Positive thinking budgets are rejected; WithReasoningEffort sets output effort.
func (m *MiniMaxChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	r, err := m.request(ctx, msgs, false, opts)
	if err != nil {
		return nil, err
	}
	var parsed anthropicResponse
	if err := httpx.DoJSONRequest(ctx, m.httpClient, http.MethodPost, m.baseURL+"/v1/messages", r, &parsed, m.headers); err != nil {
		return nil, fmt.Errorf("minimax: %w", err)
	}
	response, err := parseAnthropicResponse(&parsed)
	if err != nil {
		return nil, fmt.Errorf("minimax: %w", err)
	}
	response.StopReason = normalizeStopReason(parsed.StopReason)
	return response, nil
}

// ChatStream emits deltas followed by an assembled final response. Inspect
// ChatResponse.Error and ctx.Err(); cancellation may close without a final reply.
func (m *MiniMaxChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	r, err := m.request(ctx, msgs, true, opts)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	sse, err := httpx.DoSSERequest(streamCtx, m.httpClient, http.MethodPost, m.baseURL+"/v1/messages", r, m.headers)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("minimax: %w", err)
	}
	out := make(chan ChatResponse, 16)
	go func() {
		defer cancel()
		processAnthropicStreamWithPolicy(streamCtx, sse, out, anthropicStreamPolicy{stopAtMessageStop: true, preserveReportedUsage: true})
	}()
	return out, nil
}

// CountTokens estimates input size; it is separate from provider-reported usage.
func (m *MiniMaxChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// DisableThinkingOptions supplies the structured-output compatibility attempt.
// Some models reject disabling thinking; provider rejection remains observable.
func (m *MiniMaxChatModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}
