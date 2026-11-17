package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestGeminiToolResultErrorWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			requests := make(chan geminiRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req geminiRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				requests <- req
				body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ack"}]},"finishReason":"STOP"}]}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, body)
				}
			}))
			defer server.Close()
			model, err := NewGeminiChatModel(GeminiConfig{APIKey: "test-only", Model: "test-model", BaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			blocks := []message.ContentBlock{
				message.ToolResultBlock{Type: "tool_result", ID: "ok", Name: "success", State: message.ToolResultSuccess, Output: "ok"},
				message.ToolResultBlock{Type: "tool_result", ID: "failed", Name: "failure", State: message.ToolResultError, Output: []message.ContentBlock{message.TextBlock{Text: "first"}, message.TextBlock{Text: "second"}}},
				message.ToolResultBlock{Type: "tool_result", ID: "empty", Name: "empty", State: message.ToolResultError, Output: ""},
				message.ToolResultBlock{Type: "tool_result", ID: "denied", Name: "denied", State: message.ToolResultDenied, Output: "no"},
			}
			msg := message.NewMsg("worker", message.RoleUser, blocks)
			before, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if stream {
				ch, callErr := model.ChatStream(ctx, []*message.Msg{msg})
				if callErr != nil {
					t.Fatal(callErr)
				}
				for chunk := range ch {
					if chunk.Error != nil {
						t.Fatal(chunk.Error)
					}
				}
			} else if _, callErr := model.Chat(ctx, []*message.Msg{msg}); callErr != nil {
				t.Fatal(callErr)
			}
			select {
			case req := <-requests:
				want := []geminiContent{{Role: "user", Parts: []geminiPart{
					{FunctionResponse: &geminiFunctionResponse{Name: "success", Response: map[string]any{"result": "ok"}}},
					{FunctionResponse: &geminiFunctionResponse{Name: "failure", Response: map[string]any{"error": "first\nsecond"}}},
					{FunctionResponse: &geminiFunctionResponse{Name: "empty", Response: map[string]any{"error": ""}}},
					{FunctionResponse: &geminiFunctionResponse{Name: "denied", Response: map[string]any{"result": "no"}}},
				}}}
				if !reflect.DeepEqual(req.Contents, want) {
					gotJSON, _ := json.Marshal(req.Contents)
					t.Errorf("unexpected wire contents: %s", gotJSON)
				}
			case <-ctx.Done():
				t.Fatal("no HTTP request captured")
			}
			after, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("stored tool results changed")
			}
		})
	}
}
