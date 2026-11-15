// Command dingtalk_channel connects an agent to DingTalk as an
// enterprise robot (Phase 2 channel, DingTalk first).
//
// Inbound messages arrive over the official DingTalk Stream connection
// (no public endpoint needed); replies and text-mode tool confirmations
// (reply y / n / a in the chat) go back through the per-message session
// webhook. Create a DingTalk enterprise app robot and set:
//
//	DINGTALK_CLIENT_ID      (AppKey)
//	DINGTALK_CLIENT_SECRET  (AppSecret)
//
// then run:
//
//	go run ./examples/dingtalk_channel
//
// and @ the bot in a group (or DM it) to chat.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/channel"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/channel/dingtalk"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	as.Init()

	clientID := os.Getenv("DINGTALK_CLIENT_ID")
	clientSecret := os.Getenv("DINGTALK_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("set DINGTALK_CLIENT_ID and DINGTALK_CLIENT_SECRET")
	}

	cm, err := loadChatModelFromEnv()
	if err != nil {
		return err
	}

	ch, err := dingtalk.NewChannel(dingtalk.Config{
		ClientID:     clientID,
		ClientSecret: model.NewSecretStr(clientSecret),
	})
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	defer ch.Close()

	// One agent per chat (session key = conversation id). Bash access +
	// default permission mode means dangerous calls trigger the text-mode
	// confirmation round-trip in the chat.
	factory := func(sessionKey string) (channel.Agent, error) {
		tk := tool.NewToolkit(tool.BashTool(), tool.ReadTool())
		return agent.NewUnifiedAgent(
			"assistant",
			"You are a helpful assistant. Be concise; answers are sent to an IM chat.",
			cm,
			agent.WithToolkit(tk),
			agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)),
			agent.WithReactConfig(agent.ReactConfig{MaxIters: 8}),
		), nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	gw := channel.NewGateway(ch, factory)
	if err := gw.Start(ctx); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	fmt.Println("DingTalk channel listening. @ the bot in a group or DM it. Ctrl+C to stop.")
	<-ctx.Done()
	fmt.Println("shutting down")
	return nil
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
