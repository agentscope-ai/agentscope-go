package thread

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func edgeCloud() (*fakePart, *fakePart) {
	return newFakePart("edge", ZoneEdge), newFakePart("cloud", ZoneCloud)
}

func TestNewCoordinatorValidation(t *testing.T) {
	ok := func() []Participant { e, c := edgeCloud(); return []Participant{e, c} }
	edge2 := newFakePart("edge2", ZoneEdge)
	badBind := newFakePart("bad", ZoneEdge)
	badBind.bindErr = errors.New("no")
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no store", Config{Participants: ok()}, "needs a store"},
		{"no participants", Config{Store: NewMemoryStore()}, "at least one participant"},
		{"nil participant", Config{Store: NewMemoryStore(), Participants: []Participant{nil}}, "is nil"},
		{"invalid descriptor", Config{Store: NewMemoryStore(), Participants: []Participant{newFakePart("a/b", ZoneEdge)}}, "may not contain"},
		{"reserved user", Config{Store: NewMemoryStore(), Participants: []Participant{newFakePart("user", ZoneEdge)}}, "reserved"},
		{"reserved coordinator", Config{Store: NewMemoryStore(), Participants: []Participant{newFakePart("coordinator", ZoneCloud)}}, "reserved"},
		{"duplicate", Config{Store: NewMemoryStore(), Participants: []Participant{newFakePart("x", ZoneEdge), newFakePart("x", ZoneCloud)}}, "used twice"},
		{"two edges", Config{Store: NewMemoryStore(), Participants: append(ok(), edge2)}, "PeerTarget"},
		{"unknown peer target", Config{Store: NewMemoryStore(), Participants: ok(), Summaries: SummaryPolicy{PeerTarget: "nope"}}, "not a participant"},
		{"cloud peer target", Config{Store: NewMemoryStore(), Participants: ok(), Summaries: SummaryPolicy{PeerTarget: "cloud"}}, "not an edge"},
		{"share above one", Config{Store: NewMemoryStore(), Participants: ok(), Import: ImportPolicy{EdgeShare: 1.5}}, "at most 1"},
		{"bind error", Config{Store: NewMemoryStore(), Participants: []Participant{badBind}}, "bind bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCoordinator(context.Background(), tc.cfg)
			if err == nil {
				_ = c.Close()
				t.Fatal("NewCoordinator succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// Two edges are fine once PeerTarget names one of them.
	e, c := edgeCloud()
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Summaries.PeerTarget = "edge" }, e, c, newFakePart("edge2", ZoneEdge))
	if env.c.edge != "edge" {
		t.Fatalf("peer target = %q", env.c.edge)
	}
	if _, err := NewCoordinator(context.Background(), Config{Store: &failingStore{Store: NewMemoryStore(), headErr: errors.New("disk")}, Participants: ok()}); err == nil {
		t.Fatal("NewCoordinator ignored a head error")
	}
}

// releasingPart is a fakePart that accepts one Host at a time, like the
// participants in package participant, and records what it was released from.
type releasingPart struct {
	*fakePart
	bound    Host
	released []Host
}

func (r *releasingPart) Bind(h Host) error {
	r.mu.Lock()
	busy := r.bound != nil
	r.mu.Unlock()
	if busy {
		return errors.New("already bound")
	}
	if err := r.fakePart.Bind(h); err != nil {
		return err
	}
	r.mu.Lock()
	r.bound = h
	r.mu.Unlock()
	return nil
}

func (r *releasingPart) Release(h Host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, h)
	if r.bound == h {
		r.bound = nil
	}
}

func TestCoordinatorReleasesParticipants(t *testing.T) {
	edge := &releasingPart{fakePart: newFakePart("edge", ZoneEdge)}
	cloud := &releasingPart{fakePart: newFakePart("cloud", ZoneCloud)}
	store := NewMemoryStore()
	cfg := Config{Store: store, Participants: []Participant{edge, cloud}}

	c1, err := NewCoordinator(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCoordinator(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("a second Coordinator bound participants still in use: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*releasingPart{edge, cloud} {
		if len(p.released) == 0 || p.bound != nil {
			t.Fatalf("%s was not released on Close: released %d, bound %v", p.desc.Name, len(p.released), p.bound != nil)
		}
	}

	// A new Coordinator over the same participants binds them again.
	c2, err := NewCoordinator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewCoordinator after Close: %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}

	// A NewCoordinator that fails after binding releases what it bound.
	bad := Config{Store: &failingStore{Store: store, headErr: errors.New("disk")}, Participants: []Participant{edge, cloud}}
	if _, err := NewCoordinator(t.Context(), bad); err == nil {
		t.Fatal("NewCoordinator ignored a head error")
	}
	if edge.bound != nil || cloud.bound != nil {
		t.Fatal("a failed NewCoordinator left participants bound")
	}
	failing := &releasingPart{fakePart: newFakePart("cloud", ZoneCloud)}
	failing.bindErr = errors.New("no")
	if _, err := NewCoordinator(t.Context(), Config{Store: store, Participants: []Participant{edge, failing}}); err == nil {
		t.Fatal("NewCoordinator ignored a bind error")
	}
	if edge.bound != nil {
		t.Fatal("a bind error left the participants bound before it bound")
	}
}

// failingStore fails chosen calls.
type failingStore struct {
	Store
	headErr   error
	appendErr atomic.Pointer[error]
}

func (s *failingStore) Head(ctx context.Context) (Head, error) {
	if s.headErr != nil {
		return Head{}, s.headErr
	}
	return s.Store.Head(ctx)
}

func (s *failingStore) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	if p := s.appendErr.Load(); p != nil {
		return 0, *p
	}
	return s.Store.Append(ctx, entries, g)
}

