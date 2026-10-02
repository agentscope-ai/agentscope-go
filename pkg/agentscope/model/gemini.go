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

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// GeminiChatModel wraps the Google Gemini generateContent API.
type GeminiChatModel struct {
	apiKey         string
	baseURL        string
	model          string
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// GeminiConfig configures GeminiChatModel.
type GeminiConfig struct {
	APIKey        string
	SecretAPIKey  SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL       string
	Model         string
	HTTPClient    *http.Client
	ClientOptions *ClientOptions
}

// NewGeminiChatModel creates a ChatModel backed by Google Gemini.
func NewGeminiChatModel(cfg GeminiConfig) (*GeminiChatModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("gemini: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("gemini: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultGeminiBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &GeminiChatModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements the ChatModel interface using Gemini generateContent.
func (m *GeminiChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("gemini: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody := m.buildRequest(msgs, callOpts)

	// Send the API key via the x-goog-api-key header rather than a ?key= query
	// param so it never appears in URLs (which httpx logs and wraps into errors).
	url := fmt.Sprintf("%s/models/%s:generateContent", m.baseURL, m.model)

	var parsed geminiResponse
	if err := httpx.DoJSONRequest(
		ctx,
		m.httpClient,
		http.MethodPost,
		url,
		reqBody,
		&parsed,
		mergeHeaders(map[string]string{
			"Content-Type":   "application/json",
			"x-goog-api-key": m.apiKey,
		}, m.defaultHeaders),
	); err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}

	return parseGeminiResponse(parsed)
}

// ChatStream implements streaming via Gemini streamGenerateContent.
func (m *GeminiChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("gemini: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody := m.buildRequest(msgs, callOpts)

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", m.baseURL, m.model)

	sseCh, err := httpx.DoSSERequest(
		ctx,
		m.httpClient,
		"POST",
		url,
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type":   "application/json",
			"x-goog-api-key": m.apiKey,
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go processGeminiStream(ctx, sseCh, outCh)
	return outCh, nil
}

// CountTokens estimates token count.
func (m *GeminiChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

func (m *GeminiChatModel) buildRequest(msgs []*message.Msg, callOpts *CallOptions) geminiRequest {
	var systemInstruction *geminiContent
	var contents []geminiContent

	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		role := string(msg.Role)

		if role == "system" {
			txt := geminiTextContent(msg)
			if txt == "" {
				continue
			}
			systemInstruction = &geminiContent{
				Parts: []geminiPart{{Text: txt}},
			}
			continue
		}

		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}

		var parts []geminiPart

		// Tool results
		toolResults := msg.GetContentBlocks(message.ContentBlockToolResult)
		if len(toolResults) > 0 {
			for _, tr := range toolResults {
				trb := tr.(message.ToolResultBlock)
				parts = append(parts, geminiPart{
					FunctionResponse: &geminiFunctionResponse{
						Name: trb.Name,
						Response: map[string]any{
							"result": formatter.ConvertToolResultToString(trb.Output),
						},
					},
				})
			}
			contents = append(contents, geminiContent{Role: "user", Parts: parts})
			continue
		}

		// Tool calls
		toolCalls := msg.GetContentBlocks(message.ContentBlockToolCall)
		if len(toolCalls) > 0 {
			for _, tc := range toolCalls {
				tcb := tc.(message.ToolCallBlock)
				var args map[string]any
				_ = json.Unmarshal([]byte(tcb.Input), &args)
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tcb.Name,
						Args: args,
					},
				})
			}
			contents = append(contents, geminiContent{Role: "model", Parts: parts})
			continue
		}

		// Text content
		if txt := geminiTextContent(msg); txt != "" {
			parts = append(parts, geminiPart{Text: txt})
		}

		if len(parts) > 0 {
			contents = append(contents, geminiContent{Role: geminiRole, Parts: parts})
		}
	}

	req := geminiRequest{
		Contents:          contents,
		SystemInstruction: systemInstruction,
	}

	// Generation config
	genCfg := geminiGenerationConfig{}
	hasGenCfg := false
	if callOpts.Temperature != nil {
		t := float32(*callOpts.Temperature)
		genCfg.Temperature = &t
		hasGenCfg = true
	}
	if callOpts.MaxTokens != nil {
		genCfg.MaxOutputTokens = callOpts.MaxTokens
		hasGenCfg = true
	}
	if callOpts.TopP != nil {
		p := float32(*callOpts.TopP)
		genCfg.TopP = &p
		hasGenCfg = true
	}
	if hasGenCfg {
		req.GenerationConfig = &genCfg
	}

	// Tools
	if len(callOpts.Tools) > 0 {
		var funcs []geminiFunctionDeclaration
		for _, t := range callOpts.Tools {
			funcs = append(funcs, geminiFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  SanitizeSchemaForGemini(t.Function.Parameters),
			})
		}
		req.Tools = []geminiTool{{FunctionDeclarations: funcs}}
	}

	return req
}

// --- Gemini API types ---

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"system_instruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generation_config,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiGenerationConfig struct {
	Temperature     *float32 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	TopP            *float32 `json:"topP,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"function_declarations"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	ToolUsePromptTokenCount int `json:"toolUsePromptTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

// geminiUsageToChatUsage maps Gemini usageMetadata onto ChatUsage
// (upstream #2406): tool-use prompt tokens count as input, thought tokens
// count as output, and cached-content tokens feed the cache accounting.
func geminiUsageToChatUsage(u *geminiUsage) *ChatUsage {
	if u == nil {
		return nil
	}
	return &ChatUsage{
		InputTokens:      u.PromptTokenCount + u.ToolUsePromptTokenCount,
		OutputTokens:     u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheInputTokens: u.CachedContentTokenCount,
	}
}

