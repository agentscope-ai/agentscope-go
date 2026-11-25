package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestGeminiMixedHistoryHTTP(t *testing.T) {
	text := func(s string) message.TextBlock { return message.TextBlock{Type: "text", Text: s} }
	call := func(id string) message.ToolCallBlock {
		return message.ToolCallBlock{
			Type: "tool_call", ID: id, Name: "lookup", Input: "{}",
			Extra: map[string]any{"thought_signature": "sig-" + id},
		}
	}
	result := func(id string, state message.ToolResultState) message.ToolResultBlock {
		return message.ToolResultBlock{
			Type: "tool_result", ID: id, Name: "lookup", Output: "fixture", State: state,
		}
	}
	asst := func(blocks ...message.ContentBlock) *message.Msg {
		return &message.Msg{Role: message.RoleAssistant, Content: blocks}
	}
	success := message.ToolResultSuccess
	lineup := []struct {
		name    string
		history []*message.Msg
		want    string
	}{
		{
			name:    "separate-messages",
			history: []*message.Msg{asst(text("before")), asst(call("a")), asst(result("a", success)), asst(text("after"))},
			want:    "model{text:before};model{call:a:sig-a};user{result:a:fixture};model{text:after}",
		},
		{
			name:    "plain-text",
			history: []*message.Msg{{Role: message.RoleUser, Content: []message.ContentBlock{text("ordinary query")}}},
			want:    "user{text:ordinary query}",
		},
		{
			name:    "failed-tool-result",
			history: []*message.Msg{asst(call("a")), asst(result("a", message.ToolResultError))},
			want:    "model{call:a:sig-a};user{error:a:fixture}",
		},
		{
			name:    "mixed-completed-reply",
			history: []*message.Msg{asst(text("before"), call("a"), result("a", success), text("after"))},
			want:    "model{text:before,call:a:sig-a};user{result:a:fixture};model{text:after}",
		},
		{
			name:    "pending-parallel-sibling",
			history: []*message.Msg{asst(text("before"), call("a"), call("b"), result("a", success))},
			want:    "model{text:before,call:a:sig-a,call:b:sig-b};user{result:a:fixture}",
		},
		{
			name:    "call-with-text-before-result",
			history: []*message.Msg{asst(text("before"), call("a"))},
			want:    "model{text:before,call:a:sig-a}",
		},
		{
			name: "parallel-completion-order",
			history: []*message.Msg{asst(call("a"), call("b"), result("b", success),
				message.ThinkingBlock{Type: "thinking", Thinking: "ignored"}, result("a", message.ToolResultError))},
			want: "model{call:a:sig-a,call:b:sig-b};user{result:b:fixture,error:a:fixture}",
		},
		{
			name: "sequential-tool-steps",
			history: []*message.Msg{asst(call("a"), result("a", success), text("next"), call("b"),
				result("b", success)), message.UserMsg("u", "follow up")},
			want: "model{call:a:sig-a};user{result:a:fixture};model{text:next,call:b:sig-b};user{result:b:fixture};user{text:follow up}",
		},
		{
			name: "plain-text-blocks",
			history: []*message.Msg{nil, asst(text("first"), text(""),
				message.ThinkingBlock{Type: "thinking", Thinking: "ignored"}, text(" "), text("last")), asst(text(""))},
			want: "model{text:first\n \nlast}",
		},
	}
	for _, stream := range []bool{false, true} {
		transport := "buffered"
		if stream {
			transport = "stream"
		}
		for _, tt := range lineup {
			t.Run(transport+"/"+tt.name, func(t *testing.T) {
				before, err := json.Marshal(tt.history)
				if err != nil {
					t.Fatal(err)
				}
				requestBodies := make(chan geminiRequest, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request geminiRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						http.Error(w, "decode", http.StatusBadRequest)
						return
					}
					select {
					case requestBodies <- request:
					default:
						t.Error("received an unexpected additional request")
					}
					const response = `{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture"}]},"finishReason":"STOP"}]}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\n", response)
						return
					}
					fmt.Fprint(w, response)
				}))
				defer server.Close()
				adapter, err := NewGeminiChatModel(GeminiConfig{
					APIKey: "test-only", Model: "fixture", BaseURL: server.URL, HTTPClient: server.Client(),
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if stream {
					responses, err := adapter.ChatStream(ctx, tt.history)
					if err != nil {
						t.Fatal(err)
					}
					final := false
					for response := range responses {
						if response.Error != nil {
							t.Fatal(response.Error)
						}
						final = final || response.IsLast
					}
					if !final {
						t.Fatal("missing final stream response")
					}
				} else if _, err := adapter.Chat(ctx, tt.history); err != nil {
					t.Fatal(err)
				}
				after, err := json.Marshal(tt.history)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Error("request mutated stored message blocks")
				}
				select {
				case request := <-requestBodies:
					if got := summarizeGeminiContents(request.Contents); got != tt.want {
						t.Errorf("ordered contents got %q; want %q", got, tt.want)
					}
				default:
					t.Fatal("missing local HTTP request")
				}
			})
		}
	}
}

func summarizeGeminiContents(contents []geminiContent) string {
	var entries []string
	for _, entry := range contents {
		var parts []string
		for _, part := range entry.Parts {
			switch {
			case part.FunctionCall != nil:
				parts = append(parts, "call:"+part.FunctionCall.ID+":"+part.ThoughtSignature)
			case part.FunctionResponse != nil:
				for _, key := range []string{"result", "error"} {
					if value, ok := part.FunctionResponse.Response[key]; ok {
						parts = append(parts, fmt.Sprintf("%s:%s:%v", key, part.FunctionResponse.ID, value))
					}
				}
			default:
				parts = append(parts, "text:"+part.Text)
			}
		}
		entries = append(entries, entry.Role+"{"+strings.Join(parts, ",")+"}")
	}
	return strings.Join(entries, ";")
}
