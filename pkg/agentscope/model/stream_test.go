package model

import (
	"context"
	"os"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestDashScopeChatStream(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set")
	}

	m, err := NewDashScopeChatModel(DashScopeConfig{
		APIKey: apiKey,
		Model:  "qwen-plus",
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []*message.Msg{
		message.UserMsg("test", "Say hello in one short sentence."),
	}

	ch, err := m.ChatStream(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}

	var gotDelta, gotFinal bool
	var finalText string
	for resp := range ch {
		if resp.IsLast {
			gotFinal = true
			finalText = resp.GetTextContent()
			if resp.Usage == nil {
				t.Error("final response should have usage")
			}
		} else {
			gotDelta = true
		}
	}

	if !gotDelta {
		t.Error("expected at least one delta chunk")
	}
	if !gotFinal {
		t.Error("expected a final response")
	}
	if finalText == "" {
		t.Error("final text should not be empty")
	}
	t.Logf("Stream response: %s", finalText)
}

func TestDashScopeChatStreamWithToolCalling(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set")
	}

	m, err := NewDashScopeChatModel(DashScopeConfig{
		APIKey: apiKey,
		Model:  "qwen-plus",
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []*message.Msg{
		message.UserMsg("test", "What is the weather in Beijing today?"),
	}

	tools := []ToolSchema{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_weather",
				Description: "Get the current weather for a location",
				Parameters:  []byte(`{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}`),
			},
		},
	}

	ch, err := m.ChatStream(context.Background(), msgs, WithTools(tools))
	if err != nil {
		t.Fatal(err)
	}

	var gotFinal bool
	var hasToolCall bool
	for resp := range ch {
		if resp.IsLast {
			gotFinal = true
			for _, b := range resp.Content {
				if _, ok := b.(message.ToolCallBlock); ok {
					hasToolCall = true
				}
			}
		}
	}

	if !gotFinal {
		t.Error("expected a final response")
	}
	if !hasToolCall {
		t.Error("expected tool call in final response")
	}
}
