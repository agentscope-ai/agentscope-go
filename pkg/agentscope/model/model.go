package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	aserr "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// ChatModel is the unified interface for all chat models.
type ChatModel interface {
	// Chat generates a reply given conversation history.
	Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error)

	// ChatStream returns a channel of streaming ChatResponse chunks.
	// The final chunk has IsLast=true.
	// If a model does not support streaming, it should return (nil, ErrStreamNotSupported).
	ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error)

	// CountTokens estimates the token count for messages and optional tool schemas.
	CountTokens(msgs []*message.Msg, tools []ToolSchema) int
}

// ChatResponse represents a model response, supporting both streaming and non-streaming.
type ChatResponse struct {
	Content   []message.ContentBlock `json:"content"`
	IsLast    bool                   `json:"is_last"`
	ID        string                 `json:"id"`
	CreatedAt string                 `json:"created_at"`
	Usage     *ChatUsage             `json:"usage,omitempty"`
	Metadata  map[string]any         `json:"metadata,omitempty"`
	ModelName string                 `json:"model_name,omitempty"`

	// StopReason is the normalized reason generation stopped: "stop", "length"
	// (max tokens / truncated), "tool_calls", or "content_filter". Empty when the
	// provider did not report one.
	StopReason string `json:"stop_reason,omitempty"`

	// Error carries a terminal streaming failure. A streaming consumer must check
	// this on every chunk: a non-nil Error means the stream failed mid-flight and
	// the accumulated content is incomplete (previously such failures were only
	// logged, so a truncated stream looked complete with IsLast=true).
	Error error `json:"-"`
}

// Normalized StopReason values.
const (
	StopReasonStop          = "stop"
	StopReasonLength        = "length"
	StopReasonToolCalls     = "tool_calls"
	StopReasonContentFilter = "content_filter"
)

// normalizeStopReason maps a provider-specific finish/stop reason to the
// normalized StopReason vocabulary. Unknown non-empty values are returned as-is.
func normalizeStopReason(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ""
	case "stop", "end_turn", "eos", "complete", "finished":
		return StopReasonStop
	case "length", "max_tokens", "model_length", "max_output_tokens":
		return StopReasonLength
	case "tool_calls", "tool_use", "function_call":
		return StopReasonToolCalls
	case "content_filter", "safety", "recitation", "blocklist":
		return StopReasonContentFilter
	default:
		return raw
	}
}

// GetTextContent concatenates all TextBlock content from the response.
func (r *ChatResponse) GetTextContent() string {
	var text string
	for _, b := range r.Content {
		if tb, ok := b.(message.TextBlock); ok {
			text += tb.Text
		}
	}
	return text
}

// Deprecated: Msg field on ChatResponse. Use Content field instead.
// ToMsg converts a ChatResponse into a Msg for backward compatibility.
func (r *ChatResponse) ToMsg(name string) *message.Msg {
	msg := message.AssistantMsg(name, r.Content)
	msg.ID = r.ID
	if r.Usage != nil {
		msg.Usage = &message.Usage{
			InputTokens:              r.Usage.InputTokens,
			OutputTokens:             r.Usage.OutputTokens,
			CacheCreationInputTokens: r.Usage.CacheCreationInputTokens,
			CacheInputTokens:         r.Usage.CacheInputTokens,
		}
	}
	return msg
}

// ChatUsage tracks token consumption for a model call.
type ChatUsage struct {
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	Time                     float64 `json:"time,omitempty"` // seconds
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CacheInputTokens         int     `json:"cache_input_tokens,omitempty"`
}

// ToolSchema defines a tool for model API function calling.
type ToolSchema struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function for the model.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ToolChoice controls how the model selects tools.
type ToolChoice struct {
	Mode  string   `json:"mode"`            // "auto", "none", "required", or specific tool name
	Tools []string `json:"tools,omitempty"` // optional whitelist filter
}

