package model

import (
	"bytes"
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

type miniMaxTransport func(*http.Request) (*http.Response, error)

func (f miniMaxTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newMiniMaxTestModel(t *testing.T, cfg *MiniMaxConfig) *MiniMaxChatModel {
	t.Helper()
	m, err := NewMiniMaxChatModel(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func miniMaxCall(t *testing.T, m ChatModel, stream bool, msgs []*message.Msg, opts ...CallOption) *ChatResponse {
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

const miniMaxResponse = `{"id":"mini-id","model":"MiniMax-M3.1-Flash","content":[{"type":"thinking","thinking":"analyze","signature":"signed"},{"type":"text","text":"answer"},{"type":"tool_use","id":"call-1","name":"weather","input":{"city":"Beijing"}}],"stop_reason":"tool_use","usage":{"input_tokens":13,"output_tokens":9,"cache_creation_input_tokens":0,"cache_read_input_tokens":159}}`

const miniMaxSSE = `event: message_start
data: {"type":"message_start","message":{"id":"mini-id","model":"MiniMax-M3.1-Flash","usage":{"input_tokens":13,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":159}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"analyze"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ans"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"wer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-1","name":"weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"Beijing\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`

func miniMaxReply(w http.ResponseWriter, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, miniMaxSSE)
	} else {
		fmt.Fprint(w, miniMaxResponse)
	}
}

func TestMiniMaxConfiguration(t *testing.T) {
	for _, cfg := range []*MiniMaxConfig{nil, {}, {APIKey: "key"}, {Model: "model"}, {APIKey: "key", Model: "m", MaxOutputTokens: -1}} {
		if _, err := NewMiniMaxChatModel(cfg); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
	for _, stream := range []bool{false, true} {
		for _, casing := range []string{"Authorization", "authorization", "aUtHoRiZaTiOn"} {
			t.Run(fmt.Sprintf("%s/stream=%t", casing, stream), func(t *testing.T) {
				headers := map[string]string{casing: "override", "content-type": "text/plain", "X-Test": "original"}
				var calls int
				transport := miniMaxTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != http.MethodPost || r.URL.String() != "https://api.minimax.io/anthropic/v1/messages" {
						t.Errorf("dispatch: %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Test") != "original" {
						t.Error("incorrect header precedence")
					}
					for _, name := range []string{"Authorization", "Content-Type"} {
						count := 0
						for k := range r.Header {
							if strings.EqualFold(k, name) {
								count++
							}
						}
						if count != 1 {
							t.Errorf("duplicate %s", name)
						}
					}
					body, ct := miniMaxResponse, "application/json"
					if stream {
						body, ct = miniMaxSSE, "text/event-stream"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				client := &http.Client{Transport: transport, Timeout: time.Second}
				m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "legacy", SecretAPIKey: NewSecretStr("secret"), Model: "MiniMax-M3.1-Flash", HTTPClient: client, ClientOptions: &ClientOptions{DefaultHeaders: headers}})
				headers["X-Test"] = "changed"
				var _ ChatModel = m
				var _ ThinkingDisabler = m
				miniMaxCall(t, m, stream, []*message.Msg{message.UserMsg("user", "hi")})
				if calls != 1 || client.Timeout != time.Second {
					t.Fatal("custom client was changed")
				}
				if m.CountTokens(nil, nil) != 0 || m.CountTokens([]*message.Msg{message.UserMsg("u", "hello")}, nil) <= 0 {
					t.Fatal("invalid estimate")
				}
			})
		}
	}
}

func TestMiniMaxRequestWire(t *testing.T) {
	tool := ToolSchema{Type: "function", Function: ToolFunction{Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
	cases := []struct {
		name   string
		opts   []CallOption
		fields map[string]any
	}{
		{"unset", nil, nil},
		{"adaptive", []CallOption{WithThinking(true, 0)}, map[string]any{"thinking": map[string]any{"type": "adaptive"}}},
		{"disabled", []CallOption{WithThinking(false, 0)}, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"disable_helper", []CallOption{WithThinkingDisabled()}, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"max_tokens", []CallOption{WithMaxTokens(777)}, map[string]any{"max_tokens": float64(777)}},
		{"ignored_sampling", []CallOption{WithTemperature(0.5), WithTopP(0.8), WithSeed(7)}, nil},
	}
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		cases = append(cases, struct {
			name   string
			opts   []CallOption
			fields map[string]any
		}{"effort_" + effort, []CallOption{WithReasoningEffort(effort)}, map[string]any{"output_config": map[string]any{"effort": effort}}})
	}
	for _, choice := range []string{"auto", "required", "weather"} {
		wire := map[string]any{"type": choice}
		if choice == "required" {
			wire["type"] = "any"
		}
		if choice == "weather" {
			wire["type"] = "tool"
			wire["name"] = "weather"
		}
		cases = append(cases, struct {
			name   string
			opts   []CallOption
			fields map[string]any
		}{"tools_" + choice, []CallOption{WithTools([]ToolSchema{tool}), WithToolChoice(&ToolChoice{Mode: choice})}, map[string]any{"tools": []any{map[string]any{"name": "weather", "description": "", "input_schema": map[string]any{"type": "object"}}}, "tool_choice": wire}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [2]map[string]any
			for i, stream := range []bool{false, true} {
				captured := make(chan map[string]any, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/proxy/anthropic/v1/messages" {
						t.Error("incorrect custom path")
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					captured <- body
					miniMaxReply(w, stream)
				}))
				base := srv.URL + "/proxy/anthropic"
				if stream {
					base += "/"
				}
				m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: base, MaxOutputTokens: 4096})
				miniMaxCall(t, m, stream, []*message.Msg{message.SystemMsg("system", "system"), message.UserMsg("user", "hi")}, tc.opts...)
				srv.Close()
				body := <-captured
				if stream {
					if body["stream"] != true {
						t.Fatal("missing stream flag")
					}
					delete(body, "stream")
				}
				want := map[string]any{"model": "m", "max_tokens": float64(4096), "system": "system", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}}}
				for k, v := range tc.fields {
					want[k] = v
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("body=%#v, want=%#v", body, want)
				}
				bodies[i] = body
			}
			if !reflect.DeepEqual(bodies[0], bodies[1]) {
				t.Fatal("streamed and buffered request options differ")
			}
		})
	}
}

