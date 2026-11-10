package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// ErrConversationClosed is returned by Conversation methods after Close.
var ErrConversationClosed = errors.New("agent: conversation closed")

// Custom event names emitted on the conversation event stream (in addition to
// the regular per-turn reply events).
const (
	// ConversationEventInputInjected reports that pending user input was
	// steered into the running turn. Data carries {"inputs": []string}.
	ConversationEventInputInjected = "conversation_input_injected"
	// ConversationEventTurnInterrupted reports that Interrupt aborted a
	// turn. Data carries {"reply_id": string}.
	ConversationEventTurnInterrupted = "conversation_turn_interrupted"
	// ConversationEventTurnError reports that a queued turn failed to
	// start. Data carries {"error": string}.
	ConversationEventTurnError = "conversation_turn_error"

	// ConversationEventUserShellStart marks the start of a user-initiated
	// shell command. Data carries {"command_id": string, "command": string}.
	ConversationEventUserShellStart = "user_shell_start"
	// ConversationEventUserShellOutput streams merged stdout/stderr of a
	// user shell command. Data carries {"command_id": string, "data": string}.
	ConversationEventUserShellOutput = "user_shell_output"
	// ConversationEventUserShellEnd ends a user shell command. Data carries
	// {"command_id": string, "exit_code": int, "duration_ms": int64} and,
	// on failure, {"error": string}.
	ConversationEventUserShellEnd = "user_shell_end"
)

// interruptedTurnMarkerText is appended to the context after an interrupted
// turn so the model can distinguish a user abort from a tool failure (codex
// writes an equivalent history marker).
const interruptedTurnMarkerText = "[Interrupted by user] The previous turn was aborted before completion; tool calls above may have been canceled mid-execution."

// convOpKind enumerates queued conversation operations.
type convOpKind int

const (
	convOpInput convOpKind = iota
	convOpCompact
)

// convOp is one queued conversation operation. ack is used by Compact to
// report completion; Submit is fire-and-forget.
type convOp struct {
	kind  convOpKind
	input string
	ack   chan error
}

// conversationTurn carries the conversation handle through the turn context
// so the reply machinery can find the drain queue and the session-lifetime
// event context.
type conversationTurn struct {
	conv   *Conversation
	cancel context.CancelFunc
}

type conversationTurnKey struct{}

func withConversationTurn(ctx context.Context, t *conversationTurn) context.Context {
	return context.WithValue(ctx, conversationTurnKey{}, t)
}

func conversationTurnFrom(ctx context.Context) *conversationTurn {
	t, _ := ctx.Value(conversationTurnKey{}).(*conversationTurn)
	return t
}

// Conversation is an EXPERIMENTAL persistent turn kernel for a UnifiedAgent
// (see STABILITY.md). It adds the session semantics a chat-style host needs
// on top of the one-shot Reply/ReplyStream API:
//
//   - Submit queues user input. With no active turn the conversation starts
//     one immediately; during a turn the input is steered into it at the next
//     certified boundary (just before a model call, when no tool call awaits
//     its result), never splitting a tool_call/tool_result pair.
//   - Interrupt aborts the active turn: the turn context is canceled (which
//     kills in-flight tool processes via their command contexts), terminal
//     events are still delivered on the session-lifetime stream, a
//     model-visible interruption marker is recorded, and pending input is
//     RETAINED for the next turn (a deliberate divergence from codex, which
//     drops pending input on interrupt: typed input is user intent and
//     silently discarding it reads as data loss in a host UI).
//   - Compact queues a forced compression: it runs at the same certified
//     boundary during a turn, or immediately between turns.
//   - Events returns one persistent stream across turns; each turn still
//     emits its own ReplyStart/ReplyEnd pair.
//
// While a Conversation is active on an agent, direct Reply, ReplyStream and
// ResumeReplyStream calls are rejected; drives must go through the
// conversation. Close ends the conversation and releases the agent.
//
// Contracts hosts must know:
//   - Middleware must wrap (not replace) the reply context: a middleware that
//     drops context values silently degrades terminal-event delivery back to
//     the canceled turn context.
//   - A turn that fails to start surfaces as ConversationEventTurnError and
//     its input is NOT re-queued.
//   - Close relies on cooperative cancellation: a tool that ignores its
//     context keeps Close waiting (Go cannot kill goroutines).
//
// Concurrency: Submit/Interrupt/Compact/Close are safe to call from any
// goroutine. The conversation consumes queued operations from a single run
// goroutine, so turns never overlap.
type Conversation struct {
	agent  *UnifiedAgent
	ctx    context.Context
	cancel context.CancelFunc

	events chan event.Event

	mu       sync.Mutex
	pending  []convOp
	turn     *conversationTurn
	closed   bool
	wakeCh   chan struct{}
	loopDone chan struct{}

	// shellWG tracks user-shell goroutines. They forward events from
	// outside the run loop, so Close must wait for them before closing
	// the events channel.
	shellWG sync.WaitGroup
}

