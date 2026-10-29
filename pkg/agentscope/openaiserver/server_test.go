package openaiserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/openaiserver"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type fixtureModel struct {
	chat   func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error)
	stream func(context.Context, []*message.Msg, model.CallOptions) (<-chan model.ChatResponse, error)
}

func (m fixtureModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	if m.chat != nil {
		return m.chat(ctx, msgs, o)
	}
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hello"}}, IsLast: true}, nil
}
func (m fixtureModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	if m.stream != nil {
		var o model.CallOptions
		for _, opt := range opts {
			opt(&o)
		}
		return m.stream(ctx, msgs, o)
	}
	r, err := m.Chat(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.ChatResponse, 1)
	ch <- *r
	close(ch)
	return ch, nil
}
func (fixtureModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func handler(t *testing.T, m model.ChatModel, factory func(*openaiserver.RequestInfo) (*agent.UnifiedAgent, error)) http.Handler {
	t.Helper()
	h, err := openaiserver.New(openaiserver.Config{Models: map[string]model.ChatModel{"support": m}, AgentFactory: factory, ReplyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func request(h http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return w
}

const basic = `{"model":"support","messages":[{"role":"user","content":"hi"}]}`

func TestOfficialSDKRequestFixtures(t *testing.T) {
	for _, name := range []string{"sdk-buffered.json", "sdk-streamed.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
				return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "SDK fixture answer"}}, Usage: &model.ChatUsage{InputTokens: 31, OutputTokens: 4}}, nil
			}}
			w := request(handler(t, m, nil), string(body))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "SDK fixture answer") || !strings.Contains(w.Body.String(), `"prompt_tokens":31`) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if name == "sdk-streamed.json" && (!strings.Contains(w.Body.String(), `"choices":[]`) || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n")) {
				t.Fatalf("SSE: %s", w.Body)
			}
		})
	}
}

