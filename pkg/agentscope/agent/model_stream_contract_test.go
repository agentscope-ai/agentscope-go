package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event/streamcheck"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

type streamContractModel struct {
	chat   func(context.Context) (*model.ChatResponse, error)
	stream func(context.Context) (<-chan model.ChatResponse, error)
}

func (m *streamContractModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.chat != nil {
		return m.chat(ctx)
	}
	return nil, errors.New("unexpected Chat call")
}
func (m *streamContractModel) ChatStream(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	if m.stream != nil {
		return m.stream(ctx)
	}
	return nil, model.ErrStreamNotSupported
}
func (*streamContractModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func fixtureStream(chunks ...model.ChatResponse) (<-chan model.ChatResponse, error) {
	ch := make(chan model.ChatResponse, len(chunks))
	for i := range chunks {
		ch <- chunks[i]
	}
	close(ch)
	return ch, nil
}

func textChunk(text string, final bool) model.ChatResponse {
	return model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}, IsLast: final}
}

func collectModelStream(t *testing.T, a *UnifiedAgent) ([]event.Event, string, event.ReplyEndEvent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	var events []event.Event
	var text strings.Builder
	var end event.ReplyEndEvent
	for e := range ch {
		events = append(events, e)
		switch v := e.(type) {
		case event.TextBlockDeltaEvent:
			text.WriteString(v.Delta)
		case event.ReplyEndEvent:
			end = v
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	if err := streamcheck.Validate(events); err != nil {
		t.Fatal(err)
	}
	return events, text.String(), end
}

func TestModelStreamingBufferedCompatibility(t *testing.T) {
	for _, mode := range []string{"disabled", "unsupported", "redact", "block"} {
		t.Run(mode, func(t *testing.T) {
			var streams, chats atomic.Int32
			m := &streamContractModel{
				chat: func(context.Context) (*model.ChatResponse, error) {
					chats.Add(1)
					r := textChunk("secret", true)
					return &r, nil
				},
				stream: func(context.Context) (<-chan model.ChatResponse, error) {
					streams.Add(1)
					return nil, fmt.Errorf("wrapped: %w", model.ErrStreamNotSupported)
				},
			}
			var opts []AgentOption
			switch mode {
			case "disabled":
				opts = append(opts, WithModelStreaming(false))
			case "redact":
				opts = append(opts, WithMiddlewares(middleware.NewGuardrailMiddleware(middleware.KeywordRedactRule("filter", "filtered", "secret"))))
			case "block":
				opts = append(opts, WithMiddlewares(middleware.NewGuardrailMiddleware(middleware.KeywordBlockRule("filter", "secret"))))
			}
			a := NewUnifiedAgent("bot", "test", m, opts...)
			_, text, end := collectModelStream(t, a)
			want := "secret"
			if mode == "redact" {
				want = "filtered"
			}
			if mode == "block" {
				want = ""
				if end.FinishedReason != types.ReplyError {
					t.Fatal(end)
				}
			}
			if text != want || chats.Load() != 1 {
				t.Fatalf("text=%q chats=%d", text, chats.Load())
			}
			wantStreams := int32(0)
			if mode == "unsupported" {
				wantStreams = 1
			}
			if streams.Load() != wantStreams {
				t.Fatalf("streams=%d", streams.Load())
			}
		})
	}
}

func TestModelStreamingSetupRetryAndFallback(t *testing.T) {
	var primary, fallback atomic.Int32
	m := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
		primary.Add(1)
		return nil, errors.New("setup failed")
	}}
	f := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
		fallback.Add(1)
		return fixtureStream(textChunk("fallback", true))
	}}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 2, RetryDelay: time.Millisecond, FallbackModel: f}))
	_, text, end := collectModelStream(t, a)
	if text != "fallback" || end.FinishedReason != types.ReplyCompleted || primary.Load() != 2 || fallback.Load() != 1 {
		t.Fatalf("text=%q primary=%d fallback=%d end=%+v", text, primary.Load(), fallback.Load(), end)
	}
}

func TestModelStreamingInvalidCompletionNeverRetries(t *testing.T) {
	for _, mode := range []string{"nil", "missing_final", "terminal_error", "rewrite", "length", "content_filter", "partial_tool"} {
		t.Run(mode, func(t *testing.T) {
			var attempts, fallbacks atomic.Int32
			m := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
				attempts.Add(1)
				if mode == "nil" {
					return nil, nil
				}
				first := textChunk("visible", false)
				if mode == "missing_final" {
					return fixtureStream(first)
				}
				last := textChunk("visible", true)
				last.Usage = &model.ChatUsage{InputTokens: 7, OutputTokens: 2}
				switch mode {
				case "terminal_error":
					last.Error = errors.New("stream failed")
				case "rewrite":
					last = textChunk("changed", true)
				case "length":
					last.StopReason = model.StopReasonLength
				case "content_filter":
					last.StopReason = model.StopReasonContentFilter
				case "partial_tool":
					last.Content = append(last.Content, message.ToolCallBlock{Type: "tool_call", ID: "partial", Name: "work", Input: `{"value":`})
				}
				return fixtureStream(first, last)
			}}
			f := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
				fallbacks.Add(1)
				return fixtureStream(textChunk("wrong", true))
			}}
			a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, FallbackModel: f}))
			events, text, end := collectModelStream(t, a)
			if end.FinishedReason != types.ReplyError || attempts.Load() != 1 || fallbacks.Load() != 0 || strings.Contains(text, "changed") {
				t.Fatalf("end=%+v attempts=%d fallbacks=%d text=%q", end, attempts.Load(), fallbacks.Load(), text)
			}
			if len(a.state.Context) != 1 {
				t.Fatal("failed generation committed to history")
			}
			if mode == "terminal_error" {
				for _, e := range events {
					if v, ok := e.(event.ModelCallEndEvent); ok && v.InputTokens != 7 {
						t.Fatal("failed response usage lost")
					}
				}
			}
		})
	}
}

