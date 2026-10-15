package agent

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Deliberately not thread-safe: Hook has no concurrency requirement. Read the
// slice only after core completion; the atomic counter supports gated admission.
type dispatchHook struct {
	calls  []string
	before atomic.Int32
	check  func(string)
}

func (*dispatchHook) BeforeModelCall(protocol.LoopState, int)       {}
func (*dispatchHook) AfterModelCall(protocol.LoopState, int, error) {}
func (h *dispatchHook) BeforeToolExec(_ protocol.LoopState, _ int, name string) {
	if h.check != nil {
		h.check(name)
	}
	h.calls = append(h.calls, "before:"+name)
	h.before.Add(1)
}
func (h *dispatchHook) AfterToolExec(_ protocol.LoopState, _ int, name string, _ error) {
	h.calls = append(h.calls, "after:"+name)
}
func (*dispatchHook) OnStateTransition(protocol.LoopState, protocol.LoopState, int) {}
func (*dispatchHook) OnLoopStart()                                                  {}
func (*dispatchHook) OnLoopEnd(error)                                               {}

type dispatchSaver struct {
	*fakeStateSaver
	rejectCanceled bool
}

func (s *dispatchSaver) SaveState(ctx context.Context, id string, state *AgentState) error {
	if s.rejectCanceled && ctx.Err() != nil {
		return ctx.Err()
	}
	return s.fakeStateSaver.SaveState(ctx, id, state)
}

type dispatchFixture struct {
	a       *UnifiedAgent
	saver   *dispatchSaver
	hook    *dispatchHook
	core    *recoveryCancelForwarder
	cancel  context.CancelFunc
	entered chan int
	gates   []chan struct{}
	once    []sync.Once
	drained chan struct{}
	events  []event.Event
	peak    atomic.Int32
	active  atomic.Int32
	calls   []message.ToolCallBlock
}

type dispatchConfig struct {
	width, count                            int
	resume, mixed, rejectSave, confirmation bool
	wrap                                    func(tool.Tool) tool.Tool
	before                                  func(context.CancelFunc, string)
}

