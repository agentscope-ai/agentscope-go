package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware/memory"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example demonstrates the file-based long-term memory stack added in
// Phase 3:
//
//  1. memory.FileStore — a MemoryStore persisted as JSON Lines in a
//     directory (workspace-friendly). Memories survive process restarts.
//  2. memory.AgenticMemoryMiddleware — file-backed "agentic memory" where
//     the agent maintains a Markdown memory store (<workdir>/Memory/
//     MEMORY.md) and the middleware injects the Auto-Memory instructions
//     plus a token-budgeted MEMORY.md snapshot into the system prompt.
//
// Part 1 (FileStore) runs without any API key. Part 2 (live agent) needs
// one of ANTHROPIC_API_KEY / DASHSCOPE_API_KEY / OPENAI_API_KEY.

func main() {
	as.Init()
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "agentscope-memory-*")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer os.RemoveAll(dir)

	// ---------- Part 1: FileStore persistence (no API key needed) ----------
	fmt.Println("=== Part 1: memory.FileStore (JSON Lines persistence) ===")
	storeDir := filepath.Join(dir, "memories")

	store, err := memory.NewFileStore(storeDir)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	_ = store.Add(ctx, "User prefers Go over Python for backend services", "alice", "assistant")
	_ = store.Add(ctx, "User deploys on Fridays after the integration freeze", "alice", "assistant")
	fmt.Printf("Store file: %s\n\n", filepath.Join(storeDir, memory.FileStoreFilename))

	// A brand-new store instance over the same directory sees everything —
	// this is what survives a process restart.
	reloaded, err := memory.NewFileStore(storeDir)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	mems, _ := reloaded.List(ctx, "alice")
	fmt.Printf("Reloaded %d memories from disk:\n", len(mems))
	for _, m := range mems {
		fmt.Printf("  [%s] %s\n", m.ID, m.Text)
	}
	hits, _ := reloaded.Search(ctx, "friday deploy", "alice", nil)
	fmt.Printf("Search 'friday deploy' -> %d hit(s)\n\n", len(hits))

	// ---------- Part 2: AgenticMemoryMiddleware + live agent ----------
	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("=== Part 2 skipped (", err, ") ===")
		return
	}

	workdir := filepath.Join(dir, "workspace")
	agentic, err := memory.NewAgenticMemory(memory.AgenticMemoryConfig{
		Workdir:   workdir,
		MaxTokens: 2000,
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Seed the memory index as if earlier conversations had saved to it.
	if err := os.MkdirAll(agentic.MemoryDirPath(), 0o755); err != nil {
		fmt.Println("Error:", err)
		return
	}
	seed := "# MEMORY.md\n\n- The user is a senior Go engineer; keep examples terse.\n" +
		"- The user dislikes trailing summaries in answers.\n"
	if err := os.WriteFile(agentic.MemoryMDPath(), []byte(seed), 0o644); err != nil {
		fmt.Println("Error:", err)
		return
	}

	// The prompt the model will see: base prompt + Auto-Memory instructions
	// + a bounded MEMORY.md snapshot.
	prompt := agentic.OnSystemPrompt(ctx, "assistant", "You are a helpful assistant.")
	fmt.Println("=== Part 2: AgenticMemoryMiddleware ===")
	fmt.Println("Injected system prompt (last 400 chars):")
	if len(prompt) > 400 {
		fmt.Println("  ...", prompt[len(prompt)-400:])
	} else {
		fmt.Println(prompt)
	}

	a := agent.NewUnifiedAgent(
		"assistant",
		"You are a helpful assistant.",
		cm,
		agent.WithMiddlewares(agentic),
	)
	reply, err := a.Reply(ctx, "How should I tailor your answers for me?")
	if err != nil {
		fmt.Println("Reply error:", err)
		return
	}
	if txt := reply.GetTextContent("\n"); txt != nil {
		fmt.Println("\nAssistant:", *txt)
	}
	fmt.Println("\nNote: this example builds a bare agent (no file tools registered).")
	fmt.Println("In a real setup, give the agent read/write tools so it can update")
	fmt.Println("  ", agentic.MemoryMDPath())
	fmt.Println("itself; future sessions then see the refreshed snapshot.")
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
	return nil, fmt.Errorf("set ANTHROPIC_API_KEY, DASHSCOPE_API_KEY, or OPENAI_API_KEY for the live-agent part")
}