func TestMiniMaxInvalidCalls(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "unexpected", 400) }))
	defer srv.Close()
	m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
	msgs := []*message.Msg{message.UserMsg("u", "hi")}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			ctx  context.Context
			msgs []*message.Msg
			opts []CallOption
		}{
			{context.Background(), nil, nil}, {nil, msgs, nil},
			{context.Background(), msgs, []CallOption{WithThinking(true, 100)}},
			{context.Background(), msgs, []CallOption{WithThinking(false, 100)}},
			{context.Background(), msgs, []CallOption{WithMaxTokens(0)}},
			{context.Background(), msgs, []CallOption{WithMaxTokens(-1)}},
		} {
			var err error
			if stream {
				_, err = m.ChatStream(tc.ctx, tc.msgs, tc.opts...)
			} else {
				_, err = m.Chat(tc.ctx, tc.msgs, tc.opts...)
			}
			if err == nil {
				t.Fatal("invalid call succeeded")
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid calls dispatched")
	}
}

func TestMiniMaxResponsesAndToolHistory(t *testing.T) {
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
				miniMaxReply(w, stream)
			}))
			defer srv.Close()
			m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
			msgs := []*message.Msg{message.UserMsg("u", "weather?")}
			r := miniMaxCall(t, m, stream, msgs)
			if r.ID != "mini-id" || r.ModelName != "MiniMax-M3.1-Flash" || r.GetTextContent() != "answer" || r.StopReason != StopReasonToolCalls || len(r.Content) != 3 {
				t.Fatalf("response: %+v", r)
			}
			if r.Usage == nil || r.Usage.InputTokens != 13 || r.Usage.OutputTokens != 9 || r.Usage.CacheInputTokens != 159 || r.Usage.EffectiveInputTokens() != 172 {
				t.Fatalf("usage: %+v", r.Usage)
			}
			thinking, ok := r.Content[0].(message.ThinkingBlock)
			if !ok || thinking.Thinking != "analyze" || thinking.Extra["signature"] != "signed" {
				t.Fatalf("thinking: %+v", r.Content[0])
			}
			call, ok := r.Content[2].(message.ToolCallBlock)
			if !ok || call.ID != "call-1" || call.Name != "weather" || call.Input != `{"city":"Beijing"}` {
				t.Fatalf("call: %+v", r.Content[2])
			}
			<-captured
			assistant := r.ToMsg("agent")
			result := message.NewMsg("tool", message.RoleUser, []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "call-1", Output: "18 C"}})
			miniMaxCall(t, m, stream, append(msgs, assistant, result))
			history := <-captured
			wantJSON := `[{"role":"user","content":[{"type":"text","text":"weather?"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"analyze","signature":"signed"},{"type":"text","text":"answer"},{"type":"tool_use","id":"call-1","name":"weather","input":{"city":"Beijing"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"18 C"}]}]`
			var want []map[string]any
			if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(history, want) {
				t.Fatalf("history=%#v", history)
			}
		})
	}
}

