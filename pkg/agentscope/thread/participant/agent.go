package participant

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/loop"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/thread"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// AgentConfig configures an AgentParticipant.
type AgentConfig struct {
	Descriptor thread.Descriptor
	// Agent is the participant's agent. For Prepared.Participant it must be
	// built with Prepared.Options() and with Prepared.Tools() in its toolkit,
	// and with a PermissionContext when a writing thread tool is installed.
	// The participant owns its reply lifecycle from then on.
	Agent *agent.UnifiedAgent
	// CaptureOutputs records each tool result into the host's event list.
	// The default is false: the event list is opt-in (set this on the cloud
	// participant and add event_read to the edge's PrepareOptions.Tools).
	CaptureOutputs *bool
	// MaxEventOutput caps one recorded tool output, in bytes (default 64 KiB).
	MaxEventOutput int
	// KeepForkToolChoice makes a fork send the main request's own tool choice
	// instead of "none" (agent.ForkRequest.KeepToolChoice). Set it for a
	// provider that drops cached message blocks when the tool choice changes,
	// so a fork reuses the main thread's prompt cache.
	KeepForkToolChoice bool
	// ForkGuard runs the host's guardrail rules on every summary a fork
	// writes, before the Coordinator appends or applies it: a fork does not
	// go through the reply loop, so its middleware does not see the text. An
	// error fails the fork (agent.ForkRequest.Guard).
	ForkGuard func(ctx context.Context, text string) error
}

// PrepareOptions selects what Prepare installs.
type PrepareOptions struct {
	// Tools selects the thread tools to install. Default: all of them but
	// event_read (opt-in, see AgentConfig.CaptureOutputs) for the cloud zone,
	// and also but publish_artifact for the edge.
	Tools []string
	// LoopHooks are the host's own loop hooks. agent.WithLoopHooks replaces
	// earlier hooks, so pass them here instead of with a separate option.
	LoopHooks []loop.Hook
}

// Prepared holds the options and tools an agent for one participant must be
// built with. Call Prepare before constructing the agent, then Participant
// once the agent exists.
type Prepared struct {
	c     *core
	mu    sync.Mutex
	bound bool
}

// Prepare returns the options and thread tools an agent for the participant
// d must be built with.
func Prepare(d thread.Descriptor, opts PrepareOptions) (*Prepared, error) { //nolint:gocritic // the value parameter keeps the documented API
	c, err := newCore(d, opts, true)
	if err != nil {
		return nil, err
	}
	return &Prepared{c: c}, nil
}

// Options returns the agent options: boundary input (WithBoundaryInput), the
// participant's loop hook with PrepareOptions.LoopHooks (WithLoopHooks),
// WithExclusiveTools("transfer") and, for the cloud zone, WithForkCapture. Apply them after any option they would otherwise replace.
func (p *Prepared) Options() []agent.AgentOption { return p.c.options() }

// Tools returns the thread tools to install in the agent's toolkit.
func (p *Prepared) Tools() []tool.Tool { return append([]tool.Tool(nil), p.c.tools...) }

// Participant binds the agent built with Options and Tools to a new
// participant. It may be called once.
func (p *Prepared) Participant(cfg AgentConfig) (*AgentParticipant, error) { //nolint:gocritic // the value parameter keeps the documented API
	if cfg.Descriptor.Name != p.c.desc.Name || cfg.Descriptor.Zone != p.c.desc.Zone {
		return nil, fmt.Errorf("participant: config describes %s (%s) but the agent was prepared for %s (%s)",
			cfg.Descriptor.Name, cfg.Descriptor.Zone, p.c.desc.Name, p.c.desc.Zone)
	}
	if cfg.Agent == nil {
		return nil, errors.New("participant: AgentConfig.Agent is required")
	}
	if cfg.Agent.PermissionEngine() == nil && p.c.hasWriteTools() {
		return nil, fmt.Errorf("participant %s: the agent needs a PermissionContext: the writing thread tools "+
			"are answered through its external-result channel", p.c.desc.Name)
	}
	if err := cfg.Descriptor.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bound {
		return nil, fmt.Errorf("participant %s: Prepared.Participant was already called", p.c.desc.Name)
	}
	p.bound = true
	p.c.desc = cfg.Descriptor
	return newAgentParticipant(p.c, &cfg)
}

// NewAgentParticipant wraps an agent built without Prepare: it has no thread
// tools and no boundary input, so entries that arrive during an activation
// wait for the next one.
func NewAgentParticipant(cfg AgentConfig) (*AgentParticipant, error) { //nolint:gocritic // the value parameter keeps the documented API
	c, err := newCore(cfg.Descriptor, PrepareOptions{}, false)
	if err != nil {
		return nil, err
	}
	return newAgentParticipant(c, &cfg)
}