// NewConversation starts an EXPERIMENTAL persistent conversation on the
// agent. ctx bounds the conversation lifetime; Close releases the agent for
// direct use again. NewConversation fails while a reply is in flight or
// another conversation is active.
func (a *UnifiedAgent) NewConversation(ctx context.Context) (*Conversation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeConv != nil {
		return nil, fmt.Errorf("agent %s: conversation already active", a.name)
	}
	if a.activeReplies > 0 {
		return nil, fmt.Errorf("agent %s: reply in progress; wait for it to finish before starting a conversation", a.name)
	}
	convCtx, cancel := context.WithCancel(ctx)
	c := &Conversation{
		agent:    a,
		ctx:      convCtx,
		cancel:   cancel,
		events:   make(chan event.Event, 64),
		wakeCh:   make(chan struct{}, 1),
		loopDone: make(chan struct{}),
	}
	a.activeConv = c
	go c.run()
	return c, nil
}

// Events returns the persistent event stream. It is closed by Close.
func (c *Conversation) Events() <-chan event.Event { return c.events }

// Submit queues user input. During an active turn the input is injected at
// the next certified boundary; otherwise a new turn starts. Multiple inputs
// queued while idle are batched into a single turn.
func (c *Conversation) Submit(input string) error {
	if strings.TrimSpace(input) == "" {
		return fmt.Errorf("agent: conversation: empty input")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConversationClosed
	}
	c.pending = append(c.pending, convOp{kind: convOpInput, input: input})
	c.mu.Unlock()
	c.wake()
	return nil
}

// Interrupt aborts the active turn and reports whether a turn was running.
// Pending input is retained and drives the next turn.
func (c *Conversation) Interrupt() bool {
	c.mu.Lock()
	t := c.turn
	c.mu.Unlock()
	if t == nil {
		return false
	}
	t.cancel()
	return true
}

// Compact forces a context compression, queued behind the active turn's
// certified boundary when a turn is running, or run immediately when idle.
// It returns once the compression (or a no-op) completed.
func (c *Conversation) Compact(ctx context.Context) error {
	ack := make(chan error, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConversationClosed
	}
	c.pending = append(c.pending, convOp{kind: convOpCompact, ack: ack})
	c.mu.Unlock()
	c.wake()
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// Close interrupts any active turn, stops the conversation and releases the
// agent. The events channel is closed once the run loop and any in-flight
// user shell commands have exited.
func (c *Conversation) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	<-c.loopDone
	c.shellWG.Wait()
	close(c.events)
	c.agent.mu.Lock()
	if c.agent.activeConv == c {
		c.agent.activeConv = nil
	}
	c.agent.mu.Unlock()
	return nil
}

