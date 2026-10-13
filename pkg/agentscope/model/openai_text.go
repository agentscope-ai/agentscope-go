package model

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// OpenAITextClient is an opt-in, single-attempt Chat Completions primitive for
// managed text tasks. It does not implement ChatModel or change ordinary Chat.
// Only standard *http.Transport is supported. Configuration is snapshotted;
// host transport hooks must be concurrency-safe and honor request cancellation.
type OpenAITextClient struct {
	model    *OpenAIChatModel
	client   *http.Client
	endpoint string
}

// PreparedTextRequest owns immutable final JSON bound to its creating client.
// It contains projected text and must not be logged by default.
type PreparedTextRequest struct {
	owner *OpenAITextClient
	body  []byte
}

// JSON returns a copy of the exact request representation used for dispatch.
func (p *PreparedTextRequest) JSON() []byte { return bytes.Clone(p.body) }

// TextUsage preserves unknown counts independently; zero pointers mean known zero.
// Completion tokens include reasoning; cached input is a subset of input.
type TextUsage struct {
	InputTokens       *int64
	OutputTokens      *int64
	CachedInputTokens *int64
}

// TextCallResult retains recognized model and usage even on HTTP/validation errors.
// Text is populated only for a validated, complete, tool-free text response.
type TextCallResult struct {
	Text         string
	Model        string
	FinishReason string
	Usage        *TextUsage
}

// TextHTTPError exposes a status without retaining provider bodies or headers.
type TextHTTPError struct{ StatusCode int }

func (e *TextHTTPError) Error() string { return "openai text: HTTP request failed" }

// Errors on the opt-in text primitive support errors.Is via AgentError codes.
var (
	ErrTextInvalidRequest  = &ae.AgentError{Category: ae.CategoryConfig, Code: "model.text.invalid_request", Message: "openai text: invalid request"}
	ErrTextInvalidResponse = &ae.AgentError{Category: ae.CategoryModel, Code: "model.text.invalid_response", Message: "openai text: invalid response"}
	ErrTextUnsupported     = &ae.AgentError{Category: ae.CategoryModel, Code: "model.text.unsupported", Message: "openai text: unsupported response"}
	ErrTextIncomplete      = &ae.AgentError{Category: ae.CategoryModel, Code: "model.text.incomplete", Message: "openai text: incomplete response"}
)