func TestCoordinatorRoutesAndSealsReply(t *testing.T) {
	edge, cloud := edgeCloud()
	env := newCoEnv(t, nil, nil, edge, cloud)
	seq := env.say("hello")
	env.idle()

	if seq != 1 {
		t.Fatalf("user_input seq = %d", seq)
	}
	log := env.log()
	if got := kindsOf(log); !reflect.DeepEqual(got, []string{"user_input", "owner_change", "reply"}) {
		t.Fatalf("log kinds = %v", got)
	}
	if log[0].Origin.Participant != "user" || !strings.HasPrefix(log[0].DedupKey, "user:") {
		t.Fatalf("user_input = %+v", log[0])
	}
	oc := log[1]
	if oc.Origin != (Origin{}) || oc.Owner.From != "" || oc.Owner.To != "edge" || oc.Owner.Reason != ReasonRouter {
		t.Fatalf("owner_change = %+v %+v", oc, oc.Owner)
	}
	rep := log[2]
	if rep.Origin != (Origin{Participant: "edge"}) || rep.BasedOn != 1 || rep.Text() != "hello from edge" ||
		!strings.HasPrefix(rep.DedupKey, "reply:") || rep.Epoch != 1 {
		t.Fatalf("reply = %+v", rep)
	}
	calls := env.store.appends()
	if len(calls) != 3 || calls[1].guard == nil || *calls[1].guard != (Guard{Owner: "", Epoch: 0}) ||
		calls[2].guard == nil || *calls[2].guard != (Guard{Owner: "edge", Epoch: 1}) {
		t.Fatalf("appends = %+v", calls)
	}
	acts := edge.activations()
	if len(acts) != 1 || acts[0].Trigger.Seq != 1 || acts[0].Input != "hello" || !acts[0].Foreground ||
		acts[0].Epoch != 1 || acts[0].Depth != 0 || acts[0].Sync != nil {
		t.Fatalf("activation = %+v", acts)
	}
	if owner, epoch := env.c.Owner(); owner != "edge" || epoch != 1 {
		t.Fatalf("Owner() = %s, %d", owner, epoch)
	}
	var kinds []string
	for _, ev := range env.events() {
		k := string(ev.Kind)
		switch ev.Kind {
		case EventEntry:
			k += ":" + string(ev.Entry.Kind)
		case EventAgent:
			if !ev.Foreground || ev.Participant != "edge" || ev.Epoch != 1 {
				t.Fatalf("agent event = %+v", ev)
			}
		case EventRoute:
			if ev.Route.Target != "edge" || ev.Route.Source != RouteDefault {
				t.Fatalf("route = %+v", ev.Route)
			}
		}
		kinds = append(kinds, k)
	}
	want := []string{"route", "entry:owner_change", "agent", "entry:reply", "idle"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	if env.c.Ledger() == nil {
		t.Fatal("no ledger")
	}
}

func TestCoordinatorSubmitValidationAndRetry(t *testing.T) {
	edge, cloud := edgeCloud()
	env := newCoEnv(t, nil, nil, edge, cloud)
	if _, err := env.c.Submit(context.Background(), Query{Text: "  "}); err == nil {
		t.Fatal("empty query accepted")
	}
	first := env.submit(Query{OperationID: "op-1", Text: "hi"})
	env.idle()
	again := env.submit(Query{OperationID: "op-1", Text: "hi"})
	env.idle()
	if first != again {
		t.Fatalf("retry returned %d, first %d", again, first)
	}
	if n := len(findEntries(env.log(), KindUserInput, "")); n != 1 {
		t.Fatalf("%d user inputs after a retry", n)
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("%d activations after a retry", n)
	}
	if _, err := env.c.Submit(canceledCtx(), Query{Text: "late"}); err == nil {
		t.Fatal("Submit with a canceled context succeeded")
	}
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestCoordinatorRouterDecisions(t *testing.T) {
	edge, cloud := edgeCloud()
	var step atomic.Int32
	router := &fakeRouter{}
	router.fn = func(_ context.Context, req *RouteRequest) (RouteDecision, error) {
		switch step.Add(1) {
		case 1:
			return RouteDecision{Target: "cloud", Source: RouteClassifier, ClassifierCalls: 1, UsageKnown: true,
				ClassifierUsage: &model.ChatUsage{InputTokens: 30, OutputTokens: 2}}, nil
		case 2:
			return RouteDecision{}, ErrNoCandidate
		case 3:
			return RouteDecision{ClassifierCalls: 3}, errors.New("classifier down")
		default:
			return RouteDecision{Target: "nobody"}, nil // unknown: the default rule decides
		}
	}
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = router }, edge, cloud)

	env.say("one")
	env.idle()
	if owner, _ := env.c.Owner(); owner != "cloud" {
		t.Fatalf("owner = %s", owner)
	}
	two := env.say("two")
	env.idle()
	markers := findEntries(env.log(), KindMarker, MarkerNoOwner)
	if len(markers) != 1 || markers[0].BasedOn != two {
		t.Fatalf("no_owner markers = %+v", markers)
	}
	env.c.mu.Lock()
	parked := len(env.c.inputs) == 1 && env.c.inputs[0].parked && env.c.inputs[0].entry.Seq == two
	env.c.mu.Unlock()
	if !parked {
		t.Fatal("the unroutable input is not waiting")
	}
	env.say("three") // router error: the default keeps the owner
	env.idle()
	if n := len(cloud.activations()); n != 2 {
		t.Fatalf("cloud ran %d times", n)
	}
	env.c.mu.Lock()
	waiting := len(env.c.inputs)
	env.c.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d inputs still waiting after a new cycle", waiting)
	}
	// The older parked input reached cloud as context.
	if acts := cloud.activations(); !containsText(syncTexts(acts[1].Sync), "two") {
		t.Fatalf("parked input not imported: %v", syncTexts(acts[1].Sync))
	}
	env.submit(Query{Text: "four", Explicit: "edge"})
	env.idle()
	if acts := edge.activations(); len(acts) != 1 || acts[0].Input != "four" {
		t.Fatalf("the default rule did not take the named participant: %+v", acts)
	}

	byKind := env.c.Ledger().TotalsByKind()[UsageClassifier]
	if byKind.Calls != 4 || byKind.UnknownCalls != 3 || byKind.InputTokens != 30 {
		t.Fatalf("classifier ledger = %+v", byKind)
	}
	var routes int
	for _, ev := range env.events() {
		if ev.Kind == EventRoute {
			routes++
		}
	}
	if routes != 4 {
		t.Fatalf("%d route events", routes)
	}
}

