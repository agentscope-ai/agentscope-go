package participant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/pipeline"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// MasterFactory builds the master agent for one activation. It must install
// assign and the thread tools with
// pipeline.NewTeamLeaderToolkit(assign, append(threadTools, own...)...),
// apply every option in opts after its own, give the agent a
// PermissionContext and a StateSaver, and restore the master's persisted
// state (agent.LoadCheckpoint plus agent.WithState) so every activation
// continues the same master context. Thread updates applied between
// activations reach the next master only through that checkpoint.
type MasterFactory func(ctx context.Context, assign tool.Tool, threadTools []tool.Tool, opts []agent.AgentOption) (*agent.UnifiedAgent, error)

// TeamConfig configures a TeamParticipant.
type TeamConfig struct {
	Descriptor thread.Descriptor
	// Master builds the master agent for each activation.
	Master MasterFactory
	// Members are the sub-agents. They never see the thread and must not be
	// given thread tools; their text never becomes a reply entry.
	Members []pipeline.TeamMember
	// CaptureOutputs and MaxEventOutput work as in AgentConfig and apply to
	// member tool results too.
	CaptureOutputs *bool
	MaxEventOutput int
	// Prepare selects the master's thread tools and extra loop hooks, with
	// Prepare's defaults.
	Prepare PrepareOptions
	// Load returns the master's persisted state, the state the Master factory
	// restores, or nil when there is none yet. NewTeamParticipant calls it
	// once, with a background context (bound a slow store inside Load), so
	// that before the first activation State reports the persisted
	// cursor and an estimate of the input instead of an empty master:
	// the Coordinator syncs and recovers from that state, and a participant
	// rebuilt after a restart must not receive the entries its master already
	// holds a second time. Without Load the participant is for a new session
	// only: State reports cursor 0 until the first activation, and a factory
	// that restores an older master would get those entries again.
	Load func(ctx context.Context) (*agent.AgentState, error)
	// KeepForkToolChoice and ForkGuard work as in AgentConfig, for the
	// master's forks.
	KeepForkToolChoice bool
	ForkGuard          func(ctx context.Context, text string) error
}

// TeamParticipant is a master agent with sub-agents run through
// pipeline.TeamPipeline, one run per activation. Sync, Compact and the
// Forker methods work on the last master instance between activations;
// before the first activation updates are queued and applied to the first
// master right after it is built.
type TeamParticipant struct {
	c        *core
	cfg      TeamConfig
	pipeline *pipeline.TeamPipeline

	mu     sync.Mutex
	master *agent.UnifiedAgent
	// restored is the cursor of the state TeamConfig.Load returned; the
	// first master must hold at least that much.
	restored     uint64
	queued       []agent.ThreadUpdate
	queuedCursor uint64
	queuedTokens int // estimated input before the first master: the loaded state plus the queued updates
	actSync      *thread.Sync
	// syncApplied is set once the leader applied actSync (or it was empty).
	syncApplied bool
	run         *pipeline.TeamRun
	confirmReqs map[string]string // agent path -> pending confirmation request ID

	// offerMu orders own-summary offers with a master's build. While
	// building is set, an offer waits in pendingOffer and goes to the new
	// master once it is published, so it never lands on the master being
	// replaced.
	offerMu      sync.Mutex
	building     bool
	pendingOffer *ownOffer
}

// ownOffer is an own-summary offer that arrived while a master was built.
type ownOffer struct {
	snap thread.Snapshot
	text string
}

var (
	_ thread.Participant = (*TeamParticipant)(nil)
	_ thread.Forker      = (*TeamParticipant)(nil)
	_ thread.Releaser    = (*TeamParticipant)(nil)
)

// NewTeamParticipant builds a team participant. With TeamConfig.Load it
// reads the master's persisted state once, here.
func NewTeamParticipant(cfg TeamConfig) (*TeamParticipant, error) { //nolint:gocritic // the value parameter keeps the documented API
	if cfg.Master == nil {
		return nil, errors.New("participant: TeamConfig.Master is required")
	}
	c, err := newCore(cfg.Descriptor, cfg.Prepare, true)
	if err != nil {
		return nil, err
	}
	c.configure(cfg.CaptureOutputs, cfg.MaxEventOutput)
	tp := &TeamParticipant{c: c, cfg: cfg, confirmReqs: map[string]string{}}
	if cfg.Load != nil {
		st, err := cfg.Load(context.Background())
		if err != nil {
			return nil, fmt.Errorf("participant %s: load the master's state: %w", c.desc.Name, err)
		}
		if st != nil {
			if st.SchemaVersion > agent.StateSchemaVersion {
				return nil, fmt.Errorf("participant %s: the master's state has schema version %d, newer than the supported %d",
					c.desc.Name, st.SchemaVersion, agent.StateSchemaVersion)
			}
			tp.restored, tp.queuedCursor = st.ThreadCursor, st.ThreadCursor
			tp.queuedTokens = stateTokens(st)
		}
	}
	p, err := pipeline.NewTeamPipeline(tp.leader, cfg.Members...)
	if err != nil {
		return nil, err
	}
	tp.pipeline = p
	return tp, nil
}

