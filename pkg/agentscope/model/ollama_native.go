package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// OllamaNativeChatModel wraps a local Ollama instance via the native
// /api/chat endpoint, allowing per-request options (num_ctx, think) the
// OpenAI-compatible endpoint cannot set. No API key is required. Native tool
// calling is not supported yet: passing tools via WithTools returns an error.
// WithThinking/WithThinkingDisabled map to the native think boolean; the
// budget argument has no native counterpart and is ignored. Reasoning the
// server reports in message.thinking (Ollama 0.9+) is surfaced as
// ThinkingBlock content.
type OllamaNativeChatModel struct {
	baseURL        string
	model          string
	numCtx         int
	defaultHeaders map[string]string
	httpClient     *http.Client
}

// OllamaNativeConfig configures OllamaNativeChatModel.
type OllamaNativeConfig struct {
	BaseURL       string
	Model         string
	HTTPClient    *http.Client
	ClientOptions *ClientOptions
	// NumCtx is sent as options.num_ctx on every request and reported through
	// ContextSizer so context compression measures against that window. Zero
	// leaves the server default in place and reports an unknown context size.
	NumCtx int
}

// NewOllamaNativeChatModel creates a ChatModel backed by a local Ollama
// instance via the native /api/chat endpoint.
func NewOllamaNativeChatModel(cfg OllamaNativeConfig) (*OllamaNativeChatModel, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("ollama native: Model is required")
	}
	if cfg.NumCtx < 0 {
		return nil, fmt.Errorf("ollama native: NumCtx must not be negative")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultOllamaBaseURL
	}
	var defHeaders map[string]string
	if cfg.ClientOptions != nil {
		defHeaders = cfg.ClientOptions.DefaultHeaders
	}
	return &OllamaNativeChatModel{
		baseURL:        base,
		model:          cfg.Model,
		numCtx:         cfg.NumCtx,
		defaultHeaders: defHeaders,
		httpClient:     defaultHTTPClient(cfg.HTTPClient, cfg.ClientOptions),
	}, nil
}

// Chat implements the ChatModel interface.
func (m *OllamaNativeChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("ollama native: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody, err := m.buildRequest(msgs, callOpts, false)
	if err != nil {
		return nil, err
	}

	var parsed ollamaNativeChatResponse
	if err := httpx.DoJSONRequest(
		ctx,
		m.httpClient,
		http.MethodPost,
		m.baseURL+"/api/chat",
		reqBody,
		&parsed,
		mergeHeaders(map[string]string{
			"Content-Type": "application/json",
		}, m.defaultHeaders),
	); err != nil {
		return nil, fmt.Errorf("ollama native: %w", err)
	}

	resp := &ChatResponse{
		IsLast:     true,
		CreatedAt:  time.Now().Format(message.TimestampFormat),
		Usage:      ollamaNativeUsage(&parsed),
		ModelName:  parsed.Model,
		StopReason: mapOllamaNativeStopReason(parsed.DoneReason),
	}
	if resp.ModelName == "" {
		resp.ModelName = m.model
	}
	if parsed.Message.Thinking != "" {
		resp.Content = append(resp.Content, message.ThinkingBlock{Type: "thinking", Thinking: parsed.Message.Thinking})
	}
	if parsed.Message.Content != "" {
		resp.Content = append(resp.Content, message.TextBlock{Type: "text", Text: parsed.Message.Content})
	}
	return resp, nil
}

// ChatStream implements streaming chat via NDJSON (one JSON object per line).
func (m *OllamaNativeChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("ollama native: msgs must not be empty")
	}

	callOpts := &CallOptions{}
	for _, opt := range opts {
		opt(callOpts)
	}

	reqBody, err := m.buildRequest(msgs, callOpts, true)
	if err != nil {
		return nil, err
	}

	ndjsonCh, err := httpx.DoNDJSONRequest(
		ctx,
		m.httpClient,
		http.MethodPost,
		m.baseURL+"/api/chat",
		reqBody,
		mergeHeaders(map[string]string{
			"Content-Type": "application/json",
		}, m.defaultHeaders),
	)
	if err != nil {
		return nil, fmt.Errorf("ollama native: %w", err)
	}

	outCh := make(chan ChatResponse, 16)
	go m.processStream(ctx, ndjsonCh, outCh)
	return outCh, nil
}

