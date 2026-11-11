package thread

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// testTimeout bounds every wait in the coordinator tests. Waits use channels
// and WaitIdle; the timeout only turns a hang into a failure.
const testTimeout = 10 * time.Second

// ---------------------------------------------------------------- participant

// fakeScript runs one activation and returns its end.
type fakeScript func(r *fakeRun) *ActivationEnd

type fakeCompact struct {
	ok     bool
	err    error
	tokens int // InputTokens after a successful compaction
}

// fakePart is a scripted Participant. It records everything the Coordinator
// hands it.
type fakePart struct {
	desc Descriptor

	mu          sync.Mutex
	host        Host
	bindErr     error
	state       ParticipantState
	acts        []Activation
	syncs       []Sync // applied through Sync and Activation.Sync
	reads       [][]Entry
	seen        []string // every text that entered its context
	compacts    int
	compactPlan []fakeCompact
	onCompact   func()
	syncErr     error
	saveErr     error // Activate applies its Sync, then fails (an unsaved checkpoint)
	activateErr error
	scripts     []fakeScript
	script      fakeScript // used when scripts is empty
	confirms    []string
	started     chan Activation
	buf         int // buffer of the activation channel
}

func newFakePart(name string, zone Zone) *fakePart {
	return &fakePart{desc: Descriptor{Name: name, Zone: zone, Window: 8000, SummaryBudget: 200}}
}

func (f *fakePart) Descriptor() Descriptor { return f.desc }

func (f *fakePart) Bind(h Host) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bindErr != nil {
		return f.bindErr
	}
	f.host = h
	return nil
}

func (f *fakePart) State() ParticipantState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakePart) Sync(_ context.Context, s Sync) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.applyLocked(&s)
}

// applyLocked applies s with the agent's cursor rule (ApplyThreadUpdate): the
// cursor never moves back.
func (f *fakePart) applyLocked(s *Sync) error {
	if s.Cursor != 0 && s.Cursor < f.state.Cursor {
		return fmt.Errorf("fake: cursor %d is behind the current cursor %d", s.Cursor, f.state.Cursor)
	}
	cp := *s
	cp.Messages = slices.Clone(s.Messages)
	f.syncs = append(f.syncs, cp)
	if s.Reset {
		f.state.InputTokens = 100
	}
	if s.PeerSummary != nil {
		f.seen = append(f.seen, s.PeerSummary.Text())
	}
	for _, m := range s.Messages {
		f.seen = append(f.seen, msgText(m))
		f.state.InputTokens += estimateTokens(m)
	}
	if s.Cursor != 0 {
		f.state.Cursor = s.Cursor
	}
	return nil
}

func (f *fakePart) Compact(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	hook := f.onCompact
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compacts++
	plan := fakeCompact{ok: true, tokens: 100}
	if len(f.compactPlan) > 0 {
		plan = f.compactPlan[0]
		f.compactPlan = f.compactPlan[1:]
	}
	if plan.err != nil {
		return false, plan.err
	}
	if plan.ok {
		f.state.InputTokens = plan.tokens
	}
	return plan.ok, nil
}

func (f *fakePart) Activate(ctx context.Context, act Activation) (<-chan ActivationEvent, error) { //nolint:gocritic // the Participant interface takes activations by value
	f.mu.Lock()
	if f.activateErr != nil {
		err := f.activateErr
		f.mu.Unlock()
		return nil, err
	}
	if f.state.Busy {
		f.mu.Unlock()
		return nil, errors.New("fake: already busy")
	}
	if act.Sync != nil {
		// Like the in-repo participants: a Sync that cannot be applied fails
		// Activate, and nothing runs.
		err := f.syncErr
		if err == nil {
			err = f.applyLocked(act.Sync)
		}
		if err == nil {
			err = f.saveErr
		}
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
	}
	f.state.Busy = true
	f.acts = append(f.acts, act)
	f.seen = append(f.seen, act.Input)
	f.state.Cursor = max(f.state.Cursor, act.Trigger.Seq)
	script := f.script
	if len(f.scripts) > 0 {
		script = f.scripts[0]
		f.scripts = f.scripts[1:]
	}
	started, buf := f.started, f.buf
	f.mu.Unlock()

	ch := make(chan ActivationEvent, buf)
	run := &fakeRun{f: f, ctx: ctx, act: act, out: ch}
	go func() {
		defer close(ch)
		var end *ActivationEnd
		if script != nil {
			end = script(run)
		} else {
			end = run.reply("hello from " + f.desc.Name)
		}
		f.mu.Lock()
		f.state.Busy = false
		f.mu.Unlock()
		if end != nil {
			ch <- ActivationEvent{End: end}
		}
	}()
	if started != nil {
		started <- act
	}
	return ch, nil
}

