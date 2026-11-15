package thread

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// TestCoordinatorRecovery: a request the stopped process was answering is
// resumed on its owner. The restarted marker carries the host's note, the
// input quotes the request and warns about unknown effects, the import holds
// what the log says. A task without a final report is reported unfinished,
// task IDs continue, and a second restart finds nothing new.
func TestCoordinatorRecovery(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("answered"))
	mustAppend(t, store, &Guard{}, ownerChange("", "cloud"))
	rep := replyFrom("cloud", "done")
	rep.BasedOn = 1
	mustAppend(t, store, &Guard{Owner: "cloud", Epoch: 1}, rep)
	mustAppend(t, store, nil,
		userInput("unanswered"),
		Entry{Kind: KindAssign, Origin: Origin{Participant: "cloud"}, To: []string{"edge"},
			Task: &Task{ID: "t-7"}, Content: textBlocks("do it")},
		Entry{Kind: KindArtifact, Refs: []ArtifactRef{{URI: "file:///notes"}}},
		Entry{Kind: KindProgress, Origin: Origin{Participant: "cloud"}, BasedOn: 4, Run: "a-1",
			Content: textBlocks("1. [in_progress] look it up"), DedupKey: "progress:a-1:1"},
	)
	edge, cloud := edgeCloud()
	edge.setState(func(s *ParticipantState) { s.Cursor = 6 })
	cloud.setState(func(s *ParticipantState) { s.Cursor = 4 })
	var taskID string
	cloud.push(func(r *fakeRun) *ActivationEnd {
		taskID, _, _ = r.assign("edge", "next task", false)
		return r.reply("checked, done now")
	})
	env := newCoEnv(t, store, func(cfg *Config) { cfg.RecoveryNote = "power loss\nat 02:00" }, edge, cloud)
	env.idle()

	log := env.log()
	if got := kindsOf(log[7:9]); !reflect.DeepEqual(got, []string{"marker:restarted", "report"}) {
		t.Fatalf("recovery entries = %v", got)
	}
	mk := log[7]
	if mk.BasedOn != 4 || mk.DedupKey != "restart:4" || !strings.Contains(mk.Marker.Detail, "#4") ||
		!strings.HasSuffix(mk.Marker.Detail, "Host: power loss\nat 02:00") {
		t.Fatalf("restarted marker = %+v %+v", mk, mk.Marker)
	}
	orphan := log[8]
	if orphan.Task == nil || orphan.Task.ID != "t-7" || !reflect.DeepEqual(orphan.To, []string{"cloud"}) || orphan.BasedOn != 5 {
		t.Fatalf("orphan report = %+v", orphan)
	}
	acts := cloud.activations()
	if len(acts) != 1 || !acts[0].Foreground || acts[0].Trigger.Seq != mk.Seq {
		t.Fatalf("cloud activations = %+v", acts)
	}
	wantInput := `[The session restarted while the user's request #4 "unanswered" was being answered (host: power loss at 02:00). ` +
		`Work done before the restart may or may not have taken effect: check the actual state before you redo anything. ` +
		`The latest progress is #7. Continue the request.]`
	if acts[0].Input != wantInput {
		t.Fatalf("resume input:\n%s\nwant\n%s", acts[0].Input, wantInput)
	}
	if imported := strings.Join(syncTexts(acts[0].Sync), "\n"); !strings.Contains(imported, "[#7 progress of cloud]") {
		t.Fatalf("the resumed owner does not see its own progress:\n%s", imported)
	}
	var resumed *Entry
	for i := range log {
		if log[i].Kind == KindReply && log[i].Text() == "checked, done now" {
			resumed = &log[i]
		}
	}
	if resumed == nil || resumed.BasedOn != mk.Seq {
		t.Fatalf("resumed reply = %+v", resumed)
	}
	if taskID != "t-8" {
		t.Fatalf("task id after recovery = %q", taskID)
	}
	if owner, epoch := env.c.Owner(); owner != "cloud" || epoch != 1 {
		t.Fatalf("owner = %s %d", owner, epoch)
	}
	env.c.mu.Lock()
	waiting := len(env.c.inputs)
	env.c.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("waiting inputs %d", waiting)
	}

	// A second restart finds nothing new to recover.
	edge2, cloud2 := edgeCloud()
	before := len(env.log())
	c2, err := NewCoordinator(t.Context(), Config{Store: store, Participants: []Participant{edge2, cloud2}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if after, _ := store.Head(t.Context()); after.LastSeq != uint64(before) {
		t.Fatalf("second recovery appended %d entries", after.LastSeq-uint64(before))
	}
}

// TestCoordinatorRecoveryAfterAFailover: the process stopped after the cloud
// failed over to the edge and before the edge answered. The edge resumes
// the request; its input points at the cloud's failure and at the newest
// progress, its own, which its first import brings back although it wrote
// it past its checkpoint's cursor.
func TestCoordinatorRecoveryAfterAFailover(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("plan my day"))
	mustAppend(t, store, &Guard{}, ownerChange("", "cloud"))
	failed := markerEntry(MarkerFailed, 1, "cloud failed: cloud model unreachable")
	failed.DedupKey = "failed:a-1"
	fo := ownerChange("cloud", "edge")
	fo.BasedOn = 1
	fo.Owner.Reason = ReasonFailover
	mustAppend(t, store, nil,
		Entry{Kind: KindProgress, Origin: Origin{Participant: "cloud"}, BasedOn: 1, Run: "a-1",
			Content: textBlocks("1. [in_progress] draft the morning"), DedupKey: "progress:a-1:1"},
		failed)
	mustAppend(t, store, &Guard{Owner: "cloud", Epoch: 1}, fo)
	mustAppend(t, store, nil,
		Entry{Kind: KindProgress, Origin: Origin{Participant: "edge"}, BasedOn: 5, Run: "a-2",
			Content: textBlocks("1. [in_progress] read the calendar"), DedupKey: "progress:a-2:1"})
	edge, cloud := edgeCloud()
	edge.setState(func(s *ParticipantState) { s.Cursor = 2 })
	edge.push(replyWith("Here is your plan."))
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()

	acts := edge.activations()
	if len(acts) != 1 || acts[0].Trigger.Kind != KindMarker || acts[0].Trigger.BasedOn != 5 {
		t.Fatalf("edge activations = %+v", acts)
	}
	in := acts[0].Input
	for _, want := range []string{`request #1 "plan my day"`, "A failure before it is recorded in #4", "The latest progress is #6"} {
		if !strings.Contains(in, want) {
			t.Fatalf("resume input %q lacks %q", in, want)
		}
	}
	imported := strings.Join(syncTexts(acts[0].Sync), "\n")
	for _, want := range []string{"[#3 progress of cloud]", "[#4 marker failed]", "[#6 progress of edge]"} {
		if !strings.Contains(imported, want) {
			t.Fatalf("the edge's import lacks %q:\n%s", want, imported)
		}
	}
	if len(cloud.activations()) != 0 {
		t.Fatal("the cloud ran after the failover")
	}
}

