package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates UnifiedAgent with a custom FunctionTool.
// The model uses native API-level function calling to invoke tools.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("load chat model error:", err)
		return
	}

	// Define a simple sum tool using FunctionTool.
	sumTool := tool.NewFunctionTool(
		"sum_numbers",
		"Sum a list of numbers and return the total",
		nil,
		func(ctx context.Context, args map[string]any) (any, error) {
			raw, ok := args["numbers"]
			if !ok {
				return nil, fmt.Errorf("numbers is required")
			}
			list, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("numbers must be array")
			}
			var total float64
			for _, v := range list {
				if n, ok := v.(float64); ok {
					total += n
				}
			}
			return map[string]any{"result": total}, nil
		},
	)

	tk := tool.NewToolkit(sumTool)

	a := agent.NewUnifiedAgent(
		"assistant",
		"You are a helpful assistant. Use the sum_numbers tool when the user asks for a calculation.",
		cm,
		agent.WithToolkit(tk),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	ctx := context.Background()
	reply, err := a.Reply(ctx, "Please calculate the sum of [1, 2, 3.5] and tell me the result.")
	if err != nil {
		fmt.Println("error:", err)
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
