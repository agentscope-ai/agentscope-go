package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// transferTool marks the agent handed off once it ran, like a committed
// foreground transfer.
func transferTool(target **UnifiedAgent, runs *atomic.Int32) tool.Tool {
	return tool.NewFunctionTool("transfer", "Move the foreground", nil, func(context.Context, map[string]any) (any, error) {
		runs.Add(1)
		(*target).MarkHandedOff()
		return "transferred", nil
	})
}

func countingTool(name string, runs *atomic.Int32) tool.Tool {
	return tool.NewFunctionTool(name, name, nil, func(context.Context, map[string]any) (any, error) {
		runs.Add(1)
		return name + " ran", nil
	})
}

func handoffModel() *seamModel {
	return &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "t1", Name: "transfer"}), nil
		}
		return seamText("must not be called"), nil
	}}
}

func TestHandoffEndsReplyAfterToolStep(t *testing.T) {
	for _, width := range []int{0, 2} {
		name := "sequential"
		if width > 0 {
			name = "bounded"
		}
		t.Run(name, func(t *testing.T) {
			var a *UnifiedAgent
			var runs atomic.Int32
			m := handoffModel()
			opts := []AgentOption{WithToolkit(tool.NewToolkit(transferTool(&a, &runs)))}
			if width > 0 {
				opts = append(opts, WithToolConcurrencyLimit(width))
			}
			a = NewUnifiedAgent("master", "sys", m, opts...)
			ch, err := a.ReplyStream(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			events := drain(t, ch)
			end := replyEnd(events)
			if end == nil || end.FinishedReason != types.ReplyHandedOff || string(end.FinishedReason) != "handed_off" || end.Error != nil {
				t.Fatalf("reply end = %+v", end)
			}
			if m.callCount() != 1 || runs.Load() != 1 {
				t.Fatalf("model calls=%d tool runs=%d", m.callCount(), runs.Load())
			}
			sawStats := false
			for _, e := range events {
				if c, ok := e.(event.CustomEvent); ok && c.Name == "turn_stats" {
					sawStats = true
				}
			}
			if !sawStats {
				t.Fatal("a handed-off reply still reports turn stats")
			}

			// Reply returns the reply message normally; the mark is cleared.
			var b *UnifiedAgent
			b = NewUnifiedAgent("master", "sys", handoffModel(), WithToolkit(tool.NewToolkit(transferTool(&b, &runs))))
			msg, err := b.Reply(context.Background(), "go")
			if err != nil || msg == nil || len(msg.GetContentBlocks(message.ContentBlockToolResult)) != 1 {
				t.Fatalf("Reply = %+v, %v", msg, err)
			}
		})
	}
}

func TestHandoffOnResumedToolStep(t *testing.T) {
	var a *UnifiedAgent
	var runs atomic.Int32
	m := handoffModel()
	state := &AgentState{
		SessionID: "s",
		ReplyID:   "saved",
		Context: []*message.Msg{message.AssistantMsg("master", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "t1", Name: "transfer", Input: `{}`, State: message.ToolCallPending},
		})},
		ReplyRecovery: &ReplyRecoveryState{Version: 1, ReplyID: "saved", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1}},
	}
	a = NewUnifiedAgent("master", "sys", m, WithState(state), WithStateSaver(newFakeStateSaver()), WithReplyRecovery(),
		WithToolkit(tool.NewToolkit(transferTool(&a, &runs))))
	ch, err := a.ResumeReplyStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	end := replyEnd(drain(t, ch))
	if end == nil || end.FinishedReason != types.ReplyHandedOff {
		t.Fatalf("reply end = %+v", end)
	}
	if m.callCount() != 0 || runs.Load() != 1 {
		t.Fatalf("model calls=%d tool runs=%d", m.callCount(), runs.Load())
	}
}

