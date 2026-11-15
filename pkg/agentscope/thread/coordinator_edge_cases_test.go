package thread

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// edgeEraStore holds a session where the edge answered with eight long
// replies (#3–#10), the cloud took over, and a summary written for the edge
// (#14) excludes them (as a summary excludes entries its writer had not
// imported when it forked). The edge's cursor is 10.
func edgeEraStore(t *testing.T) Store {
	t.Helper()
	s := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, s, nil, userInput("start"))
	mustAppend(t, s, &Guard{}, ownerChange("", "edge"))
	for i := 0; i < 8; i++ {
		e := replyFrom("edge", strings.Repeat("b", 120))
		e.Origin.Path = ""
		e.BasedOn = 1
		mustAppend(t, s, &Guard{Owner: "edge", Epoch: 1}, e)
	}
	mustAppend(t, s, &Guard{Owner: "edge", Epoch: 1}, ownerChange("edge", "cloud"))
	mustAppend(t, s, nil, userInput("next"))
	cloud := replyFrom("cloud", "cloud answer")
	cloud.Origin.Path = ""
	cloud.BasedOn = 12
	mustAppend(t, s, &Guard{Owner: "cloud", Epoch: 2}, cloud)
	mustAppend(t, s, nil, Entry{
		Kind: KindSummary, Origin: Origin{Participant: "cloud"},
		Summary:  &SummaryMeta{Target: "edge", Budget: 200},
		Coverage: &Coverage{Through: 13, Excluded: []uint64{3, 4, 5, 6, 7, 8, 9, 10}},
		Content:  textBlocks("SUMMARY: the cloud's part"),
	})
	return s
}

// TestCoordinatorClearIsOneSync: a clear is one Sync with the activation,
// however large: the reset, the summary, the index and every entry the
// summary does not cover (the edge's own excluded replies included, since
// the reset drops them). It is not chunked or compacted on the way in, and
// its cursor never moves back.
func TestCoordinatorClearIsOneSync(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 10
	_, cloud := edgeCloud()
	env := newCoEnv(t, edgeEraStore(t), nil, edge, cloud)
	seq := env.submit(Query{Text: "back to the edge", Explicit: "edge"})
	env.idle()

	syncs := edge.appliedSyncs()
	if len(syncs) != 1 || !syncs[0].Reset || syncs[0].PeerSummary == nil || syncs[0].PeerSummary.Seq != 14 {
		t.Fatalf("want one reset Sync carrying summary #14: %+v", syncs)
	}
	if syncs[0].Cursor < 10 {
		t.Fatalf("the clear moved the cursor back to %d", syncs[0].Cursor)
	}
	if n := edge.compactCount(); n != 0 {
		t.Fatalf("the clear compacted %d times", n)
	}
	acts := edge.activations()
	if len(acts) != 1 || acts[0].Trigger.Seq != seq || acts[0].Sync == nil || !acts[0].Sync.Reset {
		t.Fatalf("the clear must go with the edge's activation for #%d: %+v", seq, acts)
	}
	got := strings.Join(syncTexts(&syncs[0]), "\n")
	for seq := 3; seq <= 10; seq++ {
		if !strings.Contains(got, "[#"+strconv.Itoa(seq)+" reply from edge]") {
			t.Fatalf("the clear lost the edge's reply #%d:\n%s", seq, got)
		}
	}
	env.c.mu.Lock()
	from := env.c.parts["edge"].resetFrom
	env.c.mu.Unlock()
	if from != 14 {
		t.Fatalf("resetFrom = %d", from)
	}
}

