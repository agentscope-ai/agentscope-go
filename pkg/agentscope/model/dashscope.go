package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/formatter"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const defaultDashScopeBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

// DashScopeChatModel calls Alibaba Cloud DashScope (Qwen) models via the OpenAI-compatible API.
type DashScopeChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// DashScopeConfig configures DashScopeChatModel.
type DashScopeConfig struct {
	APIKey       string
	SecretAPIKey SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL      string    // Optional, defaults to DashScope compatible endpoint
	Model        string

	HTTPClient    *http.Client
	ClientOptions *ClientOptions
}

// NewDashScopeChatModel creates a ChatModel using the DashScope backend.
func NewDashScopeChatModel(cfg DashScopeConfig) (*DashScopeChatModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("dashscope: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("dashscope: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultDashScopeBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &DashScopeChatModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements ChatModel using the OpenAI-compatible chat/completions path.
func (m *DashScopeChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("dashscope: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody := openAIChatRequest{
		Model:    m.model,
		Messages: convertMessagesWithFormatter(msgs, formatter.NewDashScopeFormatter()),
	}
	if callOpts.Temperature != nil {
		t := float32(*callOpts.Temperature)
		reqBody.Temperature = &t
	}
	if callOpts.MaxTokens != nil {
		reqBody.MaxTokens = callOpts.MaxTokens
	}
	if callOpts.TopP != nil {
		p := float32(*callOpts.TopP)
		reqBody.TopP = &p
	}
	if callOpts.Seed != nil {
		reqBody.Seed = callOpts.Seed
	}
	if len(callOpts.Tools) > 0 {
		reqBody.Tools = callOpts.Tools
	}
	if callOpts.ToolChoice != nil {
		reqBody.ToolChoice = formatToolChoice(callOpts.ToolChoice)
	}
	if callOpts.ThinkingEnable != nil {
		reqBody.EnableThinking = callOpts.ThinkingEnable
	}
	if callOpts.Voice != nil {
		reqBody.Audio = &openAIAudioConfig{Voice: *callOpts.Voice, Format: "pcm16"}
		reqBody.Modalities = []string{"text", "audio"}
	}

	var parsed openAIChatResponse
	if err := httpx.DoJSONRequest(
		ctx,
		m.httpClient,
		http.MethodPost,
		m.baseURL+"/chat/completions",
		reqBody,
		&parsed,
		mergeHeaders(map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + m.apiKey,
		}, m.defaultHeaders),
	); err != nil {
		return nil, fmt.Errorf("dashscope: %w", err)
	}

	return parseOpenAIResponse(&parsed, msgs)
}

// ChatStream implements streaming chat via SSE (OpenAI-compatible format).
func (m *DashScopeChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("dashscope: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody := openAIChatRequest{
		Model:         m.model,
		Messages:      convertMessagesWithFormatter(msgs, formatter.NewDashScopeFormatter()),
		Stream:        true,
		StreamOptions: &openAIStreamOpts{IncludeUsage: true},
	}
	if callOpts.Temperature != nil {
		t := float32(*callOpts.Temperature)
		reqBody.Temperature = &t
	}
	if callOpts.MaxTokens != nil {
		reqBody.MaxTokens = callOpts.MaxTokens
	}
	if callOpts.TopP != nil {
		p := float32(*callOpts.TopP)
		reqBody.TopP = &p
	}
	if callOpts.Seed != nil {
		reqBody.Seed = callOpts.Seed
	}
	if len(callOpts.Tools) > 0 {
		reqBody.Tools = callOpts.Tools
	}
	if callOpts.ToolChoice != nil {
		reqBody.ToolChoice = formatToolChoice(callOpts.ToolChoice)
	}
	if callOpts.ThinkingEnable != nil {
		reqBody.EnableThinking = callOpts.ThinkingEnable
	}
	if callOpts.Voice != nil {
		reqBody.Audio = &openAIAudioConfig{Voice: *callOpts.Voice, Format: "pcm16"}
		reqBody.Modalities = []string{"text", "audio"}
	}

	sseCh, err := httpx.DoSSERequest(
		ctx,
		m.httpClient,
		"POST",
		m.baseURL+"/chat/completions",
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + m.apiKey,
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("dashscope: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processOpenAIStream(ctx, sseCh, outCh)
	return outCh, nil
}

// processOpenAIStream consumes OpenAI-compatible SSE chunks and emits ChatResponse values.
// Shared by DashScope and OpenAI adapters.
// assembleStreamContent builds the accumulated content blocks (thinking, text,
// tool calls, audio) from a streamed OpenAI-style response. Shared by the normal
// completion path and the terminal-error path.
func assembleStreamContent(thinking, text string, accToolCalls map[int]*openAIToolCall, accAudioData []byte, audioBlockID, responseID string) []message.ContentBlock {
	var content []message.ContentBlock
	if thinking != "" {
		content = append(content, message.ThinkingBlock{
			Type:     "thinking",
			ID:       fmt.Sprintf("thinking_%s", responseID),
			Thinking: thinking,
		})
	}
	if text != "" {
		content = append(content, message.TextBlock{
			Type: "text",
			ID:   fmt.Sprintf("text_%s", responseID),
			Text: text,
		})
	}
	for i := 0; i < len(accToolCalls); i++ {
		tc := accToolCalls[i]
		if tc == nil {
			continue
		}
		content = append(content, message.ToolCallBlock{
			Type:  "tool_call",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
			State: message.ToolCallPending,
		})
	}
	if len(accAudioData) > 0 {
		wavData := buildWAV(accAudioData, 24000, 1, 16)
		content = append(content, message.DataBlock{
			Type: "data",
			ID:   audioBlockID,
			Source: message.Base64Source{
				Type:      "base64",
				MediaType: "audio/wav",
				Data:      base64.StdEncoding.EncodeToString(wavData),
			},
		})
	}
	return content
}

func processOpenAIStream(ctx context.Context, sseCh <-chan httpx.SSEEvent, outCh chan<- ChatResponse) {
	processOpenAIStreamCfg(ctx, sseCh, outCh, false)
}

// processOpenAIStreamCfg is processOpenAIStream with an xAI-specific usage
// option. includeReasoningInOutput adds usage.completion_tokens_details.
// reasoning_tokens into OutputTokens (upstream #2461). Only xAI needs this:
// its completion_tokens EXCLUDE reasoning tokens, while OpenAI/DashScope/
// Moonshot/DeepSeek already include them — adding it there would double-bill.
func processOpenAIStreamCfg(ctx context.Context, sseCh <-chan httpx.SSEEvent, outCh chan<- ChatResponse, includeReasoningInOutput bool) {
	defer close(outCh)

	var (
		accText      strings.Builder
		accThinking  strings.Builder
		accToolCalls = make(map[int]*openAIToolCall) // index → accumulated tool call
		responseID   string
		modelName    string
		usage        *ChatUsage

		accAudioData    []byte
		audioBlockID    string
		audioHeaderSent bool
		finishReason    string
	)

	for evt := range sseCh {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// A terminal transport/scanner error: surface it as a final response with
		// Error set (plus whatever was accumulated) instead of ending silently.
		if evt.Err != nil {
			select {
			case outCh <- ChatResponse{
				Content:    assembleStreamContent(accThinking.String(), accText.String(), accToolCalls, accAudioData, audioBlockID, responseID),
				IsLast:     true,
				ID:         responseID,
				Usage:      usage,
				ModelName:  modelName,
				StopReason: normalizeStopReason(finishReason),
				Error:      evt.Err,
			}:
			case <-ctx.Done():
			}
			return
		}

		if evt.Data == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(evt.Data), &chunk); err != nil {
			continue
		}

		if chunk.ID != "" {
			responseID = chunk.ID
		}
		if chunk.Model != "" {
			modelName = chunk.Model
		}

		if chunk.Usage != nil {
			outTokens := chunk.Usage.CompletionTokens
			if includeReasoningInOutput && chunk.Usage.CompletionTokensDetails != nil {
				outTokens += chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			usage = &ChatUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: outTokens,
			}
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
			finishReason = *fr
		}

		delta := chunk.Choices[0].Delta

		// Accumulate text
		if delta.Content != "" {
			accText.WriteString(delta.Content)
		}

		// Accumulate reasoning/thinking content
		if delta.ReasoningContent != "" {
			accThinking.WriteString(delta.ReasoningContent)
		}

		// Accumulate tool calls
		for _, tc := range delta.ToolCalls {
			existing, ok := accToolCalls[tc.Index]
			if !ok {
				accToolCalls[tc.Index] = &openAIToolCall{
					ID:   tc.ID,
					Type: tc.Type,
				}
				accToolCalls[tc.Index].Function.Name = tc.Function.Name
				accToolCalls[tc.Index].Function.Arguments = tc.Function.Arguments
			} else {
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Function.Name != "" {
					existing.Function.Name += tc.Function.Name
				}
				existing.Function.Arguments += tc.Function.Arguments
			}
		}

		// Accumulate audio and fold transcript into text
		if delta.Audio != nil {
			if delta.Audio.Data != "" {
				pcm, decErr := base64.StdEncoding.DecodeString(delta.Audio.Data)
				if decErr != nil {
					continue
				}
				accAudioData = append(accAudioData, pcm...)
				if audioBlockID == "" {
					audioBlockID = agentscope.GenerateID()
				}
				var payload []byte
				if !audioHeaderSent {
					payload = append(buildStreamingWAVHeader(24000, 1, 16), pcm...)
					audioHeaderSent = true
				} else {
					payload = pcm
				}
				audioBlock := message.DataBlock{
					Type: "data",
					ID:   audioBlockID,
					Source: message.Base64Source{
						Type:      "base64",
						MediaType: "audio/wav",
						Data:      base64.StdEncoding.EncodeToString(payload),
					},
				}
				resp := ChatResponse{
					Content:   []message.ContentBlock{audioBlock},
					IsLast:    false,
					ID:        responseID,
					ModelName: modelName,
				}
				select {
				case outCh <- resp:
				case <-ctx.Done():
					return
				}
			}
			if delta.Audio.Transcript != "" {
				accText.WriteString(delta.Audio.Transcript)
			}
		}

		// Emit delta chunk
		var deltaContent []message.ContentBlock
		if delta.Content != "" {
			deltaContent = append(deltaContent, message.TextBlock{
				Type: "text",
				Text: delta.Content,
			})
		}
		if delta.ReasoningContent != "" {
			deltaContent = append(deltaContent, message.ThinkingBlock{
				Type:     "thinking",
				Thinking: delta.ReasoningContent,
			})
		}

		if len(deltaContent) > 0 || len(delta.ToolCalls) > 0 {
			resp := ChatResponse{
				Content:   deltaContent,
				IsLast:    false,
				ID:        responseID,
				ModelName: modelName,
			}
			select {
			case outCh <- resp:
			case <-ctx.Done():
				return
			}
		}
	}

	finalResp := ChatResponse{
		Content:    assembleStreamContent(accThinking.String(), accText.String(), accToolCalls, accAudioData, audioBlockID, responseID),
		IsLast:     true,
		ID:         responseID,
		CreatedAt:  time.Now().Format(message.TimestampFormat),
		Usage:      usage,
		ModelName:  modelName,
		StopReason: normalizeStopReason(finishReason),
	}
	select {
	case outCh <- finalResp:
	case <-ctx.Done():
	}
}

// DisableThinkingOptions implements ThinkingDisabler (upstream #2140).
func (m *DashScopeChatModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}

// CountTokens estimates token count by byte length / 4.
func (m *DashScopeChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// --- shared OpenAI-compatible request/response types ---

type openAIChatMessage struct {
	Role             string              `json:"role"`
	Content          interface{}         `json:"content"`
	Name             string              `json:"name,omitempty"`
	ToolCalls        []openAIToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string              `json:"tool_call_id,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	Audio            *openAIMessageAudio `json:"audio,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIAudioConfig struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

type openAIAudioDelta struct {
	Data       string `json:"data,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

type openAIMessageAudio struct {
	Data       string `json:"data"`
	Transcript string `json:"transcript"`
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
	Stream   bool                `json:"stream,omitempty"`

	Temperature         *float32     `json:"temperature,omitempty"`
	MaxTokens           *int         `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int         `json:"max_completion_tokens,omitempty"`
	TopP                *float32     `json:"top_p,omitempty"`
	Tools               []ToolSchema `json:"tools,omitempty"`
	ToolChoice          any          `json:"tool_choice,omitempty"`
	Seed                *int64       `json:"seed,omitempty"`
	// EnableThinking maps to the OpenAI-compatible enable_thinking toggle
	// (qwen3 and similar); only DashScope sets it. Thinking on DeepSeek and
	// Moonshot uses the Thinking object below instead (upstream #2140).
	EnableThinking *bool `json:"enable_thinking,omitempty"`
	// Thinking carries provider-specific reasoning configuration: DeepSeek
	// and Moonshot disable thinking via {"type":"disabled"} (upstream #2140
	// structured-output fallback).
	Thinking      *openAIThinking    `json:"thinking,omitempty"`
	Audio         *openAIAudioConfig `json:"audio,omitempty"`
	Modalities    []string           `json:"modalities,omitempty"`
	StreamOptions *openAIStreamOpts  `json:"stream_options,omitempty"`
}

// openAIThinking is the OpenAI-compatible thinking configuration object
// ({"type":"disabled"} turns reasoning off on DeepSeek/Moonshot).
type openAIThinking struct {
	Type string `json:"type"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int               `json:"index"`
		Message      openAIChatMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

// openAIStreamChunk represents a single chunk in the OpenAI streaming response.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int              `json:"index"`
		Delta        openAIChunkDelta `json:"delta"`
		FinishReason *string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

type openAIChunkDelta struct {
	Role             string                 `json:"role,omitempty"`
	Content          string                 `json:"content,omitempty"`
	ReasoningContent string                 `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIStreamToolCall `json:"tool_calls,omitempty"`
	Audio            *openAIAudioDelta      `json:"audio,omitempty"`
}

type openAIStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// formatToolChoice converts our ToolChoice to the OpenAI API format.
func formatToolChoice(tc *ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case "auto", "none", "required":
		return tc.Mode
	default:
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Mode},
		}
	}
}

// parseOpenAIResponse converts an OpenAI-compatible response to our ChatResponse.
func parseOpenAIResponse(parsed *openAIChatResponse, msgs []*message.Msg) (*ChatResponse, error) {
	return parseOpenAIResponseCfg(parsed, msgs, false)
}

// parseOpenAIResponseCfg is parseOpenAIResponse with the xAI-specific
// includeReasoningInOutput usage option (see processOpenAIStreamCfg).
func parseOpenAIResponseCfg(parsed *openAIChatResponse, msgs []*message.Msg, includeReasoningInOutput bool) (*ChatResponse, error) {
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response")
	}

	choice := parsed.Choices[0]
	var content []message.ContentBlock

	// Extract thinking content
	if choice.Message.ReasoningContent != "" {
		content = append(content, message.ThinkingBlock{
			Type:     "thinking",
			ID:       fmt.Sprintf("thinking_%s", parsed.ID),
			Thinking: choice.Message.ReasoningContent,
		})
	}

	// Extract text content
	if textContent, ok := choice.Message.Content.(string); ok && textContent != "" {
		content = append(content, message.TextBlock{
			Type: "text",
			ID:   fmt.Sprintf("text_%s", parsed.ID),
			Text: textContent,
		})
	} else if choice.Message.Content != nil {
		b, err := json.Marshal(choice.Message.Content)
		if err == nil && string(b) != "null" && string(b) != `""` {
			content = append(content, message.TextBlock{
				Type: "text",
				ID:   fmt.Sprintf("text_%s", parsed.ID),
				Text: string(b),
			})
		}
	}

	// Extract tool calls
	for _, tc := range choice.Message.ToolCalls {
		content = append(content, message.ToolCallBlock{
			Type:  "tool_call",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
			State: message.ToolCallPending,
		})
	}

	// Extract audio
	if choice.Message.Audio != nil && choice.Message.Audio.Data != "" {
		pcm, decErr := base64.StdEncoding.DecodeString(choice.Message.Audio.Data)
		if decErr != nil {
			return nil, fmt.Errorf("decode audio data: %w", decErr)
		}
		wavData := buildWAV(pcm, 24000, 1, 16)
		content = append(content, message.DataBlock{
			Type: "data",
			ID:   fmt.Sprintf("audio_%s", parsed.ID),
			Source: message.Base64Source{
				Type:      "base64",
				MediaType: "audio/wav",
				Data:      base64.StdEncoding.EncodeToString(wavData),
			},
		})
		if choice.Message.Audio.Transcript != "" && len(content) > 0 {
			hasText := false
			for _, b := range content {
				if _, ok := b.(message.TextBlock); ok {
					hasText = true
					break
				}
			}
			if !hasText {
				content = append(content, message.TextBlock{
					Type: "text",
					ID:   fmt.Sprintf("text_%s", parsed.ID),
					Text: choice.Message.Audio.Transcript,
				})
			}
		}
	}

	var usage *ChatUsage
	if parsed.Usage != nil {
		outTokens := parsed.Usage.CompletionTokens
		if includeReasoningInOutput && parsed.Usage.CompletionTokensDetails != nil {
			outTokens += parsed.Usage.CompletionTokensDetails.ReasoningTokens
		}
		usage = &ChatUsage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: outTokens,
		}
		if parsed.Usage.PromptTokensDetails != nil {
			usage.CacheInputTokens = parsed.Usage.PromptTokensDetails.CachedTokens
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
