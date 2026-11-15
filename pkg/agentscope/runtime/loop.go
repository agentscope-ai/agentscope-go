// Package runtime provides conversation-loop and I/O primitives that drive a
// UnifiedAgent across multiple turns, decoupling the agent's event stream from
// concrete input sources and output renderers.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// InterruptAction determines how to handle user interrupts.
type InterruptAction int

const (
	InterruptPause  InterruptAction = iota // pause current reply, allow resume
	InterruptAbort                         // abort current reply
	InterruptIgnore                        // ignore interrupt signal
)

// InputProvider reads user input for the conversation loop.
type InputProvider interface {
	// ReadInput blocks until user input is available.
	// Returns io.EOF when input is exhausted (e.g. stdin closed).
	ReadInput(ctx context.Context) (string, error)

	// OnInterrupt is called when an interrupt signal is received during agent
	// processing. The returned action decides how the loop reacts.
	OnInterrupt() InterruptAction
}

// OutputSink renders agent output to the user.
type OutputSink interface {
	WriteText(text string) error
	WriteThinking(text string) error
	WriteToolCall(name string, input map[string]any) error
	WriteToolResult(name string, output string, state string) error
	WriteError(err error) error
	Flush() error
}

// replyStreamer is the minimal surface of *agent.UnifiedAgent that the loop
// depends on. Keeping it as an interface lets tests drive the loop with a mock
// while the public constructor still accepts the concrete agent type.
type replyStreamer interface {
	ReplyStream(ctx context.Context, input string) (<-chan event.Event, error)
}

// LoopOption configures the ConversationLoop.
type LoopOption func(*ConversationLoop)

// WithIdleTimeout sets the idle timeout after which the loop exits if no user
// input arrives. Zero (the default) disables the timeout.
func WithIdleTimeout(d time.Duration) LoopOption {
	return func(l *ConversationLoop) { l.idleTimeout = d }
}

// WithOnEvent sets a callback invoked for every event emitted by the agent.
func WithOnEvent(fn func(event.Event)) LoopOption {
	return func(l *ConversationLoop) { l.onEvent = fn }
}

// WithMaxTurns sets a maximum number of conversation turns (0 = unlimited).
func WithMaxTurns(n int) LoopOption {
	return func(l *ConversationLoop) { l.maxTurns = n }
}

// ConversationLoop manages a multi-turn conversation with an agent.
type ConversationLoop struct {
	agent       replyStreamer
	input       InputProvider
	output      OutputSink
	onEvent     func(event.Event)
	idleTimeout time.Duration
	maxTurns    int
}

// NewConversationLoop creates a new conversation loop.
func NewConversationLoop(a *agent.UnifiedAgent, input InputProvider, output OutputSink, opts ...LoopOption) *ConversationLoop {
	l := &ConversationLoop{
		agent:  a,
		input:  input,
		output: output,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Run starts the conversation loop. It blocks until:
//   - ctx is canceled
//   - InputProvider returns io.EOF
//   - maxTurns is reached
//   - IdleTimeout expires while waiting for input
func (l *ConversationLoop) Run(ctx context.Context) error {
	for turn := 0; l.maxTurns == 0 || turn < l.maxTurns; turn++ {
		if ctx.Err() != nil {
			return nil
		}

		input, err := l.readInput(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if input == "" {
			continue
		}

		if err := l.runTurn(ctx, input); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
	return nil
}

// readInput reads a single line of user input, applying the idle timeout.
func (l *ConversationLoop) readInput(ctx context.Context) (string, error) {
	if l.idleTimeout <= 0 {
		return l.input.ReadInput(ctx)
	}
	tctx, cancel := context.WithTimeout(ctx, l.idleTimeout)
	defer cancel()
	return l.input.ReadInput(tctx)
}

// runTurn streams one agent reply and dispatches its events to the output sink.
func (l *ConversationLoop) runTurn(ctx context.Context, input string) error {
	ch, err := l.agent.ReplyStream(ctx, input)
	if err != nil {
		_ = l.output.WriteError(err)
		return err
	}

	st := newTurnState()
	for {
		select {
		case <-ctx.Done():
			// Interrupt received via context cancellation: consult the input
			// provider for how to proceed.
			switch l.input.OnInterrupt() {
			case InterruptIgnore:
				// Keep consuming; fall through to the channel receive below.
			default: // InterruptAbort, InterruptPause
				return ctx.Err()
			}
			ev, ok := <-ch
			if !ok {
				return l.output.Flush()
			}
			l.handleEvent(ev, st)
		case ev, ok := <-ch:
			if !ok {
				return l.output.Flush()
			}
			l.handleEvent(ev, st)
		}
	}
}

// turnState accumulates streamed fragments so that tool-call arguments and
// tool-result bodies can be rendered as complete units once their streams end.
type turnState struct {
	toolNames   map[string]string // toolCallID -> tool name
	toolArgs    map[string]string // toolCallID -> accumulated argument JSON
	resultNames map[string]string // toolCallID -> tool name (from result start)
	resultText  map[string]string // toolCallID -> accumulated result text
}

func newTurnState() *turnState {
	return &turnState{
		toolNames:   map[string]string{},
		toolArgs:    map[string]string{},
		resultNames: map[string]string{},
		resultText:  map[string]string{},
	}
}

func (l *ConversationLoop) handleEvent(ev event.Event, st *turnState) {
	if l.onEvent != nil {
		l.onEvent(ev)
	}

	switch e := ev.(type) {
	case event.TextBlockDeltaEvent:
		_ = l.output.WriteText(e.Delta)
	case event.ThinkingBlockDeltaEvent:
		_ = l.output.WriteThinking(e.Delta)

	case event.ToolCallStartEvent:
		st.toolNames[e.ToolCallID] = e.ToolCallName
	case event.ToolCallDeltaEvent:
		st.toolArgs[e.ToolCallID] += e.Delta
	case event.ToolCallEndEvent:
		name := st.toolNames[e.ToolCallID]
		var input map[string]any
		if raw := st.toolArgs[e.ToolCallID]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &input)
		}
		_ = l.output.WriteToolCall(name, input)

	case event.ToolResultStartEvent:
		st.resultNames[e.ToolCallID] = e.ToolCallName
	case event.ToolResultTextDeltaEvent:
		st.resultText[e.ToolCallID] += e.Delta
	case event.ToolResultEndEvent:
		name := st.resultNames[e.ToolCallID]
		if name == "" {
			name = st.toolNames[e.ToolCallID]
		}
		_ = l.output.WriteToolResult(name, st.resultText[e.ToolCallID], string(e.State))

	case event.ReplyEndEvent:
		_ = l.output.Flush()
	}
}
