package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func teamText(s string) *model.ChatResponse {
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: s}}, IsLast: true}
}

func teamLeader(calls []message.ToolCallBlock, inspect func([]message.ToolResultBlock) error, others ...tool.Tool) TeamLeaderFactory {
	return func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
		tk, err := NewTeamLeaderToolkit(assign, others...)
		if err != nil {
			return nil, err
		}
		n := 0
		m := &goalFixtureModel{call: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
			n++
			if n == 1 && len(calls) > 0 {
				blocks := make([]message.ContentBlock, len(calls))
				for i, c := range calls {
					blocks[i] = c
				}
				return &model.ChatResponse{Content: blocks}, nil
			}
			var results []message.ToolResultBlock
			for _, msg := range msgs {
				for _, block := range msg.Content {
					if tr, ok := block.(message.ToolResultBlock); ok {
						results = append(results, tr)
					}
				}
			}
			if inspect != nil {
				if err := inspect(results); err != nil {
					return nil, err
				}
			}
			return teamText("team complete"), nil
		}}
		return agent.NewUnifiedAgent("leader", "fixture", m, append(teamOptions(), agent.WithToolkit(tk))...), nil
	}
}

func teamMember(name string, respond func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error), extra ...agent.AgentOption) TeamMember {
	return TeamMember{Name: name, Description: "fixture specialist", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		return agent.NewUnifiedAgent(name, "fixture", &goalFixtureModel{call: respond}, append(teamOptions(), extra...)...), nil
	}}
}

func teamDrain(t *testing.T, p *TeamPipeline, host func(*TeamRun, event.Event)) ([]event.Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := p.Start(ctx, "delegate")
	if err != nil {
		return nil, err
	}
	defer r.Cancel()
	var events []event.Event
	for ev := range r.Events() {
		events = append(events, ev)
		if host != nil {
			host(r, ev)
		}
	}
	return events, r.Wait()
}

