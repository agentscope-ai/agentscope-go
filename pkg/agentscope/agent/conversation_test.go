package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// convModel is a scripted model that records every call's input. Turn calls
// arrive via ChatStream (agents stream by default); compression uses Chat.
type convModel struct {
	mu      sync.Mutex
	calls   [][]*message.Msg
	respond func(call int, msgs []*message.Msg) *model.ChatResponse
}

func (m *convModel) record(msgs []*message.Msg) (int, []*message.Msg) {
	m.mu.Lock()
	cp := make([]*message.Msg, len(msgs))
	copy(cp, msgs)
	m.calls = append(m.calls, cp)
	m.mu.Unlock()
	return len(m.calls) - 1, cp
}

func (m *convModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	idx, cp := m.record(msgs)
	return m.respond(idx, cp), nil
}

func (m *convModel) ChatStream(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	idx, cp := m.record(msgs)
	resp := m.respond(idx, cp)
	ch := make(chan model.ChatResponse, 1)
	ch <- *resp
	close(ch)
	return ch, nil
}

func (m *convModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func (m *convModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *convModel) call(i int) []*message.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[i]
}

func convText(text string) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}},
		IsLast:  true,
		Usage:   &model.ChatUsage{InputTokens: 5, OutputTokens: 1},
	}
}

func convToolCall(id, name string) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type:  "tool_call",
			ID:    id,
			Name:  name,
			Input: `{}`,
			State: message.ToolCallPending,
		}},
		IsLast: true,
		Usage:  &model.ChatUsage{InputTokens: 5, OutputTokens: 1},
	}
}

// gatedTool blocks in Execute until gate is closed or the context is
// canceled.
type gatedTool struct {
	tool.BaseTool
	started    chan struct{}
	gate       chan struct{}
	canceled   chan struct{}
	once       sync.Once
	cancelOnce sync.Once
}

func newGatedTool(name string) *gatedTool {
	t := &gatedTool{started: make(chan struct{}), gate: make(chan struct{}), canceled: make(chan struct{})}
	t.ToolName = name
	t.ToolDescription = "test gate"
	t.ToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	return t
}

func (t *gatedTool) Execute(ctx context.Context, _ map[string]any) (*tool.ToolResponse, error) {
	t.once.Do(func() { close(t.started) })
	select {
	case <-t.gate:
		return tool.NewTextResponse("gate released"), nil
	case <-ctx.Done():
		t.cancelOnce.Do(func() { close(t.canceled) })
		return nil, ctx.Err()
	}
}

// waitToolCanceled fails unless the gate tool observes cancellation. The
// observation lands in the tool goroutine, which is not joined by the turn
// unwind path (the forwarder can close the reply channel early on
// cancellation), so it can trail Close/Interrupt — poll with a deadline
// instead of asserting synchronously.
func waitToolCanceled(t *testing.T, gate *gatedTool, msg string) {
	t.Helper()
	select {
	case <-gate.canceled:
	case <-time.After(10 * time.Second):
		t.Fatal(msg)
	}
}

// memSaver is an in-memory StateSaver that flags snapshots containing a
// marker string.
type memSaver struct {
	mu     sync.Mutex
	snaps  int
	marker string
	saw    atomic.Bool
}

func (s *memSaver) SaveState(_ context.Context, _ string, st *AgentState) error {
	s.mu.Lock()
	s.snaps++
	s.mu.Unlock()
	if s.marker != "" {
		for _, m := range st.Context {
			if m == nil {
				continue
			}
			for _, b := range m.GetContentBlocks(message.ContentBlockText) {
				if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, s.marker) {
					s.saw.Store(true)
				}
			}
		}
	}
	return nil
}

func (s *memSaver) LoadState(context.Context, string) (*AgentState, error) { return nil, nil }
func (s *memSaver) ListSessions(context.Context) ([]SessionInfo, error)    { return nil, nil }
func (s *memSaver) DeleteSession(context.Context, string) error            { return nil }

func (s *memSaver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snaps
}

// convWait collects events until pred matches or the deadline passes.
func convWait(t *testing.T, c *Conversation, what string, pred func(event.Event) bool) []event.Event {
	t.Helper()
	var evs []event.Event
	timeout := time.After(15 * time.Second)
	for {
		select {
		case e, ok := <-c.Events():
			if !ok {
				t.Fatalf("events channel closed waiting for %s (got %d events)", what, len(evs))
			}
			evs = append(evs, e)
			if pred(e) {
				return evs
			}
		case <-timeout:
			t.Fatalf("timeout waiting for %s (got %d events)", what, len(evs))
		}
	}
}

