package thread

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// These tests reproduce the probes of an independent review of the
// Coordinator, one per finding.

// A clear is recorded only once its reset is applied: when Activate fails,
// the edge's view stays as it was and the clear is tried again next time.
func TestCoordinatorClearCommitsOnlyWhenApplied(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("plan"))
	mustAppend(t, store, &Guard{}, ownerChange("", "cloud"))
	rep := replyFrom("cloud", "the plan")
	rep.Origin.Path = ""
	mustAppend(t, store, &Guard{Owner: "cloud", Epoch: 1}, rep)
	mustAppend(t, store, nil, Entry{
		Kind: KindSummary, Origin: Origin{Participant: "cloud"},
		Summary: &SummaryMeta{Target: "edge"}, Coverage: &Coverage{Through: 3}, Content: textBlocks("SUMMARY"),
	}) // #4
	edge, cloud := edgeCloud()
	edge.activateErr = errors.New("model not loaded")
	env := newCoEnv(t, store, nil, edge, cloud)

	env.submit(Query{Text: "continue", Explicit: "edge"})
	env.idle()
	env.c.mu.Lock()
	ps := env.c.parts["edge"]
	held, from := ps.heldCoverage, ps.resetFrom
	env.c.mu.Unlock()
	if held != nil || from != 0 {
		t.Fatalf("after a failed Activate: held %+v resetFrom %d; nothing may change", held, from)
	}

	edge.mu.Lock()
	edge.activateErr = nil
	edge.mu.Unlock()
	env.submit(Query{Text: "continue again", Explicit: "edge"})
	env.idle()
	acts := edge.activations()
	if len(acts) != 1 || acts[0].Sync == nil || !acts[0].Sync.Reset {
		t.Fatalf("the clear was not applied: %+v", acts)
	}
	env.c.mu.Lock()
	held, from = ps.heldCoverage, ps.resetFrom
	env.c.mu.Unlock()
	if held == nil || held.Through != 3 || from != 4 {
		t.Fatalf("after the clear: held %+v resetFrom %d", held, from)
	}
}

// Interrupting the routing of a query also ends the older inputs it would
// have carried as context, each with its own marker.
func TestCoordinatorInterruptRoutingEndsContextInputs(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	release := make(chan struct{})
	edge.push(gate(release, replyWith("first done")))
	routing := make(chan struct{}, 4)
	var calls atomic.Int32
	router := &fakeRouter{fn: func(ctx context.Context, _ *RouteRequest) (RouteDecision, error) {
		if calls.Add(1) == 1 {
			return RouteDecision{Target: "edge"}, nil
		}
		routing <- struct{}{}
		<-ctx.Done()
		return RouteDecision{}, ctx.Err()
	}}
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = router }, edge, cloud)
	env.say("first")
	waitStarted(t, edge)
	second := env.say("second")
	third := env.say("third")
	close(release)
	<-routing
	env.c.Interrupt()
	env.idle()
	var based []uint64
	for _, m := range findEntries(env.log(), KindMarker, MarkerInterrupted) {
		based = append(based, m.BasedOn)
	}
	if !reflect.DeepEqual(based, []uint64{second, third}) {
		t.Fatalf("interrupted markers based on %v, want [%d %d]", based, second, third)
	}
	env.c.mu.Lock()
	defer env.c.mu.Unlock()
	if len(env.c.inputs) != 0 {
		t.Fatalf("%d inputs still waiting", len(env.c.inputs))
	}
}

