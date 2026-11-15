package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example demonstrates switching between multiple model providers.
// It shows how to use the credential system and model cards to discover
// available models, then uses whichever provider is configured via env vars.

func main() {
	as.Init()

	// List all known model cards
	fmt.Println("=== Available Model Cards ===")
	cards := model.ListModels()
	fmt.Printf("Total: %d models across providers\n\n", len(cards))

	providers := map[string]int{}
	for i := range cards {
		providers[cards[i].Provider]++
	}
	for p, n := range providers {
		fmt.Printf("  %s: %d models\n", p, n)
	}
	fmt.Println()

	// Show detailed info for a specific model
	if card, err := model.GetModelCard("claude-opus-4-6"); err == nil {
		fmt.Printf("Model: %s\n", card.Name)
		fmt.Printf("  Label: %s\n", card.Label)
		fmt.Printf("  Provider: %s\n", card.Provider)
		fmt.Printf("  Context: %d tokens\n", card.ContextSize)
		fmt.Printf("  Max Output: %d tokens\n", card.OutputSize)
		fmt.Printf("  Thinking: %v, Images: %v, Audio: %v\n\n",
			card.SupportsThinking(), card.SupportsImages(), card.SupportsAudio())
	}

	// Auto-detect provider from env and create a model
	cred := credential.FromEnv()
	if cred == nil {
		fmt.Println("No API key found. Set one of:")
		fmt.Println("  ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, OPENAI_API_KEY,")
		fmt.Println("  DEEPSEEK_API_KEY, GEMINI_API_KEY, MOONSHOT_API_KEY, XAI_API_KEY")
		return
	}

	fmt.Printf("Detected provider: %s\n", cred.Provider())

	cm, err := createModelFromCredential(cred)
	if err != nil {
		fmt.Println("Error creating model:", err)
		return
	}

	// Make a simple chat call
	msg := []*message.Msg{message.UserMsg("user", "Say hello in exactly 5 words.")}
	resp, err := cm.Chat(context.Background(), msg)
	if err != nil {
		fmt.Println("Chat error:", err)
		return
	}

	fmt.Printf("Response: %s\n", resp.GetTextContent())
	if resp.Usage != nil {
		fmt.Printf("Tokens: in=%d out=%d\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
}

func createModelFromCredential(cred credential.Credential) (model.ChatModel, error) {
	switch cred.Provider() {
	case "anthropic":
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey:          cred.APIKey(),
			BaseURL:         cred.BaseURL(),
			Model:           "claude-sonnet-4-20250514",
			MaxOutputTokens: 1024,
		})
	case "dashscope":
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   "qwen-plus",
		})
	case "openai":
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   "gpt-4o-mini",
		})
	case "deepseek":
		return model.NewDeepSeekChatModel(model.DeepSeekConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   "deepseek-chat",
		})
	case "gemini":
		return model.NewGeminiChatModel(model.GeminiConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   "gemini-2.5-flash",
		})
	case "moonshot":
		return model.NewMoonshotChatModel(model.MoonshotConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   "moonshot-v1-8k",
		})
	case "xai":
		return model.NewXAIChatModel(model.XAIConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   "grok-3-mini",
		})
	default:
		return nil, fmt.Errorf("unsupported provider: %s", cred.Provider())
	}
}

func init() {
	// Allow overriding the default DashScope base URL.
	if url := os.Getenv("DASHSCOPE_BASE_URL"); url != "" {
		_ = url
	}
}
