package event

import (
	"fmt"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// EventType enumerates all event types in the agent lifecycle.
type EventType string

const (
	// Reply lifecycle
	EventReplyStart EventType = "reply_start"
	EventReplyEnd   EventType = "reply_end"

	// Model call lifecycle
	EventModelCallStart EventType = "model_call_start"
	EventModelCallEnd   EventType = "model_call_end"

	// Text block streaming
	EventTextBlockStart EventType = "text_block_start"
	EventTextBlockDelta EventType = "text_block_delta"
	EventTextBlockEnd   EventType = "text_block_end"

	// Thinking block streaming
	EventThinkingBlockStart EventType = "thinking_block_start"
	EventThinkingBlockDelta EventType = "thinking_block_delta"
	EventThinkingBlockEnd   EventType = "thinking_block_end"

	// Data block streaming
	EventDataBlockStart EventType = "data_block_start"
	EventDataBlockDelta EventType = "data_block_delta"
	EventDataBlockEnd   EventType = "data_block_end"

	// Tool call streaming
	EventToolCallStart EventType = "tool_call_start"
	EventToolCallDelta EventType = "tool_call_delta"
	EventToolCallEnd   EventType = "tool_call_end"

	// Tool result streaming
	EventToolResultStart     EventType = "tool_result_start"
	EventToolResultTextDelta EventType = "tool_result_text_delta"
	EventToolResultDataDelta EventType = "tool_result_data_delta"
	EventToolResultEnd       EventType = "tool_result_end"

	// Hint block (one-shot, not streamed)
	EventHintBlock EventType = "hint_block"

	// Human-in-the-loop
	EventRequireUserConfirm       EventType = "require_user_confirm"
	EventUserConfirmResult        EventType = "user_confirm_result"
	EventRequireExternalExecution EventType = "require_external_execution"
	EventExternalExecutionResult  EventType = "external_execution_result"

	// Sandbox / tool execution lifecycle
	EventToolExecStart    EventType = "tool_exec_start"
	EventToolExecEnd      EventType = "tool_exec_end"
	EventToolPolicyDenied EventType = "tool_policy_denied"

	// Control
	EventExceedMaxIters EventType = "exceed_max_iters"
	EventCustom         EventType = "custom"
)

// Event is the interface implemented by all event types.
type Event interface {
	GetEventType() EventType
	GetEventID() string
	GetReplyID() string
	// EventTypeString returns the event type as a plain string.
	// Used by message.Msg.AppendEvent to avoid circular imports.
	EventTypeString() string
}

// Base provides common fields for all events.
type Base struct {
	ID        string         `json:"id"`
	CreatedAt string         `json:"created_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

func newBase() Base {
	return Base{
		ID:        agentscope.GenerateID(),
		CreatedAt: time.Now().Format(message.TimestampFormat),
	}
}

// --- Reply lifecycle ---

type ReplyStartEvent struct {
	Base
	SessionID string       `json:"session_id"`
	ReplyID   string       `json:"reply_id"`
	Name      string       `json:"name"`
	Role      message.Role `json:"role"`
}

func (e ReplyStartEvent) GetEventType() EventType { return EventReplyStart }
func (e ReplyStartEvent) GetEventID() string      { return e.ID }
func (e ReplyStartEvent) GetReplyID() string      { return e.ReplyID }

func NewReplyStartEvent(sessionID, replyID, name string, role message.Role) ReplyStartEvent {
	return ReplyStartEvent{Base: newBase(), SessionID: sessionID, ReplyID: replyID, Name: name, Role: role}
}

type ReplyEndEvent struct {
	Base
	SessionID string `json:"session_id"`
	ReplyID   string `json:"reply_id"`
	// FinishedReason says why the reply ended (default completed).
	FinishedReason types.ReplyFinishedReason `json:"finished_reason,omitempty"`
	// Error carries structured error info, populated only when
	// FinishedReason == types.ReplyError.
	Error *types.ReplyErrorInfo `json:"error,omitempty"`
}

func (e ReplyEndEvent) GetEventType() EventType { return EventReplyEnd }
func (e ReplyEndEvent) GetEventID() string      { return e.ID }
func (e ReplyEndEvent) GetReplyID() string      { return e.ReplyID }

// NewReplyEndEvent creates a completed-reply end event.
func NewReplyEndEvent(sessionID, replyID string) ReplyEndEvent {
	return ReplyEndEvent{Base: newBase(), SessionID: sessionID, ReplyID: replyID,
		FinishedReason: types.ReplyCompleted}
}

// NewReplyEndEventWithReason creates an end event with an explicit reason
// (interrupted, exceed_max_iters, ...).
func NewReplyEndEventWithReason(sessionID, replyID string, reason types.ReplyFinishedReason) ReplyEndEvent {
	return ReplyEndEvent{Base: newBase(), SessionID: sessionID, ReplyID: replyID, FinishedReason: reason}
}

// NewReplyEndEventWithError creates an error end event.
func NewReplyEndEventWithError(sessionID, replyID string, errType types.ReplyErrorType, msg string) ReplyEndEvent {
	return ReplyEndEvent{Base: newBase(), SessionID: sessionID, ReplyID: replyID,
		FinishedReason: types.ReplyError,
		Error:          &types.ReplyErrorInfo{Type: errType, Message: msg}}
}

// --- Model call lifecycle ---

type ModelCallStartEvent struct {
	Base
	ReplyID   string `json:"reply_id"`
	ModelName string `json:"model_name"`
}

func (e ModelCallStartEvent) GetEventType() EventType { return EventModelCallStart }
func (e ModelCallStartEvent) GetEventID() string      { return e.ID }
func (e ModelCallStartEvent) GetReplyID() string      { return e.ReplyID }

func NewModelCallStartEvent(replyID, modelName string) ModelCallStartEvent {
	return ModelCallStartEvent{Base: newBase(), ReplyID: replyID, ModelName: modelName}
}

type ModelCallEndEvent struct {
	Base
	ReplyID             string `json:"reply_id"`
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_tokens,omitempty"`
}

func (e ModelCallEndEvent) GetEventType() EventType { return EventModelCallEnd }
func (e ModelCallEndEvent) GetEventID() string      { return e.ID }
func (e ModelCallEndEvent) GetReplyID() string      { return e.ReplyID }

func NewModelCallEndEvent(replyID string, inputTokens, outputTokens int) ModelCallEndEvent {
	return ModelCallEndEvent{Base: newBase(), ReplyID: replyID, InputTokens: inputTokens, OutputTokens: outputTokens}
}

// NewModelCallEndEventWithCache is like NewModelCallEndEvent but also carries
// prompt-cache token counts (e.g. Anthropic cache_creation_input_tokens /
// cache_read_input_tokens). Use when the model's ChatUsage reports them.
func NewModelCallEndEventWithCache(replyID string, inputTokens, outputTokens, cacheCreation, cacheRead int) ModelCallEndEvent {
	return ModelCallEndEvent{
		Base:                newBase(),
		ReplyID:             replyID,
		InputTokens:         inputTokens,
		OutputTokens:        outputTokens,
		CacheCreationTokens: cacheCreation,
		CacheReadTokens:     cacheRead,
	}
}

// --- Text block streaming ---

type TextBlockStartEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
}