// TestCoordinatorRecoveryStopsAfterRepeatedRestarts: a request whose run
// stopped the process maxRestarts times is parked with a recovered marker on
// its newest restart, which ends it for the next recovery too.
func TestCoordinatorRecoveryStopsAfterRepeatedRestarts(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("crashy"))
	mustAppend(t, store, &Guard{}, ownerChange("", "cloud"))
	first := markerEntry(MarkerRestarted, 1, "restarted")
	first.DedupKey = "restart:1"
	mustAppend(t, store, nil, first)
	second := markerEntry(MarkerRestarted, 3, "restarted")
	second.DedupKey = "restart:3"
	mustAppend(t, store, nil, second)
	edge, cloud := edgeCloud()
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	log := env.log()
	if got := kindsOf(log[4:]); !reflect.DeepEqual(got, []string{"marker:recovered"}) || log[4].BasedOn != 4 || log[4].DedupKey != "recovered:4" {
		t.Fatalf("recovery entries = %v", got)
	}
	if len(cloud.activations()) != 0 {
		t.Fatal("the request ran a third time")
	}
	env.c.mu.Lock()
	parked := len(env.c.inputs) == 1 && env.c.inputs[0].parked && env.c.inputs[0].entry.Seq == 1
	env.c.mu.Unlock()
	if !parked {
		t.Fatal("the request does not wait for the next message")
	}

	// The process stops again with a task left open: the parked request
	// stays ended, and the task's report is written.
	again := NewMemoryStore(WithClock(fixedClock()))
	for _, e := range log {
		e.Seq, e.Epoch = 0, 0
		var g *Guard
		if e.Kind == KindOwnerChange {
			g = &Guard{}
		}
		mustAppend(t, again, g, e)
	}
	mustAppend(t, again, nil, assignTo("edge", "t-9"))
	edge2, cloud2 := edgeCloud()
	env2 := newCoEnv(t, again, nil, edge2, cloud2)
	env2.idle()
	if got := kindsOf(env2.log()[6:]); !reflect.DeepEqual(got, []string{"report"}) {
		t.Fatalf("second recovery wrote %v", got)
	}
	if len(cloud2.activations())+len(edge2.activations()) != 0 {
		t.Fatal("the parked request ran again")
	}
}

// assignTo is a task the cloud assigned to a participant.
func assignTo(to, id string) Entry {
	return Entry{Kind: KindAssign, Origin: Origin{Participant: "cloud"}, To: []string{to},
		Task: &Task{ID: id}, Content: textBlocks("check the sensors")}
}

// TestCoordinatorRecoveryCountsEveryRequeue: a request whose routing never
// finished gets a restarted marker of its own at every restart, also when
// the recovery writes other entries, and is parked after maxRestarts.
func TestCoordinatorRecoveryCountsEveryRequeue(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("route me"))
	first := markerEntry(MarkerRestarted, 1, "restarted")
	first.DedupKey = "requeue:1:1"
	mustAppend(t, store, nil, first, assignTo("edge", "t-7"))
	edge, cloud := edgeCloud()
	blocked := &fakeRouter{fn: func(ctx context.Context, _ *RouteRequest) (RouteDecision, error) {
		<-ctx.Done()
		return RouteDecision{}, ctx.Err()
	}}
	env := newCoEnv(t, store, func(cfg *Config) { cfg.Router = blocked }, edge, cloud)
	log := env.log()
	if got := kindsOf(log[3:]); !reflect.DeepEqual(got, []string{"marker:restarted", "report"}) ||
		log[3].BasedOn != 1 || log[3].DedupKey != "requeue:1:2" {
		t.Fatalf("second recovery wrote %v: %+v", got, log[3:])
	}

	third := NewMemoryStore(WithClock(fixedClock()))
	for _, e := range log {
		e.Seq, e.Epoch = 0, 0
		mustAppend(t, third, nil, e)
	}
	edge3, cloud3 := edgeCloud()
	env3 := newCoEnv(t, third, nil, edge3, cloud3)
	env3.idle()
	log3 := env3.log()
	if got := kindsOf(log3[5:]); !reflect.DeepEqual(got, []string{"marker:recovered"}) || log3[5].BasedOn != 1 {
		t.Fatalf("third recovery wrote %v", got)
	}
	if len(cloud3.activations())+len(edge3.activations()) != 0 {
		t.Fatal("the request was routed a third time")
	}
}

