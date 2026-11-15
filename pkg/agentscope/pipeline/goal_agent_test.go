package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type goalFixtureModel struct {
	call func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error)
}

func (m *goalFixtureModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	return m.call(ctx, msgs, o)
}
func (*goalFixtureModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*goalFixtureModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }
func goalJSON(text string) *model.ChatResponse {
	return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: text}}, Usage: &model.ChatUsage{InputTokens: 1, OutputTokens: 1}}
}

func TestGoalAgentReportCorrectionDoesNotRepeatWork(t *testing.T) {
	var writes atomic.Int32
	outputs := 0
	m := &goalFixtureModel{call: func(_ context.Context, msgs []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		if len(o.Tools) == 1 && o.Tools[0].Function.Name == "generate_structured_output" {
			outputs++
			if outputs == 1 {
				return goalJSON(`{"report":" "}`), nil
			}
			return goalJSON(`{"report":"artifact checked"}`), nil
		}
		if writes.Load() == 0 {
			return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "write", Name: "write_artifact", Input: "{}"}}}, nil
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}}, nil
	}}
	a := agent.NewUnifiedAgent("worker", "work", m, agent.WithToolkit(tool.NewToolkit(tool.NewFunctionTool("write_artifact", "fixture", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { writes.Add(1); return "written", nil }))))
	e, err := NewGoalExecutor(a, GoalAgentConfig{MaxOutputRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.Execute(context.Background(), "write an artifact", "")
	if err != nil || report.Report != "artifact checked" || writes.Load() != 1 || outputs != 2 {
		t.Fatalf("%+v err=%v writes=%d outputs=%d", report, err, writes.Load(), outputs)
	}
}

func TestGoalAgentVerifierChecksArtifactWithFreshContext(t *testing.T) {
	artifact := "missing cap"
	factoryCalls := 0
	factory := func(context.Context) (*agent.UnifiedAgent, error) {
		factoryCalls++
		n := 0
		m := &goalFixtureModel{call: func(_ context.Context, msgs []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
			if len(o.Tools) == 1 && o.Tools[0].Function.Name == "generate_structured_output" {
				if artifact == "missing cap" {
					return goalJSON(`{"result":"fail","message":"add page-size cap"}`), nil
				}
				return goalJSON(`{"result":"pass","message":"artifact passed"}`), nil
			}
			n++
			if n == 1 {
				if len(msgs) != 2 {
					t.Fatalf("verifier context reused: %d", len(msgs))
				}
				return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "inspect", Name: "inspect", Input: "{}"}}}, nil
			}
			return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "checked"}}}, nil
		}}
		inspect := tool.NewFunctionTool("inspect", "read actual artifact", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { return artifact, nil })
		return agent.NewUnifiedAgent("verifier", "check the artifact", m, agent.WithToolkit(tool.NewToolkit(inspect))), nil
	}
	verifier, err := NewGoalVerifier(factory, GoalAgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	e := goalExecutorFunc(func(context.Context, string, string) (ExecutionReport, error) {
		if factoryCalls > 0 {
			artifact = "cap implemented"
		}
		return ExecutionReport{Report: "I claim everything is correct"}, nil
	})
	p, _ := NewGoalPipeline(e, verifier, GoalConfig{MaxIterations: 2})
	out, err := p.Run(context.Background(), "implement cap")
	if err != nil || out.Status != GoalPassed || factoryCalls != 2 || out.Attempts[0].Verification.Result != VerdictFail {
		t.Fatalf("%+v %v factories=%d", out, err, factoryCalls)
	}
}

func TestGoalAgentUnsupportedInteraction(t *testing.T) {
	m := &goalFixtureModel{call: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "ask", Name: "AskUser", Input: `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Proceed"},{"label":"No","description":"Stop"}]}]}`}}}, nil
	}}
	a := agent.NewUnifiedAgent("worker", "ask first", m, agent.WithToolkit(tool.NewToolkit(tool.AskUserTool())), agent.WithPermissionContext(&permission.Context{Mode: permission.ModeDefault}))
	e, err := NewGoalExecutor(a, GoalAgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Execute(context.Background(), "ask the user", "")
	if !errors.Is(err, ErrGoalInteraction) {
		t.Fatalf("interaction not rejected: %v", err)
	}
}

func TestGoalAgentInvalidVerdictStopsAtOutputBudget(t *testing.T) {
	outputCalls := 0
	m := &goalFixtureModel{call: func(_ context.Context, _ []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		if len(o.Tools) > 0 {
			outputCalls++
			return goalJSON(`{"result":"pass","result":"fail","message":"ambiguous"}`), nil
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "checked"}}}, nil
	}}
	v, err := NewGoalVerifier(func(context.Context) (*agent.UnifiedAgent, error) {
		return agent.NewUnifiedAgent("v", "verify", m), nil
	}, GoalAgentConfig{MaxOutputRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(context.Background(), "goal", ExecutionReport{Report: "artifact"})
	if err == nil || !strings.Contains(err.Error(), "output") || outputCalls != 2 {
		t.Fatalf("calls=%d err=%v", outputCalls, err)
	}
}

func TestGoalAgentConfigurationFactoryAndCancellation(t *testing.T) {
	if _, err := NewGoalExecutor(nil, GoalAgentConfig{}); err == nil {
		t.Fatal("nil executor")
	}
	if _, err := NewGoalVerifier(nil, GoalAgentConfig{}); err == nil {
		t.Fatal("nil factory")
	}
	cause := errors.New("factory failed")
	v, _ := NewGoalVerifier(func(context.Context) (*agent.UnifiedAgent, error) { return nil, cause }, GoalAgentConfig{})
	if _, err := v.Verify(context.Background(), "g", ExecutionReport{}); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	v, _ = NewGoalVerifier(func(context.Context) (*agent.UnifiedAgent, error) { return nil, nil }, GoalAgentConfig{})
	if _, err := v.Verify(context.Background(), "g", ExecutionReport{}); err == nil {
		t.Fatal("nil factory result")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &goalFixtureModel{call: func(ctx context.Context, _ []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
		cancel()
		return nil, ctx.Err()
	}}
	e, _ := NewGoalExecutor(agent.NewUnifiedAgent("e", "work", m), GoalAgentConfig{})
	if _, err := e.Execute(ctx, "g", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestGoalAgentRejectsConflictingJSONText(t *testing.T) {
	m := &goalFixtureModel{call: func(_ context.Context, _ []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		text := "checked"
		if len(o.Tools) > 0 {
			text = `[{"result":"pass","message":"yes"},{"result":"fail","message":"no"}]`
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}}, nil
	}}
	v, err := NewGoalVerifier(func(context.Context) (*agent.UnifiedAgent, error) {
		return agent.NewUnifiedAgent("v", "verify", m), nil
	}, GoalAgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := v.Verify(context.Background(), "goal", ExecutionReport{Report: "artifact"})
	if err == nil || out.Result != "" {
		t.Fatalf("conflicting array accepted: %+v %v", out, err)
	}
}