func newDispatchFixture(t *testing.T, cfg dispatchConfig) *dispatchFixture {
	width, count, resume, mixed, rejectSave := cfg.width, cfg.count, cfg.resume, cfg.mixed, cfg.rejectSave
	t.Helper()
	f := &dispatchFixture{
		saver: &dispatchSaver{fakeStateSaver: newFakeStateSaver(), rejectCanceled: rejectSave},
		hook:  &dispatchHook{}, core: &recoveryCancelForwarder{done: make(chan struct{})},
		entered: make(chan int, count), drained: make(chan struct{}),
		gates: make([]chan struct{}, count), once: make([]sync.Once, count),
	}
	var tools []tool.Tool
	for i := range count {
		f.gates[i] = make(chan struct{})
		name := fmt.Sprintf("lookup-%02d", i)
		f.calls = append(f.calls, message.ToolCallBlock{Type: "tool_call", ID: name, Name: name, Input: `{}`, State: message.ToolCallPending})
		lookup := tool.NewFunctionTool(name, "gated lookup", nil, func(context.Context, map[string]any) (any, error) {
			active := f.active.Add(1)
			defer f.active.Add(-1)
			for old := f.peak.Load(); active > old && !f.peak.CompareAndSwap(old, active); old = f.peak.Load() {
			}
			f.entered <- i
			<-f.gates[i] // deliberately ignores cancellation; ownership must remain
			if i == 1 {
				return nil, fmt.Errorf("ordinary fixture error")
			}
			return name, nil
		})
		lookup.ConcurrencySafe = !mixed || i != 4 || cfg.confirmation
		if mixed && i == 5 {
			external := newHITLMockTool(name, true)
			external.ConcurrencySafe = true
			tools = append(tools, external)
		} else {
			var resolved tool.Tool = lookup
			if cfg.wrap != nil {
				resolved = cfg.wrap(resolved)
			}
			tools = append(tools, resolved)
		}
	}
	permissionCtx := permission.NewContext(permission.ModeBypass)
	if cfg.confirmation {
		permissionCtx = permission.NewContext(permission.ModeDefault)
		for _, call := range f.calls {
			if call.Name != "lookup-04" {
				permissionCtx.AllowRules[call.Name] = []permission.Rule{{ToolName: call.Name, Behavior: permission.BehaviorAllow}}
			}
		}
	}
	if mixed {
		f.hook.check = func(name string) {
			if (name == "lookup-04" || name == "lookup-05") && f.active.Load() != 0 {
				t.Errorf("barrier %s overlapped active tools", name)
			}
		}
	}
	state := &AgentState{SessionID: "dispatch-fixture"}
	m := &mockChatModel{}
	if resume {
		blocks := make([]message.ContentBlock, len(f.calls))
		for i, call := range f.calls {
			blocks[i] = call
		}
		state.SchemaVersion = StateSchemaVersion
		state.ReplyID = "saved-reply"
		state.Context = []*message.Msg{message.AssistantMsg("bot", blocks)}
		state.ReplyRecovery = &ReplyRecoveryState{Version: 1, ReplyID: state.ReplyID, Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1, Counters: map[string]middleware.ReplyBudgetCounter{"fixture": {UsedTokens: 7}}}}
	} else {
		blocks := make([]message.ContentBlock, len(f.calls))
		for i, call := range f.calls {
			blocks[i] = call
		}
		m.responses = []model.ChatResponse{{Content: blocks, IsLast: true}}
	}
	opts := []AgentOption{WithState(state), WithStateSaver(f.saver), WithReplyRecovery(), WithToolkit(tool.NewToolkit(tools...)), WithPermissionContext(permissionCtx), WithModelStreaming(false), WithMiddlewares(f.core), WithLoopHooks(f.hook), WithReactConfig(ReactConfig{MaxIters: 4})}
	if width > 0 {
		opts = append(opts, WithToolConcurrencyLimit(width))
	}
	f.a = NewUnifiedAgent("bot", "fixture", m, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	if cfg.before != nil {
		check := f.hook.check
		f.hook.check = func(name string) {
			if check != nil {
				check(name)
			}
			cfg.before(cancel, name)
		}
	}
	t.Cleanup(func() {
		cancel()
		f.releaseAll()
		waitDispatch(t, f.core.done, "core cleanup")
		waitDispatch(t, f.drained, "outward cleanup")
	})
	var stream <-chan event.Event
	var err error
	if resume {
		stream, err = f.a.ResumeReplyStream(ctx)
	} else {
		stream, err = f.a.ReplyStream(ctx, "compare independent records")
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(f.drained)
		for e := range stream {
			f.events = append(f.events, e)
			if request, ok := e.(event.RequireUserConfirmEvent); ok {
				confirmed := request.ToolCalls[0]
				confirmed.State = message.ToolCallAllowed
				f.a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: confirmed}}})
			}
			if request, ok := e.(event.RequireExternalExecutionEvent); ok {
				f.a.SubmitExternalResult(&event.ExternalExecutionResultEvent{ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: request.ToolCalls[0].ID, State: message.ToolResultSuccess, Output: "host answer"}}})
			}
		}
	}()
	return f
}

func waitDispatch(t *testing.T, ch <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", why)
	}
}

func (f *dispatchFixture) starts(t *testing.T, n int) []int {
	t.Helper()
	got := make([]int, 0, n)
	for range n {
		select {
		case i := <-f.entered:
			got = append(got, i)
		case <-time.After(5 * time.Second):
			t.Fatal("tool admission timed out")
		}
	}
	slices.Sort(got)
	return got
}