// Tools returns the master's thread tools, the ones passed to the factory.
func (tp *TeamParticipant) Tools() []tool.Tool { return append([]tool.Tool(nil), tp.c.tools...) }

// leader wraps the master factory for the pipeline. It applies the queued
// updates in order, then the activation's own Sync, before the run starts.
func (tp *TeamParticipant) leader(ctx context.Context, assign tool.Tool) (master *agent.UnifiedAgent, err error) {
	tp.offerMu.Lock() // waits for an offer still being made to the last master
	tp.mu.Lock()
	tp.building = true
	tp.mu.Unlock()
	tp.offerMu.Unlock()
	defer func() {
		if err == nil {
			return
		}
		// No new master: a waiting offer goes to the last one, as it would
		// have without the build.
		tp.offerMu.Lock()
		defer tp.offerMu.Unlock()
		tp.mu.Lock()
		tp.building = false
		last, pending := tp.master, tp.pendingOffer
		tp.pendingOffer = nil
		tp.mu.Unlock()
		switch {
		case pending == nil:
		case last == nil:
			agentscope.Log().WithField("participant", tp.c.desc.Name).
				Info("participant: own notes offered while the first master build failed were dropped: no master has run yet")
		default:
			if oerr := offerOn(ctx, last, pending.snap, pending.text, true); oerr != nil {
				agentscope.Log().WithError(oerr).WithField("participant", tp.c.desc.Name).
					Info("participant: own notes offered while a master build failed were not installed")
			}
		}
	}()
	a, err := tp.cfg.Master(ctx, assign, tp.Tools(), tp.c.options())
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("participant: the master factory returned no agent")
	}
	if a.PermissionEngine() == nil {
		return nil, errors.New("participant: the master needs a PermissionContext")
	}
	tp.mu.Lock()
	first, restored := tp.master == nil, tp.restored
	tp.mu.Unlock()
	if cur := a.ThreadView().Cursor; first && cur < restored {
		// State reported the loaded cursor, so the Coordinator synced only
		// what follows it; this master would miss the entries in between.
		return nil, fmt.Errorf("participant %s: the master factory built a master at thread cursor %d, behind the cursor %d "+
			"of the state TeamConfig.Load returned; it must restore that state", tp.c.desc.Name, cur, restored)
	}
	for {
		tp.mu.Lock()
		if len(tp.queued) == 0 {
			tp.mu.Unlock()
			break
		}
		u := tp.queued[0]
		tp.mu.Unlock()
		if err := a.ApplyThreadUpdate(ctx, u); err != nil {
			return nil, fmt.Errorf("participant %s: apply a queued thread update: %w", tp.c.desc.Name, err)
		}
		tp.mu.Lock()
		tp.queued = tp.queued[1:]
		tp.mu.Unlock()
	}
	tp.mu.Lock()
	s := tp.actSync
	tp.mu.Unlock()
	if !s.Empty() {
		u, err := toUpdate(s)
		if err == nil {
			err = a.ApplyThreadUpdate(ctx, u)
		}
		if err != nil {
			return nil, fmt.Errorf("participant %s: apply activation sync: %w", tp.c.desc.Name, err)
		}
	}
	tp.mu.Lock()
	tp.syncApplied = true
	tp.mu.Unlock()
	if act := tp.c.current(); act != nil {
		act.start(a.ThreadView())
	}
	tp.offerMu.Lock()
	defer tp.offerMu.Unlock()
	tp.mu.Lock()
	tp.master, tp.building = a, false
	pending := tp.pendingOffer
	tp.pendingOffer = nil
	tp.mu.Unlock()
	if pending != nil {
		if err := offerOn(ctx, a, pending.snap, pending.text, true); err != nil {
			agentscope.Log().WithError(err).WithField("participant", tp.c.desc.Name).
				Info("participant: own notes offered while the master was built were not installed")
		}
	}
	return a, nil
}

