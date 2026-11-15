package channel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// fakeChannel records SendResponse/Notify calls and lets tests control
// inbound delivery.
type fakeChannel struct {
	mu        sync.Mutex
	responses []Response
	notices   []string
	drained   []string // final accumulated text per response
}

func (f *fakeChannel) Type() string      { return "fake" }
func (f *fakeChannel) ChannelID() string { return "fake-1" }
func (f *fakeChannel) Capabilities() Capability {
	return Capability{Text: true, MaxMessageLength: 100}
}
func (f *fakeChannel) StartListening(_ context.Context, _ EmitFunc) error { return nil }
func (f *fakeChannel) Status() Status                                     { return Status{State: StatusConnected} }
func (f *fakeChannel) Close() error                                       { return nil }

func (f *fakeChannel) SendResponse(_ context.Context, r Response) error {
	f.mu.Lock()
	f.responses = append(f.responses, r)
	f.mu.Unlock()
	var sb strings.Builder
	for evt := range r.Events {
		if de, ok := evt.(event.TextBlockDeltaEvent); ok {
			sb.WriteString(de.Delta)
		}
	}
	f.mu.Lock()
	f.drained = append(f.drained, sb.String())
	f.mu.Unlock()
	return nil
}

func (f *fakeChannel) Notify(_ context.Context, _ string, _ ChatKind, text string) error {
	f.mu.Lock()
	f.notices = append(f.notices, text)
	f.mu.Unlock()
	return nil
}

// fakeAgent parks on HITL when the input starts with "hitl".
type fakeAgent struct {
	mu       sync.Mutex
	inputs   []string
	confirms []*event.UserConfirmResultEvent
	gate     chan struct{}
	text     string
}

func (a *fakeAgent) SubmitUserConfirm(r *event.UserConfirmResultEvent) {
	a.mu.Lock()
	a.confirms = append(a.confirms, r)
	g := a.gate
	a.gate = nil
	a.mu.Unlock()
	if g != nil {
		close(g)
	}
}

func (a *fakeAgent) ReplyStream(ctx context.Context, input string) (<-chan event.Event, error) {
	a.mu.Lock()
	a.inputs = append(a.inputs, input)
	a.mu.Unlock()
	ch := make(chan event.Event, 16)
	go func() {
		defer close(ch)
		replyID := "reply-" + input
		send := func(e event.Event) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(event.NewReplyStartEvent("sess", replyID, "bot", message.RoleAssistant)) {
			return
		}
		if strings.HasPrefix(input, "hitl") {
			tc := message.ToolCallBlock{Type: "tool_call", ID: "tc-1", Name: "bash", Input: `{"command":"ls"}`, State: message.ToolCallAsking,
				SuggestedRules: []any{struct{}{}}}
			if !send(event.NewRequireUserConfirmEvent(replyID, []message.ToolCallBlock{tc})) {
				return
			}
			gate := make(chan struct{})
			a.mu.Lock()
			a.gate = gate
			a.mu.Unlock()
			select {
			case <-gate:
			case <-ctx.Done():
				return
			}
		}
		if !send(event.NewTextBlockStartEvent(replyID, "blk")) {
			return
		}
		if !send(event.NewTextBlockDeltaEvent(replyID, "blk", a.text)) {
			return
		}
		if !send(event.NewTextBlockEndEvent(replyID, "blk")) {
			return
		}
		send(event.NewReplyEndEvent("sess", replyID))
	}()
	return ch, nil
}

func newTestGateway(t *testing.T, agent *fakeAgent) (*Gateway, *fakeChannel, context.Context) {
	t.Helper()
	ch := &fakeChannel{}
	g := NewGateway(ch, func(string) (Agent, error) { return agent, nil })
	return g, ch, context.Background()
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestGateway_RunsReplyAndRendersStream(t *testing.T) {
	agent := &fakeAgent{text: "hello back"}
	g, ch, ctx := newTestGateway(t, agent)

	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hi"}})
	waitFor(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return len(ch.drained) == 1
	}, "response not rendered")

	ch.mu.Lock()
	got := ch.drained[0]
	ch.mu.Unlock()
	if got != "hello back" {
		t.Errorf("rendered text = %q", got)
	}
}