func TestMiniMaxStopReasons(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ raw, want string }{{"end_turn", StopReasonStop}, {"tool_use", StopReasonToolCalls}, {"max_tokens", StopReasonLength}} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.raw, stream), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, strings.ReplaceAll(miniMaxSSE, `"stop_reason":"tool_use"`, `"stop_reason":"`+tc.raw+`"`))
					} else {
						fmt.Fprint(w, strings.ReplaceAll(miniMaxResponse, `"stop_reason":"tool_use"`, `"stop_reason":"`+tc.raw+`"`))
					}
				}))
				defer srv.Close()
				m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
				r := miniMaxCall(t, m, stream, []*message.Msg{message.UserMsg("u", "hi")})
				if r.StopReason != tc.want {
					t.Fatalf("stop reason=%q", r.StopReason)
				}
			})
		}
	}
}

func TestMiniMaxReasoningOnlyAndCacheUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"x","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":159}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"reasoning"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":0,"output_tokens":9,"cache_read_input_tokens":200,"cache_creation_input_tokens":0}}

event: message_stop
data: {"type":"message_stop"}

`)
			} else {
				fmt.Fprint(w, `{"content":[{"type":"thinking","thinking":"reasoning"}],"stop_reason":"max_tokens","usage":{"input_tokens":0,"output_tokens":9,"cache_read_input_tokens":200}}`)
			}
		}))
		m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		r := miniMaxCall(t, m, stream, []*message.Msg{message.UserMsg("u", "hi")})
		srv.Close()
		if r.GetTextContent() != "" || r.StopReason != StopReasonLength || len(r.Content) != 1 || r.Usage == nil || r.Usage.CacheInputTokens != 200 || r.Usage.EffectiveInputTokens() != 200 {
			t.Fatalf("reasoning-only exhaustion: %+v usage=%+v", r, r.Usage)
		}
	}
}

func TestMiniMaxStreamFailuresAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, event     string
		wantError, hold bool
	}{
		{"complete", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", false, true},
		{"malformed", "event: content_block_delta\ndata: {bad\n\n", true, true},
		{"provider_error", "event: error\ndata: {\"error\":{\"type\":\"api_error\",\"message\":\"failed\"}}\n\n", true, true},
		{"premature_eof", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(stopped)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.event)
				w.(http.Flusher).Flush()
				if tc.hold {
					<-r.Context().Done()
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
			ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
			if err != nil {
				t.Fatal(err)
			}
			var last ChatResponse
			for r := range ch {
				last = r
			}
			if ctx.Err() != nil || !last.IsLast || (last.Error != nil) != tc.wantError {
				t.Fatalf("terminal response: %+v, ctx=%v", last, ctx.Err())
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("HTTP reader retained")
			}
		})
	}
}

func TestMiniMaxCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
	ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		if r.IsLast || r.GetTextContent() != "partial" {
			t.Fatalf("delta: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("no delta")
	}
	cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return
			}
			if r.IsLast && r.Error == nil {
				t.Fatal("cancellation became successful completion")
			}
		case <-deadline:
			t.Fatal("stream did not close")
		}
	}
}

func TestMiniMaxProviderRejections(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, opts := range [][]CallOption{{WithThinkingDisabled()}, {WithReasoningEffort("none")}, {WithToolChoice(&ToolChoice{Mode: "required"}), WithTools([]ToolSchema{{Type: "function", Function: ToolFunction{Name: "weather", Parameters: json.RawMessage(`{}`)}}})}} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":{"message":"model rejects thinking/effort/tool_choice combination"}}`, 400)
			}))
			m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "MiniMax-M3.1-Flash", BaseURL: srv.URL})
			msgs := []*message.Msg{message.UserMsg("u", "hi")}
			var err error
			if stream {
				_, err = m.ChatStream(context.Background(), msgs, opts...)
			} else {
				_, err = m.Chat(context.Background(), msgs, opts...)
			}
			srv.Close()
			if err == nil || !strings.Contains(err.Error(), "model rejects") {
				t.Fatalf("provider rejection: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if stream {
				_, err = m.ChatStream(ctx, msgs)
			} else {
				_, err = m.Chat(ctx, msgs)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("context error: %v", err)
			}
		}
	}
}

