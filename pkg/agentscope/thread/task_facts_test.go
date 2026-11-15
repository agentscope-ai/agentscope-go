package thread

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// crashStore loses every write once crashed, as the disk of a process that
// stopped would.
type crashStore struct {
	Store
	crashed atomic.Bool
}

func (s *crashStore) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	if s.crashed.Load() {
		return 0, errors.New("the process stopped")
	}
	return s.Store.Append(ctx, entries, g)
}

// taskLedger follows tasks the way docs/main-thread.md tells a host to: it
// reads the log when it starts, then follows the event stream, skipping
// entries the read already covered.
type taskLedger struct {
	t       *testing.T
	seen    map[uint64]bool
	assigns map[uint64]string // assign seq -> task ID
	open    map[string]bool
	ends    map[string]types.ReplyFinishedReason
	unknown map[string]bool // an unknown_effects marker followed the task's report
}

func newTaskLedger(t *testing.T) *taskLedger {
	return &taskLedger{t: t, seen: map[uint64]bool{}, assigns: map[uint64]string{}, open: map[string]bool{},
		ends: map[string]types.ReplyFinishedReason{}, unknown: map[string]bool{}}
}

func (l *taskLedger) apply(e *Entry) {
	l.t.Helper()
	if l.seen[e.Seq] {
		return
	}
	l.seen[e.Seq] = true
	switch {
	case e.Kind == KindAssign:
		l.assigns[e.Seq] = e.Task.ID
		l.open[e.Task.ID] = true
	case e.Kind == KindReport && e.Task != nil && e.Task.End != "":
		if !l.open[e.Task.ID] {
			l.t.Fatalf("report #%d ends task %s, which is not open (ends so far %v)", e.Seq, e.Task.ID, l.ends)
		}
		delete(l.open, e.Task.ID)
		l.ends[e.Task.ID] = e.Task.End
	case e.Kind == KindMarker && e.Marker.Kind == MarkerUnknownEffects:
		if id, ok := l.assigns[e.BasedOn]; ok {
			l.unknown[id] = true
		}
	}
}

// start reads the whole log, as a host does when it starts.
func (l *taskLedger) start(env *coEnv) {
	l.t.Helper()
	log := env.log()
	for i := range log {
		l.apply(&log[i])
	}
}

// follow applies the entry events delivered so far and returns their entries.
func (l *taskLedger) follow(evs []Event) []*Entry {
	l.t.Helper()
	var entries []*Entry
	for _, ev := range evs {
		if ev.Kind == EventEntry {
			entries = append(entries, ev.Entry)
			l.apply(ev.Entry)
		}
	}
	return entries
}

// closedEvents closes the Coordinator and returns every event it delivered.
func closedEvents(t *testing.T, env *coEnv) []Event {
	t.Helper()
	if err := env.c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-env.ev.done
	env.ev.mu.Lock()
	defer env.ev.mu.Unlock()
	return slices.Clone(env.ev.list)
}

