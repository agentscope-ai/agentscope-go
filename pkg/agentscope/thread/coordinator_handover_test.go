package thread

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// readFailStore fails the Reads that fail matches.
type readFailStore struct {
	Store
	mu   sync.Mutex
	fail func(after uint64, f Filter, limit int) bool
}

func (s *readFailStore) Read(ctx context.Context, after uint64, f Filter, limit int) ([]Entry, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail != nil && fail(after, f, limit) {
		return nil, errors.New("store read failed")
	}
	return s.Store.Read(ctx, after, f, limit)
}

// A handover whose reads fail ends before the edge replies, and the request
// fails over to the cloud: the read of the summaries after its trigger, and
// the read of the entries before it.
func TestCoordinatorHandoverReadErrorFailsOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(after uint64, f Filter, limit int) bool
	}{
		{"summaries", func(_ uint64, f Filter, _ int) bool { return slices.Contains(f.Kinds, KindSummary) }},
		{"entries", func(after uint64, f Filter, limit int) bool { return after == 0 && len(f.Kinds) == 0 && limit == 8 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &readFailStore{Store: clearRuleStore(t, false)}
			edge, cloud := edgeCloud()
			env := newCoEnv(t, store, nil, edge, cloud)
			store.mu.Lock()
			store.fail = tc.fail
			store.mu.Unlock()
			env.submit(Query{Text: "continue", Explicit: "edge"}) // #9
			env.idle()
			if n := len(edge.activations()); n != 0 {
				t.Fatalf("the edge replied after a failed import: %d activations", n)
			}
			if n := len(cloud.activations()); n != 1 {
				t.Fatalf("the request must fail over to the cloud: %d activations", n)
			}
		})
	}
}

// Interrupting a handover while it waits for the peer summary ends it before
// the edge replies.
func TestCoordinatorInterruptDuringSummaryWait(t *testing.T) {
	env, edge, cloud := forkEnv(t, nil, func(cfg *Config) { cfg.Summaries.PeerEvery = 1 << 30 })
	gate := make(chan struct{})
	cloud.fmu.Lock()
	cloud.gate, cloud.forked = gate, make(chan ForkSpec, 4)
	cloud.fmu.Unlock()
	var oc atomic.Uint64
	cloud.push(handOffScript(t, &oc))
	env.say("go")
	select {
	case <-cloud.forked:
	case <-time.After(testTimeout):
		t.Fatal("the handoff started no peer fork")
	}
	// The edge was queued by the transfer, before the fork started; give it
	// time to reach the wait.
	time.Sleep(20 * time.Millisecond)
	env.c.Interrupt()
	// The interrupt ends the wait itself: its marker comes while the summary
	// is still being written.
	waitForEntry(t, env, "interrupted marker", func(e *Entry) bool {
		return e.Kind == KindMarker && e.Marker != nil && e.Marker.Kind == MarkerInterrupted
	})
	close(gate)
	env.idle()
	if n := len(edge.activations()); n != 0 {
		t.Fatalf("the interrupted handover started a reply: %d activations", n)
	}
}

