package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// openAIMockServer returns an httptest.Server that responds with a minimal
// valid OpenAI-compatible chat/completions JSON body. Callers must defer
// srv.Close().
func openAIMockServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") || r.URL.Query().Get("alt") == "sse" {
			// Gemini streaming endpoint
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+geminiResponseJSON()+"\n\n")
			return
		}
		if strings.Contains(r.URL.Path, "generateContent") {
			// Gemini non-streaming endpoint
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, geminiResponseJSON())
			return
		}
		if strings.Contains(r.URL.Path, "/v1/responses") {
			// OpenAI Responses API endpoint
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, openAIResponseAPIJSON())
			return
		}
		if strings.Contains(r.URL.Path, "/v1/messages") {
			// Anthropic messages endpoint
			if r.Header.Get("x-api-key") != "" {
				// Check if streaming
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if stream, ok := body["stream"].(bool); ok && stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, anthropicStreamResponseSSE())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, anthropicResponseJSON())
				return
			}
		}
		// Default: OpenAI-compatible chat/completions
		// Check if streaming
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, ok := body["stream"].(bool); ok && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, openAIStreamResponseSSE())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAIResponseJSON())
	}))
}

func openAIResponseJSON() string {
	return `{
		"id": "chatcmpl-test123",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "test-model",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello from mock"},
			"finish_reason": "stop"
		}],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5,
			"total_tokens": 15
		}
	}`
}

func openAIStreamResponseSSE() string {
	return `data: {"id":"chatcmpl-test123","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-test123","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}

data: [DONE]

`
}

func anthropicResponseJSON() string {
	return `{
		"id": "msg_test123",
		"model": "claude-test",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello from Anthropic mock"}],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5
		}
	}`
}

