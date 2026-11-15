package service

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// AG-UI protocol event types.
// These map internal AgentScope events to the AG-UI standard format
// so that AG-UI compatible frontends can consume the SSE stream.
const (
	AGUIRunStarted     = "RUN_STARTED"
	AGUIRunFinished    = "RUN_FINISHED"
	AGUIRunError       = "RUN_ERROR"
	AGUIStepStarted    = "STEP_STARTED"
	AGUIStepFinished   = "STEP_FINISHED"
	AGUITextMsgStart   = "TEXT_MESSAGE_START"
	AGUITextMsgContent = "TEXT_MESSAGE_CONTENT"
	AGUITextMsgEnd     = "TEXT_MESSAGE_END"
	AGUIToolCallStart  = "TOOL_CALL_START"
	AGUIToolCallArgs   = "TOOL_CALL_ARGS"
	AGUIToolCallEnd    = "TOOL_CALL_END"
	AGUIToolCallResult = "TOOL_CALL_RESULT"
	AGUICustomEvent    = "CUSTOM"
)

// AGUIEvent is the AG-UI protocol event envelope.
type AGUIEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`

	// Run events
	RunID string `json:"run_id,omitempty"`

	// Text message events
	MessageID string `json:"message_id,omitempty"`
	Delta     string `json:"delta,omitempty"`
	Role      string `json:"role,omitempty"`

	// Tool call events
	ToolCallID   string `json:"tool_call_id,omitempty"`
	ToolCallName string `json:"tool_call_name,omitempty"`
	Args         string `json:"args,omitempty"`
	Result       string `json:"result,omitempty"`

	// Step events
	StepID string `json:"step_id,omitempty"`

	// Error
	Error string `json:"error,omitempty"`

	// Custom
	Name  string `json:"name,omitempty"`
	Value any    `json:"value,omitempty"`
}

// ConvertToAGUI converts an internal AgentScope event to the AG-UI format.
// Returns nil for events with no AG-UI mapping.
func ConvertToAGUI(evt event.Event) *AGUIEvent {
	switch e := evt.(type) {
	case event.ReplyStartEvent:
		return &AGUIEvent{
			Type:    AGUIRunStarted,
			EventID: e.GetEventID(),
			RunID:   e.ReplyID,
		}
	case event.ReplyEndEvent:
		return &AGUIEvent{
			Type:    AGUIRunFinished,
			EventID: e.GetEventID(),
			RunID:   e.ReplyID,
		}
	case event.ExceedMaxItersEvent:
		return &AGUIEvent{
			Type:    AGUIRunError,
			EventID: e.GetEventID(),
			RunID:   e.ReplyID,
			Error:   "exceeded maximum iterations",
		}
	case event.ModelCallStartEvent:
		return &AGUIEvent{
			Type:    AGUIStepStarted,
			EventID: e.GetEventID(),
			StepID:  e.GetEventID(),
		}
	case event.ModelCallEndEvent:
		return &AGUIEvent{
			Type:    AGUIStepFinished,
			EventID: e.GetEventID(),
			StepID:  e.GetEventID(),
		}
	case event.TextBlockStartEvent:
		return &AGUIEvent{
			Type:      AGUITextMsgStart,
			EventID:   e.GetEventID(),
			MessageID: e.GetEventID(),
			Role:      "assistant",
		}
	case event.TextBlockDeltaEvent:
		return &AGUIEvent{
			Type:      AGUITextMsgContent,
			EventID:   e.GetEventID(),
			MessageID: e.GetReplyID(),
			Delta:     e.Delta,
		}
	case event.TextBlockEndEvent:
		return &AGUIEvent{
			Type:      AGUITextMsgEnd,
			EventID:   e.GetEventID(),
			MessageID: e.GetReplyID(),
		}
	case event.ToolCallStartEvent:
		return &AGUIEvent{
			Type:         AGUIToolCallStart,
			EventID:      e.GetEventID(),
			ToolCallID:   e.ToolCallID,
			ToolCallName: e.ToolCallName,
		}
	case event.ToolCallDeltaEvent:
		return &AGUIEvent{
			Type:       AGUIToolCallArgs,
			EventID:    e.GetEventID(),
			ToolCallID: e.ToolCallID,
			Args:       e.Delta,
		}
	case event.ToolCallEndEvent:
		return &AGUIEvent{
			Type:       AGUIToolCallEnd,
			EventID:    e.GetEventID(),
			ToolCallID: e.ToolCallID,
		}
	case event.ToolResultEndEvent:
		return &AGUIEvent{
			Type:       AGUIToolCallResult,
			EventID:    e.GetEventID(),
			ToolCallID: e.ToolCallID,
			Result:     string(e.State),
		}
	default:
		return &AGUIEvent{
			Type:    AGUICustomEvent,
			EventID: evt.GetEventID(),
			Name:    string(evt.GetEventType()),
			Value:   evt,
		}
	}
}