// A handover the router decides waits for the peer summary the cloud's last
// reply started, and resets to it.
func TestCoordinatorRoutedHandoverWaitsForThePeerSummary(t *testing.T) {
	env, edge, cloud := forkEnv(t, nil, nil)
	gate := make(chan struct{})
	cloud.fmu.Lock()
	cloud.gate, cloud.forked = gate, make(chan ForkSpec, 4)
	cloud.fmu.Unlock()
	edge.started = make(chan Activation, 4)
	cloud.push(grow(500, "plan ready"))
	edge.push(replyWith("lamp on"))
	env.say("plan") // #1, owner_change #2, reply #3
	select {
	case <-cloud.forked:
	case <-time.After(testTimeout):
		t.Fatal("the cloud's reply started no peer fork")
	}
	env.submit(Query{Text: "lamp", Explicit: "edge"}) // #4
	select {
	case a := <-edge.started:
		t.Fatalf("the edge started before the summary it waits for: %+v", a.Sync)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	a := waitStarted(t, edge)
	if a.Sync == nil || !a.Sync.Reset || a.Sync.PeerSummary == nil || a.Sync.PeerSummary.Coverage.Through != 3 {
		t.Fatalf("the edge must reset to the summary through the cloud's reply #3: %+v", a.Sync)
	}
	env.idle()
}

// A query for the edge that arrives right after the cloud's reply, before
// the Coordinator decided on the summary that reply calls for, waits too:
// the summary is marked as pending when the cloud's stream ends.
func TestCoordinatorHandoverWaitsBeforeTheForkIsPlanned(t *testing.T) {
	env, edge, cloud := forkEnv(t, nil, nil)
	gate, planning, proceed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	release := func() { released.Do(func() { close(proceed) }) }
	t.Cleanup(release) // before the Coordinator's Close, which waits for the planning
	cloud.fmu.Lock()
	cloud.gate, cloud.forked = gate, make(chan ForkSpec, 4)
	cloud.beforeSnap = func() {
		once.Do(func() {
			close(planning)
			<-proceed
		})
	}
	cloud.fmu.Unlock()
	edge.started = make(chan Activation, 4)
	cloud.push(grow(500, "plan ready"))
	edge.push(replyWith("lamp on"))
	env.say("plan") // #1, owner_change #2, reply #3
	select {
	case <-planning:
	case <-time.After(testTimeout):
		t.Fatal("the cloud's reply planned no fork")
	}
	env.submit(Query{Text: "lamp", Explicit: "edge"}) // #4
	select {
	case a := <-edge.started:
		t.Fatalf("the edge started while the summary was being decided: %+v", a.Sync)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case a := <-edge.started:
		t.Fatalf("the edge started before the summary it waits for: %+v", a.Sync)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	a := waitStarted(t, edge)
	if a.Sync == nil || !a.Sync.Reset || a.Sync.PeerSummary == nil || a.Sync.PeerSummary.Coverage.Through != 3 {
		t.Fatalf("the edge must reset to the summary through the cloud's reply #3: %+v", a.Sync)
	}
	env.idle()
}

// The wait ends with the peer summary: the cloud's own notes, forked in the
// same job, may still be written while the edge starts from it.
func TestCoordinatorHandoverDoesNotWaitForOwnNotes(t *testing.T) {
	env, edge, cloud := forkEnv(t, nil, func(cfg *Config) { cfg.Summaries.OwnInit = 100 })
	ownGate := make(chan struct{})
	cloud.fmu.Lock()
	cloud.ownGate, cloud.forked = ownGate, make(chan ForkSpec, 4)
	cloud.fmu.Unlock()
	edge.started = make(chan Activation, 4)
	cloud.push(grow(500, "plan ready"))
	edge.push(replyWith("lamp on"))
	env.say("plan") // #1, owner_change #2, reply #3
	for range 2 {
		select {
		case <-cloud.forked:
		case <-time.After(testTimeout):
			t.Fatal("the cloud's reply did not fork both summaries")
		}
	}
	env.submit(Query{Text: "lamp", Explicit: "edge"})
	select {
	case a := <-edge.started:
		if a.Sync == nil || !a.Sync.Reset || a.Sync.PeerSummary == nil || a.Sync.PeerSummary.Coverage.Through != 3 {
			t.Fatalf("the edge must reset to the summary through the cloud's reply #3: %+v", a.Sync)
		}
	case <-time.After(testTimeout):
		t.Fatal("the edge waited for the cloud's own notes")
	}
	close(ownGate)
	env.idle()
	if len(cloud.offeredOwn()) != 1 {
		t.Fatalf("the own notes must still be offered: %v", cloud.offeredOwn())
	}
}

// bigEdgeEraStore is a log where the edge wrote eight long replies after the
// summary for it was written, then the cloud took over: a reset to that
// summary re-imports them and outgrows smallEdge's budget.
//
//	#1 user, #2 none -> cloud, #3 summary for edge through #2,
//	#4 cloud -> edge, #5-#12 edge replies, #13 edge -> cloud, #14 user,
//	#15 cloud reply
func bigEdgeEraStore(t *testing.T) Store {
	t.Helper()
	s := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, s, nil, userInput("start"))
	mustAppend(t, s, &Guard{}, ownerChange("", "cloud"))
	mustAppend(t, s, nil, Entry{
		Kind: KindSummary, Origin: Origin{Participant: "cloud"},
		Summary:  &SummaryMeta{Target: "edge", Budget: 200},
		Coverage: &Coverage{Through: 2},
		Content:  textBlocks("SUMMARY: the start"),
	})
	mustAppend(t, s, &Guard{Owner: "cloud", Epoch: 1}, ownerChange("cloud", "edge"))
	for range 8 {
		e := replyFrom("edge", strings.Repeat("b", 400))
		e.Origin.Path = ""
		e.BasedOn = 1
		mustAppend(t, s, &Guard{Owner: "edge", Epoch: 2}, e)
	}
	mustAppend(t, s, &Guard{Owner: "edge", Epoch: 2}, ownerChange("edge", "cloud"))
	mustAppend(t, s, nil, userInput("next"))
	cloud := replyFrom("cloud", "cloud answer")
	cloud.Origin.Path = ""
	cloud.BasedOn = 14
	mustAppend(t, s, &Guard{Owner: "cloud", Epoch: 3}, cloud)
	return s
}

// checkCursorsForward fails when a Sync applied to f, or its activation's,
// moves the cursor back from start.
func checkCursorsForward(t *testing.T, f *fakePart, start uint64) {
	t.Helper()
	cur := start
	syncs := f.appliedSyncs()
	acts := f.activations()
	for i := range acts {
		if acts[i].Sync != nil {
			syncs = append(syncs, *acts[i].Sync)
		}
	}
	for i, s := range syncs {
		if s.Cursor != 0 && s.Cursor < cur {
			t.Fatalf("sync %d moves the cursor back from %d to %d", i, cur, s.Cursor)
		}
		cur = max(cur, s.Cursor)
	}
}

// A reset that does not fit re-imports the edge's own entries, which are
// behind its cursor: the chunks keep the cursor until they pass it, and the
// edge answers.
func TestCoordinatorFallbackResetKeepsTheCursor(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 12
	_, cloud := edgeCloud()
	edge.push(replyWith("back"))
	env := newCoEnv(t, bigEdgeEraStore(t), nil, edge, cloud)
	env.submit(Query{Text: "back to the edge", Explicit: "edge"}) // #16
	env.idle()
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("the edge must answer: %d activations, markers %+v", n, findEntries(env.log(), KindMarker, ""))
	}
	syncs := edge.appliedSyncs()
	if len(syncs) == 0 || !syncs[0].Reset || syncs[0].Cursor != 0 {
		t.Fatalf("the reset must be applied first and keep the cursor: %+v", syncs)
	}
	checkCursorsForward(t, edge, 12)
	if a := edge.activations()[0]; a.Sync == nil || a.Sync.Cursor != 15 {
		t.Fatalf("the last chunk must bring the cursor to #15, before the trigger: %+v", a.Sync)
	}
}