func TestCoordinatorRouteRequest(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil,
		Entry{Kind: KindSummary, Origin: Origin{Participant: "cloud"},
			Summary: &SummaryMeta{Target: "edge"}, Coverage: &Coverage{}, Content: textBlocks("the summary")},
		Entry{Kind: KindArtifact, Refs: []ArtifactRef{{URI: "file:///a"}}},
		Entry{Kind: KindArtifact, Refs: []ArtifactRef{{URI: "file:///b"}}},
	)
	edge, cloud := edgeCloud()
	router := &fakeRouter{fn: func(_ context.Context, _ *RouteRequest) (RouteDecision, error) {
		return RouteDecision{Target: "edge"}, nil
	}}
	env := newCoEnv(t, store, func(cfg *Config) {
		cfg.Router = router
		cfg.Import.StaleAfter = 2
		cfg.Available = func(name string) bool { return name != "cloud" }
	}, edge, cloud)
	env.submit(Query{OperationID: "op", Text: "route me", Explicit: "edge", NeedsGroups: []string{"device"}})
	env.idle()

	reqs := router.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d route requests", len(reqs))
	}
	req := reqs[0]
	if req.OperationID != "op" || req.Query != "route me" || req.Owner != "" ||
		req.Explicit != "edge" || !reflect.DeepEqual(req.NeedsGroups, []string{"device"}) {
		t.Fatalf("request = %+v", req)
	}
	if req.PeerSummary != "the summary" || !req.SummaryStale {
		t.Fatalf("peer summary = %q stale %v", req.PeerSummary, req.SummaryStale)
	}
	// Both lag behind the whole log, the query included.
	if len(req.Participants) != 2 || req.Participants[0].Lag != 4 || !req.Participants[0].Available ||
		req.Participants[1].Lag != 4 || req.Participants[1].Available {
		t.Fatalf("candidates = %+v", req.Participants)
	}
	if len(req.Recent) != 4 {
		t.Fatalf("recent = %d entries", len(req.Recent))
	}
}

func TestCoordinatorDefaultRoute(t *testing.T) {
	edge, cloud := edgeCloud()
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.submit(Query{Text: "to cloud", Explicit: "cloud"})
	env.idle()
	if owner, _ := env.c.Owner(); owner != "cloud" {
		t.Fatalf("explicit owner = %s", owner)
	}
	env.submit(Query{Text: "stay"})
	env.idle()
	if n := len(cloud.activations()); n != 2 {
		t.Fatalf("cloud ran %d times", n)
	}
	env.submit(Query{Text: "back", Explicit: "edge"})
	env.idle()
	if owner, _ := env.c.Owner(); owner != "edge" {
		t.Fatalf("explicit owner = %s", owner)
	}
}

func TestCoordinatorForwardsOnlyForeground(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "look it up", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		r.say("asked cloud")
		r.confirm("")
		return &ActivationEnd{Reason: types.ReplyCompleted, Text: r.text()}
	})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		r.say("background words")
		r.confirm("researcher")
		return &ActivationEnd{Reason: types.ReplyCompleted, Text: "found it"}
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("go")
	env.idle()

	var fg, bg []Event
	for _, ev := range env.events() {
		if ev.Kind != EventAgent {
			continue
		}
		if ev.Participant == "edge" {
			fg = append(fg, ev)
		} else {
			bg = append(bg, ev)
		}
	}
	if len(fg) != 2 || !fg[0].Foreground || !fg[1].Foreground {
		t.Fatalf("foreground events = %+v", fg)
	}
	if len(bg) != 1 || bg[0].Foreground || bg[0].Path != "researcher" ||
		bg[0].Agent.GetEventType() != event.EventRequireUserConfirm {
		t.Fatalf("background events = %+v", bg)
	}
	res := event.NewUserConfirmResultEvent("reply", nil)
	if err := env.c.SubmitUserConfirm("cloud", "researcher", &res); err != nil {
		t.Fatal(err)
	}
	if cloud.confirms[0] != "researcher" {
		t.Fatalf("confirmation routed to %v", cloud.confirms)
	}
	if err := env.c.SubmitUserConfirm("nobody", "", &res); err == nil {
		t.Fatal("unknown participant accepted")
	}
	if err := env.c.SubmitUserConfirm("cloud", "", nil); err == nil {
		t.Fatal("nil result accepted")
	}
}