func TestMiniMaxStructuredFallbackCannotDisable(t *testing.T) {
	var step atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		switch step.Add(1) {
		case 1:
			http.Error(w, "tool_choice rejected", 400)
		case 2:
			fmt.Fprint(w, `{"content":[{"type":"text","text":"no structured answer"}]}`)
		case 3:
			if !reflect.DeepEqual(body["thinking"], map[string]any{"type": "disabled"}) {
				t.Error("missing disable-thinking fallback")
			}
			http.Error(w, "model requires adaptive thinking", 400)
		default:
			if _, ok := body["thinking"]; ok {
				t.Error("disabled thinking leaked to next strategy")
			}
			fmt.Fprint(w, `{"content":[{"type":"text","text":"{\"answer\":\"ok\"}"}],"stop_reason":"end_turn"}`)
		}
	}))
	defer srv.Close()
	m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "MiniMax-M3.1-Flash", BaseURL: srv.URL})
	out, err := GenerateStructuredOutput(context.Background(), m, []*message.Msg{message.UserMsg("u", "answer")}, json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`))
	if err != nil || string(out) != `{"answer":"ok"}` || step.Load() != 4 {
		t.Fatalf("result=%s, steps=%d, err=%v", out, step.Load(), err)
	}
}

func TestMiniMaxCacheOnlyUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"cache_read_input_tokens\":159}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			} else {
				fmt.Fprint(w, `{"content":[{"type":"text","text":""}],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":159}}`)
			}
		}))
		m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		r := miniMaxCall(t, m, stream, []*message.Msg{message.UserMsg("u", "hi")})
		srv.Close()
		if r.Usage == nil || r.Usage.InputTokens != 0 || r.Usage.OutputTokens != 0 || r.Usage.CacheInputTokens != 159 || r.Usage.EffectiveInputTokens() != 159 {
			t.Fatalf("cache-only usage: %+v", r.Usage)
		}
	}
}

func TestMiniMaxTrailingUsageAndZeroUsage(t *testing.T) {
	for _, zero := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if zero {
				fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			} else {
				trailer := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":12}}\n\n"
				fmt.Fprint(w, strings.Replace(miniMaxSSE, "event: message_stop", trailer+"event: message_stop", 1))
			}
		}))
		m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		r := miniMaxCall(t, m, true, []*message.Msg{message.UserMsg("u", "hi")})
		srv.Close()
		if r.Usage == nil {
			t.Fatal("reported usage lost")
		}
		if zero {
			if r.Usage.EffectiveInputTokens() != 0 || r.Usage.OutputTokens != 0 {
				t.Fatalf("zero usage: %+v", r.Usage)
			}
		} else if r.StopReason != StopReasonToolCalls || r.Usage.OutputTokens != 12 || r.Usage.CacheInputTokens != 159 || r.Usage.InputTokens != 13 {
			t.Fatalf("trailing usage: %+v, reason=%q", r.Usage, r.StopReason)
		}
	}
}

func TestMiniMaxClientOptionsAndDecodeErrors(t *testing.T) {
	var calls int
	transport := miniMaxTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["max_tokens"] != float64(4096) {
			t.Error("incorrect default output limit")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(miniMaxResponse)), Request: r}, nil
	})
	m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", ClientOptions: &ClientOptions{Transport: transport, Timeout: time.Second}})
	miniMaxCall(t, m, false, []*message.Msg{message.UserMsg("u", "hi")})
	if calls != 1 || m.httpClient.Timeout != time.Second {
		t.Fatal("client options not applied")
	}
	for _, body := range []string{`{"content":[]}`, `{bad`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		m = newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		_, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
		srv.Close()
		if err == nil {
			t.Fatalf("accepted invalid response %q", body)
		}
	}
}

