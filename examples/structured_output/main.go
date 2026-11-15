package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// This example demonstrates GenerateStructuredOutput.
// The model is forced to produce JSON matching the given schema,
// using a synthetic tool call mechanism under the hood.

type MovieReview struct {
	Title     string   `json:"title"`
	Rating    float64  `json:"rating"`
	Summary   string   `json:"summary"`
	Pros      []string `json:"pros"`
	Cons      []string `json:"cons"`
	Recommend bool     `json:"recommend"`
}

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"title":     {"type": "string", "description": "Movie title"},
			"rating":    {"type": "number", "description": "Rating out of 10", "minimum": 0, "maximum": 10},
			"summary":   {"type": "string", "description": "One-sentence summary"},
			"pros":      {"type": "array", "items": {"type": "string"}, "description": "List of positive aspects"},
			"cons":      {"type": "array", "items": {"type": "string"}, "description": "List of negative aspects"},
			"recommend": {"type": "boolean", "description": "Whether you'd recommend this movie"}
		},
		"required": ["title", "rating", "summary", "pros", "cons", "recommend"]
	}`)

	msgs := []*message.Msg{
		message.SystemMsg("system", "You are a movie critic. Provide structured reviews."),
		message.UserMsg("user", "Review the movie 'Inception' (2010) by Christopher Nolan."),
	}

	fmt.Println("Generating structured movie review...")
	result, err := model.GenerateStructuredOutput(context.Background(), cm, msgs, schema)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// Parse and display the structured output
	var review MovieReview
	if err := json.Unmarshal(result, &review); err != nil {
		fmt.Println("Parse error:", err)
		fmt.Println("Raw JSON:", string(result))
		return
	}

	fmt.Printf("\n=== Movie Review ===\n")
	fmt.Printf("Title: %s\n", review.Title)
	fmt.Printf("Rating: %.1f/10\n", review.Rating)
	fmt.Printf("Summary: %s\n", review.Summary)
	fmt.Printf("Recommend: %v\n", review.Recommend)
	fmt.Printf("\nPros:\n")
	for _, p := range review.Pros {
		fmt.Printf("  + %s\n", p)
	}
	fmt.Printf("Cons:\n")
	for _, c := range review.Cons {
		fmt.Printf("  - %s\n", c)
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
