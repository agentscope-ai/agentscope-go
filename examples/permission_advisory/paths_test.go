package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type pathModel struct {
	scriptedModel
	callsOut []message.ToolCallBlock
}

func (m *pathModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	out, err := m.scriptedModel.Chat(ctx, msgs, opts...)
	if m.calls == 1 {
		out.Content = nil
		for _, c := range m.callsOut {
			out.Content = append(out.Content, c)
		}
	}
	return out, err
}

type pathTool struct {
	*syntheticTool
	external bool
	allow    bool
}

func (t *pathTool) CheckPermissions(input map[string]any, p *permission.Context) permission.Decision {
	if t.allow {
		return permission.Decision{Behavior: permission.BehaviorAllow}
	}
	return t.syntheticTool.CheckPermissions(input, p)
}
func (t *pathTool) IsExternalTool() bool { return t.external }

// A saver is required to enable recovery; no test loads from persistence.
type pathSaver struct{}

func (pathSaver) SaveState(context.Context, string, *agent.AgentState) error   { return nil }
func (pathSaver) LoadState(context.Context, string) (*agent.AgentState, error) { return nil, io.EOF }
func (pathSaver) ListSessions(context.Context) ([]agent.SessionInfo, error)    { return nil, nil }
func (pathSaver) DeleteSession(context.Context, string) error                  { return nil }

func TestExcludedAgentPathsNeverClassify(t *testing.T) {
	for _, kind := range []string{"no engine", "buffered", "resume new", "restored pending", "restored allowed", "restored asking", "reprompt asking", "restored submitted", "external", "parallel", "bounded", "width one", "loop", "orchestrator"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			target := &pathTool{syntheticTool: newSyntheticTool()}
			target.ConcurrencySafe = true
			target.allow = kind == "buffered" || kind == "parallel" || kind == "bounded" || kind == "width one"
			c := &testClassifier{}
			p := newPolicy(target)
			r, err := newReviewer(c, p, io.Discard, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			call := message.ToolCallBlock{Type: "tool_call", ID: "one", Name: "read_fixture", Input: `{"target":"fixture.csv"}`, State: message.ToolCallPending}
			calls := []message.ToolCallBlock{call}
			if kind == "parallel" || kind == "bounded" || kind == "width one" {
				other := call
				other.ID = "two"
				calls = append(calls, other)
			}
			kit := tool.NewToolkit(target)
			opts := []agent.AgentOption{agent.WithToolkit(kit), agent.WithMiddlewares(r)}
			if kind != "no engine" {
				opts = append(opts, agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)))
			}
			if kind == "width one" {
				opts = append(opts, agent.WithToolConcurrencyLimit(1))
			}
			if kind == "bounded" {
				opts = append(opts, agent.WithToolConcurrencyLimit(2))
			}
			if kind == "external" || kind == "restored submitted" {
				target.external = true
			}
			resume := false
			switch kind {
			case "resume new", "restored pending", "restored allowed", "restored asking", "reprompt asking", "restored submitted":
				state := &agent.AgentState{SessionID: "fixture", ReplyID: "old", ReplyRecovery: &agent.ReplyRecoveryState{Version: 1, ReplyID: "old", Active: true, Budgets: middleware.ReplyBudgetSnapshot{Version: 1}}}
				if kind == "resume new" {
					state.Context = []*message.Msg{message.UserMsg("user", demoIntent)}
				} else {
					switch kind {
					case "restored allowed":
						call.State = message.ToolCallAllowed
					case "restored asking", "reprompt asking":
						call.State = message.ToolCallAsking
					case "restored submitted":
						call.State = message.ToolCallSubmitted
					}
					msg := message.NewMsg("demo", message.RoleAssistant, []message.ContentBlock{call})
					msg.ID = "old"
					state.Context = []*message.Msg{msg}
					calls = nil
				}
				opts = append(opts, agent.WithState(state))
				if kind != "reprompt asking" {
					opts = append(opts, agent.WithStateSaver(pathSaver{}), agent.WithReplyRecovery())
					resume = true
				}
			}
			a := agent.NewUnifiedAgent("demo", "synthetic", &pathModel{callsOut: calls}, opts...)
			p.engine = a.PermissionEngine()
			switch kind {
			case "buffered":
				_, err = a.Reply(ctx, demoIntent)
			case "loop":
				_, err = loop.New(agent.NewUnifiedAgentRunner(a).LoopOptions()...).RunSync(ctx, demoIntent)
			case "orchestrator":
				_, err = tool.NewOrchestrator(tool.OrchestratorConfig{Toolkit: kit, PermEngine: p.engine}).Execute(ctx, call)
			default:
				var stream <-chan event.Event
				if resume {
					stream, err = a.ResumeReplyStream(ctx)
				} else {
					stream, err = a.ReplyStream(ctx, demoIntent)
				}
				if err == nil {
					for e := range stream {
						switch ev := e.(type) {
						case event.RequireUserConfirmEvent:
							var answers []event.ConfirmResult
							for _, tc := range ev.ToolCalls {
								answers = append(answers, event.ConfirmResult{ToolCall: tc, Confirmed: false})
							}
							a.SubmitUserConfirm(&event.UserConfirmResultEvent{ConfirmResults: answers})
						case event.RequireExternalExecutionEvent:
							cancel()
						}
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.calls.Load() != 0 {
				t.Fatalf("excluded path dispatched classifier %d times", c.calls.Load())
			}
			expected := int32(0)
			if kind == "no engine" || kind == "buffered" {
				expected = 1
			}
			if kind == "parallel" || kind == "bounded" || kind == "width one" {
				expected = 2
			}
			if target.executions.Load() != expected {
				t.Fatalf("executions=%d, want %d", target.executions.Load(), expected)
			}
		})
	}
}
