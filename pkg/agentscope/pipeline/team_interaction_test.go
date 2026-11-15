package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type teamExternalFixture struct{ tool.BaseTool }

func (*teamExternalFixture) IsExternalTool() bool { return true }

func TestTeamParticipantRoutingAndOwnedSubmissions(t *testing.T) {
	var callsB atomic.Int32
	var workerA *agent.UnifiedAgent
	var confirmed, externalCount int
	lookup := tool.NewFunctionTool("confirm", "fixture", nil, func(context.Context, map[string]any) (any, error) { confirmed++; return "PRIVATE_TOOL_RESULT", nil })
	a := TeamMember{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		n := 0
		m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
			n++
			if n == 1 {
				return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "EARLY_PRIVATE_NARRATIVE"}, teamCall("same", "confirm", `{}`)}}, nil
			}
			return teamText("A final"), nil
		}}
		workerA = agent.NewUnifiedAgent("A", "fixture", m, append(teamOptions(lookup), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)))...)
		return workerA, nil
	}}
	b := teamMember("B", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		if callsB.Add(1) == 1 {
			return &model.ChatResponse{Content: []message.ContentBlock{teamCall("same", "host_lookup", `{}`)}}, nil
		}
		return teamText("B final"), nil
	}, agent.WithToolkit(tool.NewToolkit(&teamExternalFixture{tool.BaseTool{ToolName: "host_lookup"}})))
	leader := teamLeader([]message.ToolCallBlock{teamCall("a", TeamAssignToolName, `{"member":"A","prompt":"task"}`), teamCall("b", TeamAssignToolName, `{"member":"B","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
		if len(results) != 2 || results[0].GetOutputText() != "A final" || results[1].GetOutputText() != "B final" {
			return fmt.Errorf("wrong final results %v", results)
		}
		return nil
	})
	p, _ := NewTeamPipeline(leader, a, b)
	var lastConfirm event.UserConfirmResultEvent
	events, err := teamDrain(t, p, func(r *TeamRun, ev event.Event) {
		switch e := ev.(type) {
		case event.RequireUserConfirmEvent:
			if callsB.Load() != 0 {
				t.Error("later member started before confirmation")
			}
			bad := event.UserConfirmResultEvent{ReplyID: "wrong participant", ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: e.ToolCalls[0]}}}
			if !errors.Is(r.SubmitUserConfirm(e.GetEventID(), &bad), ErrTeamInput) {
				t.Error("wrong reply accepted")
			}
			good := event.UserConfirmResultEvent{ReplyID: e.ReplyID, ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: e.ToolCalls[0], Rules: []any{permission.Rule{ToolName: "confirm", Behavior: permission.BehaviorAllow, Source: "user"}}}}}
			bad = good
			bad.ConfirmResults = append([]event.ConfirmResult(nil), good.ConfirmResults...)
			bad.ConfirmResults[0].ToolCall.Name = "other tool"
			if !errors.Is(r.SubmitUserConfirm(e.GetEventID(), &bad), ErrTeamInput) {
				t.Error("edited tool identity accepted")
			}
			if err := r.SubmitUserConfirm(e.GetEventID(), &good); err != nil {
				t.Error(err)
			}
			lastConfirm = good
			good.ConfirmResults[0].Rules[0] = permission.Rule{ToolName: "mutated", Behavior: permission.BehaviorDeny}
			if !errors.Is(r.SubmitUserConfirm(e.GetEventID(), &lastConfirm), ErrTeamInput) {
				t.Error("duplicate confirm accepted")
			}
		case event.RequireExternalExecutionEvent:
			externalCount++
			if !errors.Is(r.SubmitUserConfirm(e.GetEventID(), &lastConfirm), ErrTeamInput) {
				t.Error("stale previous participant accepted")
			}
			if err := r.SubmitExternalResult(e.GetEventID(), &event.ExternalExecutionResultEvent{ReplyID: e.ReplyID, ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: e.ToolCalls[0].ID, Name: e.ToolCalls[0].Name, State: message.ToolResultSuccess, Output: "PRIVATE_HOST_RESULT", Metadata: map[string]any{"n": []any{1}}}}}); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil || confirmed != 1 || externalCount != 1 {
		t.Fatalf("err=%v confirmed=%d external=%d", err, confirmed, externalCount)
	}
	if len(workerA.PermissionEngine().Context.AllowRules["confirm"]) == 0 {
		t.Fatal("typed host permission.Rule was lost")
	}
	for _, ev := range events {
		if e, ok := ev.(event.ToolResultTextDeltaEvent); ok && e.ReplyID != "" && e.ToolCallID == "a" && e.Delta != "A final" {
			t.Fatalf("intermediate member text leaked %q", e.Delta)
		}
	}
}

func TestTeamOrdinaryLeaderExternalInputValidation(t *testing.T) {
	ext := &teamExternalFixture{tool.BaseTool{ToolName: "host"}}
	p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("host-call", "host", `{}`)}, func(results []message.ToolResultBlock) error {
		if len(results) != 1 || results[0].Metadata["value"].(map[string]any)["n"] != 3 {
			return fmt.Errorf("submission copy changed: %v", results)
		}
		return nil
	}, ext), teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return teamText("unused"), nil
	}))
	var saved *TeamRun
	_, err := teamDrain(t, p, func(r *TeamRun, ev event.Event) {
		e, ok := ev.(event.RequireExternalExecutionEvent)
		if !ok {
			return
		}
		saved = r
		base := event.ExternalExecutionResultEvent{ReplyID: e.ReplyID, ExecutionResults: []message.ToolResultBlock{{Type: "tool_result", ID: "host-call", Name: "host", State: message.ToolResultSuccess, Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "host result"}}, Metadata: map[string]any{"value": map[string]any{"n": 3}}}}}
		if !errors.Is(r.SubmitExternalResult(e.GetEventID(), nil), ErrTeamInput) || !errors.Is(r.SubmitUserConfirm(e.GetEventID(), nil), ErrTeamInput) {
			t.Error("nil input accepted")
		}
		for _, mutate := range []func(*event.ExternalExecutionResultEvent){
			func(v *event.ExternalExecutionResultEvent) { v.ReplyID = "stale" },
			func(v *event.ExternalExecutionResultEvent) { v.ExecutionResults = nil },
			func(v *event.ExternalExecutionResultEvent) { v.ExecutionResults[0].ID = "unknown" },
			func(v *event.ExternalExecutionResultEvent) { v.ExecutionResults[0].Name = "wrong" },
			func(v *event.ExternalExecutionResultEvent) { v.ExecutionResults[0].State = "bogus" },
			func(v *event.ExternalExecutionResultEvent) {
				v.ExecutionResults = append(v.ExecutionResults, v.ExecutionResults[0])
			},
			func(v *event.ExternalExecutionResultEvent) { v.ExecutionResults[0].Output = func() {} },
			func(v *event.ExternalExecutionResultEvent) {
				v.ExecutionResults[0].Metadata = map[string]any{"bad": map[int]string{1: "x"}}
			},
		} {
			bad, _ := cloneTeamInput(base)
			mutate(&bad)
			if !errors.Is(r.SubmitExternalResult(e.GetEventID(), &bad), ErrTeamInput) {
				t.Error("invalid external input accepted")
			}
		}
		if err := r.SubmitExternalResult(e.GetEventID(), &base); err != nil {
			t.Error(err)
		}
		base.ExecutionResults[0].Metadata["value"].(map[string]any)["n"] = 9
		if !errors.Is(r.SubmitExternalResult(e.GetEventID(), &base), ErrTeamInput) {
			t.Error("duplicate external result accepted")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(saved.SubmitExternalResult("", &event.ExternalExecutionResultEvent{}), ErrTeamClosed) {
		t.Fatal("closed run accepted input")
	}
}

func TestTeamCancellationWaitsForActualMemberCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var memberCalls atomic.Int32
	hold := tool.NewFunctionTool("hold", "fixture ignores context", nil, func(context.Context, map[string]any) (any, error) { close(entered); <-release; return "returned", nil })
	member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		memberCalls.Add(1)
		return &model.ChatResponse{Content: []message.ContentBlock{teamCall("hold", "hold", `{}`)}}, nil
	}, agent.WithToolkit(tool.NewToolkit(hold)))
	p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("first", TeamAssignToolName, `{"member":"A","prompt":"hold"}`), teamCall("later", TeamAssignToolName, `{"member":"A","prompt":"must not start"}`)}, nil), member)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := p.Start(ctx, "delegate")
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		for range r.Events() {
		}
		close(drained)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("member did not start")
	}
	r.Cancel()
	if _, err := p.Start(context.Background(), "other run"); !errors.Is(err, ErrTeamBusy) {
		t.Fatalf("ownership released before cleanup: %v", err)
	}
	if !errors.Is(r.SubmitUserConfirm("", &event.UserConfirmResultEvent{}), ErrTeamClosed) {
		t.Error("canceled run accepted input")
	}
	select {
	case <-r.Done():
		t.Error("run completed before held tool returned")
	default:
	}
	once.Do(func() { close(release) })
	if !errors.Is(r.Wait(), context.Canceled) {
		t.Fatal("cancellation was successful")
	}
	<-drained
	if memberCalls.Load() != 1 {
		t.Fatalf("later member work admitted: %d", memberCalls.Load())
	}
}

func TestTeamConcurrentDuplicateResponsesDoNotBlock(t *testing.T) {
	ext := &teamExternalFixture{tool.BaseTool{ToolName: "host"}}
	p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("call", "host", `{}`)}, nil, ext), teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return teamText("unused"), nil
	}))
	accepted := atomic.Int32{}
	_, err := teamDrain(t, p, func(r *TeamRun, ev event.Event) {
		e, ok := ev.(event.RequireExternalExecutionEvent)
		if !ok {
			return
		}
		response := event.ExternalExecutionResultEvent{ReplyID: e.ReplyID, ExecutionResults: []message.ToolResultBlock{{ID: "call", Name: "host", State: message.ToolResultSuccess, Output: "done"}}}
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r.SubmitExternalResult(e.GetEventID(), &response) == nil {
					accepted.Add(1)
				}
			}()
		}
		wg.Wait()
	})
	if err != nil || accepted.Load() != 1 {
		t.Fatalf("err=%v accepted=%d", err, accepted.Load())
	}
}

func TestTeamRequestGenerationSeparatesToolApprovalFromDirectoryGrant(t *testing.T) {
	for _, approveGrant := range []bool{false, true} {
		t.Run(fmt.Sprintf("grant=%v", approveGrant), func(t *testing.T) {
			var leaderAgent *agent.UnifiedAgent
			attempts := 0
			path := t.TempDir() + "/new-directory/file"
			operation := tool.NewFunctionTool("inspect", "fixture reports an escape", nil, func(context.Context, map[string]any) (any, error) {
				attempts++
				if attempts == 1 {
					return nil, &tool.WorkspaceEscapeError{Path: path, Root: "/fixture/workspace"}
				}
				return "after explicit directory grant", nil
			})
			leader := func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
				tk, err := NewTeamLeaderToolkit(assign, operation)
				if err != nil {
					return nil, err
				}
				n := 0
				m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
					n++
					if n == 1 {
						return &model.ChatResponse{Content: []message.ContentBlock{teamCall("same-operation", "inspect", `{}`)}}, nil
					}
					return teamText("done"), nil
				}}
				leaderAgent = agent.NewUnifiedAgent("leader", "fixture", m, append(teamOptions(), agent.WithToolkit(tk), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)))...)
				return leaderAgent, nil
			}
			p, _ := NewTeamPipeline(leader, teamMember("A", nil))
			count := 0
			var old event.UserConfirmResultEvent
			var oldRequest string
			_, err := teamDrain(t, p, func(r *TeamRun, ev event.Event) {
				e, ok := ev.(event.RequireUserConfirmEvent)
				if !ok {
					return
				}
				count++
				if count == 1 {
					oldRequest = e.GetEventID()
					old = event.UserConfirmResultEvent{ReplyID: e.ReplyID, ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: e.ToolCalls[0]}}}
					if err := r.SubmitUserConfirm(oldRequest, &old); err != nil {
						t.Error(err)
					}
					return
				}
				if count == 2 {
					if oldRequest == e.GetEventID() || old.ReplyID != e.ReplyID || old.ConfirmResults[0].ToolCall.ID != e.ToolCalls[0].ID {
						t.Error("fixture did not exercise same-call, new-request semantics")
					}
					if err := r.SubmitUserConfirm(oldRequest, &old); !errors.Is(err, ErrTeamInput) {
						t.Errorf("stale tool approval accepted for directory grant: %v", err)
					}
					fresh := event.UserConfirmResultEvent{ReplyID: e.ReplyID, ConfirmResults: []event.ConfirmResult{{Confirmed: approveGrant, ToolCall: e.ToolCalls[0]}}}
					if err := r.SubmitUserConfirm(e.GetEventID(), &fresh); err != nil {
						t.Error(err)
					}
				}
			})
			if err != nil || count != 2 {
				t.Fatalf("err=%v requests=%d", err, count)
			}
			grants := len(leaderAgent.PermissionEngine().Context.WorkingDirectories)
			if approveGrant {
				if grants != 1 || attempts != 2 {
					t.Fatalf("grant=%d attempts=%d", grants, attempts)
				}
			} else if grants != 0 || attempts != 1 {
				t.Fatalf("stale approval granted access: grants=%d attempts=%d", grants, attempts)
			}
		})
	}
}

func TestTeamCancellationWhileHostInputIsPending(t *testing.T) {
	ext := &teamExternalFixture{tool.BaseTool{ToolName: "host"}}
	p, _ := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("pending", "host", `{}`)}, nil, ext), teamMember("A", nil))
	_, err := teamDrain(t, p, func(r *TeamRun, ev event.Event) {
		if _, ok := ev.(event.RequireExternalExecutionEvent); ok {
			r.Cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTeamLeaderAssignmentPermissionRemainsHostOwned(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(fmt.Sprintf("approve=%v", approve), func(t *testing.T) {
			var memberCalls atomic.Int32
			leader := func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
				n := 0
				m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
					n++
					if n == 1 {
						return &model.ChatResponse{Content: []message.ContentBlock{teamCall("assign", TeamAssignToolName, `{"member":"A","prompt":"work"}`)}}, nil
					}
					return teamText("done"), nil
				}}
				return agent.NewUnifiedAgent("leader", "fixture", m, append(teamOptions(assign), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)))...), nil
			}
			member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				memberCalls.Add(1)
				return teamText("A"), nil
			})
			p, _ := NewTeamPipeline(leader, member)
			requests := 0
			_, err := teamDrain(t, p, func(r *TeamRun, ev event.Event) {
				if e, ok := ev.(event.RequireUserConfirmEvent); ok {
					requests++
					if err := r.SubmitUserConfirm(e.GetEventID(), &event.UserConfirmResultEvent{ReplyID: e.ReplyID, ConfirmResults: []event.ConfirmResult{{Confirmed: approve, ToolCall: e.ToolCalls[0]}}}); err != nil {
						t.Error(err)
					}
				}
			})
			want := int32(0)
			if approve {
				want = 1
			}
			if err != nil || requests != 1 || memberCalls.Load() != want {
				t.Fatalf("err=%v requests=%d member calls=%d", err, requests, memberCalls.Load())
			}
		})
	}
}
