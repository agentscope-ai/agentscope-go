// Command console launches an interactive terminal chat with an agent
// (Go port of Python agentscope's launch_console). It renders every
// streamed event, asks for tool-call confirmation (y/N/a) when the
// permission engine requires it, and turns Ctrl+C into an interruption
// of the current reply. Type exit/quit or press Ctrl+D to leave.
//
// Usage:
//
//	go run ./examples/console
package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/console"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Built-in tools + default permission mode: dangerous calls (bash,
	// writes) trigger the interactive y/N/a confirmation.
	tk := tool.NewToolkit(tool.BashTool(), tool.ReadTool(), tool.WriteTool())

	a := agent.NewUnifiedAgent(
		"assistant",
		"You are a helpful assistant with shell and file access. Be concise.",
		cm,
		agent.WithToolkit(tk),
		agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 8}),
	)

	// console.Launch handles SIGINT itself: Ctrl+C during a reply
	// interrupts that reply; at the prompt it exits.
	if err := console.Launch(context.Background(), a, console.WithUserName("user")); err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
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
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
