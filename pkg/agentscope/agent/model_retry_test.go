package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// scriptedStream plays scripted stream outcomes in order and counts calls.
type scriptedStream struct {
	calls atomic.Int32
	steps []streamStep
}

type streamStep struct {
	err    error
	chunks []model.ChatResponse
	// close the stream immediately without any chunk (simulates a network
	// cut right after the response headers)
	cutBeforeDeltas bool
}

func (m *scriptedStream) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	return nil, errors.New("unexpected Chat call")
}

func (m *scriptedStream) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	i := int(m.calls.Add(1)) - 1
	if i >= len(m.steps) {
		i = len(m.steps) - 1
	}
	step := m.steps[i]
	if step.err != nil {
		return nil, step.err
	}
	if step.cutBeforeDeltas {
		ch := make(chan model.ChatResponse)
		close(ch)
		return ch, nil
	}
	return fixtureStream(step.chunks...)
}

func (m *scriptedStream) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func textFinalChunk(text string) model.ChatResponse {
	return model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 5, OutputTokens: 1},
	}
}

// Transient setup failures (429/5xx) are retried within the same reply.
func TestStreamingSetupRetryOnTransientError(t *testing.T) {
	m := &scriptedStream{steps: []streamStep{
		{err: errors.New("openai: httpx: unexpected status 503: service unavailable")},
		{err: errors.New("openai: httpx: unexpected status 429: rate limit")},
		{chunks: []model.ChatResponse{textFinalChunk("ok")}},
	}}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 5, RetryDelay: time.Millisecond}))
	_, text, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyCompleted || text != "ok" {
		t.Fatalf("end=%+v text=%q", end, text)
	}
	if m.calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", m.calls.Load())
	}
}

// Permanent setup failures (400/401) fail fast: no retry, no wasted calls.
func TestStreamingSetupNoRetryOnPermanentError(t *testing.T) {
	m := &scriptedStream{steps: []streamStep{
		{err: errors.New("openai: httpx: unexpected status 400: invalid request")},
	}}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 5, RetryDelay: time.Millisecond}))
	_, _, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end=%+v", end)
	}
	if m.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (permanent errors fail fast)", m.calls.Load())
	}
}

// A stream that dies before publishing anything is replayed invisibly.
func TestStreamingEstablishedRetryBeforeDeltas(t *testing.T) {
	m := &scriptedStream{steps: []streamStep{
		{cutBeforeDeltas: true},
		{chunks: []model.ChatResponse{textFinalChunk("recovered")}},
	}}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Millisecond}))
	_, text, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyCompleted || text != "recovered" {
		t.Fatalf("end=%+v text=%q", end, text)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", m.calls.Load())
	}
}

// A stream that dies after publishing deltas is not replayed: consumers
// already rendered the partial output.
func TestStreamingEstablishedNoRetryAfterDeltas(t *testing.T) {
	var calls atomic.Int32
	m := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
		calls.Add(1)
		return fixtureStream(textChunk("visible", false)) // closes without final
	}}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Millisecond}))
	_, text, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end=%+v", end)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (published deltas cannot be replayed)", calls.Load())
	}
	if text != "visible" {
		t.Fatalf("published text = %q", text)
	}
}

// Context-window rejections are not retried inside the model call; the reply
// loop recovers. With a persistent overflow the reply still fails after the
// bounded number of recoveries.
func TestStreamingContextLimitHandledByLoopRecovery(t *testing.T) {
	overflow := errors.New(`openai: httpx: unexpected status 400: {"error":{"code":"context_length_exceeded"}}`)
	m := &scriptedStream{steps: []streamStep{{err: overflow}}}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 5, RetryDelay: time.Millisecond}))
	_, _, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end=%+v", end)
	}
	// 1 initial + maxContextOverflowRecoveries recovery retries; the model
	// call layer itself must not retry (that would show up as a multiple).
	want := int32(1 + maxContextOverflowRecoveries)
	if m.calls.Load() != want {
		t.Fatalf("calls = %d, want %d", m.calls.Load(), want)
	}
}

