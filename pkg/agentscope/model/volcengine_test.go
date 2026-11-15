package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type volcengineTestTransport func(*http.Request) (*http.Response, error)

func (f volcengineTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newVolcengineTestModel(t *testing.T, cfg *VolcengineConfig) *VolcengineChatModel {
	t.Helper()
	m, err := NewVolcengineChatModel(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func volcengineTestCall(t *testing.T, m ChatModel, stream bool, msgs []*message.Msg, opts ...CallOption) *ChatResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !stream {
		r, err := m.Chat(ctx, msgs, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ch, err := m.ChatStream(ctx, msgs, opts...)
	if err != nil {
		t.Fatal(err)
	}
	var last *ChatResponse
	for r := range ch {
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		if r.IsLast {
			cp := r
			last = &cp
		}
	}
	if last == nil {
		t.Fatalf("missing final response: %v", ctx.Err())
	}
	return last
}

func TestVolcengineConfiguration(t *testing.T) {
	if _, err := NewVolcengineChatModel(nil); err == nil {
		t.Fatal("accepted nil config")
	}
	for _, cfg := range []VolcengineConfig{{}, {APIKey: "key"}, {Model: "model"}} {
		if _, err := NewVolcengineChatModel(&cfg); err == nil {
			t.Fatalf("accepted incomplete config: %+v", cfg)
		}
	}
	for _, casing := range []string{"Authorization", "authorization", "aUtHoRiZaTiOn"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", casing, stream), func(t *testing.T) {
				var calls int
				transport := volcengineTestTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.String() != "https://ark.cn-beijing.volces.com/api/v3/chat/completions" {
						t.Errorf("URL = %s", r.URL)
					}
					if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Test") != "custom" {
						t.Errorf("unexpected headers")
					}
					body, contentType := openAIResponseJSON(), "application/json"
					if stream {
						body, contentType = openAIStreamResponseSSE(), "text/event-stream"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				client := &http.Client{Transport: transport}
				m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "legacy", SecretAPIKey: NewSecretStr("secret"), Model: "ark-model", HTTPClient: client, ClientOptions: &ClientOptions{DefaultHeaders: map[string]string{"X-Test": "custom", casing: "override", "content-type": "text/plain"}}})
				for _, name := range []string{"Authorization", "Content-Type"} {
					count := 0
					for key := range m.headers() {
						if strings.EqualFold(key, name) {
							count++
						}
					}
					if count != 1 {
						t.Fatalf("duplicate case-insensitive header %s", name)
					}
				}
				var _ ChatModel = m
				var _ ThinkingDisabler = m
				volcengineTestCall(t, m, stream, []*message.Msg{message.UserMsg("user", "hello")})
				if calls != 1 {
					t.Fatalf("requests = %d", calls)
				}
				if m.CountTokens(nil, nil) != 0 || m.CountTokens([]*message.Msg{message.UserMsg("u", "hello world")}, nil) <= 0 {
					t.Fatal("invalid token estimate")
				}
			})
		}
	}
}

func TestVolcengineRequestWire(t *testing.T) {
	cases := []struct {
		name string
		opts []CallOption
		want map[string]any
	}{
		{"unset", nil, map[string]any{}},
		{"enabled", []CallOption{WithThinking(true, 0)}, map[string]any{"thinking": map[string]any{"type": "enabled"}}},
		{"disabled", []CallOption{WithThinking(false, 0)}, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"disable_helper", []CallOption{WithThinkingDisabled()}, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"sampling", []CallOption{WithTemperature(0.5), WithTopP(0.75), WithMaxTokens(123)}, map[string]any{"temperature": 0.5, "top_p": 0.75, "max_tokens": float64(123)}},
		{"tools", []CallOption{WithTools([]ToolSchema{{Type: "function", Function: ToolFunction{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}}), WithToolChoice(&ToolChoice{Mode: "weather"})}, map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "weather", "description": "", "parameters": map[string]any{"type": "object"}}}}, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}}},
	}
	for _, effort := range []string{"minimal", "low", "medium", "high"} {
		cases = append(cases, struct {
			name string
			opts []CallOption
			want map[string]any
		}{"effort_" + effort, []CallOption{WithReasoningEffort(effort)}, map[string]any{"reasoning_effort": effort}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [2]map[string]any
			for i, stream := range []bool{false, true} {
				captured := make(chan map[string]any, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/proxy/api/v3/chat/completions" || r.Header.Get("Authorization") != "Bearer key" {
						t.Error("incorrect dispatch")
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					captured <- body
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, openAIStreamResponseSSE())
					} else {
						fmt.Fprint(w, openAIResponseJSON())
					}
				}))
				base := srv.URL + "/proxy/api/v3"
				if stream {
					base += "/"
				}
				m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "ark-model", BaseURL: base})
				volcengineTestCall(t, m, stream, []*message.Msg{message.UserMsg("user", "hello")}, tc.opts...)
				srv.Close()
				body := <-captured
				if stream {
					if body["stream"] != true || !reflect.DeepEqual(body["stream_options"], map[string]any{"include_usage": true}) {
						t.Fatalf("stream flags: %#v", body)
					}
					delete(body, "stream")
					delete(body, "stream_options")
				}
				want := map[string]any{"model": "ark-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
				for k, v := range tc.want {
					want[k] = v
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("request=%#v, want=%#v", body, want)
				}
				bodies[i] = body
			}
			if !reflect.DeepEqual(bodies[0], bodies[1]) {
				t.Fatal("chat and stream options differ")
			}
		})
	}
}