func TestGateway_TextConfirmationApprove(t *testing.T) {
	agent := &fakeAgent{text: "done"}
	g, ch, ctx := newTestGateway(t, agent)

	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl run"}})
	waitFor(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.gate != nil
	}, "reply did not park at confirmation")

	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "y"}})
	waitFor(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return len(ch.drained) == 1
	}, "reply did not complete after approval")

	agent.mu.Lock()
	confirms := agent.confirms
	agent.mu.Unlock()
	if len(confirms) != 1 || len(confirms[0].ConfirmResults) != 1 || !confirms[0].ConfirmResults[0].Confirmed {
		t.Fatalf("expected one approved confirmation, got %+v", confirms)
	}
}

func TestGateway_TextConfirmationDenyAndAlways(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		approved bool
		rules    bool
	}{
		{"n", false, false},
		{"no", false, false},
		{"拒绝", false, false},
		{"a", true, true},
		{"总是", true, true},
		{"是", true, false},
	} {
		agent := &fakeAgent{text: "done"}
		g, ch, ctx := newTestGateway(t, agent)
		_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl run"}})
		waitFor(t, func() bool {
			agent.mu.Lock()
			defer agent.mu.Unlock()
			return agent.gate != nil
		}, "reply did not park")
		_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: tc.answer}})
		waitFor(t, func() bool {
			ch.mu.Lock()
			defer ch.mu.Unlock()
			return len(ch.drained) == 1
		}, "reply did not finish for answer "+tc.answer)
		agent.mu.Lock()
		got := agent.confirms
		agent.mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("[%s] expected 1 confirm event, got %d", tc.answer, len(got))
		}
		cr := got[0].ConfirmResults[0]
		if cr.Confirmed != tc.approved {
			t.Errorf("[%s] Confirmed = %v, want %v", tc.answer, cr.Confirmed, tc.approved)
		}
		if tc.rules && len(cr.Rules) == 0 {
			t.Errorf("[%s] always must pass suggested rules through", tc.answer)
		}
		if !tc.rules && len(cr.Rules) != 0 {
			t.Errorf("[%s] rules must not be passed", tc.answer)
		}
	}
}

func TestGateway_NonDecisionTextWhileParkedIsDropped(t *testing.T) {
	agent := &fakeAgent{text: "done"}
	g, ch, ctx := newTestGateway(t, agent)
	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl run"}})
	waitFor(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.gate != nil
	}, "reply did not park")

	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "something else entirely"}})
	waitFor(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return len(ch.notices) == 1
	}, "expected a waiting-for-confirmation notice")

	ch.mu.Lock()
	notice := ch.notices[0]
	inputs := len(agent.inputs)
	ch.mu.Unlock()
	if !strings.Contains(notice, "confirmation") {
		t.Errorf("notice should mention confirmation: %q", notice)
	}
	if inputs != 1 {
		t.Errorf("dropped text must not start a new reply, inputs=%v", agent.inputs)
	}
}

func TestGateway_BusySessionDropsMessage(t *testing.T) {
	agent := &fakeAgent{text: "done"}
	g, ch, ctx := newTestGateway(t, agent)
	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl run"}})
	waitFor(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.gate != nil
	}, "reply did not park")

	// Approve to unpark, then immediately send two messages: the reply
	// finishes fast, so simulate busy by sending while the reply is still
	// draining is racy — instead verify the busy notice path directly via
	// a second hitl park.
	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "y"}})
	waitFor(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return len(ch.drained) == 1
	}, "first reply did not finish")

	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl again"}})
	waitFor(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.gate != nil
	}, "second reply did not park")
	// Parked (busy) session: plain text gets the confirmation notice, not
	// a new reply — same guard as the parked case; verify no 3rd input.
	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hello?"}})
	time.Sleep(50 * time.Millisecond)
	agent.mu.Lock()
	n := len(agent.inputs)
	agent.mu.Unlock()
	if n != 2 {
		t.Errorf("no new reply must start while one is in flight, inputs=%v", agent.inputs)
	}
}

func TestGateway_NativeConfirmationEvent(t *testing.T) {
	agent := &fakeAgent{text: "done"}
	g, ch, ctx := newTestGateway(t, agent)
	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl run"}})
	waitFor(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.gate != nil
	}, "reply did not park")

	_ = g.emit(ctx, Inbound{Confirmation: &ConfirmationEvent{
		ChatID: "chat-1", ToolCallID: "tc-1", Approved: true, Actor: "user-x",
	}})
	waitFor(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return len(ch.drained) == 1
	}, "reply did not complete after native confirmation")

	agent.mu.Lock()
	confirms := agent.confirms
	agent.mu.Unlock()
	if len(confirms) != 1 || !confirms[0].ConfirmResults[0].Confirmed || confirms[0].ConfirmResults[0].ToolCall.ID != "tc-1" {
		t.Fatalf("unexpected confirmation: %+v", confirms)
	}
}