func (e TextBlockStartEvent) GetEventType() EventType { return EventTextBlockStart }
func (e TextBlockStartEvent) GetEventID() string      { return e.ID }
func (e TextBlockStartEvent) GetReplyID() string      { return e.ReplyID }

func NewTextBlockStartEvent(replyID, blockID string) TextBlockStartEvent {
	return TextBlockStartEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID}
}

type TextBlockDeltaEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
	Delta   string `json:"delta"`
}

func (e TextBlockDeltaEvent) GetEventType() EventType { return EventTextBlockDelta }
func (e TextBlockDeltaEvent) GetEventID() string      { return e.ID }
func (e TextBlockDeltaEvent) GetReplyID() string      { return e.ReplyID }

func NewTextBlockDeltaEvent(replyID, blockID, delta string) TextBlockDeltaEvent {
	return TextBlockDeltaEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID, Delta: delta}
}

type TextBlockEndEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
}

func (e TextBlockEndEvent) GetEventType() EventType { return EventTextBlockEnd }
func (e TextBlockEndEvent) GetEventID() string      { return e.ID }
func (e TextBlockEndEvent) GetReplyID() string      { return e.ReplyID }

func NewTextBlockEndEvent(replyID, blockID string) TextBlockEndEvent {
	return TextBlockEndEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID}
}

// --- Thinking block streaming ---

type ThinkingBlockStartEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
}

func (e ThinkingBlockStartEvent) GetEventType() EventType { return EventThinkingBlockStart }
func (e ThinkingBlockStartEvent) GetEventID() string      { return e.ID }
func (e ThinkingBlockStartEvent) GetReplyID() string      { return e.ReplyID }