func parseGeminiResponse(parsed geminiResponse) (*ChatResponse, error) {
	if len(parsed.Candidates) == 0 {
		return nil, fmt.Errorf("gemini: empty candidates")
	}

	candidate := parsed.Candidates[0]
	var content []message.ContentBlock

	for idx, part := range candidate.Content.Parts {
		if part.Thought {
			content = append(content, message.ThinkingBlock{
				Type:     "thinking",
				Thinking: part.Text,
			})
		} else if part.FunctionCall != nil {
			argsJSON, _ := json.Marshal(part.FunctionCall.Args)
			content = append(content, message.ToolCallBlock{
				Type:  "tool_call",
				ID:    fmt.Sprintf("gemini-call-%s-%d", part.FunctionCall.Name, idx),
				Name:  part.FunctionCall.Name,
				Input: string(argsJSON),
				State: message.ToolCallPending,
			})
		} else if part.Text != "" {
			content = append(content, message.TextBlock{
				Type: "text",
				Text: part.Text,
			})
		}
	}

	usage := geminiUsageToChatUsage(parsed.UsageMetadata)

	return &ChatResponse{
		Content:   content,
		IsLast:    true,
		CreatedAt: time.Now().Format(message.TimestampFormat),
		Usage:     usage,
		ModelName: parsed.ModelVersion,
	}, nil
}

func processGeminiStream(ctx context.Context, sseCh <-chan httpx.SSEEvent, outCh chan<- ChatResponse) {
	defer close(outCh)

	var (
		accText      string
		accThinking  string
		toolCallIdx  int
		toolCalls    []message.ContentBlock
		finishReason string
		streamErr    error
		modelName    string
		usage        *ChatUsage
	)

	for evt := range sseCh {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Terminal transport/scanner error: surface it instead of ending silently.
		if evt.Err != nil {
			var content []message.ContentBlock
			if accText != "" {
				content = append(content, message.TextBlock{Type: "text", Text: accText})
			}
			select {
			case outCh <- ChatResponse{Content: content, IsLast: true, ModelName: modelName, Usage: usage, Error: evt.Err}:
			case <-ctx.Done():
			}
			return
		}

		var chunk geminiResponse
		if err := json.Unmarshal([]byte(evt.Data), &chunk); err != nil {
			streamErr = fmt.Errorf("gemini: invalid stream chunk: %w", err)
			break
		}

		if chunk.ModelVersion != "" {
			modelName = chunk.ModelVersion
		}

		if u := geminiUsageToChatUsage(chunk.UsageMetadata); u != nil {
			usage = u
		}

		if len(chunk.Candidates) == 0 {
			continue
		}
		if reason := chunk.Candidates[0].FinishReason; reason != "" {
			finishReason = reason
		}

		var deltaContent []message.ContentBlock
		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.Thought {
				accThinking += part.Text
				deltaContent = append(deltaContent, message.ThinkingBlock{
					Type:     "thinking",
					Thinking: part.Text,
				})
			} else if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				tc := message.ToolCallBlock{
					Type:  "tool_call",
					ID:    fmt.Sprintf("gemini-call-%s-%d", part.FunctionCall.Name, toolCallIdx),
					Name:  part.FunctionCall.Name,
					Input: string(argsJSON),
					State: message.ToolCallPending,
				}
				toolCallIdx++
				toolCalls = append(toolCalls, tc)
				deltaContent = append(deltaContent, tc)
			} else if part.Text != "" {
				accText += part.Text
				deltaContent = append(deltaContent, message.TextBlock{
					Type: "text",
					Text: part.Text,
				})
			}
		}

		if len(deltaContent) > 0 {
			resp := ChatResponse{
				Content:   deltaContent,
				IsLast:    false,
				ModelName: modelName,
			}
			select {
			case outCh <- resp:
			case <-ctx.Done():
				return
			}
		}
	}

	// Final accumulated response
	var finalContent []message.ContentBlock
	if accThinking != "" {
		finalContent = append(finalContent, message.ThinkingBlock{
			Type:     "thinking",
			Thinking: accThinking,
		})
	}
	if accText != "" {
		finalContent = append(finalContent, message.TextBlock{
			Type: "text",
			Text: accText,
		})
	}

	finalContent = append(finalContent, toolCalls...)
	if streamErr == nil && finishReason != "STOP" && finishReason != "MAX_TOKENS" {
		streamErr = fmt.Errorf("gemini: stream ended without successful finishReason (%q)", finishReason)
	}
	finalResp := ChatResponse{
		Content:    finalContent,
		IsLast:     true,
		CreatedAt:  time.Now().Format(message.TimestampFormat),
		Usage:      usage,
		ModelName:  modelName,
		StopReason: normalizeStopReason(finishReason),
		Error:      streamErr,
	}
	select {
	case outCh <- finalResp:
	case <-ctx.Done():
	}
}

// geminiTextContent filters empty blocks before joining so separators cannot
// turn an otherwise empty message into provider content. Whitespace is retained.
func geminiTextContent(msg *message.Msg) string {
	var texts []string
	for _, block := range msg.GetContentBlocks(message.ContentBlockText) {
		if text, ok := block.(message.TextBlock); ok && text.Text != "" {
			texts = append(texts, text.Text)
		}
	}
	return strings.Join(texts, "\n")
}
