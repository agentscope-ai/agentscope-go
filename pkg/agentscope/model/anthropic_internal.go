package model

import (
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const (
	anthropicMinThinkingBudget      = 1024
	anthropicThinkingOutputHeadroom = 1024
)

// baseAnthropicRequest shares only the Messages protocol envelope. Thinking,
// output configuration, authentication and caching remain provider decisions.
func baseAnthropicRequest(model string, maxTokens int, msgs []*message.Msg, opts *CallOptions) *anthropicRequest {
	am, system := convertMessagesToAnthropic(msgs)
	r := &anthropicRequest{Model: model, Messages: am, MaxTokens: maxTokens, System: system}
	for _, ts := range opts.Tools {
		r.Tools = append(r.Tools, anthropicTool{Name: ts.Function.Name, Description: ts.Function.Description, InputSchema: ts.Function.Parameters})
	}
	if opts.ToolChoice != nil && len(r.Tools) > 0 {
		r.ToolChoice = formatToolChoiceAnthropic(opts.ToolChoice)
	}
	return r
}

func (m *AnthropicChatModel) request(msgs []*message.Msg, opts *CallOptions) (*anthropicRequest, error) {
	r := baseAnthropicRequest(m.model, m.maxOutputTok, msgs, opts)
	if opts.MaxTokens != nil {
		if *opts.MaxTokens <= 0 {
			return nil, fmt.Errorf("anthropic: max_tokens must be positive")
		}
		r.MaxTokens = *opts.MaxTokens
	}
	if opts.ThinkingEnable != nil && *opts.ThinkingEnable {

		budget := max(anthropicMinThinkingBudget, r.MaxTokens/2)
		if opts.ThinkingBudget != nil {
			budget = *opts.ThinkingBudget
			if budget < anthropicMinThinkingBudget {
				return nil, fmt.Errorf("anthropic: thinking budget must be at least %d tokens", anthropicMinThinkingBudget)
			}
		}
		if budget >= r.MaxTokens {
			if opts.MaxTokens != nil {
				return nil, fmt.Errorf("anthropic: thinking budget must be less than the per-call max_tokens limit")
			}
			// The configured/default limit may grow, but a per-call ceiling never does.
			if budget > int(^uint(0)>>1)-anthropicThinkingOutputHeadroom {
				return nil, fmt.Errorf("anthropic: thinking budget leaves no representable output limit")
			}
			r.MaxTokens = budget + anthropicThinkingOutputHeadroom
		}
		r.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	} else if opts.ThinkingEnable != nil {
		r.Thinking = &anthropicThinking{Type: "disabled"}
	}
	system, _ := r.System.(string)
	r.System, r.Tools = m.applyPromptCaching(system, r.Tools)
	return r, nil
}

// parseAnthropicResponse retains the original Anthropic response semantics.
// Provider-specific stop-reason handling is applied by MiniMax after parsing.
func parseAnthropicResponse(parsed *anthropicResponse) (*ChatResponse, error) {
	if len(parsed.Content) == 0 {
		return nil, fmt.Errorf("anthropic: empty content")
	}

	var content []message.ContentBlock
	for i := range parsed.Content {
		c := &parsed.Content[i]
		switch c.Type {
		case "text":
			content = append(content, message.TextBlock{
				Type: "text",
				ID:   fmt.Sprintf("text_%s", parsed.ID),
				Text: c.Text,
			})
		case "thinking":
			tb := message.ThinkingBlock{
				Type:     "thinking",
				ID:       fmt.Sprintf("thinking_%s", parsed.ID),
				Thinking: c.Thinking,
			}
			if c.Signature != "" {
				tb.Extra = map[string]any{"signature": c.Signature}
			}
			content = append(content, tb)
		case "redacted_thinking":
			content = append(content, message.RedactedThinkingBlock{
				Type: "redacted_thinking", ID: fmt.Sprintf("redacted_thinking_%s", parsed.ID), Data: c.Data,
			})
		case "tool_use":
			inputStr := string(c.Input)
			content = append(content, message.ToolCallBlock{
				Type:  "tool_call",
				ID:    c.ID,
				Name:  c.Name,
				Input: inputStr,
				State: message.ToolCallPending,
			})
		}
	}

	var usage *ChatUsage
	if parsed.Usage != nil {
		usage = &ChatUsage{
			InputTokens:              parsed.Usage.InputTokens,
			OutputTokens:             parsed.Usage.OutputTokens,
			CacheCreationInputTokens: parsed.Usage.CacheCreationInputTokens,
			CacheInputTokens:         parsed.Usage.CacheReadInputTokens,
			// Anthropic's input_tokens excludes cache-read/cache-creation
			// tokens; the context-window fill level is the sum.
			TotalInputTokens: parsed.Usage.InputTokens +
				parsed.Usage.CacheCreationInputTokens +
				parsed.Usage.CacheReadInputTokens,
		}
	}

	return &ChatResponse{
		Content:   content,
		IsLast:    true,
		ID:        parsed.ID,
		CreatedAt: time.Now().Format(message.TimestampFormat),
		Usage:     usage,
		ModelName: parsed.Model,
	}, nil
}
