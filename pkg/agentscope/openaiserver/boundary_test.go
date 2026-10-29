package openaiserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/openaiserver"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func TestConfigAndRoutingBoundaries(t *testing.T) {
	var nilModel *fixtureModel
	for _, cfg := range []openaiserver.Config{
		{}, {Models: map[string]model.ChatModel{"bad": nil}}, {Models: map[string]model.ChatModel{"": fixtureModel{}}},
		{Models: map[string]model.ChatModel{"nil": nilModel}},
		{Models: map[string]model.ChatModel{"x": fixtureModel{}}, MaxConcurrent: -1},
		{Models: map[string]model.ChatModel{"x": fixtureModel{}}, MaxBodyBytes: -1},
		{Models: map[string]model.ChatModel{"x": fixtureModel{}}, ReplyTimeout: -1},
	} {
		if _, err := openaiserver.New(cfg); err == nil {
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
	models := map[string]model.ChatModel{"support": fixtureModel{}}
	h, err := openaiserver.New(openaiserver.Config{Models: models, MaxBodyBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	delete(models, "support")
	if w := request(h, basic); w.Code != 200 {
		t.Fatalf("map not copied: %d %s", w.Code, w.Body)
	}
	for _, spec := range []struct {
		method, path string
		status       int
	}{{"GET", "/v1/chat/completions", 405}, {"POST", "/v1/models", 405}, {"GET", "/v1/unknown", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(spec.method, spec.path, nil))
		if w.Code != spec.status {
			t.Fatalf("route: %d %s", w.Code, w.Body)
		}
	}
	w := request(h, strings.Repeat(" ", 101)+basic)
	if w.Code != 413 {
		t.Fatalf("body cap: %d %s", w.Code, w.Body)
	}
	w = request(h, basic+strings.Repeat(" ", 101))
	if w.Code != 413 {
		t.Fatalf("trailing body cap: %d %s", w.Code, w.Body)
	}
}

func TestForcedFinalizationPreservesNone(t *testing.T) {
	m := fixtureModel{chat: func(_ context.Context, _ []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		if o.ToolChoice != nil && o.ToolChoice.Mode == "none" {
			return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "final"}}}, nil
		}
		return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "loop", Name: "lookup", Input: "{}", State: message.ToolCallPending}}}, nil
	}}
	h := handler(t, m, func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		return r.NewAgent("support", openaiserver.AgentConfig{MaxIters: 1, Toolkit: tool.NewToolkit(tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { return "ok", nil }))}), nil
	})
	w := request(h, strings.TrimSuffix(basic, "}")+`,"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"final"`) {
		t.Fatalf("forced summary: %d %s", w.Code, w.Body)
	}
}

func TestFactoryOwnershipAndErrors(t *testing.T) {
	for name, factory := range map[string]func(*openaiserver.RequestInfo) (*agent.UnifiedAgent, error){
		"nil": func(*openaiserver.RequestInfo) (*agent.UnifiedAgent, error) { return nil, nil },
		"error": func(*openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			return nil, errors.New("secret internal credential")
		},
		"unowned": func(*openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent("shared", "", fixtureModel{}), nil
		},
		"multiple": func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			r.NewAgent("first", openaiserver.AgentConfig{})
			return r.NewAgent("second", openaiserver.AgentConfig{}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := request(handler(t, fixtureModel{}, factory), basic)
			if w.Code != 500 || strings.Contains(w.Body.String(), "secret internal credential") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	var modelCalls atomic.Int32
	m := fixtureModel{chat: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
		if modelCalls.Add(1) == 2 {
			for _, msg := range msgs {
				if text := msg.GetTextContent(""); text != nil && strings.Contains(*text, "private other request") {
					t.Error("request state leaked")
				}
			}
		}
		return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}}}, nil
	}}
	h := handler(t, m, func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		return r.NewAgent("private", openaiserver.AgentConfig{}), nil
	})
	for _, body := range []string{strings.Replace(basic, `"hi"`, `"private other request"`, 1), basic} {
		if w := request(h, body); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if modelCalls.Load() != 2 {
		t.Fatalf("calls=%d", modelCalls.Load())
	}
}

