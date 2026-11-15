package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aserrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// soFakeModel is a scripted ChatModel for structured-output ladder tests.
type soFakeModel struct {
	calls   []CallOptions  // recorded options per Chat call
	script  []soScriptStep // one step per Chat call
	callIdx int
}

type soScriptStep struct {
	err  error         // return this error (if non-nil)
	resp *ChatResponse // else return this response
}

func (m *soFakeModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	var o CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	m.calls = append(m.calls, o)
	step := m.script[m.callIdx]
	m.callIdx++
	if step.err != nil {
		return nil, step.err
	}
	return step.resp, nil
}

func (m *soFakeModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	return nil, fmt.Errorf("not supported")
}

func (m *soFakeModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int { return 1 }
func (m *soFakeModel) ContextSize() int                                        { return 1000 }

// DisableThinkingOptions implements ThinkingDisabler.
func (m *soFakeModel) DisableThinkingOptions() []CallOption {
	return []CallOption{WithThinkingDisabled()}
}

func soToolCallResp(input string) *ChatResponse {
	return &ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: "tc1", Name: structuredOutputToolName,
			Input: input, State: message.ToolCallPending,
		}},
		IsLast: true,
	}
}

var soSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)

func TestStructuredOutput_FirstStrategySucceeds(t *testing.T) {
	m := &soFakeModel{script: []soScriptStep{{resp: soToolCallResp(`{"answer":"42"}`)}}}
	out, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"answer":"42"}` {
		t.Errorf("out = %s", out)
	}
	if len(m.calls) != 1 {
		t.Fatalf("expected 1 call (forced strategy), got %d", len(m.calls))
	}
	if m.calls[0].ToolChoice == nil || m.calls[0].ToolChoice.Mode != "required" {
		t.Errorf("first strategy must use forced tool_choice, got %+v", m.calls[0].ToolChoice)
	}
}

func TestStructuredOutput_LadderFallsThroughToNoThink(t *testing.T) {
	// forced → provider rejects (tool_choice error); auto → model produces no
	// tool call (StructuredOutputError class); no_think → succeeds with
	// thinking disabled.
	m := &soFakeModel{script: []soScriptStep{
		{err: fmt.Errorf("400 bad request: tool_choice cannot be used with thinking")},
		{resp: &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "no tool call"}}, IsLast: true}},
		{resp: soToolCallResp(`{"answer":"ok"}`)},
	}}
	out, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"answer":"ok"}` {
		t.Errorf("out = %s", out)
	}
	if len(m.calls) != 3 {
		t.Fatalf("expected 3 ladder calls, got %d", len(m.calls))
	}
	if m.calls[1].ToolChoice == nil || m.calls[1].ToolChoice.Mode != "auto" {
		t.Errorf("second strategy must use auto tool_choice, got %+v", m.calls[1].ToolChoice)
	}
	third := m.calls[2]
	if third.ToolChoice == nil || third.ToolChoice.Mode != "required" {
		t.Errorf("no_think strategy must re-force tool_choice, got %+v", third.ToolChoice)
	}
	if third.ThinkingEnable == nil || *third.ThinkingEnable != false {
		t.Errorf("no_think strategy must disable thinking, got %+v", third.ThinkingEnable)
	}
}

func TestStructuredOutput_NoneStrategyParsesTextJSON(t *testing.T) {
	// Every tool-based strategy fails; the none strategy succeeds by parsing
	// JSON out of plain text.
	m := &soFakeModel{script: []soScriptStep{
		{err: fmt.Errorf("tool_choice unsupported")},
		{err: fmt.Errorf("tool_choice unsupported")},
		{err: fmt.Errorf("tool_choice unsupported")},
		{resp: &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: `{"answer":"text-json"}`}}, IsLast: true}},
	}}
	out, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"answer":"text-json"}` {
		t.Errorf("out = %s", out)
	}
	if last := m.calls[len(m.calls)-1]; last.ToolChoice != nil {
		t.Errorf("none strategy must not set tool_choice, got %+v", last.ToolChoice)
	}
}

func TestStructuredOutput_AllStrategiesFailTypedError(t *testing.T) {
	m := &soFakeModel{script: []soScriptStep{
		{err: fmt.Errorf("tool_choice rejected")},
		{err: fmt.Errorf("tool_choice rejected")},
		{err: fmt.Errorf("tool_choice rejected")},
		{err: fmt.Errorf("thinking rejected")},
	}}
	_, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema)
	if err == nil {
		t.Fatal("expected error when every strategy fails")
	}
	if !errors.Is(err, aserrors.ErrStructuredOutput) {
		t.Errorf("error should wrap ErrStructuredOutput, got: %v", err)
	}
}

func TestStructuredOutput_NonFallbackErrorStopsLadder(t *testing.T) {
	// A non-fallback error (e.g. auth) must not walk the ladder.
	m := &soFakeModel{script: []soScriptStep{
		{err: fmt.Errorf("401 invalid api key")},
	}}
	_, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(m.calls) != 1 {
		t.Errorf("non-fallback error must stop the ladder, got %d calls", len(m.calls))
	}
}

func TestDashScope_DisableThinkingSendsEnableThinkingFalse(t *testing.T) {
	// Upstream #2140: providers with a thinking toggle send
	// enable_thinking=false when the caller disables thinking.
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	m, err := NewDashScopeChatModel(DashScopeConfig{APIKey: "k", Model: "qwen3-max", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewDashScopeChatModel: %v", err)
	}
	var cm ChatModel = m
	if _, ok := cm.(ThinkingDisabler); !ok {
		t.Fatal("DashScopeChatModel must implement ThinkingDisabler")
	}
	_, err = m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")}, WithThinkingDisabled())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(string(captured), `"enable_thinking":false`) {
		t.Errorf("request body missing enable_thinking=false: %s", captured)
	}
}

func TestDeepSeekMoonshot_DisableThinkingSendsThinkingDisabled(t *testing.T) {
	// Upstream #2140: DeepSeek/Moonshot disable thinking via
	// {"thinking":{"type":"disabled"}} — NOT enable_thinking.
	cases := []struct {
		name string
		ctor func(baseURL string) (ChatModel, error)
	}{
		{"DeepSeek", func(u string) (ChatModel, error) {
			return NewDeepSeekChatModel(DeepSeekConfig{APIKey: "k", Model: "deepseek-chat", BaseURL: u})
		}},
		{"Moonshot", func(u string) (ChatModel, error) {
			return NewMoonshotChatModel(MoonshotConfig{APIKey: "k", Model: "kimi-k2", BaseURL: u})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			m, err := tc.ctor(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := m.(ThinkingDisabler); !ok {
				t.Fatalf("%s must implement ThinkingDisabler", tc.name)
			}
			if _, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")}, WithThinkingDisabled()); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if !strings.Contains(string(captured), `"thinking":{"type":"disabled"}`) {
				t.Errorf("request body missing thinking.type=disabled: %s", captured)
			}
			if strings.Contains(string(captured), "enable_thinking") {
				t.Errorf("request must not use enable_thinking on %s: %s", tc.name, captured)
			}
		})
	}
}

func TestAnthropic_DisableThinkingSendsThinkingDisabled(t *testing.T) {
	// Upstream #2140: Anthropic disables thinking via {"type":"disabled"}.
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	m, err := NewAnthropicChatModel(&AnthropicConfig{APIKey: "k", Model: "claude-sonnet-4-5", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var cm ChatModel = m
	if _, ok := cm.(ThinkingDisabler); !ok {
		t.Fatal("AnthropicChatModel must implement ThinkingDisabler")
	}
	if _, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")}, WithThinkingDisabled()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(string(captured), `"thinking":{"type":"disabled"}`) {
		t.Errorf("request body missing thinking.type=disabled: %s", captured)
	}
	if strings.Contains(string(captured), "budget_tokens") {
		t.Errorf("disabled thinking must not send budget_tokens: %s", captured)
	}
}

type soClassifiedModel struct {
	soFakeModel
	classify func(error) bool
}

func (m *soClassifiedModel) IsStructuredOutputFallbackError(err error) bool {
	return m.classify(err)
}

func TestStructuredOutput_ProviderClassifierTakesPrecedence(t *testing.T) {
	// Upstream #2140 parity: a provider-declared fallback classifier
	// replaces the marker heuristic entirely.
	t.Run("classifier-advances-without-markers", func(t *testing.T) {
		m := &soClassifiedModel{
			soFakeModel: soFakeModel{script: []soScriptStep{
				{err: fmt.Errorf("provider rejected request shape")},
				{resp: soToolCallResp(`{"answer":"ok"}`)},
			}},
			classify: func(error) bool { return true },
		}
		if _, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(m.calls) != 2 {
			t.Errorf("classifier should advance the ladder, got %d calls", len(m.calls))
		}
	})
	t.Run("classifier-stops-despite-markers", func(t *testing.T) {
		m := &soClassifiedModel{
			soFakeModel: soFakeModel{script: []soScriptStep{
				{err: fmt.Errorf("tool_choice mentioned but this is terminal")},
			}},
			classify: func(error) bool { return false },
		}
		_, err := GenerateStructuredOutput(context.Background(), m, nil, soSchema)
		if err == nil {
			t.Fatal("expected error")
		}
		if len(m.calls) != 1 {
			t.Errorf("classifier=false must stop the ladder despite markers, got %d calls", len(m.calls))
		}
	})
}