// A cloud participant imports what it lacks in full, whatever its window:
// nothing is cut, skipped or compacted on the way in, so its peer summary
// excludes only other participants' entries past its snapshot's cursor.
func TestCoordinatorCloudImportsInFull(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	for i := 1; i <= 5; i++ {
		mustAppend(t, store, nil, Entry{Kind: KindArtifact, Origin: Origin{Participant: "edge"},
			Content: textBlocks(fmt.Sprintf("EDGE-%d ", i) + strings.Repeat("e", 110))})
	}
	edge := newFakePart("edge", ZoneEdge)
	cloud := newFakeForker("cloud")
	cloud.desc.Window = 400
	cloud.state.InputTokens = 150
	cloud.push(grow(500, "plan ready"))
	env := newCoEnv(t, store, func(cfg *Config) {
		cfg.Router = cloudRouter()
		cfg.Summaries.PeerEvery = 100
	}, edge, cloud)
	env.say("plan") // #6, owner_change #7, reply #8
	env.idle()

	syncs := cloud.appliedSyncs()
	if len(syncs) != 1 {
		t.Fatalf("want the cloud's import in one Sync: %+v", syncs)
	}
	for i := 1; i <= 5; i++ {
		if want := fmt.Sprintf("EDGE-%d %s", i, strings.Repeat("e", 110)); !containsText(syncTexts(&syncs[0]), want) {
			t.Fatalf("the cloud did not import EDGE-%d in full: %q", i, syncTexts(&syncs[0]))
		}
	}
	if n := cloud.compactCount(); n != 0 {
		t.Fatalf("the cloud's import compacted %d times", n)
	}
	sums := findEntries(env.log(), KindSummary, "")
	if len(sums) != 1 {
		t.Fatalf("summaries = %+v", sums)
	}
	if got := sums[0].Coverage.Excluded; !reflect.DeepEqual(got, []uint64{7}) {
		t.Fatalf("coverage excludes %v, want [7]", got)
	}
}

// A wake that fails writes its markers and does not fail over: the query
// it would pass on was already answered.
func TestCoordinatorFailedWakeDoesNotFailOver(t *testing.T) {
	edge, cloud := edgeCloud()
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "count the photos", true); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("ORIGINAL answer")
	}, func(*fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("cloud down"), ToolCalls: 1}
	})
	edge.push(replyWith("42 photos"))
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("ORIGINAL-Q1 please")
	env.idle()

	for _, a := range edge.activations() {
		if strings.Contains(a.Input, "ORIGINAL-Q1") {
			t.Fatalf("the answered query was failed over to the edge: %q", a.Input)
		}
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("edge ran %d times", n)
	}
	log := env.log()
	rep := findEntries(log, KindReport, "")
	if len(rep) != 1 {
		t.Fatalf("reports = %+v", rep)
	}
	if m := findEntries(log, KindMarker, MarkerUnknownEffects); len(m) != 1 || m[0].BasedOn != rep[0].Seq {
		t.Fatalf("unknown_effects = %+v", m)
	}
	for _, oc := range findEntries(log, KindOwnerChange, "") {
		if oc.Owner.Reason == ReasonFailover {
			t.Fatalf("failover after a failed wake: %+v", oc.Owner)
		}
	}
	if owner, _ := env.c.Owner(); owner != "cloud" {
		t.Fatalf("owner = %s", owner)
	}
	checkNotStranded(t, env, map[string]*fakePart{"edge": edge, "cloud": cloud})
}

// A wake that fails does not fail over, so a user message that steered it
// is queued again and gets a cycle of its own instead of being lost. The
// failed marker records it, and recovery would leave it open.
func TestCoordinatorFailedWakeQueuesItsSteering(t *testing.T) {
	edge, cloud := edgeCloud()
	cloud.started = make(chan Activation, 8)
	release := make(chan struct{})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "count the photos", true); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked the edge")
	}, gate(release, func(r *fakeRun) *ActivationEnd {
		r.pending()
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("cloud down")}
	}), replyWith("the album is sorted"))
	edge.push(replyWith("42 photos"))
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("count my photos")
	waitStarted(t, cloud)
	wake := waitStarted(t, cloud)
	steer := env.say("then sort the album")
	close(release)
	env.idle()

	if wake.Trigger.Kind != KindReport {
		t.Fatalf("second cloud activation = %+v", wake)
	}
	log := env.log()
	failed := findEntries(log, KindMarker, MarkerFailed)
	if len(failed) != 1 || !slices.Equal(failed[0].Steered, []uint64{steer}) {
		t.Fatalf("failed markers = %+v", failed)
	}
	acts := cloud.activations()
	if len(acts) != 3 || acts[2].Trigger.Seq != steer || acts[2].Input != "then sort the album" {
		t.Fatalf("cloud activations = %+v", acts)
	}
	chains := map[uint64]*reqChain{steer: {root: &Entry{Seq: steer}, leaf: steer, last: steer}}
	recordChainEntry(chains, &failed[0], nil)
	if chains[steer].done() {
		t.Fatal("recovery takes a failed wake's steering for answered")
	}
}

