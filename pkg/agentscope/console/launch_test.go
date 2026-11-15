package console

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// fakeConsoleAgent scripts one reply per input. When confirmGate is non-nil,
// the reply parks after emitting the confirmation request until a
// confirmation is submitted (mimicking UnifiedAgent's StateWait).
type fakeConsoleAgent struct {
	mu          sync.Mutex
	inputs      []string
	confirms    []*event.UserConfirmResultEvent
	confirmGate chan struct{}
	withRules   bool
	text        string
	// Lets tests force a confirmation to arrive before the publisher resumes.
	afterConfirmPublish func()
}

func (f *fakeConsoleAgent) SubmitUserConfirm(result *event.UserConfirmResultEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms = append(f.confirms, result)
	if f.confirmGate != nil {
		close(f.confirmGate)
		f.confirmGate = nil
	}
}

func (f *fakeConsoleAgent) ReplyStream(ctx context.Context, input string) (<-chan event.Event, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, input)
	f.mu.Unlock()

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
			tc := message.ToolCallBlock{Type: "tool_call", ID: "tc-1", Name: "bash", Input: `{"command":"ls"}`, State: message.ToolCallAsking}
			if f.withRules {
				tc.SuggestedRules = []any{permission.Rule{ToolName: "bash", RuleContent: "ls", Behavior: permission.BehaviorAllow, Source: "test"}}
			}
			// Install the waiter before publishing: a consumer may answer as
			// soon as it sees the event, before this goroutine runs again.
			gate := make(chan struct{})
			f.mu.Lock()
			f.confirmGate = gate
			f.mu.Unlock()
			defer func() {
				f.mu.Lock()
				if f.confirmGate == gate {
					f.confirmGate = nil
				}
				f.mu.Unlock()
			}()
			if !send(event.NewRequireUserConfirmEvent(replyID, []message.ToolCallBlock{tc})) {
				return
			}
			if f.afterConfirmPublish != nil {
				f.afterConfirmPublish()
			}
			// Park until a confirmation arrives (or ctx cancels).
			select {
			case <-gate:
			case <-ctx.Done():
				return
			}
		}
		if !send(event.NewTextBlockStartEvent(replyID, "blk-1")) {
			return
		}
		if !send(event.NewTextBlockDeltaEvent(replyID, "blk-1", f.text)) {
			return
		}
		if !send(event.NewTextBlockEndEvent(replyID, "blk-1")) {
			return
		}
		send(event.NewReplyEndEvent("sess", replyID))
	}()
	return ch, nil
}

func launchWith(t *testing.T, a *fakeConsoleAgent, input string, opts ...LaunchOption) string {
	t.Helper()
	var out bytes.Buffer
	opts = append(opts, WithInput(strings.NewReader(input)), WithOutput(&out))
	if err := Launch(context.Background(), a, opts...); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return out.String()
}

func TestLaunch_ChatRoundTrip(t *testing.T) {
	a := &fakeConsoleAgent{text: "hi there"}
	out := launchWith(t, a, "hello\nexit\n")
	if !strings.Contains(out, "hi there") {
		t.Errorf("reply text not rendered:\n%q", out)
	}
	if len(a.inputs) != 1 || a.inputs[0] != "hello" {
		t.Errorf("inputs = %v, want [hello]", a.inputs)
	}
	if len(a.confirms) != 0 {
		t.Errorf("no confirmations expected, got %d", len(a.confirms))
	}
}

func TestLaunch_QuitWordsAndEOF(t *testing.T) {
	a := &fakeConsoleAgent{text: "x"}
	launchWith(t, a, "quit\n")
	if len(a.inputs) != 0 {
		t.Errorf("quit must not start a reply, inputs = %v", a.inputs)
	}
	launchWith(t, a, "") // immediate EOF
	if len(a.inputs) != 0 {
		t.Errorf("EOF must not start a reply, inputs = %v", a.inputs)
	}
}

func TestLaunch_ConfirmYes(t *testing.T) {
	a := &fakeConsoleAgent{text: "done"}
	out := launchWith(t, a, "hitl please\ny\nexit\n")
	if len(a.confirms) != 1 {
		t.Fatalf("expected 1 confirmation, got %d", len(a.confirms))
	}
	cr := a.confirms[0].ConfirmResults
	if len(cr) != 1 || !cr[0].Confirmed {
		t.Errorf("confirmation must be Confirmed=true: %+v", cr)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("reply must continue after confirmation:\n%q", out)
	}
}

func TestFakeConsoleAgentImmediateConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	submitted := make(chan struct{})
	a := &fakeConsoleAgent{text: "confirmed"}
	a.afterConfirmPublish = func() {
		// Pause the publisher after the event is visible, until the consumer
		// submits its answer. This forces the fast-consumer CI interleaving.
		select {
		case <-submitted:
		case <-ctx.Done():
		}
	}
	ch, err := a.ReplyStream(ctx, "hitl immediate")
	if err != nil {
		t.Fatal(err)
	}
	confirmed, completed, text := false, false, ""
	for evt := range ch {
		switch e := evt.(type) {
		case event.RequireUserConfirmEvent:
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{
				ReplyID: e.ReplyID,
				ConfirmResults: []event.ConfirmResult{{
					Confirmed: true,
					ToolCall:  e.ToolCalls[0],
				}},
			})
			confirmed = true
			close(submitted)
		case event.TextBlockDeltaEvent:
			text += e.Delta
		case event.ReplyEndEvent:
			completed = true
		}
	}
	if !confirmed || !completed || text != "confirmed" {
		t.Fatalf("immediate confirmation lost: confirmed=%t completed=%t text=%q context=%v", confirmed, completed, text, ctx.Err())
	}
}

func TestFakeConsoleAgentCanceledConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a := &fakeConsoleAgent{text: "must not continue", afterConfirmPublish: cancel}
	ch, err := a.ReplyStream(ctx, "hitl canceled")
	if err != nil {
		t.Fatal(err)
	}
	published := false
	for evt := range ch {
		if _, ok := evt.(event.RequireUserConfirmEvent); ok {
			published = true
		}
		if _, ok := evt.(event.ReplyEndEvent); ok {
			t.Fatal("canceled confirmation completed a reply")
		}
	}
	if !published || ctx.Err() != context.Canceled {
		t.Fatalf("confirmation did not reach explicit cancellation: published=%t context=%v", published, ctx.Err())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.confirmGate != nil {
		t.Fatal("canceled reply retained its confirmation gate")
	}
}

func TestLaunch_ConfirmNo(t *testing.T) {
	a := &fakeConsoleAgent{text: "after deny"}
	launchWith(t, a, "hitl please\nn\nexit\n")
	if len(a.confirms) != 1 || a.confirms[0].ConfirmResults[0].Confirmed {
		t.Errorf("n must produce Confirmed=false: %+v", a.confirms)
	}
}

func TestLaunch_ConfirmAlwaysPassesRules(t *testing.T) {
	a := &fakeConsoleAgent{text: "done", withRules: true}
	launchWith(t, a, "hitl please\na\nexit\n")
	if len(a.confirms) != 1 {
		t.Fatalf("expected 1 confirmation, got %d", len(a.confirms))
	}
	cr := a.confirms[0].ConfirmResults[0]
	if !cr.Confirmed {
		t.Fatal("always must confirm")
	}
	if len(cr.Rules) != 1 {
		t.Fatalf("always must pass through the suggested rules, got %v", cr.Rules)
	}
	if _, ok := cr.Rules[0].(permission.Rule); !ok {
		t.Errorf("rule must be a permission.Rule, got %T", cr.Rules[0])
	}
}

func TestLaunch_ConfirmAlwaysWithoutRulesIsRejected(t *testing.T) {
	// Python parity: "a" is only a valid answer when suggested rules are
	// offered; without them it is treated as a rejection.
	a := &fakeConsoleAgent{text: "done"} // no suggested rules
	launchWith(t, a, "hitl please\na\nexit\n")
	if len(a.confirms) != 1 {
		t.Fatalf("expected 1 confirmation, got %d", len(a.confirms))
	}
	cr := a.confirms[0].ConfirmResults[0]
	if cr.Confirmed {
		t.Error("a without suggested rules must not confirm (Python parity)")
	}
	if len(cr.Rules) != 0 {
		t.Errorf("no rules to pass through, got %v", cr.Rules)
	}
}

func TestLaunch_ContextCancelStops(t *testing.T) {
	a := &fakeConsoleAgent{text: "x"}
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	// Never-ending input stream: Launch must still return when ctx cancels.
	pr, _ := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Launch(ctx, a, WithInput(pr), WithOutput(&out))
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Launch returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Launch did not return after ctx cancel")
	}
}

func TestLaunch_EOFDuringConfirmExitsCleanly(t *testing.T) {
	// HARNESS review M-1: EOF consumed at a confirm question must abort
	// the parked reply AND let Launch exit — not hang at the next prompt.
	a := &fakeConsoleAgent{text: "never"}
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- Launch(context.Background(), a,
			WithInput(strings.NewReader("hitl please\n")), WithOutput(&out))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Launch hung after EOF at confirm prompt")
	}
	if len(a.confirms) != 0 {
		t.Errorf("no confirmation should be submitted, got %d", len(a.confirms))
	}
}

func TestLaunch_SIGINTInterruptsMidStream(t *testing.T) {
	// HARNESS review HIGH-1: Ctrl+C while the reply is streaming must
	// cancel that reply (and must not mislabel a completed one).
	a := &slowStreamAgent{}
	inReader, inWriter := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Launch(context.Background(), a, WithInput(inReader), WithOutput(&out))
	}()
	// Feed one input so the reply starts streaming, then interrupt.
	if _, err := inWriter.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := interruptSelf(); err != nil {
		t.Skipf("cannot raise SIGINT: %v", err)
	}
	// The interrupted reply must surface the notice; then exit cleanly.
	time.Sleep(100 * time.Millisecond)
	if _, err := inWriter.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Launch did not return after SIGINT + exit")
	}
	if !strings.Contains(out.String(), "Reply interrupted by the user") {
		t.Errorf("missing interruption notice:\n%q", out.String())
	}
	if !a.wasCanceled() {
		t.Error("streaming reply context was not canceled by SIGINT")
	}
}

// slowStreamAgent streams one delta, then blocks until its reply ctx is
// canceled — simulating a long-running reply for SIGINT tests.
type slowStreamAgent struct {
	mu       sync.Mutex
	wasCanc  bool
	confirms int
}

func (s *slowStreamAgent) wasCanceled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wasCanc
}

func (s *slowStreamAgent) SubmitUserConfirm(*event.UserConfirmResultEvent) { s.confirms++ }

func (s *slowStreamAgent) ReplyStream(ctx context.Context, input string) (<-chan event.Event, error) {
	ch := make(chan event.Event)
	go func() {
		defer close(ch)
		send := func(e event.Event) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(event.NewReplyStartEvent("sess", "reply-slow", "bot", message.RoleAssistant)) {
			return
		}
		if !send(event.NewTextBlockStartEvent("reply-slow", "blk-1")) {
			return
		}
		if !send(event.NewTextBlockDeltaEvent("reply-slow", "blk-1", "streaming…")) {
			return
		}
		// Block until canceled (the "long reply").
		<-ctx.Done()
		s.mu.Lock()
		s.wasCanc = true
		s.mu.Unlock()
	}()
	return ch, nil
}