func (tp *TeamParticipant) currentMaster() *agent.UnifiedAgent {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.master
}

// Descriptor returns the participant's descriptor.
func (tp *TeamParticipant) Descriptor() thread.Descriptor { return tp.c.desc }

// Bind stores the host. A second call is an error until the host is
// released.
func (tp *TeamParticipant) Bind(h thread.Host) error { return tp.c.bind(h) }

// Release forgets h when it is the bound host; the Coordinator calls it on
// Close so a later Coordinator can bind the participant again.
func (tp *TeamParticipant) Release(h thread.Host) { tp.c.release(h) }

// State reports the master's thread view. Before the first activation it
// reports the state TeamConfig.Load returned with the queued updates on top:
// their cursor, and a rough estimate of the input (about four
// characters per token, without the system prompt and tool schemas). The
// window is unknown (0) until a master exists.
func (tp *TeamParticipant) State() thread.ParticipantState {
	busy := tp.c.busy()
	tp.mu.Lock()
	m, cursor, tokens := tp.master, tp.queuedCursor, tp.queuedTokens
	tp.mu.Unlock()
	if m == nil {
		return thread.ParticipantState{Cursor: cursor, InputTokens: tokens, Busy: busy}
	}
	v := m.ThreadView()
	return thread.ParticipantState{Cursor: v.Cursor, InputTokens: v.InputTokens, Window: v.Window, SummaryTokens: v.SummaryTokens,
		CompactAt: v.CompactAt, Busy: busy}
}