func newAgentParticipant(c *core, cfg *AgentConfig) (*AgentParticipant, error) {
	if cfg.Agent == nil {
		return nil, errors.New("participant: AgentConfig.Agent is required")
	}
	c.configure(cfg.CaptureOutputs, cfg.MaxEventOutput)
	return &AgentParticipant{c: c, agent: cfg.Agent, keepToolChoice: cfg.KeepForkToolChoice, forkGuard: cfg.ForkGuard}, nil
}

// AgentParticipant wraps one UnifiedAgent as a thread participant. It
// implements thread.Participant and thread.Forker; Snapshot needs fork
// capture, which Prepare installs for the cloud zone only, so an edge
// participant reports thread.ErrNoSnapshot.
type AgentParticipant struct {
	c              *core
	agent          *agent.UnifiedAgent
	keepToolChoice bool
	forkGuard      func(ctx context.Context, text string) error
}

var (
	_ thread.Participant = (*AgentParticipant)(nil)
	_ thread.Forker      = (*AgentParticipant)(nil)
	_ thread.Releaser    = (*AgentParticipant)(nil)
)

// Descriptor returns the participant's descriptor.
func (p *AgentParticipant) Descriptor() thread.Descriptor { return p.c.desc }

// Bind stores the host. A second call is an error until the host is
// released.
func (p *AgentParticipant) Bind(h thread.Host) error { return p.c.bind(h) }

// Release forgets h when it is the bound host; the Coordinator calls it on
// Close so a later Coordinator can bind the participant again.
func (p *AgentParticipant) Release(h thread.Host) { p.c.release(h) }

// State reports the agent's thread view plus whether an activation runs. The
// agent is the host's: when the host restores it from a checkpoint before
// binding the participant, the view already holds the restored cursor.
func (p *AgentParticipant) State() thread.ParticipantState {
	busy := p.c.busy()
	v := p.agent.ThreadView()
	return thread.ParticipantState{Cursor: v.Cursor, InputTokens: v.InputTokens, Window: v.Window, SummaryTokens: v.SummaryTokens,
		CompactAt: v.CompactAt, Busy: busy}
}

// Sync applies s between activations; it refuses while busy.
func (p *AgentParticipant) Sync(ctx context.Context, s thread.Sync) error { //nolint:gocritic // the value parameter is the thread.Participant contract
	if err := p.c.beginOp(); err != nil {
		return err
	}
	defer p.c.endOp()
	u, err := toUpdate(&s)
	if err != nil {
		return fmt.Errorf("participant %s: %w", p.c.desc.Name, err)
	}
	return p.agent.ApplyThreadUpdate(ctx, u)
}

// Compact strictly compacts the agent's context between activations. The
// compaction call's usage is not recorded here: the agent keeps it pending
// and its next reply reports it as a model-call event without a call start,
// which the activation records as thread.UsageCompaction. The compactions
// since the last reply are reported together, as one entry, and a
// compaction whose provider reported no usage is not recorded.
func (p *AgentParticipant) Compact(ctx context.Context, prompt string) (bool, error) {
	if err := p.c.beginOp(); err != nil {
		return false, err
	}
	defer p.c.endOp()
	return p.agent.CompactStrict(ctx, agent.CompactStrictOptions{Prompt: prompt})
}