// ContextSizer is an optional interface that ChatModel implementations can
// provide to report the model's context window size in tokens.
// Used by context compression to determine when compression is needed.
type ContextSizer interface {
	ContextSize() int
}

// ModelNamer is an optional interface that ChatModel implementations can
// provide to report the model name used for model card lookups.
type ModelNamer interface {
	ModelName() string
}

// ResolveContextSize returns the context window size for a model.
// It checks (in order): ContextSizer interface, ModelNamer + model card lookup, default.
//
// A ConnectivityAwareModel resolves to the smaller of its members' resolved
// windows, independent of circuit state: a single call can fall back from the
// cloud model to the local one. The result never exceeds what either member
// would resolve to on its own.
func ResolveContextSize(m ChatModel, fallback int) int {
	if cam, ok := m.(*ConnectivityAwareModel); ok {
		return min(ResolveContextSize(cam.local, fallback), ResolveContextSize(cam.cloud, fallback))
	}
	if cs, ok := m.(ContextSizer); ok {
		if s := cs.ContextSize(); s > 0 {
			return s
		}
	}
	if mn, ok := m.(ModelNamer); ok {
		if card, err := GetModelCard(mn.ModelName()); err == nil && card.ContextSize > 0 {
			return card.ContextSize
		}
	}
	return fallback
}

// ErrStreamNotSupported indicates that a model does not support streaming calls.
var ErrStreamNotSupported = fmt.Errorf("chat model: stream not supported")

// CallOptions stores model call options configured via functional options.
type CallOptions struct {
	Temperature     *float64
	MaxTokens       *int
	TopP            *float64
	Seed            *int64 // sampling seed where the provider supports it (eval determinism, HARNESS_DESIGN C5)
	Tools           []ToolSchema
	ToolChoice      *ToolChoice
	ThinkingEnable  *bool
	ThinkingBudget  *int
	ReasoningEffort *string // "low", "medium", "high"
	Voice           *string // audio output voice (e.g. "alloy"); enables audio modality
	ResponseFormat  *ResponseFormat
	MaxRetries      int
	RetryDelay      time.Duration
}

// CallOption mutates CallOptions.
type CallOption func(*CallOptions)

func WithTemperature(t float64) CallOption {
	return func(o *CallOptions) {
		o.Temperature = &t
	}
}

// WithThinkingDisabled explicitly turns provider-side thinking/reasoning off
// for this call. Used by the structured-output fallback ladder (upstream
// #2140): providers with a thinking toggle honor false by sending
// enable_thinking=false (or equivalent); providers without one ignore it.
func WithThinkingDisabled() CallOption {
	return func(o *CallOptions) {
		f := false
		o.ThinkingEnable = &f
	}
}

func WithMaxTokens(n int) CallOption {
	return func(o *CallOptions) {
		o.MaxTokens = &n
	}
}

// WithSeed pins the sampling seed on providers that support it (OpenAI
// family). Providers without seed support ignore it.
func WithSeed(seed int64) CallOption {
	return func(o *CallOptions) {
		o.Seed = &seed
	}
}

func WithTopP(p float64) CallOption {
	return func(o *CallOptions) {
		o.TopP = &p
	}
}

// WithTools sets the tool schemas for native function calling.
func WithTools(tools []ToolSchema) CallOption {
	return func(o *CallOptions) {
		o.Tools = tools
	}
}

// WithToolChoice sets the tool choice mode.
func WithToolChoice(tc *ToolChoice) CallOption {
	return func(o *CallOptions) {
		o.ToolChoice = tc
	}
}

// WithThinking enables extended thinking with an optional token budget.
func WithThinking(enable bool, budget int) CallOption {
	return func(o *CallOptions) {
		o.ThinkingEnable = &enable
		if budget > 0 {
			o.ThinkingBudget = &budget
		}
	}
}

