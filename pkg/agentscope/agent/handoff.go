package agent

import (
	"context"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// MarkHandedOff (EXPERIMENTAL) records that the foreground moved to another
// participant. A tool callback calls it once its handoff is committed. When
// the current tool step finishes, the reply ends with
// types.ReplyHandedOff instead of making another model call. The end cannot
// be swallowed by OnReply middleware, Reply returns the reply message
// normally, and a Conversation records no interruption marker for it. The
// mark is cleared at the start of every reply, so a mark set outside a reply
// has no effect. The loop bridge (UnifiedAgentRunner) does not observe it.
//
// The mark is not checkpointed: a reply resumed by reply recovery after a
// crash between the handoff and the reply's end makes another model call.
// It is also one per agent: a host that starts a reply while a canceled
// one is still finishing must wait for that one first, or the two can clear
// or set each other's mark.
func (a *UnifiedAgent) MarkHandedOff() {
	a.handedOff.Store(true)
}

// WithExclusiveTools (EXPERIMENTAL) names tools that must be the only call in
// a model response. When a listed tool appears together with any other call,
// none of the calls runs: every call in that response is answered with an
// error result "<name> must be called alone; call it again by itself", which
// keeps each tool_call/tool_result pair intact and lets the model resend the
// exclusive call by itself. Fresh, restored (resumed) and bounded tool steps
// all honor it, as does the loop bridge.
//
// Call names are matched the way the toolkit resolves them (an exact name
// first, then a case-insensitive one), so "Transfer" counts as the exclusive
// tool "transfer" whenever the toolkit would run that tool for it. A name the
// toolkit cannot resolve is compared case-insensitively.
func WithExclusiveTools(names ...string) AgentOption {
	return func(a *UnifiedAgent) {
		if len(names) == 0 {
			return
		}
		if a.exclusiveTools == nil {
			a.exclusiveTools = make(map[string]bool, len(names))
		}
		for _, n := range names {
			a.exclusiveTools[n] = true
		}
	}
}

// exclusiveViolation returns the exclusive tool name to report when calls mix
// an exclusive tool with any other call, or "" when the step may run.
func (a *UnifiedAgent) exclusiveViolation(calls []message.ToolCallBlock) string {
	if len(a.exclusiveTools) == 0 || len(calls) < 2 {
		return ""
	}
	for i := range calls {
		if a.isExclusiveCall(calls[i].Name) {
			return calls[i].Name
		}
	}
	return ""
}

// isExclusiveCall reports whether a call named name would run an exclusive
// tool. The name is resolved through the toolkit, which falls back to a
// case-insensitive match, so a call cannot slip past the check by changing
// case. A name that resolves to no active tool is compared
// case-insensitively.
func (a *UnifiedAgent) isExclusiveCall(name string) bool {
	if len(a.exclusiveTools) == 0 {
		return false
	}
	if a.exclusiveTools[name] {
		return true
	}
	resolved := a.resolvedToolName(name)
	for e := range a.exclusiveTools {
		if resolved == "" {
			if strings.EqualFold(e, name) {
				return true
			}
			continue
		}
		if e == resolved || a.resolvedToolName(e) == resolved {
			return true
		}
	}
	return false
}

// resolvedToolName returns the name of the tool the toolkit runs for a call
// named name, or "" when no active tool matches.
func (a *UnifiedAgent) resolvedToolName(name string) string {
	if a.toolkit == nil {
		return ""
	}
	if t := a.toolkit.Get(name); t != nil {
		return t.Name()
	}
	return ""
}

// exclusiveRejection is the error result text for one call of a rejected step.
// An exclusive call names itself; the others name the first exclusive call.
func (a *UnifiedAgent) exclusiveRejection(call *message.ToolCallBlock, first string) string {
	name := first
	if a.isExclusiveCall(call.Name) {
		name = call.Name
	}
	return name + " must be called alone; call it again by itself"
}

// enforceExclusiveTools records error results for a step that violates
// WithExclusiveTools and returns no calls to execute; otherwise it returns
// calls unchanged. Nothing is executed or permission-checked for a rejected
// step.
func (a *UnifiedAgent) enforceExclusiveTools(ctx context.Context, ch chan<- event.Event, replyID string, calls []message.ToolCallBlock) []message.ToolCallBlock {
	first := a.exclusiveViolation(calls)
	if first == "" {
		return calls
	}
	results := make([]toolResult, len(calls))
	for i := range calls {
		results[i] = toolResult{index: i, state: message.ToolResultError, text: a.exclusiveRejection(&calls[i], first)}
	}
	a.recordConcurrentResults(ctx, ch, replyID, calls, results)
	return nil
}
