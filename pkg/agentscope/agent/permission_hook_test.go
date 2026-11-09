package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

type permissionHook struct {
	middleware.BaseMiddleware
	fn func(context.Context, *middleware.CheckPermissionInput, middleware.CheckPermissionHandler) (*permission.Decision, error)
}

func (m *permissionHook) OnCheckPermission(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
	return m.fn(ctx, in, next)
}

type permissionHookTool struct {
	tool.BaseTool
	executions atomic.Int32
	checks     func()
	external   bool
}

func (t *permissionHookTool) CheckPermissions(map[string]any, *permission.Context) permission.Decision {
	if t.checks != nil {
		t.checks()
	}
	return permission.Decision{Behavior: permission.BehaviorAllow}
}
func (t *permissionHookTool) Execute(context.Context, map[string]any) (*tool.ToolResponse, error) {
	t.executions.Add(1)
	return tool.NewTextResponse("executed"), nil
}
func (t *permissionHookTool) IsExternalTool() bool { return t.external }
func hookCall(id string) message.ToolCallBlock {
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "checked", Input: `{"nested":{"value":"original"}}`, State: message.ToolCallPending}
}
func hookModel(calls ...message.ToolCallBlock) *mockChatModel {
	content := make([]message.ContentBlock, len(calls))
	for i := range calls {
		content[i] = calls[i]
	}
	return &mockChatModel{responses: []model.ChatResponse{{Content: content, IsLast: true}, {Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}}}
}
func hookTool() *permissionHookTool {
	return &permissionHookTool{BaseTool: tool.BaseTool{ToolName: "checked", ConcurrencySafe: true}}
}
func drainHookReply(t *testing.T, a *UnifiedAgent, resume bool) (asks int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var ch <-chan event.Event
	var err error
	if resume {
		ch, err = a.ResumeReplyStream(ctx)
	} else {
		ch, err = a.ReplyStream(ctx, "run")
	}
	if err != nil {
		t.Fatal(err)
	}
	for e := range ch {
		switch ev := e.(type) {
		case event.RequireUserConfirmEvent:
			asks++
			results := []event.ConfirmResult{}
			for _, tc := range ev.ToolCalls {
				results = append(results, event.ConfirmResult{ToolCall: tc, Confirmed: false})
			}
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: results})
		case event.RequireExternalExecutionEvent:
			cancel()
		}
	}
	return asks
}

func TestPermissionHookDeny(t *testing.T) {
	target := hookTool()
	var calls atomic.Int32
	mw := &permissionHook{fn: func(_ context.Context, in *middleware.CheckPermissionInput, _ middleware.CheckPermissionHandler) (*permission.Decision, error) {
		calls.Add(1)
		if in.AgentName != "bot" || in.ToolCall.ID != "fresh" || in.ToolInput["nested"].(map[string]any)["value"] != "original" {
			t.Error("wrong permission input")
		}
		return &permission.Decision{Behavior: permission.BehaviorDeny}, nil
	}}
	a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("fresh")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mw))
	drainHookReply(t, a, false)
	if calls.Load() != 1 || target.executions.Load() != 0 {
		t.Fatalf("hook=%d executions=%d", calls.Load(), target.executions.Load())
	}
}

func TestPermissionHookOnionAndEngine(t *testing.T) {
	var order []string
	target := hookTool()
	target.checks = func() { order = append(order, "engine") }
	mk := func(label string) *permissionHook {
		return &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
			order = append(order, label+" before")
			d, e := next(ctx, in)
			order = append(order, label+" after")
			return d, e
		}}
	}
	a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("fresh")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mk("outer"), mk("inner")))
	drainHookReply(t, a, false)
	if !reflect.DeepEqual(order, []string{"outer before", "inner before", "engine", "inner after", "outer after"}) || target.executions.Load() != 1 {
		t.Fatalf("order=%v executions=%d", order, target.executions.Load())
	}
}

func TestPermissionHookFailures(t *testing.T) {
	for _, kind := range []string{"error", "nil", "unknown", "rewrite next", "rewrite after", "cancel", "passthrough", "ask"} {
		t.Run(kind, func(t *testing.T) {
			target := hookTool()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mw := &permissionHook{fn: func(c context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
				switch kind {
				case "error":
					return nil, errors.New("hook failure")
				case "nil":
					return nil, nil
				case "unknown":
					return &permission.Decision{Behavior: "invalid"}, nil
				case "rewrite next":
					in.ToolInput["nested"].(map[string]any)["value"] = "rewritten"
					return next(c, in)
				case "rewrite after":
					d, e := next(c, in)
					in.ToolCall.Name = "other"
					return d, e
				case "cancel":
					d, e := next(c, in)
					cancel()
					return d, e
				case "ask":
					return &permission.Decision{Behavior: permission.BehaviorAsk}, nil
				default:
					return &permission.Decision{Behavior: permission.BehaviorPassthrough}, nil
				}
			}}
			a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("fresh")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mw))
			ch, err := a.ReplyStream(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			asks := 0
			for e := range ch {
				if ev, ok := e.(event.RequireUserConfirmEvent); ok {
					asks++
					a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: []event.ConfirmResult{{ToolCall: ev.ToolCalls[0], Confirmed: false}}})
				}
			}
			if target.executions.Load() != 0 {
				t.Fatal("permission failure executed tool")
			}
			if (kind == "ask" || kind == "passthrough") && asks != 1 {
				t.Fatal("lost confirmation request")
			}
		})
	}
}

