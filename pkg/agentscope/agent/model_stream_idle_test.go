package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// idleStreamModel serves scripted stream behaviors per call.
type idleStreamModel struct {
	calls atomic.Int32
	// silentFirst: call 1 streams nothing and blocks until its context ends.
	silentFirst bool
	// publishThenSilent: every call publishes a text delta, then blocks.
	publishThenSilent bool
}

func (m *idleStreamModel) Chat(context.Context, []*message.Msg, ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *idleStreamModel) ChatStream(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	call := m.calls.Add(1)
	ch := make(chan model.ChatResponse, 2)
	if m.publishThenSilent || (m.silentFirst && call == 1) {
		if m.publishThenSilent {
			ch <- model.ChatResponse{Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "partial"},
			}}
		}
		// Go silent; close only when the caller cancels the attempt.
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	ch <- model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "recovered"}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 1, OutputTokens: 1},
	}
	close(ch)
	return ch, nil
}

func (m *idleStreamModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

// A stream that goes silent before publishing anything is retried: replay is
// invisible to consumers, so the reply recovers on a later attempt.
func TestStreamIdleTimeout_RetriedBeforePublish(t *testing.T) {
	m := &idleStreamModel{silentFirst: true}
	a := NewUnifiedAgent("idle-agent", "helpful", m,
		WithModelConfig(ModelConfig{
			MaxRetries:        2,
			RetryDelay:        time.Millisecond,
			StreamIdleTimeout: 20 * time.Millisecond,
		}),
	)
	events, err := a.ReplyStream(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	var gotText bool
	for evt := range events {
		if d, ok := evt.(event.TextBlockDeltaEvent); ok && strings.Contains(d.Delta, "recovered") {
			gotText = true
		}
	}
	if !gotText {
		t.Error("expected the retried attempt's text")
	}
	if m.calls.Load() < 2 {
		t.Errorf("expected a retry after the silent stream, got %d calls", m.calls.Load())
	}
}

// Once a text delta escaped to consumers, an idle stream fails the reply
// instead of replaying (the consumer already rendered partial output).
func TestStreamIdleTimeout_FailsAfterPublish(t *testing.T) {
	m := &idleStreamModel{publishThenSilent: true}
	a := NewUnifiedAgent("idle-agent", "helpful", m,
		WithModelConfig(ModelConfig{
			MaxRetries:        2,
			RetryDelay:        time.Millisecond,
			StreamIdleTimeout: 20 * time.Millisecond,
		}),
	)
	events, err := a.ReplyStream(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for evt := range events {
		if end, ok := evt.(event.ReplyEndEvent); ok && end.FinishedReason == types.ReplyError {
			failed = true
		}
	}
	if !failed {
		t.Error("expected the reply to fail after a published-then-idle stream")
	}
	if m.calls.Load() != 1 {
		t.Errorf("published output must not be replayed, got %d calls", m.calls.Load())
	}
}

// A negative idle timeout disables the guard: a silent stream blocks until
// the caller's context ends, surfacing the context error (not an idle error).
func TestStreamIdleTimeout_Disabled(t *testing.T) {
	m := &idleStreamModel{publishThenSilent: true}
	a := NewUnifiedAgent("idle-agent", "helpful", m,
		WithModelConfig(ModelConfig{
			MaxRetries:        1,
			RetryDelay:        time.Millisecond,
			StreamIdleTimeout: -1,
		}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	events, err := a.ReplyStream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	// The reply ended via context cancellation, i.e. the idle guard did not
	// fire first (20ms would have beaten the 100ms context).
	if ctx.Err() == nil {
		t.Error("expected the caller context to end the reply")
	}
}