func TestTeamFactoriesAreFreshAndConfigurationOwned(t *testing.T) {
	created := 0
	members := []TeamMember{{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		created++
		m := &goalFixtureModel{call: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
			n := 0
			for _, msg := range msgs {
				if msg.Role == message.RoleUser {
					n++
				}
			}
			return teamText(fmt.Sprintf("users=%d", n)), nil
		}}
		return agent.NewUnifiedAgent("A", "fixture", m, teamOptions()...), nil
	}}}
	p, err := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("a", TeamAssignToolName, `{"member":"A","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
		if len(results) != 1 || results[0].GetOutputText() != "users=1" {
			return fmt.Errorf("reused member history: %v", results)
		}
		return nil
	}), members...)
	if err != nil {
		t.Fatal(err)
	}
	members[0].Name = "caller changed configuration"
	for range 2 {
		if _, err := teamDrain(t, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	if created != 2 {
		t.Fatal(created)
	}
}

func TestTeamConstructionAndStartErrors(t *testing.T) {
	member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return teamText("ok"), nil
	})
	for _, tc := range []struct {
		leader  TeamLeaderFactory
		members []TeamMember
	}{
		{nil, []TeamMember{member}}, {teamLeader(nil, nil), nil}, {teamLeader(nil, nil), []TeamMember{{Name: " "}}}, {teamLeader(nil, nil), []TeamMember{member, member}},
	} {
		if _, err := NewTeamPipeline(tc.leader, tc.members...); err == nil {
			t.Fatal("invalid constructor accepted")
		}
	}
	baseLeader := teamLeader(nil, nil)
	for _, tc := range []struct {
		name   string
		leader TeamLeaderFactory
		member TeamMember
	}{
		{"leader error", func(context.Context, tool.Tool) (*agent.UnifiedAgent, error) {
			return nil, fmt.Errorf("factory failed")
		}, member},
		{"nil leader", func(context.Context, tool.Tool) (*agent.UnifiedAgent, error) { return nil, nil }, member},
		{"leader without permission channels", func(context.Context, tool.Tool) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("leader", "fixture", &goalFixtureModel{}), nil
		}, member},
		{"leader name conflict", func(context.Context, tool.Tool) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("A", "fixture", &goalFixtureModel{}, teamOptions()...), nil
		}, member},
		{"member error", baseLeader, TeamMember{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) { return nil, fmt.Errorf("factory failed") }}},
		{"nil member", baseLeader, TeamMember{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) { return nil, nil }}},
		{"wrong member name", baseLeader, TeamMember{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("wrong", "fixture", &goalFixtureModel{}, teamOptions()...), nil
		}}},
		{"member without permission channels", baseLeader, TeamMember{Name: "A", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("A", "fixture", &goalFixtureModel{}), nil
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := NewTeamPipeline(tc.leader, tc.member)
			if _, err := p.Start(context.Background(), "task"); err == nil {
				t.Fatal("bad factory accepted")
			}
			if p.busy.Load() {
				t.Fatal("failed construction retained ownership")
			}
		})
	}
	p, _ := NewTeamPipeline(baseLeader, member)
	if _, err := p.Start(nil, "task"); err == nil { //nolint:staticcheck // Exercise the explicit nil-context rejection contract.
		t.Fatal("nil context accepted")
	}
	if _, err := p.Start(context.Background(), " "); err == nil {
		t.Fatal("empty task accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Start(ctx, "task"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, scope := runctx.New(context.Background())
	scope.Close()
	if _, err := p.Start(ctx, "task"); !errors.Is(err, ErrTeamClosed) {
		t.Fatal(err)
	}
	var nilPipeline *TeamPipeline
	if _, err := nilPipeline.Start(context.Background(), "task"); err == nil {
		t.Fatal("nil pipeline accepted")
	}
	var run *TeamRun
	run.Cancel()
	if run.Events() != nil || run.Done() != nil || !errors.Is(run.Wait(), ErrTeamClosed) {
		t.Fatal("nil run methods")
	}
}

func TestTeamToolkitRejectsReservedAndDuplicateTools(t *testing.T) {
	assign := newTeamAssign([]TeamMember{{Name: "A"}})
	ordinary := tool.NewFunctionTool("ordinary", "fixture", nil, nil)
	var typedNil *tool.FunctionTool
	for _, tc := range []struct {
		assign tool.Tool
		others []tool.Tool
	}{
		{nil, nil}, {ordinary, nil}, {assign, []tool.Tool{assign}}, {assign, []tool.Tool{nil}}, {assign, []tool.Tool{typedNil}}, {assign, []tool.Tool{ordinary, ordinary}},
	} {
		if _, err := NewTeamLeaderToolkit(tc.assign, tc.others...); err == nil {
			t.Fatal("invalid toolkit accepted")
		}
	}
	tk, err := NewTeamLeaderToolkit(assign, ordinary)
	if err != nil || tk.Get(TeamAssignToolName) != assign || tk.Get("ordinary") != ordinary {
		t.Fatalf("toolkit=%v err=%v", tk, err)
	}
}
func teamCall(id, name, input string) message.ToolCallBlock {
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: name, Input: input, State: message.ToolCallPending}
}
func teamOptions(tools ...tool.Tool) []agent.AgentOption {
	return []agent.AgentOption{agent.WithToolkit(tool.NewToolkit(tools...)), agent.WithModelStreaming(false), agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)), agent.WithReactConfig(agent.ReactConfig{MaxIters: 8})}
}

func TestTeamAssignRejectsInvalidOrCanceledRequests(t *testing.T) {
	for _, tc := range []struct {
		name, input, wantOutput string
		canceled                bool
		wantState               message.ToolResultState
	}{
		{name: "malformed JSON", input: `{"member":`, wantState: message.ToolResultError, wantOutput: "invalid TeamAssign input"},
		{name: "unknown member", input: `{"member":"missing","prompt":"task"}`, wantState: message.ToolResultError, wantOutput: "unknown member or empty assignment prompt"},
		{name: "empty prompt", input: `{"member":"A","prompt":""}`, wantState: message.ToolResultError, wantOutput: "unknown member or empty assignment prompt"},
		{name: "whitespace prompt", input: `{"member":"A","prompt":" \t\n"}`, wantState: message.ToolResultError, wantOutput: "unknown member or empty assignment prompt"},
		{name: "canceled before assignment", input: `{"member":"A","prompt":"task"}`, canceled: true, wantState: message.ToolResultInterrupted, wantOutput: "team canceled before assignment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			calls := 0
			member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				calls++
				return teamText("unexpected member execution"), nil
			})
			worker, err := member.Factory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r := newTeamRun(ctx, cancel, map[string]*agent.UnifiedAgent{"A": worker})
			if tc.canceled {
				cancel()
			}
			call := teamCall("rejected-assignment", TeamAssignToolName, tc.input)
			result := r.assign(ctx, &call)
			if result.Type != "tool_result" || result.ID != call.ID || result.Name != call.Name {
				t.Fatalf("assignment result lost call identity: %+v", result)
			}
			if result.State != tc.wantState || result.GetOutputText() != tc.wantOutput {
				t.Fatalf("unexpected assignment result: %+v", result)
			}
			if calls != 0 {
				t.Fatalf("rejected assignment invoked member model %d times", calls)
			}
		})
	}
}

func TestTeamUnknownMemberRejectedBeforeDelegation(t *testing.T) {
	calls := 0
	member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		calls++
		return teamText("unexpected member execution"), nil
	})
	p, err := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("unknown", TeamAssignToolName, `{"member":"missing","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
		if len(results) != 1 || results[0].ID != "unknown" || results[0].Name != TeamAssignToolName || results[0].State != message.ToolResultError {
			return fmt.Errorf("unknown-member error was not returned to leader: %v", results)
		}
		if output := results[0].GetOutputText(); !strings.Contains(output, `field "member"`) || !strings.Contains(output, "not one of the allowed values") {
			return fmt.Errorf("unknown member bypassed assignment schema: %q", output)
		}
		return nil
	}), member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamDrain(t, p, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("unknown assignment invoked registered member %d times", calls)
	}
}

