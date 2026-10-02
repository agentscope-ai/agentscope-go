package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const (
	defaultAnthropicBaseURL         = "https://api.anthropic.com"
	defaultAnthropicVersion         = "2023-06-01"
	defaultAnthropicMaxOutputTokens = 1024
)

// AnthropicChatModel wraps Anthropic's Messages API.
type AnthropicChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	version        string
	maxOutputTok   int
	defaultHeaders map[string]string
	promptCaching  bool // if true, mark system + last tool with cache_control:{type:ephemeral}

	httpClient *http.Client
}

// AnthropicConfig configures AnthropicChatModel.
type AnthropicConfig struct {
	APIKey          string
	SecretAPIKey    SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL         string
	Model           string
	Version         string
	MaxOutputTokens int
	HTTPClient      *http.Client
	ClientOptions   *ClientOptions
	// PromptCaching enables Anthropic prompt caching: the system prompt and the
	// last tool definition are marked with cache_control:{type:"ephemeral"} so
	// Anthropic reuses the input-token prefix across turns. Default false to
	// avoid changing the wire format for existing callers.
	PromptCaching bool
}

// NewAnthropicChatModel creates a ChatModel backed by Anthropic.
func NewAnthropicChatModel(cfg *AnthropicConfig) (*AnthropicChatModel, error) {
	apiKey := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("anthropic: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("anthropic: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultAnthropicBaseURL
	}
	ver := cfg.Version
	if ver == "" {
		ver = defaultAnthropicVersion
	}
	maxTok := cfg.MaxOutputTokens
	if maxTok <= 0 {
		maxTok = defaultAnthropicMaxOutputTokens
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &AnthropicChatModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		version:        ver,
		maxOutputTok:   maxTok,
		defaultHeaders: defHeaders,
		promptCaching:  cfg.PromptCaching,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// anthropicCache is the Anthropic cache_control marker attached to a system
// text block or a tool definition. Currently only "ephemeral" is supported
// (5-minute TTL, no beta header needed).
type anthropicCache struct {
	Type string `json:"type"`
}

// anthropicSystemBlock is one element in a structured system prompt (used when
// prompt caching is enabled; without caching the system is sent as a flat
// string per the wire format's shorthand).
type anthropicSystemBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl *anthropicCache `json:"cache_control,omitempty"`
}

// applyPromptCaching returns the value to set on anthropicRequest.System +
// the (possibly mutated) tools slice. When caching is disabled the caller
// gets back its inputs unchanged, so wire format matches pre-M8a exactly.
// When enabled: system becomes a single-element []anthropicSystemBlock with
// cache_control on the block, and the LAST tool gets cache_control (Anthropic
// treats a cache breakpoint on a tool as "cache the entire tool list up to
// and including this one"). Empty system returns nil so we do not send a
// stray empty block.
func (m *AnthropicChatModel) applyPromptCaching(systemText string, tools []anthropicTool) (interface{}, []anthropicTool) {
	if !m.promptCaching {
		return systemText, tools
	}
	var sys interface{}
	if systemText != "" {
		sys = []anthropicSystemBlock{{
			Type: "text", Text: systemText,
			CacheControl: &anthropicCache{Type: "ephemeral"},
		}}
	}
	if len(tools) > 0 {
		out := make([]anthropicTool, len(tools))
		copy(out, tools)
		out[len(out)-1].CacheControl = &anthropicCache{Type: "ephemeral"}
		tools = out
	}
	return sys, tools
}

type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type anthropicRequest struct {
	Model      string               `json:"model"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	System     interface{}          `json:"system,omitempty"` // string OR []anthropicSystemBlock (M8a)
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking   *anthropicThinking   `json:"thinking,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`           // "auto", "any", "tool", "none"
	Name string `json:"name,omitempty"` // only for type="tool"
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *anthropicCache `json:"cache_control,omitempty"` // M8a
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Role    string `json:"role"`
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		Thinking  string          `json:"thinking,omitempty"`
		Signature string          `json:"signature,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

// Chat implements the non-streaming ChatModel call for Anthropic.
func (m *AnthropicChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("anthropic: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	am, system := convertMessagesToAnthropic(msgs)

	reqBody := anthropicRequest{
		Model:     m.model,
		Messages:  am,
		MaxTokens: m.maxOutputTok,
		System:    system,
	}

	// Thinking parameters
	if callOpts.ThinkingEnable != nil && *callOpts.ThinkingEnable {
		budget := reqBody.MaxTokens / 2
		if callOpts.ThinkingBudget != nil && *callOpts.ThinkingBudget > 0 {
			budget = *callOpts.ThinkingBudget
		}
		if budget >= reqBody.MaxTokens {
			reqBody.MaxTokens = budget + 1024
		}
		reqBody.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	} else if callOpts.ThinkingEnable != nil && !*callOpts.ThinkingEnable {
		// Upstream #2140: explicitly disable thinking (structured-output
		// fallback rung; Anthropic rejects forced tool_choice with thinking).
		reqBody.Thinking = &anthropicThinking{Type: "disabled"}
	}

	if len(callOpts.Tools) > 0 {
		for _, ts := range callOpts.Tools {
			reqBody.Tools = append(reqBody.Tools, anthropicTool{
				Name:        ts.Function.Name,
				Description: ts.Function.Description,
				InputSchema: ts.Function.Parameters,
			})
		}
	}

	if callOpts.ToolChoice != nil && len(reqBody.Tools) > 0 {
		reqBody.ToolChoice = formatToolChoiceAnthropic(callOpts.ToolChoice)
	}

	// M8a: apply prompt caching last so it sees the finalized system + tools.
	reqBody.System, reqBody.Tools = m.applyPromptCaching(system, reqBody.Tools)

	// Retry loop
	maxRetries := callOpts.MaxRetries
	retryDelay := callOpts.RetryDelay
	if retryDelay == 0 {
		retryDelay = time.Second
	}

	var parsed anthropicResponse
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		lastErr = httpx.DoJSONRequest(
			ctx,
			m.httpClient,
			http.MethodPost,
			m.baseURL+"/v1/messages",
			reqBody,
			&parsed,
			mergeHeaders(map[string]string{
				"Content-Type":      "application/json",
				"x-api-key":         m.apiKey,
				"anthropic-version": m.version,
			}, m.defaultHeaders),
		)
		if lastErr == nil {
			break
		}
		if !IsRetryableError(lastErr) {
			return nil, fmt.Errorf("anthropic: %w", lastErr)
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("anthropic: %w", lastErr)
	}

	if len(parsed.Content) == 0 {
		return nil, fmt.Errorf("anthropic: empty content")
	}

	var content []message.ContentBlock
	for _, c := range parsed.Content {
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

// ChatStream implements streaming chat via Anthropic's SSE event format.
func (m *AnthropicChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("anthropic: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	am, system := convertMessagesToAnthropic(msgs)

	reqBody := anthropicStreamRequest{
		Model:     m.model,
		Messages:  am,
		MaxTokens: m.maxOutputTok,
		System:    system,
		Stream:    true,
	}

	if callOpts.ThinkingEnable != nil && *callOpts.ThinkingEnable {
		budget := reqBody.MaxTokens / 2
		if callOpts.ThinkingBudget != nil && *callOpts.ThinkingBudget > 0 {
			budget = *callOpts.ThinkingBudget
		}
		if budget >= reqBody.MaxTokens {
			reqBody.MaxTokens = budget + 1024
		}
		reqBody.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	} else if callOpts.ThinkingEnable != nil && !*callOpts.ThinkingEnable {
		// Upstream #2140: explicitly disable thinking (structured-output
		// fallback rung; Anthropic rejects forced tool_choice with thinking).
		reqBody.Thinking = &anthropicThinking{Type: "disabled"}
	}

	if len(callOpts.Tools) > 0 {
		for _, ts := range callOpts.Tools {
			reqBody.Tools = append(reqBody.Tools, anthropicTool{
				Name:        ts.Function.Name,
				Description: ts.Function.Description,
				InputSchema: ts.Function.Parameters,
			})
		}
	}

	if callOpts.ToolChoice != nil && len(reqBody.Tools) > 0 {
		reqBody.ToolChoice = formatToolChoiceAnthropic(callOpts.ToolChoice)
	}

	// M8a: apply prompt caching last so it sees the finalized system + tools.
	reqBody.System, reqBody.Tools = m.applyPromptCaching(system, reqBody.Tools)

	sseCh, err := httpx.DoSSERequest(
		ctx,
		m.httpClient,
		"POST",
		m.baseURL+"/v1/messages",
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type":      "application/json",
			"x-api-key":         m.apiKey,
			"anthropic-version": m.version,
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processAnthropicStream(ctx, sseCh, outCh)
	return outCh, nil
}

type anthropicStreamRequest struct {
	Model      string               `json:"model"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	System     interface{}          `json:"system,omitempty"` // string OR []anthropicSystemBlock (M8a)
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking   *anthropicThinking   `json:"thinking,omitempty"`
	Stream     bool                 `json:"stream"`
}

// Anthropic SSE event data types

type anthropicMessageStart struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
}

type anthropicContentBlockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type      string          `json:"type"` // "text", "thinking", "tool_use"
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Text      string          `json:"text,omitempty"`
		Thinking  string          `json:"thinking,omitempty"`
		Signature string          `json:"signature,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
	} `json:"content_block"`
}

type anthropicContentBlockDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"` // "text_delta", "thinking_delta", "input_json_delta", "signature_delta"
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		Signature   string `json:"signature,omitempty"`
	} `json:"delta"`
}

type anthropicMessageDelta struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

func processAnthropicStream(ctx context.Context, sseCh <-chan httpx.SSEEvent, outCh chan<- ChatResponse) {
	defer close(outCh)

	var (
		responseID               string
		modelName                string
		inputTokens              int
		outputTokens             int
		cacheCreationInputTokens int
		cacheInputTokens         int
		accBlocks                []anthropicAccBlock
		sawMessageStop           bool
		finishReason             string
		// streamErr carries an Anthropic "error" SSE event, which arrives
		// inside an otherwise successful HTTP 200 stream.
		streamErr error
	)

	for evt := range sseCh {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Terminal transport/scanner error: surface it instead of ending silently.
		if evt.Err != nil {
			select {
			case outCh <- ChatResponse{
				IsLast:    true,
				ID:        responseID,
				ModelName: modelName,
				Usage: &ChatUsage{
					InputTokens:              inputTokens,
					OutputTokens:             outputTokens,
					CacheCreationInputTokens: cacheCreationInputTokens,
					CacheInputTokens:         cacheInputTokens,
				},
				Error: evt.Err,
			}:
			case <-ctx.Done():
			}
			return
		}

		decode := func(dst any) bool {
			if !strings.HasPrefix(strings.TrimSpace(evt.Data), "{") {
				streamErr = fmt.Errorf("anthropic: invalid %s event object", evt.Event)
				return false
			}
			if err := json.Unmarshal([]byte(evt.Data), dst); err != nil {
				streamErr = fmt.Errorf("anthropic: invalid %s event: %w", evt.Event, err)
				return false
			}
			return true
		}
		switch evt.Event {
		case "message_start":
			var ms anthropicMessageStart
			if decode(&ms) {
				responseID = ms.Message.ID
				modelName = ms.Message.Model
				if ms.Message.Usage != nil {
					inputTokens = ms.Message.Usage.InputTokens
				}
			}
			// Also try to extract cache tokens from message_start
			var raw map[string]json.RawMessage
			if json.Unmarshal([]byte(evt.Data), &raw) == nil {
				if msgRaw, ok := raw["message"]; ok {
					var msgData struct {
						Usage *struct {
							CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
							CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						} `json:"usage"`
					}
					if json.Unmarshal(msgRaw, &msgData) == nil && msgData.Usage != nil {
						cacheCreationInputTokens = msgData.Usage.CacheCreationInputTokens
						cacheInputTokens = msgData.Usage.CacheReadInputTokens
					}
				}
			}

		case "content_block_start":
			var cbs anthropicContentBlockStart
			if decode(&cbs) {
				if cbs.Index != len(accBlocks) {
					streamErr = fmt.Errorf("anthropic: invalid content block start index %d", cbs.Index)
					break
				}
				block := anthropicAccBlock{blockType: cbs.ContentBlock.Type, id: cbs.ContentBlock.ID, name: cbs.ContentBlock.Name, text: cbs.ContentBlock.Text, signature: cbs.ContentBlock.Signature}
				var initial []message.ContentBlock
				switch block.blockType {
				case "text":
					if block.text != "" {
						initial = []message.ContentBlock{message.TextBlock{Type: "text", Text: block.text}}
					}
				case "thinking":
					block.text = cbs.ContentBlock.Thinking
					if block.text != "" {
						initial = []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Thinking: block.text}}
					}
				case "tool_use":
					block.text = string(cbs.ContentBlock.Input)
				}
				accBlocks = append(accBlocks, block)
				if len(initial) > 0 {
					select {
					case outCh <- ChatResponse{Content: initial, ID: responseID, ModelName: modelName}:
					case <-ctx.Done():
						return
					}
				}
			}

		case "content_block_delta":
			var cbd anthropicContentBlockDelta
			if decode(&cbd) {
				idx := cbd.Index
				if idx < 0 || idx >= len(accBlocks) || accBlocks[idx].closed {
					streamErr = fmt.Errorf("anthropic: delta for inactive content block %d", idx)
					break
				}
				{
					expected := map[string]string{"text_delta": "text", "thinking_delta": "thinking", "signature_delta": "thinking", "input_json_delta": "tool_use"}
					if kind, known := expected[cbd.Delta.Type]; known && kind != accBlocks[idx].blockType {
						streamErr = fmt.Errorf("anthropic: delta type does not match content block %d", idx)
						break
					}
					switch cbd.Delta.Type {
					case "text_delta":
						accBlocks[idx].text += cbd.Delta.Text
						// Emit delta
						resp := ChatResponse{
							Content: []message.ContentBlock{message.TextBlock{
								Type: "text",
								Text: cbd.Delta.Text,
							}},
							IsLast:    false,
							ID:        responseID,
							ModelName: modelName,
						}
						select {
						case outCh <- resp:
						case <-ctx.Done():
							return
						}
					case "thinking_delta":
						accBlocks[idx].text += cbd.Delta.Thinking
						resp := ChatResponse{
							Content: []message.ContentBlock{message.ThinkingBlock{
								Type:     "thinking",
								Thinking: cbd.Delta.Thinking,
							}},
							IsLast:    false,
							ID:        responseID,
							ModelName: modelName,
						}
						select {
						case outCh <- resp:
						case <-ctx.Done():
							return
						}
					case "input_json_delta":
						if !accBlocks[idx].inputStarted {
							accBlocks[idx].text = ""
							accBlocks[idx].inputStarted = true
						}
						accBlocks[idx].text += cbd.Delta.PartialJSON
					case "signature_delta":
						accBlocks[idx].signature += cbd.Delta.Signature
					}
				}
			}

		case "content_block_stop":
			var stop struct {
				Index int `json:"index"`
			}
			if decode(&stop) {
				if stop.Index < 0 || stop.Index >= len(accBlocks) || accBlocks[stop.Index].closed {
					streamErr = fmt.Errorf("anthropic: invalid content block stop index %d", stop.Index)
				} else {
					accBlocks[stop.Index].closed = true
				}
			}

		case "message_delta":
			var md anthropicMessageDelta
			if decode(&md) {
				finishReason = md.Delta.StopReason
				if md.Usage != nil {
					outputTokens = md.Usage.OutputTokens
				}
			}

		case "message_stop":
			var stop struct {
				Type string `json:"type"`
			}
			if decode(&stop) {
				sawMessageStop = true
			}

		case "error":
			// Anthropic reports mid-stream failures (overloaded_error, rate
			// limits, an aborted generation) as an EVENT, not as an HTTP
			// status: the response is already 200 and open. Skipping it left
			// the only visible symptom as the misleading "stream ended
			// without message_stop (truncated)".
			var ae anthropicStreamError
			if json.Unmarshal([]byte(evt.Data), &ae) == nil && ae.Error.Type != "" {
				streamErr = fmt.Errorf("anthropic: stream error %s: %s", ae.Error.Type, ae.Error.Message)
			} else {
				data := evt.Data
				if len(data) > 512 {
					data = data[:512] + "..."
				}
				streamErr = fmt.Errorf("anthropic: unparseable stream error event: %s", data)
			}
		}
		if streamErr != nil {
			break
		}
	}

	// Build final accumulated response
	var finalContent []message.ContentBlock
	for _, ab := range accBlocks {
		switch ab.blockType {
		case "text":
			finalContent = append(finalContent, message.TextBlock{
				Type: "text",
				ID:   fmt.Sprintf("text_%s", responseID),
				Text: ab.text,
			})
		case "thinking":
			tb := message.ThinkingBlock{
				Type:     "thinking",
				ID:       fmt.Sprintf("thinking_%s", responseID),
				Thinking: ab.text,
			}
			if ab.signature != "" {
				tb.Extra = map[string]any{"signature": ab.signature}
			}
			finalContent = append(finalContent, tb)
		case "tool_use":
			finalContent = append(finalContent, message.ToolCallBlock{
				Type:  "tool_call",
				ID:    ab.id,
				Name:  ab.name,
				Input: ab.text,
				State: message.ToolCallPending,
			})
		}
	}

	var usage *ChatUsage
	if inputTokens > 0 || outputTokens > 0 {
		usage = &ChatUsage{
			InputTokens:              inputTokens,
			OutputTokens:             outputTokens,
			CacheCreationInputTokens: cacheCreationInputTokens,
			CacheInputTokens:         cacheInputTokens,
		}
	}

	finalResp := ChatResponse{
		Content:    finalContent,
		IsLast:     true,
		ID:         responseID,
		CreatedAt:  time.Now().Format(message.TimestampFormat),
		Usage:      usage,
		ModelName:  modelName,
		StopReason: normalizeStopReason(finishReason),
	}
	// Upstream #2350 class: never end silently. A stream that terminates
	// without message_stop is truncated (proxy reset, upstream abort); the
	// final response still carries whatever accumulated, plus an Error so
	// consumers can distinguish completion from truncation. ctx cancellation
	// is an abandoned-consumer path, not a truncation.
	switch {
	case streamErr != nil && !sawMessageStop && ctx.Err() == nil:
		finalResp.Error = fmt.Errorf("%w (stream also ended without message_stop)", streamErr)
	case streamErr != nil:
		finalResp.Error = streamErr
	case !sawMessageStop && ctx.Err() == nil:
		finalResp.Error = fmt.Errorf("anthropic: stream ended without message_stop (truncated)")
	}
	select {
	case outCh <- finalResp:
	case <-ctx.Done():
	}
}