// RunUserShellCommand executes a user-typed shell command (codex's "!"
// escape) outside the model loop: the command runs through the platform
// shell, its merged output streams as user_shell_output events, and a
// user_shell_end event carries the exit code. The command never enters
// model context. It is user-initiated, so no permission prompt applies;
// it may run during an active turn (auxiliary output) or while idle.
// The call itself is non-blocking — execution reports through Events().
// A zero timeout leaves the conversation lifetime as the only bound.
//
// The events carry the command ID in their data payload and in the
// CustomEvent's reply-ID field, so consumers can correlate start, output
// and end for one command.
func (c *Conversation) RunUserShellCommand(command string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("agent: user shell command is empty")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", ErrConversationClosed
	}
	c.shellWG.Add(1)
	c.mu.Unlock()

	commandID := agentscope.GenerateID()
	c.forward(event.NewCustomEvent(commandID, ConversationEventUserShellStart,
		map[string]any{"command_id": commandID, "command": command}))

	go func() {
		defer c.shellWG.Done()
		start := time.Now()
		code, err := tool.RunUserShell(c.ctx, command, timeout, func(data []byte) {
			c.forward(event.NewCustomEvent(commandID, ConversationEventUserShellOutput,
				map[string]any{"command_id": commandID, "data": string(data)}))
		})
		value := map[string]any{
			"command_id":  commandID,
			"exit_code":   code,
			"duration_ms": time.Since(start).Milliseconds(),
		}
		if err != nil {
			value["error"] = err.Error()
		}
		c.forward(event.NewCustomEvent(commandID, ConversationEventUserShellEnd, value))
	}()
	return commandID, nil
}

// run is the single consumer of the operation queue.
func (c *Conversation) run() {
	defer close(c.loopDone)
	for {
		op, ok := c.nextOp()
		if !ok {
			return
		}
		switch op.kind {
		case convOpInput:
			inputs := []string{op.input}
			for {
				next, ok := c.peekOp()
				if !ok || next.kind != convOpInput {
					break
				}
				c.popOp()
				inputs = append(inputs, next.input)
			}
			c.runTurn(strings.Join(inputs, "\n\n"))
		case convOpCompact:
			c.runIdleCompact(op.ack)
		}
	}
}

// nextOp pops the head operation, blocking until work arrives or the
// conversation closes.
func (c *Conversation) nextOp() (convOp, bool) {
	for {
		c.mu.Lock()
		if len(c.pending) > 0 {
			op := c.pending[0]
			c.pending = c.pending[1:]
			c.mu.Unlock()
			return op, true
		}
		if c.closed {
			c.mu.Unlock()
			return convOp{}, false
		}
		c.mu.Unlock()
		select {
		case <-c.wakeCh:
		case <-c.ctx.Done():
			return convOp{}, false
		}
	}
}

func (c *Conversation) peekOp() (convOp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return convOp{}, false
	}
	return c.pending[0], true
}

func (c *Conversation) popOp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 {
		c.pending = c.pending[1:]
	}
}

// takePending drains every queued operation; called by the reply loop at the
// certified steering boundary.
func (c *Conversation) takePending() []convOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	ops := make([]convOp, len(c.pending))
	copy(ops, c.pending)
	c.pending = c.pending[:0]
	return ops
}

func (c *Conversation) wake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

// forward delivers one event on the session stream. Terminal turn events use
// the session context (never the canceled turn context), so an interrupted
// turn's ReplyEnd cannot be lost to a canceled-ctx select.
func (c *Conversation) forward(evt event.Event) {
	select {
	case c.events <- evt:
	case <-c.ctx.Done():
	}
}

