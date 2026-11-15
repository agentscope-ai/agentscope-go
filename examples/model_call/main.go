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

// This example mirrors Python scripts/model_examples/*_call.py.
// It demonstrates three core model API patterns at the raw model layer
// (not the agent layer): streaming call, two-round tool calling, and
// structured output.

func main() {
	as.Init()

	cm, err := loadChatModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	exampleSimpleCall(cm)
	exampleToolCall(cm)
	exampleStructuredOutput(cm)
}

// ---------------------------------------------------------------------------
// Example 1: Simple streaming call
// ---------------------------------------------------------------------------

func exampleSimpleCall(cm model.ChatModel) {
	fmt.Println("=== Simple Streaming Call ===")
	msgs := []*message.Msg{
		message.UserMsg("user", "What is 1 + 1? Answer briefly."),
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

// ---------------------------------------------------------------------------
// Example 2: Two-round tool calling
// ---------------------------------------------------------------------------

func getWeather(city string) string {
	return fmt.Sprintf("The weather in %s is sunny and 25°C.", city)
}

func exampleToolCall(cm model.ChatModel) {
	fmt.Println("=== Tool Call - Round 1 ===")

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
		message.UserMsg("user", "What is the weather in Beijing?"),
	}

	resp, err := cm.Chat(context.Background(), msgs, model.WithTools(tools))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Println("Response:", resp.GetTextContent())

	var toolCalls []message.ToolCallBlock
	for _, b := range resp.Content {
		if tc, ok := b.(message.ToolCallBlock); ok {
			toolCalls = append(toolCalls, tc)
		}
	}

	if len(toolCalls) == 0 {
		fmt.Println("No tool calls returned")
		return
	}

	fmt.Printf("Tool call: %s(%s)\n", toolCalls[0].Name, toolCalls[0].Input)

	var args map[string]any
	_ = json.Unmarshal([]byte(toolCalls[0].Input), &args)
	city, _ := args["city"].(string)
	result := getWeather(city)

	var toolResults []message.ContentBlock
	for _, tc := range toolCalls {
		toolResults = append(toolResults, message.ToolResultBlock{
			Type:   "tool_result",
			ID:     tc.ID,
			Name:   tc.Name,
			Output: result,
			State:  message.ToolResultSuccess,
		})
	}

	assistantMsg := message.AssistantMsg("assistant", resp.Content)
	toolMsg := message.NewMsg("tool", message.RoleAssistant, toolResults)

	msgs = append(msgs, assistantMsg, toolMsg)

	fmt.Println("=== Tool Call - Round 2 (Final) ===")
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
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Example 3: Structured output
// ---------------------------------------------------------------------------

func exampleStructuredOutput(cm model.ChatModel) {
	fmt.Println("=== Structured Output ===")

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"problem":  {"type": "string", "description": "The original problem"},
			"answer":   {"type": "number", "description": "The final numeric answer"},
			"steps":    {"type": "array", "items": {"type": "string"}, "description": "Step-by-step reasoning"}
		},
		"required": ["problem", "answer", "steps"]
	}`)

	msgs := []*message.Msg{
		message.UserMsg("user", "Solve: A train travels at 60 km/h for 2.5 hours. How far does it travel in km?"),
	}

	resp, err := model.GenerateStructuredOutput(context.Background(), cm, msgs, schema)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Println("Structured response:", string(resp))
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