func NewThinkingBlockStartEvent(replyID, blockID string) ThinkingBlockStartEvent {
	return ThinkingBlockStartEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID}
}

type ThinkingBlockDeltaEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
	Delta   string `json:"delta"`
}

func (e ThinkingBlockDeltaEvent) GetEventType() EventType { return EventThinkingBlockDelta }
func (e ThinkingBlockDeltaEvent) GetEventID() string      { return e.ID }
func (e ThinkingBlockDeltaEvent) GetReplyID() string      { return e.ReplyID }

func NewThinkingBlockDeltaEvent(replyID, blockID, delta string) ThinkingBlockDeltaEvent {
	return ThinkingBlockDeltaEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID, Delta: delta}
}

type ThinkingBlockEndEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
}

func (e ThinkingBlockEndEvent) GetEventType() EventType { return EventThinkingBlockEnd }
func (e ThinkingBlockEndEvent) GetEventID() string      { return e.ID }
func (e ThinkingBlockEndEvent) GetReplyID() string      { return e.ReplyID }

func NewThinkingBlockEndEvent(replyID, blockID string) ThinkingBlockEndEvent {
	return ThinkingBlockEndEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID}
}

// --- Data block streaming ---

type DataBlockStartEvent struct {
	Base
	ReplyID   string `json:"reply_id"`
	BlockID   string `json:"block_id"`
	MediaType string `json:"media_type"`
}

func (e DataBlockStartEvent) GetEventType() EventType { return EventDataBlockStart }
func (e DataBlockStartEvent) GetEventID() string      { return e.ID }
func (e DataBlockStartEvent) GetReplyID() string      { return e.ReplyID }

func NewDataBlockStartEvent(replyID, blockID, mediaType string) DataBlockStartEvent {
	return DataBlockStartEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID, MediaType: mediaType}
}

type DataBlockDeltaEvent struct {
	Base
	ReplyID   string `json:"reply_id"`
	BlockID   string `json:"block_id"`
	Data      string `json:"data"` // base64 encoded chunk
	MediaType string `json:"media_type"`
}

func (e DataBlockDeltaEvent) GetEventType() EventType { return EventDataBlockDelta }
func (e DataBlockDeltaEvent) GetEventID() string      { return e.ID }
func (e DataBlockDeltaEvent) GetReplyID() string      { return e.ReplyID }

func NewDataBlockDeltaEvent(replyID, blockID, data, mediaType string) DataBlockDeltaEvent {
	return DataBlockDeltaEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID, Data: data, MediaType: mediaType}
}

type DataBlockEndEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
}

func (e DataBlockEndEvent) GetEventType() EventType { return EventDataBlockEnd }
func (e DataBlockEndEvent) GetEventID() string      { return e.ID }
func (e DataBlockEndEvent) GetReplyID() string      { return e.ReplyID }

func NewDataBlockEndEvent(replyID, blockID string) DataBlockEndEvent {
	return DataBlockEndEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID}
}

// --- Tool call streaming ---

type ToolCallStartEvent struct {
	Base
	ReplyID       string `json:"reply_id"`
	ToolCallID    string `json:"tool_call_id"`
	ToolCallName  string `json:"tool_call_name"`
	ToolCallInput string `json:"tool_call_input,omitempty"`
}

func (e ToolCallStartEvent) GetEventType() EventType { return EventToolCallStart }
func (e ToolCallStartEvent) GetEventID() string      { return e.ID }
func (e ToolCallStartEvent) GetReplyID() string      { return e.ReplyID }

func NewToolCallStartEvent(replyID, toolCallID, toolCallName string) ToolCallStartEvent {
	return ToolCallStartEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID, ToolCallName: toolCallName}
}

type ToolCallDeltaEvent struct {
	Base
	ReplyID    string `json:"reply_id"`
	ToolCallID string `json:"tool_call_id"`
	Delta      string `json:"delta"` // JSON fragment of arguments
}

func (e ToolCallDeltaEvent) GetEventType() EventType { return EventToolCallDelta }
func (e ToolCallDeltaEvent) GetEventID() string      { return e.ID }
func (e ToolCallDeltaEvent) GetReplyID() string      { return e.ReplyID }

func NewToolCallDeltaEvent(replyID, toolCallID, delta string) ToolCallDeltaEvent {
	return ToolCallDeltaEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID, Delta: delta}
}