// TestHandoffOnRepromptedConfirmation: a restored call that still awaited its
// confirmation is re-asked and, once confirmed, runs inline on the wait path.
// A handoff it commits ends the reply there, without a model call.
func TestHandoffOnRepromptedConfirmation(t *testing.T) {
	var a *UnifiedAgent
	var runs atomic.Int32
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return seamText("must not be called"), nil
	}}
	state := &AgentState{
		SessionID: "s",
		Context: []*message.Msg{message.AssistantMsg("master", []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "t1", Name: "transfer", Input: `{}`, State: message.ToolCallAsking},
		})},
	}
	a = NewUnifiedAgent("master", "sys", m, WithState(state),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
		WithToolkit(tool.NewToolkit(transferTool(&a, &runs))))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "go on")
	if err != nil {
		t.Fatal(err)
	}
	asks := 0
	var events []event.Event
	for e := range ch {
		events = append(events, e)
		if req, ok := e.(event.RequireUserConfirmEvent); ok {
			asks++
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: req.ToolCalls[0]}}})
		}
	}
	end := replyEnd(events)
	if asks != 1 || runs.Load() != 1 || end == nil || end.FinishedReason != types.ReplyHandedOff {
		t.Fatalf("asks=%d runs=%d end=%+v", asks, runs.Load(), end)
	}
	if m.callCount() != 0 {
		t.Fatalf("a handoff on the confirmation path must end the reply before any model call, got %d", m.callCount())
	}
}

func TestHandoffCannotBeSwallowed(t *testing.T) {
	var a *UnifiedAgent
	var runs atomic.Int32
	m := handoffModel()
	mw := &swallowEndMiddleware{maxSwallow: -1}
	a = NewUnifiedAgent("master", "sys", m, WithMiddlewares(mw), WithToolkit(tool.NewToolkit(transferTool(&a, &runs))))
	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch) // the stream closes without waiting for a swallow verdict
	if m.callCount() != 1 {
		t.Fatalf("a swallowed handoff must not start another round: %d model calls", m.callCount())
	}

	var b *UnifiedAgent
	pass := &testLoggingMiddleware{}
	b = NewUnifiedAgent("master", "sys", handoffModel(), WithMiddlewares(pass), WithToolkit(tool.NewToolkit(transferTool(&b, &runs))))
	ch, err = b.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if end := replyEnd(drain(t, ch)); end == nil || end.FinishedReason != types.ReplyHandedOff {
		t.Fatalf("a forwarding middleware delivers the handed-off end: %+v", end)
	}
}

func TestHandoffMarkClearedAtReplyStart(t *testing.T) {
	var runs atomic.Int32
	m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "e", Name: "echo"}), nil
		}
		return seamText("done"), nil
	}}
	a := NewUnifiedAgent("a", "sys", m, WithToolkit(tool.NewToolkit(countingTool("echo", &runs))))
	a.MarkHandedOff() // outside a reply: no effect
	ch, err := a.ReplyStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if end := replyEnd(drain(t, ch)); end == nil || end.FinishedReason != types.ReplyCompleted || m.callCount() != 2 {
		t.Fatalf("reply end = %+v after %d calls", end, m.callCount())
	}
}

func TestHandoffConversationTurnHasNoInterruptMarker(t *testing.T) {
	var a *UnifiedAgent
	var runs atomic.Int32
	a = NewUnifiedAgent("master", "sys", handoffModel(), WithToolkit(tool.NewToolkit(transferTool(&a, &runs))))
	conv, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.Submit("go"); err != nil {
		t.Fatal(err)
	}
	convWait(t, conv, "handed-off end", func(e event.Event) bool {
		end, ok := e.(event.ReplyEndEvent)
		return ok && end.FinishedReason == types.ReplyHandedOff
	})
	_ = conv.Close()
	for _, m := range a.state.Context {
		if msgText(m) == interruptedTurnMarkerText {
			t.Fatal("a handed-off turn is not an interruption")
		}
	}
}

func TestExclusiveToolsRejectMixedStep(t *testing.T) {
	// The model may spell the exclusive tool in any case the toolkit
	// resolves; every spelling must trip the check on every path.
	for _, callName := range []string{"transfer", "Transfer", "TRANSFER"} {
		t.Run(callName, func(t *testing.T) { testExclusiveToolsRejectMixedStep(t, callName) })
	}
}

func testExclusiveToolsRejectMixedStep(t *testing.T, callName string) {
	mixed := []message.ToolCallBlock{{ID: "x1", Name: callName}, {ID: "x2", Name: "echo"}}
	want := callName + " must be called alone; call it again by itself"
	check := func(t *testing.T, a *UnifiedAgent, m *seamModel, transferRuns, echoRuns *atomic.Int32, wantCalls int) {
		t.Helper()
		if transferRuns.Load() != 0 || echoRuns.Load() != 0 {
			t.Fatalf("no call of a mixed step may run: transfer=%d echo=%d", transferRuns.Load(), echoRuns.Load())
		}
		if m.callCount() != wantCalls {
			t.Fatalf("model calls = %d, want %d", m.callCount(), wantCalls)
		}
		input := m.call(wantCalls - 1).msgs
		var results []message.ToolResultBlock
		for _, msg := range input {
			for _, b := range msg.GetContentBlocks(message.ContentBlockToolResult) {
				results = append(results, b.(message.ToolResultBlock))
			}
		}
		if len(results) != 2 {
			t.Fatalf("every call needs a result, got %d", len(results))
		}
		for _, r := range results {
			if r.State != message.ToolResultError || r.GetOutputText() != want {
				t.Fatalf("result = %+v", r)
			}
		}
	}

	for _, width := range []int{0, 2} {
		t.Run("fresh step width "+string(rune('0'+width)), func(t *testing.T) {
			var transferRuns, echoRuns atomic.Int32
			m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
				if call == 0 {
					return seamToolCalls(mixed...), nil
				}
				return seamText("done"), nil
			}}
			opts := []AgentOption{
				WithToolkit(tool.NewToolkit(countingTool("transfer", &transferRuns), countingTool("echo", &echoRuns))),
				WithExclusiveTools("transfer"),
			}
			if width > 0 {
				opts = append(opts, WithToolConcurrencyLimit(width))
			}
			a := NewUnifiedAgent("a", "sys", m, opts...)
			if _, err := a.Reply(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			check(t, a, m, &transferRuns, &echoRuns, 2)
		})
	}

	t.Run("resumed step", func(t *testing.T) {
		var transferRuns, echoRuns atomic.Int32
		m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
			return seamText("done"), nil
		}}
		blocks := []message.ContentBlock{}
		for _, c := range mixed {
			c.Type, c.Input, c.State = "tool_call", `{}`, message.ToolCallPending
			blocks = append(blocks, c)
		}
		state := &AgentState{
			SessionID:     "s",
			ReplyID:       "saved",
			Context:       []*message.Msg{message.AssistantMsg("a", blocks)},
			ReplyRecovery: &ReplyRecoveryState{Version: 1, ReplyID: "saved", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1}},
		}
		a := NewUnifiedAgent("a", "sys", m, WithState(state), WithStateSaver(newFakeStateSaver()), WithReplyRecovery(),
			WithToolkit(tool.NewToolkit(countingTool("transfer", &transferRuns), countingTool("echo", &echoRuns))),
			WithExclusiveTools("transfer"))
		ch, err := a.ResumeReplyStream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		drain(t, ch)
		check(t, a, m, &transferRuns, &echoRuns, 1)
	})

	t.Run("alone runs", func(t *testing.T) {
		var transferRuns, echoRuns atomic.Int32
		m := &seamModel{respond: func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
			if call == 0 {
				return seamToolCalls(message.ToolCallBlock{ID: "x1", Name: callName}), nil
			}
			return seamText("done"), nil
		}}
		a := NewUnifiedAgent("a", "sys", m, WithExclusiveTools("transfer"),
			WithToolkit(tool.NewToolkit(countingTool("transfer", &transferRuns), countingTool("echo", &echoRuns))))
		if _, err := a.Reply(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if transferRuns.Load() != 1 {
			t.Fatal("an exclusive call alone must run")
		}
	})

	t.Run("loop bridge", func(t *testing.T) {
		var transferRuns, echoRuns atomic.Int32
		a := NewUnifiedAgent("a", "sys", &seamModel{}, WithExclusiveTools("transfer"),
			WithToolkit(tool.NewToolkit(countingTool("transfer", &transferRuns), countingTool("echo", &echoRuns))),
			WithPermissionContext(permission.NewContext(permission.ModeBypass)))
		exec := &toolExecutorAdapter{agent: a, orchestrator: tool.NewOrchestrator(tool.OrchestratorConfig{Toolkit: a.toolkit, PermEngine: a.engine})}
		calls := []message.ToolCallBlock{{Type: "tool_call", ID: "x1", Name: callName, Input: `{}`}, {Type: "tool_call", ID: "x2", Name: "echo", Input: `{}`}}
		results := exec.BatchExecute(context.Background(), calls)
		if len(results) != 2 || transferRuns.Load() != 0 || echoRuns.Load() != 0 {
			t.Fatalf("results=%d transfer=%d echo=%d", len(results), transferRuns.Load(), echoRuns.Load())
		}
		for _, r := range results {
			if r.Response == nil || r.Response.State != message.ToolResultError || responseText(r.Response) != want {
				t.Fatalf("result = %+v", r.Response)
			}
		}
	})
}