func TestVolcengineInvalidCalls(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected dispatch", 400)
	}))
	defer srv.Close()
	m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "model", BaseURL: srv.URL})
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			msgs []*message.Msg
			opts []CallOption
		}{
			{}, {msgs: []*message.Msg{message.UserMsg("u", "hi")}, opts: []CallOption{WithThinking(true, 100)}},
			{msgs: []*message.Msg{message.UserMsg("u", "hi")}, opts: []CallOption{WithThinking(false, 100)}},
		} {
			var err error
			if stream {
				_, err = m.ChatStream(context.Background(), tc.msgs, tc.opts...)
			} else {
				_, err = m.Chat(context.Background(), tc.msgs, tc.opts...)
			}
			if err == nil {
				t.Fatal("invalid call succeeded")
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid calls dispatched %d requests", requests.Load())
	}
}

const volcengineResponseFixture = `{"id":"ark-id","model":"ark-model","choices":[{"message":{"role":"assistant","reasoning_content":"check","content":"answer","tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Beijing\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}}`

const volcengineStreamFixture = `data: {"id":"ark-id","model":"ark-model","choices":[{"delta":{"reasoning_content":"check","content":"ans","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}

data: {"choices":[{"delta":{"content":"wer","tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}}

data: [DONE]

`

func TestVolcengineResponsesAndToolFollowup(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			captured := make(chan []map[string]any, 2)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []map[string]any `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "decode", 400)
					return
				}
				captured <- req.Messages
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, volcengineStreamFixture)
				} else {
					fmt.Fprint(w, volcengineResponseFixture)
				}
			}))
			defer srv.Close()
			m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "ark-model", BaseURL: srv.URL})
			msgs := []*message.Msg{message.UserMsg("user", "weather?")}
			r := volcengineTestCall(t, m, stream, msgs)
			if r.ID != "ark-id" || r.ModelName != "ark-model" || r.StopReason != StopReasonToolCalls || r.GetTextContent() != "answer" || !r.IsLast {
				t.Fatalf("response: %+v", r)
			}
			if r.Usage == nil || r.Usage.InputTokens != 20 || r.Usage.OutputTokens != 9 || r.Usage.CacheInputTokens != 4 {
				t.Fatalf("usage: %+v", r.Usage)
			}
			if len(r.Content) != 3 {
				t.Fatalf("content: %+v", r.Content)
			}
			thinking, ok := r.Content[0].(message.ThinkingBlock)
			if !ok || thinking.Thinking != "check" {
				t.Fatalf("thinking: %+v", r.Content[0])
			}
			call, ok := r.Content[2].(message.ToolCallBlock)
			if !ok || call.ID != "call-1" || call.Name != "weather" || call.Input != `{"city":"Beijing"}` {
				t.Fatalf("tool: %+v", r.Content[2])
			}
			<-captured
			answer := r.ToMsg("agent")
			answer.Content = append(answer.Content, message.ToolResultBlock{Type: "tool_result", ID: call.ID, Output: "18 C"})
			volcengineTestCall(t, m, stream, append(msgs, answer))
			history := <-captured
			if len(history) != 3 || history[1]["role"] != "assistant" || history[2]["role"] != "tool" || history[2]["tool_call_id"] != "call-1" || history[2]["content"] != "18 C" {
				t.Fatalf("tool history: %#v", history)
			}
			calls := history[1]["tool_calls"].([]any)
			if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call-1" {
				t.Fatalf("preceding calls: %#v", calls)
			}
		})
	}
}

func TestVolcengineStreamFailures(t *testing.T) {
	for _, suffix := range []string{"", "data: {bad\n\n"} {
		t.Run(fmt.Sprintf("suffix=%q", suffix), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"+suffix)
			}))
			defer srv.Close()
			m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
			if err != nil {
				t.Fatal(err)
			}
			var last ChatResponse
			for r := range ch {
				last = r
			}
			if !last.IsLast || last.Error == nil || last.GetTextContent() != "partial" {
				t.Fatalf("truncated response: %+v", last)
			}
		})
	}
}

func TestVolcengineCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		if r.GetTextContent() != "partial" || r.IsLast {
			t.Fatalf("initial delta: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("no initial delta")
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return
			}
			if r.IsLast && r.Error == nil {
				t.Fatal("canceled stream completed successfully")
			}
		case <-deadline:
			t.Fatal("canceled stream did not close")
		}
	}
}

func TestVolcengineSetupErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"unsupported thinking"}}`, http.StatusBadRequest)
		}))
		m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		msgs := []*message.Msg{message.UserMsg("u", "hi")}
		var err error
		if stream {
			_, err = m.ChatStream(context.Background(), msgs)
		} else {
			_, err = m.Chat(context.Background(), msgs)
		}
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "unsupported thinking") {
			t.Fatalf("provider error: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if stream {
			_, err = m.ChatStream(ctx, msgs)
		} else {
			_, err = m.Chat(ctx, msgs)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
}

func TestVolcengineStructuredOutputFallback(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		step := requests.Add(1)
		if step == 1 {
			http.Error(w, "tool_choice cannot be used with thinking", 400)
			return
		}
		if step == 2 {
			fmt.Fprint(w, openAIResponseJSON())
			return
		}
		if !reflect.DeepEqual(body["thinking"], map[string]any{"type": "disabled"}) || body["tool_choice"] != "required" {
			t.Errorf("fallback request: %#v", body)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"result","type":"function","function":{"name":"generate_structured_output","arguments":"{\"answer\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()
	m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
	out, err := GenerateStructuredOutput(context.Background(), m, []*message.Msg{message.UserMsg("u", "answer")}, json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`))
	if err != nil || string(out) != `{"answer":"ok"}` || requests.Load() != 3 {
		t.Fatalf("out=%s, requests=%d, err=%v", out, requests.Load(), err)
	}
}

// The provider must release the HTTP reader when processing ends, even when
// the caller keeps its context alive and the server does not close the socket.
func TestVolcengineStreamReleasesReader(t *testing.T) {
	for _, terminal := range []string{"[DONE]", "{bad"} {
		t.Run(terminal, func(t *testing.T) {
			stopped := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", terminal)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(stopped)
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
			ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.After(2 * time.Second)
			for {
				select {
				case r, ok := <-ch:
					if !ok {
						select {
						case <-stopped:
							return
						case <-deadline:
							t.Fatal("HTTP reader retained after processor exit")
						}
					}
					if r.IsLast && (r.Error != nil) != (terminal == "{bad") {
						t.Fatalf("terminal response: %+v", r)
					}
				case <-deadline:
					t.Fatal("stream processor did not finish")
				}
			}
		})
	}
}

func TestVolcengineStopReasons(t *testing.T) {
	for _, tc := range []struct{ wire, want string }{
		{"stop", StopReasonStop}, {"length", StopReasonLength},
		{"content_filter", StopReasonContentFilter}, {"tool_calls", StopReasonToolCalls},
		{"", ""},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, strings.Replace(volcengineResponseFixture, `"finish_reason":"tool_calls"`, `"finish_reason":"`+tc.wire+`"`, 1))
			}))
			defer srv.Close()
			m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
			r := volcengineTestCall(t, m, false, []*message.Msg{message.UserMsg("u", "hi")})
			if r.StopReason != tc.want {
				t.Fatalf("stop reason=%q, want=%q", r.StopReason, tc.want)
			}
		})
	}
}

func TestVolcengineClientOptions(t *testing.T) {
	headers := map[string]string{"X-Test": "original"}
	var calls int
	transport := volcengineTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("X-Test") != "original" {
			t.Error("caller header mutation affected model")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(openAIResponseJSON())), Request: r}, nil
	})
	m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", ClientOptions: &ClientOptions{Transport: transport, Timeout: time.Second, DefaultHeaders: headers}})
	headers["X-Test"] = "changed"
	volcengineTestCall(t, m, false, []*message.Msg{message.UserMsg("u", "hi")})
	if calls != 1 || m.httpClient.Timeout != time.Second {
		t.Fatal("client options were not applied")
	}
}

func TestVolcengineResponseErrors(t *testing.T) {
	for _, body := range []string{`{"choices":[]}`, `{bad`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		m := newVolcengineTestModel(t, &VolcengineConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		_, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
		srv.Close()
		if err == nil {
			t.Fatalf("accepted invalid response %q", body)
		}
	}
}
