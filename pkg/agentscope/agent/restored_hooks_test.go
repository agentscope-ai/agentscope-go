package agent

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// orderHook logs the tool hooks and state transitions with the state and
// iteration each one saw. answerReply writes the handshake requests into the
// same log, so their order relative to the hooks is observable.
type orderHook struct {
	mu  sync.Mutex
	log []string
}

func (h *orderHook) add(entry string) {
	h.mu.Lock()
	h.log = append(h.log, entry)
	h.mu.Unlock()
}

func (h *orderHook) entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.log)
}

func (*orderHook) BeforeModelCall(protocol.LoopState, int)       {}
func (*orderHook) AfterModelCall(protocol.LoopState, int, error) {}
func (h *orderHook) BeforeToolExec(state protocol.LoopState, iter int, name string) {
	h.add(fmt.Sprintf("before:%s@%s#%d", name, state, iter))
}
func (h *orderHook) AfterToolExec(state protocol.LoopState, iter int, name string, _ error) {
	h.add(fmt.Sprintf("after:%s@%s#%d", name, state, iter))
}
func (h *orderHook) OnStateTransition(from, to protocol.LoopState, iter int) {
	h.add(fmt.Sprintf("transition:%s->%s#%d", from, to, iter))
}
func (*orderHook) OnLoopStart()    {}
func (*orderHook) OnLoopEnd(error) {}

// answerReply drains a reply. It logs every confirmation and external
// request before answering it: confirmations with confirm, external requests
// with a successful result. The reply is blocked on each answer, so nothing
// it does afterwards can reach the log before the request's entry.
func answerReply(a *UnifiedAgent, ch <-chan event.Event, hook *orderHook, confirm bool) {
	for evt := range ch {
		switch e := evt.(type) {
		case event.RequireUserConfirmEvent:
			answers := make([]event.ConfirmResult, 0, len(e.ToolCalls))
			for _, tc := range e.ToolCalls {
				hook.add(fmt.Sprintf("confirm_requested:%s confirmed=%v", tc.Name, confirm))
				answers = append(answers, event.ConfirmResult{Confirmed: confirm, ToolCall: tc})
			}
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: answers})
		case event.RequireExternalExecutionEvent:
			results := make([]message.ToolResultBlock, 0, len(e.ToolCalls))
			for _, tc := range e.ToolCalls {
				hook.add("external_requested:" + tc.Name)
				results = append(results, message.ToolResultBlock{
					Type: "tool_result", ID: tc.ID, Name: tc.Name, Output: "ext-ok", State: message.ToolResultSuccess,
				})
			}
			a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ExecutionResults: results})
		}
	}
}

// hookedAgent builds an agent whose loop hooks write to hook.
func hookedAgent(hook *orderHook, policy *permission.Context, responses []model.ChatResponse, opts ...AgentOption) *UnifiedAgent {
	base := []AgentOption{
		WithPermissionContext(policy),
		WithReactConfig(ReactConfig{MaxIters: 4}),
		WithStateSaver(newFakeStateSaver()),
		WithLoopHooks(hook),
	}
	return NewUnifiedAgent("a", "sys", &mockChatModel{responses: responses}, append(base, opts...)...)
}

func textResponse(text string) model.ChatResponse {
	return model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}, IsLast: true}
}

// parkedState is a checkpoint whose reply parked on calls, as WithState
// restores it. With recovery set it also holds the active recovery snapshot
// ResumeReplyStream needs.
func parkedState(recovery bool, calls ...message.ToolCallBlock) *AgentState {
	blocks := make([]message.ContentBlock, len(calls))
	for i := range calls {
		blocks[i] = calls[i]
	}
	parked := message.AssistantMsg("a", blocks)
	st := &AgentState{
		SchemaVersion: StateSchemaVersion,
		SessionID:     "sess-hooks",
		Context:       []*message.Msg{message.UserMsg("user", "how am I doing"), parked},
	}
	if recovery {
		parked.ID = "r"
		st.ReplyID = "r"
		st.ReplyRecovery = &ReplyRecoveryState{
			Version: 1, ReplyID: "r", Active: true,
			Budgets: middleware.ReplyBudgetSnapshot{Version: 1},
		}
	}
	return st
}

// askingEntry names how a call that needs confirmation reaches a reply.
type askingEntry string