func TestPermissionHookExcludedPaths(t *testing.T) {
	for _, kind := range []string{"no engine", "parallel", "bounded parallel", "bounded width-one batch", "external", "restored pending", "restored allowed", "restored asking", "reprompt asking", "restored submitted", "loop", "orchestrator", "buffered reply", "resume new call"} {
		t.Run(kind, func(t *testing.T) {
			target := hookTool()
			var hooks atomic.Int32
			mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
				hooks.Add(1)
				return next(ctx, in)
			}}
			calls := []message.ToolCallBlock{hookCall("one")}
			if kind == "parallel" || kind == "bounded parallel" || kind == "bounded width-one batch" {
				calls = append(calls, hookCall("two"))
			}
			opts := []AgentOption{WithToolkit(tool.NewToolkit(target)), WithMiddlewares(mw)}
			if kind != "no engine" {
				opts = append(opts, WithPermissionContext(permission.NewContext(permission.ModeDefault)))
			}
			if kind == "bounded width-one batch" {
				opts = append(opts, WithToolConcurrencyLimit(1))
			}
			if kind == "bounded parallel" {
				opts = append(opts, WithToolConcurrencyLimit(2))
			}
			if kind == "external" {
				target.external = true
			}
			var state *AgentState
			if kind == "restored pending" || kind == "restored allowed" || kind == "restored asking" || kind == "reprompt asking" || kind == "restored submitted" {
				tc := hookCall("old")
				if kind == "restored allowed" {
					tc.State = message.ToolCallAllowed
				}
				if kind == "restored asking" || kind == "reprompt asking" {
					tc.State = message.ToolCallAsking
				}
				state = &AgentState{SessionID: "session", ReplyID: "logical", Context: []*message.Msg{message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{tc})}, ReplyRecovery: &ReplyRecoveryState{Version: 1, ReplyID: "logical", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1}}}
				if kind == "restored submitted" {
					tc.State = message.ToolCallSubmitted
					target.external = true
					state.Context[0].Content = []message.ContentBlock{tc}
				}
				state.Context[0].ID = "logical"
				if kind == "reprompt asking" {
					opts = append(opts, WithState(state))
				} else {
					opts = append(opts, WithState(state), WithStateSaver(newFakeStateSaver()), WithReplyRecovery())
				}
				calls = nil
			}
			if kind == "resume new call" {
				state = &AgentState{SessionID: "session", ReplyID: "logical", Context: []*message.Msg{message.UserMsg("user", "run")}, ReplyRecovery: &ReplyRecoveryState{Version: 1, ReplyID: "logical", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1}}}
				opts = append(opts, WithState(state), WithStateSaver(newFakeStateSaver()), WithReplyRecovery())
			}
			mock := hookModel(calls...)
			if len(calls) == 0 {
				mock = &mockChatModel{responses: []model.ChatResponse{{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}}}
			}
			a := NewUnifiedAgent("bot", "prompt", mock, opts...)
			switch kind {
			case "loop":
				_, err := loop.New(NewUnifiedAgentRunner(a).LoopOptions()...).RunSync(context.Background(), "run")
				if err != nil {
					t.Fatal(err)
				}
			case "buffered reply":
				_, err := a.Reply(context.Background(), "run")
				if err != nil {
					t.Fatal(err)
				}
			case "orchestrator":
				_, err := tool.NewOrchestrator(tool.OrchestratorConfig{Toolkit: a.toolkit, PermEngine: a.engine}).Execute(context.Background(), hookCall("direct"))
				if err != nil {
					t.Fatal(err)
				}
			default:
				drainHookReply(t, a, kind == "resume new call" || kind == "restored pending" || kind == "restored allowed" || kind == "restored asking" || kind == "restored submitted")
			}
			expected := int32(1)
			if kind == "parallel" || kind == "bounded parallel" || kind == "bounded width-one batch" {
				expected = 2
			}
			if kind == "external" || kind == "reprompt asking" || kind == "restored submitted" {
				expected = 0
			}
			if target.executions.Load() != expected {
				t.Fatalf("excluded path %s executed %d times, want %d", kind, target.executions.Load(), expected)
			}
			if hooks.Load() != 0 {
				t.Fatalf("excluded %s invoked hook %d times", kind, hooks.Load())
			}
		})
	}
}