func (f *fakePart) SubmitUserConfirm(path string, _ *event.UserConfirmResultEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms = append(f.confirms, path)
	return nil
}

func (f *fakePart) push(scripts ...fakeScript) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, scripts...)
}

func (f *fakePart) setState(fn func(s *ParticipantState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.state)
}

func (f *fakePart) activations() []Activation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.acts)
}

func (f *fakePart) appliedSyncs() []Sync {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

func (f *fakePart) seenTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.seen)
	for _, rs := range f.reads {
		for i := range rs {
			out = append(out, renderEntry(&rs[i]))
		}
	}
	return out
}

func (f *fakePart) compactCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compacts
}

// fakeRun is one running activation of a fakePart.
type fakeRun struct {
	f     *fakePart
	ctx   context.Context
	act   Activation
	out   chan<- ActivationEvent
	shown []string
}

func (r *fakeRun) name() string { return r.f.desc.Name }
func (r *fakeRun) host() Host {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return r.f.host
}

// say emits a text delta the user would see.
func (r *fakeRun) say(text string) {
	r.shown = append(r.shown, text)
	r.out <- ActivationEvent{Event: event.NewTextBlockDeltaEvent("reply", "block", text)}
}

// confirm emits a confirmation request.
func (r *fakeRun) confirm(path string) {
	r.out <- ActivationEvent{Path: path, Event: event.NewRequireUserConfirmEvent("reply", nil)}
}

// text is what the activation showed since it started or the last seal.
func (r *fakeRun) text() string { return strings.Join(r.shown, "") }

// reply says text and ends completed with it.
func (r *fakeRun) reply(text string) *ActivationEnd {
	r.say(text)
	return &ActivationEnd{Reason: types.ReplyCompleted, Text: r.text()}
}

// pending asks the host for boundary input and applies it.
func (r *fakeRun) pending() *Sync {
	cursor := r.f.State().Cursor
	s, err := r.host().Pending(r.ctx, r.name(), cursor)
	r.f.mu.Lock()
	if err == nil && s != nil {
		if aerr := r.f.applyLocked(s); aerr != nil {
			// Pending projects past the given cursor, so this is a
			// Coordinator bug; the agent would end the reply.
			panic(aerr)
		}
	}
	r.f.mu.Unlock()
	return s
}

// plan publishes a plan_updated event, as an agent's update_plan tool does.
func (r *fakeRun) plan(steps ...string) {
	items := make([]any, 0, len(steps))
	for i, st := range steps {
		status := "completed"
		if i == len(steps)-1 {
			status = "in_progress"
		}
		items = append(items, map[string]any{"step": st, "status": status})
	}
	r.out <- ActivationEvent{Event: event.NewCustomEvent("", planEvent, map[string]any{"plan": items})}
}

// read reads the thread and records what it got.
func (r *fakeRun) read(q ReadQuery) ([]Entry, error) {
	got, err := r.host().Read(r.ctx, r.name(), q)
	r.f.mu.Lock()
	r.f.reads = append(r.f.reads, got)
	r.f.mu.Unlock()
	return got, err
}

func testBrief() Brief {
	return Brief{Goal: "finish the trip plan", Remaining: []string{"book the hotel"}}
}

// transfer hands the foreground over, sealing what was shown.
func (r *fakeRun) transfer(to string) (uint64, error) {
	seq, err := r.host().Transfer(r.ctx, TransferRequest{
		From:  Origin{Participant: r.name()},
		Epoch: r.act.Epoch,
		To:    to,
		Brief: testBrief(),
		Text:  r.text(),
	})
	if err == nil {
		r.shown = nil
	}
	return seq, err
}

func (r *fakeRun) assign(to, text string, notify bool) (string, uint64, error) {
	return r.host().Assign(r.ctx, AssignRequest{
		From:   Origin{Participant: r.name()},
		Epoch:  r.act.Epoch,
		Depth:  r.act.Depth,
		To:     to,
		Text:   text,
		Notify: notify,
	})
}

