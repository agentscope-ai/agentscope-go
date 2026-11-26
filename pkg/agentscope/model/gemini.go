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
	raiseOnContentPolicy bool
	apiKey               string
	baseURL              string
	model                string
	defaultHeaders       map[string]string
	httpClient           *http.Client
}

// GeminiConfig configures GeminiChatModel.
type GeminiConfig struct {
	// RaiseOnContentPolicy annotates explicit output policy signals in response Error.
	RaiseOnContentPolicy bool
	APIKey               string
	SecretAPIKey         SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL              string
	Model                string
	HTTPClient           *http.Client
	ClientOptions        *ClientOptions
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
		raiseOnContentPolicy: cfg.RaiseOnContentPolicy,
		apiKey:               apiKey,
		baseURL:              base,
		model:                cfg.Model,
		defaultHeaders:       defHeaders,
		httpClient:           defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
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

	reqBody, err := m.buildRequest(msgs, callOpts)
	if err != nil {
		return nil, err
	}

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

	resp, err := parseGeminiResponse(parsed)
	if err == nil && m.raiseOnContentPolicy {
		if e := classifyContentPolicy("gemini", parsed.Candidates[0].FinishReason, "", false, resp.Error); e != nil {
			resp.Error = e
		}
	}
	return resp, err
}

