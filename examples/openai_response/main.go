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

// This example mirrors Python scripts/model_examples/openai_response_call.py.
// It demonstrates the OpenAI Responses API (distinct from Chat Completions),
// including streaming, tool calling, and structured output via the
// Responses-specific model adapter.

func main() {
	as.Init()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENAI_API_KEY to run this example")
		return
	}

	cm, err := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{
		APIKey: apiKey,
		Model:  "gpt-4.1",
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	exampleSimpleCall(cm)
	exampleToolCall(cm)
	exampleStructuredOutput(cm)
}

// ---------------------------------------------------------------------------
// Example 1: Simple call via Responses API
// ---------------------------------------------------------------------------

func exampleSimpleCall(cm model.ChatModel) {
	fmt.Println("=== Responses API: Simple Call ===")
	msgs := []*message.Msg{
		message.UserMsg("user", "What is 1 + 1? Answer briefly."),
	}

	resp, err := cm.Chat(context.Background(), msgs)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("Response:", resp.GetTextContent())
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Example 2: Tool calling via Responses API
// ---------------------------------------------------------------------------

func exampleToolCall(cm model.ChatModel) {
	fmt.Println("=== Responses API: Tool Call ===")

	weatherSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"city": {"type": "string", "description": "The city name"}
		},
		"required": ["city"]
	}`)

	tools := []model.ToolSchema{{
		Type: "function",
		Function: model.ToolFunction{
			Name:        "get_weather",
			Description: "Get the current weather for a city",
			Parameters:  weatherSchema,
		},
	}}

	msgs := []*message.Msg{
		message.UserMsg("user", "What is the weather in Tokyo?"),
	}

	resp, err := cm.Chat(context.Background(), msgs, model.WithTools(tools))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Println("Response:", resp.GetTextContent())
	for _, b := range resp.Content {
		if tc, ok := b.(message.ToolCallBlock); ok {
			fmt.Printf("Tool call: %s(%s)\n", tc.Name, tc.Input)
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Example 3: Structured output via Responses API
// ---------------------------------------------------------------------------

func exampleStructuredOutput(cm model.ChatModel) {
	fmt.Println("=== Responses API: Structured Output ===")

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"problem": {"type": "string"},
			"answer":  {"type": "number"},
			"steps":   {"type": "array", "items": {"type": "string"}}
		},
		"required": ["problem", "answer", "steps"]
	}`)

	msgs := []*message.Msg{
		message.UserMsg("user", "Solve: A car travels at 80 km/h for 3 hours. How far?"),
	}

	resp, err := model.GenerateStructuredOutput(context.Background(), cm, msgs, schema)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Println("Structured response:", string(resp))
	fmt.Println()
}
