package participant

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func countingFetch(ran *atomic.Int32) tool.Tool {
	return tool.NewFunctionTool("web_fetch", "test tool web_fetch", objectSchema, func(context.Context, map[string]any) (any, error) {
		ran.Add(1)
		return "page", nil
	})
}

func confirmed(replyID string, calls ...message.ToolCallBlock) *event.UserConfirmResultEvent {
	results := make([]event.ConfirmResult, len(calls))
	for i, c := range calls {
		results[i] = event.ConfirmResult{Confirmed: true, ToolCall: c}
	}
	return &event.UserConfirmResultEvent{ReplyID: replyID, ConfirmResults: results}
}

// TestSubmitUserConfirmRefusesAnswersToOtherRequests: an activation is
// interrupted while it waits for an approval, and the next one asks its own.
// An answer to the interrupted activation's request, to a call the pending
// request does not ask about, or to no call fails and leaves the request
// pending, so its own answer still gets through. Before, each of them was
// counted as the answer and the agent dropped it, so the real answer failed
// with "no confirmation is pending" and the activation waited forever.
func TestSubmitUserConfirmRefusesAnswersToOtherRequests(t *testing.T) {
	var ran atomic.Int32
	f := newFixture(t, fixtureOptions{
		mode:   permission.ModeDefault,
		noPrep: true,
		tools:  []tool.Tool{countingFetch(&ran)},
		respond: steps(
			resp("", call("c0", "web_fetch", map[string]any{"url": "a"})),
			resp("", call("c1", "web_fetch", map[string]any{"url": "b"})),
			textResp("done"),
		),
	})

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ch1, err := f.p.Activate(ctx1, foreground(1, "first"))
	if err != nil {
		t.Fatal(err)
	}
	var old event.RequireUserConfirmEvent
	_, end1 := drain(t, ch1, func(it thread.ActivationEvent) {
		if req, ok := it.Event.(event.RequireUserConfirmEvent); ok {
			old = req
			cancel1()
		}
	})
	if end1.Reason != types.ReplyInterrupted || old.ReplyID == "" {
		t.Fatalf("first activation: %+v, request %+v", end1, old)
	}

	var errs []error
	_, end := activate(t, f.p, foreground(2, "second"), func(it thread.ActivationEvent) {
		req, ok := it.Event.(event.RequireUserConfirmEvent)
		if !ok {
			return
		}
		other := message.ToolCallBlock{Type: "tool_call", ID: "c-other", Name: "web_fetch", Input: `{}`}
		for _, wrong := range []*event.UserConfirmResultEvent{
			confirmed(old.ReplyID, old.ToolCalls[0]), // the interrupted activation's request
			confirmed(req.ReplyID, other),            // a call this request does not ask about
			confirmed(req.ReplyID),                   // no call
		} {
			errs = append(errs, f.p.SubmitUserConfirm(it.Path, wrong))
		}
		if err := f.p.SubmitUserConfirm(it.Path, confirmed(req.ReplyID, req.ToolCalls[0])); err != nil {
			t.Errorf("the request's own answer: %v", err)
		}
	})
	if len(errs) != 3 {
		t.Fatalf("the second activation asked %d times, want once", len(errs)/3)
	}
	for i, err := range errs {
		if !errors.Is(err, errConfirmMatchesNone) {
			t.Errorf("wrong answer %d: err = %v, want errConfirmMatchesNone", i, err)
		}
	}
	if end.Reason != types.ReplyCompleted || ran.Load() != 1 {
		t.Fatalf("end %+v after %d runs, want a completed reply that ran the call once", end, ran.Load())
	}
}

// TestSubmitUserConfirmAcceptsAnAnswerWithoutReplyID: an answer that names
// no reply still answers the request by its tool call, as the agent matches
// it.
func TestSubmitUserConfirmAcceptsAnAnswerWithoutReplyID(t *testing.T) {
	var ran atomic.Int32
	f := newFixture(t, fixtureOptions{
		mode:   permission.ModeDefault,
		noPrep: true,
		tools:  []tool.Tool{countingFetch(&ran)},
		respond: steps(
			resp("", call("c1", "web_fetch", map[string]any{"url": "b"})),
			textResp("done"),
		),
	})
	_, end := activate(t, f.p, foreground(1, "fetch"), func(it thread.ActivationEvent) {
		if req, ok := it.Event.(event.RequireUserConfirmEvent); ok {
			if err := f.p.SubmitUserConfirm(it.Path, confirmed("", req.ToolCalls[0])); err != nil {
				t.Errorf("an answer without a reply ID: %v", err)
			}
		}
	})
	if end.Reason != types.ReplyCompleted || ran.Load() != 1 {
		t.Fatalf("end %+v after %d runs, want a completed reply that ran the call once", end, ran.Load())
	}
}
