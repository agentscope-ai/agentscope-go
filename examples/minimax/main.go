// Direct MiniMax tool use with a local weather fixture and a streamed answer.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cm, err := model.NewMiniMaxChatModel(&model.MiniMaxConfig{
		SecretAPIKey:    model.NewSecretStr(os.Getenv("MINIMAX_API_KEY")),
		Model:           os.Getenv("MINIMAX_MODEL"),
		BaseURL:         os.Getenv("MINIMAX_BASE_URL"),
		MaxOutputTokens: 4096,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tools := []model.ToolSchema{{Type: "function", Function: model.ToolFunction{
		Name: "weather", Description: "Return a local demonstration weather reading for Beijing.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","enum":["Beijing"]}},"required":["city"]}`),
	}}}
	msgs := []*message.Msg{message.UserMsg("user", "Use weather for Beijing, then summarize the reading in one sentence.")}
	response, err := cm.Chat(ctx, msgs, model.WithTools(tools), model.WithToolChoice(&model.ToolChoice{Mode: "auto"}))
	if err != nil {
		return err
	}
	if response.StopReason == model.StopReasonLength {
		return fmt.Errorf("output limit reached; increase MaxOutputTokens")
	}
	var results []message.ContentBlock
	for _, block := range response.Content {
		call, ok := block.(message.ToolCallBlock)
		if !ok {
			continue
		}
		var input struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
			return err
		}
		if call.Name != "weather" || input.City != "Beijing" {
			return fmt.Errorf("unexpected tool request")
		}
		results = append(results, message.ToolResultBlock{Type: "tool_result", ID: call.ID, Output: "Local demonstration data: Beijing, 18 C, clear. This is not a live weather reading."})
	}
	if len(results) == 0 {
		if response.GetTextContent() == "" {
			return fmt.Errorf("model returned no text or tool request")
		}
		fmt.Println(response.GetTextContent())
		return nil
	}
	// Preserve the entire assistant response, including thinking and signatures.
	msgs = append(msgs, response.ToMsg("agent"), message.NewMsg("tools", message.RoleUser, results))
	stream, err := cm.ChatStream(ctx, msgs)
	if err != nil {
		return err
	}
	finished := false
	for chunk := range stream {
		if chunk.Error != nil {
			return chunk.Error
		}
		if chunk.IsLast {
			if chunk.StopReason == model.StopReasonLength {
				return fmt.Errorf("output limit reached; increase MaxOutputTokens")
			}
			if chunk.GetTextContent() == "" {
				return fmt.Errorf("model returned no final text")
			}
			finished = true
			fmt.Println()
			if chunk.Usage != nil {
				fmt.Printf("Tokens: new input=%d cached=%d output=%d\n", chunk.Usage.InputTokens, chunk.Usage.CacheInputTokens, chunk.Usage.OutputTokens)
			}
		} else {
			fmt.Print(chunk.GetTextContent())
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !finished {
		return fmt.Errorf("stream closed without a final response")
	}
	return nil
}