const (
	askedFresh    askingEntry = "fresh"    // the model emits the call in this reply
	askedRestored askingEntry = "restored" // parked asking, restored with WithState, picked up by a new ReplyStream
	askedResumed  askingEntry = "resumed"  // parked asking, restored with WithState, continued by ResumeReplyStream
)

// runAskingCall runs one sensor call that needs confirmation through entry
// and answers the confirmation with confirm. It returns the hook log, how
// often the tool ran and the result recorded for the call.
func runAskingCall(t *testing.T, entry askingEntry, confirm bool) (log []string, executions int32, result message.ToolResultBlock) {
	t.Helper()
	var ran atomic.Int32
	sensor := tool.NewFunctionTool("sensor", "reads the heart rate", nil,
		func(context.Context, map[string]any) (any, error) {
			ran.Add(1)
			return "heart rate 142 bpm", nil
		})
	call := message.ToolCallBlock{Type: "tool_call", ID: "tc_ask", Name: "sensor", Input: `{}`, State: message.ToolCallAsking}
	responses := []model.ChatResponse{textResponse("done")}
	opts := []AgentOption{WithToolkit(tool.NewToolkit(sensor))}
	switch entry {
	case askedFresh:
		pending := call
		pending.State = message.ToolCallPending
		responses = append([]model.ChatResponse{{Content: []message.ContentBlock{pending}, IsLast: true}}, responses...)
	case askedRestored:
		opts = append(opts, WithState(parkedState(false, call)))
	case askedResumed:
		opts = append(opts, WithState(parkedState(true, call)), WithReplyRecovery())
	}
	hook := &orderHook{}
	a := hookedAgent(hook, permission.NewContext(permission.ModeDefault), responses, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ch <-chan event.Event
	var err error
	if entry == askedResumed {
		ch, err = a.ResumeReplyStream(ctx)
	} else {
		ch, err = a.ReplyStream(ctx, "continue")
	}
	if err != nil {
		t.Fatal(err)
	}
	answerReply(a, ch, hook, confirm)
	result, found := findToolResult(a, "tc_ask")
	if !found {
		t.Fatalf("%s: no result was recorded for the call", entry)
	}
	return hook.entries(), ran.Load(), result
}

// A resumed call confirmed again gets the loop hooks once, around the
// re-prompted confirmation and its execution. They see StateAct with the
// parked iteration although no transition to StateAct is reported.
func TestResumedConfirmedCallRunsTheLoopHooks(t *testing.T) {
	log, executions, result := runAskingCall(t, askedRestored, true)
	want := []string{
		"transition:reason->wait#0",
		"before:sensor@act#0",
		"confirm_requested:sensor confirmed=true",
		"after:sensor@act#0",
		"transition:wait->reason#1",
		"transition:reason->inspect#1",
		"transition:inspect->exit#1",
	}
	if !slices.Equal(log, want) {
		t.Fatalf("hook log:\n got  %q\n want %q", log, want)
	}
	if executions != 1 || result.State != message.ToolResultSuccess {
		t.Fatalf("confirmed call: executions=%d result=%+v", executions, result)
	}
}

// A resumed call the user denies again runs nothing, but its handling is
// bracketed by the loop hooks, as a denied call is on the ordinary path.
func TestResumedDeniedCallRunsTheLoopHooks(t *testing.T) {
	log, executions, result := runAskingCall(t, askedRestored, false)
	want := []string{
		"transition:reason->wait#0",
		"before:sensor@act#0",
		"confirm_requested:sensor confirmed=false",
		"after:sensor@act#0",
		"transition:wait->reason#1",
		"transition:reason->inspect#1",
		"transition:inspect->exit#1",
	}
	if !slices.Equal(log, want) {
		t.Fatalf("hook log:\n got  %q\n want %q", log, want)
	}
	if executions != 0 || result.State != message.ToolResultDenied {
		t.Fatalf("denied call: executions=%d result=%+v", executions, result)
	}
}

// However a call that needs confirmation reaches a reply (emitted by the
// model, parked and picked up by a new reply, or parked and continued by
// ResumeReplyStream), the tool hooks bracket the confirmation and the
// outcome in the same way.
func TestLoopHooksBracketAnAskingCallOnEveryEntry(t *testing.T) {
	iteration := regexp.MustCompile(`#\d+$`)
	for _, confirm := range []bool{true, false} {
		want := []string{
			"before:sensor@act",
			fmt.Sprintf("confirm_requested:sensor confirmed=%v", confirm),
			"after:sensor@act",
		}
		for _, entry := range []askingEntry{askedFresh, askedRestored, askedResumed} {
			log, _, _ := runAskingCall(t, entry, confirm)
			var got []string
			for _, e := range log {
				if !strings.HasPrefix(e, "transition:") {
					got = append(got, iteration.ReplaceAllString(e, ""))
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s, confirmed=%v: tool hooks and prompts:\n got  %q\n want %q", entry, confirm, got, want)
			}
		}
	}
}

// A submitted external call that ResumeReplyStream re-requests gets the loop
// hooks around the whole external handshake, as on the ordinary tool path.
func TestResumedExternalCallRunsTheLoopHooks(t *testing.T) {
	policy := permission.NewContext(permission.ModeDefault)
	policy.AllowRules["ext_tool"] = []permission.Rule{{ToolName: "ext_tool", Behavior: permission.BehaviorAllow}}
	call := message.ToolCallBlock{Type: "tool_call", ID: "tc_ext", Name: "ext_tool", Input: `{}`, State: message.ToolCallSubmitted}
	hook := &orderHook{}
	a := hookedAgent(hook, policy, []model.ChatResponse{textResponse("done")},
		WithToolkit(tool.NewToolkit(newHITLMockTool("ext_tool", true))),
		WithState(parkedState(true, call)), WithReplyRecovery())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := a.ResumeReplyStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answerReply(a, ch, hook, true)
	want := []string{
		"transition:reason->wait#0",
		"before:ext_tool@act#0",
		"external_requested:ext_tool",
		"after:ext_tool@act#0",
		"transition:wait->reason#1",
		"transition:reason->inspect#1",
		"transition:inspect->exit#1",
	}
	if got := hook.entries(); !slices.Equal(got, want) {
		t.Fatalf("hook log:\n got  %q\n want %q", got, want)
	}
	if result, found := findToolResult(a, "tc_ext"); !found || result.State != message.ToolResultSuccess {
		t.Fatalf("external call: found=%v result=%+v", found, result)
	}
}

// Several parked calls each get one pair of hooks: an asking local call, an
// asking external call (confirmed, then handed to the host) and a submitted
// external call the current policy asks about again.
func TestResumedCallsGetOnePairOfHooksEach(t *testing.T) {
	var ran atomic.Int32
	sensor := tool.NewFunctionTool("sensor", "reads the heart rate", nil,
		func(context.Context, map[string]any) (any, error) {
			ran.Add(1)
			return "heart rate 142 bpm", nil
		})
	hook := &orderHook{}
	a := hookedAgent(hook, permission.NewContext(permission.ModeDefault), []model.ChatResponse{textResponse("done")},
		WithToolkit(tool.NewToolkit(sensor, newHITLMockTool("ext_ask", true), newHITLMockTool("ext_sub", true))),
		WithState(parkedState(false,
			message.ToolCallBlock{Type: "tool_call", ID: "tc_1", Name: "sensor", Input: `{}`, State: message.ToolCallAsking},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_2", Name: "ext_ask", Input: `{}`, State: message.ToolCallAsking},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_3", Name: "ext_sub", Input: `{}`, State: message.ToolCallSubmitted},
		)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "continue")
	if err != nil {
		t.Fatal(err)
	}
	answerReply(a, ch, hook, true)
	want := []string{
		"transition:reason->wait#0",
		"before:sensor@act#0",
		"confirm_requested:sensor confirmed=true",
		"after:sensor@act#0",
		"before:ext_ask@act#0",
		"confirm_requested:ext_ask confirmed=true",
		"external_requested:ext_ask",
		"after:ext_ask@act#0",
		"before:ext_sub@act#0",
		"confirm_requested:ext_sub confirmed=true",
		"external_requested:ext_sub",
		"after:ext_sub@act#0",
		"transition:wait->reason#1",
		"transition:reason->inspect#1",
		"transition:inspect->exit#1",
	}
	if got := hook.entries(); !slices.Equal(got, want) {
		t.Fatalf("hook log:\n got  %q\n want %q", got, want)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("sensor ran %d times, want 1", n)
	}
	for _, id := range []string{"tc_1", "tc_2", "tc_3"} {
		if result, found := findToolResult(a, id); !found || result.State != message.ToolResultSuccess {
			t.Fatalf("%s: found=%v result=%+v", id, found, result)
		}
	}
}