func isReplyEnd(e event.Event) (event.ReplyEndEvent, bool) {
	end, ok := e.(event.ReplyEndEvent)
	return end, ok
}

func replyEndWithReason(reason types.ReplyFinishedReason) func(event.Event) bool {
	return func(e event.Event) bool {
		end, ok := isReplyEnd(e)
		return ok && end.FinishedReason == reason
	}
}

func isCustom(name string) func(event.Event) bool {
	return func(e event.Event) bool {
		ce, ok := e.(event.CustomEvent)
		return ok && ce.Name == name
	}
}

func msgsContainUser(msgs []*message.Msg, text string) bool {
	for _, m := range msgs {
		if m == nil || m.Role != message.RoleUser {
			continue
		}
		for _, b := range m.GetContentBlocks(message.ContentBlockText) {
			if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, text) {
				return true
			}
		}
	}
	return false
}

func TestConversationBasicTurn(t *testing.T) {
	m := &convModel{respond: func(int, []*message.Msg) *model.ChatResponse { return convText("answer") }}
	a := NewUnifiedAgent("bot", "test", m)
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Submit("hello"); err != nil {
		t.Fatal(err)
	}
	evs := convWait(t, c, "reply end", replyEndWithReason(types.ReplyCompleted))
	var sawStart, sawText bool
	for _, e := range evs {
		if _, ok := e.(event.ReplyStartEvent); ok {
			sawStart = true
		}
		if d, ok := e.(event.TextBlockDeltaEvent); ok && strings.Contains(d.Delta, "answer") {
			sawText = true
		}
	}
	if !sawStart || !sawText {
		t.Fatalf("start=%v text=%v", sawStart, sawText)
	}
	if m.callCount() != 1 {
		t.Fatalf("model calls = %d, want 1", m.callCount())
	}
	if !msgsContainUser(m.call(0), "hello") {
		t.Fatal("model call 0 missing user input")
	}
}

// Steering: input submitted mid-turn is injected at the next certified
// boundary and reaches the following model call, without ever splitting a
// tool_call/tool_result pair.
func TestConversationSteerMidTurn(t *testing.T) {
	gate := newGatedTool("gate")
	m := &convModel{respond: func(call int, _ []*message.Msg) *model.ChatResponse {
		if call == 0 {
			return convToolCall("tc1", "gate")
		}
		return convText("done")
	}}
	a := NewUnifiedAgent("bot", "test", m, WithToolkit(tool.NewToolkit(gate)))
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Submit("start"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}
	// Two steered inputs while the tool is blocked must batch into one drain.
	if err := c.Submit("steer one"); err != nil {
		t.Fatal(err)
	}
	if err := c.Submit("steer two"); err != nil {
		t.Fatal(err)
	}
	close(gate.gate)

	evs := convWait(t, c, "reply end", replyEndWithReason(types.ReplyCompleted))
	var injected *event.CustomEvent
	for _, e := range evs {
		if ce, ok := e.(event.CustomEvent); ok && ce.Name == ConversationEventInputInjected {
			cpy := ce
			injected = &cpy
		}
	}
	if injected == nil {
		t.Fatal("no input_injected event")
	}
	inputs, _ := injected.Value["inputs"].([]string)
	if len(inputs) != 2 {
		t.Fatalf("injected inputs = %v, want 2 batched", inputs)
	}

	if m.callCount() != 2 {
		t.Fatalf("model calls = %d, want 2", m.callCount())
	}
	second := m.call(1)
	if !msgsContainUser(second, "steer one") || !msgsContainUser(second, "steer two") {
		t.Fatal("second model call missing steered input")
	}

	// Certified-boundary invariant: in the final context, the tool result
	// closes its call before the steered user message appears.
	a.mu.Lock()
	defer a.mu.Unlock()
	var callMsgIdx, steerMsgIdx = -1, -1
	for i, msg := range a.state.Context {
		if msg == nil {
			continue
		}
		for _, b := range msg.Content {
			if tr, ok := b.(message.ToolResultBlock); ok && tr.ID == "tc1" {
				callMsgIdx = i
			}
		}
		if msg.Role == message.RoleUser {
			for _, b := range msg.GetContentBlocks(message.ContentBlockText) {
				if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, "steer one") {
					steerMsgIdx = i
				}
			}
		}
	}
	if callMsgIdx < 0 || steerMsgIdx < 0 {
		t.Fatalf("result msg=%d steer msg=%d", callMsgIdx, steerMsgIdx)
	}
	if steerMsgIdx < callMsgIdx {
		t.Fatalf("steered user message (msg %d) precedes the tool result (msg %d)", steerMsgIdx, callMsgIdx)
	}
}