func TestMiniMaxMergedToolHistory(t *testing.T) {
	msgs := []*message.Msg{message.UserMsg("u", "weather?"), message.AssistantMsg("agent", []message.ContentBlock{
		message.ThinkingBlock{Type: "thinking", Thinking: "consider", Extra: map[string]any{"signature": "sig"}},
		message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "weather", Input: `{"city":"Beijing"}`},
		message.ToolCallBlock{Type: "tool_call", ID: "b", Name: "weather", Input: `{"city":"Shanghai"}`},
		message.ToolResultBlock{Type: "tool_result", ID: "a", Output: "18 C"},
		message.ToolResultBlock{Type: "tool_result", ID: "b", Output: "20 C"},
		message.TextBlock{Type: "text", Text: "one more"},
		message.ToolCallBlock{Type: "tool_call", ID: "c", Name: "weather", Input: `{"city":"Hangzhou"}`},
		message.ToolResultBlock{Type: "tool_result", ID: "c", Output: "19 C"},
		message.TextBlock{Type: "text", Text: "done"},
	})}
	before, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		captured := make(chan []map[string]any, 1)
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
			miniMaxReply(w, stream)
		}))
		m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
		miniMaxCall(t, m, stream, msgs)
		srv.Close()
		history := <-captured
		wantJSON := `[{"role":"user","content":[{"type":"text","text":"weather?"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"consider","signature":"sig"},{"type":"tool_use","id":"a","name":"weather","input":{"city":"Beijing"}},{"type":"tool_use","id":"b","name":"weather","input":{"city":"Shanghai"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"18 C"},{"type":"tool_result","tool_use_id":"b","content":"20 C"}]},{"role":"assistant","content":[{"type":"text","text":"one more"},{"type":"tool_use","id":"c","name":"weather","input":{"city":"Hangzhou"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"19 C"}]},{"role":"assistant","content":[{"type":"text","text":"done"}]}]`
		var want []map[string]any
		if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(history, want) {
			t.Fatalf("merged history emitted invalid roles/order: %#v", history)
		}
	}
	after, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("caller history was mutated")
	}
}

func TestMiniMaxStreamDeltaContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { miniMaxReply(w, true) }))
	defer srv.Close()
	m := newMiniMaxTestModel(t, &MiniMaxConfig{APIKey: "key", Model: "m", BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	var text, thinking strings.Builder
	finals := 0
	for r := range ch {
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		if r.IsLast {
			finals++
			if r.GetTextContent() != text.String() || r.Content[0].(message.ThinkingBlock).Thinking != thinking.String() {
				t.Fatal("final response does not assemble deltas")
			}
			continue
		}
		text.WriteString(r.GetTextContent())
		for _, b := range r.Content {
			if v, ok := b.(message.ThinkingBlock); ok {
				thinking.WriteString(v.Thinking)
			}
		}
	}
	if finals != 1 || text.String() != "answer" || thinking.String() != "analyze" {
		t.Fatalf("finals=%d text=%q thinking=%q", finals, text.String(), thinking.String())
	}
}

func TestAnthropicRequestCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		captured := make(chan map[string]any, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("x-api-key") != "anthropic-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
				t.Error("Anthropic headers changed")
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
				fmt.Fprint(w, anthropicStreamResponseSSE())
			} else {
				fmt.Fprint(w, anthropicResponseJSON())
			}
		}))
		m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "anthropic-key", Model: "claude", BaseURL: srv.URL, MaxOutputTokens: 2000})
		if err != nil {
			t.Fatal(err)
		}
		miniMaxCall(t, m, stream, []*message.Msg{message.UserMsg("u", "hi")}, WithThinking(true, 3000), WithMaxTokens(77), WithReasoningEffort("low"))
		srv.Close()
		body := <-captured
		if body["max_tokens"] != float64(4024) || !reflect.DeepEqual(body["thinking"], map[string]any{"type": "enabled", "budget_tokens": float64(3000)}) {
			t.Fatalf("Anthropic behavior changed: %#v", body)
		}
		if _, ok := body["output_config"]; ok {
			t.Fatal("MiniMax effort leaked into Anthropic")
		}
	}
}
