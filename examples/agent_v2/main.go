package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the unified Agent (v2) with native tool calling.
// The model uses API-level function calling to invoke tools, rather than
// parsing JSON from text output.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Define tools with JSON Schema
	weatherSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"location": {"type": "string", "description": "City name"}
		},
		"required": ["location"]
	}`)

	weatherTool := tool.NewFunctionTool("get_weather", "Get current weather for a city", weatherSchema,
		func(ctx context.Context, input map[string]any) (any, error) {
			loc, _ := input["location"].(string)
			return map[string]any{
				"location":    loc,
				"temperature": "18°C",
				"condition":   "partly cloudy",
				"humidity":    "65%",
			}, nil
		},
	)

	tk := tool.NewToolkit(weatherTool)

	// Create the unified agent
	a := agent.NewUnifiedAgent(
		"weather-assistant",
		"You are a helpful weather assistant. Use the get_weather tool to look up weather information when asked.",
		cm,
		agent.WithToolkit(tk),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	// Synchronous reply
	reply, err := a.Reply(context.Background(), "What's the weather like in Shanghai?")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("Assistant:", *txt)
	}
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey:          key,
			Model:           "claude-sonnet-4-20250514",
			MaxOutputTokens: 1024,
		})
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey:  key,
			BaseURL: os.Getenv("DASHSCOPE_BASE_URL"),
			Model:   "qwen-plus",
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key,
			Model:  "gpt-4o-mini",
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
