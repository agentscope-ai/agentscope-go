package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tracing"
)

// This example demonstrates TracingMiddleware, which automatically creates
// nested spans for agent lifecycle events:
//   - invoke_agent: wraps the entire Reply lifecycle
//   - chat: wraps each model API call
//   - execute_tool: wraps each tool execution
//
// Spans are nested via Go context propagation, producing a trace tree.
// The example uses LoggerTracer for console output; switch to OTELTracer
// for production OpenTelemetry integration.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Set up logger-based tracing (prints span start/end to console).
	tracer := tracing.LoggerTracer{Logger: as.Logger()}
	tracing.SetupTracing(tracer)

	// Create the tracing middleware (uses the global tracer if nil is passed).
	tracingMW := middleware.NewTracingMiddleware(nil)

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
				"temperature": "24°C",
				"condition":   "clear sky",
			}, nil
		},
	)

	tk := tool.NewToolkit(weatherTool)

	a := agent.NewUnifiedAgent(
		"traced-assistant",
		"You are a helpful assistant. Use get_weather to look up weather.",
		cm,
		agent.WithToolkit(tk),
		agent.WithMiddlewares(tracingMW),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	fmt.Println("Sending request (watch trace output in logs)...")

	reply, err := a.Reply(context.Background(), "What's the weather in Tokyo?")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Println()
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
