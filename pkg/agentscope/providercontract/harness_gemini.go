package providercontract

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const geminiSuccessBody = `{
	"candidates": [{"content": {"role": "model", "parts": [{"text": "hello contract"}]}, "finishReason": "STOP"}],
	"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 19,
		"toolUsePromptTokenCount": 2, "thoughtsTokenCount": 1, "cachedContentTokenCount": 4}
}`

// Upstream #2406 accounting: input = prompt + tool-use prompt tokens;
// output = candidates + thought tokens; cached content feeds cache input.
var geminiExpectUsage = &model.ChatUsage{InputTokens: 12, OutputTokens: 6, CacheInputTokens: 4}

const geminiStreamBody = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello "}]},"finishReason":"STOP"}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"contract"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}

`

func geminiServe(w http.ResponseWriter, r *http.Request, scn Scenario, captured *[]byte) {
	streaming := strings.Contains(r.URL.Path, "streamGenerateContent")
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
		fmt.Fprint(w, geminiSuccessBody)
	case ScnCaptureBodyStream:
		if captured != nil {
			buf, _ := io.ReadAll(r.Body)
			*captured = buf
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStreamBody)
	case ScnStreamSuccess:
		_ = streaming
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStreamBody)
	case ScnStreamTruncated:
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n")
		fl.Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	case ScnStreamSlow:
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"slow\"}]}}]}\n\n")
		fl.Flush()
		time.Sleep(2 * time.Second)
	case ScnErr429:
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":429,"message":"rate limit exceeded","status":"RESOURCE_EXHAUSTED"}}`)
	case ScnErr401:
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":401,"message":"invalid api key","status":"UNAUTHENTICATED"}}`)
	}
}

// GeminiHarness returns the contract harness for Gemini.
func GeminiHarness() Harness {
	return Harness{
		Name: "gemini",
		NewModel: func(baseURL string) (model.ChatModel, error) {
			return model.NewGeminiChatModel(model.GeminiConfig{
				APIKey: "test-key", Model: "gemini-test", BaseURL: baseURL,
			})
		},
		Serve:             geminiServe,
		ExpectUsage:       geminiExpectUsage,
		ExpectStreamText:  "hello contract",
		ExpectStreamUsage: &model.ChatUsage{InputTokens: 8, OutputTokens: 4},
		MaxTokensKey:      "maxOutputTokens",
		MaxTokensPath:     "generation_config.maxOutputTokens",
	}
}