func TestCoordinatorSteeringThroughPending(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	var got *Sync
	edge.push(gate(release, func(r *fakeRun) *ActivationEnd {
		got = r.pending()
		return r.reply("both answered")
	}))
	env := newCoEnv(t, nil, nil, edge, cloud)
	first := env.say("first")
	waitStarted(t, edge)
	second := env.say("second")
	close(release)
	env.idle()

	if got == nil || got.Cursor != second || !containsText(syncTexts(got), "second") {
		t.Fatalf("pending = %+v", got)
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("%d activations; the steering input started a new cycle", n)
	}
	reps := findEntries(env.log(), KindReply, "")
	if len(reps) != 1 || reps[0].BasedOn != first {
		t.Fatalf("replies = %+v", reps)
	}
	env.c.mu.Lock()
	defer env.c.mu.Unlock()
	if len(env.c.inputs) != 0 {
		t.Fatal("the delivered input is still waiting")
	}
}

func TestCoordinatorStrandedInputsStartNewCycle(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	edge.push(gate(release, replyWith("first done")))
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("first")
	waitStarted(t, edge)
	env.say("second")
	third := env.say("third")
	close(release)
	env.idle()

	acts := edge.activations()
	if len(acts) != 2 || acts[1].Trigger.Seq != third || acts[1].Input != "third" {
		t.Fatalf("activations = %+v", acts)
	}
	if texts := syncTexts(acts[1].Sync); !containsText(texts, "second") || containsText(texts, "first done") {
		t.Fatalf("second activation imported %v", texts)
	}
}

func TestCoordinatorMaxPendingInputs(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	edge.push(gate(release, replyWith("ok")))
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Limits.MaxPendingInputs = 2 }, edge, cloud)
	env.say("a")
	waitStarted(t, edge)
	env.say("b")
	env.say("c")
	before := len(env.log())
	if _, err := env.c.Submit(context.Background(), Query{Text: "d"}); err == nil {
		t.Fatal("Submit beyond MaxPendingInputs succeeded")
	}
	if len(env.log()) != before {
		t.Fatal("a rejected Submit appended")
	}
	close(release)
	env.idle()
}

func TestCoordinatorTransfer(t *testing.T) {
	edge, cloud := edgeCloud()
	var seq uint64
	edge.push(func(r *fakeRun) *ActivationEnd {
		r.say("looking")
		var err error
		if seq, err = r.transfer("cloud"); err != nil {
			t.Errorf("transfer: %v", err)
		}
		return &ActivationEnd{Reason: types.ReplyHandedOff}
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("plan a trip")
	env.idle()

	log := env.log()
	if got := kindsOf(log); !reflect.DeepEqual(got, []string{"user_input", "owner_change", "reply", "owner_change", "reply"}) {
		t.Fatalf("log = %v", got)
	}
	if seq != 4 {
		t.Fatalf("Transfer returned %d", seq)
	}
	sealed, oc := log[2], log[3]
	if sealed.Origin.Participant != "edge" || sealed.Text() != "looking" || sealed.BasedOn != 1 {
		t.Fatalf("sealed reply = %+v", sealed)
	}
	if oc.Origin.Participant != "edge" || oc.Owner.Reason != ReasonTransfer || oc.Owner.From != "edge" ||
		oc.Owner.To != "cloud" || oc.Owner.Brief == nil || oc.BasedOn != 1 {
		t.Fatalf("transfer owner_change = %+v %+v", oc, oc.Owner)
	}
	var batch *appendCall
	for _, call := range env.store.appends() {
		if len(call.kinds) == 2 {
			batch = &call
		}
	}
	if batch == nil || !reflect.DeepEqual(batch.kinds, []Kind{KindReply, KindOwnerChange}) ||
		batch.guard == nil || *batch.guard != (Guard{Owner: "edge", Epoch: 1}) {
		t.Fatalf("transfer append = %+v", batch)
	}
	acts := cloud.activations()
	brief := testBrief()
	want := "[The foreground was handed to you by edge]\n" + brief.Render() + "\nContinue with the user from here."
	if len(acts) != 1 || acts[0].Trigger.Seq != 4 || acts[0].Input != want || !acts[0].Foreground || acts[0].Epoch != 2 {
		t.Fatalf("cloud activation = %+v", acts)
	}
	if !containsText(syncTexts(acts[0].Sync), "looking") {
		t.Fatalf("cloud did not import the sealed reply: %v", syncTexts(acts[0].Sync))
	}
	if owner, epoch := env.c.Owner(); owner != "cloud" || epoch != 2 {
		t.Fatalf("owner = %s %d", owner, epoch)
	}
	if n := len(findEntries(log, KindReply, "")); n != 2 {
		t.Fatalf("%d replies", n)
	}
}

func TestCoordinatorTransferRefusals(t *testing.T) {
	edge, cloud := edgeCloud()
	var cloudUp atomic.Bool
	cloudUp.Store(true)
	errs := map[string]error{}
	edge.push(func(r *fakeRun) *ActivationEnd {
		h := r.host()
		base := TransferRequest{From: Origin{Participant: "edge"}, Epoch: r.act.Epoch, To: "cloud", Brief: testBrief()}
		try := func(name string, mutate func(*TransferRequest)) {
			req := base
			mutate(&req)
			_, errs[name] = h.Transfer(r.ctx, req)
		}
		try("stale epoch", func(q *TransferRequest) { q.Epoch = 99 })
		try("wrong origin", func(q *TransferRequest) { q.From.Participant = "cloud" })
		try("unknown target", func(q *TransferRequest) { q.To = "nobody" })
		try("self", func(q *TransferRequest) { q.To = "edge" })
		try("no goal", func(q *TransferRequest) { q.Brief.Goal = "" })
		cloudUp.Store(false)
		try("unavailable", func(*TransferRequest) {})
		cloudUp.Store(true)
		return r.reply("staying")
	})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		_, errs["background caller"] = r.host().Transfer(r.ctx, TransferRequest{
			From: Origin{Participant: "cloud"}, Epoch: r.act.Epoch, To: "edge", Brief: testBrief()})
		return r.reply("bg done")
	})
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Available = func(name string) bool { return name != "cloud" || cloudUp.Load() }
	}, edge, cloud)
	env.say("start")
	env.idle()

	for _, name := range []string{"stale epoch"} {
		if !errors.Is(errs[name], ErrNotOwner) {
			t.Errorf("%s: %v, want ErrNotOwner", name, errs[name])
		}
	}
	for _, name := range []string{"wrong origin", "unknown target", "self", "no goal", "unavailable"} {
		if errs[name] == nil {
			t.Errorf("%s: transfer succeeded", name)
		}
	}

	// A background activation may not transfer.
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "check", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked")
	})
	env.say("next")
	env.idle()
	if !errors.Is(errs["background caller"], ErrNotOwner) {
		t.Errorf("background caller: %v", errs["background caller"])
	}
}