// TestCoordinatorParkedInputsDoNotBlockSubmit: queries that found no owner
// while every participant was down wait parked; they must not fill
// MaxPendingInputs and refuse every later query, and at most
// MaxPendingInputs of them are kept.
func TestCoordinatorParkedInputsDoNotBlockSubmit(t *testing.T) {
	edge, cloud := edgeCloud()
	var cloudUp atomic.Bool
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Limits.MaxPendingInputs = 3
		cfg.Available = func(name string) bool { return name == "cloud" && cloudUp.Load() }
	}, edge, cloud)
	var parked []uint64
	for i := 0; i < 5; i++ {
		parked = append(parked, env.submit(Query{Text: "early " + strconv.Itoa(i)}))
		env.idle()
	}
	if m := findEntries(env.log(), KindMarker, MarkerNoOwner); len(m) != len(parked) {
		t.Fatalf("no_owner markers = %d", len(m))
	}
	env.c.mu.Lock()
	var kept []uint64
	for _, r := range env.c.inputs {
		if !r.parked {
			t.Errorf("input #%d waits unparked", r.entry.Seq)
		}
		kept = append(kept, r.entry.Seq)
	}
	env.c.mu.Unlock()
	if want := parked[2:]; !slices.Equal(kept, want) {
		t.Fatalf("parked inputs = %v, want the newest three %v", kept, want)
	}

	cloudUp.Store(true)
	env.say("a later question")
	env.idle()
	if n := len(cloud.activations()); n != 1 {
		t.Fatalf("the later query must reach the cloud: %d activations", n)
	}
}

// TestCoordinatorEntriesWaitBehindAnOpenBacklog: while a Transfer's backlog
// is open, its entries wait for the events shown before it; entries committed
// meanwhile wait behind them, so entry events stay in log order.
func TestCoordinatorEntriesWaitBehindAnOpenBacklog(t *testing.T) {
	edge, cloud := edgeCloud()
	env := newCoEnv(t, nil, nil, edge, cloud)
	reply := Entry{Seq: 10, Kind: KindReply, Origin: Origin{Participant: "edge"}, Content: textBlocks("before the transfer")}
	later := markerEntry(MarkerNoOwner, 9, "committed while the backlog is open")
	later.Seq = 11
	c := env.c
	c.mu.Lock()
	a := &activation{backlog: make(chan struct{}), held: []Entry{reply}, eventsBefore: 2}
	c.open = a
	c.emitEntriesLocked([]Entry{later})
	c.flushBacklogLocked(a, true)
	c.mu.Unlock()
	var seqs []uint64
	for _, ev := range env.events() {
		if ev.Kind == EventEntry {
			seqs = append(seqs, ev.Entry.Seq)
		}
	}
	if !slices.Equal(seqs, []uint64{10, 11}) {
		t.Fatalf("entry events in order %v, want [10 11]", seqs)
	}
	if c.open != nil {
		t.Fatal("the flush must close the open backlog")
	}
}