func anthropicStreamResponseSSE() string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_test123","model":"claude-test","usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`
}

func geminiResponseJSON() string {
	return `{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [{"text": "Hello from Gemini mock"}]
			},
			"finishReason": "STOP"
		}],
		"usageMetadata": {
			"promptTokenCount": 8,
			"candidatesTokenCount": 4,
			"totalTokenCount": 12
		},
		"modelVersion": "gemini-test"
	}`
}

func openAIResponseAPIJSON() string {
	return `{
		"id": "resp_test123",
		"output": [
			{
				"type": "message",
				"content": [{"type": "output_text", "text": "Hello from Responses API"}]
			}
		],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5
		}
	}`
}

func testMessages() []*message.Msg {
	return []*message.Msg{
		message.UserMsg("user", "Hello"),
	}
}

// ---------------------------------------------------------------------------
// 1. Config construction — missing required fields
// ---------------------------------------------------------------------------

func TestOpenAIConfig_MissingFields(t *testing.T) {
	_, err := NewOpenAIChatModel(OpenAIConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewOpenAIChatModel(OpenAIConfig{APIKey: "sk-test"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestAnthropicConfig_MissingFields(t *testing.T) {
	_, err := NewAnthropicChatModel(&AnthropicConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewAnthropicChatModel(&AnthropicConfig{APIKey: "sk-test"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestDashScopeConfig_MissingFields(t *testing.T) {
	_, err := NewDashScopeChatModel(DashScopeConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewDashScopeChatModel(DashScopeConfig{APIKey: "sk-test"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestDeepSeekConfig_MissingFields(t *testing.T) {
	_, err := NewDeepSeekChatModel(DeepSeekConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewDeepSeekChatModel(DeepSeekConfig{APIKey: "sk-test"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestGeminiConfig_MissingFields(t *testing.T) {
	_, err := NewGeminiChatModel(GeminiConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewGeminiChatModel(GeminiConfig{APIKey: "test-key"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestMoonshotConfig_MissingFields(t *testing.T) {
	_, err := NewMoonshotChatModel(MoonshotConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewMoonshotChatModel(MoonshotConfig{APIKey: "sk-test"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestOllamaConfig_MissingFields(t *testing.T) {
	_, err := NewOllamaChatModel(OllamaConfig{})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestXAIConfig_MissingFields(t *testing.T) {
	_, err := NewXAIChatModel(XAIConfig{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("expected APIKey error, got %v", err)
	}

	_, err = NewXAIChatModel(XAIConfig{APIKey: "xai-test"})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestOpenAIResponseConfig_MissingFields(t *testing.T) {
	_, err := NewOpenAIResponseModel(&OpenAIResponseConfig{})
	if err == nil || !strings.Contains(err.Error(), "api key is required") {
		t.Fatalf("expected api key error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2. Successful config construction — each creates a valid ChatModel
// ---------------------------------------------------------------------------

func TestOpenAIConfig_ValidConstruction(t *testing.T) {
	m, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "sk-test", Model: "gpt-4"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestAnthropicConfig_ValidConstruction(t *testing.T) {
	m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "sk-test", Model: "claude-3"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestDashScopeConfig_ValidConstruction(t *testing.T) {
	m, err := NewDashScopeChatModel(DashScopeConfig{APIKey: "sk-test", Model: "qwen-turbo"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestDeepSeekConfig_ValidConstruction(t *testing.T) {
	m, err := NewDeepSeekChatModel(DeepSeekConfig{APIKey: "sk-test", Model: "deepseek-chat"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestGeminiConfig_ValidConstruction(t *testing.T) {
	m, err := NewGeminiChatModel(GeminiConfig{APIKey: "test-key", Model: "gemini-pro"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestMoonshotConfig_ValidConstruction(t *testing.T) {
	m, err := NewMoonshotChatModel(MoonshotConfig{APIKey: "sk-test", Model: "moonshot-v1"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestOllamaConfig_ValidConstruction(t *testing.T) {
	m, err := NewOllamaChatModel(OllamaConfig{Model: "llama3"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestXAIConfig_ValidConstruction(t *testing.T) {
	m, err := NewXAIChatModel(XAIConfig{APIKey: "xai-test", Model: "grok-1"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

func TestOpenAIResponseConfig_ValidConstruction(t *testing.T) {
	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	var _ ChatModel = m
}

// ---------------------------------------------------------------------------
// 3. CountTokens returns a reasonable value
// ---------------------------------------------------------------------------

func TestCountTokens_AllProviders(t *testing.T) {
	msgs := []*message.Msg{
		message.SystemMsg("system", "You are a helpful assistant."),
		message.UserMsg("user", "What is the capital of France?"),
	}

	tests := []struct {
		name  string
		model ChatModel
	}{
		{"OpenAI", must(NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m"}))},
		{"Anthropic", must(NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m"}))},
		{"DashScope", must(NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m"}))},
		{"DeepSeek", must(NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m"}))},
		{"Gemini", must(NewGeminiChatModel(GeminiConfig{APIKey: "k", Model: "m"}))},
		{"Moonshot", must(NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m"}))},
		{"Ollama", mustOllama(NewOllamaChatModel(OllamaConfig{Model: "m"}))},
		{"XAI", must(NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m"}))},
		{"OpenAIResponse", mustChatModel(NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "k"}))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := tt.model.CountTokens(msgs, nil)
			if count <= 0 {
				t.Errorf("CountTokens returned %d, expected > 0", count)
			}
		})
	}
}

func TestCountTokens_WithTools(t *testing.T) {
	msgs := []*message.Msg{message.UserMsg("user", "call a tool")}
	tools := []ToolSchema{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_weather",
				Description: "Get current weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		},
	}

	m, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m"})
	countWithTools := m.CountTokens(msgs, tools)
	countWithout := m.CountTokens(msgs, nil)

	if countWithTools <= countWithout {
		t.Errorf("CountTokens with tools (%d) should be > without (%d)", countWithTools, countWithout)
	}
}

func TestCountTokens_NilMessages(t *testing.T) {
	m, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m"})
	count := m.CountTokens(nil, nil)
	if count != 0 {
		t.Errorf("CountTokens(nil, nil) = %d, want 0", count)
	}
}

// ---------------------------------------------------------------------------
// 4. Chat returns error for empty messages
// ---------------------------------------------------------------------------

func TestChat_EmptyMessages_AllProviders(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()

	// Note: OpenAIResponse is excluded because its Chat method does not guard
	// against empty messages — it sends the request and relies on the server to
	// reject it. All other providers perform an early "msgs must not be empty"
	// check.
	tests := []struct {
		name  string
		model ChatModel
	}{
		{"OpenAI", mustChatModel(NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Anthropic", mustChatModel(NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DashScope", mustChatModel(NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DeepSeek", mustChatModel(NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Gemini", mustChatModel(NewGeminiChatModel(GeminiConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Moonshot", mustChatModel(NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Ollama", mustChatModel(NewOllamaChatModel(OllamaConfig{Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"XAI", mustChatModel(NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.model.Chat(ctx, []*message.Msg{})
			if err == nil {
				t.Error("expected error for empty messages, got nil")
			}
			if err != nil && !strings.Contains(err.Error(), "must not be empty") {
				t.Errorf("expected 'must not be empty' error, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. ChatStream returns a channel (with mock server)
// ---------------------------------------------------------------------------

func TestChatStream_ReturnsChannel_AllProviders(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()
	msgs := testMessages()

	tests := []struct {
		name  string
		model ChatModel
	}{
		{"OpenAI", mustChatModel(NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Anthropic", mustChatModel(NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DashScope", mustChatModel(NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DeepSeek", mustChatModel(NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Gemini", mustChatModel(NewGeminiChatModel(GeminiConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Moonshot", mustChatModel(NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Ollama", mustChatModel(NewOllamaChatModel(OllamaConfig{Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"XAI", mustChatModel(NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := tt.model.ChatStream(ctx, msgs)
			if err != nil {
				t.Fatalf("ChatStream returned error: %v", err)
			}
			if ch == nil {
				t.Fatal("ChatStream returned nil channel")
			}
			// Drain the channel to avoid goroutine leaks
			for range ch {
			}
		})
	}
}

func TestChatStream_EmptyMessages_AllProviders(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()

	tests := []struct {
		name  string
		model ChatModel
	}{
		{"OpenAI", mustChatModel(NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Anthropic", mustChatModel(NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DashScope", mustChatModel(NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DeepSeek", mustChatModel(NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Gemini", mustChatModel(NewGeminiChatModel(GeminiConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Moonshot", mustChatModel(NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Ollama", mustChatModel(NewOllamaChatModel(OllamaConfig{Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"XAI", mustChatModel(NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.model.ChatStream(ctx, []*message.Msg{})
			if err == nil {
				t.Error("expected error for empty messages, got nil")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. Chat with mock server returns valid response (OpenAI-compat providers)
// ---------------------------------------------------------------------------

func TestChat_MockServer_OpenAICompat(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()
	msgs := testMessages()

	tests := []struct {
		name  string
		model ChatModel
	}{
		{"OpenAI", mustChatModel(NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DashScope", mustChatModel(NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"DeepSeek", mustChatModel(NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Moonshot", mustChatModel(NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"Ollama", mustChatModel(NewOllamaChatModel(OllamaConfig{Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
		{"XAI", mustChatModel(NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTPClient: client}))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := tt.model.Chat(ctx, msgs)
			if err != nil {
				t.Fatalf("Chat returned error: %v", err)
			}
			if resp == nil {
				t.Fatal("Chat returned nil response")
			}
			text := resp.GetTextContent()
			if text == "" {
				t.Error("expected non-empty text content")
			}
			if !resp.IsLast {
				t.Error("expected IsLast=true for non-streaming response")
			}
		})
	}
}

func TestChat_MockServer_Anthropic(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()
	msgs := testMessages()

	m, err := NewAnthropicChatModel(&AnthropicConfig{
		APIKey:     "sk-test",
		Model:      "claude-3",
		BaseURL:    srv.URL,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := m.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Chat returned nil response")
	}
	text := resp.GetTextContent()
	if !strings.Contains(text, "Anthropic") {
		t.Errorf("expected Anthropic response text, got %q", text)
	}
}

func TestChat_MockServer_Gemini(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()
	msgs := testMessages()

	m, err := NewGeminiChatModel(GeminiConfig{
		APIKey:     "test-key",
		Model:      "gemini-pro",
		BaseURL:    srv.URL,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := m.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Chat returned nil response")
	}
	text := resp.GetTextContent()
	if !strings.Contains(text, "Gemini") {
		t.Errorf("expected Gemini response text, got %q", text)
	}
}

func TestChat_MockServer_OpenAIResponse(t *testing.T) {
	srv := openAIMockServer()
	defer srv.Close()

	ctx := context.Background()
	msgs := testMessages()

	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{
		APIKey:  "sk-test",
		Model:   "gpt-4.1",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := m.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Chat returned nil response")
	}
	text := resp.GetTextContent()
	if !strings.Contains(text, "Responses API") {
		t.Errorf("expected Responses API text, got %q", text)
	}
}

// ---------------------------------------------------------------------------
// 7. Default values applied when optional fields are omitted
// ---------------------------------------------------------------------------

func TestOpenAIConfig_DefaultBaseURL(t *testing.T) {
	m, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if m.baseURL != defaultOpenAIBaseURL {
		t.Errorf("baseURL = %q, want %q", m.baseURL, defaultOpenAIBaseURL)
	}
}

func TestAnthropicConfig_Defaults(t *testing.T) {
	m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if m.baseURL != defaultAnthropicBaseURL {
		t.Errorf("baseURL = %q, want %q", m.baseURL, defaultAnthropicBaseURL)
	}
	if m.version != defaultAnthropicVersion {
		t.Errorf("version = %q, want %q", m.version, defaultAnthropicVersion)
	}
	if m.maxOutputTok != defaultAnthropicMaxOutputTokens {
		t.Errorf("maxOutputTok = %d, want %d", m.maxOutputTok, defaultAnthropicMaxOutputTokens)
	}
}

func TestDashScopeConfig_DefaultBaseURL(t *testing.T) {
	m, err := NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if m.baseURL != defaultDashScopeBaseURL {
		t.Errorf("baseURL = %q, want %q", m.baseURL, defaultDashScopeBaseURL)
	}
}

func TestOllamaConfig_DefaultBaseURL(t *testing.T) {
	m, err := NewOllamaChatModel(OllamaConfig{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if m.baseURL != defaultOllamaBaseURL {
		t.Errorf("baseURL = %q, want %q", m.baseURL, defaultOllamaBaseURL)
	}
}

func TestOpenAIResponseConfig_DefaultModel(t *testing.T) {
	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	resp := m.(*openaiResponseModel)
	if resp.cfg.Model != "gpt-4.1" {
		t.Errorf("Model = %q, want %q", resp.cfg.Model, "gpt-4.1")
	}
	if resp.cfg.BaseURL != "https://api.openai.com" {
		t.Errorf("BaseURL = %q, want %q", resp.cfg.BaseURL, "https://api.openai.com")
	}
}

// ---------------------------------------------------------------------------
// 8. Custom BaseURL is applied
// ---------------------------------------------------------------------------

func TestCustomBaseURL(t *testing.T) {
	customURL := "https://my-proxy.example.com"

	m1, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m1.baseURL != customURL {
		t.Errorf("OpenAI baseURL = %q, want %q", m1.baseURL, customURL)
	}

	m2, _ := NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m2.baseURL != customURL {
		t.Errorf("Anthropic baseURL = %q, want %q", m2.baseURL, customURL)
	}

	m3, _ := NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m3.baseURL != customURL {
		t.Errorf("DashScope baseURL = %q, want %q", m3.baseURL, customURL)
	}

	m4, _ := NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m4.baseURL != customURL {
		t.Errorf("DeepSeek baseURL = %q, want %q", m4.baseURL, customURL)
	}

	m5, _ := NewGeminiChatModel(GeminiConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m5.baseURL != customURL {
		t.Errorf("Gemini baseURL = %q, want %q", m5.baseURL, customURL)
	}

	m6, _ := NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m6.baseURL != customURL {
		t.Errorf("Moonshot baseURL = %q, want %q", m6.baseURL, customURL)
	}

	m7, _ := NewOllamaChatModel(OllamaConfig{Model: "m", BaseURL: customURL})
	if m7.baseURL != customURL {
		t.Errorf("Ollama baseURL = %q, want %q", m7.baseURL, customURL)
	}

	m8, _ := NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", BaseURL: customURL})
	if m8.baseURL != customURL {
		t.Errorf("XAI baseURL = %q, want %q", m8.baseURL, customURL)
	}
}

// ---------------------------------------------------------------------------
// 9. ContextSizer / ModelNamer interfaces (OpenAI Response model)
// ---------------------------------------------------------------------------

func TestOpenAIResponseModel_ContextSizer(t *testing.T) {
	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}

	cs, ok := m.(ContextSizer)
	if !ok {
		t.Fatal("openaiResponseModel should implement ContextSizer")
	}
	if cs.ContextSize() != 200000 {
		t.Errorf("ContextSize() = %d, want 200000", cs.ContextSize())
	}
}

func TestOpenAIResponseModel_ModelNamer(t *testing.T) {
	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "k", Model: "gpt-4.1-mini"})
	if err != nil {
		t.Fatal(err)
	}

	mn, ok := m.(ModelNamer)
	if !ok {
		t.Fatal("openaiResponseModel should implement ModelNamer")
	}
	if mn.ModelName() != "gpt-4.1-mini" {
		t.Errorf("ModelName() = %q, want %q", mn.ModelName(), "gpt-4.1-mini")
	}
}

// ---------------------------------------------------------------------------
// 10. Custom HTTPClient is used
// ---------------------------------------------------------------------------

func TestCustomHTTPClient(t *testing.T) {
	customClient := &http.Client{}

	m1, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m1.httpClient != customClient {
		t.Error("OpenAI did not use custom HTTP client")
	}

	m2, _ := NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m2.httpClient != customClient {
		t.Error("Anthropic did not use custom HTTP client")
	}

	m3, _ := NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m3.httpClient != customClient {
		t.Error("DashScope did not use custom HTTP client")
	}

	m4, _ := NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m4.httpClient != customClient {
		t.Error("DeepSeek did not use custom HTTP client")
	}

	m5, _ := NewGeminiChatModel(GeminiConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m5.httpClient != customClient {
		t.Error("Gemini did not use custom HTTP client")
	}

	m6, _ := NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m6.httpClient != customClient {
		t.Error("Moonshot did not use custom HTTP client")
	}

	m7, _ := NewOllamaChatModel(OllamaConfig{Model: "m", HTTPClient: customClient})
	if m7.httpClient != customClient {
		t.Error("Ollama did not use custom HTTP client")
	}

	m8, _ := NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", HTTPClient: customClient})
	if m8.httpClient != customClient {
		t.Error("XAI did not use custom HTTP client")
	}
}

// ---------------------------------------------------------------------------
// Helper functions for test setup
// ---------------------------------------------------------------------------

// must is a generic helper for constructors that return (*T, error) where T
// is a concrete model type that implements ChatModel.
func must[T ChatModel](m T, err error) ChatModel {
	if err != nil {
		panic(fmt.Sprintf("must: %v", err))
	}
	return m
}

// mustOllama helps with OllamaChatModel which has no APIKey.
func mustOllama(m *OllamaChatModel, err error) ChatModel {
	if err != nil {
		panic(fmt.Sprintf("mustOllama: %v", err))
	}
	return m
}

// mustChatModel is for constructors that return (ChatModel, error) directly.
func mustChatModel(m ChatModel, err error) ChatModel {
	if err != nil {
		panic(fmt.Sprintf("mustChatModel: %v", err))
	}
	return m
}