func (f *dispatchFixture) release(i int) { f.once[i].Do(func() { close(f.gates[i]) }) }
func (f *dispatchFixture) releaseAll() {
	for i := range f.gates {
		f.release(i)
	}
}
func (f *dispatchFixture) finish(t *testing.T) {
	t.Helper()
	waitDispatch(t, f.core.done, "actual core completion")
	waitDispatch(t, f.drained, "stream completion")
}
func (f *dispatchFixture) results(t *testing.T) []message.ToolResultBlock {
	t.Helper()
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	var results []message.ToolResultBlock
	for _, msg := range f.a.state.Context {
		for _, b := range msg.Content {
			if result, ok := b.(message.ToolResultBlock); ok {
				results = append(results, result)
			}
			if call, ok := b.(message.ToolCallBlock); ok && call.State != message.ToolCallFinished {
				t.Errorf("call %s left %s", call.ID, call.State)
			}
		}
	}
	if len(results) != len(f.calls) {
		t.Fatalf("results=%d, want %d", len(results), len(f.calls))
	}
	for i, result := range results {
		if result.ID != f.calls[i].ID {
			t.Errorf("result %d ID=%s", i, result.ID)
		}
	}
	return results
}

func TestBoundedDispatchWidths(t *testing.T) {
	for _, width := range []int{0, 1, 2, 20} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			f := newDispatchFixture(t, dispatchConfig{width: width, count: 6})
			initial := 6
			if width > 0 {
				initial = min(width, initial)
			}
			f.starts(t, initial)
			if got := int(f.hook.before.Load()); got != initial {
				t.Errorf("admitted execution hooks=%d while %d calls are gated", got, initial)
			}
			f.releaseAll()
			f.finish(t)
			if int(f.peak.Load()) > initial {
				t.Errorf("tool peak=%d exceeds width %d", f.peak.Load(), initial)
			}
			results := f.results(t)
			if results[1].State != message.ToolResultError || results[5].State != message.ToolResultSuccess {
				t.Fatal("ordinary failure canceled a sibling or changed its outcome")
			}
			var ids []string
			for _, e := range f.events {
				if end, ok := e.(event.ToolResultEndEvent); ok {
					ids = append(ids, end.ToolCallID)
				}
			}
			for i, id := range ids {
				if id != f.calls[i].ID {
					t.Errorf("event order=%v", ids)
				}
			}
			if len(ids) != 6 || len(f.hook.calls) != 12 {
				t.Fatalf("terminal events=%d hooks=%v", len(ids), f.hook.calls)
			}
		})
	}
}

func TestBoundedDispatchRefillsWhileBlocked(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			f := newDispatchFixture(t, dispatchConfig{width: 2, count: 5, resume: resume})
			if got := f.starts(t, 2); !slices.Equal(got, []int{0, 1}) {
				t.Fatalf("first admitted=%v", got)
			}
			if f.hook.before.Load() != 2 {
				t.Fatalf("queued tools already admitted: %d", f.hook.before.Load())
			}
			f.release(1) // error result must still refill without waiting for call0
			if got := f.starts(t, 1); !slices.Equal(got, []int{2}) {
				t.Fatalf("refill=%v", got)
			}
			f.releaseAll()
			f.finish(t)
			f.results(t)
		})
	}
}

