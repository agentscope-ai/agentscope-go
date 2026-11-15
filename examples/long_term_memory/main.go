package main

import (
	"context"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware/memory"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// This example demonstrates the long-term memory middleware.
// The middleware provides cross-session memory using a MemoryStore backend.
// Three modes are available:
//   - static_control: automatic memory search/inject (no tools)
//   - agent_control: tools only (search_memory, add_memory)
//   - both: automatic injection plus tool access
//
// Here we use "both" mode with an InMemoryStore for demonstration.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	ctx := context.Background()
	store := memory.NewInMemoryStore()

	// Pre-populate some memories to simulate past conversations.
	store.Add(ctx, "User prefers Go over Python for backend development", "user-123", "assistant")
	store.Add(ctx, "User's name is Alice and she works at Acme Corp", "user-123", "assistant")
	store.Add(ctx, "User is building a multi-agent framework called AgentScope", "user-123", "assistant")
	store.Add(ctx, "User likes detailed explanations with code examples", "user-123", "assistant")

	fmt.Println("=== Pre-populated Memories ===")
	all, _ := store.List(ctx, "user-123")
	for _, m := range all {
		fmt.Printf("  [%s] %s\n", m.ID, m.Text)
	}
	fmt.Println()

	// Create the long-term memory middleware in "both" mode.
	memMiddleware, err := memory.New(&memory.Config{
		UserID:  "user-123",
		AgentID: "assistant",
		Store:   store,
		Mode:    memory.ModeBoth,
		TopK:    3,
	})
	if err != nil {
		fmt.Println("Error creating memory middleware:", err)
		return
	}

	// Build toolkit: memory tools + a custom tool.
	memTools := memMiddleware.Tools()
	allTools := make([]tool.Tool, 0, len(memTools)+1)
	allTools = append(allTools, memTools...)
	allTools = append(allTools, tool.NewFunctionTool("get_project_status", "Get the status of a project", nil,
		func(ctx context.Context, input map[string]any) (any, error) {
			return "AgentScope v2.0: 95% complete, remaining work: examples and documentation", nil
		},
	))
	tk := tool.NewToolkit(allTools...)

	a := agent.NewUnifiedAgent(
		"assistant",
		"You are a helpful assistant with long-term memory. "+
			"You can remember facts about the user across conversations. "+
			"When the user tells you something worth remembering, use add_memory. "+
			"When you need to recall past information, use search_memory.",
		cm,
		agent.WithToolkit(tk),
		agent.WithMiddlewares(memMiddleware),
		agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
	)

	// The memory middleware will automatically search for relevant memories
	// based on the user's input and inject them into the system prompt.
	reply, err := a.Reply(ctx, "What programming language do I prefer for backend work? Also, what's the status of my project?")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("Assistant:", *txt)
	}

	// Show that the conversation was also stored as a new memory.
	fmt.Println("\n=== Memories After Conversation ===")
	all, _ = store.List(ctx, "user-123")
	fmt.Printf("Total memories: %d\n", len(all))
	if len(all) > 0 {
		last := all[len(all)-1]
		preview := last.Text
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		fmt.Printf("Latest: [%s] %s\n", last.ID, preview)
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
