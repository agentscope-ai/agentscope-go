package event

// EventTypeString methods for all event types.
// These return the event type as a plain string, enabling
// message.Msg.AppendEvent to work without circular imports.

func (e ReplyStartEvent) EventTypeString() string               { return string(e.GetEventType()) }
func (e ReplyEndEvent) EventTypeString() string                 { return string(e.GetEventType()) }
func (e ModelCallStartEvent) EventTypeString() string           { return string(e.GetEventType()) }
func (e ModelCallEndEvent) EventTypeString() string             { return string(e.GetEventType()) }
func (e TextBlockStartEvent) EventTypeString() string           { return string(e.GetEventType()) }
func (e TextBlockDeltaEvent) EventTypeString() string           { return string(e.GetEventType()) }
func (e TextBlockEndEvent) EventTypeString() string             { return string(e.GetEventType()) }
func (e ThinkingBlockStartEvent) EventTypeString() string       { return string(e.GetEventType()) }
func (e ThinkingBlockDeltaEvent) EventTypeString() string       { return string(e.GetEventType()) }
func (e ThinkingBlockEndEvent) EventTypeString() string         { return string(e.GetEventType()) }
func (e DataBlockStartEvent) EventTypeString() string           { return string(e.GetEventType()) }
func (e DataBlockDeltaEvent) EventTypeString() string           { return string(e.GetEventType()) }
func (e DataBlockEndEvent) EventTypeString() string             { return string(e.GetEventType()) }
func (e ToolCallStartEvent) EventTypeString() string            { return string(e.GetEventType()) }
func (e ToolCallDeltaEvent) EventTypeString() string            { return string(e.GetEventType()) }
func (e ToolCallEndEvent) EventTypeString() string              { return string(e.GetEventType()) }
func (e ToolResultStartEvent) EventTypeString() string          { return string(e.GetEventType()) }
func (e ToolResultTextDeltaEvent) EventTypeString() string      { return string(e.GetEventType()) }
func (e ToolResultDataDeltaEvent) EventTypeString() string      { return string(e.GetEventType()) }
func (e ToolResultEndEvent) EventTypeString() string            { return string(e.GetEventType()) }
func (e HintBlockEvent) EventTypeString() string                { return string(e.GetEventType()) }
func (e RequireUserConfirmEvent) EventTypeString() string       { return string(e.GetEventType()) }
func (e UserConfirmResultEvent) EventTypeString() string        { return string(e.GetEventType()) }
func (e RequireExternalExecutionEvent) EventTypeString() string { return string(e.GetEventType()) }
func (e ExternalExecutionResultEvent) EventTypeString() string  { return string(e.GetEventType()) }
func (e ToolExecStartEvent) EventTypeString() string            { return string(e.GetEventType()) }
func (e ToolExecEndEvent) EventTypeString() string              { return string(e.GetEventType()) }
func (e ToolPolicyDeniedEvent) EventTypeString() string         { return string(e.GetEventType()) }
func (e ExceedMaxItersEvent) EventTypeString() string           { return string(e.GetEventType()) }
func (e CustomEvent) EventTypeString() string                   { return string(e.GetEventType()) }
