package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example demonstrates streaming with the unified Agent (v2).
// It uses ReplyStream to receive events in real-time and prints text
// deltas as they arrive from the model.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	a := agent.NewUnifiedAgent(
		"storyteller",
		"You are a creative storyteller. Tell short, engaging stories in 2-3 sentences.",
		cm,
	)

	ch, err := a.ReplyStream(context.Background(), "Tell me a short story about a robot learning to paint.")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Print("Assistant: ")
	for evt := range ch {
		switch e := evt.(type) {
		case event.TextBlockDeltaEvent:
			fmt.Print(e.Delta)
		case event.ModelCallStartEvent:
			// Model call started
		case event.ModelCallEndEvent:
			if e.InputTokens > 0 {
				fmt.Printf("\n\n[Tokens: input=%d, output=%d]", e.InputTokens, e.OutputTokens)
			}
		case event.ReplyEndEvent:
			fmt.Println()
		}
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