func TestTeamRoundTripRetainsNamedMemberHistory(t *testing.T) {
	var order []string
	leader := func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
		n := 0
		m := &goalFixtureModel{call: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
			n++
			if n == 1 {
				return &model.ChatResponse{Content: []message.ContentBlock{teamCall("a1", TeamAssignToolName, `{"member":"A","prompt":"first"}`), teamCall("b1", TeamAssignToolName, `{"member":"B","prompt":"independent"}`), teamCall("a2", TeamAssignToolName, `{"member":"A","prompt":"followup"}`)}}, nil
			}
			var results []string
			for _, msg := range msgs {
				for _, block := range msg.Content {
					if tr, ok := block.(message.ToolResultBlock); ok {
						results = append(results, tr.GetOutputText())
					}
				}
			}
			if strings.Join(results, ",") != "A:1,B:1,A:2" {
				return nil, fmt.Errorf("member results: %v", results)
			}
			return teamText("team complete"), nil
		}}
		return agent.NewUnifiedAgent("leader", "fixture", m, teamOptions(assign)...), nil
	}
	members := []TeamMember{}
	for _, name := range []string{"A", "B"} {
		members = append(members, TeamMember{Name: name, Description: "specialist", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
			m := &goalFixtureModel{call: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
				order = append(order, name)
				n := 0
				for _, msg := range msgs {
					if msg.Role == message.RoleUser {
						n++
					}
				}
				return teamText(fmt.Sprintf("%s:%d", name, n)), nil
			}}
			return agent.NewUnifiedAgent(name, "fixture", m, teamOptions()...), nil
		}})
	}
	p, err := NewTeamPipeline(leader, members...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := p.Start(ctx, "delegate")
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for ev := range run.Events() {
		if e, ok := ev.(event.TextBlockDeltaEvent); ok {
			last = e.Delta
		}
	}
	if err := run.Wait(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "A,B,A" || last != "team complete" {
		t.Fatalf("order=%v last=%q", order, last)
	}
}