// CountTokens estimates token count by byte length / 4.
func (m *OllamaNativeChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return countTokensByBytes(msgs, tools)
}

// ContextSize implements ContextSizer with OllamaNativeConfig.NumCtx; zero
// means unknown.
func (m *OllamaNativeChatModel) ContextSize() int {
	return m.numCtx
}

func (m *OllamaNativeChatModel) buildRequest(msgs []*message.Msg, callOpts *CallOptions, stream bool) (*ollamaNativeChatRequest, error) {
	if len(callOpts.Tools) > 0 {
		return nil, fmt.Errorf("ollama native: tools are not supported by the native transport yet; " +
			"use OllamaChatModel (OpenAI-compatible endpoint) for tool calling")
	}

	reqBody := &ollamaNativeChatRequest{
		Model:    m.model,
		Messages: convertMessagesToOllamaNative(msgs),
		Stream:   stream,
	}

	var options *ollamaNativeOptions
	ensureOptions := func() *ollamaNativeOptions {
		if options == nil {
			options = &ollamaNativeOptions{}
		}
		return options
	}
	if m.numCtx > 0 {
		ensureOptions().NumCtx = m.numCtx
	}
	if callOpts.MaxTokens != nil {
		ensureOptions().NumPredict = callOpts.MaxTokens
	}
	if callOpts.Temperature != nil {
		ensureOptions().Temperature = callOpts.Temperature
	}
	if callOpts.TopP != nil {
		ensureOptions().TopP = callOpts.TopP
	}
	if callOpts.Seed != nil {
		ensureOptions().Seed = callOpts.Seed
	}
	reqBody.Options = options

	if callOpts.ThinkingEnable != nil {
		reqBody.Think = callOpts.ThinkingEnable
	}

	if rf := callOpts.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			reqBody.Format = json.RawMessage(`"json"`)
		case "json_schema":
			if len(rf.JSONSchema) > 0 {
				reqBody.Format = rf.JSONSchema
			}
		}
	}

	return reqBody, nil
}

// processStream consumes native /api/chat NDJSON frames. Usage and done_reason
// appear only on the final (done=true) frame; a stream that ends without one is
// truncated: the final response carries Error plus the accumulated content.
func (m *OllamaNativeChatModel) processStream(ctx context.Context, ndjsonCh <-chan httpx.NDJSONEvent, outCh chan<- ChatResponse) {
	defer close(outCh)

	var (
		accThinking strings.Builder
		accText     strings.Builder
		modelName   string
		usage       *ChatUsage
		doneReason  string
		completed   bool
		streamErr   error
	)

	for evt := range ndjsonCh {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// A terminal transport/scanner error: surface it as a final response with
		// Error set (plus whatever was accumulated) instead of ending silently.
		if evt.Err != nil {
			streamErr = evt.Err
			break
		}

		var frame ollamaNativeChatResponse
		if err := json.Unmarshal(evt.Data, &frame); err != nil {
			streamErr = fmt.Errorf("ollama native: invalid stream chunk: %w", err)
			break
		}

		if frame.Model != "" {
			modelName = frame.Model
		}

		// Thinking is published before text within a chunk, matching the block
		// order of the final assembled response.
		var deltaContent []message.ContentBlock
		if frame.Message.Thinking != "" {
			accThinking.WriteString(frame.Message.Thinking)
			deltaContent = append(deltaContent, message.ThinkingBlock{Type: "thinking", Thinking: frame.Message.Thinking})
		}
		if frame.Message.Content != "" {
			accText.WriteString(frame.Message.Content)
			deltaContent = append(deltaContent, message.TextBlock{Type: "text", Text: frame.Message.Content})
		}
		if len(deltaContent) > 0 {
			select {
			case outCh <- ChatResponse{Content: deltaContent, ModelName: modelName}:
			case <-ctx.Done():
				return
			}
		}

		if frame.Done {
			completed = true
			usage = ollamaNativeUsage(&frame)
			doneReason = frame.DoneReason
			break
		}
	}

	// Keep draining after an early exit (done frame or terminal error): a server
	// that keeps writing frames would otherwise block parseNDJSONStream on send
	// forever, leaking its goroutine and holding the response body open.
	if completed || streamErr != nil {
		go func() {
			for range ndjsonCh {
			}
		}()
	}

	if ctx.Err() != nil {
		return
	}
	if !completed && streamErr == nil {
		streamErr = fmt.Errorf("ollama native: stream ended without a done frame (truncated)")
	}
	if modelName == "" {
		modelName = m.model
	}

	final := ChatResponse{
		IsLast:     true,
		CreatedAt:  time.Now().Format(message.TimestampFormat),
		Usage:      usage,
		ModelName:  modelName,
		StopReason: mapOllamaNativeStopReason(doneReason),
		Error:      streamErr,
	}
	if thinking := accThinking.String(); thinking != "" {
		final.Content = append(final.Content, message.ThinkingBlock{Type: "thinking", Thinking: thinking})
	}
	if text := accText.String(); text != "" {
		final.Content = append(final.Content, message.TextBlock{Type: "text", Text: text})
	}
	select {
	case outCh <- final:
	case <-ctx.Done():
	}
}