func (r *fakeRun) report(text string, notify bool, to ...string) (uint64, error) {
	return r.host().Report(r.ctx, ReportRequest{From: Origin{Participant: r.name()}, Text: text, To: to, Notify: notify})
}

// gate returns a script that waits for release (or cancellation) and then
// runs next.
func gate(release <-chan struct{}, next fakeScript) fakeScript {
	return func(r *fakeRun) *ActivationEnd {
		select {
		case <-release:
		case <-r.ctx.Done():
		}
		return next(r)
	}
}

// replyWith returns a script that replies text.
func replyWith(text string) fakeScript {
	return func(r *fakeRun) *ActivationEnd { return r.reply(text) }
}

// ---------------------------------------------------------------- forker

type fakeSnap struct{ cursor uint64 }

func (s fakeSnap) Cursor() uint64 { return s.cursor }

// fakeForker is a cloud participant that forks summaries.
type fakeForker struct {
	*fakePart

	fmu         sync.Mutex
	snapErrs    []error
	specs       []ForkSpec
	peerOut     ForkOutput
	ownOut      ForkOutput
	forkErr     error
	offered     []string
	offerErr    error
	beforeOffer func()
	gate        chan struct{}
	forked      chan ForkSpec
}

func newFakeForker(name string) *fakeForker {
	f := &fakeForker{fakePart: newFakePart(name, ZoneCloud)}
	f.desc.Window = 100000
	f.peerOut = ForkOutput{Text: "peer summary text", Calls: 1, Usage: &model.ChatUsage{InputTokens: 10, OutputTokens: 5}}
	f.ownOut = ForkOutput{Text: "own summary text", Calls: 1, Usage: &model.ChatUsage{InputTokens: 20, OutputTokens: 6}}
	return f
}

func (f *fakeForker) Snapshot() (Snapshot, error) {
	f.fmu.Lock()
	if len(f.snapErrs) > 0 {
		err := f.snapErrs[0]
		f.snapErrs = f.snapErrs[1:]
		f.fmu.Unlock()
		if err != nil {
			return nil, err
		}
	} else {
		f.fmu.Unlock()
	}
	return fakeSnap{cursor: f.State().Cursor}, nil
}

func (f *fakeForker) Fork(ctx context.Context, _ Snapshot, spec ForkSpec) (ForkOutput, error) { //nolint:gocritic // the Forker interface takes specs by value
	f.fmu.Lock()
	f.specs = append(f.specs, spec)
	g, forked := f.gate, f.forked
	out := f.peerOut
	if spec.Prompt == DefaultOwnPrompt || spec.Prompt == DefaultOwnUpdatePrompt {
		out = f.ownOut
	}
	err := f.forkErr
	f.fmu.Unlock()
	if forked != nil {
		forked <- spec
	}
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return ForkOutput{}, ctx.Err()
		}
	}
	return out, err
}

func (f *fakeForker) OfferOwnSummary(_ context.Context, _ Snapshot, text string) error {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	if f.beforeOffer != nil {
		f.beforeOffer()
	}
	if f.offerErr != nil {
		return f.offerErr
	}
	f.offered = append(f.offered, text)
	return nil
}

func (f *fakeForker) forkSpecs() []ForkSpec {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return slices.Clone(f.specs)
}

func (f *fakeForker) offeredOwn() []string {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return slices.Clone(f.offered)
}

// ---------------------------------------------------------------- router and policy

type fakeRouter struct {
	mu   sync.Mutex
	reqs []RouteRequest
	fn   func(ctx context.Context, req *RouteRequest) (RouteDecision, error)
}

func (r *fakeRouter) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) { //nolint:gocritic // the Router interface takes requests by value
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	fn := r.fn
	r.mu.Unlock()
	return fn(ctx, &req)
}

func (r *fakeRouter) requests() []RouteRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

// cloudRouter sends the queries that name the edge to edge and the rest to
// cloud.
func cloudRouter() *fakeRouter {
	return &fakeRouter{fn: func(_ context.Context, req *RouteRequest) (RouteDecision, error) {
		if req.Explicit == "edge" {
			return RouteDecision{Target: "edge", Source: RouteHard}, nil
		}
		return RouteDecision{Target: "cloud", Source: RouteRule}, nil
	}}
}

// ---------------------------------------------------------------- store

type appendCall struct {
	kinds []Kind
	guard *Guard
	err   error
}

