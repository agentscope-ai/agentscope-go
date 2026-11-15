package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// This example demonstrates UnifiedAgent combined with a simple RAG setup.
// Knowledge is retrieved from an in-memory index and injected into the
// system prompt before the model call.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("load chat model error:", err)
		return
	}

	// Build an in-memory index and insert documents.
	idx := rag.NewInMemoryIndex()
	docs := []rag.Document{
		{ID: "1", Content: "Go is a statically typed, compiled programming language."},
		{ID: "2", Content: "AgentScope-Go is a Go framework for building multi-agent LLM applications."},
	}
	if err := idx.AddDocuments(context.Background(), docs); err != nil {
		panic(err)
	}

	kb := rag.NewSimpleKnowledgeBase("docs", idx)

	// Retrieve relevant knowledge for the user's question.
	ctx := context.Background()
	userQuestion := "What is agentscope-go? Please briefly introduce it."

	results, err := kb.Query(ctx, userQuestion, 3)
	if err != nil {
		fmt.Println("knowledge retrieval error:", err)
		return
	}

	// Build a system prompt augmented with retrieved knowledge.
	var knowledgeSection strings.Builder
	if len(results) > 0 {
		knowledgeSection.WriteString("\n\n[KNOWLEDGE]\n")
		for _, r := range results {
			knowledgeSection.WriteString("- ")
			knowledgeSection.WriteString(r.Content)
			knowledgeSection.WriteString("\n")
		}
	}

	sysPrompt := "You are an expert on Go and multi-agent frameworks. " +
		"You may reference information from the knowledge base when answering." +
		knowledgeSection.String()

	a := agent.NewUnifiedAgent("assistant", sysPrompt, cm)

	reply, err := a.Reply(ctx, userQuestion)
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
