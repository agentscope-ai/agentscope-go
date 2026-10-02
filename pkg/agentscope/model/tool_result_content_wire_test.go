package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenAIFamilyToolResultContentWire(t *testing.T) {
	const wantJSON = `[
		{"role":"user","content":"Weather?"},
		{"role":"assistant","content":"Checking.","tool_calls":[
			{"id":"a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Shanghai\"}"}},
			{"id":"b","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Hangzhou\"}"}}]},
		{"role":"tool","tool_call_id":"a","content":"first\n[image/png data]\nlast"},
		{"role":"tool","tool_call_id":"b","content":"[audio/wav data]"},
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
			message.ToolResultBlock{Type: "tool_result", ID: "a", Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}, message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "image/png", Data: strings.Repeat("A", 10000)}}, message.TextBlock{Type: "text", Text: "last"}}},
			message.ToolResultBlock{Type: "tool_result", ID: "b", Output: []message.ContentBlock{message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "audio/wav", Data: strings.Repeat("B", 10000)}}}},
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

func TestNativeToolResultContentWire(t *testing.T) {
	for _, provider := range []string{"gemini", "anthropic", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, stream), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, err.Error(), 400)
						return
					}
					raw, _ := json.Marshal(body)
					if !strings.Contains(string(raw), `first\n[audio/wav data]\nlast`) {
						http.Error(w, "tool output missing text or media placeholder", 400)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					switch provider {
					case "gemini":
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							var compact bytes.Buffer
							if err := json.Compact(&compact, []byte(geminiResponseJSON())); err != nil {
								t.Error(err)
								return
							}
							fmt.Fprintf(w, "data: %s\n\n", compact.String())
						} else {
							fmt.Fprint(w, geminiResponseJSON())
						}
					case "anthropic":
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, anthropicStreamResponseSSE())
						} else {
							fmt.Fprint(w, anthropicResponseJSON())
						}
					case "responses":
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
						} else {
							fmt.Fprint(w, `{"status":"completed","output":[]}`)
						}
					}
				}))
				defer srv.Close()
				var m ChatModel
				var err error
				switch provider {
				case "gemini":
					m, err = NewGeminiChatModel(GeminiConfig{APIKey: "fixture", Model: "fixture", BaseURL: srv.URL})
				case "anthropic":
					m, err = NewAnthropicChatModel(&AnthropicConfig{APIKey: "fixture", Model: "fixture", BaseURL: srv.URL})
				case "responses":
					m, err = NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "fixture", Model: "fixture", BaseURL: srv.URL})
				}
				if err != nil {
					t.Fatal(err)
				}
				msgs := []*message.Msg{message.AssistantMsg("bot", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "read", Input: "{}"}}), message.NewMsg("tool", message.RoleUser, []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "call", Name: "read", Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}, message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "audio/wav", Data: strings.Repeat("A", 10000)}}, message.TextBlock{Type: "text", Text: "last"}}}})}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if !stream {
					if _, err := m.Chat(ctx, msgs); err != nil {
						t.Fatal(err)
					}
				} else {
					ch, err := m.ChatStream(ctx, msgs)
					if err != nil {
						t.Fatal(err)
					}
					last := false
					for response := range ch {
						if response.Error != nil {
							t.Error(response.Error)
						}
						last = last || response.IsLast
					}
					if !last {
						t.Fatal("missing stream terminal")
					}
				}
			})
		}
	}
}

func TestToolResultTokenEstimateIncludesAllText(t *testing.T) {
	m, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "fixture", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	first := message.TextBlock{Type: "text", Text: "first"}
	msgs := []*message.Msg{message.AssistantMsg("bot", []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "call", Output: []message.ContentBlock{first}}})}
	before := m.CountTokens(msgs, nil)
	msgs[0].Content = []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "call", Output: []message.ContentBlock{first, message.TextBlock{Type: "text", Text: strings.Repeat("second", 10000)}}}}
	after := m.CountTokens(msgs, nil)
	if after < before+10000 {
		t.Fatalf("later tool text ignored: before=%d after=%d", before, after)
	}
}

func TestResponsesMixedNativeToolMediaWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Input []map[string]any `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				var output any
				for _, item := range body.Input {
					if item["type"] == "function_call_output" {
						if item["call_id"] != "call" {
							t.Error("lost tool pairing")
						}
						output = item["output"]
					}
				}
				want := []any{map[string]any{"type": "input_text", "text": "first"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,QUJD"}, map[string]any{"type": "input_text", "text": "[audio/wav data]"}, map[string]any{"type": "input_text", "text": "last"}}
				if !reflect.DeepEqual(output, want) {
					t.Errorf("output=%v", output)
					http.Error(w, "incorrect media sequence", 400)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				} else {
					fmt.Fprint(w, `{"output":[]}`)
				}
			}))
			defer srv.Close()
			m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "fixture", Model: "fixture", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			output := []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}, message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "image/png", Data: "QUJD"}}, message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "audio/wav", Data: "QUJD"}}, message.TextBlock{Type: "text", Text: "last"}}
			msgs := []*message.Msg{message.AssistantMsg("bot", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "read", Input: "{}"}, message.ToolResultBlock{Type: "tool_result", ID: "call", Name: "read", Output: output}})}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if !stream {
				if _, err := m.Chat(ctx, msgs); err != nil {
					t.Fatal(err)
				}
			} else {
				ch, err := m.ChatStream(ctx, msgs)
				if err != nil {
					t.Fatal(err)
				}
				last := false
				for response := range ch {
					if response.Error != nil {
						t.Error(response.Error)
					}
					last = last || response.IsLast
				}
				if !last {
					t.Fatal("missing terminal")
				}
			}
		})
	}
}
