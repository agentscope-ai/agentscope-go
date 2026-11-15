package model

import (
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

func TestOpenAIFamilyToolHistoryWire(t *testing.T) {
	const wantJSON = `[
		{"role":"user","content":"Weather?"},
		{"role":"assistant","content":"Checking.","tool_calls":[
			{"id":"a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Shanghai\"}"}},
			{"id":"b","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Hangzhou\"}"}}]},
		{"role":"tool","tool_call_id":"a","content":"18 C"},
		{"role":"tool","tool_call_id":"b","content":"20 C"},
		{"role":"assistant","content":"Done."}
	]`
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages any  `json:"messages"`
			Stream   bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !reflect.DeepEqual(req.Messages, want) {
			http.Error(w, fmt.Sprintf("unexpected tool history: %#v", req.Messages), http.StatusBadRequest)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, openAIStreamResponseSSE())
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, openAIResponseJSON())
		}
	}))
	defer srv.Close()
	providers := []struct {
		name string
		m    ChatModel
	}{
		{"openai", mustChatModel(NewOpenAIChatModel(OpenAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL}))},
		{"dashscope", mustChatModel(NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "m", BaseURL: srv.URL}))},
		{"deepseek", mustChatModel(NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "m", BaseURL: srv.URL}))},
		{"moonshot", mustChatModel(NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "m", BaseURL: srv.URL}))},
		{"ollama", mustChatModel(NewOllamaChatModel(OllamaConfig{Model: "m", BaseURL: srv.URL}))},
		{"xai", mustChatModel(NewXAIChatModel(XAIConfig{APIKey: "k", Model: "m", BaseURL: srv.URL}))},
	}
	msgs := []*message.Msg{
		message.UserMsg("user", "Weather?"),
		message.AssistantMsg("agent", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "Checking."},
			message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "weather", Input: `{"city":"Shanghai"}`},
			message.ToolCallBlock{Type: "tool_call", ID: "b", Name: "weather", Input: `{"city":"Hangzhou"}`},
			message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "18 C"},
			message.ToolResultBlock{Type: "tool_result", ID: "b", Output: "20 C"},
			message.TextBlock{Type: "text", Text: "Done."},
		}),
	}
	for _, provider := range providers {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider.name, stream), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if !stream {
					if _, err := provider.m.Chat(ctx, msgs); err != nil {
						t.Fatal(err)
					}
					return
				}
				ch, err := provider.m.ChatStream(ctx, msgs)
				if err != nil {
					t.Fatal(err)
				}
				finished := false
				for response := range ch {
					if response.Error != nil {
						t.Fatal(response.Error)
					}
					finished = finished || response.IsLast
				}
				if !finished {
					t.Fatalf("stream did not finish: %v", ctx.Err())
				}
			})
		}
	}
}