// TestCoordinatorRecoveryLeavesEndedRequests: a request the log already
// ended (interrupted, no owner, recovered before) is not resumed.
func TestCoordinatorRecoveryLeavesEndedRequests(t *testing.T) {
	for _, kind := range []MarkerKind{MarkerInterrupted, MarkerNoOwner, MarkerRecovered} {
		t.Run(string(kind), func(t *testing.T) {
			store := NewMemoryStore(WithClock(fixedClock()))
			mustAppend(t, store, nil, userInput("stop it"))
			mustAppend(t, store, &Guard{}, ownerChange("", "cloud"))
			mustAppend(t, store, nil, markerEntry(kind, 1, "ended"))
			edge, cloud := edgeCloud()
			env := newCoEnv(t, store, nil, edge, cloud)
			env.idle()
			if log := env.log(); len(log) != 3 {
				t.Fatalf("recovery wrote %v", kindsOf(log[3:]))
			}
			if len(cloud.activations())+len(edge.activations()) != 0 {
				t.Fatal("an ended request ran again")
			}
		})
	}
}

// TestCoordinatorRecoveryRoutesUnroutedRequests: requests whose routing
// never finished (there is no owner yet) ran nothing. They are routed again
// as Submit would route them: the newest with a restarted marker that counts
// the restart, the older one as its context, which the reply records as
// answered.
func TestCoordinatorRecoveryRoutesUnroutedRequests(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("old"), userInput("newest"))
	edge, cloud := edgeCloud()
	edge.push(replyWith("hi"))
	cloud.push(replyWith("hi"))
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	log := env.log()
	if got := kindsOf(log[2:3]); !reflect.DeepEqual(got, []string{"marker:restarted"}) || log[2].BasedOn != 2 || log[2].DedupKey != "requeue:2:1" {
		t.Fatalf("recovery entries = %v", kindsOf(log))
	}
	if m := findEntries(log, KindMarker, MarkerRecovered); len(m) != 0 {
		t.Fatalf("an input that never ran was marked as not run again: %+v", m)
	}
	var routed bool
	for _, a := range append(edge.activations(), cloud.activations()...) {
		if a.Trigger.Seq == 2 && a.Input == "newest" {
			routed = true
		}
	}
	if !routed {
		t.Fatal("the unrouted request was not routed again")
	}
	replies := findEntries(log, KindReply, "")
	if len(replies) != 1 || replies[0].BasedOn == 1 || !slices.Equal(replies[0].Steered, []uint64{1}) {
		t.Fatalf("the reply must record the older input it carried as context: %+v", replies)
	}
}

// TestCoordinatorRecoveryJoinsSteering: the user steered the running
// request, and the process stopped before the reply was sealed, so nothing
// records the steering. The request is resumed with the steering message
// quoted after it; neither gets a recovered marker, and the resumed reply
// records the steering as answered. A later recovery would not take it for a
// request of its own.
func TestCoordinatorRecoveryJoinsSteering(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("plan my trip"))
	oc := ownerChange("", "cloud")
	oc.BasedOn = 1
	mustAppend(t, store, &Guard{}, oc)
	mustAppend(t, store, nil, userInput("use metric units"))
	edge, cloud := edgeCloud()
	cloud.push(replyWith("planned, in metric"))
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	log := env.log()
	if m := findEntries(log, KindMarker, MarkerRecovered); len(m) != 0 {
		t.Fatalf("recovered markers = %+v", m)
	}
	restarted := findEntries(log, KindMarker, MarkerRestarted)
	if len(restarted) != 1 || restarted[0].BasedOn != 1 || !strings.Contains(restarted[0].Marker.Detail, "while #1, #3 was being answered") {
		t.Fatalf("restarted markers = %+v", restarted)
	}
	acts := cloud.activations()
	want := `[The session restarted while the user's messages #1 "plan my trip", #3 "use metric units" were being answered.`
	if len(acts) != 1 || !strings.HasPrefix(acts[0].Input, want) {
		t.Fatalf("cloud activations = %+v", acts)
	}
	replies := findEntries(log, KindReply, "")
	if len(replies) != 1 || !slices.Equal(replies[0].Steered, []uint64{3}) {
		t.Fatalf("the resumed reply must record the steering: %+v", replies)
	}
}

