package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type failingOpenAIFormatter struct{ err error }

func (f failingOpenAIFormatter) Format([]*message.Msg) ([]map[string]any, error) {
	return []map[string]any{{"role": "user", "content": "partial"}}, f.err
}

func TestCompatibleFormatterErrorBeforeDispatch(t *testing.T) {
	providers := []struct {
		name     string
		newModel func(string) (ChatModel, error)
	}{
		{"openai", func(url string) (ChatModel, error) {
			return NewOpenAIChatModel(OpenAIConfig{APIKey: "test", Model: "test", BaseURL: url})
		}},
		{"dashscope", func(url string) (ChatModel, error) {
			return NewDashScopeChatModel(DashScopeConfig{APIKey: "test", Model: "test", BaseURL: url + "/v1"})
		}},
		{"deepseek", func(url string) (ChatModel, error) {
			return NewDeepSeekChatModel(DeepSeekConfig{APIKey: "test", Model: "test", BaseURL: url})
		}},
		{"moonshot", func(url string) (ChatModel, error) {
			return NewMoonshotChatModel(MoonshotConfig{APIKey: "test", Model: "test", BaseURL: url})
		}},
		{"ollama", func(url string) (ChatModel, error) {
			return NewOllamaChatModel(OllamaConfig{Model: "test", BaseURL: url})
		}},
		{"xai", func(url string) (ChatModel, error) {
			return NewXAIChatModel(XAIConfig{APIKey: "test", Model: "test", BaseURL: url})
		}},
		{"volcengine", func(url string) (ChatModel, error) {
			return NewVolcengineChatModel(&VolcengineConfig{APIKey: "test", Model: "test", BaseURL: url + "/v1"})
		}},
	}

	for _, p := range providers {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", p.name, stream), func(t *testing.T) {
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected dispatch", 400) }))
				defer srv.Close()
				m, err := p.newModel(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				msgs := []*message.Msg{
					message.AssistantMsg("bot", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "lookup", Input: "{}"}}),
					message.NewMsg("tool", message.RoleUser, []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "call_1", Output: "shipped"}}),
					message.UserMsg("user", []message.ContentBlock{message.DataBlock{Type: "data", Source: message.Base64Source{MediaType: "audio/ogg", Data: "QUJD"}}}),
				}
				before, err := json.Marshal(msgs)
				if err != nil {
					t.Fatal(err)
				}
				if stream {
					ch, e := m.ChatStream(context.Background(), msgs)
					err = e
					if ch != nil {
						for range ch {
						}
						t.Error("returned stream on formatter failure")
					}
				} else {
					response, e := m.Chat(context.Background(), msgs)
					err = e
					if response != nil {
						t.Error("returned response on formatter failure")
					}
				}
				if err == nil {
					t.Fatal("expected formatter error")
				}
				if !strings.Contains(err.Error(), "audio") {
					t.Errorf("lost audio error cause: %v", err)
				}
				after, marshalErr := json.Marshal(msgs)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if !bytes.Equal(before, after) {
					t.Error("formatter failure mutated history")
				}
				if calls.Load() != 0 {
					t.Fatalf("formatter failure dispatched %d HTTP requests: %v", calls.Load(), err)
				}
			})
		}
	}
}

func TestOpenAICustomFormatterErrorIdentity(t *testing.T) {
	sentinel := errors.New("formatter rejected history")
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 400) }))
			defer srv.Close()
			m, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "test", Model: "test", BaseURL: srv.URL, Formatter: failingOpenAIFormatter{sentinel}})
			if err != nil {
				t.Fatal(err)
			}
			msgs := []*message.Msg{message.UserMsg("u", "hello")}
			if stream {
				ch, e := m.ChatStream(context.Background(), msgs)
				err = e
				if ch != nil {
					for range ch {
					}
					t.Error("non-nil stream")
				}
			} else {
				_, err = m.Chat(context.Background(), msgs)
			}
			if !errors.Is(err, sentinel) {
				t.Errorf("error=%v, want wrapped sentinel", err)
			}
			if calls.Load() != 0 {
				t.Errorf("HTTP requests=%d", calls.Load())
			}
		})
	}
}