// WithReasoningEffort sets the reasoning effort level (e.g. "low", "medium", "high").
func WithReasoningEffort(effort string) CallOption {
	return func(o *CallOptions) {
		o.ReasoningEffort = &effort
	}
}

// WithVoice enables audio output with the specified voice (e.g. "alloy", "coral").
// When set, the model request includes audio modality and PCM16 format.
func WithVoice(voice string) CallOption {
	return func(o *CallOptions) {
		o.Voice = &voice
	}
}

// WithRetries configures retry behavior for transient errors.
func WithRetries(maxRetries int, delay time.Duration) CallOption {
	return func(o *CallOptions) {
		o.MaxRetries = maxRetries
		o.RetryDelay = delay
	}
}

// ResponseFormat configures the model to return structured output.
type ResponseFormat struct {
	// Type is the response format type: "text", "json_object", or "json_schema".
	Type string `json:"type"`
	// JSONSchema is the JSON Schema the model output must conform to.
	// Only used when Type is "json_schema".
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
	// Name is an optional identifier for the schema (used by some providers).
	Name string `json:"name,omitempty"`
	// Strict indicates whether the model must strictly follow the schema.
	Strict bool `json:"strict,omitempty"`
}

// WithResponseFormat sets the response format for structured output.
func WithResponseFormat(rf *ResponseFormat) CallOption {
	return func(o *CallOptions) {
		o.ResponseFormat = rf
	}
}

// ValidateToolChoice validates that a tool choice references valid tools.
func ValidateToolChoice(tc *ToolChoice, tools []ToolSchema) error {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case "auto", "none", "required", "":
		return nil
	default:
		for _, t := range tools {
			if t.Function.Name == tc.Mode {
				return nil
			}
		}
		return fmt.Errorf("model: tool_choice references unknown tool %q", tc.Mode)
	}
}

// IsRetryableError checks if an error is a transient error worth retrying.
// It first honors a typed AgentError.Retryable flag, then falls back to
// substring classification of the error message.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if aserr.IsRetryable(err) {
		return true
	}
	s := err.Error()
	for _, pattern := range []string{"429", "rate limit", "timeout", "connection reset", "connection refused", "500", "502", "503", "overloaded"} {
		if contains(s, pattern) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsLower(s, substr))
}

func containsLower(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// countTokensByBytes estimates tokens from byte length across all block types.
func countTokensByBytes(msgs []*message.Msg, tools []ToolSchema) int {
	total := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, b := range m.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				total += len(blk.Text)
			case message.ThinkingBlock:
				total += len(blk.Thinking)
			case message.ToolCallBlock:
				total += len(blk.Input)
			case message.ToolResultBlock:
				if text, ok := blk.Output.(string); ok {
					total += len(text)
				}
				// A multimodal tool result (e.g. Read on an image, upstream
				// #2114) stores its payload as a block list; counting only
				// the text would under-report the base64 image by orders of
				// magnitude and defeat the context-size guard.
				if list, ok := blk.Output.([]message.ContentBlock); ok {
					for index, sub := range list {
						// Include separators inserted by text rendering.
						if index > 0 {
							total++
						}
						if text, ok := sub.(message.TextBlock); ok {
							total += len(text.Text)
						}
						db, isData := sub.(message.DataBlock)
						if !isData {
							continue
						}
						switch src := db.Source.(type) {
						case message.Base64Source:
							total += len(src.Data) * 3 / 4 // base64 -> raw bytes
						case message.URLSource:
							total += len(src.URL)
						}
					}
				}
			case message.HintBlock:
				total += len(blk.GetHintText())
			case message.DataBlock:
				if src, ok := blk.Source.(message.Base64Source); ok {
					total += len(src.Data) * 3 / 4 // base64 → raw bytes
				} else if src, ok := blk.Source.(message.URLSource); ok {
					total += len(src.URL)
				}
			}
		}
	}
	if len(tools) > 0 {
		b, _ := json.Marshal(tools)
		total += len(b)
	}
	return total / 4
}