// Default config (no WithModelConfig) retries transient failures.
func TestStreamingDefaultAttemptsRetryTransient(t *testing.T) {
	m := &scriptedStream{steps: []streamStep{
		{err: errors.New("anthropic: httpx: unexpected status 529: overloaded")},
		{chunks: []model.ChatResponse{textFinalChunk("ok")}},
	}}
	a := NewUnifiedAgent("bot", "test", m)
	_, text, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyCompleted || text != "ok" {
		t.Fatalf("end=%+v text=%q", end, text)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", m.calls.Load())
	}
}

// Buffered path: permanent errors fail fast, transient errors retry.
func TestCallModelRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantCalls int32
	}{
		{"transient 503", errors.New("httpx: unexpected status 503: overloaded"), 3},
		{"permanent 400", errors.New("httpx: unexpected status 400: bad request"), 1},
		{"context limit", errors.New("httpx: unexpected status 400: prompt is too long: 9 > 1 maximum"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			m := &streamContractModel{chat: func(context.Context) (*model.ChatResponse, error) {
				calls.Add(1)
				return nil, tc.err
			}}
			a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Millisecond}))
			_, err := a.callModel(context.Background(), nil, nil)
			if err == nil {
				t.Fatal("want error")
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
		})
	}
}

// Broken streamed tool arguments (truncated JSON) are kept in history and
// answered with a parse-error tool result, so the model can re-issue the
// call, instead of killing the reply (codex parity).
func TestStreamingBrokenToolArgsFeedBack(t *testing.T) {
	tk := tool.NewToolkit(&echoTool{output: "should not run"})
	broken := model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: "tc1", Name: "echo", Input: `{"value":`, State: message.ToolCallPending,
		}},
		IsLast: true,
		Usage:  &model.ChatUsage{InputTokens: 5, OutputTokens: 1},
	}
	m := &scriptedStream{steps: []streamStep{
		{chunks: []model.ChatResponse{broken}},
		{chunks: []model.ChatResponse{textFinalChunk("reissued")}},
	}}
	a := NewUnifiedAgent("bot", "test", m, WithToolkit(tk))
	text, end, _ := collectReply(t, a, "go")
	if end.FinishedReason != types.ReplyCompleted || text != "reissued" {
		t.Fatalf("end=%+v text=%q", end, text)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (broken call + re-issue)", m.calls.Load())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var sawParseError bool
	for _, msg := range a.state.Context {
		for _, b := range msg.Content {
			tr, ok := b.(message.ToolResultBlock)
			if !ok || tr.ID != "tc1" {
				continue
			}
			if tr.State != message.ToolResultError {
				t.Fatalf("tool result state = %s, want error", tr.State)
			}
			if s, ok := tr.Output.(string); ok && strings.Contains(s, "JSON") {
				sawParseError = true
			}
		}
	}
	if !sawParseError {
		t.Fatal("no parse-error tool result recorded for the broken call")
	}
}

// A provider response that completed with an in-band error (partial content)
// is terminal: the provider answered as designed, so no retry happens. This
// regresses the evalkit laterTurnFailure scenario.
func TestStreamingPartialResponseNotRetried(t *testing.T) {
	var chats atomic.Int32
	m := &streamContractModel{
		chat: func(context.Context) (*model.ChatResponse, error) {
			chats.Add(1)
			return &model.ChatResponse{
				Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}},
				Error:   errors.New("truncated"),
			}, nil
		},
		stream: func(context.Context) (<-chan model.ChatResponse, error) {
			return nil, model.ErrStreamNotSupported
		},
	}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Millisecond}))
	_, _, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end=%+v", end)
	}
	if chats.Load() != 1 {
		t.Fatalf("chats = %d, want 1 (in-band partial response is terminal)", chats.Load())
	}
}