// transferLog writes a request the edge took and handed to the cloud with a
// Transfer, then the cloud's progress: #1 the request, #2 the router's owner
// change, #3 the edge's progress, #4 the Transfer, #5 the cloud's progress.
func transferLog(t *testing.T, request *Entry) *MemoryStore {
	t.Helper()
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, *request)
	oc := ownerChange("", "edge")
	oc.BasedOn = 1
	mustAppend(t, store, &Guard{}, oc)
	mustAppend(t, store, nil, Entry{Kind: KindProgress, Origin: Origin{Participant: "edge"},
		BasedOn: 1, Run: "a-1", Content: textBlocks("1. [in_progress] check the calendar"), DedupKey: "progress:a-1:1"})
	tr := ownerChange("edge", "cloud")
	tr.BasedOn, tr.Origin = 1, Origin{Participant: "edge"}
	tr.Owner.Reason = ReasonTransfer
	tr.Owner.Brief = &Brief{Goal: "book the garage service for Friday", Remaining: []string{"pick a slot"}}
	tr.DedupKey = "transfer:a-1"
	mustAppend(t, store, &Guard{Owner: "edge", Epoch: 1}, tr)
	mustAppend(t, store, nil, Entry{Kind: KindProgress, Origin: Origin{Participant: "cloud"},
		BasedOn: 4, Run: "a-2", Content: textBlocks("1. [in_progress] find a slot"), DedupKey: "progress:a-2:1"})
	return store
}

// TestCoordinatorRecoveryResumesATransfer: the process stopped while the
// cloud answered a request the edge had handed to it with a Transfer. The
// cloud resumes it, with the inputs that came later joined; the edge does
// not run.
func TestCoordinatorRecoveryResumesATransfer(t *testing.T) {
	request := userInput("book the garage service")
	store := transferLog(t, &request)
	mustAppend(t, store, nil, userInput("Friday morning if possible"), userInput("and send me the invoice")) // #6, #7
	edge, cloud := edgeCloud()
	edge.setState(func(s *ParticipantState) { s.Cursor = 3 })
	cloud.push(replyWith("booked for Friday 9:00"))
	env := newCoEnv(t, store, func(cfg *Config) { cfg.RecoveryNote = "power loss" }, edge, cloud)
	env.idle()

	acts := cloud.activations()
	if len(acts) != 1 || !acts[0].Foreground {
		t.Fatalf("cloud activations = %+v", acts)
	}
	mk := acts[0].Trigger
	if mk.Kind != KindMarker || mk.Marker.Kind != MarkerRestarted || mk.BasedOn != 4 || mk.DedupKey != "restart:4" ||
		!strings.Contains(mk.Marker.Detail, "#1, #6, #7") {
		t.Fatalf("resume trigger = %+v %+v", mk, mk.Marker)
	}
	wantInput := `[The session restarted while the user's messages #1 "book the garage service", ` +
		`#6 "Friday morning if possible", #7 "and send me the invoice" were being answered (host: power loss). ` +
		`Work done before the restart may or may not have taken effect: check the actual state before you redo anything. ` +
		`The latest progress is #5. Continue the request.]`
	if acts[0].Input != wantInput {
		t.Fatalf("resume input:\n%s\nwant\n%s", acts[0].Input, wantInput)
	}
	imported := strings.Join(syncTexts(acts[0].Sync), "\n")
	for _, want := range []string{"[#4 foreground: edge → cloud, reason transfer]", "book the garage service for Friday", "[#5 progress of cloud]"} {
		if !strings.Contains(imported, want) {
			t.Fatalf("the cloud's import lacks %q:\n%s", want, imported)
		}
	}
	if acts := edge.activations(); len(acts) != 0 {
		t.Fatalf("the edge ran: %+v", acts)
	}
	log := env.log()
	if m := findEntries(log, KindMarker, MarkerRestarted); len(m) != 1 {
		t.Fatalf("restarted markers = %+v", m)
	}
	rep := findEntries(log, KindReply, "")
	if len(rep) != 1 || rep[0].Origin.Participant != "cloud" || rep[0].BasedOn != mk.Seq || !slices.Equal(rep[0].Steered, []uint64{6, 7}) {
		t.Fatalf("cloud replies = %+v", rep)
	}

	// The answered request is settled: a second restart recovers nothing.
	before, _ := store.Head(t.Context())
	edge2, cloud2 := edgeCloud()
	c2, err := NewCoordinator(t.Context(), Config{Store: store, Participants: []Participant{edge2, cloud2}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if after, _ := store.Head(t.Context()); after.LastSeq != before.LastSeq {
		t.Fatalf("second recovery appended %d entries", after.LastSeq-before.LastSeq)
	}
}

// TestCoordinatorRecoveryJoinsAnEarlierRequest: a request recovery routed
// again at an earlier restart was waiting while a newer one ran. Both are
// the turn in flight: the newer one, which the router sent to the cloud, is
// resumed with the earlier one joined to it and its progress, and neither is
// marked as not run again.
func TestCoordinatorRecoveryJoinsAnEarlierRequest(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("book the hotel"))
	requeued := markerEntry(MarkerRestarted, 1, "restarted")
	requeued.DedupKey = "requeue:1:1"
	mustAppend(t, store, nil, requeued,
		Entry{Kind: KindProgress, Origin: Origin{Participant: "cloud"}, BasedOn: 1, Run: "a-0",
			Content: textBlocks("1. [in_progress] compare hotels"), DedupKey: "progress:a-0:1"},
		userInput("and a car"))
	oc := ownerChange("", "cloud")
	oc.BasedOn = 4
	mustAppend(t, store, &Guard{}, oc)
	edge, cloud := edgeCloud()
	cloud.push(replyWith("booked both"))
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	log := env.log()
	if m := findEntries(log, KindMarker, MarkerRecovered); len(m) != 0 {
		t.Fatalf("recovered markers = %+v", m)
	}
	restarted := findEntries(log, KindMarker, MarkerRestarted)
	if len(restarted) != 2 || restarted[1].DedupKey != "restart:4" || !strings.Contains(restarted[1].Marker.Detail, "while #4, #1 was being answered") {
		t.Fatalf("restarted markers = %+v", restarted)
	}
	acts := cloud.activations()
	want := `[The session restarted while the user's messages #1 "book the hotel", #4 "and a car" were being answered.`
	if len(acts) != 1 || !strings.HasPrefix(acts[0].Input, want) || !strings.Contains(acts[0].Input, "The latest progress is #3") {
		t.Fatalf("cloud activations = %+v", acts)
	}
	replies := findEntries(log, KindReply, "")
	if len(replies) != 1 || !slices.Equal(replies[0].Steered, []uint64{1}) {
		t.Fatalf("replies = %+v", replies)
	}
}

// TestCoordinatorRecoveryJoinsAFailedRunsSteering: the edge's run failed
// after the user steered it, and the process stopped before the failover
// was written. The failed marker records the steering, so it goes with the
// request the edge resumes.
func TestCoordinatorRecoveryJoinsAFailedRunsSteering(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("plan my trip"))
	oc := ownerChange("", "edge")
	oc.BasedOn = 1
	mustAppend(t, store, &Guard{}, oc)
	mustAppend(t, store, nil, userInput("use metric units"))
	failed := markerEntry(MarkerFailed, 1, "edge failed: its activation ended with an error")
	failed.DedupKey, failed.Steered = "failed:a-1", []uint64{3}
	mustAppend(t, store, nil, failed)
	edge, cloud := edgeCloud()
	edge.push(replyWith("planned, in metric"))
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	acts := edge.activations()
	want := `[The session restarted while the user's messages #1 "plan my trip", #3 "use metric units" were being answered.`
	if len(acts) != 1 || !strings.HasPrefix(acts[0].Input, want) || !strings.Contains(acts[0].Input, "A failure before it is recorded in #4") {
		t.Fatalf("edge activations = %+v", acts)
	}
	replies := findEntries(env.log(), KindReply, "")
	if len(replies) != 1 || !slices.Equal(replies[0].Steered, []uint64{3}) {
		t.Fatalf("replies = %+v", replies)
	}
}