// A wake that cannot start leaves its report for the owner's next
// foreground activation; it does not retry the wake in the same cycle.
func TestCoordinatorWakeThatCannotStartWaits(t *testing.T) {
	edge, cloud := edgeCloud()
	var cloudDown atomic.Bool
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "scan", true); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("scanning")
	}, replyWith("next answer"))
	edge.push(func(r *fakeRun) *ActivationEnd {
		cloudDown.Store(true)
		return r.reply("SCAN-RESULT")
	})
	env := newCoEnv(t, nil, func(cfg *Config) {
		cfg.Router = cloudRouter()
		cfg.Available = func(name string) bool { return name != "cloud" || !cloudDown.Load() }
	}, edge, cloud)
	env.say("scan please")
	env.idle()
	if n := len(cloud.activations()); n != 1 {
		t.Fatalf("cloud ran %d times while unavailable", n)
	}
	if n := len(edge.activations()); n != 1 {
		t.Fatalf("edge ran %d times; a failed wake must not fail over", n)
	}
	env.c.mu.Lock()
	waiting := len(env.c.parts["cloud"].notify)
	wakes := env.c.cycle.wakes
	env.c.mu.Unlock()
	if waiting != 1 {
		t.Fatalf("%d reports wait for the cloud", waiting)
	}
	if wakes != 1 {
		t.Fatalf("the owner was woken %d times for one report; a failed wake must not be retried in the cycle", wakes)
	}

	cloudDown.Store(false)
	env.say("what did the scan find?")
	env.idle()
	ca := cloud.activations()
	if len(ca) != 2 || !containsText(syncTexts(ca[1].Sync), "SCAN-RESULT") {
		t.Fatalf("the report did not reach the owner's next activation: %+v", ca)
	}
	env.c.mu.Lock()
	waiting = len(env.c.parts["cloud"].notify)
	env.c.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d reports still wait after a foreground activation imported them", waiting)
	}
}

// reportSignal closes a channel once a task report from a participant is
// appended.
type reportSignal struct {
	Store
	from string
	once sync.Once
	done chan struct{}
}

func (s *reportSignal) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	seq, err := s.Store.Append(ctx, entries, g)
	for i := range entries {
		if e := &entries[i]; err == nil && e.Kind == KindReport && e.Task != nil && e.Origin.Participant == s.from {
			s.once.Do(func() { close(s.done) })
		}
	}
	return seq, err
}

// A Notify report meant for the idle owner is not consumed by the owner's
// own background activation: the owner is still woken to answer it.
func TestCoordinatorNotifyReportWakesOwnerAfterBackground(t *testing.T) {
	edge, cloud := edgeCloud()
	store := &reportSignal{Store: NewMemoryStore(WithClock(fixedClock())), from: "cloud", done: make(chan struct{})}
	bRunning := make(chan struct{})
	var bPending *Sync
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "task A", true); err != nil {
			t.Errorf("assign A: %v", err)
		}
		return r.reply("asked the cloud")
	}, func(r *fakeRun) *ActivationEnd { // background task B
		close(bRunning)
		select {
		case <-store.done:
		case <-time.After(testTimeout):
			t.Error("report A was never appended")
		}
		bPending = r.pending()
		return r.reply("B result")
	}, replyWith("handled A"))
	cloud.push(func(r *fakeRun) *ActivationEnd { // background task A
		if _, _, err := r.assign("edge", "task B", false); err != nil {
			t.Errorf("assign B: %v", err)
		}
		<-bRunning
		return r.reply("A result")
	})
	env := newCoEnv(t, store, nil, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()

	if bPending != nil && containsText(syncTexts(bPending), "A result") {
		t.Fatalf("report A was delivered into background task B: %q", syncTexts(bPending))
	}
	acts := edge.activations()
	if len(acts) != 3 || !acts[2].Foreground || acts[2].Trigger.Kind != KindReport || !strings.Contains(acts[2].Input, "A result") {
		t.Fatalf("the idle owner was not woken by report A: %d activations %+v", len(acts), acts)
	}
	checkNotStranded(t, env, map[string]*fakePart{"edge": edge, "cloud": cloud})
}