type ToolCallEndEvent struct {
	Base
	ReplyID    string `json:"reply_id"`
	ToolCallID string `json:"tool_call_id"`
}

func (e ToolCallEndEvent) GetEventType() EventType { return EventToolCallEnd }
func (e ToolCallEndEvent) GetEventID() string      { return e.ID }
func (e ToolCallEndEvent) GetReplyID() string      { return e.ReplyID }

func NewToolCallEndEvent(replyID, toolCallID string) ToolCallEndEvent {
	return ToolCallEndEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID}
}

// --- Tool result streaming ---

type ToolResultStartEvent struct {
	Base
	ReplyID      string `json:"reply_id"`
	ToolCallID   string `json:"tool_call_id"`
	ToolCallName string `json:"tool_call_name"`
}

func (e ToolResultStartEvent) GetEventType() EventType { return EventToolResultStart }
func (e ToolResultStartEvent) GetEventID() string      { return e.ID }
func (e ToolResultStartEvent) GetReplyID() string      { return e.ReplyID }

func NewToolResultStartEvent(replyID, toolCallID, toolCallName string) ToolResultStartEvent {
	return ToolResultStartEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID, ToolCallName: toolCallName}
}

type ToolResultTextDeltaEvent struct {
	Base
	ReplyID    string `json:"reply_id"`
	ToolCallID string `json:"tool_call_id"`
	Delta      string `json:"delta"`
}

func (e ToolResultTextDeltaEvent) GetEventType() EventType { return EventToolResultTextDelta }
func (e ToolResultTextDeltaEvent) GetEventID() string      { return e.ID }
func (e ToolResultTextDeltaEvent) GetReplyID() string      { return e.ReplyID }

func NewToolResultTextDeltaEvent(replyID, toolCallID, delta string) ToolResultTextDeltaEvent {
	return ToolResultTextDeltaEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID, Delta: delta}
}

type ToolResultDataDeltaEvent struct {
	Base
	ReplyID    string `json:"reply_id"`
	ToolCallID string `json:"tool_call_id"`
	BlockID    string `json:"block_id"`
	MediaType  string `json:"media_type"`
	Data       string `json:"data,omitempty"` // base64
	URL        string `json:"url,omitempty"`
}

func (e ToolResultDataDeltaEvent) GetEventType() EventType { return EventToolResultDataDelta }
func (e ToolResultDataDeltaEvent) GetEventID() string      { return e.ID }
func (e ToolResultDataDeltaEvent) GetReplyID() string      { return e.ReplyID }

// NewToolResultDataDeltaEvent builds the event. Producers MUST pass exactly
// one of data/url and call Validate() before emitting (upstream #2370
// enforces this at construction in Python).
func NewToolResultDataDeltaEvent(replyID, toolCallID, blockID, mediaType, data, url string) ToolResultDataDeltaEvent {
	return ToolResultDataDeltaEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID, BlockID: blockID, MediaType: mediaType, Data: data, URL: url}
}

// Validate enforces the source invariant (upstream #2370): exactly one of
// Data (inline base64) or URL must be set. Producers must validate before
// emitting; both-empty and both-set events are invalid.
func (e ToolResultDataDeltaEvent) Validate() error {
	hasData := e.Data != ""
	hasURL := e.URL != ""
	if hasData == hasURL {
		if hasData {
			return fmt.Errorf("tool result data delta: Data and URL are mutually exclusive, got both")
		}
		return fmt.Errorf("tool result data delta: exactly one of Data or URL is required, got neither")
	}
	return nil
}

type ToolResultEndEvent struct {
	Base
	ReplyID    string                  `json:"reply_id"`
	ToolCallID string                  `json:"tool_call_id"`
	State      message.ToolResultState `json:"state"`
}

func (e ToolResultEndEvent) GetEventType() EventType { return EventToolResultEnd }
func (e ToolResultEndEvent) GetEventID() string      { return e.ID }
func (e ToolResultEndEvent) GetReplyID() string      { return e.ReplyID }

// GetMetadata exposes structured tool output to message stream reconstruction.
// The returned map belongs to the event and must be treated as read-only.
func (e ToolResultEndEvent) GetMetadata() map[string]any { return e.Metadata }

func NewToolResultEndEvent(replyID, toolCallID string, state message.ToolResultState) ToolResultEndEvent {
	return ToolResultEndEvent{Base: newBase(), ReplyID: replyID, ToolCallID: toolCallID, State: state}
}

