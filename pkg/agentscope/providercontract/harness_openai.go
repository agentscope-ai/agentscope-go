package providercontract

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const openAISuccessBody = `{
	"id": "chatcmpl-c1", "model": "gpt-test",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "hello contract"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
		"prompt_tokens_details": {"cached_tokens": 3}}
}`

const openAIStreamBody = `data: {"id":"c1","model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello "}}]}

data: {"id":"c1","model":"gpt-test","choices":[{"index":0,"delta":{"content":"contract"}}]}

data: {"id":"c1","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4}}

data: [DONE]

`

// openAIServe is shared by the OpenAI-compatible family; thinking wire
// assertions differ per provider and are layered by the caller.
func openAIServe(w http.ResponseWriter, r *http.Request, scn Scenario, captured *[]byte) {
	switch scn {
	case ScnChatSuccess, ScnCaptureBody:
		if captured != nil {
			// io.ReadAll: single Read short-reads and ContentLength == -1
			// (chunked) would corrupt the captured wire body (HARNESS
			// review M8).
			buf, _ := io.ReadAll(r.Body)
			*captured = buf
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAISuccessBody)
	case ScnCaptureBodyStream:
		if captured != nil {
			buf, _ := io.ReadAll(r.Body)
			*captured = buf
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, openAIStreamBody)
	case ScnStreamSuccess:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, openAIStreamBody)
	case ScnStreamTruncated:
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		fl.Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // cut the chunked stream mid-flight
	case ScnStreamSlow:
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"slow\"}}]}\n\n")
		fl.Flush()
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	case ScnErr429:
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit"}}`)
	case ScnErr401:
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key","type":"auth"}}`)
	}
}

// OpenAIHarness returns the contract harness for the OpenAI provider.
// OpenAI is the one adapter in the family that sends the output limit as
// max_completion_tokens (the Chat Completions API deprecated max_tokens).
func OpenAIHarness() Harness {
	return Harness{
		Name: "openai",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewOpenAIChatModel(model.OpenAIConfig{
				APIKey: "test-key", Model: "gpt-test", BaseURL: baseURL,
			})
		},
		Serve:             openAIServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "max_completion_tokens",
	}
}

// DashScopeHarness returns the contract harness for DashScope
// (OpenAI-compatible wire; enable_thinking toggle).
func DashScopeHarness() Harness {
	return Harness{
		Name: "dashscope",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewDashScopeChatModel(model.DashScopeConfig{
				APIKey: "test-key", Model: "qwen-test", BaseURL: baseURL,
			})
		},
		Serve:             openAIServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "max_tokens",
		DisableThinkingCheck: func(t *testing.T, body []byte) {
			requireContains(t, body, `"enable_thinking":false`, "dashscope disable-thinking wire format")
		},
		EnableThinkingCheck: func(t *testing.T, body []byte) {
			requireContains(t, body, `"enable_thinking":true`, "dashscope enable-thinking wire format")
		},
	}
}

// DeepSeekHarness returns the contract harness for DeepSeek
// (OpenAI-compatible wire; thinking.type=disabled object).
func DeepSeekHarness() Harness {
	return Harness{
		Name: "deepseek",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewDeepSeekChatModel(model.DeepSeekConfig{
				APIKey: "test-key", Model: "deepseek-chat", BaseURL: baseURL,
			})
		},
		Serve:             openAIServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "max_tokens",
		DisableThinkingCheck: func(t *testing.T, body []byte) {
			requireContains(t, body, `"thinking":{"type":"disabled"}`, "deepseek disable-thinking wire format")
		},
	}
}

// MoonshotHarness returns the contract harness for Moonshot
// (OpenAI-compatible wire; thinking.type=disabled object).
func MoonshotHarness() Harness {
	return Harness{
		Name: "moonshot",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewMoonshotChatModel(model.MoonshotConfig{
				APIKey: "test-key", Model: "kimi-test", BaseURL: baseURL,
			})
		},
		Serve:             openAIServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "max_tokens",
		DisableThinkingCheck: func(t *testing.T, body []byte) {
			requireContains(t, body, `"thinking":{"type":"disabled"}`, "moonshot disable-thinking wire format")
		},
	}
}

// OllamaHarness returns the contract harness for Ollama's OpenAI-compatible
// endpoint (no API key; local servers are where a dropped max_tokens hurts
// most, agentscope-go#8).
func OllamaHarness() Harness {
	return Harness{
		Name: "ollama",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewOllamaChatModel(model.OllamaConfig{
				Model: "qwen-test", BaseURL: baseURL,
			})
		},
		Serve:             openAIServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "max_tokens",
	}
}

// XAIHarness returns the contract harness for xAI/Grok (OpenAI-compatible
// wire; reasoning tokens are folded into OutputTokens when the usage block
// reports them, which the shared fixture does not).
func XAIHarness() Harness {
	return Harness{
		Name: "xai",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewXAIChatModel(model.XAIConfig{
				APIKey: "test-key", Model: "grok-test", BaseURL: baseURL,
			})
		},
		Serve:             openAIServe,
		ExpectUsage:       &model.ChatUsage{InputTokens: 10, OutputTokens: 5, CacheInputTokens: 3},
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "max_tokens",
	}
}