// A Notify report that a background activation of the owner imported as
// context (its trigger came after the report) still wakes the owner: wakes
// follow the reports no foreground activation received, not the cursor.
func TestCoordinatorNotifyReportImportedByBackgroundStillWakes(t *testing.T) {
	edge, cloud := edgeCloud()
	bAssigned := make(chan struct{})
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "task A", true); err != nil {
			t.Errorf("assign A: %v", err)
		}
		return r.reply("asked the cloud")
	}, func(r *fakeRun) *ActivationEnd { // background task X, running while report A arrives
		if _, _, err := r.assign("cloud", "task C", false); err != nil {
			t.Errorf("assign C: %v", err)
		}
		select {
		case <-bAssigned:
		case <-time.After(testTimeout):
			t.Error("task B was never assigned")
		}
		return r.reply("X result")
	}, replyWith("B result"), replyWith("handled A"))
	cloud.push(func(r *fakeRun) *ActivationEnd { // background task A
		if _, _, err := r.assign("edge", "task X", false); err != nil {
			t.Errorf("assign X: %v", err)
		}
		return r.reply("A result")
	}, func(r *fakeRun) *ActivationEnd { // background task C, after report A
		if _, _, err := r.assign("edge", "task B", false); err != nil {
			t.Errorf("assign B: %v", err)
		}
		close(bAssigned)
		return r.reply("C result")
	})
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Limits.MaxAssignDepth = 5 }, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()

	acts := edge.activations()
	if len(acts) != 4 {
		t.Fatalf("edge ran %d times: %+v", len(acts), acts)
	}
	b, wake := acts[2], acts[3]
	if b.Foreground || !containsText(syncTexts(b.Sync), "A result") {
		t.Fatalf("background task B did not import report A as context: %+v", b)
	}
	if !wake.Foreground || wake.Trigger.Kind != KindReport || wake.Trigger.Seq >= b.Trigger.Seq || !strings.Contains(wake.Input, "A result") {
		t.Fatalf("the idle owner was not woken by report A: %+v", wake)
	}
	checkNotStranded(t, env, map[string]*fakePart{"edge": edge, "cloud": cloud})
}

// A background activation receives through Pending the reports of tasks it
// assigned itself, and they do not wake the owner afterwards.
func TestCoordinatorBackgroundPendingGetsOwnTaskReport(t *testing.T) {
	edge, cloud := edgeCloud()
	store := &reportSignal{Store: NewMemoryStore(WithClock(fixedClock())), from: "edge", done: make(chan struct{})}
	var got *Sync
	cloud.push(func(r *fakeRun) *ActivationEnd { // background task from the edge
		if _, _, err := r.assign("edge", "subtask", true); err != nil {
			t.Errorf("assign: %v", err)
		}
		select {
		case <-store.done:
		case <-time.After(testTimeout):
			t.Error("the subtask report was never appended")
		}
		got = r.pending()
		return r.reply("task done")
	})
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("cloud", "task", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked")
	}, replyWith("SUBTASK-RESULT"))
	env := newCoEnv(t, store, func(cfg *Config) { cfg.Limits.MaxAssignDepth = 3 }, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()
	if got == nil || !containsText(syncTexts(got), "SUBTASK-RESULT") {
		t.Fatalf("the assigning activation did not receive its task's report: %+v", got)
	}
	if n := len(cloud.activations()); n != 1 {
		t.Fatalf("cloud ran %d times; the report was already delivered", n)
	}
}