func TestCompletionAndModels(t *testing.T) {
	h := handler(t, fixtureModel{}, nil)
	w := request(h, basic)
	var response struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Role, Content string }
			Finish  string `json:"finish_reason"`
		}
		Usage *struct{} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Object != "chat.completion" || response.Model != "support" || !strings.HasPrefix(response.ID, "chatcmpl-") || len(response.Choices) != 1 {
		t.Fatalf("response: %d %s", w.Code, w.Body)
	}
	if response.Choices[0].Message.Content != "hello" || response.Choices[0].Message.Role != "assistant" || response.Choices[0].Finish != "stop" || response.Usage != nil {
		t.Fatalf("response: %s", w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	var models struct {
		Object string
		Data   []struct{ ID, Object string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || models.Object != "list" || len(models.Data) != 1 || models.Data[0].ID != "support" || models.Data[0].Object != "model" {
		t.Fatalf("models: %d %s", w.Code, w.Body)
	}
}

func TestHydrationCompletedToolHistoryAndCallOptions(t *testing.T) {
	m := fixtureModel{chat: func(_ context.Context, msgs []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		if len(msgs) != 6 || msgs[0].Role != message.RoleSystem || *msgs[0].GetTextContent("") != "Be helpful" || *msgs[5].GetTextContent("") != "next" {
			t.Errorf("hydration: %+v", msgs)
		}
		call := msgs[2].Content[0].(message.ToolCallBlock)
		result := msgs[3].Content[0].(message.ToolResultBlock)
		if call.ID != "past" || call.State != message.ToolCallFinished || result.ID != call.ID || result.Name != "lookup" || result.GetOutputText() != "delivered" {
			t.Errorf("history: %+v %+v", call, result)
		}
		if o.Temperature == nil || *o.Temperature != 0.2 || o.TopP == nil || *o.TopP != 0.9 || o.MaxTokens == nil || *o.MaxTokens != 100 {
			t.Errorf("options: %+v", o)
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}}, IsLast: true, Usage: &model.ChatUsage{InputTokens: 31, OutputTokens: 4}}, nil
	}}
	w := request(handler(t, m, nil), `{"model":"support","messages":[{"role":"system","content":"Be helpful"},{"role":"user","content":"where?"},{"role":"assistant","content":null,"tool_calls":[{"id":"past","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"past","content":"delivered"},{"role":"assistant","content":"done"},{"role":"user","content":[{"type":"text","text":"next"}]}],"temperature":0.2,"top_p":0.9,"max_tokens":100}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"prompt_tokens":31`) || !strings.Contains(w.Body.String(), `"completion_tokens":4`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestValidation(t *testing.T) {
	var calls atomic.Int32
	h := handler(t, fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		calls.Add(1)
		return nil, fmt.Errorf("must not call")
	}}, nil)
	for _, body := range []string{
		`null`, `{}`, basic + `{}`, `{"model":"support","messages":[]}`,
		`{"model":"support","messages":[{"role":"assistant","content":"hi"}]}`,
		`{"model":"support","messages":[{"role":"user","content":""}]}`,
		`{"model":"support","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		`{"model":"support","messages":[{"role":"tool","tool_call_id":"orphan","content":"x"},{"role":"user","content":"hi"}]}`,
		`{"model":"support","messages":[{"role":"assistant","tool_calls":[{"id":"pending","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"user","content":"hi"}]}`,
		strings.TrimSuffix(basic, "}") + `,"tool_choice":"required"}`,
		strings.TrimSuffix(basic, "}") + `,"response_format":{"type":"json_object"}}`,
		strings.TrimSuffix(basic, "}") + `,"seed":42}`,
		strings.TrimSuffix(basic, "}") + `,"n":2}`,
		strings.TrimSuffix(basic, "}") + `,"logprobs":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := request(h, body)
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_request_error"`) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	w := request(h, strings.Replace(basic, "support", "missing", 1))
	if w.Code != 404 {
		t.Fatalf("unknown model: %d", w.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid requests reached provider")
	}
}

func TestToolsExecuteOnServerAndUsageAggregates(t *testing.T) {
	var executed atomic.Int32
	m := fixtureModel{chat: func(_ context.Context, msgs []*message.Msg, o model.CallOptions) (*model.ChatResponse, error) {
		if len(o.Tools) != 1 || o.Tools[0].Function.Name != "lookup" {
			t.Errorf("tools: %+v", o.Tools)
		}
		for _, msg := range msgs {
			for _, b := range msg.Content {
				if _, ok := b.(message.ToolResultBlock); ok {
					return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "delivered"}}, IsLast: true, Usage: &model.ChatUsage{InputTokens: 20, OutputTokens: 3}}, nil
				}
			}
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ThinkingBlock{Type: "thinking", Thinking: "private reasoning"}, message.ToolCallBlock{Type: "tool_call", ID: "lookup-now", Name: "lookup", Input: "{}", State: message.ToolCallPending}}, IsLast: true, Usage: &model.ChatUsage{InputTokens: 10, OutputTokens: 2}}, nil
	}}
	factory := func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		tk := tool.NewToolkit(tool.NewFunctionTool("lookup", "Lookup order", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { executed.Add(1); return "delivered", nil }))
		return r.NewAgent("support", openaiserver.AgentConfig{Toolkit: tk}), nil
	}
	w := request(handler(t, m, factory), strings.TrimSuffix(basic, "}")+`,"stream":true,"stream_options":{"include_usage":true},"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup order","parameters":{"type":"object"}}}]}`)
	if w.Code != 200 || executed.Load() != 1 || !strings.Contains(w.Body.String(), `"content":"delivered"`) || !strings.Contains(w.Body.String(), `"prompt_tokens":20`) || !strings.Contains(w.Body.String(), `"completion_tokens":5`) || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("%d %s executions=%d", w.Code, w.Body, executed.Load())
	}
	if strings.Contains(w.Body.String(), "tool_calls") || strings.Contains(w.Body.String(), "private reasoning") {
		t.Fatalf("leaked private events: %s", w.Body)
	}
}

func TestUnknownToolsAndNoneCannotExecute(t *testing.T) {
	var executions atomic.Int32
	factory := func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		return r.NewAgent("support", openaiserver.AgentConfig{Toolkit: tool.NewToolkit(tool.NewFunctionTool("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { executions.Add(1); return "bad", nil }))}), nil
	}
	var providerCalls atomic.Int32
	m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		providerCalls.Add(1)
		return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "bad", Name: "lookup", Input: "{}"}}}, nil
	}}
	w := request(handler(t, m, factory), strings.TrimSuffix(basic, "}")+`,"tools":[{"type":"function","function":{"name":"unknown","parameters":{"type":"object"}}}]}`)
	if w.Code != 400 || providerCalls.Load() != 0 {
		t.Fatalf("unknown tools: %d %s calls=%d", w.Code, w.Body, providerCalls.Load())
	}
	w = request(handler(t, m, factory), strings.TrimSuffix(basic, "}")+`,"tool_choice":"none"}`)
	if w.Code != 502 || executions.Load() != 0 {
		t.Fatalf("none: %d %s executions=%d", w.Code, w.Body, executions.Load())
	}
}