// TestCoordinatorRecoveryResumesTheNextRequest: the user sent a request while
// the previous reply was still streaming, or right after interrupting it.
// It waited, ran on the same owner once that run ended (the router wrote no
// owner change), and the process stopped before it wrote anything. It is
// resumed with the restart warning, not run again as a new request.
func TestCoordinatorRecoveryResumesTheNextRequest(t *testing.T) {
	for _, end := range []string{"reply", "interrupted"} {
		t.Run(end, func(t *testing.T) {
			store := NewMemoryStore(WithClock(fixedClock()))
			mustAppend(t, store, nil, userInput("first"))
			oc := ownerChange("", "cloud")
			oc.BasedOn = 1
			mustAppend(t, store, &Guard{}, oc)
			mustAppend(t, store, nil, userInput("send the email to Bob"))
			if end == "reply" {
				rep := replyFrom("cloud", "answer to first")
				rep.BasedOn, rep.DedupKey = 1, "reply:a-1"
				mustAppend(t, store, &Guard{Owner: "cloud", Epoch: 1}, rep)
			} else {
				mk := markerEntry(MarkerInterrupted, 1, "cloud was interrupted while answering #1")
				mk.DedupKey = "interrupted:a-1"
				mustAppend(t, store, nil, mk)
			}
			edge, cloud := edgeCloud()
			cloud.push(replyWith("sent once"))
			env := newCoEnv(t, store, nil, edge, cloud)
			env.idle()
			acts := cloud.activations()
			if len(acts) != 1 || !strings.HasPrefix(acts[0].Input, `[The session restarted while the user's request #3 "send the email to Bob"`) {
				t.Fatalf("cloud activations = %+v", acts)
			}
			if len(edge.activations()) != 0 {
				t.Fatal("the request ran on the edge too")
			}
			if m := findEntries(env.log(), KindMarker, MarkerRecovered); len(m) != 0 {
				t.Fatalf("recovered markers = %+v", m)
			}
		})
	}
}

// failedWith is a failed marker of run id based on seq that lists the
// inputs the run took.
func failedWith(seq uint64, id string, steered ...uint64) Entry {
	mk := markerEntry(MarkerFailed, seq, "failed: its activation ended with an error")
	mk.DedupKey, mk.Steered = "failed:"+id, steered
	return mk
}