// Activate starts one reply. The returned channel carries every reply event,
// then exactly one End item, and closes; see sender for delivery on
// cancellation. When the activation's Sync cannot be applied (or its outcome
// is unknown, as when persisting it failed), Activate returns the error and
// runs nothing, so the Coordinator never takes the Sync for applied.
func (p *AgentParticipant) Activate(ctx context.Context, act thread.Activation) (<-chan thread.ActivationEvent, error) { //nolint:gocritic // the value parameter is the thread.Participant contract
	if ctx == nil {
		return nil, errors.New("participant: nil context")
	}
	a := newActivation(&act)
	host, err := p.c.beginActivation(a)
	if err != nil {
		return nil, err
	}
	if !act.Sync.Empty() {
		u, err := toUpdate(act.Sync)
		if err == nil {
			err = p.agent.ApplyThreadUpdate(ctx, u)
		}
		if err != nil {
			p.c.endActivation()
			return nil, fmt.Errorf("participant %s: apply activation sync: %w", p.c.desc.Name, err)
		}
	}
	a.start(p.agent.ThreadView())

	release, accepted := runctx.Start(ctx)
	if !accepted {
		p.c.endActivation()
		return endNow(types.ReplyError, fmt.Errorf("participant %s: the run scope is closed", p.c.desc.Name)), nil
	}
	replyCtx, cancel := context.WithCancel(ctx)
	replyCtx, scope := runctx.New(replyCtx)
	a.ctx = replyCtx
	rctx := replyCtx
	if act.Trigger.Seq > 0 {
		rctx = agent.WithThreadInput(replyCtx, act.Trigger.Seq)
	}
	stream, err := p.agent.ReplyStream(rctx, act.Input)
	if err != nil {
		cancel()
		scope.Close()
		release()
		p.c.endActivation()
		return endNow(types.ReplyError, fmt.Errorf("participant %s: start reply: %w", p.c.desc.Name, err)), nil
	}

	out := make(chan thread.ActivationEvent, eventBuffer)
	snd := &sender{ctx: ctx, out: out}
	t := newTracker(p.c, a, host)
	t.sent = func() int { return snd.sent }
	t.markHandedOff = p.agent.MarkHandedOff
	t.submit = func(e *event.RequireExternalExecutionEvent, results []message.ToolResultBlock) {
		// The answer is offered even after a cancellation, because a write
		// may have committed: an agent that still waits records it. The bound
		// keeps an agent that stopped waiting from holding the forwarder; an
		// answer the agent did not record reaches it as a note when the
		// activation ends (settle).
		sctx, done := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
		defer done()
		_ = p.agent.SubmitExternalResultContext(sctx, &event.ExternalExecutionResultEvent{ReplyID: e.ReplyID, ExecutionResults: results})
	}
	t.onConfirm = func(_ string, e *event.RequireUserConfirmEvent) { a.addConfirm(e) }
	go func() {
		for ev := range stream {
			if forward, path := t.handle(ev); forward {
				snd.send(thread.ActivationEvent{Path: path, Event: ev})
			}
		}
		t.finishAll()
		// The stream closing is not the reply core exiting: join it before
		// the agent counts as idle again.
		cancel()
		scope.Close()
		release()
		end := t.end(ctx.Err(), nil)
		p.settle(ctx, t, end)
		p.c.endActivation()
		snd.end(end)
	}()
	return out, nil
}

// settle brings the idle agent in line with what the activation did, before
// the next activation can start: a note for each writing call whose answer
// it did not record (tracker.notes). It always saves the agent's
// state, even with nothing to apply: the agent does not checkpoint after a
// final answer, and an agent the host restores later must hold the whole
// reply. ApplyThreadUpdate saves through the agent's StateSaver (a no-op
// without one); a failure is reported in end.
func (p *AgentParticipant) settle(ctx context.Context, t *tracker, end *thread.ActivationEnd) {
	u := agent.ThreadUpdate{Messages: t.notes()}
	if err := p.agent.ApplyThreadUpdate(context.WithoutCancel(ctx), u); err != nil {
		reportSettle(p.c.desc.Name, end, fmt.Errorf("participant %s: update the agent after the reply: %w", p.c.desc.Name, err))
	}
}

// SubmitUserConfirm answers a confirmation request of the running
// activation. The main agent's path is "". The answer must belong to a
// request the activation emitted and has not had answered: it names that
// request's reply ID (or none) and answers only that request's tool calls.
// Any other answer, such as one to a request of an interrupted activation
// or of a run before a restart, fails and leaves the requests pending.
func (p *AgentParticipant) SubmitUserConfirm(path string, result *event.UserConfirmResultEvent) error {
	if path != "" {
		return fmt.Errorf("participant %s: unknown agent path %q", p.c.desc.Name, path)
	}
	if result == nil {
		return errors.New("participant: nil confirmation result")
	}
	act := p.c.current()
	if act == nil {
		return fmt.Errorf("participant %s: no activation is running", p.c.desc.Name)
	}
	pending, err := act.takeConfirm(result)
	if err != nil {
		return fmt.Errorf("participant %s: %w", p.c.desc.Name, err)
	}
	// The agent takes the answer at once while it waits for it. The bound
	// keeps a stale answer in its channel from blocking the caller; the
	// confirmation stays pending when the answer was not delivered.
	ctx, cancel := context.WithTimeout(act.ctx, submitTimeout)
	defer cancel()
	if err := p.agent.SubmitUserConfirmContext(ctx, result); err != nil {
		act.restoreConfirm(pending)
		return fmt.Errorf("participant %s: submit the confirmation: %w", p.c.desc.Name, err)
	}
	return nil
}