func TestBoundedDispatchCancellationAndRecovery(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, rejectSave := range []bool{false, true} {
			t.Run(fmt.Sprintf("resume=%v/rejectSave=%v", resume, rejectSave), func(t *testing.T) {
				f := newDispatchFixture(t, dispatchConfig{width: 2, count: 8, resume: resume, mixed: true, rejectSave: rejectSave})
				if got := f.starts(t, 2); !slices.Equal(got, []int{0, 1}) {
					t.Fatalf("first admitted=%v", got)
				}
				f.cancel()
				waitDispatch(t, f.drained, "outward cancellation")
				if _, err := f.a.ResumeReplyStream(context.Background()); err == nil {
					t.Fatal("recovery lease released while admitted tools still run")
				}
				f.releaseAll()
				f.finish(t)
				results := f.results(t)
				for i, result := range results {
					want := message.ToolResultInterrupted
					if i == 0 {
						want = message.ToolResultSuccess
					} else if i == 1 {
						want = message.ToolResultError
					}
					if result.State != want {
						t.Errorf("call%d state=%s want=%s", i, result.State, want)
					}
				}
				if f.hook.before.Load() != 2 || len(f.hook.calls) != 4 {
					t.Errorf("never-admitted calls emitted execution hooks: %v", f.hook.calls)
				}
				stored, err := f.saver.LoadState(context.Background(), "dispatch-fixture")
				if err != nil {
					t.Fatal(err)
				}
				storedResults := 0
				for _, msg := range stored.Context {
					storedResults += len(msg.GetContentBlocks(message.ContentBlockToolResult))
				}
				if rejectSave {
					if storedResults != 0 || f.a.activeRecovery != nil {
						t.Fatalf("failed save changed prior snapshot or retained core: results=%d", storedResults)
					}
				} else {
					if storedResults != 8 || stored.ReplyRecovery.NextIteration != 1 || stored.SchemaVersion != StateSchemaVersion {
						t.Fatalf("checkpoint lost completion/iteration/schema: %+v results=%d", stored.ReplyRecovery, storedResults)
					}
					if resume && stored.ReplyRecovery.Budgets.Counters["fixture"].UsedTokens != 7 {
						t.Fatal("resumed budget counter changed")
					}
					var extraAttempts atomic.Int32
					var followTools []tool.Tool
					for _, call := range f.calls {
						followTools = append(followTools, tool.NewFunctionTool(call.Name, "must not execute", nil, func(context.Context, map[string]any) (any, error) {
							extraAttempts.Add(1)
							return nil, fmt.Errorf("recorded call executed again")
						}))
					}
					follow := NewUnifiedAgent("bot", "fixture", &mockChatModel{}, WithState(stored), WithStateSaver(f.saver), WithToolkit(tool.NewToolkit(followTools...)), WithPermissionContext(permission.NewContext(permission.ModeBypass)), WithReplyRecovery(), WithToolConcurrencyLimit(2))
					followCtx, followCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer followCancel()
					stream, err := follow.ResumeReplyStream(followCtx)
					if err != nil {
						t.Fatal(err)
					}
					for range stream {
					}
					final, err := f.saver.LoadState(context.Background(), "dispatch-fixture")
					if err != nil || final.ReplyRecovery.Active {
						t.Fatalf("recorded interrupted calls failed to resume to terminal: %v", err)
					}
					finalResults := 0
					for _, msg := range final.Context {
						finalResults += len(msg.GetContentBlocks(message.ContentBlockToolResult))
					}
					if extraAttempts.Load() != 0 || finalResults != 8 || followCtx.Err() != nil {
						t.Fatalf("resume repeated calls: attempts=%d results=%d error=%v", extraAttempts.Load(), finalResults, followCtx.Err())
					}
				}
			})
		}
	}
}

func TestBoundedDispatchRejectsNonpositiveWidth(t *testing.T) {
	for _, width := range []int{0, -1} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid configured width did not panic during construction")
				}
			}()
			NewUnifiedAgent("bot", "fixture", &mockChatModel{}, WithToolConcurrencyLimit(width))
		})
	}
}

func TestBoundedDispatchCancelDuringAdmissionHook(t *testing.T) {
	f := newDispatchFixture(t, dispatchConfig{width: 1, count: 3, before: func(cancel context.CancelFunc, _ string) { cancel() }})
	if got := f.starts(t, 1); !slices.Equal(got, []int{0}) {
		t.Fatalf("admitted call=%v", got)
	}
	// Cancellation happened after handoff, before the body began. It must not
	// abandon the start gate or relabel the admitted call's actual success.
	f.releaseAll()
	f.finish(t)
	results := f.results(t)
	if results[0].State != message.ToolResultSuccess || results[1].State != message.ToolResultInterrupted || results[2].State != message.ToolResultInterrupted {
		t.Fatalf("handoff outcomes=%v", results)
	}
	if f.hook.before.Load() != 1 || len(f.hook.calls) != 2 {
		t.Fatalf("handoff hooks=%v", f.hook.calls)
	}
}