func TestPermissionHookMetadataSnapshot(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(fmt.Sprintf("mutate=%v", mutate), func(t *testing.T) {
			target := hookTool()
			tc := hookCall("metadata")
			tc.Input = `{"number":1.25}`
			tc.Extra = map[string]any{"raw": json.RawMessage(`{"z":1,"a":2}`), "large": int64(9007199254740993), "nested": map[string]any{"value": "original"}}
			var calls int
			mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
				calls++
				if in.ToolInput["number"] != float64(1.25) {
					t.Errorf("parsed argument type changed: %T", in.ToolInput["number"])
				}
				if n, ok := in.ToolCall.Extra["large"].(json.Number); !ok || n.String() != "9007199254740993" {
					t.Error("metadata integer lost precision")
				}
				if mutate {
					in.ToolCall.Extra["nested"].(map[string]any)["value"] = "changed"
				}
				return next(ctx, in)
			}}
			a := NewUnifiedAgent("bot", "prompt", hookModel(tc), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mw))
			drainHookReply(t, a, false)
			want := int32(1)
			if mutate {
				want = 0
			}
			if calls != 1 || target.executions.Load() != want {
				t.Fatalf("calls=%d execution=%d", calls, target.executions.Load())
			}
			if tc.Extra["nested"].(map[string]any)["value"] != "original" {
				t.Fatal("snapshot mutation reached original metadata")
			}
		})
	}
}

func TestPermissionHookDeterministicResults(t *testing.T) {
	for _, behavior := range []permission.PermissionBehavior{permission.BehaviorDeny, permission.BehaviorAsk} {
		t.Run(string(behavior), func(t *testing.T) {
			target := hookTool()
			var seen permission.PermissionBehavior
			mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
				d, e := next(ctx, in)
				if d != nil {
					seen = d.Behavior
				}
				return d, e
			}}
			pctx := permission.NewContext(permission.ModeDefault)
			engine := permission.NewEngine(pctx)
			engine.AddRule(permission.Rule{ToolName: "checked", Behavior: behavior})
			a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("rules")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(pctx), WithMiddlewares(mw))
			asks := drainHookReply(t, a, false)
			if seen != behavior || target.executions.Load() != 0 || (behavior == permission.BehaviorAsk && asks != 1) {
				t.Fatalf("seen=%s asks=%d executions=%d", seen, asks, target.executions.Load())
			}
		})
	}
}

func TestPermissionHookBoundedSingle(t *testing.T) {
	target := hookTool()
	var calls int
	mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
		calls++
		return next(ctx, in)
	}}
	a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("single")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mw), WithToolConcurrencyLimit(1))
	drainHookReply(t, a, false)
	if calls != 1 || target.executions.Load() != 1 {
		t.Fatalf("hook=%d executions=%d", calls, target.executions.Load())
	}
}

func TestPermissionHookConversation(t *testing.T) {
	target := hookTool()
	var calls atomic.Int32
	mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
		calls.Add(1)
		return next(ctx, in)
	}}
	a := NewUnifiedAgent("bot", "prompt", hookModel(hookCall("conversation")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mw))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conv, err := a.NewConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	if err := conv.Submit("run"); err != nil {
		t.Fatal(err)
	}
	convWait(t, conv, "reply end", replyEndWithReason(types.ReplyCompleted))
	if calls.Load() != 1 || target.executions.Load() != 1 {
		t.Fatalf("hook=%d executions=%d", calls.Load(), target.executions.Load())
	}
}

func TestPermissionHookOutsideEngineLock(t *testing.T) {
	target := hookTool()
	var a *UnifiedAgent
	mw := &permissionHook{fn: func(ctx context.Context, in *middleware.CheckPermissionInput, next middleware.CheckPermissionHandler) (*permission.Decision, error) {
		d, e := next(ctx, in)
		a.engine.AddRule(permission.Rule{ToolName: "unrelated", Behavior: permission.BehaviorDeny})
		return d, e
	}}
	a = NewUnifiedAgent("bot", "prompt", hookModel(hookCall("lock")), WithToolkit(tool.NewToolkit(target)), WithPermissionContext(permission.NewContext(permission.ModeDefault)), WithMiddlewares(mw))
	drainHookReply(t, a, false)
	if target.executions.Load() != 1 || len(a.engine.SnapshotContext().DenyRules["unrelated"]) != 1 {
		t.Fatal("hook did not run outside engine lock")
	}
}