// guardStore records every Append's kinds and guard.
type guardStore struct {
	Store
	mu    sync.Mutex
	calls []appendCall
}

func (s *guardStore) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	seq, err := s.Store.Append(ctx, entries, g)
	call := appendCall{err: err}
	for i := range entries {
		call.kinds = append(call.kinds, entries[i].Kind)
	}
	if g != nil {
		gc := *g
		call.guard = &gc
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	return seq, err
}

func (s *guardStore) appends() []appendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// ---------------------------------------------------------------- environment

// eventLog collects a Coordinator's events.
type eventLog struct {
	mu       sync.Mutex
	list     []Event
	barriers chan struct{}
	done     chan struct{}
}

const barrierKind EventKind = "test-barrier"

func collectEvents(c *Coordinator) *eventLog {
	l := &eventLog{barriers: make(chan struct{}, 16), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for ev := range c.Events() {
			if ev.Kind == barrierKind {
				l.barriers <- struct{}{}
				continue
			}
			l.mu.Lock()
			l.list = append(l.list, ev)
			l.mu.Unlock()
		}
	}()
	return l
}

// flush returns every event queued so far, in order.
func (l *eventLog) flush(t *testing.T, c *Coordinator) []Event {
	t.Helper()
	c.mu.Lock()
	c.enqueueLocked(&Event{Kind: barrierKind})
	c.mu.Unlock()
	select {
	case <-l.barriers:
	case <-time.After(testTimeout):
		t.Fatal("events were not delivered")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list)
}

type coEnv struct {
	t     *testing.T
	store *guardStore
	c     *Coordinator
	ev    *eventLog
}

// newCoEnv builds a Coordinator over a fresh MemoryStore (or base, when
// given) with the participants, after mutate adjusts the config.
func newCoEnv(t *testing.T, base Store, mutate func(*Config), parts ...Participant) *coEnv {
	t.Helper()
	if base == nil {
		base = NewMemoryStore(WithClock(fixedClock()))
	}
	gs := &guardStore{Store: base}
	cfg := Config{Store: gs, Participants: parts, Clock: fixedClock()}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewCoordinator(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	env := &coEnv{t: t, store: gs, c: c, ev: collectEvents(c)}
	t.Cleanup(func() {
		_ = c.Close()
		<-env.ev.done
	})
	return env
}

func (e *coEnv) submit(q Query) uint64 { //nolint:gocritic // test helper mirrors Submit
	e.t.Helper()
	seq, err := e.c.Submit(context.Background(), q)
	if err != nil {
		e.t.Fatalf("Submit(%q): %v", q.Text, err)
	}
	return seq
}

func (e *coEnv) say(text string) uint64 { e.t.Helper(); return e.submit(Query{Text: text}) }

func (e *coEnv) idle() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := e.c.WaitIdle(ctx); err != nil {
		e.t.Fatalf("WaitIdle: %v", err)
	}
}

func (e *coEnv) log() []Entry {
	e.t.Helper()
	got, err := e.store.Read(context.Background(), 0, Filter{}, 0)
	if err != nil {
		e.t.Fatalf("Read: %v", err)
	}
	return got
}

func (e *coEnv) events() []Event { e.t.Helper(); return e.ev.flush(e.t, e.c) }

// waitStarted waits for the next activation of a participant whose started
// channel is set.
func waitStarted(t *testing.T, f *fakePart) Activation {
	t.Helper()
	select {
	case a := <-f.started:
		return a
	case <-time.After(testTimeout):
		t.Fatalf("%s did not start", f.desc.Name)
		return Activation{}
	}
}

// kindsOf lists the kinds of entries, markers as "marker:<kind>".
func kindsOf(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		k := string(e.Kind)
		if e.Marker != nil {
			k += ":" + string(e.Marker.Kind)
		}
		out = append(out, k)
	}
	return out
}

// findEntries returns the entries of kind (and marker kind, when given).
func findEntries(entries []Entry, kind Kind, marker MarkerKind) []Entry {
	var out []Entry
	for i := range entries {
		e := &entries[i]
		if e.Kind != kind || (marker != "" && (e.Marker == nil || e.Marker.Kind != marker)) {
			continue
		}
		out = append(out, *e)
	}
	return out
}

func syncTexts(s *Sync) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Messages))
	for _, m := range s.Messages {
		out = append(out, msgText(m))
	}
	return out
}

func containsText(list []string, sub string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, sub) })
}
