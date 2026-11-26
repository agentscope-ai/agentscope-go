package participant

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// TestActivationInterruptsRestoredConfirmation covers #207 on a participant:
// its agent is restored with a call still asking for confirmation, as a host
// restores it after a restart, and the next activation brings new input. The
// activation closes the call as interrupted instead of asking again: the call
// does not run, the activation does not count it, and the model sees its
// result, saying it did not run, right after the call and before the input.
func TestActivationInterruptsRestoredConfirmation(t *testing.T) {
	var ran atomic.Int32
	fetch := tool.NewFunctionTool("web_fetch", "test tool web_fetch", objectSchema, func(context.Context, map[string]any) (any, error) {
		ran.Add(1)
		return "page", nil
	})
	restored := &agent.AgentState{SessionID: "cloud-session", Context: []*message.Msg{
		message.UserMsg("user", "q1"),
		message.AssistantMsg(cloudDesc.Name, []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "c0", Name: "web_fetch", Input: `{"url":"u"}`, State: message.ToolCallAsking},
		}),
	}}
	f := newFixture(t, fixtureOptions{
		mode:   permission.ModeDefault,
		noPrep: true,
		tools:  []tool.Tool{fetch},
		agent:  []agent.AgentOption{agent.WithState(restored)},
		respond: func(context.Context, int, *model.CallOptions) (*model.ChatResponse, error) {
			return textResp("done"), nil
		},
	})

	asks := 0
	_, end := activate(t, f.p, foreground(1, "Q2-INPUT"), confirmAll(t, f.p, &asks))
	if end.Reason != types.ReplyCompleted || asks != 0 || ran.Load() != 0 {
		t.Fatalf("end %+v after %d confirmations and %d runs, want a finished reply after none", end, asks, ran.Load())
	}
	if f.m.calls() != 1 {
		t.Fatalf("model calls = %d, want 1", f.m.calls())
	}
	// The closed call did not run in this activation.
	if end.ToolCalls != 0 || end.Digest != nil {
		t.Fatalf("the activation counts the closed call: ToolCalls=%d, Digest=%+v", end.ToolCalls, end.Digest)
	}
	sent := f.m.input(0)
	call := strings.Index(sent, `call web_fetch {"url":"u"}`)
	result := strings.Index(sent, "result web_fetch: Tool call did not run")
	input := strings.Index(sent, "Q2-INPUT")
	if call < 0 || result < call || input < result {
		t.Fatalf("want the call, its result and then the input:\n%s", sent)
	}
}

// TestRestoredConfirmationAnswerIsRefused: after a restart, the request the
// host showed before it is void. An answer the host still submits for it is
// refused while the activation waits for its own request, and the activation
// goes on with that request's answer. The restored call does not run.
func TestRestoredConfirmationAnswerIsRefused(t *testing.T) {
	var ran atomic.Int32
	old := message.ToolCallBlock{Type: "tool_call", ID: "c0", Name: "web_fetch", Input: `{"url":"u"}`, State: message.ToolCallAsking}
	restored := &agent.AgentState{SessionID: "cloud-session", ReplyID: "reply-before-restart", Context: []*message.Msg{
		message.UserMsg("user", "q1"),
		message.AssistantMsg(cloudDesc.Name, []message.ContentBlock{old}),
	}}
	f := newFixture(t, fixtureOptions{
		mode:   permission.ModeDefault,
		noPrep: true,
		tools:  []tool.Tool{countingFetch(&ran)},
		agent:  []agent.AgentOption{agent.WithState(restored)},
		respond: steps(
			resp("", call("c1", "web_fetch", map[string]any{"url": "b"})),
			textResp("done"),
		),
	})

	var stale []error
	_, end := activate(t, f.p, foreground(1, "Q2-INPUT"), func(it thread.ActivationEvent) {
		req, ok := it.Event.(event.RequireUserConfirmEvent)
		if !ok {
			return
		}
		stale = append(stale, f.p.SubmitUserConfirm(it.Path, confirmed("reply-before-restart", old)))
		if err := f.p.SubmitUserConfirm(it.Path, confirmed(req.ReplyID, req.ToolCalls[0])); err != nil {
			t.Errorf("the request's own answer: %v", err)
		}
	})
	if len(stale) != 1 || !errors.Is(stale[0], errConfirmMatchesNone) {
		t.Fatalf("the void request's answer: %v, want one errConfirmMatchesNone", stale)
	}
	if end.Reason != types.ReplyCompleted || ran.Load() != 1 {
		t.Fatalf("end %+v after %d runs, want a completed reply that ran only the new call", end, ran.Load())
	}
}
