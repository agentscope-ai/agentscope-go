package event

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"

// GetEventType returns the event type as a string, satisfying message.AppendableEvent.
// Each concrete event already has GetEventType() EventType — these wrappers
// are needed because message.AppendableEvent expects string return.

// All events satisfy message.AppendableEvent via their GetEventType() and GetReplyID().
// Since EventType is a string type, we just need the accessors for the
// data interfaces used by Msg.AppendEvent.

// --- BlockID accessors ---

func (e TextBlockStartEvent) GetBlockID() string      { return e.BlockID }
func (e TextBlockDeltaEvent) GetBlockID() string      { return e.BlockID }
func (e TextBlockEndEvent) GetBlockID() string        { return e.BlockID }
func (e ThinkingBlockStartEvent) GetBlockID() string  { return e.BlockID }
func (e ThinkingBlockDeltaEvent) GetBlockID() string  { return e.BlockID }
func (e ThinkingBlockEndEvent) GetBlockID() string    { return e.BlockID }
func (e DataBlockStartEvent) GetBlockID() string      { return e.BlockID }
func (e DataBlockDeltaEvent) GetBlockID() string      { return e.BlockID }
func (e DataBlockEndEvent) GetBlockID() string        { return e.BlockID }
func (e ToolResultDataDeltaEvent) GetBlockID() string { return e.BlockID }
func (e HintBlockEvent) GetBlockID() string           { return e.BlockID }

// --- Delta accessors ---

func (e TextBlockDeltaEvent) GetDelta() string      { return e.Delta }
func (e ThinkingBlockDeltaEvent) GetDelta() string  { return e.Delta }
func (e ToolCallDeltaEvent) GetDelta() string       { return e.Delta }
func (e ToolResultTextDeltaEvent) GetDelta() string { return e.Delta }

// --- Token accessors ---

func (e ModelCallEndEvent) GetInputTokens() int  { return e.InputTokens }
func (e ModelCallEndEvent) GetOutputTokens() int { return e.OutputTokens }

// --- ToolCallID accessors ---

func (e ToolCallStartEvent) GetToolCallID() string       { return e.ToolCallID }
func (e ToolCallDeltaEvent) GetToolCallID() string       { return e.ToolCallID }
func (e ToolCallEndEvent) GetToolCallID() string         { return e.ToolCallID }
func (e ToolResultStartEvent) GetToolCallID() string     { return e.ToolCallID }
func (e ToolResultTextDeltaEvent) GetToolCallID() string { return e.ToolCallID }
func (e ToolResultDataDeltaEvent) GetToolCallID() string { return e.ToolCallID }
func (e ToolResultEndEvent) GetToolCallID() string       { return e.ToolCallID }

// --- ToolCallName accessors ---

func (e ToolCallStartEvent) GetToolCallName() string   { return e.ToolCallName }
func (e ToolResultStartEvent) GetToolCallName() string { return e.ToolCallName }

// --- ToolResultState accessor ---

func (e ToolResultEndEvent) GetState() message.ToolResultState { return e.State }

// --- Data accessor ---

func (e DataBlockDeltaEvent) GetData() string      { return e.Data }
func (e ToolResultDataDeltaEvent) GetData() string { return e.Data }
func (e ToolResultDataDeltaEvent) GetURL() string  { return e.URL }

// --- MediaType accessor ---

func (e DataBlockStartEvent) GetMediaType_() string      { return e.MediaType }
func (e DataBlockDeltaEvent) GetMediaType_() string      { return e.MediaType }
func (e ToolResultDataDeltaEvent) GetMediaType_() string { return e.MediaType }

// --- Hint accessors ---

func (e HintBlockEvent) GetHint() string {
	if s, ok := e.Hint.(string); ok {
		return s
	}
	return ""
}
func (e HintBlockEvent) GetSource() string { return e.Source }