// --- Hint block (one-shot) ---

type HintBlockEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	BlockID string `json:"block_id"`
	Source  string `json:"source,omitempty"`
	Hint    any    `json:"hint"` // string or []message.ContentBlock
}

func (e HintBlockEvent) GetEventType() EventType { return EventHintBlock }
func (e HintBlockEvent) GetEventID() string      { return e.ID }
func (e HintBlockEvent) GetReplyID() string      { return e.ReplyID }

// NewHintBlockEvent creates a HintBlockEvent. The hint parameter accepts
// either a plain string or any value (e.g. []message.ContentBlock for
// multimodal hints).
func NewHintBlockEvent(replyID, blockID, source string, hint any) HintBlockEvent {
	return HintBlockEvent{Base: newBase(), ReplyID: replyID, BlockID: blockID, Source: source, Hint: hint}
}

// --- Human-in-the-loop ---

// ConfirmResult carries the user's decision for a single tool call.
type ConfirmResult struct {
	Confirmed bool                  `json:"confirmed"`
	ToolCall  message.ToolCallBlock `json:"tool_call"`
	Rules     []any                 `json:"rules,omitempty"`
}

type RequireUserConfirmEvent struct {
	Base
	ReplyID   string                  `json:"reply_id"`
	ToolCalls []message.ToolCallBlock `json:"tool_calls"`
}

func (e RequireUserConfirmEvent) GetEventType() EventType { return EventRequireUserConfirm }
func (e RequireUserConfirmEvent) GetEventID() string      { return e.ID }
func (e RequireUserConfirmEvent) GetReplyID() string      { return e.ReplyID }

func NewRequireUserConfirmEvent(replyID string, toolCalls []message.ToolCallBlock) RequireUserConfirmEvent {
	return RequireUserConfirmEvent{Base: newBase(), ReplyID: replyID, ToolCalls: toolCalls}
}

type UserConfirmResultEvent struct {
	Base
	ReplyID        string          `json:"reply_id"`
	ConfirmResults []ConfirmResult `json:"confirm_results"`
}

func (e UserConfirmResultEvent) GetEventType() EventType { return EventUserConfirmResult }
func (e UserConfirmResultEvent) GetEventID() string      { return e.ID }
func (e UserConfirmResultEvent) GetReplyID() string      { return e.ReplyID }

func NewUserConfirmResultEvent(replyID string, results []ConfirmResult) UserConfirmResultEvent {
	return UserConfirmResultEvent{Base: newBase(), ReplyID: replyID, ConfirmResults: results}
}

type RequireExternalExecutionEvent struct {
	Base
	ReplyID   string                  `json:"reply_id"`
	ToolCalls []message.ToolCallBlock `json:"tool_calls"`
}

func (e RequireExternalExecutionEvent) GetEventType() EventType {
	return EventRequireExternalExecution
}
func (e RequireExternalExecutionEvent) GetEventID() string { return e.ID }
func (e RequireExternalExecutionEvent) GetReplyID() string { return e.ReplyID }

func NewRequireExternalExecutionEvent(replyID string, toolCalls []message.ToolCallBlock) RequireExternalExecutionEvent {
	return RequireExternalExecutionEvent{Base: newBase(), ReplyID: replyID, ToolCalls: toolCalls}
}

type ExternalExecutionResultEvent struct {
	Base
	ReplyID          string                    `json:"reply_id"`
	ExecutionResults []message.ToolResultBlock `json:"execution_results"`
}

func (e ExternalExecutionResultEvent) GetEventType() EventType { return EventExternalExecutionResult }
func (e ExternalExecutionResultEvent) GetEventID() string      { return e.ID }
func (e ExternalExecutionResultEvent) GetReplyID() string      { return e.ReplyID }

func NewExternalExecutionResultEvent(replyID string, results []message.ToolResultBlock) ExternalExecutionResultEvent {
	return ExternalExecutionResultEvent{Base: newBase(), ReplyID: replyID, ExecutionResults: results}
}

// --- Control ---

type ExceedMaxItersEvent struct {
	Base
	ReplyID string `json:"reply_id"`
	Name    string `json:"name"`
}

func (e ExceedMaxItersEvent) GetEventType() EventType { return EventExceedMaxIters }
func (e ExceedMaxItersEvent) GetEventID() string      { return e.ID }
func (e ExceedMaxItersEvent) GetReplyID() string      { return e.ReplyID }

