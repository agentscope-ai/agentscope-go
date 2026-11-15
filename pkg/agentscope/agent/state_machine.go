package agent

import (
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// getLastAssistantMsg returns the last message in context if it belongs to
// this agent and has role "assistant". Otherwise returns nil.
func (a *UnifiedAgent) getLastAssistantMsg() *message.Msg {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Context) == 0 {
		return nil
	}
	last := a.state.Context[len(a.state.Context)-1]
	if last.Role == message.RoleAssistant && last.Name == a.name {
		return last
	}
	return nil
}

// checkNextAction inspects the last assistant message's tool call states to
// determine the next loop state:
//   - StateAct: there are pending/allowed tool calls to execute
//   - StateWait: there are awaiting (asking/submitted) calls but nothing executable
//   - StateReason: no unfinished tool calls — ready for next model call
func (a *UnifiedAgent) checkNextAction() (protocol.LoopState, *message.Msg) {
	lastMsg := a.getLastAssistantMsg()
	if lastMsg == nil {
		// Resume scenario (HARNESS_DESIGN F1): a fresh user input follows a
		// restored assistant message that still has asking/submitted calls.
		// Detect the pending handshake so the loop re-drives it instead of
		// reasoning over an unfinished conversation.
		if askCount, subCount := a.countAwaitingInContext(); askCount+subCount > 0 {
			return protocol.StateWait, waitExitMsg(a.name, askCount, subCount)
		}
		return protocol.StateReason, nil
	}

	finishedIDs := make(map[string]bool)
	for _, b := range lastMsg.GetContentBlocks(message.ContentBlockToolResult) {
		if tr, ok := b.(message.ToolResultBlock); ok {
			finishedIDs[tr.ID] = true
		}
	}

	var executable, awaiting []message.ToolCallBlock
	for _, b := range lastMsg.GetContentBlocks(message.ContentBlockToolCall) {
		tc, ok := b.(message.ToolCallBlock)
		if !ok || finishedIDs[tc.ID] {
			continue
		}
		switch tc.State {
		case message.ToolCallPending, message.ToolCallAllowed:
			executable = append(executable, tc)
		case message.ToolCallAsking, message.ToolCallSubmitted:
			awaiting = append(awaiting, tc)
		}
	}

	if len(executable) > 0 {
		return protocol.StateAct, nil
	}

	if len(awaiting) > 0 {
		askCount, subCount := 0, 0
		for _, tc := range awaiting {
			if tc.State == message.ToolCallAsking {
				askCount++
			} else {
				subCount++
			}
		}
		return protocol.StateWait, waitExitMsg(a.name, askCount, subCount)
	}

	return protocol.StateReason, nil
}

// getExecutableToolCalls returns tool calls from the last assistant message
// that are ready to execute (state pending or allowed, no result yet).
func (a *UnifiedAgent) getExecutableToolCalls() []message.ToolCallBlock {
	lastMsg := a.getLastAssistantMsg()
	if lastMsg == nil {
		return nil
	}

	resultIDs := make(map[string]bool)
	for _, b := range lastMsg.GetContentBlocks(message.ContentBlockToolResult) {
		if tr, ok := b.(message.ToolResultBlock); ok {
			resultIDs[tr.ID] = true
		}
	}

	var calls []message.ToolCallBlock
	for _, b := range lastMsg.GetContentBlocks(message.ContentBlockToolCall) {
		tc, ok := b.(message.ToolCallBlock)
		if !ok || resultIDs[tc.ID] {
			continue
		}
		if tc.State == message.ToolCallPending || tc.State == message.ToolCallAllowed {
			calls = append(calls, tc)
		}
	}
	return calls
}

// updateToolCallState mutates the State field of a ToolCallBlock in the last
// assistant message's content. This is an in-place mutation matching Python's
// behavior — the context holds the authoritative state.
func (a *UnifiedAgent) updateToolCallState(callID string, newState message.ToolCallState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Scan backwards: after a resume the call may live in an assistant
	// message that is no longer the last context message (a fresh user
	// input follows it). Call IDs are unique per reply.
	for i := len(a.state.Context) - 1; i >= 0; i-- {
		msg := a.state.Context[i]
		if msg == nil {
			continue
		}
		for j, b := range msg.Content {
			if tc, ok := b.(message.ToolCallBlock); ok && tc.ID == callID {
				tc.State = newState
				msg.Content[j] = tc
				return
			}
		}
	}
}

// saveToContext merges content blocks into the last assistant message in
// context (if it belongs to this agent), or creates a new one. Audio
// DataBlocks are filtered out. Usage is accumulated across calls.
func (a *UnifiedAgent) saveToContext(blocks []message.ContentBlock, usage *model.ChatUsage) {
	persisted := filterAudioBlocks(blocks)
	if len(persisted) == 0 && usage == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.state.Context) > 0 {
		last := a.state.Context[len(a.state.Context)-1]
		if last.Role == message.RoleAssistant && last.Name == a.name {
			last.Content = append(last.Content, persisted...)
			if usage != nil {
				if last.Usage == nil {
					last.Usage = &message.Usage{
						InputTokens:  usage.InputTokens,
						OutputTokens: usage.OutputTokens,
					}
				} else {
					last.Usage.InputTokens += usage.InputTokens
					last.Usage.OutputTokens += usage.OutputTokens
				}
			}
			return
		}
	}

	msg := message.AssistantMsg(a.name, persisted)
	msg.ID = a.state.ReplyID // tie message ID to reply session
	if usage != nil {
		msg.Usage = &message.Usage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		}
	}
	a.state.Context = append(a.state.Context, msg)
}

// waitExitMsg builds the "waiting for ..." assistant message shown when the
// loop parks on unfinished handshakes.
func waitExitMsg(agentName string, askCount, subCount int) *message.Msg {
	var parts []string
	if askCount > 0 {
		parts = append(parts, "user confirmation")
	}
	if subCount > 0 {
		parts = append(parts, "external execution results")
	}
	text := "Waiting for " + strings.Join(parts, " and ") + "."
	msg := message.AssistantMsg(agentName, text)
	return msg
}

// countAwaitingInContext scans backwards for an assistant message that still
// has unfinished asking/submitted tool calls (resume detection). Finished
// calls (with results or finished state) never count.
func (a *UnifiedAgent) countAwaitingInContext() (ask, sub int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.state.Context) - 1; i >= 0; i-- {
		msg := a.state.Context[i]
		if msg == nil || msg.Role != message.RoleAssistant || msg.Name != a.name {
			continue
		}
		finished := map[string]bool{}
		for _, b := range msg.GetContentBlocks(message.ContentBlockToolResult) {
			if tr, ok := b.(message.ToolResultBlock); ok {
				finished[tr.ID] = true
			}
		}
		for _, b := range msg.GetContentBlocks(message.ContentBlockToolCall) {
			tc, ok := b.(message.ToolCallBlock)
			if !ok || finished[tc.ID] {
				continue
			}
			switch tc.State {
			case message.ToolCallAsking:
				ask++
			case message.ToolCallSubmitted:
				sub++
			}
		}
		if ask+sub > 0 {
			return ask, sub
		}
		return 0, 0 // most recent assistant message has no awaiting calls
	}
	return 0, 0
}
