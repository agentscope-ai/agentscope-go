package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// handshake abstracts the two one-value channels so both submit variants run
// through the same scenarios.
type handshake struct {
	name   string
	submit func(a *UnifiedAgent, ctx context.Context, id string) error
	stale  func(a *UnifiedAgent, id string) // leaves a value nobody reads
	take   func(a *UnifiedAgent) (string, bool)
}

func handshakes() []handshake {
	return []handshake{
		{
			name: "confirm",
			submit: func(a *UnifiedAgent, ctx context.Context, id string) error {
				return a.SubmitUserConfirmContext(ctx, &event.UserConfirmResultEvent{ReplyID: id})
			},
			stale: func(a *UnifiedAgent, id string) { a.confirmCh <- event.UserConfirmResultEvent{ReplyID: id} },
			take: func(a *UnifiedAgent) (string, bool) {
				select {
				case v := <-a.confirmCh:
					return v.ReplyID, true
				default:
					return "", false
				}
			},
		},
		{
			name: "external",
			submit: func(a *UnifiedAgent, ctx context.Context, id string) error {
				return a.SubmitExternalResultContext(ctx, &event.ExternalExecutionResultEvent{ReplyID: id})
			},
			stale: func(a *UnifiedAgent, id string) { a.externalCh <- event.ExternalExecutionResultEvent{ReplyID: id} },
			take: func(a *UnifiedAgent) (string, bool) {
				select {
				case v := <-a.externalCh:
					return v.ReplyID, true
				default:
					return "", false
				}
			},
		},
	}
}

func TestSubmitContextArguments(t *testing.T) {
	plain := NewUnifiedAgent("a", "sys", &seamModel{})
	if err := plain.SubmitUserConfirmContext(context.Background(), &event.UserConfirmResultEvent{}); err == nil || !strings.Contains(err.Error(), "permission context") {
		t.Fatalf("no permission context: %v", err)
	}
	if err := plain.SubmitExternalResultContext(context.Background(), &event.ExternalExecutionResultEvent{}); err == nil {
		t.Fatal("no permission context: want an error")
	}
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
	if err := a.SubmitUserConfirmContext(context.Background(), nil); err == nil {
		t.Fatal("nil confirmation result: want an error")
	}
	if err := a.SubmitExternalResultContext(context.Background(), nil); err == nil {
		t.Fatal("nil external result: want an error")
	}
	var nilCtx context.Context
	if err := a.SubmitUserConfirmContext(nilCtx, &event.UserConfirmResultEvent{}); err == nil {
		t.Fatal("nil context: want an error")
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.SubmitUserConfirmContext(done, &event.UserConfirmResultEvent{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context: %v", err)
	}
	if len(a.confirmCh) != 0 {
		t.Fatal("an ended context must not deliver the value")
	}
}

// TestSubmitContextReplacesStaleValueWhenIdle: with no reply core running,
// nothing reads the channel; a value a finished reply left in it is dropped
// instead of blocking the next submit forever.
func TestSubmitContextReplacesStaleValueWhenIdle(t *testing.T) {
	for _, h := range handshakes() {
		t.Run(h.name, func(t *testing.T) {
			a := NewUnifiedAgent("a", "sys", &seamModel{}, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
			h.stale(a, "stale")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := h.submit(a, ctx, "fresh"); err != nil {
				t.Fatalf("submit over a stale value: %v", err)
			}
			if got, ok := h.take(a); !ok || got != "fresh" {
				t.Fatalf("channel holds %q (%v), want the fresh value", got, ok)
			}
		})
	}
}

// TestSubmitContextWaitsWhileACoreRuns: while a reply core runs, a value in
// the channel may still be read, so it is never dropped: the submit waits and
// returns ctx.Err() when ctx ends. Once the core returns, the waiting submit
// replaces the value nobody read.
func TestSubmitContextWaitsWhileACoreRuns(t *testing.T) {
	for _, h := range handshakes() {
		t.Run(h.name, func(t *testing.T) {
			gate := newGatedTool("gate")
			m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
				if call == 0 {
					return seamToolCalls(message.ToolCallBlock{ID: "g", Name: "gate"}), nil
				}
				return seamText("done"), nil
			}}
			perms := permission.NewContext(permission.ModeDefault)
			perms.AllowRules["gate"] = []permission.Rule{{ToolName: "gate", Behavior: permission.BehaviorAllow}}
			a := NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(gate)), WithPermissionContext(perms))
			ch, err := a.ReplyStream(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-gate.started:
			case <-time.After(5 * time.Second):
				t.Fatal("tool did not start")
			}
			h.stale(a, "pending")

			short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			err = h.submit(a, short, "late")
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("submit while a core runs and the channel is full: %v", err)
			}

			result := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result <- h.submit(a, ctx, "after")
			}()
			select {
			case err := <-result:
				t.Fatalf("the submit must wait while the core runs, returned %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if got, ok := h.take(a); !ok || got != "pending" {
				t.Fatalf("a value a running core may read must stay: %q %v", got, ok)
			}
			// The waiting submit fills the room just made.
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if got, ok := h.take(a); !ok || got != "after" {
				t.Fatalf("channel holds %q (%v), want the waiting value", got, ok)
			}

			// Full again; the core ends while a submit waits: the stale value
			// is dropped and the submit completes.
			h.stale(a, "unread")
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result <- h.submit(a, ctx, "final")
			}()
			close(gate.gate)
			drain(t, ch)
			if err := <-result; err != nil {
				t.Fatalf("the submit must complete once the core returned: %v", err)
			}
			if got, ok := h.take(a); !ok || got != "final" {
				t.Fatalf("channel holds %q (%v), want the final value", got, ok)
			}
		})
	}
}