// A Transfer the owner makes after it was interrupted commits nothing: no
// owner change, no answer from the target.
func TestCoordinatorInterruptDuringTransferCommitsNothing(t *testing.T) {
	edge, cloud := edgeCloud()
	started := make(chan struct{})
	release := make(chan struct{})
	var transferErr error
	edge.push(func(r *fakeRun) *ActivationEnd {
		r.say("handing over")
		close(started)
		<-release
		_, transferErr = r.host().Transfer(r.ctx, TransferRequest{From: Origin{Participant: "edge"}, Epoch: r.act.Epoch,
			To: "cloud", Brief: testBrief(), Text: r.text()})
		return &ActivationEnd{Reason: types.ReplyInterrupted, Text: r.text()}
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	q := env.submit(Query{Text: "go", Explicit: "edge"})
	<-started
	env.c.Interrupt()
	close(release)
	env.idle()

	if !errors.Is(transferErr, ErrInterrupted) || !errors.Is(transferErr, context.Canceled) {
		t.Fatalf("Transfer after Interrupt = %v, want ErrInterrupted wrapping context.Canceled", transferErr)
	}
	for _, oc := range findEntries(env.log(), KindOwnerChange, "") {
		if oc.Owner.Reason == ReasonTransfer {
			t.Fatalf("the transfer was committed after the interrupt: %+v", oc.Owner)
		}
	}
	if n := len(cloud.activations()); n != 0 {
		t.Fatalf("cloud ran %d times", n)
	}
	if m := findEntries(env.log(), KindMarker, MarkerInterrupted); len(m) != 1 || m[0].BasedOn != q {
		t.Fatalf("interrupted markers = %+v", m)
	}
}

// Assign, Report and Publish with a canceled context write nothing.
func TestCoordinatorCanceledWritesCommitNothing(t *testing.T) {
	edge, cloud := edgeCloud()
	errs := map[string]error{}
	var before, after int
	var env *coEnv
	edge.push(func(r *fakeRun) *ActivationEnd {
		h := r.host()
		before = len(env.log())
		ctx := canceledCtx()
		_, _, errs["assign"] = h.Assign(ctx, AssignRequest{From: Origin{Participant: "edge"}, To: "cloud", Text: "x"})
		_, errs["report"] = h.Report(ctx, ReportRequest{From: Origin{Participant: "edge"}, Text: "x", To: []string{"cloud"}})
		_, errs["publish"] = h.Publish(ctx, PublishRequest{From: Origin{Participant: "edge"}, Refs: []ArtifactRef{{URI: "x"}}})
		_, errs["transfer"] = h.Transfer(ctx, TransferRequest{From: Origin{Participant: "edge"}, Epoch: r.act.Epoch, To: "cloud", Brief: testBrief()})
		after = len(env.log())
		var none context.Context
		if _, err := h.Report(none, ReportRequest{From: Origin{Participant: "edge"}, Text: "no context", To: []string{"cloud"}}); err != nil {
			t.Errorf("Report without a context: %v", err)
		}
		return r.reply("done")
	})
	env = newCoEnv(t, nil, nil, edge, cloud)
	env.submit(Query{Text: "go", Explicit: "edge"})
	env.idle()
	for name, err := range errs {
		if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a canceled context: %v", name, err)
		}
	}
	if len(errs) != 4 || after != before {
		t.Fatalf("errors %v; %d entries written", errs, after-before)
	}
}

// Text shown before a Transfer reaches the user even when the Coordinator
// read it from the activation only after the commit: in order, before the
// Transfer's entries and before the new owner's events.
func TestCoordinatorTransferForwardsEventsShownBefore(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.buf = 8
	transferred := make(chan struct{})
	edge.push(func(r *fakeRun) *ActivationEnd {
		for _, s := range []string{"a", "b", "c"} {
			r.say(s)
		}
		if _, err := r.host().Transfer(r.ctx, TransferRequest{From: Origin{Participant: "edge"}, Epoch: r.act.Epoch,
			To: "cloud", Brief: testBrief(), Text: r.text(), EventsBefore: 3}); err != nil {
			t.Errorf("transfer: %v", err)
		}
		close(transferred)
		r.say("late")
		return &ActivationEnd{Reason: types.ReplyHandedOff}
	})
	cloud.push(replyWith("cloud answer"))
	c, err := NewCoordinator(context.Background(), Config{
		Store: NewMemoryStore(), Participants: []Participant{edge, cloud}, Limits: Limits{EventBuffer: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Submit(context.Background(), Query{Text: "go", Explicit: "edge"}); err != nil {
		t.Fatal(err)
	}
	<-transferred // the user has read nothing yet
	var seen []string
	for ev := range c.Events() {
		switch {
		case ev.Kind == EventAgent:
			d := ev.Agent.(event.TextBlockDeltaEvent).Delta
			if ev.Participant == "edge" && !ev.Foreground {
				t.Errorf("edge delta %q is not marked foreground", d)
			}
			seen = append(seen, ev.Participant+":"+d)
		case ev.Kind == EventEntry && ev.Entry.Kind == KindReply:
			seen = append(seen, "reply:"+ev.Entry.Text())
		}
		if ev.Kind == EventEntry && ev.Entry.Kind == KindReply && ev.Participant == "cloud" {
			break
		}
	}
	want := []string{"edge:a", "edge:b", "edge:c", "reply:abc", "cloud:cloud answer", "reply:cloud answer"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("the user saw %q, want %q", seen, want)
	}
}

// Recovery counts only the report that ended a task, restores lastSealed
// from task reports and keeps Notify reports past the cursor waiting.
func TestCoordinatorRecoveryTaskReports(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	assign := func(id string) Entry {
		return Entry{Kind: KindAssign, Origin: Origin{Participant: "cloud"}, To: []string{"edge"},
			Task: &Task{ID: id}, Content: textBlocks("do " + id)}
	}
	taskReport := func(id, key string, end types.ReplyFinishedReason, notify bool) Entry {
		return Entry{Kind: KindReport, Origin: Origin{Participant: "edge"}, To: []string{"cloud"},
			Task: &Task{ID: id, Notify: notify, End: end}, Notify: notify, Content: textBlocks("about " + id), DedupKey: key}
	}
	mustAppend(t, store, nil,
		assign("t-1"),                    // #1
		taskReport("t-1", "", "", false), // #2: progress only
		assign("t-2"),                    // #3
		taskReport("t-2", "", types.ReplyCompleted, true),                                               // #4: the final report, Notify; End alone marks it
		Entry{Kind: KindArtifact, Origin: Origin{Participant: "edge"}, Refs: []ArtifactRef{{URI: "f"}}}, // #5
	)
	edge, cloud := edgeCloud()
	cloud.setState(func(s *ParticipantState) { s.Cursor = 3 })
	env := newCoEnv(t, store, nil, edge, cloud)
	orphans := findEntries(env.log(), KindReport, "")
	var ids []string
	for _, o := range orphans {
		if strings.HasPrefix(o.DedupKey, orphanPrefix) {
			ids = append(ids, o.Task.ID)
			if o.Task.End != TaskUnfinished {
				t.Fatalf("orphan report #%d end = %q, want %q", o.Seq, o.Task.End, TaskUnfinished)
			}
		}
	}
	if !reflect.DeepEqual(ids, []string{"t-1"}) {
		t.Fatalf("orphan reports for %v, want only t-1", ids)
	}
	env.c.mu.Lock()
	defer env.c.mu.Unlock()
	if got := env.c.parts["edge"].lastSealed; got != 4 {
		t.Fatalf("edge lastSealed = %d, want the task report #4", got)
	}
	if n := env.c.parts["edge"].lastActivity; n != 5 {
		t.Fatalf("edge lastActivity = %d", n)
	}
	if _, ok := env.c.parts["cloud"].notify[4]; !ok || len(env.c.parts["cloud"].notify) != 1 {
		t.Fatalf("cloud notify = %v, want #4 waiting", env.c.parts["cloud"].notify)
	}
}

// A participant whose cursor is past the log head cannot be recovered over
// that log.
func TestCoordinatorRecoveryRejectsCursorPastHead(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("one"), userInput("two"))
	edge, cloud := edgeCloud()
	edge.setState(func(s *ParticipantState) { s.Cursor = 9 })
	_, err := NewCoordinator(t.Context(), Config{Store: store, Participants: []Participant{edge, cloud}})
	if err == nil {
		t.Fatal("NewCoordinator accepted a cursor past the log head")
	}
	for _, want := range []string{"edge", "cursor 9", "#2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// A failed Activate gives back the range it marked as seen.
func TestCoordinatorActivateErrorRestoresSeen(t *testing.T) {
	edge, cloud := edgeCloud()
	edge.activateErr = errors.New("model not loaded")
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("hi")
	env.idle()
	env.c.mu.Lock()
	seen := env.c.parts["edge"].seen
	env.c.mu.Unlock()
	if seen != 0 {
		t.Fatalf("edge seen = %d after a failed Activate, want 0", seen)
	}
}

// failOwnerChange fails the first append that holds an owner change.
type failOwnerChange struct {
	Store
	failed atomic.Bool
}

func (s *failOwnerChange) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	for i := range entries {
		if entries[i].Kind == KindOwnerChange && s.failed.CompareAndSwap(false, true) {
			return 0, errors.New("disk full")
		}
	}
	return s.Store.Append(ctx, entries, g)
}

// An input whose owner change cannot be recorded stays parked, so the next
// owner's activation still receives it.
func TestCoordinatorUnrecordedOwnerChangeParksInput(t *testing.T) {
	store := &failOwnerChange{Store: NewMemoryStore(WithClock(fixedClock()))}
	edge, cloud := edgeCloud()
	edge.push(replyWith("answered both"))
	env := newCoEnv(t, store, nil, edge, cloud)
	first := env.submit(Query{Text: "first", Explicit: "edge"})
	env.idle()
	env.c.mu.Lock()
	parked := len(env.c.inputs) == 1 && env.c.inputs[0].parked && env.c.inputs[0].entry.Seq == first
	env.c.mu.Unlock()
	if n := len(edge.activations()); n != 0 || !parked {
		t.Fatalf("after the failed owner change: %d activations, input parked %v", n, parked)
	}

	env.submit(Query{Text: "second", Explicit: "edge"})
	env.idle()
	acts := edge.activations()
	if len(acts) != 1 || !containsText(syncTexts(acts[0].Sync), fmt.Sprintf("[#%d user]", first)) {
		t.Fatalf("the parked input did not reach the next owner: %+v", acts)
	}
	env.c.mu.Lock()
	defer env.c.mu.Unlock()
	if len(env.c.inputs) != 0 {
		t.Fatalf("%d inputs still waiting", len(env.c.inputs))
	}
}

// A clear the edge cannot apply leaves the Coordinator's view as it was: the
// edge holds no new coverage and is cleared again at the next takeover, with
// the whole import in that one Sync.
func TestCoordinatorFailedClearKeepsTheView(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("plan"))
	mustAppend(t, store, &Guard{}, ownerChange("", "cloud"))
	rep := replyFrom("cloud", "the plan")
	rep.Origin.Path = ""
	mustAppend(t, store, &Guard{Owner: "cloud", Epoch: 1}, rep)
	mustAppend(t, store, nil, Entry{
		Kind: KindSummary, Origin: Origin{Participant: "cloud"},
		Summary: &SummaryMeta{Target: "edge"}, Coverage: &Coverage{Through: 3}, Content: textBlocks("SUMMARY"),
	}) // #4
	for i := 1; i <= 8; i++ {
		mustAppend(t, store, nil, Entry{Kind: KindArtifact,
			Content: textBlocks(fmt.Sprintf("CHUNK-%d ", i) + strings.Repeat("b", 300))})
	}
	edge := smallEdge()
	edge.syncErr = errors.New("checkpoint save failed")
	_, cloud := edgeCloud()
	cloud.push(replyWith("taken over"))
	env := newCoEnv(t, store, nil, edge, cloud)

	env.submit(Query{Text: "continue", Explicit: "edge"})
	env.idle()
	if n := len(edge.activations()); n != 0 || len(edge.appliedSyncs()) != 0 {
		t.Fatalf("want nothing applied and no reply: %d activations, syncs %+v", n, edge.appliedSyncs())
	}
	env.c.mu.Lock()
	ps := env.c.parts["edge"]
	held, from := ps.heldCoverage, ps.resetFrom
	env.c.mu.Unlock()
	if held != nil || from != 0 {
		t.Fatalf("after the failed clear: held %+v resetFrom %d", held, from)
	}

	edge.mu.Lock()
	edge.syncErr = nil
	edge.mu.Unlock()
	edge.push(replyWith("back"))
	env.submit(Query{Text: "continue again", Explicit: "edge"})
	env.idle()
	again := edge.appliedSyncs()
	if len(again) != 1 || !again[0].Reset {
		t.Fatalf("the next takeover must clear again in one Sync: %+v", again)
	}
	texts := syncTexts(&again[0])
	if !containsText(texts, "CHUNK-1") || !containsText(texts, "CHUNK-8") {
		t.Fatalf("the clear must bring the whole import: %q", texts)
	}
	env.c.mu.Lock()
	from = ps.resetFrom
	env.c.mu.Unlock()
	if from != 4 {
		t.Fatalf("after the clear: resetFrom %d", from)
	}
}