// TestCoordinatorRecoveryMovesSteeringWithItsRequest: an input a failed run
// took follows that run's request after a crash: it joins the request when
// the owner the run failed over to resumes it, gets a recovered marker when
// the request is parked, and is queued with the request when the request is
// routed again. Steered lists that point back at each other or at entries
// that are not requests change nothing.
func TestCoordinatorRecoveryMovesSteeringWithItsRequest(t *testing.T) {
	t.Run("joined by the new owner", func(t *testing.T) {
		store := NewMemoryStore(WithClock(fixedClock()))
		mustAppend(t, store, nil, userInput("plan my trip"))
		toEdge := ownerChange("", "edge")
		toEdge.BasedOn = 1
		mustAppend(t, store, &Guard{}, toEdge)
		mustAppend(t, store, nil, userInput("and book the hotel"), failedWith(1, "a-1", 3, 2))
		mustAppend(t, store, &Guard{Owner: "edge", Epoch: 1}, Entry{Kind: KindOwnerChange, BasedOn: 1,
			Owner: &OwnerChange{From: "edge", To: "cloud", Reason: ReasonFailover}, DedupKey: "failover:a-1"})
		edge, cloud := edgeCloud()
		cloud.push(replyWith("planned"))
		edge.push(replyWith("noted"))
		env := newCoEnv(t, store, nil, edge, cloud)
		env.idle()
		restarted := findEntries(env.log(), KindMarker, MarkerRestarted)
		if len(restarted) != 1 || restarted[0].BasedOn != 5 || !strings.Contains(restarted[0].Marker.Detail, "#1, #3") {
			t.Fatalf("restarted markers = %+v", restarted)
		}
		if acts := cloud.activations(); len(acts) != 1 || !strings.Contains(acts[0].Input, `#3 "and book the hotel"`) {
			t.Fatalf("cloud activations = %+v", acts)
		}
		if acts := edge.activations(); len(acts) != 0 {
			t.Fatalf("edge activations = %+v", acts)
		}
	})
	t.Run("request parked", func(t *testing.T) {
		store := NewMemoryStore(WithClock(fixedClock()))
		mustAppend(t, store, nil, userInput("crashy"))
		oc := ownerChange("", "cloud")
		oc.BasedOn = 1
		mustAppend(t, store, &Guard{}, oc)
		first := markerEntry(MarkerRestarted, 1, "restarted")
		first.DedupKey = "restart:1"
		mustAppend(t, store, nil, first)
		second := markerEntry(MarkerRestarted, 3, "restarted")
		second.DedupKey = "restart:3"
		mustAppend(t, store, nil, second, userInput("faster please"), failedWith(4, "a-3", 5))
		edge, cloud := edgeCloud()
		env := newCoEnv(t, store, nil, edge, cloud)
		env.idle()
		log := env.log()
		if got := kindsOf(log[6:]); !reflect.DeepEqual(got, []string{"marker:recovered", "marker:recovered"}) ||
			log[6].BasedOn != 4 || log[7].BasedOn != 5 {
			t.Fatalf("recovery wrote %v: %+v", got, log[6:])
		}
		if len(cloud.activations())+len(edge.activations()) != 0 {
			t.Fatal("a parked request or its steering ran")
		}
	})
	t.Run("request routed again", func(t *testing.T) {
		store := NewMemoryStore(WithClock(fixedClock()))
		mustAppend(t, store, nil, userInput("plan my trip"), userInput("in May"), failedWith(1, "a-1", 2))
		edge, cloud := edgeCloud()
		edge.push(replyWith("planned for May"))
		cloud.push(replyWith("planned for May"))
		env := newCoEnv(t, store, nil, edge, cloud)
		env.idle()
		log := env.log()
		if log[3].Kind != KindMarker || log[3].DedupKey != "requeue:1:1" {
			t.Fatalf("recovery wrote %+v", log[3])
		}
		replies := findEntries(log, KindReply, "")
		if len(replies) != 1 || replies[0].BasedOn != 2 || !slices.Equal(replies[0].Steered, []uint64{1}) {
			t.Fatalf("replies = %+v", replies)
		}
	})
	t.Run("steered lists that loop", func(t *testing.T) {
		store := NewMemoryStore(WithClock(fixedClock()))
		mustAppend(t, store, nil, userInput("a"), userInput("b"), failedWith(1, "a-1", 2, 99), failedWith(2, "a-2", 1))
		edge, cloud := edgeCloud()
		edge.push(replyWith("done"))
		cloud.push(replyWith("done"))
		env := newCoEnv(t, store, nil, edge, cloud)
		env.idle()
		if m := findEntries(env.log(), KindMarker, MarkerRestarted); len(m) != 1 || m[0].BasedOn != 1 {
			t.Fatalf("restarted markers = %+v", m)
		}
	})
}

// TestCoordinatorRecoveryEndsSteeringWithItsRun: the user interrupted a run
// they had steered; the interrupted marker records the steering, so neither
// message is taken up again after a restart.
func TestCoordinatorRecoveryEndsSteeringWithItsRun(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("plan my trip"))
	oc := ownerChange("", "cloud")
	oc.BasedOn = 1
	mustAppend(t, store, &Guard{}, oc)
	mustAppend(t, store, nil, userInput("actually, stop"))
	stop := markerEntry(MarkerInterrupted, 1, "cloud was interrupted while answering #1")
	stop.DedupKey, stop.Steered = "interrupted:a-1", []uint64{3}
	mustAppend(t, store, nil, stop)
	edge, cloud := edgeCloud()
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	if log := env.log(); len(log) != 4 {
		t.Fatalf("recovery wrote %v", kindsOf(log[4:]))
	}
	if len(cloud.activations())+len(edge.activations()) != 0 {
		t.Fatal("an interrupted run's messages ran again")
	}
}

