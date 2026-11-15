package main

import (
	"context"
	"fmt"
	"os"
	"time"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example mirrors Python scripts/model_examples/*_multiagent_multimodal.py.
// It combines multi-agent conversation with multimodal (image) input:
// Alice shares an image, Bob comments on it, and a moderator summarizes.

const testImageURL = "https://help-static-aliyun-doc.aliyuncs.com/file-manage-files/zh-CN/20241022/emyrja/dog_and_girl.jpeg"

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Println("=== Multi-Agent + Multimodal Conversation ===")

	imageBlock := message.DataBlock{
		Type: "data",
		ID:   "shared-img",
		Source: message.URLSource{
			Type:      "url",
			URL:       testImageURL,
			MediaType: "image/jpeg",
		},
	}

	msgs := []*message.Msg{
		message.SystemMsg("system", "You are a helpful moderator. Summarize the conversation including what is shown in any shared images."),
		message.NewMsg("alice", message.RoleUser, []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Hey Bob, look at this photo I took today!"},
			imageBlock,
		}),
		message.NewMsg("bob", message.RoleAssistant, "That looks lovely, Alice! What a cute scene. Where was this taken?"),
		message.NewMsg("alice", message.RoleUser, "At the park near my house. The weather was perfect."),
		message.NewMsg("moderator", message.RoleUser, "Please summarize the conversation above, including a description of the shared image."),
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
}

func loadChatModelFromEnv() (model.ChatModel, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey: key, Model: "claude-sonnet-4-20250514", MaxOutputTokens: 1024,
			ClientOptions: &model.ClientOptions{Timeout: 120 * time.Second},
		})
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey: key, BaseURL: os.Getenv("DASHSCOPE_BASE_URL"), Model: "qwen3.5-plus",
			ClientOptions: &model.ClientOptions{Timeout: 120 * time.Second},
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey: key, Model: "gpt-4o-mini",
			ClientOptions: &model.ClientOptions{Timeout: 120 * time.Second},
		})
	}
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY")
}