func TestBoundedDispatchBarriers(t *testing.T) {
	for _, confirmation := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmation), func(t *testing.T) {
			f := newDispatchFixture(t, dispatchConfig{width: 2, count: 8, mixed: true, confirmation: confirmation})
			f.starts(t, 2)
			f.releaseAll()
			f.finish(t)
			results := f.results(t)
			for i, result := range results {
				want := message.ToolResultSuccess
				if i == 1 {
					want = message.ToolResultError
				}
				if result.State != want {
					t.Errorf("call%d=%s want=%s", i, result.State, want)
				}
			}
			confirms, external := 0, 0
			for _, e := range f.events {
				switch e.(type) {
				case event.RequireUserConfirmEvent:
					confirms++
				case event.RequireExternalExecutionEvent:
					external++
				}
			}
			wantConfirms := 0
			if confirmation {
				wantConfirms = 1
			}
			if confirms != wantConfirms || external != 1 || f.peak.Load() > 2 {
				t.Fatalf("confirms=%d external=%d peak=%d", confirms, external, f.peak.Load())
			}
		})
	}
}

type dispatchMCPClient struct{ original tool.Tool }

func (*dispatchMCPClient) ListTools(context.Context) ([]model.ToolSchema, error) { return nil, nil }
func (*dispatchMCPClient) Close() error                                          { return nil }
func (c *dispatchMCPClient) CallTool(ctx context.Context, _ string, in map[string]any) (*tool.ToolResponse, error) {
	return c.original.Execute(ctx, in)
}

func TestBoundedDispatchToolAdapters(t *testing.T) {
	wrappers := map[string]func(tool.Tool) tool.Tool{
		"streaming": func(t tool.Tool) tool.Tool { return tool.WrapNonStreamingTool(t) },
		"MCP": func(t tool.Tool) tool.Tool {
			return mcp.NewMCPTool(&dispatchMCPClient{original: t}, model.ToolSchema{Type: "function", Function: model.ToolFunction{Name: t.Name(), Description: t.Description(), Parameters: t.InputSchema()}})
		},
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t, dispatchConfig{width: 2, count: 5, wrap: wrap})
			if got := f.starts(t, 2); !slices.Equal(got, []int{0, 1}) || f.hook.before.Load() != 2 {
				t.Fatalf("initial adapter calls=%v hooks=%d", got, f.hook.before.Load())
			}
			f.release(1)
			if got := f.starts(t, 1); !slices.Equal(got, []int{2}) {
				t.Fatalf("adapter refill=%v", got)
			}
			f.releaseAll()
			f.finish(t)
			if f.peak.Load() > 2 || f.results(t)[1].State != message.ToolResultError {
				t.Fatal("adapter lost width or ordinary-error semantics")
			}
		})
	}
}

func TestBoundedDispatchEmptyAndAlreadyCanceled(t *testing.T) {
	f := newDispatchFixture(t, dispatchConfig{width: 2})
	f.finish(t)
	if f.hook.before.Load() != 0 {
		t.Fatal("empty reply executed tools")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := []message.ToolCallBlock{{Type: "tool_call", ID: "not-started", Name: "missing", Input: `{}`, State: message.ToolCallPending}}
	f.a.state.Context = append(f.a.state.Context, message.AssistantMsg("bot", []message.ContentBlock{calls[0]}))
	f.a.executeBoundedToolStep(ctx, nil, "canceled", calls, func(context.Context, *middleware.ActingInput) (*tool.ToolResponse, error) {
		t.Error("already canceled call reached acting")
		return nil, nil
	}, loop.NewHookRunner(f.hook), protocol.StateAct, 0)
	last := f.a.state.Context[len(f.a.state.Context)-1]
	results := last.GetContentBlocks(message.ContentBlockToolResult)
	if len(results) != 1 || results[0].(message.ToolResultBlock).State != message.ToolResultInterrupted || f.hook.before.Load() != 0 {
		t.Fatalf("already-canceled outcome=%v", results)
	}
}