// A failover to the edge resets it the same way when the cloud's reply
// fails before any refresh.
func TestCoordinatorFailoverResetKeepsTheCursor(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 12
	_, cloud := edgeCloud()
	cloud.push(func(*fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("cloud model down")}
	})
	edge.push(replyWith("from the edge"))
	env := newCoEnv(t, bigEdgeEraStore(t), nil, edge, cloud)
	env.submit(Query{Text: "cloud please", Explicit: "cloud"})
	env.idle()
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("the failover must reach the edge's reply: %d activations, markers %+v", n, findEntries(env.log(), KindMarker, ""))
	}
	checkCursorsForward(t, edge, 12)
}

// Without an edge to write for, a cloud reply opens no pending summary.
func TestCoordinatorNoPeerPendingWithoutAnEdge(t *testing.T) {
	cloud := newFakeForker("cloud")
	other := newFakePart("cloud2", ZoneCloud)
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, cloud, other)
	env.say("hi")
	env.idle()
	env.c.mu.Lock()
	p := env.c.parts["cloud"].peerPending
	env.c.mu.Unlock()
	if p != nil {
		t.Fatal("a pending peer summary was opened without an edge")
	}
	if n := len(cloud.activations()); n != 1 {
		t.Fatalf("the cloud must answer: %d activations", n)
	}
}

func TestResetBudget(t *testing.T) {
	sum := &Entry{Kind: KindSummary, Content: textBlocks(strings.Repeat("s", 400))} // 100 tokens
	for _, tc := range []struct {
		name   string
		ratio  float64
		window int
		want   int
	}{
		{"the peer ratio share", 0, 1000, 700},
		{"the import share when the rule is off", -1, 1000, 400},
		{"no limit for an unknown window", 0, 0, 0},
		{"at least 1", 0, 100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edge, cloud := edgeCloud()
			edge.desc.Window = tc.window
			env := newCoEnv(t, nil, func(cfg *Config) { cfg.Summaries.PeerRatio = tc.ratio }, edge, cloud)
			im := &importer{c: env.c, ps: env.c.parts["edge"]}
			if got := im.resetBudget(ParticipantState{}, sum); got != tc.want {
				t.Fatalf("resetBudget = %d, want %d", got, tc.want)
			}
		})
	}
}