func TestCoordinatorLateOutput(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.push(func(r *fakeRun) *ActivationEnd {
		r.say("before")
		if _, err := r.transfer("cloud"); err != nil {
			t.Errorf("transfer: %v", err)
		}
		r.say("after the transfer")
		return &ActivationEnd{Reason: types.ReplyCompleted, Text: r.text()}
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("go")
	env.idle()

	log := env.log()
	reps := findEntries(log, KindReply, "")
	for i := range reps {
		if strings.Contains(reps[i].Text(), "after the transfer") {
			t.Fatal("late output became a reply")
		}
	}
	late := findEntries(log, KindReport, "")
	if len(late) != 1 || late[0].Origin.Participant != "edge" || !reflect.DeepEqual(late[0].To, []string{"cloud"}) ||
		late[0].Notify || late[0].Text() != "after the transfer" {
		t.Fatalf("late report = %+v", late)
	}
	if m := findEntries(log, KindMarker, MarkerRejected); len(m) != 1 {
		t.Fatalf("rejected markers = %+v", m)
	}
	for _, ev := range env.events() {
		if ev.Kind == EventAgent && ev.Participant == "edge" {
			if s, ok := ev.Agent.(event.TextBlockDeltaEvent); ok && s.Delta == "after the transfer" {
				t.Fatal("late output reached the user")
			}
		}
	}
}

func TestCoordinatorAssignReportAndWake(t *testing.T) {
	edge, cloud := edgeCloud()
	var taskID string
	cloud.push(func(r *fakeRun) *ActivationEnd {
		var err error
		if taskID, _, err = r.assign("edge", "count the photos", true); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("working on it")
	}, replyWith("there are 42 photos"))
	edge.push(func(r *fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyCompleted, Text: "42 photos", Digest: &ToolDigest{Calls: []ToolCallDigest{{Name: "Glob"}}}}
	})
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("how many photos?")
	env.idle()

	if taskID != "t-1" {
		t.Fatalf("task id = %q", taskID)
	}
	log := env.log()
	assigns := findEntries(log, KindAssign, "")
	if len(assigns) != 1 || assigns[0].Task.ID != "t-1" || !assigns[0].Task.Notify || !reflect.DeepEqual(assigns[0].To, []string{"edge"}) {
		t.Fatalf("assign = %+v", assigns)
	}
	ea := edge.activations()
	want := "[Task t-1 from cloud]\ncount the photos\nYour final answer is reported back to cloud automatically."
	if len(ea) != 1 || ea[0].Foreground || ea[0].Depth != 1 || ea[0].Input != want || ea[0].Trigger.Kind != KindAssign {
		t.Fatalf("edge activation = %+v", ea)
	}
	reps := findEntries(log, KindReport, "")
	if len(reps) != 1 {
		t.Fatalf("reports = %+v", reps)
	}
	rep := reps[0]
	if rep.Origin.Participant != "edge" || !reflect.DeepEqual(rep.To, []string{"cloud"}) || rep.Task == nil ||
		rep.Task.ID != "t-1" || !rep.Notify || rep.BasedOn != assigns[0].Seq || rep.Text() != "42 photos" || rep.Digest == nil {
		t.Fatalf("report = %+v", rep)
	}
	ca := cloud.activations()
	if len(ca) != 2 || ca[1].Trigger.Seq != rep.Seq || ca[1].Input != renderEntry(&rep) || !ca[1].Foreground {
		t.Fatalf("wake = %+v", ca)
	}
	if got := findEntries(log, KindReply, ""); got[len(got)-1].Text() != "there are 42 photos" {
		t.Fatalf("wake reply = %+v", got)
	}
}

func TestCoordinatorAssignLimits(t *testing.T) {
	edge, cloud := edgeCloud()
	errs := map[string]error{}
	edge.push(func(r *fakeRun) *ActivationEnd {
		h := r.host()
		_, _, errs["self"] = r.assign("edge", "x", false)
		_, _, errs["unknown"] = r.assign("nobody", "x", false)
		_, _, errs["empty"] = r.assign("cloud", " ", false)
		_, _, errs["wrong origin"] = h.Assign(r.ctx, AssignRequest{From: Origin{Participant: "cloud"}, To: "edge", Text: "x"})
		if _, _, err := r.assign("cloud", "dig", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked")
	})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		_, _, errs["depth"] = r.assign("edge", "deeper", false)
		return r.reply("dug")
	})
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Limits.MaxAssignDepth = 1 }, edge, cloud)
	env.say("go")
	env.idle()
	for name, err := range errs {
		if err == nil {
			t.Errorf("%s: assign succeeded", name)
		}
	}
	if len(errs) != 5 {
		t.Fatalf("errors = %v", errs)
	}
}

