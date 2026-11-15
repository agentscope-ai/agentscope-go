// An offline executor/verifier loop over an actual temporary artifact. Models
// are deterministic fixtures; no API credentials or external services are used.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type fixtureModel struct {
	verifier  bool
	workCalls int
	observed  int
}

func (m *fixtureModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	if len(o.Tools) == 1 && o.Tools[0].Function.Name == "generate_structured_output" {
		output := `{"report":"page_size.json contains the page-size cap; check it with inspect_cap"}`
		if m.verifier {
			output = `{"result":"fail","message":"The cap exceeds 100; update page_size.json"}`
			if m.observed > 0 && m.observed <= 100 {
				output = `{"result":"pass","message":"inspect_cap confirmed a cap of 100"}`
			}
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: output}}, Usage: &model.ChatUsage{InputTokens: 10, OutputTokens: 5}}, nil
	}
	m.workCalls++
	if m.workCalls%2 == 1 {
		name := "write_cap"
		if m.verifier {
			name = "inspect_cap"
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: fmt.Sprintf("call-%d", m.workCalls), Name: name, Input: "{}"}}, Usage: &model.ChatUsage{InputTokens: 10, OutputTokens: 5}}, nil
	}
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "The artifact is ready for checking."}}, Usage: &model.ChatUsage{InputTokens: 10, OutputTokens: 5}}, nil
}
func (*fixtureModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*fixtureModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir, err := os.MkdirTemp("", "agentscope-goal-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	artifact := filepath.Join(dir, "page_size.json")
	writes := 0
	write := tool.NewFunctionTool("write_cap", "write the fixture artifact", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) {
		writes++
		capValue := 1000
		if writes > 1 {
			capValue = 100
		}
		return "artifact written", os.WriteFile(artifact, []byte(fmt.Sprintf(`{"cap":%d}`, capValue)), 0600)
	})
	executor, err := pipeline.NewGoalExecutor(agent.NewUnifiedAgent("executor", "Implement the goal and report artifacts.", &fixtureModel{}, agent.WithToolkit(tool.NewToolkit(write))), pipeline.GoalAgentConfig{MaxOutputRetries: 1})
	if err != nil {
		return err
	}
	verifier, err := pipeline.NewGoalVerifier(func(context.Context) (*agent.UnifiedAgent, error) {
		m := &fixtureModel{verifier: true}
		inspect := tool.NewFunctionTool("inspect_cap", "read the actual artifact", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) {
			data, err := os.ReadFile(artifact)
			if err != nil {
				return nil, err
			}
			var value struct {
				Cap int `json:"cap"`
			}
			if err := json.Unmarshal(data, &value); err != nil {
				return nil, err
			}
			m.observed = value.Cap
			return value, nil
		})
		return agent.NewUnifiedAgent("verifier", "Use inspect_cap to check the cap; do not trust the report alone.", m, agent.WithToolkit(tool.NewToolkit(inspect))), nil
	}, pipeline.GoalAgentConfig{MaxOutputRetries: 1})
	if err != nil {
		return err
	}
	p, err := pipeline.NewGoalPipeline(executor, verifier, pipeline.GoalConfig{MaxIterations: 3})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.Run(ctx, "Write a pagination configuration with a positive cap no greater than 100.")
	for i, attempt := range result.Attempts {
		if attempt.Verification != nil {
			fmt.Printf("Attempt %d: %s — %s\n", i+1, attempt.Verification.Result, attempt.Verification.Message)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("Status: %s; artifact writes: %d\n", result.Status, writes)
	if result.Status != pipeline.GoalPassed {
		return fmt.Errorf("goal did not pass")
	}
	return nil
}