// Sync applies s to the last master between activations, or queues it
// before the first activation.
func (tp *TeamParticipant) Sync(ctx context.Context, s thread.Sync) error { //nolint:gocritic // the value parameter is the thread.Participant contract
	if err := tp.c.beginOp(); err != nil {
		return err
	}
	defer tp.c.endOp()
	u, err := toUpdate(&s)
	if err != nil {
		return fmt.Errorf("participant %s: %w", tp.c.desc.Name, err)
	}
	if m := tp.currentMaster(); m != nil {
		return m.ApplyThreadUpdate(ctx, u)
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	// The agent's cursor rule: it never moves back.
	if u.Cursor != 0 && u.Cursor < tp.queuedCursor {
		return fmt.Errorf("participant %s: thread update cursor %d is behind the queued cursor %d", tp.c.desc.Name, u.Cursor, tp.queuedCursor)
	}
	tp.queued = append(tp.queued, u)
	if u.Cursor != 0 {
		tp.queuedCursor = u.Cursor
	}
	if u.Reset {
		tp.queuedTokens = 0
	}
	tp.queuedTokens += updateTokens(&u)
	return nil
}

// Compact strictly compacts the last master's context. Before the first
// activation there is nothing to compact. The next activation builds a new
// master, so the compaction usage the old one keeps pending would never be
// reported: a compaction that changed the context is recorded as one
// thread.UsageCompaction call of unknown usage. CompactStrict saves the
// compacted state through the master's StateSaver, so the next master starts
// from it.
func (tp *TeamParticipant) Compact(ctx context.Context, prompt string) (bool, error) {
	if err := tp.c.beginOp(); err != nil {
		return false, err
	}
	defer tp.c.endOp()
	m := tp.currentMaster()
	if m == nil {
		return false, nil
	}
	changed, err := m.CompactStrict(ctx, agent.CompactStrictOptions{Prompt: prompt})
	if changed {
		if h := tp.c.hostOf(); h != nil {
			h.RecordUsage(thread.LedgerEntry{Participant: tp.c.desc.Name, Kind: thread.UsageCompaction})
		}
	}
	return changed, err
}

// Activate runs one team activation: a TeamPipeline run whose leader is the
// master built for it.
func (tp *TeamParticipant) Activate(ctx context.Context, act thread.Activation) (<-chan thread.ActivationEvent, error) { //nolint:gocritic // the value parameter is the thread.Participant contract
	if ctx == nil {
		return nil, errors.New("participant: nil context")
	}
	a := newActivation(&act)
	host, err := tp.c.beginActivation(a)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.ctx = runCtx
	startCtx := runCtx
	if act.Trigger.Seq > 0 {
		// The mark reaches the master's reply input. Members' replies share
		// the context, so their own input messages carry it too; members
		// never receive thread updates, so it changes nothing for them.
		startCtx = agent.WithThreadInput(runCtx, act.Trigger.Seq)
	}
	tp.mu.Lock()
	tp.actSync = act.Sync
	tp.syncApplied = false
	tp.confirmReqs = map[string]string{}
	tp.mu.Unlock()
	run, err := tp.pipeline.Start(startCtx, act.Input)
	tp.mu.Lock()
	tp.actSync = nil
	tp.run = run
	master, applied := tp.master, tp.syncApplied
	tp.mu.Unlock()
	if err != nil {
		cancel()
		tp.c.endActivation()
		err = fmt.Errorf("participant %s: start team run: %w", tp.c.desc.Name, err)
		if !applied {
			// The run never reached the leader's Sync: nothing ran and the
			// Sync is not applied, which only an Activate error says.
			return nil, err
		}
		return endNow(types.ReplyError, err), nil
	}

	out := make(chan thread.ActivationEvent, eventBuffer)
	snd := &sender{ctx: ctx, out: out}
	t := newTracker(tp.c, a, host)
	t.sent = func() int { return snd.sent }
	t.team, t.mainName = true, master.Name()
	t.markHandedOff = master.MarkHandedOff
	t.submit = func(e *event.RequireExternalExecutionEvent, results []message.ToolResultBlock) {
		// The run queues the answer without blocking and fails once it is
		// closing; an answer the master did not record reaches it as a note
		// when the activation ends (settle).
		_ = run.SubmitExternalResult(e.GetEventID(), &event.ExternalExecutionResultEvent{ReplyID: e.ReplyID, ExecutionResults: results})
	}
	t.onConfirm = func(path string, e *event.RequireUserConfirmEvent) {
		tp.mu.Lock()
		tp.confirmReqs[path] = e.GetEventID()
		tp.mu.Unlock()
	}
	go func() {
		for ev := range run.Events() {
			if forward, path := t.handle(ev); forward {
				snd.send(thread.ActivationEvent{Path: path, Event: ev})
			}
		}
		waitErr := run.Wait()
		t.finishAll()
		cancel()
		end := t.end(ctx.Err(), waitErr)
		tp.settle(ctx, master, t, end)
		tp.mu.Lock()
		tp.run = nil
		tp.confirmReqs = map[string]string{}
		tp.mu.Unlock()
		tp.c.endActivation()
		snd.end(end)
	}()
	return out, nil
}

// settle brings the master in line with what the run did once it ended, like
// AgentParticipant.settle (a note for each writing call whose answer it did
// not record), and saves its state. The next
// activation builds a new master from the checkpoint, and the agent saves one
// after each tool step but not after its final answer, so the finished state
// is saved now, also after a cancellation.
func (tp *TeamParticipant) settle(ctx context.Context, master *agent.UnifiedAgent, t *tracker, end *thread.ActivationEnd) {
	ctx = context.WithoutCancel(ctx)
	u := agent.ThreadUpdate{Messages: t.notes()}
	var err error
	if len(u.Messages) > 0 {
		// The run joined the master's reply core, but after a cancellation it
		// stops reading the master's events, and the goroutine that ends the
		// event stream may still be finishing: the agent refuses updates
		// until it has.
		if err = awaitIdle(ctx, master); err == nil {
			err = master.ApplyThreadUpdate(ctx, u) // it saves the state too
		}
	} else {
		err = master.SaveCheckpoint(ctx)
	}
	if err != nil {
		reportSettle(tp.c.desc.Name, end, fmt.Errorf("participant %s: update and save the master after the run: %w", tp.c.desc.Name, err))
	}
}

// reportSettle logs err and makes it the activation's error when it has
// none. The Coordinator reports an end's error only for a failed end, so
// after a normal end the log is where a failed settle shows.
func reportSettle(name string, end *thread.ActivationEnd, err error) {
	agentscope.Log().WithError(err).WithField("participant", name).Warn("participant: settling the agent after an activation")
	if end.Err == nil {
		end.Err = err
	}
}

// idleWait bounds how long awaitIdle waits.
const idleWait = 2 * time.Second

// awaitIdle waits until a accepts a between-replies update. The probe is an
// empty update: it changes nothing and only saves the state again. Only a
// reply still in flight is waited for; any other error (a failed save)
// returns at once.
func awaitIdle(ctx context.Context, a *agent.UnifiedAgent) error {
	deadline := time.Now().Add(idleWait)
	for {
		err := a.ApplyThreadUpdate(ctx, agent.ThreadUpdate{})
		if err == nil || !errors.Is(err, agent.ErrReplyInProgress) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

// SubmitUserConfirm answers the pending confirmation request of the agent at
// path ("" for the master, the member name for a member). The run queues the
// answer without blocking.
func (tp *TeamParticipant) SubmitUserConfirm(path string, result *event.UserConfirmResultEvent) error {
	if result == nil {
		return errors.New("participant: nil confirmation result")
	}
	tp.mu.Lock()
	run, id := tp.run, tp.confirmReqs[path]
	tp.mu.Unlock()
	if run == nil {
		return fmt.Errorf("participant %s: no activation is running", tp.c.desc.Name)
	}
	if id == "" {
		return fmt.Errorf("participant %s: no confirmation is pending for %q", tp.c.desc.Name, path)
	}
	if err := run.SubmitUserConfirm(id, result); err != nil {
		return err
	}
	tp.mu.Lock()
	if tp.confirmReqs[path] == id {
		delete(tp.confirmReqs, path)
	}
	tp.mu.Unlock()
	return nil
}

// Snapshot returns a fork snapshot of the last master instance.
func (tp *TeamParticipant) Snapshot() (thread.Snapshot, error) {
	m := tp.currentMaster()
	if m == nil {
		return nil, fmt.Errorf("%w: no master has run yet", thread.ErrNoSnapshot)
	}
	return snapshotOf(m)
}

// Fork asks the master's model for a summary of snap. Its usage is returned,
// not recorded.
func (tp *TeamParticipant) Fork(ctx context.Context, snap thread.Snapshot, spec thread.ForkSpec) (thread.ForkOutput, error) {
	m := tp.currentMaster()
	if m == nil {
		return thread.ForkOutput{}, fmt.Errorf("%w: no master has run yet", thread.ErrNoSnapshot)
	}
	return forkOn(ctx, m, snap, &spec, tp.cfg.KeepForkToolChoice, tp.cfg.ForkGuard)
}

// stateTokens estimates the input a master restored from st would send.
func stateTokens(st *agent.AgentState) int {
	n := runes(st.Summary) + msgRunes(st.Context)
	if st.PeerSummary != nil {
		n += runes(st.PeerSummary.Text)
	}
	return (n + 3) / 4
}

// updateTokens estimates the input u adds.
func updateTokens(u *agent.ThreadUpdate) int {
	n := msgRunes(u.Messages)
	if u.PeerSummary != nil {
		n += runes(u.PeerSummary.Text)
	}
	return (n + 3) / 4
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// msgRunes counts the characters of the text, tool calls and tool results
// of msgs.
func msgRunes(msgs []*message.Msg) int {
	n := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, b := range m.Content {
			switch v := b.(type) {
			case message.TextBlock:
				n += runes(v.Text)
			case message.ToolCallBlock:
				n += runes(v.Name) + runes(v.Input)
			case message.ToolResultBlock:
				n += runes(v.GetOutputText())
			case message.ThinkingBlock:
				n += runes(v.Thinking)
			}
		}
	}
	return n
}

// OfferOwnSummary hands the master a summary of its own context to install
// at its compaction line (agent.OfferSummary). The offer is saved with the
// master's state, so the master the next activation builds from that state
// still holds it. A snapshot an earlier master instance took is offered to
// the current one, which is built from the same checkpoint and matches it
// by message ID, so notes forked while the next activation started are not
// lost. An offer that arrives while the next master is built returns nil
// without waiting for the build: it is made to that master once it is built
// (a newer waiting offer replaces an older one, and one the first build's
// failure leaves without a master is dropped), and a failure is logged.
// Offers are made one at a time, so a call can still wait while another
// offer is saved with the master's state.
func (tp *TeamParticipant) OfferOwnSummary(ctx context.Context, snap thread.Snapshot, text string) error {
	tp.offerMu.Lock()
	defer tp.offerMu.Unlock()
	tp.mu.Lock()
	if tp.building {
		tp.pendingOffer = &ownOffer{snap: snap, text: text}
		tp.mu.Unlock()
		return nil
	}
	m := tp.master
	tp.mu.Unlock()
	if m == nil {
		return fmt.Errorf("%w: no master has run yet", thread.ErrStale)
	}
	return offerOn(ctx, m, snap, text, true)
}