// TestSubmitContextAnswersALiveReply: the variants deliver to a reply that
// waits, like the plain methods.
func TestSubmitContextAnswersALiveReply(t *testing.T) {
	t.Run("confirm", func(t *testing.T) {
		var runs atomic.Int32
		m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
			if call == 0 {
				return seamToolCalls(message.ToolCallBlock{ID: "c1", Name: "write"}), nil
			}
			return seamText("done"), nil
		}}
		perms := permission.NewContext(permission.ModeDefault)
		perms.AskRules["write"] = []permission.Rule{{ToolName: "write", Behavior: permission.BehaviorAsk}}
		a := NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(countingTool("write", &runs))), WithPermissionContext(perms))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ch, err := a.ReplyStream(ctx, "go")
		if err != nil {
			t.Fatal(err)
		}
		for e := range ch {
			if req, ok := e.(event.RequireUserConfirmEvent); ok {
				res := &event.UserConfirmResultEvent{ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: req.ToolCalls[0]}}}
				if err := a.SubmitUserConfirmContext(ctx, res); err != nil {
					t.Fatal(err)
				}
			}
		}
		var r message.ToolResultBlock
		for _, msg := range a.state.Context {
			for _, b := range msg.GetContentBlocks(message.ContentBlockToolResult) {
				r = b.(message.ToolResultBlock)
			}
		}
		if r.State != message.ToolResultSuccess || runs.Load() != 1 {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("external", func(t *testing.T) {
		m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
			if call == 0 {
				return seamToolCalls(message.ToolCallBlock{ID: "ext", Name: "external_tool"}), nil
			}
			return seamText("done"), nil
		}}
		a := NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(newHITLMockTool("external_tool", true))),
			WithPermissionContext(permission.NewContext(permission.ModeBypass)))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ch, err := a.ReplyStream(ctx, "go")
		if err != nil {
			t.Fatal(err)
		}
		for e := range ch {
			if _, ok := e.(event.RequireExternalExecutionEvent); ok {
				res := &event.ExternalExecutionResultEvent{ExecutionResults: []message.ToolResultBlock{
					{Type: "tool_result", ID: "ext", Name: "external_tool", Output: "from outside", State: message.ToolResultSuccess},
				}}
				if err := a.SubmitExternalResultContext(ctx, res); err != nil {
					t.Fatal(err)
				}
			}
		}
		if ctx.Err() != nil || m.callCount() != 2 {
			t.Fatalf("the reply did not finish after the external result: calls=%d", m.callCount())
		}
	})
}