// TestExclusiveRejectionSkipsPermissionHook: a step WithExclusiveTools
// rejects runs nothing, so no OnCheckPermission hook sees its calls, under
// sequential and bounded dispatch. Without the option the same step reaches
// the hook once per call.
func TestExclusiveRejectionSkipsPermissionHook(t *testing.T) {
	for _, width := range []int{0, 2} {
		for _, exclusive := range []bool{true, false} {
			t.Run(fmt.Sprintf("width %d exclusive %v", width, exclusive), func(t *testing.T) {
				var hookCalls, transferRuns atomic.Int32
				mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
					hookCalls.Add(1)
					return next(ctx, in)
				}}
				target := hookTool()
				opts := []AgentOption{
					WithToolkit(tool.NewToolkit(target, countingTool("transfer", &transferRuns))),
					WithPermissionContext(permission.NewContext(permission.ModeDefault)),
					WithMiddlewares(mw),
				}
				if exclusive {
					opts = append(opts, WithExclusiveTools("transfer"))
				}
				if width > 0 {
					opts = append(opts, WithToolConcurrencyLimit(width))
				}
				transfer := message.ToolCallBlock{Type: "tool_call", ID: "t1", Name: "transfer", Input: `{}`, State: message.ToolCallPending}
				a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("c1"), transfer), opts...)
				drainHookReply(t, a, false)
				want := int32(2)
				if exclusive {
					want = 0
					if target.executions.Load() != 0 || transferRuns.Load() != 0 {
						t.Fatal("no call of a rejected step may run")
					}
				}
				if got := hookCalls.Load(); got != want {
					t.Fatalf("permission hook calls = %d, want %d", got, want)
				}
			})
		}
	}
}

func TestExclusiveCallResolution(t *testing.T) {
	noop := func(context.Context, map[string]any) (any, error) { return "ok", nil }
	for _, tc := range []struct {
		name      string
		exclusive string
		tools     []string
		call      string
		want      bool
	}{
		{"exact", "transfer", []string{"transfer"}, "transfer", true},
		{"call in another case", "transfer", []string{"transfer"}, "Transfer", true},
		{"configured in another case", "TRANSFER", []string{"transfer"}, "transfer", true},
		{"both differ from the tool", "TRANSFER", []string{"Transfer"}, "transfer", true},
		{"unknown tool, another case", "transfer", nil, "TRANSFER", true},
		// The toolkit lowercases names (U+0130 becomes "i") where case
		// folding does not match them: only resolution catches this call.
		{"resolved by lowercasing only", "info", []string{"info"}, "İnfo", true},
		{"other tool", "transfer", []string{"transfer", "echo"}, "echo", false},
		{"unknown tool, other name", "transfer", nil, "echo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tools []tool.Tool
			for _, n := range tc.tools {
				tools = append(tools, tool.NewFunctionTool(n, n, nil, noop))
			}
			a := NewUnifiedAgent("a", "sys", &seamModel{}, WithToolkit(tool.NewToolkit(tools...)), WithExclusiveTools(tc.exclusive))
			if got := a.isExclusiveCall(tc.call); got != tc.want {
				t.Fatalf("isExclusiveCall(%q) = %v, want %v", tc.call, got, tc.want)
			}
		})
	}

	// A tool in an inactive group resolves to nothing; its name is then
	// compared case-insensitively.
	tk := tool.NewToolkit(tool.NewFunctionTool("echo", "echo", nil, noop))
	tk.AddGroup("xfer", tool.NewFunctionTool("transfer", "transfer", nil, noop))
	tk.DeactivateGroup("xfer")
	a := NewUnifiedAgent("a", "sys", &seamModel{}, WithToolkit(tk), WithExclusiveTools("transfer"))
	if !a.isExclusiveCall("Transfer") || a.isExclusiveCall("echo") {
		t.Fatal("an unresolvable name must be compared case-insensitively")
	}
}

func responseText(r *tool.ToolResponse) string {
	var sb strings.Builder
	for _, b := range r.Content {
		if tb, ok := b.(message.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}