// TestCoordinatorRecoverySkipsSteeredInputs: an input a reply lists as
// steered into its run is answered; the newest unanswered one is resumed.
func TestCoordinatorRecoverySkipsSteeredInputs(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("old"))
	oc := ownerChange("", "cloud")
	oc.BasedOn = 1
	mustAppend(t, store, &Guard{}, oc)
	mustAppend(t, store, nil, userInput("asked"), userInput("steered"))
	rep := replyFrom("cloud", "answered both")
	rep.BasedOn = 3
	rep.Steered = []uint64{4}
	mustAppend(t, store, &Guard{Owner: "cloud", Epoch: 1}, rep)
	mustAppend(t, store, nil, userInput("newest"))
	edge, cloud := edgeCloud()
	cloud.push(replyWith("done"))
	env := newCoEnv(t, store, nil, edge, cloud)
	env.idle()
	log := env.log()
	var recovered []uint64
	for _, e := range log[6:] {
		if e.Kind == KindMarker {
			recovered = append(recovered, e.BasedOn)
		}
	}
	if !reflect.DeepEqual(recovered, []uint64{1, 6}) {
		t.Fatalf("markers based on %v, want the old input's and the newest's", recovered)
	}
	if acts := cloud.activations(); len(acts) != 1 || !strings.Contains(acts[0].Input, `#6 "newest"`) {
		t.Fatalf("cloud activations = %+v", acts)
	}
}

func TestCoordinatorHostCalls(t *testing.T) {
	edge, cloud := edgeCloud()
	type result struct {
		name string
		err  error
	}
	var results []result
	check := func(name string, err error) { results = append(results, result{name, err}) }
	edge.push(func(r *fakeRun) *ActivationEnd {
		h := r.host()
		id := h.RecordEvent(EventRecord{Origin: Origin{Participant: "cloud"}, Zone: ZoneCloud, Tool: "camera", Output: "jpeg bytes"})
		h.RecordUsage(LedgerEntry{Participant: "cloud", Kind: UsageMain, Usage: &model.ChatUsage{InputTokens: 3}})
		if d := h.Descriptors(); len(d) != 2 || d[0].Name != "edge" || d[1].Name != "cloud" {
			t.Errorf("descriptors = %+v", d)
		}
		if o, e := h.Owner(); o != "edge" || e != 1 {
			t.Errorf("owner = %s %d", o, e)
		}
		_, err := h.Read(r.ctx, "cloud", ReadQuery{})
		check("read as another", err)
		_, _, err = h.EventPage("cloud", id, 0, 0)
		check("page as another", err)
		if h.EventOutline("cloud", 0) != nil || h.EventSearch("cloud", "", 0) != nil {
			t.Error("event lists answered another reader")
		}
		_, err = h.Pending(r.ctx, "cloud", 0)
		check("pending as another", err)
		_, err = h.Publish(r.ctx, PublishRequest{From: Origin{Participant: "edge"}})
		check("publish without refs", err)
		_, err = h.Publish(r.ctx, PublishRequest{From: Origin{Participant: "cloud"}, Refs: []ArtifactRef{{URI: "x"}}})
		check("publish as another", err)
		_, err = h.Report(r.ctx, ReportRequest{From: Origin{Participant: "edge"}, To: []string{"cloud"}})
		check("empty report", err)
		_, err = h.Report(r.ctx, ReportRequest{From: Origin{Participant: "edge"}, Text: "x", To: []string{"nobody"}})
		check("report to unknown", err)
		_, err = h.Report(r.ctx, ReportRequest{From: Origin{Participant: "edge"}, Text: "x", To: []string{"edge"}})
		check("report to self", err)
		_, err = h.Report(r.ctx, ReportRequest{From: Origin{Participant: "cloud"}, Text: "x"})
		check("report as another", err)
		if _, err := h.Report(r.ctx, ReportRequest{From: Origin{Participant: "edge"}, Text: "fyi", To: []string{"cloud", "cloud"}, TaskID: "t-9"}); err != nil {
			t.Errorf("report: %v", err)
		}
		if _, err := h.Publish(r.ctx, PublishRequest{From: Origin{Participant: "edge"}, Refs: []ArtifactRef{{URI: "file:///plan.md"}},
			Text: "the plan"}); err != nil {
			t.Errorf("publish: %v", err)
		}
		got, err := h.Read(r.ctx, "edge", ReadQuery{Seqs: []uint64{1, 99}, Kinds: []Kind{KindUserInput}, Limit: 1})
		if err != nil || len(got) != 1 || got[0].Seq != 1 {
			t.Errorf("read seqs = %+v %v", got, err)
		}
		// A missing seq and one of another kind are skipped.
		got, err = h.Read(r.ctx, "edge", ReadQuery{Seqs: []uint64{99, 2, 1}, Kinds: []Kind{KindUserInput}})
		if err != nil || len(got) != 1 || got[0].Seq != 1 {
			t.Errorf("read skipping seqs = %+v %v", got, err)
		}
		got, err = h.Read(r.ctx, "edge", ReadQuery{Kinds: []Kind{KindOwnerChange}, Limit: 1})
		if err != nil || len(got) != 1 || got[0].Seq != 2 {
			t.Errorf("read by kind = %+v %v", got, err)
		}
		_, err = h.Read(r.ctx, "cloud", ReadQuery{})
		check("read as another", err)
		return r.reply("done")
	})
	cloud.push(func(r *fakeRun) *ActivationEnd {
		h := r.host()
		if got := h.EventOutline("cloud", 0); len(got) != 1 || got[0].Tool != "camera" || got[0].Origin.Participant != "edge" {
			t.Errorf("cloud outline = %+v", got)
		}
		if got := h.EventSearch("cloud", "jpeg", 0); len(got) != 1 {
			t.Errorf("cloud search = %+v", got)
		}
		if text, _, err := h.EventPage("cloud", "ev-1", 0, 0); err != nil || text != "jpeg bytes" {
			t.Errorf("cloud page = %q %v", text, err)
		}
		got, err := r.read(ReadQuery{Seqs: []uint64{4, 1}})
		if err != nil || len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 1 {
			t.Errorf("cloud read = %+v %v", got, err)
		}
		return r.reply("seen")
	})
	env := newCoEnv(t, nil, nil, edge, cloud)
	env.say("hello") // #1, owner_change #2; the edge reports #3 and publishes #4
	env.idle()
	env.submit(Query{Text: "shared", Explicit: "cloud"})
	env.idle()

	for _, r := range results {
		if r.err == nil {
			t.Errorf("%s: no error", r.name)
		}
	}
	if len(results) != 10 {
		t.Fatalf("%d checks ran", len(results))
	}
	if tot := env.c.Ledger().Totals()["edge"]; tot.Calls != 1 || tot.InputTokens != 3 {
		t.Fatalf("usage recorded as %+v", env.c.Ledger().Totals())
	}
	log := env.log()
	arts := findEntries(log, KindArtifact, "")
	reps := findEntries(log, KindReport, "")
	if len(arts) != 1 || arts[0].Seq != 4 || len(reps) != 1 || !reflect.DeepEqual(reps[0].To, []string{"cloud"}) ||
		reps[0].Task == nil || reps[0].Task.ID != "t-9" {
		t.Fatalf("artifact %+v report %+v", arts, reps)
	}
}