func TestCoordinatorWakeLimit(t *testing.T) {
	edge, cloud := edgeCloud()
	cloud.push(func(r *fakeRun) *ActivationEnd {
		for _, task := range []string{"first", "second"} {
			if _, _, err := r.assign("edge", task, true); err != nil {
				t.Errorf("assign: %v", err)
			}
		}
		return r.reply("asked twice")
	}, replyWith("woken"), replyWith("next answer"))
	edge.push(replyWith("first result"), replyWith("second result"))
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Router = cloudRouter()
		cfg.Limits.MaxWakesPerQuery = 1
	}, edge, cloud)
	env.say("go")
	env.idle()
	if n := len(cloud.activations()); n != 2 {
		t.Fatalf("cloud ran %d times; the wake limit is 1", n)
	}
	reps := findEntries(env.log(), KindReport, "")
	if len(reps) != 2 {
		t.Fatalf("reports = %d", len(reps))
	}
	// The report the limit left is imported by the next activation.
	env.say("and?")
	env.idle()
	ca := cloud.activations()
	all := syncTexts(ca[1].Sync)
	if len(ca) != 3 {
		t.Fatalf("cloud ran %d times", len(ca))
	}
	all = append(all, ca[1].Input)
	all = append(all, syncTexts(ca[2].Sync)...)
	if !containsText(all, "first result") || !containsText(all, "second result") {
		t.Fatalf("results not imported: %v", all)
	}
}

func TestCoordinatorBusyTargetQueued(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "long job", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		waitStarted(t, edge)
		if _, _, err := r.assign("edge", "second job", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		r.say("handing over")
		if _, err := r.transfer("edge"); err != nil {
			t.Errorf("transfer: %v", err)
		}
		close(release)
		return &ActivationEnd{Reason: types.ReplyHandedOff}
	})
	edge.push(gate(release, replyWith("long job done")), replyWith("hello user"), replyWith("second job done"))
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("go")
	env.idle()

	acts := edge.activations()
	if len(acts) != 3 {
		t.Fatalf("edge ran %d times", len(acts))
	}
	if acts[0].Foreground || !acts[1].Foreground || acts[1].Trigger.Kind != KindOwnerChange || acts[2].Foreground {
		t.Fatalf("order: %v %v %v", acts[0].Trigger.Kind, acts[1].Trigger.Kind, acts[2].Trigger.Kind)
	}
}

func TestCoordinatorFailover(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.push(func(r *fakeRun) *ActivationEnd {
		r.say("partial")
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("disk /private/x failed"), Text: "partial", ToolCalls: 2}
	})
	cloud.push(func(*fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("cloud down")}
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	q := env.say("fix it")
	env.idle()

	log := env.log()
	want := []string{"user_input", "owner_change", "reply", "marker:failed", "marker:unknown_effects", "owner_change", "marker:failed", "marker:no_owner"}
	if got := kindsOf(log); !reflect.DeepEqual(got, want) {
		t.Fatalf("log = %v", got)
	}
	if log[2].Text() != "partial" || log[3].BasedOn != q || log[4].BasedOn != q {
		t.Fatalf("partial reply / markers = %+v %+v %+v", log[2], log[3], log[4])
	}
	if log[3].Marker.Detail != "edge failed: disk /private/x failed" || log[6].Marker.Detail != "cloud failed: cloud down" {
		t.Fatalf("failed markers = %q, %q", log[3].Marker.Detail, log[6].Marker.Detail)
	}
	fo := log[5]
	if fo.Owner.Reason != ReasonFailover || fo.Owner.From != "edge" || fo.Owner.To != "cloud" || fo.Origin != (Origin{}) {
		t.Fatalf("failover = %+v", fo.Owner)
	}
	acts := cloud.activations()
	wantInput := `[edge failed: disk /private/x failed (#4). You now answer the user: continue their request #1 "fix it" ` +
		`from where edge left off. What edge did is in the summary and the entries above.]`
	if len(acts) != 1 || acts[0].Input != wantInput || acts[0].Trigger.Seq != fo.Seq {
		t.Fatalf("failover activation = %+v", acts)
	}
}

// TestCoordinatorFailoverHandsOnSteering: the user steered the edge's run
// twice before it failed. The failed marker records both messages, since
// the run ended there, and the cloud's failover takes both, which its reply
// records.
func TestCoordinatorFailoverHandsOnSteering(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	edge.push(gate(release, func(r *fakeRun) *ActivationEnd {
		r.pending()
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("model crashed")}
	}))
	cloud.push(replyWith("planned, in metric"))
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("plan my trip")
	waitStarted(t, edge)
	metric := env.say("use metric units")
	hotel := env.say("and book a hotel")
	close(release)
	env.idle()

	log := env.log()
	failed := findEntries(log, KindMarker, MarkerFailed)
	if len(failed) != 1 || !slices.Equal(failed[0].Steered, []uint64{metric, hotel}) {
		t.Fatalf("failed markers = %+v", failed)
	}
	replies := findEntries(log, KindReply, "")
	if len(replies) != 1 || replies[0].Origin.Participant != "cloud" || !slices.Equal(replies[0].Steered, []uint64{metric, hotel}) {
		t.Fatalf("replies = %+v", replies)
	}
	if acts := edge.activations(); len(acts) != 1 {
		t.Fatalf("edge activations = %+v", acts)
	}
	if !containsText(cloud.seenTexts(), "and book a hotel") {
		t.Fatal("the steering did not reach the cloud")
	}
}