func TestFallbackCannotBypassToolPolicy(t *testing.T) {
	for _, buffered := range []bool{true, false} {
		t.Run(map[bool]string{true: "buffered", false: "streamed"}[buffered], func(t *testing.T) {
			var fallbackCalls, executions atomic.Int32
			fallback := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				fallbackCalls.Add(1)
				return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "bad", Name: "lookup", Input: "{}", State: message.ToolCallPending}}}, nil
			}}
			primary := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				return nil, errors.New("permanent upstream failure")
			}}
			factory := func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
				r.ModelConfig = agent.ModelConfig{MaxRetries: 1, FallbackModel: fallback}
				return r.NewAgent("support", openaiserver.AgentConfig{BufferedModel: buffered, Toolkit: tool.NewToolkit(tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { executions.Add(1); return "bad", nil }))}), nil
			}
			w := request(handler(t, primary, factory), strings.TrimSuffix(basic, "}")+`,"tools":[{"type":"function","function":{"name":"unknown"}}]}`)
			if w.Code != 400 || fallbackCalls.Load() != 0 {
				t.Fatalf("unknown tool reached fallback: %d %s calls=%d", w.Code, w.Body, fallbackCalls.Load())
			}
			w = request(handler(t, primary, factory), strings.TrimSuffix(basic, "}")+`,"tool_choice":"none"}`)
			if w.Code != 502 || fallbackCalls.Load() != 1 || executions.Load() != 0 {
				t.Fatalf("fallback bypass: %d %s calls=%d executions=%d", w.Code, w.Body, fallbackCalls.Load(), executions.Load())
			}
			// A separate request without a configured fallback cannot reuse it.
			override := func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
				return r.NewAgent("support", openaiserver.AgentConfig{BufferedModel: buffered}), nil
			}
			w = request(handler(t, primary, override), basic)
			if w.Code != 502 || fallbackCalls.Load() != 1 {
				t.Fatalf("raw fallback installed: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestBufferedPartialAndRetryableFailures(t *testing.T) {
	for _, spec := range []struct {
		err  error
		code int
	}{{errors.New("permanent secret provider failure"), 502}, {errors.New("503 temporarily unavailable"), 503}} {
		m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
			return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "incomplete"}}, Error: spec.err}, nil
		}}
		w := request(handler(t, m, func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			r.ModelConfig.MaxRetries = 1
			return r.NewAgent("support", openaiserver.AgentConfig{BufferedModel: true}), nil
		}), basic)
		if w.Code != spec.code || strings.Contains(w.Body.String(), "secret provider") || strings.Contains(w.Body.String(), "incomplete") {
			t.Fatalf("partial became success/leaked: %d %s", w.Code, w.Body)
		}
	}
}

func TestFinalCallMissingAndZeroUsage(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{true: "zero", false: "absent"}[known], func(t *testing.T) {
			m := fixtureModel{chat: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
				for _, msg := range msgs {
					for _, b := range msg.Content {
						if _, ok := b.(message.ToolResultBlock); ok {
							var u *model.ChatUsage
							if known {
								u = &model.ChatUsage{}
							}
							return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, Usage: u}, nil
						}
					}
				}
				return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "a", Name: "lookup", Input: "{}", State: message.ToolCallPending}}, Usage: &model.ChatUsage{InputTokens: 99, OutputTokens: 2}}, nil
			}}
			factory := func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
				return r.NewAgent("support", openaiserver.AgentConfig{Toolkit: tool.NewToolkit(tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { return "ok", nil }))}), nil
			}
			w := request(handler(t, m, factory), basic)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if known && !strings.Contains(w.Body.String(), `"prompt_tokens":0`) {
				t.Fatalf("stale prompt count: %s", w.Body)
			}
			if !known && strings.Contains(w.Body.String(), `"usage"`) {
				t.Fatalf("unknown final usage: %s", w.Body)
			}
		})
	}
}

func TestTimeoutHoldsCapacityThroughToolCleanup(t *testing.T) {
	started, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "slow", Name: "slow", Input: "{}", State: message.ToolCallPending}}}, nil
	}}
	h, err := openaiserver.New(openaiserver.Config{Models: map[string]model.ChatModel{"support": m}, MaxConcurrent: 1, ReplyTimeout: 100 * time.Millisecond, AgentFactory: func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		return r.NewAgent("support", openaiserver.AgentConfig{Toolkit: tool.NewToolkit(tool.NewFunctionTool("slow", "slow", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ map[string]any) (any, error) {
			close(started)
			<-ctx.Done()
			close(cleaning)
			<-release
			return nil, ctx.Err()
		}))}), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(h, basic) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool not started")
	}
	select {
	case <-cleaning:
	case <-time.After(time.Second):
		t.Fatal("timeout did not cancel tool")
	}
	w := request(h, basic)
	if w.Code != 429 {
		t.Fatalf("released capacity before cleanup: %d %s", w.Code, w.Body)
	}
	select {
	case <-done:
		t.Fatal("handler returned before tool cleanup")
	default:
	}
	// The deferred close releases the tool after assertions; wait outside
	// this test in cleanup so a failure cannot strand the goroutine.
	t.Cleanup(func() {
		select {
		case result := <-done:
			if result.Code != 504 {
				t.Errorf("timeout: %d %s", result.Code, result.Body)
			}
		case <-time.After(time.Second):
			t.Error("cleanup did not finish")
		}
	})
}

