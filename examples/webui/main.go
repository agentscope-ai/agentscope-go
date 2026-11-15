// Example webui demonstrates the AgentScope Web UI.
//
// Start the server:
//
//	DASHSCOPE_API_KEY=... go run ./examples/webui
//
// Then open http://localhost:8080 in your browser.
//
// The web UI provides:
//   - Session management (create, list, delete)
//   - Streaming chat with thinking blocks, tool calls, and tool results
//   - Human-in-the-loop tool call confirmation
//   - Model information browser
//
// The example shows two ways to mount the web UI:
//  1. Using [service.Service.HandlerWithWebUI] (simplest, recommended)
//  2. Using [webui.Handler] + manual mux wiring (advanced, shown in comments)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/service"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Define example tools
	weatherSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"location": {"type": "string", "description": "City name"}
		},
		"required": ["location"]
	}`)

	calcSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"expression": {"type": "string", "description": "Math expression to evaluate"}
		},
		"required": ["expression"]
	}`)

	// Agent factory creates a new agent for each session
	factory := func(name, prompt string, _ model.ChatModel) *agent.UnifiedAgent {
		weatherTool := tool.NewFunctionTool("get_weather", "Get current weather for a city", weatherSchema,
			func(ctx context.Context, input map[string]any) (any, error) {
				loc, _ := input["location"].(string)
				return map[string]any{
					"location":    loc,
					"temperature": "22C",
					"condition":   "sunny",
					"humidity":    "45%",
				}, nil
			},
		)

		calcTool := tool.NewFunctionTool("calculate", "Evaluate a math expression", calcSchema,
			func(ctx context.Context, input map[string]any) (any, error) {
				expr, _ := input["expression"].(string)
				return map[string]any{
					"expression": expr,
					"result":     "42",
					"note":       "simplified result",
				}, nil
			},
		)

		return agent.NewUnifiedAgent(
			name, prompt, cm,
			agent.WithToolkit(tool.NewToolkit(weatherTool, calcTool)),
			agent.WithReactConfig(agent.ReactConfig{MaxIters: 10}),
		)
	}

	// Create the agent HTTP service with embedded Web UI
	svc := service.New(service.Config{
		Addr:           ":8080",
		AllowedOrigins: []string{"*"},
	}, cm, factory)

	// HandlerWithWebUI merges the agent REST/SSE API with the embedded
	// web interface into a single http.Handler.
	handler := svc.HandlerWithWebUI(service.WebUIConfig{
		Enable: true,
	})

	fmt.Println("AgentScope Studio starting on http://localhost:8080")
	fmt.Println()
	fmt.Println("  Web UI:  http://localhost:8080")
	fmt.Println("  API:     http://localhost:8080/api/")
	fmt.Println("  Health:  http://localhost:8080/healthz")
	fmt.Println()

	srv := &http.Server{
		Addr:    ":8080",
		Handler: handler,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println("Server error:", err)
	}
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey:          key,
			Model:           "claude-sonnet-4-20250514",
			MaxOutputTokens: 4096,
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
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		return model.NewDeepSeekChatModel(model.DeepSeekConfig{
			APIKey: key,
			Model:  "deepseek-chat",
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, OPENAI_API_KEY, or DEEPSEEK_API_KEY")
}
