package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestMockModelRulesInOrder(t *testing.T) {
	m := NewMockModel(
		OnPromptContaining("weather", RespondWithText("it is sunny")),
		Default(RespondWithText("fallback")),
	)

	resp, err := m.Chat(context.Background(), []*message.Msg{
		message.UserMsg("u", "what is the weather"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetTextContent(); got != "it is sunny" {
		t.Fatalf("got %q", got)
	}

	resp, err = m.Chat(context.Background(), []*message.Msg{
		message.UserMsg("u", "hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetTextContent(); got != "fallback" {
		t.Fatalf("got %q", got)
	}

	if len(m.Calls()) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(m.Calls()))
	}
}

func TestMockModelOnNthCall(t *testing.T) {
	m := NewMockModel(
		OnNthCall(2, RespondWithText("second")),
		Default(RespondWithText("other")),
	)

	first, _ := m.Chat(context.Background(), nil)
	second, _ := m.Chat(context.Background(), nil)

	if first.GetTextContent() != "other" {
		t.Fatalf("call 1: got %q", first.GetTextContent())
	}
	if second.GetTextContent() != "second" {
		t.Fatalf("call 2: got %q", second.GetTextContent())
	}
}

func TestMockModelError(t *testing.T) {
	sentinel := errors.New("boom")
	m := NewMockModel(Default(RespondWithError(sentinel)))

	_, err := m.Chat(context.Background(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls := m.Calls(); len(calls) != 1 || calls[0].Err == nil {
		t.Fatalf("expected recorded call with error")
	}
}

func TestRunAgentWithToolCall(t *testing.T) {
	m := NewMockModel(
		OnToolCall("add", RespondWithText("The result is 3.")),
		Default(RespondWithToolCall("add", map[string]any{"x": 1, "y": 2})),
	)

	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}}`)
	addTool := tool.NewFunctionTool("add", "Add two numbers", schema,
		func(_ context.Context, input map[string]any) (any, error) {
			x, _ := input["x"].(float64)
			y, _ := input["y"].(float64)
			return x + y, nil
		},
	)

	a := agent.NewUnifiedAgent("calc", "You are a calculator.", m,
		agent.WithToolkit(tool.NewToolkit(addTool)),
	)

	result := RunAgent(t, a, "What is 1+2?")

	if result.FinalOutput != "The result is 3." {
		t.Fatalf("final output: got %q", result.FinalOutput)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Name != "add" {
		t.Fatalf("tool name: got %q", result.ToolCalls[0].Name)
	}
	if result.Iterations != 2 {
		t.Fatalf("expected 2 model iterations, got %d", result.Iterations)
	}
}

var _ model.ChatModel = (*MockModel)(nil)