func TestCoordinatorFailoverWhenUnavailable(t *testing.T) {
	edge, cloud := edgeCloud()
	var edgeCalls atomic.Int32
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Available = func(name string) bool {
			// The edge is up while routing and down when it would start.
			return name != "edge" || edgeCalls.Add(1) == 1
		}
	}, edge, cloud)
	env.say("hello")
	env.idle()
	if n := len(edge.activations()); n != 0 {
		t.Fatalf("unavailable edge ran %d times", n)
	}
	acts := cloud.activations()
	if len(acts) != 1 || !strings.HasPrefix(acts[0].Input, "[edge failed: it is unavailable (#") {
		t.Fatalf("cloud activation = %+v", acts)
	}
}

func TestCoordinatorInterrupt(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	edge.push(func(r *fakeRun) *ActivationEnd {
		r.say("half")
		<-r.ctx.Done()
		return &ActivationEnd{Reason: types.ReplyInterrupted, Text: r.text(), ToolCalls: 1}
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	q := env.say("long task")
	waitStarted(t, edge)
	env.c.Interrupt()
	env.idle()

	log := env.log()
	want := []string{"user_input", "owner_change", "reply", "marker:interrupted", "marker:unknown_effects"}
	if got := kindsOf(log); !reflect.DeepEqual(got, want) {
		t.Fatalf("log = %v", got)
	}
	for _, e := range log[2:] {
		if e.BasedOn != q {
			t.Fatalf("%s based on %d", e.Kind, e.BasedOn)
		}
	}
	if log[2].Text() != "half" {
		t.Fatalf("reply = %q", log[2].Text())
	}
	env.c.Interrupt() // nothing to interrupt
}

func TestCoordinatorInterruptWhileRouting(t *testing.T) {
	edge, cloud := edgeCloud()
	routing := make(chan struct{}, 1)
	router := &fakeRouter{fn: func(ctx context.Context, _ *RouteRequest) (RouteDecision, error) {
		routing <- struct{}{}
		<-ctx.Done()
		return RouteDecision{}, ctx.Err()
	}}
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = router }, edge, cloud)
	q := env.say("slow route")
	<-routing
	env.c.Interrupt()
	env.idle()
	m := findEntries(env.log(), KindMarker, MarkerInterrupted)
	if len(m) != 1 || m[0].BasedOn != q || !strings.Contains(m[0].Marker.Detail, "before an owner") {
		t.Fatalf("markers = %+v", m)
	}
	if len(edge.activations())+len(cloud.activations()) != 0 {
		t.Fatal("an interrupted query ran")
	}
}

func TestCoordinatorInterruptQueuedForeground(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	transferred := make(chan struct{})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "busy work", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		waitStarted(t, edge)
		if _, err := r.transfer("edge"); err != nil {
			t.Errorf("transfer: %v", err)
		}
		close(transferred)
		return &ActivationEnd{Reason: types.ReplyHandedOff}
	})
	edge.push(gate(release, replyWith("bg done")))
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("go")
	<-transferred // the edge's foreground activation is queued behind its task
	env.c.Interrupt()
	close(release)
	env.idle()
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("edge ran %d times; the interrupted foreground started", n)
	}
	if m := findEntries(env.log(), KindMarker, MarkerInterrupted); len(m) != 1 {
		t.Fatalf("interrupted markers = %+v", m)
	}
}

func TestCoordinatorCloseAndWaitIdle(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	edge.push(func(r *fakeRun) *ActivationEnd {
		r.say("working")
		<-r.ctx.Done()
		return &ActivationEnd{Reason: types.ReplyInterrupted, Text: r.text()}
	})
	store := NewMemoryStore()
	c, err := NewCoordinator(context.Background(), Config{Store: store, Participants: []Participant{edge, cloud}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(context.Background(), Query{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, edge)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.WaitIdle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitIdle with a canceled context = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitIdle(context.Background()) }()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitIdle across Close = %v", err)
	}
	// Close closed the channel; events still queued at Close are dropped, so
	// only the closing is checked.
	for range c.Events() {
		continue
	}
	if err := c.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v", err)
	}
	if err := c.WaitIdle(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitIdle after Close = %v", err)
	}
	if _, err := c.Submit(context.Background(), Query{Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close = %v", err)
	}
	// The interrupted activation still sealed what it showed.
	got, _ := store.Read(context.Background(), 0, Filter{}, 0)
	if kinds := kindsOf(got); !reflect.DeepEqual(kinds, []string{"user_input", "owner_change", "reply", "marker:interrupted"}) {
		t.Fatalf("log after Close = %v", kinds)
	}
}

func TestCoordinatorReleasesLeases(t *testing.T) {
	edge, cloud := edgeCloud()
	leases := NewLeases(nil)
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, err := leases.Acquire("camera", "edge", testTimeout); err != nil {
			t.Errorf("acquire: %v", err)
		}
		return r.reply("photo taken")
	})
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Leases = leases }, edge, cloud)
	env.say("take a photo")
	env.idle()
	if h, held := leases.Holder("camera"); held {
		t.Fatalf("lease still held by %s", h)
	}
}

func TestCoordinatorActivateErrorFailsOver(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.activateErr = errors.New("model not loaded")
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("hi")
	env.idle()
	acts := cloud.activations()
	if len(acts) != 1 || !strings.Contains(acts[0].Input, "[edge failed:") {
		t.Fatalf("cloud activations = %+v", acts)
	}
}