func waitForEntry(t *testing.T, env *coEnv, what string, match func(*Entry) bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		log := env.log()
		for i := range log {
			if match(&log[i]) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s in the log", what)
}

func blockUntilCanceled(toolCalls int) fakeScript {
	return func(r *fakeRun) *ActivationEnd {
		<-r.ctx.Done()
		return &ActivationEnd{Reason: types.ReplyInterrupted, ToolCalls: toolCalls}
	}
}

func isReplyText(text string) func(*Entry) bool {
	return func(e *Entry) bool { return e.Kind == KindReply && e.Text() == text }
}

// A host ledger that reads the log when it starts and then follows the event
// stream sees every task end exactly once, across a graceful Close, a crash
// and the restarts after them. An entry committed while Close runs may never
// reach the stream; it is in the log, where the next start reads it. Recovery
// marks a task that had not ended TaskUnfinished, in the log and on the new
// stream, and never reports a task that already ended.
func TestCoordinatorTaskLedgerAcrossCloseCrashAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main-thread.jsonl")
	open := func() *crashStore {
		fs, err := OpenFileStore(path, WithClock(fixedClock()))
		if err != nil {
			t.Fatal(err)
		}
		return &crashStore{Store: fs}
	}
	router := func(cfg *Config) { cfg.Router = cloudRouter() }
	ledger := newTaskLedger(t)

	// Session 1: t-1 completes; t-2 is running and t-3 is queued behind it
	// when the host closes the Coordinator.
	store := open()
	edge, cloud := edgeCloud()
	edge.started = make(chan Activation, 8)
	cloud.push(func(r *fakeRun) *ActivationEnd {
		for _, text := range []string{"count the photos", "sort the album", "tag the faces"} {
			if _, _, err := r.assign("edge", text, false); err != nil {
				t.Errorf("assign: %v", err)
			}
		}
		return r.reply("asked the edge")
	})
	edge.push(replyWith("42 photos"), blockUntilCanceled(1))
	env := newCoEnv(t, store, router, edge, cloud)
	ledger.start(env)
	env.say("count my photos, sort the album and tag the faces")
	waitStarted(t, edge)
	if a := waitStarted(t, edge); a.Trigger.Task == nil || a.Trigger.Task.ID != "t-2" {
		t.Fatalf("second edge activation = %+v", a.Trigger)
	}
	waitForEntry(t, env, "cloud reply", isReplyText("asked the edge"))
	ledger.follow(closedEvents(t, env))
	var interrupted *Entry
	log := env.log()
	for i := range log {
		if e := &log[i]; e.Kind == KindReport && e.Task != nil && e.Task.ID == "t-2" {
			interrupted = e
		}
	}
	if interrupted == nil || interrupted.Task.End != types.ReplyInterrupted {
		t.Fatalf("t-2's report in the log = %+v", interrupted)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Session 2: the restart reports t-3, which never ran, as unfinished and
	// leaves t-2 alone. Then t-4 is running when the process crashes.
	store = open()
	edge, cloud = edgeCloud()
	edge.started = make(chan Activation, 8)
	cloud.push(func(r *fakeRun) *ActivationEnd {
		if _, _, err := r.assign("edge", "check the album again", false); err != nil {
			t.Errorf("assign: %v", err)
		}
		return r.reply("asked the edge again")
	})
	edge.push(blockUntilCanceled(2))
	env = newCoEnv(t, store, router, edge, cloud)
	ledger.start(env)
	var unfinished []string
	for _, e := range ledger.follow(env.events()) {
		if e.Kind == KindReport && e.Task != nil && e.Task.End == TaskUnfinished {
			unfinished = append(unfinished, e.Task.ID)
		}
	}
	if !slices.Equal(unfinished, []string{"t-3"}) {
		t.Fatalf("unfinished reports on the restarted stream = %v, want [t-3]", unfinished)
	}
	env.say("check the album again")
	if a := waitStarted(t, edge); a.Trigger.Task == nil || a.Trigger.Task.ID != "t-4" {
		t.Fatalf("edge activation = %+v", a.Trigger)
	}
	waitForEntry(t, env, "cloud reply", isReplyText("asked the edge again"))
	ledger.follow(env.events())
	store.crashed.Store(true)
	ledger.follow(closedEvents(t, env)) // t-4's report never reaches the disk
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Session 3: the restart after the crash reports t-4 as unfinished and
	// runs nothing.
	store = open()
	edge, cloud = edgeCloud()
	env = newCoEnv(t, store, router, edge, cloud)
	ledger.start(env)
	env.idle()
	ledger.follow(closedEvents(t, env))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if n, m := len(edge.activations()), len(cloud.activations()); n != 0 || m != 0 {
		t.Fatalf("the restart ran %d edge and %d cloud activations, want none", n, m)
	}

	want := map[string]types.ReplyFinishedReason{
		"t-1": types.ReplyCompleted,
		"t-2": types.ReplyInterrupted,
		"t-3": TaskUnfinished,
		"t-4": TaskUnfinished,
	}
	if len(ledger.open) != 0 || len(ledger.ends) != len(want) {
		t.Fatalf("open %v, ends %v, want %v", ledger.open, ledger.ends, want)
	}
	for id, end := range want {
		if ledger.ends[id] != end {
			t.Fatalf("task %s ended %q, want %q (ends %v)", id, ledger.ends[id], end, ledger.ends)
		}
	}
	if !ledger.unknown["t-2"] || len(ledger.unknown) != 1 {
		t.Fatalf("unknown_effects markers for %v, want t-2 only", ledger.unknown)
	}
}

// A host can follow tasks from the event stream alone: each assign entry
// reaches it, and the report that ends a task says how it ended. A progress
// report about a task leaves End empty.
func TestCoordinatorTaskFactsReachTheHost(t *testing.T) {
	edge, cloud := edgeCloud()
	cloud.push(func(r *fakeRun) *ActivationEnd {
		for _, text := range []string{"count the photos", "sort the album"} {
			if _, _, err := r.assign("edge", text, false); err != nil {
				t.Errorf("assign: %v", err)
			}
		}
		return r.reply("asked the edge")
	})
	edge.push(func(r *fakeRun) *ActivationEnd {
		if _, err := r.host().Report(r.ctx, ReportRequest{
			From:   Origin{Participant: r.name()},
			Text:   "halfway through",
			To:     []string{"cloud"},
			TaskID: r.act.Trigger.Task.ID,
		}); err != nil {
			t.Errorf("progress report: %v", err)
		}
		return r.reply("42 photos")
	}, func(*fakeRun) *ActivationEnd {
		return &ActivationEnd{Reason: types.ReplyError, Err: errors.New("disk gone")}
	})
	env := newCoEnv(t, nil, func(cfg *Config) { cfg.Router = cloudRouter() }, edge, cloud)
	env.say("count my photos, then sort the album")
	env.idle()

	assigns := map[string]bool{}
	ends := map[string]types.ReplyFinishedReason{}
	progress := 0
	for _, ev := range env.events() {
		if ev.Kind != EventEntry {
			continue
		}
		e := ev.Entry
		switch e.Kind {
		case KindAssign:
			if ev.Foreground || ev.Participant != "cloud" || e.Task == nil {
				t.Fatalf("assign event = %+v", ev)
			}
			assigns[e.Task.ID] = true
		case KindReport:
			if e.Task == nil {
				continue
			}
			if e.Task.End == "" {
				progress++
				continue
			}
			if _, dup := ends[e.Task.ID]; dup {
				t.Fatalf("task %s ended twice", e.Task.ID)
			}
			ends[e.Task.ID] = e.Task.End
		}
	}
	if len(assigns) != 2 || !assigns["t-1"] || !assigns["t-2"] {
		t.Fatalf("assign events for %v, want t-1 and t-2", assigns)
	}
	if progress != 1 {
		t.Fatalf("progress reports = %d, want 1", progress)
	}
	want := map[string]types.ReplyFinishedReason{"t-1": types.ReplyCompleted, "t-2": types.ReplyError}
	if len(ends) != len(want) || ends["t-1"] != want["t-1"] || ends["t-2"] != want["t-2"] {
		t.Fatalf("task ends = %v, want %v", ends, want)
	}
	checkNotStranded(t, env, map[string]*fakePart{"edge": edge, "cloud": cloud})
}