// A run that ends normally with no text and no tool digest still writes its
// reply, so a restart sees the request answered and does not run it again.
func TestRecoveryEmptyCompletedReplyIsNotResumed(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	edge, cloud := edgeCloud()
	edge.push(func(r *fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyCompleted}
	})
	env := newCoEnv(t, store, nil, edge, cloud)
	env.say("hello")
	env.idle()
	first := env.log()
	if got := kindsOf(first); len(got) != 3 || got[2] != "reply" {
		t.Fatalf("log after the run = %v, want the empty reply last", got)
	}
	if rep := first[2]; rep.Text() != "" || rep.BasedOn != first[0].Seq || rep.Origin.Participant != "edge" {
		t.Fatalf("reply = %+v", rep)
	}
	_ = env.c.Close()

	edge2, cloud2 := edgeCloud()
	edge2.setState(func(s *ParticipantState) { s.Cursor = 2 })
	c2, err := NewCoordinator(context.Background(), Config{Store: store, Participants: []Participant{edge2, cloud2}, Clock: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	go func() {
		for range c2.Events() {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c2.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	all, err := store.Read(context.Background(), 0, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(first) {
		t.Fatalf("restart wrote %v", kindsOf(all[len(first):]))
	}
	if n := len(edge2.activations()) + len(cloud2.activations()); n != 0 {
		t.Fatalf("an answered request was run again: %d activations", n)
	}
}

// After a restart the owner's first activation fails at Activate, so its
// Sync was never applied. Its next import must still bring back the own
// entries it wrote past its checkpoint, here its live progress.
func TestRecoveryOwnEntriesSurviveAFailedFirstActivation(t *testing.T) {
	store := NewMemoryStore(WithClock(fixedClock()))
	mustAppend(t, store, nil, userInput("q1"))
	oc := ownerChange("", "cloud")
	oc.BasedOn = 1
	mustAppend(t, store, &Guard{}, oc)
	mustAppend(t, store, nil, Entry{Kind: KindProgress, Origin: Origin{Participant: "cloud"}, BasedOn: 1, Run: "a-1",
		Content: textBlocks("1. [in_progress] book the hotel"), DedupKey: "progress:a-1:1"})

	edge, cloud := edgeCloud()
	cloud.activateErr = errors.New("checkpoint save failed")
	env := newCoEnv(t, store, func(c *Config) { c.Router = cloudRouter() }, edge, cloud)
	env.idle()

	cloud.mu.Lock()
	cloud.activateErr = nil
	cloud.mu.Unlock()
	env.say("q2")
	env.idle()
	acts := cloud.activations()
	if len(acts) == 0 {
		t.Fatalf("cloud never ran; log %v", kindsOf(env.log()))
	}
	imported := strings.Join(syncTexts(acts[len(acts)-1].Sync), "\n")
	if !strings.Contains(imported, "progress of cloud") {
		t.Fatalf("the restored owner's own progress was not re-imported:\n%s", imported)
	}
}