func TestStreamFailureHasNoSuccessfulTerminal(t *testing.T) {
	m := fixtureModel{stream: func(context.Context, []*message.Msg, model.CallOptions) (<-chan model.ChatResponse, error) {
		ch := make(chan model.ChatResponse, 2)
		ch <- model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}}
		ch <- model.ChatResponse{Error: errors.New("provider failed")}
		close(ch)
		return ch, nil
	}}
	w := request(handler(t, m, nil), strings.TrimSuffix(basic, "}")+`,"stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), `"finish_reason":"stop"`) || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestOversizeConversationFailsWithoutHiddenCompression(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		for _, responseError := range []bool{false, true} {
			t.Run(fmt.Sprintf("buffered=%v/responseError=%v", buffered, responseError), func(t *testing.T) {
				var calls atomic.Int32
				m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
					calls.Add(1)
					err := errors.New("maximum context length exceeded")
					if responseError {
						return &model.ChatResponse{IsLast: true, Error: err}, nil
					}
					return nil, err
				}}
				w := request(handler(t, m, func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
					r.ModelConfig.MaxRetries = 1
					return r.NewAgent("support", openaiserver.AgentConfig{BufferedModel: buffered}), nil
				}), basic)
				if w.Code != 400 || calls.Load() != 1 {
					t.Fatalf("implicit recovery/compaction: %d %s calls=%d", w.Code, w.Body, calls.Load())
				}
			})
		}
	}
}

func TestInvalidAgentConstructionConfig(t *testing.T) {
	var nilFallback *fixtureModel
	for name, factory := range map[string]func(*openaiserver.RequestInfo) (*agent.UnifiedAgent, error){
		"nil fallback": func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			r.ModelConfig.FallbackModel = nilFallback
			return r.NewAgent("support", openaiserver.AgentConfig{}), nil
		},
		"negative iterations": func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			return r.NewAgent("support", openaiserver.AgentConfig{MaxIters: -1}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := request(handler(t, fixtureModel{}, factory), basic)
			if w.Code != 500 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestFactoryDeadlineReturnsTimeout(t *testing.T) {
	h, err := openaiserver.New(openaiserver.Config{Models: map[string]model.ChatModel{"support": fixtureModel{}}, ReplyTimeout: 10 * time.Millisecond, AgentFactory: func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		<-r.Request.Context().Done()
		return nil, r.Request.Context().Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := request(h, basic)
	if w.Code != 504 || !strings.Contains(w.Body.String(), `"reply_timeout"`) {
		t.Fatalf("factory deadline: %d %s", w.Code, w.Body)
	}
}

func TestOpenAIAdapterAsClient(t *testing.T) {
	srv := httptest.NewServer(handler(t, fixtureModel{}, nil))
	defer srv.Close()
	client, err := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "local-fixture", BaseURL: srv.URL, Model: "support"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []*message.Msg{message.UserMsg("user", "hello")}
	resp, err := client.Chat(context.Background(), msgs)
	if err != nil || resp.GetTextContent() != "hello" {
		t.Fatalf("buffered client: %+v %v", resp, err)
	}
	stream, err := client.ChatStream(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	var final *model.ChatResponse
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatal(chunk.Error)
		}
		if chunk.IsLast {
			final = &chunk
		}
	}
	if final == nil || final.GetTextContent() != "hello" {
		t.Fatalf("streamed client: %+v", final)
	}
}

// Move the real turn_stats after ReplyEnd, preserving all internal round
// boundaries, to exercise the permitted middleware ordering.
type trailingStatsMiddleware struct {
	middleware.BaseMiddleware
	sessionID *string
}

func (m *trailingStatsMiddleware) OnReply(ctx context.Context, input middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	in := next(ctx, input)
	out := make(chan event.Event)
	go func() {
		defer close(out)
		var stats event.Event
		for ev := range in {
			if start, ok := ev.(event.ReplyStartEvent); ok {
				*m.sessionID = start.SessionID
			}
			if custom, ok := ev.(event.CustomEvent); ok && custom.Name == "turn_stats" {
				stats = ev
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		if stats != nil {
			select {
			case out <- stats:
			case <-ctx.Done():
			}
		}
	}()
	return out
}

func TestUsageAfterReplyEndAndUniqueRequestIdentity(t *testing.T) {
	var previous string
	for range 2 {
		var session string
		m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
			return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}}, Usage: &model.ChatUsage{InputTokens: 12, OutputTokens: 4}}, nil
		}}
		h := handler(t, m, func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
			return r.NewAgent("support", openaiserver.AgentConfig{Middlewares: []middleware.Middleware{&trailingStatsMiddleware{sessionID: &session}}}), nil
		})
		w := request(h, strings.TrimSuffix(basic, "}")+`,"stream":true,"stream_options":{"include_usage":true}}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"completion_tokens":4`) {
			t.Fatalf("late stats: %d %s", w.Code, w.Body)
		}
		if session == "" || session == previous {
			t.Fatalf("request identity leaked/reused: %q", session)
		}
		previous = session
	}
}