// runTurn executes one turn to completion and reports interruptions.
func (c *Conversation) runTurn(input string) {
	turnCtx, cancel := context.WithCancel(c.ctx)
	th := &conversationTurn{conv: c, cancel: cancel}
	turnCtx = withConversationTurn(turnCtx, th)

	c.mu.Lock()
	c.turn = th
	c.mu.Unlock()

	ch, err := c.agent.replyStream(turnCtx, input, false, true)
	if err != nil {
		cancel()
		c.mu.Lock()
		c.turn = nil
		c.mu.Unlock()
		c.forward(event.NewCustomEvent("", ConversationEventTurnError, map[string]any{"error": err.Error()}))
		return
	}

	var replyID string
	interrupted := false
	for evt := range ch {
		switch e := evt.(type) {
		case event.ReplyStartEvent:
			replyID = e.ReplyID
		case event.ReplyEndEvent:
			// Only a user abort gets the interruption marker; a handed-off
			// end (MarkHandedOff) finished its tool step and is a normal end.
			if e.ReplyID == replyID && e.FinishedReason == types.ReplyInterrupted {
				interrupted = true
			}
		}
		c.forward(evt)
	}

	cancel()
	c.mu.Lock()
	c.turn = nil
	c.mu.Unlock()

	if interrupted {
		// Model-visible marker (codex parity): without it the model cannot
		// tell a user abort apart from a tool failure. The turn goroutine
		// has exited, so context writes are settled and this append cannot
		// split a tool_call/tool_result pair.
		c.agent.appendInterruptMarker()
		c.forward(event.NewCustomEvent(replyID, ConversationEventTurnInterrupted, map[string]any{"reply_id": replyID}))
	}
	// Persist the settled turn state (steered inputs are checkpointed at the
	// drain point already; this covers the interrupt marker and the final
	// assistant message for hosts with a StateSaver but no reply recovery).
	c.agent.Checkpoint(c.ctx)
}

// runIdleCompact executes a queued compression between turns.
func (c *Conversation) runIdleCompact(ack chan<- error) {
	compressed, err := c.agent.Compact(c.ctx)
	if err == nil && compressed {
		c.forward(event.NewCustomEvent("", "compacted", nil))
	}
	if ack != nil {
		ack <- err
	}
}

// appendInterruptMarker records the model-visible interruption marker.
func (a *UnifiedAgent) appendInterruptMarker() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Context = append(a.state.Context, message.UserMsg(a.name, interruptedTurnMarkerText))
}

// drainConversationOps applies queued conversation operations inside a
// running turn. It must only be called at the certified boundary: a
// StateReason iteration, where checkNextAction has confirmed that no tool
// call awaits its result — appending user messages here can never split a
// tool_call/tool_result pair, which is what makes ad-hoc Observe injection
// mid-turn unsafe.
func (a *UnifiedAgent) drainConversationOps(ctx context.Context, ch chan<- event.Event, replyID string, c *Conversation) error {
	ops := c.takePending()
	if len(ops) == 0 {
		return nil
	}
	var inputs []string
	var compacts []chan error
	for _, op := range ops {
		switch op.kind {
		case convOpInput:
			inputs = append(inputs, op.input)
		case convOpCompact:
			compacts = append(compacts, op.ack)
		}
	}
	if len(inputs) > 0 {
		a.mu.Lock()
		for _, in := range inputs {
			a.state.Context = append(a.state.Context, message.UserMsg(a.name, in))
		}
		a.mu.Unlock()
		emit(ctx, ch, event.NewCustomEvent(replyID, ConversationEventInputInjected, map[string]any{"inputs": inputs}))
		// Persist steered input immediately: with reply recovery a crash
		// before the next boundary checkpoint would silently drop input the
		// queue already consumed.
		if err := a.checkpointBoundary(ctx); err != nil {
			return err
		}
	}
	for _, ack := range compacts {
		compressed, err := a.Compact(ctx)
		if err == nil && compressed {
			emit(ctx, ch, event.NewCustomEvent(replyID, "compacted", nil))
		}
		if ack != nil {
			ack <- err
		}
		// A failed compact (e.g. compression not configured) is reported to
		// the Compact caller via ack but must not fail the running turn.
	}
	return nil
}
