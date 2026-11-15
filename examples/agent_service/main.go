package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/service"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the HTTP Agent Service with SSE streaming.
//
// Start the server:
//
//	DASHSCOPE_API_KEY=... go run ./examples/agent_service
//
// Then interact via curl:
//
//	# Create a session
//	curl -X POST http://localhost:8080/api/session \
//	  -H 'Content-Type: application/json' \
//	  -d '{"agent_name":"weather-bot","system_prompt":"You are a weather assistant."}'
//
//	# Chat (non-streaming)
//	curl -X POST http://localhost:8080/api/chat \
//	  -H 'Content-Type: application/json' \
//	  -d '{"session_id":"<SESSION_ID>","message":"What is the weather in Tokyo?"}'
//
//	# Chat (SSE streaming)
//	curl -N "http://localhost:8080/api/chat/stream?session_id=<SESSION_ID>&message=Hello"
//
//	# List sessions
//	curl http://localhost:8080/api/sessions
//
//	# List available models
//	curl http://localhost:8080/api/models

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	weatherSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"location": {"type": "string", "description": "City name"}
		},
		"required": ["location"]
	}`)

	factory := func(name, prompt string, _ model.ChatModel) *agent.UnifiedAgent {
		weatherTool := tool.NewFunctionTool("get_weather", "Get weather for a city", weatherSchema,
			func(ctx context.Context, input map[string]any) (any, error) {
				loc, _ := input["location"].(string)
				return map[string]any{"location": loc, "temp": "22°C", "condition": "sunny"}, nil
			},
		)
		return agent.NewUnifiedAgent(
			name, prompt, cm,
			agent.WithToolkit(tool.NewToolkit(weatherTool)),
			agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
		)
	}

	svc := service.New(service.Config{
		Addr:           ":8080",
		AllowedOrigins: []string{"*"},
	}, cm, factory)

	fmt.Println("Agent HTTP Service starting on :8080")
	fmt.Println("Endpoints:")
	fmt.Println("  POST /api/session      — create session")
	fmt.Println("  POST /api/chat         — send message")
	fmt.Println("  GET  /api/chat/stream  — SSE streaming")
	fmt.Println("  GET  /api/sessions     — list sessions")
	fmt.Println("  GET  /api/models       — list models")

	if err := svc.ListenAndServe(); err != nil {
		fmt.Println("Server error:", err)
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