func TestCoordinatorAppendFailureIsReported(t *testing.T) {
	edge, cloud := edgeCloud()
	fs := &failingStore{Store: NewMemoryStore()}
	edge.push(func(r *fakeRun) *ActivationEnd {
		boom := errors.New("disk full")
		fs.appendErr.Store(&boom)
		return r.reply("lost")
	})
	env := newCoEnv(t, fs, nil, edge, cloud)
	env.say("hi")
	env.idle()
	var errs int
	for _, ev := range env.events() {
		if ev.Kind == EventError && strings.Contains(ev.Err.Error(), "disk full") {
			errs++
		}
	}
	if errs == 0 {
		t.Fatal("append failure not reported")
	}
}

// queuedForeground waits until the foreground activation of name is queued
// behind its running work.
func queuedForeground(t *testing.T, c *Coordinator, name string) {
	t.Helper()
	for range 500 {
		c.mu.Lock()
		ok := c.fg != nil && c.fg.part.name == name && !c.fg.started && c.owner == name
		c.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no queued foreground activation of %s", name)
}

// failingCloudWithEdgeTask sets up a failover to a busy participant: the
// cloud owns the query, assigns the edge a background task, publishes its
// plan and fails while the task runs. It returns the user input's seq once
// the failover is queued behind the edge's task; the task waits for release.
func failingCloudWithEdgeTask(t *testing.T, edgeTask fakeScript, more ...fakeScript) (*coEnv, *fakePart, uint64, chan struct{}) {
	t.Helper()
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 4)
	cloud.started = make(chan Activation, 4)
	failCloud, release := make(chan struct{}), make(chan struct{})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "check the sensors", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		r.plan("ask the edge for the sensors", "draft the morning")
		<-failCloud
		r.say("Planning your day. ")
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("cloud model unreachable"), Text: r.text()}
	})
	edge.push(append([]fakeScript{gate(release, edgeTask)}, more...)...)
	env := newCoEnv(t, nil, nil, edge, cloud)
	seq := env.submit(Query{Text: "plan my day", Explicit: "cloud"})
	waitStarted(t, cloud)
	waitStarted(t, edge)
	close(failCloud)
	queuedForeground(t, env.c, "edge")
	return env, edge, seq, release
}

// TestCoordinatorFailoverWaitsForTheTask: a failover to a participant that
// runs a background task queues behind the task; the running task is never
// steered. The failover's input is one line pointing at the failed marker,
// the request and the newest progress, which its import holds in full.
func TestCoordinatorFailoverWaitsForTheTask(t *testing.T) {
	env, edge, seq, release := failingCloudWithEdgeTask(t, func(r *fakeRun) *ActivationEnd {
		for range 20 {
			if s := r.pending(); s != nil {
				for _, m := range syncTexts(s) {
					if strings.Contains(m, "failed") {
						t.Errorf("the running task was told about the failure: %q", m)
					}
				}
			}
		}
		return r.reply("sensors checked")
	}, replyWith("Here is your plan."))
	close(release)
	env.idle()

	acts := edge.activations()
	if len(acts) != 2 || !acts[1].Foreground || acts[1].Trigger.Kind != KindOwnerChange ||
		acts[1].Trigger.Owner.Reason != ReasonFailover {
		t.Fatalf("edge activations = %+v", acts)
	}
	log := env.log()
	var failed, progress *Entry
	for i := range log {
		e := &log[i]
		switch {
		case e.Kind == KindMarker && e.Marker.Kind == MarkerFailed:
			failed = e
		case e.Kind == KindProgress:
			progress = e
		}
	}
	if failed == nil || failed.BasedOn != seq || failed.Marker.Detail != "cloud failed: cloud model unreachable" {
		t.Fatalf("failed marker = %+v", failed)
	}
	if progress == nil || progress.Origin.Participant != "cloud" || progress.BasedOn != seq ||
		!strings.Contains(progress.Text(), "2. [in_progress] draft the morning") {
		t.Fatalf("progress = %+v", progress)
	}
	in := acts[1].Input
	for _, want := range []string{
		fmt.Sprintf("[cloud failed: cloud model unreachable (#%d)", failed.Seq),
		fmt.Sprintf(`request #%d "plan my day"`, seq),
		fmt.Sprintf("latest progress is #%d.]", progress.Seq),
	} {
		if !strings.Contains(in, want) {
			t.Fatalf("failover input %q lacks %q", in, want)
		}
	}
	if strings.Contains(in, "\n") {
		t.Fatalf("failover input is not one line: %q", in)
	}
	imported := strings.Join(syncTexts(acts[1].Sync), "\n")
	if !strings.Contains(imported, fmt.Sprintf("[#%d progress of cloud]", progress.Seq)) ||
		!strings.Contains(imported, fmt.Sprintf("[#%d marker failed]", failed.Seq)) {
		t.Fatalf("the failover's import lacks the progress or the failure:\n%s", imported)
	}
}

// TestCoordinatorRoutedQueryWaitsForTheTask: a query routed to a participant
// that runs a background task does not take the task over; it waits.
func TestCoordinatorRoutedQueryWaitsForTheTask(t *testing.T) {
	edge, cloud := edgeCloud()
	cloud.started = make(chan Activation, 4)
	release := make(chan struct{})
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "check the forecast", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked the cloud")
	})
	cloud.push(gate(release, func(r *fakeRun) *ActivationEnd {
		for range 20 {
			r.pending()
		}
		return r.reply("forecast checked")
	}), replyWith("rain at noon"))
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("start")
	waitStarted(t, cloud)
	seq := env.submit(Query{Text: "will it rain today?", Explicit: "cloud"})
	queuedForeground(t, env.c, "cloud")
	close(release)
	env.idle()
	acts := cloud.activations()
	if len(acts) != 2 || acts[1].Trigger.Seq != seq || !acts[1].Foreground {
		t.Fatalf("cloud activations = %+v", acts)
	}
}