func NewExceedMaxItersEvent(replyID, name string) ExceedMaxItersEvent {
	return ExceedMaxItersEvent{Base: newBase(), ReplyID: replyID, Name: name}
}

type CustomEvent struct {
	Base
	ReplyID string         `json:"reply_id,omitempty"`
	Name    string         `json:"name"`
	Value   map[string]any `json:"value"`
}

func (e CustomEvent) GetEventType() EventType { return EventCustom }
func (e CustomEvent) GetEventID() string      { return e.ID }
func (e CustomEvent) GetReplyID() string      { return e.ReplyID }

func NewCustomEvent(replyID, name string, value map[string]any) CustomEvent {
	return CustomEvent{Base: newBase(), ReplyID: replyID, Name: name, Value: value}
}

// --- Tool execution lifecycle ---
//
// These events give visibility into the orchestrator/sandbox execution layer
// which sits below the agent loop's tool_call/tool_result events. They record
// what actually happened at the execution level: permission decisions, sandbox
// policy enforcement, command execution, and resource usage.

// ToolExecStartEvent is emitted when the orchestrator begins executing a tool.
type ToolExecStartEvent struct {
	Base
	ReplyID    string `json:"reply_id"`
	SessionID  string `json:"session_id,omitempty"`
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Input      string `json:"input,omitempty"`   // JSON input (may be redacted)
	Backend    string `json:"backend,omitempty"` // "local", "docker", "acs", ...
}

func (e ToolExecStartEvent) GetEventType() EventType { return EventToolExecStart }
func (e ToolExecStartEvent) GetEventID() string      { return e.ID }
func (e ToolExecStartEvent) GetReplyID() string      { return e.ReplyID }

// NewToolExecStartEvent creates a ToolExecStartEvent.
func NewToolExecStartEvent(replyID, sessionID, toolCallID, toolName, input, backend string) ToolExecStartEvent {
	return ToolExecStartEvent{
		Base: newBase(), ReplyID: replyID, SessionID: sessionID,
		ToolCallID: toolCallID, ToolName: toolName, Input: input, Backend: backend,
	}
}

// ToolExecEndEvent is emitted when the orchestrator finishes executing a tool.
type ToolExecEndEvent struct {
	Base
	ReplyID    string                  `json:"reply_id"`
	SessionID  string                  `json:"session_id,omitempty"`
	ToolCallID string                  `json:"tool_call_id"`
	ToolName   string                  `json:"tool_name"`
	State      message.ToolResultState `json:"state"`
	DurationMs int64                   `json:"duration_ms"`
	ExitCode   int                     `json:"exit_code,omitempty"`
	Error      string                  `json:"error,omitempty"`
}

func (e ToolExecEndEvent) GetEventType() EventType { return EventToolExecEnd }
func (e ToolExecEndEvent) GetEventID() string      { return e.ID }
func (e ToolExecEndEvent) GetReplyID() string      { return e.ReplyID }

// NewToolExecEndEvent creates a ToolExecEndEvent.
func NewToolExecEndEvent(replyID, sessionID, toolCallID, toolName string, state message.ToolResultState, durationMs int64) ToolExecEndEvent {
	return ToolExecEndEvent{
		Base: newBase(), ReplyID: replyID, SessionID: sessionID,
		ToolCallID: toolCallID, ToolName: toolName, State: state, DurationMs: durationMs,
	}
}

// ToolPolicyDeniedEvent is emitted when a tool call is blocked by sandbox policy.
type ToolPolicyDeniedEvent struct {
	Base
	ReplyID    string `json:"reply_id"`
	SessionID  string `json:"session_id,omitempty"`
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Reason     string `json:"reason"`
	Policy     string `json:"policy,omitempty"` // which policy sub-type triggered the denial
}

func (e ToolPolicyDeniedEvent) GetEventType() EventType { return EventToolPolicyDenied }
func (e ToolPolicyDeniedEvent) GetEventID() string      { return e.ID }
func (e ToolPolicyDeniedEvent) GetReplyID() string      { return e.ReplyID }

// NewToolPolicyDeniedEvent creates a ToolPolicyDeniedEvent.
func NewToolPolicyDeniedEvent(replyID, sessionID, toolCallID, toolName, reason, policy string) ToolPolicyDeniedEvent {
	return ToolPolicyDeniedEvent{
		Base: newBase(), ReplyID: replyID, SessionID: sessionID,
		ToolCallID: toolCallID, ToolName: toolName, Reason: reason, Policy: policy,
	}
}