// ollamaNativeUsage maps prompt_eval_count/eval_count (present only on the
// final frame) to ChatUsage. The pointers keep an absent count (nil Usage,
// unknown) distinct from a reported zero.
func ollamaNativeUsage(resp *ollamaNativeChatResponse) *ChatUsage {
	if resp.PromptEvalCount == nil && resp.EvalCount == nil {
		return nil
	}
	usage := &ChatUsage{}
	if resp.PromptEvalCount != nil {
		usage.InputTokens = *resp.PromptEvalCount
	}
	if resp.EvalCount != nil {
		usage.OutputTokens = *resp.EvalCount
	}
	return usage
}

// mapOllamaNativeStopReason normalizes the native done_reason: "length" means
// truncated by num_predict; any other reported value (including "stop") maps
// to stop. Empty means the server did not report one.
func mapOllamaNativeStopReason(doneReason string) string {
	switch doneReason {
	case "":
		return ""
	case "length":
		return StopReasonLength
	default:
		return StopReasonStop
	}
}

// convertMessagesToOllamaNative maps internal Msg instances to native
// /api/chat messages. Replay policy: base64 image DataBlocks attach to the
// message's images array; URL-sourced/non-image data blocks, thinking blocks
// and tool blocks are not replayed — tool calling is unsupported, and a role
// "tool" result without its assistant tool_calls would be an orphan the server
// cannot interpret. A message whose blocks are all non-replayable produces no
// wire message.
func convertMessagesToOllamaNative(msgs []*message.Msg) []ollamaNativeMessage {
	var out []ollamaNativeMessage
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		role := string(msg.Role)
		if role == "" {
			role = "user"
		}

		blocks := msg.GetContentBlocks()
		if len(blocks) == 0 {
			out = append(out, ollamaNativeMessage{Role: role, Content: ""})
			continue
		}

		var textParts []string
		var images []string
		for _, b := range blocks {
			switch blk := b.(type) {
			case message.TextBlock:
				textParts = append(textParts, blk.Text)
			case message.HintBlock:
				textParts = append(textParts, blk.GetHintText())
			case message.DataBlock:
				if src, ok := blk.Source.(message.Base64Source); ok && strings.HasPrefix(blk.GetMediaType(), "image/") {
					images = append(images, src.Data)
				}
			}
		}
		if len(textParts) == 0 && len(images) == 0 {
			continue
		}
		out = append(out, ollamaNativeMessage{
			Role:    role,
			Content: strings.Join(textParts, "\n"),
			Images:  images,
		})
	}
	return out
}

// --- native /api/chat request/response types ---

// ollamaNativeMessage is the native /api/chat message shape. Images carries
// base64 image data (no data: URL prefix).
type ollamaNativeMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type ollamaNativeOptions struct {
	NumCtx      int      `json:"num_ctx,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

type ollamaNativeChatRequest struct {
	Model    string                `json:"model"`
	Messages []ollamaNativeMessage `json:"messages"`
	Stream   bool                  `json:"stream"`
	Options  *ollamaNativeOptions  `json:"options,omitempty"`
	// Format is `"json"` for ResponseFormat json_object or the embedded JSON
	// Schema object for json_schema.
	Format json.RawMessage `json:"format,omitempty"`
	Think  *bool           `json:"think,omitempty"`
}

type ollamaNativeChatResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		// Thinking carries the model's reasoning (string per the native API;
		// a server sending a non-string value fails decoding loudly).
		Thinking string `json:"thinking"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount *int   `json:"prompt_eval_count"`
	EvalCount       *int   `json:"eval_count"`
}
