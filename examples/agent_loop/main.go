package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the v3 agent loop infrastructure:
//   - loop.Loop: configurable reasoning-acting state machine
//   - metrics.MetricsHook: automatic instrumentation of model calls and tool executions
//   - metrics.InMemoryProvider: in-process metric collection with snapshot export
//
// The loop drives a simple tool-calling agent and prints metrics after completion.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Define a tool.
	weatherTool := tool.NewFunctionTool("get_weather", "Get weather for a city", nil,
		func(_ context.Context, args map[string]any) (any, error) {
			city, _ := args["city"].(string)
			return map[string]any{
				"city":        city,
				"temperature": "22°C",
				"condition":   "sunny",
			}, nil
		},
	)
	tk := tool.NewToolkit(weatherTool)

	// Set up in-memory metrics to capture loop telemetry.
	mp := metrics.NewInMemoryProvider()
	metricsHook := metrics.NewMetricsHook(mp)

	// Build the loop with model caller, tool executor, schema provider, and metrics hook.
	agentLoop := loop.New(
		loop.WithModelCaller(loop.ModelCallerFunc(
			func(ctx context.Context, msgs []*message.Msg, tools []model.ToolSchema) (*model.ChatResponse, error) {
				var opts []model.CallOption
				if len(tools) > 0 {
					opts = append(opts, model.WithTools(tools))
				}
				return cm.Chat(ctx, msgs, opts...)
			},
		)),
		loop.WithToolExecutor(&toolkitExecutor{tk: tk}),
		loop.WithSchemaProvider(tk),
		loop.WithSystemPrompt("You are a helpful weather assistant. Use the get_weather tool to answer weather questions."),
		loop.WithMaxIters(5),
		loop.WithHooks(metricsHook),
	)

	// RunSync drives the full reasoning-acting cycle and returns the final response.
	fmt.Println("=== Running agent loop (sync) ===")
	resp, err := agentLoop.RunSync(context.Background(), "What's the weather in Tokyo?")
	if err != nil {
		fmt.Println("Loop error:", err)
		return
	}

	if resp != nil {
		fmt.Println("Response:", resp.GetTextContent())
	}

	// Print collected metrics.
	fmt.Println("\n=== Metrics Snapshot ===")
	for name, value := range mp.Snapshot() {
		fmt.Printf("  %-50s %.2f\n", name, value)
	}
}

// toolkitExecutor adapts a tool.Toolkit to the loop.ToolExecutor interface.
type toolkitExecutor struct {
	tk *tool.Toolkit
}

func (e *toolkitExecutor) Execute(ctx context.Context, call message.ToolCallBlock) (*tool.ToolResponse, error) { //nolint:gocritic // interface requirement
	return e.tk.CallToolFromBlock(ctx, &call)
}

func (e *toolkitExecutor) BatchExecute(ctx context.Context, calls []message.ToolCallBlock) []*loop.ToolResult {
	var results []*loop.ToolResult
	for _, c := range calls {
		resp, err := e.Execute(ctx, c)
		results = append(results, &loop.ToolResult{Call: c, Response: resp, Err: err})
	}
	return results
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
