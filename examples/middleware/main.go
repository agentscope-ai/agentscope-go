package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the middleware system.
// A logging middleware wraps model calls and tool executions to print timing info.

// LoggingMiddleware logs model call durations and tool executions.
type LoggingMiddleware struct {
	middleware.BaseMiddleware
}

func (m *LoggingMiddleware) OnModelCall(
	ctx context.Context,
	input *middleware.ModelCallInput,
	next middleware.ModelCallHandler,
) (*model.ChatResponse, error) {
	start := time.Now()
	fmt.Printf("[middleware] calling model for agent %q with %d messages...\n", input.AgentName, len(input.Messages))

	resp, err := next(ctx, input)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("[middleware] model call failed after %v: %v\n", elapsed, err)
	} else {
		text := resp.GetTextContent()
		preview := text
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		fmt.Printf("[middleware] model responded in %v (%d content blocks): %s\n", elapsed, len(resp.Content), preview)
		if resp.Usage != nil {
			fmt.Printf("[middleware] tokens: input=%d output=%d\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)
		}
	}

	return resp, err
}

func (m *LoggingMiddleware) OnActing(
	ctx context.Context,
	input *middleware.ActingInput,
	next middleware.ActingHandler,
) (*tool.ToolResponse, error) {
	fmt.Printf("[middleware] executing tool %q for agent %q\n", input.ToolCall.Name, input.AgentName)
	start := time.Now()

	resp, err := next(ctx, input)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("[middleware] tool %q failed after %v: %v\n", input.ToolCall.Name, elapsed, err)
	} else {
		fmt.Printf("[middleware] tool %q completed in %v\n", input.ToolCall.Name, elapsed)
	}

	return resp, err
}

func (m *LoggingMiddleware) OnSystemPrompt(
	ctx context.Context,
	agentName string,
	currentPrompt string,
) string {
	fmt.Printf("[middleware] system prompt for %q: %d chars\n", agentName, len(currentPrompt))
	return currentPrompt
}

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

	weatherTool := tool.NewFunctionTool("get_weather", "Get current weather for a city", weatherSchema,
		func(ctx context.Context, input map[string]any) (any, error) {
			loc, _ := input["location"].(string)
			return map[string]any{
				"location":    loc,
				"temperature": "22°C",
				"condition":   "sunny",
			}, nil
		},
	)

	tk := tool.NewToolkit(weatherTool)

	logging := &LoggingMiddleware{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: "logging"},
	}

	a := agent.NewUnifiedAgent(
		"assistant",
		"You are a helpful assistant. Use get_weather to look up weather.",
		cm,
		agent.WithToolkit(tk),
		agent.WithMiddlewares(logging),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	reply, err := a.Reply(context.Background(), "What's the weather in Beijing?")
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