// ChatStream implements streaming via Gemini streamGenerateContent.
func (m *GeminiChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("gemini: httpx: new request: net/http: nil Context")
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("gemini: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody, err := m.buildRequest(msgs, callOpts)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", m.baseURL, m.model)

	streamCtx, cancel := context.WithCancel(ctx)
	sseCh, err := httpx.DoSSERequest(
		streamCtx,
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
		cancel()
		return nil, fmt.Errorf("gemini: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	if m.raiseOnContentPolicy {
		reason := make(chan string, 1)
		go processGeminiStream(streamCtx, sseCh, outCh, func(value string) { reason <- value })
		return observeContentPolicy(streamCtx, cancel, outCh, reason, "gemini"), nil
	}
	go func() { defer cancel(); processGeminiStream(streamCtx, sseCh, outCh) }()
	return outCh, nil
}

// CountTokens estimates token count.
func (m *GeminiChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

func (m *GeminiChatModel) buildRequest(msgs []*message.Msg, callOpts *CallOptions) (geminiRequest, error) {
	thinking, err := m.thinkingConfig(callOpts)
	if err != nil {
		return geminiRequest{}, err
	}
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

		contents = appendGeminiMessage(contents, msg)
	}

	req := geminiRequest{
		Contents:          contents,
		SystemInstruction: systemInstruction,
	}

	// Generation config
	genCfg := geminiGenerationConfig{ThinkingConfig: thinking}
	hasGenCfg := thinking != nil
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

	return req, nil
}

// appendGeminiMessage renders a stored message without changing its blocks.
func appendGeminiMessage(contents []geminiContent, msg *message.Msg) []geminiContent {
	textRole := "user"
	if msg.Role == message.RoleAssistant {
		textRole = "model"
	}
	// Only group parts within this message. A user-role result segment must
	// also remain separate from any ordinary user text next to it.
	segmentStart := len(contents)
	previousIsResult := false
	appendPart := func(role string, isResult bool, part geminiPart) {
		if len(contents) == segmentStart || contents[len(contents)-1].Role != role || previousIsResult != isResult {
			contents = append(contents, geminiContent{Role: role})
		}
		current := &contents[len(contents)-1]
		current.Parts = append(current.Parts, part)
		previousIsResult = isResult
	}

	var texts []string
	flushText := func() {
		if len(texts) > 0 {
			appendPart(textRole, false, geminiPart{Text: strings.Join(texts, "\n")})
			texts = texts[:0]
		}
	}
	for _, block := range msg.Content {
		switch value := block.(type) {
		case message.TextBlock:
			if value.Text != "" {
				texts = append(texts, value.Text)
			}
		case message.ToolCallBlock:
			flushText()
			var args map[string]any
			_ = json.Unmarshal([]byte(value.Input), &args)
			signature, _ := value.Extra["thought_signature"].(string)
			appendPart("model", false, geminiPart{
				FunctionCall: &geminiFunctionCall{
					ID:   value.ID,
					Name: value.Name,
					Args: args,
				},
				ThoughtSignature: signature,
			})
		case message.ToolResultBlock:
			flushText()
			responseKey := "result"
			if value.State == message.ToolResultError {
				responseKey = "error"
			}
			appendPart("user", true, geminiPart{
				FunctionResponse: &geminiFunctionResponse{
					ID:   value.ID,
					Name: value.Name,
					Response: map[string]any{
						responseKey: formatter.ConvertToolResultToString(value.Output),
					},
				},
			})
		}
	}
	flushText()
	return contents
}

// isGeminiLevelModel mirrors Python AgentScope at 3fef100e. Keep the gate
// exact: unknown model IDs retain the budget-era contract.
func isGeminiLevelModel(name string) bool {
	switch name[strings.LastIndex(name, "/")+1:] {
	case "gemini-4-argon", "gemini-4-carbon":
		return true
	default:
		return false
	}
}

func (m *GeminiChatModel) thinkingConfig(opts *CallOptions) (*geminiThinkingConfig, error) {
	levelModel := isGeminiLevelModel(m.model)
	if levelModel && opts.ThinkingBudget != nil {
		return nil, fmt.Errorf("gemini: thinking budget is unsupported for Argon and Carbon; use thinking level")
	}
	var level *string
	if opts.ThinkingLevel != nil {
		if !levelModel {
			return nil, fmt.Errorf("gemini: thinking level is supported only for Argon and Carbon")
		}
		normalized := strings.ToUpper(*opts.ThinkingLevel)
		switch normalized {
		case "LOW", "MEDIUM", "HIGH":
			level = &normalized
		default:
			return nil, fmt.Errorf("gemini: thinking level must be LOW, MEDIUM or HIGH")
		}
	}
	if levelModel {
		if opts.ThinkingEnable == nil && level == nil {
			return nil, nil
		}
		return &geminiThinkingConfig{IncludeThoughts: opts.ThinkingEnable, ThinkingLevel: level}, nil
	}
	if opts.ThinkingEnable == nil {
		return nil, nil
	}
	budget := 0
	if *opts.ThinkingEnable {
		budget = 1024
		if opts.ThinkingBudget != nil && *opts.ThinkingBudget > 0 {
			budget = *opts.ThinkingBudget
		}
	}
	return &geminiThinkingConfig{IncludeThoughts: opts.ThinkingEnable, ThinkingBudget: &budget}, nil
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
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiThinkingConfig struct {
	IncludeThoughts *bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int    `json:"thinkingBudget,omitempty"`
	ThinkingLevel   *string `json:"thinkingLevel,omitempty"`
}

type geminiGenerationConfig struct {
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
	Temperature     *float32              `json:"temperature,omitempty"`
	MaxOutputTokens *int                  `json:"maxOutputTokens,omitempty"`
	TopP            *float32              `json:"topP,omitempty"`
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
			id := part.FunctionCall.ID
			if id == "" {
				id = fmt.Sprintf("gemini-call-%s-%d", part.FunctionCall.Name, idx)
			}
			tc := message.ToolCallBlock{
				Type:  "tool_call",
				ID:    id,
				Name:  part.FunctionCall.Name,
				Input: string(argsJSON),
				State: message.ToolCallPending,
			}
			// Thinking models require this call's signature echoed back with
			// the same history part (upstream #2463).
			if part.ThoughtSignature != "" {
				tc.Extra = map[string]any{"thought_signature": part.ThoughtSignature}
			}
			content = append(content, tc)
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

func processGeminiStream(ctx context.Context, sseCh <-chan httpx.SSEEvent, outCh chan<- ChatResponse, reason ...func(string)) {
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

	if len(reason) > 0 {
		defer func() { reason[0](finishReason) }()
	}

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
				id := part.FunctionCall.ID
				if id == "" {
					id = fmt.Sprintf("gemini-call-%s-%d", part.FunctionCall.Name, toolCallIdx)
				}
				tc := message.ToolCallBlock{
					Type:  "tool_call",
					ID:    id,
					Name:  part.FunctionCall.Name,
					Input: string(argsJSON),
					State: message.ToolCallPending,
				}
				// Thinking models require this call's signature echoed back with
				// the same history part (upstream #2463).
				if part.ThoughtSignature != "" {
					tc.Extra = map[string]any{"thought_signature": part.ThoughtSignature}
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
