package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// handoffTool marks *target handed off when it runs.
func handoffTool(target **agent.UnifiedAgent) tool.Tool {
	return tool.NewFunctionTool("transfer", "Move the foreground", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) {
		(*target).MarkHandedOff()
		return "transferred", nil
	})
}

func TestTeamLeaderHandoffEndsRunWithoutError(t *testing.T) {
	var calls atomic.Int32
	leader := func(_ context.Context, assign tool.Tool) (*agent.UnifiedAgent, error) {
		var a *agent.UnifiedAgent
		tk, err := NewTeamLeaderToolkit(assign, handoffTool(&a))
		if err != nil {
			return nil, err
		}
		m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
			if calls.Add(1) == 1 {
				return &model.ChatResponse{Content: []message.ContentBlock{teamCall("h", "transfer", `{}`)}}, nil
			}
			return teamText("must not be called"), nil
		}}
		a = agent.NewUnifiedAgent("leader", "fixture", m, append(teamOptions(), agent.WithToolkit(tk))...)
		return a, nil
	}
	member := teamMember("A", func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return teamText("unused"), nil
	})
	p, err := NewTeamPipeline(leader, member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamDrain(t, p, nil); err != nil {
		t.Fatalf("a leader handoff ends the run without error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("leader model calls = %d, want 1", calls.Load())
	}
}

func TestTeamMemberHandoffCountsAsCompleted(t *testing.T) {
	var memberCalls atomic.Int32
	member := TeamMember{Name: "A", Description: "fixture specialist", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		var a *agent.UnifiedAgent
		m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
			if memberCalls.Add(1) == 1 {
				return &model.ChatResponse{Content: []message.ContentBlock{
					message.TextBlock{Type: "text", Text: "handing over"},
					teamCall("h", "transfer", `{}`),
				}}, nil
			}
			return teamText("must not be called"), nil
		}}
		a = agent.NewUnifiedAgent("A", "fixture", m, teamOptions(handoffTool(&a))...)
		return a, nil
	}}
	p, err := NewTeamPipeline(teamLeader([]message.ToolCallBlock{teamCall("a", TeamAssignToolName, `{"member":"A","prompt":"task"}`)}, func(results []message.ToolResultBlock) error {
		if len(results) != 1 || results[0].State != message.ToolResultSuccess || results[0].GetOutputText() != "handing over" {
			return fmt.Errorf("member handoff result: %+v", results)
		}
		return nil
	}), member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamDrain(t, p, nil); err != nil {
		t.Fatal(err)
	}
	if memberCalls.Load() != 1 {
		t.Fatalf("member model calls = %d, want 1", memberCalls.Load())
	}
}

func TestGoalAgentHandoffIsNotCompletion(t *testing.T) {
	var a *agent.UnifiedAgent
	m := &goalFixtureModel{call: func(_ context.Context, _ []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		if len(o.Tools) == 1 && o.Tools[0].Function.Name == "generate_structured_output" {
			return goalJSON(`{"report":"must not be reported"}`), nil
		}
		return &model.ChatResponse{Content: []message.ContentBlock{teamCall("h", "transfer", `{}`)}}, nil
	}}
	a = agent.NewUnifiedAgent("worker", "work", m, agent.WithToolkit(tool.NewToolkit(handoffTool(&a))))
	e, err := NewGoalExecutor(a, GoalAgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), "work", ""); err == nil || !strings.Contains(err.Error(), "handed off") {
		t.Fatalf("a handed-off executor must not report completion: %v", err)
	}
}