func TestModelStreamingFinalSuffixAndBlockIdentity(t *testing.T) {
	m := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
		return fixtureStream(
			model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{ID: "a", Text: "one"}}},
			model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{ID: "b", Text: "two"}}},
			model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{ID: "a", Text: "one!"}, message.TextBlock{ID: "b", Text: "two!"}, message.ThinkingBlock{ID: "c", Thinking: "final-only"}}},
		)
	}}
	a := NewUnifiedAgent("bot", "test", m)
	events, _, end := collectModelStream(t, a)
	if end.FinishedReason != types.ReplyCompleted {
		t.Fatal(end)
	}
	blocks := map[string]string{}
	for _, e := range events {
		if v, ok := e.(event.TextBlockDeltaEvent); ok {
			blocks[v.BlockID] += v.Delta
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks=%v", blocks)
	}
	var one, two bool
	for _, text := range blocks {
		one = one || text == "one!"
		two = two || text == "two!"
	}
	if !one || !two {
		t.Fatal(blocks)
	}
}

func TestModelStreamingCancellationStopsProducer(t *testing.T) {
	stopped := make(chan struct{})
	m := &streamContractModel{stream: func(ctx context.Context) (<-chan model.ChatResponse, error) {
		ch := make(chan model.ChatResponse)
		go func() {
			defer close(ch)
			defer close(stopped)
			select {
			case ch <- textChunk("first", false):
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}()
		return ch, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := NewUnifiedAgent("bot", "test", m)
	ch, err := a.ReplyStream(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	for e := range ch {
		if _, ok := e.(event.TextBlockDeltaEvent); ok {
			cancel()
		}
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("producer was not canceled")
	}
}

func TestModelStreamingCancelsAfterFinal(t *testing.T) {
	stopped := make(chan struct{})
	m := &streamContractModel{stream: func(ctx context.Context) (<-chan model.ChatResponse, error) {
		ch := make(chan model.ChatResponse, 1)
		ch <- textChunk("done", true)
		go func() { <-ctx.Done(); close(stopped); close(ch) }()
		return ch, nil
	}}
	_, text, end := collectModelStream(t, NewUnifiedAgent("bot", "test", m))
	if text != "done" || end.FinishedReason != types.ReplyCompleted {
		t.Fatal(text, end)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("final did not cancel producer")
	}
}

func TestReplyStreamingFailureAfterToolRound(t *testing.T) {
	var calls atomic.Int32
	m := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
		if calls.Add(1) == 1 {
			return fixtureStream(model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "call", Name: "missing_tool", Input: "{}", State: message.ToolCallPending}}})
		}
		return fixtureStream(textChunk("partial", false))
	}}
	a := NewUnifiedAgent("bot", "test", m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := a.Reply(ctx, "test")
	if err == nil || reply != nil || calls.Load() != 2 {
		t.Fatalf("reply=%v err=%v calls=%d", reply, err, calls.Load())
	}
}

func TestModelStreamingForcedSummary(t *testing.T) {
	var calls, executed atomic.Int32
	work := tool.NewFunctionTool("work", "work", json.RawMessage(`{"type":"object"}`), func(context.Context, map[string]any) (any, error) { executed.Add(1); return "done", nil })
	m := &streamContractModel{stream: func(context.Context) (<-chan model.ChatResponse, error) {
		if calls.Add(1) == 1 {
			return fixtureStream(model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "work", Name: "work", Input: "{}", State: message.ToolCallPending}}})
		}
		last := textChunk("summary", true)
		last.Content = append(last.Content, message.ToolCallBlock{Type: "tool_call", ID: "ghost", Name: "work", Input: "{}", State: message.ToolCallPending})
		return fixtureStream(textChunk("sum", false), textChunk("mary", false), last)
	}}
	a := NewUnifiedAgent("bot", "test", m, WithToolkit(tool.NewToolkit(work)), WithReactConfig(ReactConfig{MaxIters: 1}))
	_, text, end := collectModelStream(t, a)
	if text != "summary" || end.FinishedReason != types.ReplyExceedMaxIters || calls.Load() != 2 || executed.Load() != 1 {
		t.Fatalf("text=%q end=%+v calls=%d executed=%d", text, end, calls.Load(), executed.Load())
	}
	for _, msg := range a.state.Context {
		for _, b := range msg.Content {
			if tc, ok := b.(message.ToolCallBlock); ok && tc.ID == "ghost" {
				t.Fatal("forced summary tool stored")
			}
		}
	}
}