func TestConcurrencyLimitAndClientDisconnect(t *testing.T) {
	started, canceled := make(chan struct{}, 1), make(chan struct{}, 1)
	m := fixtureModel{stream: func(ctx context.Context, _ []*message.Msg, _ model.CallOptions) (<-chan model.ChatResponse, error) {
		ch := make(chan model.ChatResponse)
		go func() {
			defer close(ch)
			started <- struct{}{}
			select {
			case ch <- model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}}:
			case <-ctx.Done():
			}
			<-ctx.Done()
			canceled <- struct{}{}
		}()
		return ch, nil
	}}
	h, err := openaiserver.New(openaiserver.Config{Models: map[string]model.ChatModel{"support": m}, MaxConcurrent: 1, ReplyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(strings.TrimSuffix(basic, "}")+`,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	w := request(h, basic)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("limit: %d %s", w.Code, w.Body)
	}
	resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel model")
	}
}

func TestPermissionAskFailsWithoutExecuting(t *testing.T) {
	var executions atomic.Int32
	m := fixtureModel{chat: func(context.Context, []*message.Msg, model.CallOptions) (*model.ChatResponse, error) {
		return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "ask", Name: "write", Input: "{}", State: message.ToolCallPending}}}, nil
	}}
	h := handler(t, m, func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
		return r.NewAgent("support", openaiserver.AgentConfig{PermissionContext: permission.NewContext(permission.ModeDefault), Toolkit: tool.NewToolkit(tool.NewFunctionTool("write", "write", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { executions.Add(1); return "bad", nil }))}), nil
	})
	w := request(h, basic)
	if w.Code != 400 || executions.Load() != 0 {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
}

func TestIncrementalSSE(t *testing.T) {
	continueStream := make(chan struct{})
	defer close(continueStream)
	m := fixtureModel{stream: func(ctx context.Context, _ []*message.Msg, _ model.CallOptions) (<-chan model.ChatResponse, error) {
		ch := make(chan model.ChatResponse)
		go func() {
			defer close(ch)
			select {
			case ch <- model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first"}}}:
			case <-ctx.Done():
				return
			}
			select {
			case <-continueStream:
			case <-ctx.Done():
				return
			}
		}()
		return ch, nil
	}}
	srv := httptest.NewServer(handler(t, m, nil))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(strings.TrimSuffix(basic, "}")+`,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 4096)
	n, err := resp.Body.Read(b)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n == 0 || !strings.Contains(string(b[:n]), "chat.completion.chunk") {
		t.Fatalf("not incremental: %s", b[:n])
	}
}
