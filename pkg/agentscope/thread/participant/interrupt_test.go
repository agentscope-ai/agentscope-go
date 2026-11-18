package participant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// waitIdle waits until a has no reply in flight. An empty thread update
// changes nothing and fails only while a reply runs.
func waitIdle(t *testing.T, a *agent.UnifiedAgent) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.ApplyThreadUpdate(context.Background(), agent.ThreadUpdate{}) != nil {
		if time.Now().After(deadline) {
			t.Error("the agent's reply did not end")
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func setAfterWrite(h *fakeHost, fn func(context.Context, uint64)) {
	h.mu.Lock()
	h.afterWrite = fn
	h.mu.Unlock()
}

// TestCommittedWriteAfterCancelReachesTheAgent covers a write that commits
// while the reply is canceled: the agent has stopped waiting and recorded
// that no result arrived, so the participant puts a note naming the committed
// entry into its context before the next activation, without moving the
// cursor back.
func TestCommittedWriteAfterCancelReachesTheAgent(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: steps(
		resp("", call("r1", ToolReport, map[string]any{"text": "progress"})),
		textResp("next answer"),
	)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setAfterWrite(f.host, func(context.Context, uint64) {
		// The user interrupts right after the write committed, and the agent
		// stops waiting before the participant delivers the answer.
		cancel()
		waitIdle(t, f.a)
	})
	ch, err := f.p.Activate(ctx, foreground(1, "report progress"))
	if err != nil {
		t.Fatal(err)
	}
	_, end := drain(t, ch, nil)
	if end.Reason != types.ReplyInterrupted {
		t.Fatalf("end: %+v", end)
	}
	setAfterWrite(f.host, nil)
	if v := f.a.ThreadView(); v.Cursor != 1 {
		t.Fatalf("the note must not move the cursor: %+v", v)
	}

	_, end = activate(t, f.p, foreground(2, "go on"), nil)
	if end.Err != nil {
		t.Fatalf("second end: %+v", end)
	}
	in := f.m.input(1)
	if !strings.Contains(in, "External execution timed out") ||
		!strings.Contains(in, "[Your report call was committed as entry #101 before the reply was interrupted.]") {
		t.Fatalf("the next request must say the report was committed:\n%s", in)
	}
}

// TestTeamCommittedWriteAfterCancelReachesTheMaster is the same for a team:
// the run refuses the late answer, and the note reaches the next master
// through the saved state.
func TestTeamCommittedWriteAfterCancelReachesTheMaster(t *testing.T) {
	f := newTeamFixture(t, teamOptions{master: queued(
		resp("", call("r1", ToolReport, map[string]any{"text": "progress"})),
		textResp("second"),
	)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setAfterWrite(f.host, func(context.Context, uint64) { cancel() })
	ch, err := f.tp.Activate(ctx, foreground(1, "report progress"))
	if err != nil {
		t.Fatal(err)
	}
	_, end := drain(t, ch, nil)
	if end.Reason != types.ReplyInterrupted || !errors.Is(end.Err, context.Canceled) {
		t.Fatalf("end: %+v", end)
	}
	setAfterWrite(f.host, nil)

	_, end = activate(t, f.tp, foreground(2, "go on"), nil)
	if end.Err != nil || end.Text != "second" {
		t.Fatalf("second end: %+v", end)
	}
	in := f.master.input(f.master.calls() - 1)
	if !strings.Contains(in, "[Your report call was committed as entry #101 before the reply was interrupted.]") {
		t.Fatalf("the next master must hear that the report was committed:\n%s", in)
	}
}

// failSaver is a memSaver whose saves fail while fail is set.
type failSaver struct {
	*memSaver
	fail atomic.Bool
}

func (s *failSaver) SaveState(ctx context.Context, id string, st *agent.AgentState) error {
	if s.fail.Load() {
		return errors.New("disk full")
	}
	return s.memSaver.SaveState(ctx, id, st)
}

// TestSettleFailuresAreReported covers a failed update or save after an
// activation: it becomes the activation's error, or is logged when the
// activation already failed.
func TestSettleFailuresAreReported(t *testing.T) {
	saver := &failSaver{memSaver: newMemSaver()}
	saver.fail.Store(true)
	// The agent does not save after a final answer: the first save is the
	// one after the reply.
	f := newFixture(t, fixtureOptions{
		noPrep:  true,
		agent:   []agent.AgentOption{agent.WithStateSaver(saver)},
		respond: steps(textResp("done")),
	})
	_, end := activate(t, f.p, foreground(1, "fetch"), nil)
	if end.Err == nil || !strings.Contains(end.Err.Error(), "update the agent after the reply") || !strings.Contains(end.Err.Error(), "disk full") {
		t.Fatalf("end: %+v", end)
	}

	// A canceled reply keeps its own error; the failed note is logged.
	saver.fail.Store(false)
	g := newFixture(t, fixtureOptions{
		agent:   []agent.AgentOption{agent.WithStateSaver(saver)},
		respond: steps(resp("", call("r1", ToolReport, map[string]any{"text": "progress"}))),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setAfterWrite(g.host, func(context.Context, uint64) {
		cancel()
		waitIdle(t, g.a)
		saver.fail.Store(true)
	})
	ch, err := g.p.Activate(ctx, foreground(1, "report"))
	if err != nil {
		t.Fatal(err)
	}
	// The agent's interrupted end may reach the forwarder before the stream
	// closes; then the end carries no error of its own yet.
	if _, end := drain(t, ch, nil); end.Reason != types.ReplyInterrupted || end.Err == nil ||
		!errors.Is(end.Err, context.Canceled) && !strings.Contains(end.Err.Error(), "update the agent after the reply") {
		t.Fatalf("canceled end: %+v", end)
	}
	first := errors.New("first failure")
	failed := &thread.ActivationEnd{Err: first}
	reportSettle("cloud", failed, errors.New("second failure"))
	if failed.Err != first {
		t.Fatalf("an activation that already failed keeps its error: %v", failed.Err)
	}

	// A team whose master cannot be saved after the run.
	saver.fail.Store(true)
	tf := newTeamFixture(t, teamOptions{master: queued(textResp("answer")),
		factory: func(f *teamFixture, ctx context.Context, assign tool.Tool, threadTools []tool.Tool, opts []agent.AgentOption) (*agent.UnifiedAgent, error) {
			tk, err := pipeline.NewTeamLeaderToolkit(assign, threadTools...)
			if err != nil {
				return nil, err
			}
			all := append([]agent.AgentOption{agent.WithToolkit(tk), agent.WithModelStreaming(false),
				agent.WithPermissionContext(permission.NewContext(permission.ModeBypass)), agent.WithStateSaver(saver)}, opts...)
			return agent.NewUnifiedAgent("master", "sys", f.master, all...), nil
		}})
	if _, end := activate(t, tf.tp, foreground(1, "hi"), nil); end.Err == nil || !strings.Contains(end.Err.Error(), "update and save the master after the run") {
		t.Fatalf("team end: %+v", end)
	}
}

// TestInputEstimates covers the rough input estimate a team participant
// reports before its first master: text, tool calls and results, thinking
// and summaries count; a reset starts the estimate over.
func TestInputEstimates(t *testing.T) {
	st := &agent.AgentState{
		Summary: "ssss", PeerSummary: &agent.PeerSummary{Text: "pppp"},
		Context: []*message.Msg{
			nil,
			message.UserMsg("user", "uuuu"),
			message.NewMsg("a", message.RoleAssistant, []message.ContentBlock{
				message.ThinkingBlock{Type: "thinking", Thinking: "tttt"},
				message.ToolCallBlock{Type: "tool_call", ID: "1", Name: "ab", Input: "cd"},
			}),
			message.NewMsg("a", message.RoleUser, []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "1", Name: "ab", Output: "rrrr"}}),
		},
	}
	if n := stateTokens(st); n != 6 { // 24 characters
		t.Fatalf("state estimate = %d", n)
	}
	if n := updateTokens(&agent.ThreadUpdate{PeerSummary: &agent.PeerSummary{Text: "pppp"}, Messages: []*message.Msg{message.UserMsg("u", "mmmm")}}); n != 2 {
		t.Fatalf("update estimate = %d", n)
	}

	f := newTeamFixture(t, teamOptions{master: queued(textResp("unused"))})
	if err := f.tp.Sync(context.Background(), thread.Sync{Messages: []*message.Msg{projected(1, strings.Repeat("a", 400))}, Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	sum := entryText(2, thread.KindSummary, strings.Repeat("b", 40))
	if err := f.tp.Sync(context.Background(), thread.Sync{Reset: true, PeerSummary: &sum, Cursor: 2}); err != nil {
		t.Fatal(err)
	}
	if st := f.tp.State(); st.Cursor != 2 || st.InputTokens != 10 {
		t.Fatalf("a reset starts the estimate over: %+v", st)
	}
}

// gateSaver is a memSaver whose saves block, once armed, until open.
type gateSaver struct {
	*memSaver
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	open    chan struct{}
	once    sync.Once
}

func newGateSaver() *gateSaver {
	return &gateSaver{memSaver: newMemSaver(), entered: make(chan struct{}), open: make(chan struct{})}
}

func (s *gateSaver) arm() {
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
}

func (s *gateSaver) release() { s.once.Do(func() { close(s.open) }) }

func (s *gateSaver) SaveState(ctx context.Context, id string, st *agent.AgentState) error {
	s.mu.Lock()
	armed := s.armed
	s.mu.Unlock()
	if armed {
		select {
		case <-s.entered:
		default:
			close(s.entered)
		}
		<-s.open
	}
	return s.memSaver.SaveState(ctx, id, st)
}

// TestSubmitUserConfirmAfterCancelIsNotDelivered covers an answer that comes
// after the reply was canceled while it waited for a confirmation: the agent
// no longer reads it, so it is refused at once instead of being left in the
// agent's channel, and the confirmation stays pending.
func TestSubmitUserConfirmAfterCancelIsNotDelivered(t *testing.T) {
	saver := newGateSaver()
	defer saver.release()
	// In the default mode a host tool without an allow rule asks first.
	f := newFixture(t, fixtureOptions{
		mode:  permission.ModeDefault,
		tools: []tool.Tool{textTool("web_fetch", "payload")},
		agent: []agent.AgentOption{agent.WithStateSaver(saver)},
		respond: steps(
			resp("", call("c1", "web_fetch", map[string]any{"url": "u"})),
			textResp("must not be called"),
		),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := f.p.Activate(ctx, foreground(1, "fetch"))
	if err != nil {
		t.Fatal(err)
	}
	var submitErr error
	var took time.Duration
	_, end := drain(t, ch, func(it thread.ActivationEvent) {
		req, ok := it.Event.(event.RequireUserConfirmEvent)
		if !ok {
			return
		}
		// Hold the activation open in its next save, then cancel it while
		// the confirmation is pending.
		saver.arm()
		cancel()
		<-saver.entered
		start := time.Now()
		submitErr = f.p.SubmitUserConfirm("", &event.UserConfirmResultEvent{ReplyID: req.ReplyID,
			ConfirmResults: []event.ConfirmResult{{Confirmed: true, ToolCall: req.ToolCalls[0]}}})
		took = time.Since(start)
		if act := f.p.c.current(); act == nil || !act.takeConfirm() {
			t.Error("a confirmation that was not delivered must stay pending")
		}
		saver.release()
	})
	if !errors.Is(submitErr, context.Canceled) || took > submitTimeout/2 {
		t.Fatalf("submit after cancel: %v after %v", submitErr, took)
	}
	if end.Reason != types.ReplyInterrupted || f.m.calls() != 1 || len(f.host.snapshot().recorded) != 0 {
		t.Fatalf("end: %+v calls=%d", end, f.m.calls())
	}
}

// TestTransferCountsTheEventsSentBefore covers EventsBefore: every event the
// activation sent before the transfer call, among them the text the
// transfer seals.
func TestTransferCountsTheEventsSentBefore(t *testing.T) {
	f := newFixture(t, fixtureOptions{respond: steps(resp("Handing over.", call("t1", ToolTransfer, transferArgs("edge"))))})
	items, end := activate(t, f.p, foreground(1, "go"), nil)
	if end.Reason != types.ReplyHandedOff {
		t.Fatalf("end: %+v", end)
	}
	answered, text := -1, -1
	for i, it := range items {
		switch e := it.Event.(type) {
		case event.TextBlockDeltaEvent:
			if text < 0 && strings.Contains(e.Delta, "Handing over.") {
				text = i
			}
		case event.ToolResultTextDeltaEvent:
			if answered < 0 && e.ToolCallID == "t1" {
				answered = i
			}
		}
	}
	tr := f.host.snapshot().transfers
	if len(tr) != 1 || text < 0 || answered <= text || tr[0].EventsBefore != answered {
		t.Fatalf("EventsBefore = %v, text at %d, the answer at %d", tr, text, answered)
	}
}

// TestForkKeepsToolChoiceWhenConfigured covers KeepForkToolChoice: a fork
// sends the main request's own tool choice instead of "none".
func TestForkKeepsToolChoiceWhenConfigured(t *testing.T) {
	choices := func(m *scriptModel) (main, fork string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		mode := func(o model.CallOptions) string {
			if o.ToolChoice == nil {
				return "(absent)"
			}
			return o.ToolChoice.Mode
		}
		return mode(m.opts[0]), mode(m.opts[len(m.opts)-1])
	}
	check := func(name string, keep bool, m *scriptModel) {
		t.Helper()
		main, fork := choices(m)
		if main == "none" || (keep && fork != main) || (!keep && fork != "none") {
			t.Fatalf("%s keep=%v: main request tool choice %s, fork %s", name, keep, main, fork)
		}
	}
	for _, keep := range []bool{false, true} {
		f := newFixture(t, fixtureOptions{cfg: AgentConfig{KeepForkToolChoice: keep}, respond: func(context.Context, int, *model.CallOptions) (*model.ChatResponse, error) {
			return textResp("text"), nil
		}})
		activate(t, f.p, foreground(1, "question"), nil)
		snap, err := f.p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.p.Fork(context.Background(), snap, thread.ForkSpec{Prompt: "Summarize."}); err != nil {
			t.Fatal(err)
		}
		check("agent", keep, f.m)

		tf := newTeamFixture(t, teamOptions{cfg: TeamConfig{KeepForkToolChoice: keep}, master: queued(textResp("answer"), textResp("the summary"))})
		activate(t, tf.tp, foreground(1, "question"), nil)
		snap, err = tf.tp.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tf.tp.Fork(context.Background(), snap, thread.ForkSpec{Prompt: "Summarize."}); err != nil {
			t.Fatal(err)
		}
		check("team", keep, tf.master)
	}
}

// TestAgentParticipantSavesTheFinishedReply covers the save after an
// activation: the agent checkpoints after tool steps but not after a final
// answer, so a host that restores it later would lose the reply.
func TestAgentParticipantSavesTheFinishedReply(t *testing.T) {
	saver := newMemSaver()
	f := newFixture(t, fixtureOptions{
		agent:   []agent.AgentOption{agent.WithStateSaver(saver), agent.WithState(&agent.AgentState{SessionID: "edge-session"})},
		respond: steps(textResp("FINAL-ANSWER")),
	})
	activate(t, f.p, foreground(3, "question"), nil)
	st, err := saver.LoadState(context.Background(), "edge-session")
	if err != nil || st == nil {
		t.Fatalf("no state was saved: %v", err)
	}
	if st.ThreadCursor != 3 || len(st.Context) == 0 || !strings.Contains(renderMsgs(st.Context), "FINAL-ANSWER") {
		t.Fatalf("the saved state misses the finished reply: cursor %d\n%s", st.ThreadCursor, renderMsgs(st.Context))
	}

	// A save that fails after a final answer becomes the activation's error.
	failing := &failSaver{memSaver: newMemSaver()}
	failing.fail.Store(true)
	g := newFixture(t, fixtureOptions{
		agent:   []agent.AgentOption{agent.WithStateSaver(failing)},
		respond: steps(textResp("FINAL-ANSWER")),
	})
	if _, end := activate(t, g.p, foreground(1, "question"), nil); end.Err == nil || !strings.Contains(end.Err.Error(), "disk full") {
		t.Fatalf("a failed save must be reported: %+v", end)
	}
}

// TestTeamStateBeforeTheFirstMasterComesFromLoad covers a team participant
// built over a persisted master: before its first activation State reports
// the loaded cursor and an input estimate, queued updates add to it,
// and a factory that does not restore the loaded state fails instead of
// losing entries.
func TestTeamStateBeforeTheFirstMasterComesFromLoad(t *testing.T) {
	ctx := context.Background()
	persisted := &agent.AgentState{SessionID: "master-session", ThreadCursor: 9,
		Summary: strings.Repeat("s", 400), Context: []*message.Msg{message.UserMsg("user", "EARLIER-QUESTION "+strings.Repeat("q", 383))}}
	load := func(context.Context) (*agent.AgentState, error) { return persisted, nil }

	f := newTeamFixture(t, teamOptions{cfg: TeamConfig{Load: load}, master: queued(textResp("restored answer"))})
	if st := f.tp.State(); st.Cursor != 9 || st.InputTokens != 200 || st.Busy || st.Window != 0 {
		t.Fatalf("state before the first master = %+v", st)
	}
	if err := f.tp.Sync(ctx, thread.Sync{Cursor: 5}); err == nil {
		t.Fatal("a queued update may not move the loaded cursor back")
	}
	if err := f.tp.Sync(ctx, thread.Sync{Messages: []*message.Msg{projected(10, "QUEUED-NOTE")}, Cursor: 10}); err != nil {
		t.Fatal(err)
	}
	if st := f.tp.State(); st.Cursor != 10 || st.InputTokens <= 200 {
		t.Fatalf("state with a queued update = %+v", st)
	}
	if err := f.saver.SaveState(ctx, "master-session", persisted); err != nil {
		t.Fatal(err)
	}
	if _, end := activate(t, f.tp, foreground(11, "hi"), nil); end.Err != nil {
		t.Fatalf("end: %+v", end)
	}
	in := f.master.input(0)
	if strings.Count(in, "EARLIER-QUESTION") != 1 || strings.Count(in, "QUEUED-NOTE") != 1 {
		t.Fatalf("the first master must hold the restored context and the queued update once:\n%s", in)
	}

	// The factory builds a fresh master although Load returned a state.
	g := newTeamFixture(t, teamOptions{cfg: TeamConfig{Load: load}, master: queued(textResp("unused"))})
	if ch, err := g.tp.Activate(t.Context(), foreground(11, "hi")); ch != nil || err == nil ||
		!strings.Contains(err.Error(), "must restore that state") || g.master.calls() != 0 {
		t.Fatalf("a master that did not restore the loaded state must not run: %v", err)
	}

	master := func(context.Context, tool.Tool, []tool.Tool, []agent.AgentOption) (*agent.UnifiedAgent, error) {
		return nil, errors.New("unused")
	}
	members := []pipeline.TeamMember{{Name: "researcher", Description: "unused", Factory: func(context.Context) (*agent.UnifiedAgent, error) {
		return nil, errors.New("unused")
	}}}
	if _, err := NewTeamParticipant(TeamConfig{Descriptor: cloudDesc, Master: master, Members: members, Load: func(context.Context) (*agent.AgentState, error) {
		return nil, errors.New("disk gone")
	}}); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("a Load error: %v", err)
	}
	if _, err := NewTeamParticipant(TeamConfig{Descriptor: cloudDesc, Master: master, Members: members, Load: func(context.Context) (*agent.AgentState, error) {
		return &agent.AgentState{SchemaVersion: agent.StateSchemaVersion + 1}, nil
	}}); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("a newer schema: %v", err)
	}
	tp, err := NewTeamParticipant(TeamConfig{Descriptor: cloudDesc, Master: master, Members: members, Load: func(context.Context) (*agent.AgentState, error) {
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if st := tp.State(); st.Cursor != 0 || st.InputTokens != 0 {
		t.Fatalf("no persisted state is a new session: %+v", st)
	}
}

// TestRepeatedCallIDAfterInterruptedWrite: providers may reuse tool-call IDs
// across replies (Gemini numbers them per response). The answer to a write
// whose reply was interrupted must not answer the next reply's call with the
// same ID, and a reply that was not interrupted gets no interrupted-write
// note.
func TestRepeatedCallIDAfterInterruptedWrite(t *testing.T) {
	const id = "gemini-call-report-0"
	f := newFixture(t, fixtureOptions{respond: steps(
		resp("", call(id, ToolReport, map[string]any{"text": "first"})),
		resp("", call(id, ToolReport, map[string]any{"text": "second"})),
		textResp("second done"),
		resp("", call(id, ToolReport, map[string]any{"text": "third"})),
		textResp("third done"),
	)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setAfterWrite(f.host, func(context.Context, uint64) {
		cancel()
		waitIdle(t, f.a)
	})
	ch, err := f.p.Activate(ctx, foreground(1, "report"))
	if err != nil {
		t.Fatal(err)
	}
	if _, end := drain(t, ch, nil); end.Reason != types.ReplyInterrupted {
		t.Fatalf("end: %+v", end)
	}
	setAfterWrite(f.host, nil)

	if _, end := activate(t, f.p, foreground(2, "again"), nil); end.Err != nil || end.Text != "second done" {
		t.Fatalf("second end: %+v", end)
	}
	second := f.m.input(2)
	if !strings.Contains(second, "Reported (entry #102).") || strings.Contains(second, "Reported (entry #101).") {
		t.Fatalf("the second report call must hear its own entry, not the interrupted one's:\n%s", second)
	}
	if _, end := activate(t, f.p, foreground(3, "once more"), nil); end.Err != nil || end.Text != "third done" {
		t.Fatalf("third end: %+v", end)
	}
	third := f.m.input(4)
	if strings.Contains(third, "entry #102 before the reply was interrupted") || !strings.Contains(third, "Reported (entry #103).") {
		t.Fatalf("a reply that was not interrupted gets no note, and the third call hears #103:\n%s", third)
	}
}

// TestAwaitIdleReturnsOtherErrorsAtOnce: only a reply still in flight is
// waited for; a failing save is returned at once instead of costing the
// whole wait.
func TestAwaitIdleReturnsOtherErrorsAtOnce(t *testing.T) {
	s := &failSaver{memSaver: newMemSaver()}
	s.fail.Store(true)
	a := agent.NewUnifiedAgent("a", "sys", &scriptModel{}, agent.WithStateSaver(s))
	start := time.Now()
	err := awaitIdle(t.Context(), a)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("awaitIdle: %v", err)
	}
	if d := time.Since(start); d > idleWait/2 {
		t.Fatalf("a failing save was retried for %v", d)
	}
}

// TestForkGuardRunsOnSummaries: the host's guardrail sees every summary a
// fork writes, on both participant kinds, and an error fails the fork.
func TestForkGuardRunsOnSummaries(t *testing.T) {
	var seen []string
	guard := func(_ context.Context, text string) error {
		seen = append(seen, text)
		if strings.Contains(text, "BLOCKED") {
			return errors.New("guardrail: blocked")
		}
		return nil
	}
	f := newFixture(t, fixtureOptions{cfg: AgentConfig{ForkGuard: guard}, respond: func(_ context.Context, n int, _ *model.CallOptions) (*model.ChatResponse, error) {
		if n == 0 {
			return textResp("answer"), nil
		}
		return textResp("BLOCKED summary"), nil
	}})
	activate(t, f.p, foreground(1, "question"), nil)
	snap, err := f.p.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Fork(context.Background(), snap, thread.ForkSpec{Prompt: "Summarize."}); err == nil {
		t.Fatal("a summary the guardrail rejects must fail the fork")
	}

	tf := newTeamFixture(t, teamOptions{cfg: TeamConfig{ForkGuard: guard}, master: queued(textResp("answer"), textResp("the summary"))})
	activate(t, tf.tp, foreground(1, "question"), nil)
	if snap, err = tf.tp.Snapshot(); err != nil {
		t.Fatal(err)
	}
	out, err := tf.tp.Fork(context.Background(), snap, thread.ForkSpec{Prompt: "Summarize."})
	if err != nil || out.Text == "" {
		t.Fatalf("team fork = %+v, %v", out, err)
	}
	if len(seen) != 2 || seen[0] != "BLOCKED summary" || seen[1] != out.Text {
		t.Fatalf("guard saw %q", seen)
	}
}