// TestCoordinatorInterruptedSuccessorEndsTheBacklog: when the owner a Transfer
// handed to is interrupted while it waits for the old owner's pre-transfer
// events, the rest of them are no longer shown, so nothing of the old owner
// can follow a later owner's events. The Transfer's reply entry still
// carries the whole text.
func TestCoordinatorInterruptedSuccessorEndsTheBacklog(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.buf = 16
	var said []string
	for i := 0; i < 10; i++ {
		said = append(said, "d"+strconv.Itoa(i))
	}
	transferred := make(chan struct{})
	edge.push(func(r *fakeRun) *ActivationEnd {
		for _, s := range said {
			r.say(s)
		}
		if _, err := r.host().Transfer(r.ctx, TransferRequest{From: Origin{Participant: "edge"}, Epoch: r.act.Epoch,
			To: "cloud", Brief: testBrief(), Text: r.text(), EventsBefore: len(said)}); err != nil {
			t.Errorf("transfer: %v", err)
		}
		close(transferred)
		return &ActivationEnd{Reason: types.ReplyHandedOff}
	})
	cloud.push(replyWith("cloud answer"))
	store := NewMemoryStore()
	c, err := NewCoordinator(context.Background(), Config{
		Store: store, Participants: []Participant{edge, cloud}, Limits: Limits{EventBuffer: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Submit(context.Background(), Query{Text: "go", Explicit: "edge"}); err != nil {
		t.Fatal(err)
	}
	// The user reads nothing yet, so at most the first few deltas fit in the
	// buffers when the Transfer commits; the rest wait in the backlog.
	<-transferred
	c.Interrupt()
	// Wait until the interrupted successor has given up (its marker is in
	// the log) before the user starts reading.
	deadline := time.Now().Add(testTimeout)
	for len(findEntries(readAll(t, store), KindMarker, MarkerInterrupted)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the interrupted successor never ended")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := c.Submit(context.Background(), Query{Text: "next", Explicit: "cloud"}); err != nil {
		t.Fatal(err)
	}
	var edgeDeltas, order []string
	reply := ""
	for ev := range c.Events() {
		if ev.Kind == EventAgent {
			d := ev.Agent.(event.TextBlockDeltaEvent).Delta
			order = append(order, ev.Participant)
			if ev.Participant == "edge" {
				edgeDeltas = append(edgeDeltas, d)
			}
		}
		if ev.Kind == EventEntry && ev.Entry.Kind == KindReply && ev.Participant == "edge" {
			reply = ev.Entry.Text()
		}
		if ev.Kind == EventEntry && ev.Entry.Kind == KindReply && ev.Participant == "cloud" {
			break
		}
	}
	if len(edgeDeltas) >= len(said) || !slices.Equal(edgeDeltas, said[:len(edgeDeltas)]) {
		t.Fatalf("the edge deltas shown after the interrupt = %q; the backlog must end with the wait", edgeDeltas)
	}
	if last := order[len(order)-1]; last != "cloud" || slices.Index(order, "cloud") != len(edgeDeltas) {
		t.Fatalf("an old owner's event followed the new owner's: %q", order)
	}
	if reply != strings.Join(said, "") {
		t.Fatalf("the transfer's reply entry = %q", reply)
	}
}

func readAll(t *testing.T, s Store) []Entry {
	t.Helper()
	got, err := s.Read(context.Background(), 0, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCoordinatorFailedClearIsRepeated: a clear whose activation fails is
// not recorded, and the next takeover clears again with everything the
// first one carried.
func TestCoordinatorFailedClearIsRepeated(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 10
	edge.syncErr = errors.New("checkpoint save failed")
	_, cloud := edgeCloud()
	env := newCoEnv(t, edgeEraStore(t), nil, edge, cloud)
	env.submit(Query{Text: "back to the edge", Explicit: "edge"})
	env.idle()
	if n := len(edge.activations()); n != 0 || len(edge.appliedSyncs()) != 0 {
		t.Fatalf("the failed clear must change nothing: %d activations, syncs %+v", n, edge.appliedSyncs())
	}
	env.c.mu.Lock()
	from := env.c.parts["edge"].resetFrom
	env.c.mu.Unlock()
	if from != 0 {
		t.Fatalf("a failed clear was recorded: resetFrom = %d", from)
	}

	edge.mu.Lock()
	edge.syncErr = nil
	edge.mu.Unlock()
	env.submit(Query{Text: "edge again", Explicit: "edge"})
	env.idle()
	again := edge.appliedSyncs()
	if len(again) != 1 || !again[0].Reset {
		t.Fatalf("the next takeover must clear again in one Sync: %+v", again)
	}
	joined := strings.Join(syncTexts(&again[0]), "\n")
	for n := 3; n <= 10; n++ {
		if !strings.Contains(joined, "[#"+strconv.Itoa(n)+" reply from edge]") {
			t.Fatalf("the edge's own reply #%d is missing from the repeated clear:\n%s", n, joined)
		}
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("the edge must answer after the second clear: %d activations", n)
	}
}

// TestCoordinatorFailedBackgroundClearKeepsTheContext: a clear inside a
// background task that the edge cannot apply leaves its context whole. The
// failed task's report, written in the edge's name, moves its activity past
// the summary, so the next takeover imports incrementally: nothing the edge
// held was dropped.
func TestCoordinatorFailedBackgroundClearKeepsTheContext(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 10
	edge.syncErr = errors.New("checkpoint save failed")
	_, cloud := edgeCloud()
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "check the photos", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("assigned")
	})
	env := newCoEnv(t, edgeEraStore(t), nil, edge, cloud)
	env.submit(Query{Text: "next question", Explicit: "cloud"})
	env.idle()
	if n := len(edge.activations()); n != 0 || len(edge.appliedSyncs()) != 0 {
		t.Fatalf("want the task's clear to fail unapplied: %d activations, syncs %+v", n, edge.appliedSyncs())
	}
	var failed bool
	for _, e := range env.log() {
		if e.Kind == KindReport && e.Origin.Participant == "edge" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the failed task must be reported in the edge's name")
	}

	edge.mu.Lock()
	edge.syncErr = nil
	edge.mu.Unlock()
	env.submit(Query{Text: "edge again", Explicit: "edge"})
	env.idle()
	for _, s := range edge.appliedSyncs() {
		if s.Reset {
			t.Fatalf("the edge was reset after its activity passed the summary: %+v", s)
		}
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("the edge must answer: %d activations", n)
	}
}

// TestCoordinatorClearLeavesSteeringWhole: a clear has no budget, so a user
// message that steers the edge's reply after a large clear arrives in full
// instead of as a cut notice.
func TestCoordinatorClearLeavesSteeringWhole(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 10
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	var got []string
	edge.push(gate(release, func(r *fakeRun) *ActivationEnd {
		for range 3 {
			if s := r.pending(); s != nil {
				got = append(got, syncTexts(s)...)
			}
		}
		return r.reply("ok")
	}))
	_, cloud := edgeCloud()
	env := newCoEnv(t, edgeEraStore(t), nil, edge, cloud)
	env.submit(Query{Text: "back to the edge", Explicit: "edge"})
	if a := waitStarted(t, edge); a.Sync == nil || !a.Sync.Reset || countTokens(a.Sync.Messages) <= int(0.5*float64(edge.desc.Window)) {
		t.Fatalf("want a clear larger than the edge's import share: %+v", a.Sync)
	}
	steer := "STEERING: also check whether the garage door is closed"
	env.submit(Query{Text: steer, Explicit: "edge"})
	close(release)
	env.idle()
	if !containsText(got, steer) {
		t.Fatalf("the steering message did not arrive in full: %q", got)
	}
}

// TestCoordinatorUnsavedClearIsRepeated: an agent can apply a clear in memory
// and then fail to save it, which fails the activation. The Coordinator does
// not record the clear, and the repeated clear applies over the first one
// (its cursor is not behind) and still carries the edge's own replies.
func TestCoordinatorUnsavedClearIsRepeated(t *testing.T) {
	edge := smallEdge()
	edge.state.Cursor = 10
	edge.saveErr = errors.New("checkpoint save failed")
	_, cloud := edgeCloud()
	env := newCoEnv(t, edgeEraStore(t), nil, edge, cloud)
	env.submit(Query{Text: "back to the edge", Explicit: "edge"})
	env.idle()
	if n := len(edge.activations()); n != 0 || len(edge.appliedSyncs()) != 1 {
		t.Fatalf("want the clear applied without a reply: %d activations, %d syncs", n, len(edge.appliedSyncs()))
	}
	env.c.mu.Lock()
	from := env.c.parts["edge"].resetFrom
	env.c.mu.Unlock()
	if from != 0 {
		t.Fatalf("an unsaved clear was recorded: resetFrom = %d", from)
	}

	edge.mu.Lock()
	edge.saveErr = nil
	edge.mu.Unlock()
	env.submit(Query{Text: "edge again", Explicit: "edge"})
	env.idle()
	syncs := edge.appliedSyncs()
	if len(syncs) != 2 || !syncs[1].Reset || syncs[1].Cursor < syncs[0].Cursor {
		t.Fatalf("want a second clear applied over the first: %+v", syncs)
	}
	joined := strings.Join(syncTexts(&syncs[1]), "\n")
	for n := 3; n <= 10; n++ {
		if !strings.Contains(joined, "[#"+strconv.Itoa(n)+" reply from edge]") {
			t.Fatalf("the repeated clear lost the edge's reply #%d:\n%s", n, joined)
		}
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("the edge must answer after the repeated clear: %d activations", n)
	}
}
