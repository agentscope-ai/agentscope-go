package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example mirrors Python scripts/model_examples/*_multiagent.py.
// It demonstrates multi-agent conversation handling where multiple non-user
// agents (Alice, Bob) participate and a moderator agent summarizes.
//
// The model's internal formatter wraps prior conversation history so the
// model can distinguish between different speakers.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	exampleMultiAgent(cm)
}

func exampleMultiAgent(cm model.ChatModel) {
	fmt.Println("=== Multi-Agent Conversation ===")

	msgs := []*message.Msg{
		message.SystemMsg("system", "You are a helpful moderator. Summarize the conversation."),
		message.NewMsg("alice", message.RoleUser, "Hi Bob! What do you think about the weather today?"),
		message.NewMsg("bob", message.RoleAssistant, "It's quite sunny and warm, Alice. Perfect for a walk!"),
		message.NewMsg("alice", message.RoleUser, "Agreed! I might head to the park later."),
		message.NewMsg("bob", message.RoleAssistant, "Great idea. I'll join you if I finish work early."),
		message.NewMsg("moderator", message.RoleUser, "Please summarize the conversation above in one sentence."),
	}

	ch, err := cm.ChatStream(context.Background(), msgs)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	for resp := range ch {
		if !resp.IsLast {
			fmt.Print(resp.GetTextContent())
		} else {
			fmt.Println()
			if resp.Usage != nil {
				fmt.Printf("Tokens: in=%d out=%d\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)
			}
		}
	}
	fmt.Println()
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey: key, Model: "claude-sonnet-4-20250514", MaxOutputTokens: 1024,
		})
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey: key, BaseURL: os.Getenv("DASHSCOPE_BASE_URL"), Model: "qwen-plus",
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key, Model: "gpt-4o-mini",
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
