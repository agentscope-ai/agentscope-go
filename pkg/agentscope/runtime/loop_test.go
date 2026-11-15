package runtime

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// --- mocks ---

// mockInputProvider returns a pre-defined sequence of inputs, then io.EOF.
type mockInputProvider struct {
	inputs []string
	idx    int
	// blockUntilCancel, when true, makes ReadInput block until ctx is done.
	blockUntilCancel bool
	interrupt        InterruptAction
}

func (m *mockInputProvider) ReadInput(ctx context.Context) (string, error) {
	if m.blockUntilCancel {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if m.idx >= len(m.inputs) {
		return "", io.EOF
	}
	in := m.inputs[m.idx]
	m.idx++
	return in, nil
}

func (m *mockInputProvider) OnInterrupt() InterruptAction { return m.interrupt }

// mockOutputSink records every call it receives.
type mockOutputSink struct {
	mu          sync.Mutex
	texts       []string
	thinking    []string
	toolCalls   []string
	toolResults []string
	errors      []error
	flushes     int
}

func (o *mockOutputSink) WriteText(text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.texts = append(o.texts, text)
	return nil
}

func (o *mockOutputSink) WriteThinking(text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.thinking = append(o.thinking, text)
	return nil
}

func (o *mockOutputSink) WriteToolCall(name string, input map[string]any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolCalls = append(o.toolCalls, name)
	return nil
}

func (o *mockOutputSink) WriteToolResult(name string, output string, state string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolResults = append(o.toolResults, name+":"+output+":"+state)
	return nil
}

func (o *mockOutputSink) WriteError(err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.errors = append(o.errors, err)
	return nil
}

func (o *mockOutputSink) Flush() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.flushes++
	return nil
}

// mockStreamer emits a fixed event script per reply and counts invocations.
type mockStreamer struct {
	mu    sync.Mutex
	calls int
	// emit produces the events for a given call (0-based).
	emit func(call int) []event.Event
}

func (m *mockStreamer) ReplyStream(ctx context.Context, input string) (<-chan event.Event, error) {
	m.mu.Lock()
	call := m.calls
	m.calls++
	m.mu.Unlock()

	ch := make(chan event.Event, 16)
	go func() {
		defer close(ch)
		var evs []event.Event
		if m.emit != nil {
			evs = m.emit(call)
		} else {
			evs = defaultReplyEvents()
		}
		for _, ev := range evs {
			ch <- ev
		}
	}()
	return ch, nil
}

func defaultReplyEvents() []event.Event {
	return []event.Event{
		event.NewTextBlockDeltaEvent("r", "b", "hello"),
		event.NewReplyEndEvent("s", "r"),
	}
}

func newLoop(streamer replyStreamer, in InputProvider, out OutputSink, opts ...LoopOption) *ConversationLoop {
	l := &ConversationLoop{agent: streamer, input: in, output: out}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// --- tests ---

func TestSingleTurnCompletes(t *testing.T) {
	in := &mockInputProvider{inputs: []string{"hi"}}
	out := &mockOutputSink{}
	streamer := &mockStreamer{}
	loop := newLoop(streamer, in, out)

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if streamer.calls != 1 {
		t.Fatalf("expected 1 reply, got %d", streamer.calls)
	}
	if len(out.texts) != 1 || out.texts[0] != "hello" {
		t.Fatalf("unexpected texts: %v", out.texts)
	}
	if out.flushes == 0 {
		t.Fatalf("expected at least one flush")
	}
}

func TestMultiTurnConversation(t *testing.T) {
	in := &mockInputProvider{inputs: []string{"one", "two", "three"}}
	out := &mockOutputSink{}
	streamer := &mockStreamer{}
	loop := newLoop(streamer, in, out)

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if streamer.calls != 3 {
		t.Fatalf("expected 3 replies, got %d", streamer.calls)
	}
	if len(out.texts) != 3 {
		t.Fatalf("expected 3 text outputs, got %d: %v", len(out.texts), out.texts)
	}
}

func TestEOFExitsCleanly(t *testing.T) {
	in := &mockInputProvider{inputs: []string{}} // immediate io.EOF
	out := &mockOutputSink{}
	streamer := &mockStreamer{}
	loop := newLoop(streamer, in, out)

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run should return nil on EOF, got: %v", err)
	}
	if streamer.calls != 0 {
		t.Fatalf("expected 0 replies on immediate EOF, got %d", streamer.calls)
	}
}

func TestMaxTurnsLimits(t *testing.T) {
	in := &mockInputProvider{inputs: []string{"a", "b", "c", "d", "e"}}
	out := &mockOutputSink{}
	streamer := &mockStreamer{}
	loop := newLoop(streamer, in, out, WithMaxTurns(2))

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if streamer.calls != 2 {
		t.Fatalf("expected 2 replies with MaxTurns=2, got %d", streamer.calls)
	}
}

func TestContextCancellationExits(t *testing.T) {
	in := &mockInputProvider{blockUntilCancel: true}
	out := &mockOutputSink{}
	streamer := &mockStreamer{}
	loop := newLoop(streamer, in, out)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run should return nil on cancellation, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestToolCallAndResultDispatch(t *testing.T) {
	in := &mockInputProvider{inputs: []string{"call the tool"}}
	out := &mockOutputSink{}
	streamer := &mockStreamer{emit: func(call int) []event.Event {
		return []event.Event{
			event.NewToolCallStartEvent("r", "tc1", "get_weather"),
			event.NewToolCallDeltaEvent("r", "tc1", `{"location":`),
			event.NewToolCallDeltaEvent("r", "tc1", `"SF"}`),
			event.NewToolCallEndEvent("r", "tc1"),
			event.NewToolResultStartEvent("r", "tc1", "get_weather"),
			event.NewToolResultTextDeltaEvent("r", "tc1", "sunny"),
			event.NewToolResultEndEvent("r", "tc1", message.ToolResultSuccess),
			event.NewReplyEndEvent("s", "r"),
		}
	}}
	loop := newLoop(streamer, in, out)

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(out.toolCalls) != 1 || out.toolCalls[0] != "get_weather" {
		t.Fatalf("unexpected tool calls: %v", out.toolCalls)
	}
	if len(out.toolResults) != 1 || out.toolResults[0] != "get_weather:sunny:success" {
		t.Fatalf("unexpected tool results: %v", out.toolResults)
	}
}

func TestOnEventCallback(t *testing.T) {
	in := &mockInputProvider{inputs: []string{"hi"}}
	out := &mockOutputSink{}
	streamer := &mockStreamer{}

	var count int
	loop := newLoop(streamer, in, out, WithOnEvent(func(event.Event) { count++ }))

	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected onEvent called for 2 events, got %d", count)
	}
}