func TestGateway_UnknownConfirmationIgnored(t *testing.T) {
	agent := &fakeAgent{text: "done"}
	g, _, ctx := newTestGateway(t, agent)
	// No parked calls at all — must not panic or submit anything.
	_ = g.emit(ctx, Inbound{Confirmation: &ConfirmationEvent{ToolCallID: "nope", Approved: true}})
	time.Sleep(30 * time.Millisecond)
	agent.mu.Lock()
	n := len(agent.confirms)
	agent.mu.Unlock()
	if n != 0 {
		t.Errorf("unknown confirmation must be ignored, got %d", n)
	}
}

func TestGateway_SessionUnbricksAfterAbnormalEnd(t *testing.T) {
	// HARNESS R7-M1: if a reply dies while confirmations are parked, the
	// session must not stay stuck in the parked branch.
	agent := &fakeAgent{text: "done"}
	g, ch, _ := newTestGateway(t, agent)
	ctx, cancel := context.WithCancel(context.Background())

	_ = g.emit(ctx, Inbound{Message: &Event{ChatID: "chat-1", Text: "hitl run"}})
	waitFor(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.gate != nil
	}, "reply did not park")

	cancel() // kill the parked reply
	waitFor(t, func() bool {
		g.mu.Lock()
		sess := g.sessions["chat-1"]
		g.mu.Unlock()
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return !sess.busy && len(sess.parked) == 0
	}, "parked state not cleaned up after abnormal end")

	// The next message must start a fresh reply, not hit the parked branch.
	_ = g.emit(context.Background(), Inbound{Message: &Event{ChatID: "chat-1", Text: "hi again"}})
	waitFor(t, func() bool {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		return len(ch.drained) >= 1
	}, "session bricked: no reply after abnormal end")
	ch.mu.Lock()
	notices := len(ch.notices)
	ch.mu.Unlock()
	if notices != 0 {
		t.Errorf("no housekeeping notices expected, got %d", notices)
	}
}

func TestParseDecision(t *testing.T) {
	cases := []struct {
		in                  string
		approved, rules, ok bool
	}{
		{"y", true, false, true},
		{"YES", true, false, true},
		{"确认", true, false, true},
		{"a", true, true, true},
		{"always", true, true, true},
		{"n", false, false, true},
		{"取消", false, false, true},
		{"hello", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		approved, rules, ok := parseDecision(c.in)
		if approved != c.approved || rules != c.rules || ok != c.ok {
			t.Errorf("parseDecision(%q) = (%v,%v,%v), want (%v,%v,%v)",
				c.in, approved, rules, ok, c.approved, c.rules, c.ok)
		}
	}
}

func TestSplitText(t *testing.T) {
	if got := SplitText("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("no-split case: %v", got)
	}
	text := strings.Repeat("abcdefghij ", 10) // 110 runes
	got := SplitText(strings.TrimSpace(text), 25)
	for _, chunk := range got {
		if len([]rune(chunk)) > 25 {
			t.Errorf("chunk exceeds maxLen: %q (%d)", chunk, len([]rune(chunk)))
		}
	}
	if strings.Join(got, "") == "" || len(got) < 4 {
		t.Errorf("expected several chunks, got %d", len(got))
	}
	// Line-boundary preference.
	lines := "line1\nline2\nline3"
	chunks := SplitText(lines, 12)
	if chunks[0] != "line1\nline2" {
		t.Errorf("expected line-boundary split, got %q", chunks[0])
	}
	// HARNESS R7-M3: the "\n" joiner counts against the bound — two
	// 5-rune lines at maxLen 10 must NOT merge into an 11-rune chunk.
	for _, chunk := range SplitText("line1\nline2", 10) {
		if len([]rune(chunk)) > 10 {
			t.Errorf("chunk exceeds maxLen once the joiner is counted: %q", chunk)
		}
	}
	if got := SplitText("line1\nline2", 10); len(got) != 2 {
		t.Errorf("expected 2 chunks, got %v", got)
	}
}
