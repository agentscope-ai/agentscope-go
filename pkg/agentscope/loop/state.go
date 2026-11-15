// pkg/agentscope/loop/state.go
package loop

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// validTransitions defines which state transitions are allowed.
var validTransitions = map[protocol.LoopState]map[protocol.LoopState]bool{
	protocol.StateReason: {
		protocol.StateInspect: true,
	},
	protocol.StateInspect: {
		protocol.StateAct:    true,
		protocol.StateWait:   true,
		protocol.StateExit:   true,
		protocol.StateReason: true,
	},
	protocol.StateAct: {
		protocol.StateReason: true,
		protocol.StateExit:   true,
	},
	protocol.StateWait: {
		protocol.StateReason: true,
	},
	protocol.StateExit: {},
}

// IsValidTransition checks if transitioning from one state to another is allowed.
func IsValidTransition(from, to protocol.LoopState) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}
	return targets[to]
}

// InspectResult is the outcome of inspecting a model response.
type InspectResult int

const (
	InspectNoTools   InspectResult = iota // No tool calls — ready to exit or reason
	InspectHasTools                       // Has executable tool calls
	InspectNeedsHITL                      // Has tool calls awaiting user confirmation
)

// InspectResponse examines content blocks to determine what action to take.
func InspectResponse(content []message.ContentBlock) InspectResult {
	hasExecutable := false
	hasAwaiting := false

	for _, b := range content {
		tc, ok := b.(message.ToolCallBlock)
		if !ok {
			continue
		}
		switch tc.State {
		case message.ToolCallPending, message.ToolCallAllowed:
			hasExecutable = true
		case message.ToolCallAsking, message.ToolCallSubmitted:
			hasAwaiting = true
		}
	}

	if hasExecutable {
		return InspectHasTools
	}
	if hasAwaiting {
		return InspectNeedsHITL
	}
	return InspectNoTools
}