// Interrupt must deliver the terminal ReplyEnd (session-ctx emission), the
// turn_interrupted event, cancel the running tool, and record a
// model-visible marker.
func TestConversationInterruptDeliversTerminalEvents(t *testing.T) {
	gate := newGatedTool("gate") // gate never released; only ctx cancel frees it
	m := &convModel{respond: func(int, []*message.Msg) *model.ChatResponse {
		return convToolCall("tc1", "gate")
	}}
	a := NewUnifiedAgent("bot", "test", m, WithToolkit(tool.NewToolkit(gate)))
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Submit("work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}
	if !c.Interrupt() {
		t.Fatal("Interrupt reported no active turn")
	}

	evs := convWait(t, c, "turn_interrupted", isCustom(ConversationEventTurnInterrupted))
	var end *event.ReplyEndEvent
	for _, e := range evs {
		if re, ok := e.(event.ReplyEndEvent); ok {
			ev := re
			end = &ev
		}
	}
	if end == nil {
		t.Fatal("terminal ReplyEnd lost after Interrupt")
	}
	if end.FinishedReason != types.ReplyInterrupted {
		t.Fatalf("reason = %v, want interrupted", end.FinishedReason)
	}
	waitToolCanceled(t, gate, "running tool was not canceled")

	// Model-visible marker follows the settled turn, and the canceled tool
	// call still has its (error) result recorded — no dangling tool_call.
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.state.Context) == 0 {
		t.Fatal("empty context")
	}
	last := a.state.Context[len(a.state.Context)-1]
	if last.Role != message.RoleUser || !msgsContainUser([]*message.Msg{last}, "Interrupted by user") {
		t.Fatalf("last message is not the interrupt marker: role=%s", last.Role)
	}
	var sawResult bool
	for _, msg := range a.state.Context {
		if msg == nil {
			continue
		}
		for _, b := range msg.Content {
			if tr, ok := b.(message.ToolResultBlock); ok && tr.ID == "tc1" {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Fatal("canceled tool call has no recorded result")
	}
}

// Interrupt retains pending input: it drives the next turn automatically.
func TestConversationInterruptRetainsPending(t *testing.T) {
	gate := newGatedTool("gate")
	m := &convModel{respond: func(call int, _ []*message.Msg) *model.ChatResponse {
		if call == 0 {
			return convToolCall("tc1", "gate")
		}
		return convText("second turn done")
	}}
	a := NewUnifiedAgent("bot", "test", m, WithToolkit(tool.NewToolkit(gate)))
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Submit("first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}
	if err := c.Submit("next task"); err != nil {
		t.Fatal(err)
	}
	c.Interrupt()

	convWait(t, c, "second turn end", replyEndWithReason(types.ReplyCompleted))
	if m.callCount() != 2 {
		t.Fatalf("model calls = %d, want 2", m.callCount())
	}
	if !msgsContainUser(m.call(1), "next task") {
		t.Fatal("retained pending input did not drive the next turn")
	}
}

func TestConversationExclusivity(t *testing.T) {
	m := &convModel{respond: func(int, []*message.Msg) *model.ChatResponse { return convText("ok") }}
	a := NewUnifiedAgent("bot", "test", m)

	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewConversation(context.Background()); err == nil {
		t.Fatal("second conversation accepted")
	}
	if _, err := a.ReplyStream(context.Background(), "direct"); err == nil ||
		!strings.Contains(err.Error(), "conversation") {
		t.Fatalf("direct ReplyStream during conversation: err=%v", err)
	}
	if _, err := a.Reply(context.Background(), "direct"); err == nil {
		t.Fatal("direct Reply during conversation accepted")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Agent released: direct replies work again.
	if _, err := a.Reply(context.Background(), "direct now"); err != nil {
		t.Fatalf("reply after Close: %v", err)
	}

	// NewConversation during an in-flight direct reply is rejected.
	ctx, cancel := context.WithCancel(context.Background())
	gate := newGatedTool("gate")
	a2 := NewUnifiedAgent("bot2", "test", &convModel{respond: func(call int, _ []*message.Msg) *model.ChatResponse {
		if call == 0 {
			return convToolCall("tc1", "gate")
		}
		return convText("ok")
	}}, WithToolkit(tool.NewToolkit(gate)))
	ch, err := a2.ReplyStream(ctx, "hold")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}
	if _, err := a2.NewConversation(context.Background()); err == nil {
		t.Fatal("conversation accepted during in-flight reply")
	}
	cancel()
	for range ch {
	}
}

// Steered input is checkpointed before the next model call consumes it.
func TestConversationSteerCheckpointedBeforeModelCall(t *testing.T) {
	gate := newGatedTool("gate")
	saver := &memSaver{marker: "persisted steer"}
	steerSavedBeforeCall := atomic.Bool{}
	m := &convModel{respond: func(call int, _ []*message.Msg) *model.ChatResponse {
		if call == 0 {
			return convToolCall("tc1", "gate")
		}
		// The drain checkpoint must have persisted the steered input before
		// this call ran.
		steerSavedBeforeCall.Store(saver.saw.Load())
		return convText("done")
	}}
	a := NewUnifiedAgent("bot", "test", m,
		WithToolkit(tool.NewToolkit(gate)),
		WithStateSaver(saver),
	)
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Submit("start"); err != nil {
		t.Fatal(err)
	}
	<-gate.started
	if err := c.Submit("persisted steer"); err != nil {
		t.Fatal(err)
	}
	close(gate.gate)
	convWait(t, c, "reply end", replyEndWithReason(types.ReplyCompleted))

	if !steerSavedBeforeCall.Load() {
		t.Fatal("steered input reached the model call without a prior checkpoint")
	}
	if saver.count() == 0 {
		t.Fatal("no checkpoints recorded")
	}
}

func TestConversationCompact(t *testing.T) {
	var compressionCalls atomic.Int32
	m := &convModel{respond: func(_ int, msgs []*message.Msg) *model.ChatResponse {
		for _, msg := range msgs {
			if msg == nil {
				continue
			}
			for _, b := range msg.GetContentBlocks(message.ContentBlockText) {
				if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, "task described above") {
					compressionCalls.Add(1)
					return &model.ChatResponse{
						Content: []message.ContentBlock{message.ToolCallBlock{
							Type:  "tool_call",
							ID:    "tc_summary",
							Name:  "generate_structured_output",
							Input: `{"task_overview":"Test task","current_state":"In progress","important_discoveries":"None","next_steps":"Continue","context_to_preserve":"Nothing"}`,
							State: message.ToolCallPending,
						}},
						IsLast: true,
					}
				}
			}
		}
		return convText("regular answer")
	}}
	a := NewUnifiedAgent("bot", "test", m,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Submit("hello"); err != nil {
		t.Fatal(err)
	}
	convWait(t, c, "reply end", replyEndWithReason(types.ReplyCompleted))

	// Idle compact runs immediately via the queue.
	if err := c.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if compressionCalls.Load() != 1 {
		t.Fatalf("compression calls = %d, want 1", compressionCalls.Load())
	}
	a.mu.Lock()
	summary := a.state.Summary
	a.mu.Unlock()
	if !strings.Contains(summary, "Test task") {
		t.Fatalf("summary = %q", summary)
	}
}

// Close with an active turn unwinds the turn, closes the stream and releases
// the agent.
func TestConversationClose(t *testing.T) {
	gate := newGatedTool("gate")
	m := &convModel{respond: func(int, []*message.Msg) *model.ChatResponse {
		return convToolCall("tc1", "gate")
	}}
	a := NewUnifiedAgent("bot", "test", m, WithToolkit(tool.NewToolkit(gate)))
	c, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Submit("work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitToolCanceled(t, gate, "active turn not canceled by Close")
	// Events channel closed.
	for range c.Events() {
	}
	if err := c.Submit("late"); !errors.Is(err, ErrConversationClosed) {
		t.Fatalf("submit after close: %v", err)
	}
	// Agent released: NewConversation is accepted again.
	c2, err := a.NewConversation(context.Background())
	if err != nil {
		t.Fatalf("conversation after Close: %v", err)
	}
	c2.Close()
}