// anthropicStreamError is the payload of a mid-stream "error" SSE event.
type anthropicStreamError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicAccBlock struct {
	blockType    string // "text", "thinking", "tool_use"
	id           string
	name         string
	text         string
	signature    string
	closed       bool
	inputStarted bool
}

// CountTokens estimates token count.
// DisableThinkingOptions implements ThinkingDisabler (upstream #2140).
func (m *AnthropicChatModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}

func (m *AnthropicChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// convertMessagesToAnthropic converts internal Msg instances into Anthropic messages.
// It uses the AnthropicFormatter for full block-type support (thinking signatures,
// multimodal DataBlocks, HintBlocks, tool results), then converts from
// []map[string]any to []anthropicMessage.
func convertMessagesToAnthropic(msgs []*message.Msg) ([]anthropicMessage, string) {
	f := formatter.NewAnthropicFormatter()
	system := formatter.ExtractSystemPrompt(msgs)

	formatted, err := f.Format(msgs)
	if err != nil {
		// Fallback to simple text extraction
		return convertMessagesToAnthropicFallback(msgs)
	}

	out := make([]anthropicMessage, 0, len(formatted))
	for _, m := range formatted {
		role, _ := m["role"].(string)
		out = append(out, anthropicMessage{Role: role, Content: m["content"]})
	}
	return out, system
}

func convertMessagesToAnthropicFallback(msgs []*message.Msg) ([]anthropicMessage, string) {
	var system string
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if m.Role == message.RoleSystem {
			if txt := m.GetTextContent("\n"); txt != nil {
				if system != "" {
					system += "\n"
				}
				system += *txt
			}
			continue
		}
		role := string(m.Role)
		if role == "" {
			role = "user"
		}
		if txt := m.GetTextContent("\n"); txt != nil {
			out = append(out, anthropicMessage{Role: role, Content: *txt})
		} else {
			out = append(out, anthropicMessage{Role: role, Content: ""})
		}
	}
	return out, system
}

// formatToolChoiceAnthropic converts a generic ToolChoice to Anthropic's format.
// "required" → {"type":"any"}, "auto" → {"type":"auto"}, name → {"type":"tool","name":"..."}
func formatToolChoiceAnthropic(tc *ToolChoice) *anthropicToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case "auto":
		return &anthropicToolChoice{Type: "auto"}
	case "none":
		return nil
	case "required":
		return &anthropicToolChoice{Type: "any"}
	default:
		if tc.Mode != "" {
			return &anthropicToolChoice{Type: "tool", Name: tc.Mode}
		}
		return nil
	}
}