// Snapshot returns the agent's fork snapshot and its thread cursor. The
// cursor is read just before the snapshot is taken: an update landing in
// between can only make the cursor older than the snapshot, so a summary
// built from it never claims entries the snapshot does not hold.
func (p *AgentParticipant) Snapshot() (thread.Snapshot, error) {
	return snapshotOf(p.agent)
}

// Fork asks the agent's model for a summary of snap. Its usage is returned,
// not recorded: the Coordinator records fork usage.
func (p *AgentParticipant) Fork(ctx context.Context, snap thread.Snapshot, spec thread.ForkSpec) (thread.ForkOutput, error) {
	return forkOn(ctx, p.agent, snap, &spec, p.keepToolChoice, p.forkGuard)
}

// OfferOwnSummary hands the agent text, a summary of its own context built
// from snap, to install at its compaction line (agent.OfferSummary). It may
// be called while an activation runs. A snapshot that no longer matches is
// thread.ErrStale.
func (p *AgentParticipant) OfferOwnSummary(ctx context.Context, snap thread.Snapshot, text string) error {
	return offerOn(ctx, p.agent, snap, text, false)
}

// forkSnapshot is the thread.Snapshot of an agent participant.
type forkSnapshot struct {
	agent  *agent.UnifiedAgent
	snap   *agent.ForkSnapshot
	cursor uint64
}

// Cursor is the participant's thread cursor when the snapshot was taken.
func (s *forkSnapshot) Cursor() uint64 { return s.cursor }

func snapshotOf(a *agent.UnifiedAgent) (thread.Snapshot, error) {
	cursor := a.ThreadView().Cursor
	snap, err := a.ForkSnapshot()
	if err != nil {
		if errors.Is(err, agent.ErrNoForkSnapshot) {
			return nil, fmt.Errorf("%w: %w", thread.ErrNoSnapshot, err)
		}
		return nil, err
	}
	return &forkSnapshot{agent: a, snap: snap, cursor: cursor}, nil
}

// ownSnapshot returns snap as a snapshot this participant took of a. A
// snapshot another agent instance took is returned together with an ErrStale
// error, for the callers that accept it.
func ownSnapshot(a *agent.UnifiedAgent, snap thread.Snapshot) (*forkSnapshot, error) {
	s, ok := snap.(*forkSnapshot)
	if !ok || s == nil || s.snap == nil {
		return nil, fmt.Errorf("%w: the snapshot was not taken by this participant", thread.ErrNoSnapshot)
	}
	if s.agent != a {
		return s, fmt.Errorf("%w: the snapshot belongs to another agent instance", thread.ErrStale)
	}
	return s, nil
}

func forkOn(ctx context.Context, a *agent.UnifiedAgent, snap thread.Snapshot, spec *thread.ForkSpec, keepToolChoice bool,
	guard func(context.Context, string) error) (thread.ForkOutput, error) {
	s, err := ownSnapshot(a, snap)
	if err != nil {
		return thread.ForkOutput{}, err
	}
	res, err := a.ForkSummary(ctx, s.snap, agent.ForkRequest{
		Prompt:         spec.Prompt,
		Appendix:       spec.Appendix,
		MaxTokens:      spec.MaxTokens,
		Validate:       spec.Validate,
		MaxRewrites:    spec.MaxRewrites,
		KeepToolChoice: keepToolChoice,
		Guard:          guard,
	})
	var out thread.ForkOutput
	if res != nil {
		out = thread.ForkOutput{Text: res.Text, Usage: res.Usage, Calls: res.Calls, Rewrites: res.Rewrites, Rejected: res.Rejected}
	}
	if err != nil && errors.Is(err, agent.ErrNoForkSnapshot) {
		err = fmt.Errorf("%w: %w", thread.ErrNoSnapshot, err)
	}
	return out, err
}

// offerOn offers text to a. With anyInstance a snapshot another instance
// took is accepted too (a team master is rebuilt from its checkpoint for
// every activation): the agent matches it by its boundary message's ID.
func offerOn(ctx context.Context, a *agent.UnifiedAgent, snap thread.Snapshot, text string, anyInstance bool) error {
	s, err := ownSnapshot(a, snap)
	if err != nil && anyInstance && errors.Is(err, thread.ErrStale) {
		err = nil
	}
	if err != nil {
		return err
	}
	err = a.OfferSummary(ctx, s.snap, text, agent.SummaryApplyOptions{})
	if errors.Is(err, agent.ErrStaleSnapshot) {
		return fmt.Errorf("%w: %w", thread.ErrStale, err)
	}
	return err
}