// NewOpenAITextClient snapshots OpenAI configuration for single sends. Client
// timeouts are removed in favor of the caller context; redirects, cookies,
// HTTP/2 replay and request-body rewind are disabled. Custom RoundTrippers are
// rejected; cloned protocol handlers are removed. Transport hooks/proxies remain
// host trust.
func NewOpenAITextClient(cfg OpenAIConfig) (*OpenAITextClient, error) { //nolint:gocritic // matches existing provider configuration
	m, err := NewOpenAIChatModel(cfg)
	if err != nil {
		return nil, ErrTextInvalidRequest
	}
	endpoint := strings.TrimRight(m.baseURL, "/") + "/v1/chat/completions"
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, ErrTextInvalidRequest
	}
	raw := m.httpClient.Transport
	if raw == nil {
		raw = http.DefaultTransport
	}
	transport, ok := raw.(*http.Transport)
	if !ok || transport == nil {
		return nil, ErrTextInvalidRequest
	}
	tr := transport.Clone()
	tr.ForceAttemptHTTP2 = false
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig != nil {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	headers := make(map[string]string, len(m.defaultHeaders))
	for k, v := range m.defaultHeaders {
		switch strings.ToLower(k) {
		case "authorization", "content-type":
			continue
		case "host", "idempotency-key", "x-idempotency-key":
			return nil, ErrTextInvalidRequest
		}
		headers[k] = v
	}
	m.defaultHeaders = headers
	return &OpenAITextClient{model: m, endpoint: endpoint, client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Target returns the actual completion endpoint and requested model binding.
func (c *OpenAITextClient) Target() (endpoint, model string) { return c.endpoint, c.model.model }

// CloseIdleConnections releases idle connections; it does not cancel active calls.
func (c *OpenAITextClient) CloseIdleConnections() { c.client.CloseIdleConnections() }

// Prepare formats one approved user text without tools or streaming and bounds
// the final serialized JSON. It neither authorizes disclosure nor dispatches.
func (c *OpenAITextClient) Prepare(ctx context.Context, text string, outputTokens, maxRequestBytes int) (*PreparedTextRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || outputTokens <= 0 || maxRequestBytes <= 0 || len(text) > maxRequestBytes || strings.TrimSpace(text) == "" || !utf8.ValidString(text) {
		return nil, ErrTextInvalidRequest
	}
	msgs := []*message.Msg{message.UserMsg("user", text)}
	body, err := json.Marshal(openAIChatRequest{Model: c.model.model, Messages: convertMessagesToOpenAI(msgs), MaxCompletionTokens: &outputTokens, ToolChoice: "none"})
	if err != nil || len(body) > maxRequestBytes {
		return nil, ErrTextInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &PreparedTextRequest{owner: c, body: body}, nil
}

type textDispatchTransport struct {
	base  http.RoundTripper
	begin func() error
}

func (t textDispatchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if t.begin != nil {
		if err := t.begin(); err != nil {
			return nil, err
		}
	}
	return t.base.RoundTrip(r)
}

// Send performs at most one dispatch. begin runs immediately at the transport
// boundary; returning an error prevents dispatch. This hook lets an executor
// atomically arbitrate terminal cancellation, attempt limits and accounting.
// Send always returns a non-nil result. A permit must cover the whole Send call.
func (c *OpenAITextClient) Send(ctx context.Context, p *PreparedTextRequest, maxResponseBytes int, begin func() error) (result *TextCallResult, err error) {
	result = &TextCallResult{}
	defer func() {
		if cause := ctx.Err(); cause != nil {
			result.Text = ""
			err = cause
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if c == nil || p == nil || p.owner != c || maxResponseBytes <= 0 || maxResponseBytes == math.MaxInt {
		return result, ErrTextInvalidRequest
	}
	// Passing a ReadCloser prevents NewRequest from installing GetBody. POST is
	// neither idempotent nor rewindable, so net/http cannot replay its body.
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, io.NopCloser(bytes.NewReader(p.body)))
	if e != nil {
		return result, ErrTextInvalidRequest
	}
	req.ContentLength = int64(len(p.body))
	for k, v := range c.model.defaultHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.model.apiKey)
	client := *c.client
	client.Transport = textDispatchTransport{base: c.client.Transport, begin: begin}
	response, e := client.Do(req)
	if e != nil {
		if errors.Is(e, context.Canceled) {
			return result, context.Canceled
		}
		if errors.Is(e, context.DeadlineExceeded) {
			return result, context.DeadlineExceeded
		}
		return result, &ae.AgentError{Category: ae.CategoryNetwork, Code: "model.text.transport", Message: "openai text: transport failed"}
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, int64(maxResponseBytes)+1))
	if e != nil || len(raw) > maxResponseBytes {
		return result, &ae.AgentError{Category: ae.CategoryModel, Code: "model.text.response_limit", Message: "openai text: response unreadable or exceeds limit"}
	}
	var envelope map[string]json.RawMessage
	if utf8.Valid(raw) && json.Unmarshal(raw, &envelope) == nil {
		_ = json.Unmarshal(envelope["model"], &result.Model)
		result.Usage = readTextUsage(envelope["usage"])
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, &ae.AgentError{Category: ae.CategoryModel, Code: "model.text.http", Message: "openai text: HTTP request failed", Cause: &TextHTTPError{StatusCode: response.StatusCode}, Retryable: response.StatusCode == 429 || response.StatusCode >= 500 && response.StatusCode <= 599, RetryAfter: textRetryAfter(response.Header.Get("Retry-After"))}
	}
	return validateTextResponse(raw, result)
}

func readTextUsage(raw json.RawMessage) *TextUsage {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil || usage == nil {
		return nil
	}
	u := &TextUsage{InputTokens: textCount(usage["prompt_tokens"]), OutputTokens: textCount(usage["completion_tokens"])}
	var details map[string]json.RawMessage
	if json.Unmarshal(usage["prompt_tokens_details"], &details) == nil {
		u.CachedInputTokens = textCount(details["cached_tokens"])
	}
	if u.InputTokens != nil && u.CachedInputTokens != nil && *u.CachedInputTokens > *u.InputTokens {
		u.CachedInputTokens = nil
	}
	return u
}

func textCount(raw json.RawMessage) *int64 {
	var n *int64
	if json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 {
		return nil
	}
	return n
}

func textRetryAfter(s string) time.Duration {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 && n <= math.MaxInt64/int64(time.Second) {
		return time.Duration(n) * time.Second
	}
	if date, err := http.ParseTime(s); err == nil {
		if d := time.Until(date); d > 0 {
			return d
		}
	}
	return 0
}

func validateTextResponse(raw []byte, result *TextCallResult) (*TextCallResult, error) {
	var wire struct {
		Model   string          `json:"model"`
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index        *int   `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role         string          `json:"role"`
				Content      json.RawMessage `json:"content"`
				ToolCalls    json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage `json:"function_call"`
				Audio        json.RawMessage `json:"audio"`
				Refusal      json.RawMessage `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &wire) != nil || strings.TrimSpace(wire.Model) == "" || len(wire.Choices) != 1 || textPresent(wire.Error) {
		return result, ErrTextInvalidResponse
	}
	choice := wire.Choices[0]
	if choice.Index == nil || *choice.Index != 0 || choice.Message.Role != "assistant" {
		return result, ErrTextInvalidResponse
	}
	var calls []json.RawMessage
	if textPresent(choice.Message.ToolCalls) {
		if json.Unmarshal(choice.Message.ToolCalls, &calls) != nil {
			return result, ErrTextInvalidResponse
		}
	}
	if len(calls) > 0 || textPresent(choice.Message.FunctionCall) || textPresent(choice.Message.Audio) {
		return result, ErrTextUnsupported
	}
	if textPresent(choice.Message.Refusal) && string(choice.Message.Refusal) != `""` {
		result.FinishReason = StopReasonContentFilter
		return result, ErrTextIncomplete
	}
	switch choice.FinishReason {
	case StopReasonStop:
	case StopReasonLength, StopReasonContentFilter:
		result.FinishReason = choice.FinishReason
		return result, ErrTextIncomplete
	case StopReasonToolCalls, "function_call":
		return result, ErrTextUnsupported
	default:
		return result, ErrTextInvalidResponse
	}
	var text string
	if json.Unmarshal(choice.Message.Content, &text) != nil || strings.TrimSpace(text) == "" {
		return result, ErrTextInvalidResponse
	}
	var parsed openAIChatResponse
	if json.Unmarshal(raw, &parsed) != nil {
		return result, ErrTextInvalidResponse
	}
	response, err := parseOpenAIResponse(&parsed, nil)
	if err != nil {
		return result, ErrTextInvalidResponse
	}
	result.Text = response.GetTextContent()
	result.FinishReason = StopReasonStop
	return result, nil
}

func textPresent(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }
