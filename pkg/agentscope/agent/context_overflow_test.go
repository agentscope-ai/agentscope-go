package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// overflowMockModel streams scripted outcomes: each entry is either an error
// (stream setup fails, as a provider 400 does) or a set of chunks.
type overflowMockModel struct {
	calls atomic.Int32
	steps []overflowStep
}

type overflowStep struct {
	err    error
	chunks []model.ChatResponse
}

func (m *overflowMockModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	// Buffered Chat is used by the structured-summary compression path, not by
	// the (streaming) reply loop; answer with the summary tool call.
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type:  "tool_call",
			ID:    "tc_summary",
			Name:  "generate_structured_output",
			Input: `{"task_overview":"Earlier work","current_state":"In progress","important_discoveries":"None","next_steps":"Continue","context_to_preserve":"Nothing"}`,
			State: message.ToolCallPending,
		}},
		IsLast: true,
		Usage:  &model.ChatUsage{InputTokens: 5, OutputTokens: 5},
	}, nil
}

func (m *overflowMockModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	i := int(m.calls.Add(1)) - 1
	if i >= len(m.steps) {
		i = len(m.steps) - 1
	}
	step := m.steps[i]
	if step.err != nil {
		return nil, step.err
	}
	ch := make(chan model.ChatResponse, len(step.chunks))
	for i := range step.chunks {
		ch <- step.chunks[i]
	}
	close(ch)
	return ch, nil
}

func (m *overflowMockModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func overflowErr() error {
	return errors.New(`anthropic: httpx: unexpected status 400: {"error":{"message":"prompt is too long: 213504 tokens > 200000 maximum"}}`)
}

func finalTextStep(text string) overflowStep {
	return overflowStep{chunks: []model.ChatResponse{{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 10, OutputTokens: 2},
	}}}
}

func collectReply(t *testing.T, a *UnifiedAgent, input string) (string, event.ReplyEndEvent, []event.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var end event.ReplyEndEvent
	var events []event.Event
	for e := range ch {
		events = append(events, e)
		switch v := e.(type) {
		case event.TextBlockDeltaEvent:
			text.WriteString(v.Delta)
		case event.ReplyEndEvent:
			end = v
		}
	}
	return text.String(), end, events
}

// A provider context-window rejection must not kill the reply: the agent
// compacts and retries the same step.
func TestContextOverflowRecoveryCompactsAndRetries(t *testing.T) {
	m := &overflowMockModel{steps: []overflowStep{
		{err: overflowErr()},
		finalTextStep("done"),
	}}
	a := NewUnifiedAgent("bot", "test", m,
		WithContextConfig(&ContextConfig{ContextSize: 100000}),
	)
	text, end, events := collectReply(t, a, "hello")
	if end.FinishedReason != types.ReplyCompleted {
		t.Fatalf("end = %+v", end)
	}
	if text != "done" {
		t.Fatalf("text = %q", text)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2 (failure + retry)", m.calls.Load())
	}
	var sawRecovery bool
	for _, e := range events {
		if c, ok := e.(event.CustomEvent); ok && c.Name == "context_overflow_recovery" {
			sawRecovery = true
		}
	}
	if !sawRecovery {
		t.Fatal("no context_overflow_recovery event emitted")
	}
	a.mu.Lock()
	summary := a.state.Summary
	a.mu.Unlock()
	if summary == "" {
		t.Fatal("forced compaction should have produced a summary")
	}
}

// An oversized tool result is truncated in place (head+tail) before
// compaction is attempted.
func TestContextOverflowRecoveryTruncatesToolResults(t *testing.T) {
	big := strings.Repeat("x", 100*1024)
	tk := tool.NewToolkit(&echoTool{output: big})
	m := &overflowMockModel{steps: []overflowStep{
		{chunks: []model.ChatResponse{{
			Content: []message.ContentBlock{message.ToolCallBlock{
				Type: "tool_call", ID: "tc1", Name: "echo", Input: `{}`, State: message.ToolCallPending,
			}},
			IsLast: true,
			Usage:  &model.ChatUsage{InputTokens: 10, OutputTokens: 2},
		}}},
		{err: overflowErr()},
		finalTextStep("recovered"),
	}}
	a := NewUnifiedAgent("bot", "test", m,
		WithToolkit(tk),
		WithContextConfig(&ContextConfig{ContextSize: 4000}),
	)
	text, end, _ := collectReply(t, a, "go")
	if end.FinishedReason != types.ReplyCompleted || text != "recovered" {
		t.Fatalf("end=%+v text=%q", end, text)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var resultText string
	for _, msg := range a.state.Context {
		for _, b := range msg.Content {
			if tr, ok := b.(message.ToolResultBlock); ok {
				if s, ok := tr.Output.(string); ok {
					resultText = s
				}
			}
		}
	}
	if len(resultText) >= len(big) {
		t.Fatalf("tool result not truncated: %d bytes", len(resultText))
	}
	if !strings.Contains(resultText, "<<<TRUNCATED:") {
		t.Fatal("truncated tool result should carry the elision marker")
	}
	if a.state.Summary != "" {
		t.Fatal("truncation should succeed before compaction; summary should stay empty")
	}
}

// A persistent overflow is bounded: the reply fails after a fixed number of
// recovery attempts instead of looping forever.
func TestContextOverflowRecoveryBounded(t *testing.T) {
	m := &overflowMockModel{steps: []overflowStep{{err: overflowErr()}}}
	a := NewUnifiedAgent("bot", "test", m,
		WithContextConfig(&ContextConfig{ContextSize: 100000}),
	)
	_, end, _ := collectReply(t, a, "hello")
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end = %+v", end)
	}
	if got := m.calls.Load(); got > 8 {
		t.Fatalf("unbounded overflow retries: %d model calls", got)
	}
}

// Non-overflow model errors must not trigger recovery.
func TestNoRecoveryForOrdinaryModelError(t *testing.T) {
	m := &overflowMockModel{steps: []overflowStep{{err: errors.New("openai: unexpected status 401: invalid key")}}}
	a := NewUnifiedAgent("bot", "test", m,
		WithContextConfig(&ContextConfig{ContextSize: 100000}),
	)
	_, end, events := collectReply(t, a, "hello")
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end = %+v", end)
	}
	for _, e := range events {
		if c, ok := e.(event.CustomEvent); ok && c.Name == "context_overflow_recovery" {
			t.Fatal("recovery fired for a non-overflow error")
		}
	}
	if m.calls.Load() != 1 {
		t.Fatalf("model calls = %d, want 1 (no recovery retry)", m.calls.Load())
	}
}

type echoTool struct {
	tool.BaseTool
	output string
}

func (e *echoTool) Name() string { return "echo" }
func (e *echoTool) Execute(_ context.Context, _ map[string]any) (*tool.ToolResponse, error) {
	return tool.NewTextResponse(e.output), nil
}
