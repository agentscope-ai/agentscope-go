package taskenvelope

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock returns an Options with a controllable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore(t *testing.T, clk *testClock) *Store {
	t.Helper()
	opts := &Options{}
	if clk != nil {
		opts.Now = clk.Now
	}
	st, err := Open(t.TempDir()+"/ledger.json", opts)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func submitOk(t *testing.T, st *Store, sub *Submission) *Record {
	t.Helper()
	rec, err := st.Submit(sub)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateAccepted {
		t.Fatalf("state = %s, want Accepted (result %q)", rec.State, rec.Result)
	}
	return rec
}

func baseSubmission(id string) *Submission {
	return &Submission{
		ID:             id,
		RootTaskID:     "root",
		Scope:          "fs:read",
		Content:        "do something",
		AllowanceUnits: 3,
		MaxAttempts:    1,
	}
}

func setupRoot(t *testing.T, st *Store, allowance int) {
	t.Helper()
	if err := st.RegisterRoot("root", allowance); err != nil {
		t.Fatal(err)
	}
}

// Table "Forbidden transitions": no exit from terminal states, no direct
// Proposed→Completed, no exit from UnknownEffect without the table.
func TestTransitionTable_Forbidden(t *testing.T) {
	legal := [][2]State{
		{StateProposed, StateAccepted}, {StateProposed, StateFailed},
		{StateProposed, StateCanceled}, {StateProposed, StateExpired},
		{StateAccepted, StateInProgress}, {StateAccepted, StateCanceling}, {StateAccepted, StateExpired},
		{StateInProgress, StateCompleted}, {StateInProgress, StateFailed},
		{StateInProgress, StateCanceling}, {StateInProgress, StateUnknownEffect},
		{StateCanceling, StateCanceled}, {StateCanceling, StateUnknownEffect}, {StateCanceling, StateCompleted},
		{StateUnknownEffect, StateCompleted}, {StateUnknownEffect, StateFailed}, {StateUnknownEffect, StateCanceled},
	}
	for _, tr := range legal {
		if _, err := checkTransition(tr[0], tr[1]); err != nil {
			t.Errorf("legal transition %s → %s rejected: %v", tr[0], tr[1], err)
		}
	}

	illegal := [][2]State{
		{StateCompleted, StateInProgress}, {StateFailed, StateProposed},
		{StateCanceled, StateAccepted}, {StateExpired, StateAccepted},
		{StateProposed, StateCompleted},                                            // no direct Proposed→Completed
		{StateUnknownEffect, StateProposed}, {StateUnknownEffect, StateInProgress}, // no blind re-dispatch
		{StateAccepted, StateCompleted},
	}
	for _, tr := range illegal {
		if _, err := checkTransition(tr[0], tr[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("forbidden transition %s → %s accepted", tr[0], tr[1])
		}
	}
}

// Table T1/T2 + mapping 1: every admission guard failure is a recorded
// rejection (Failed, usage known-zero) that an identical retry replays.
func TestAdmissionGuards_T1_T2(t *testing.T) {
	clk := &testClock{now: time.Now()}
	st := newTestStore(t, clk)
	setupRoot(t, st, 10)

	// Baseline admission passes.
	submitOk(t, st, baseSubmission("ok"))

	cases := []struct {
		name string
		sub  *Submission
	}{
		{"unknown_root", &Submission{ID: "noroot", RootTaskID: "ghost", Scope: "fs:read", Content: "x", AllowanceUnits: 1, MaxAttempts: 1}},
		{"insufficient_allowance", &Submission{ID: "poor", RootTaskID: "root", Scope: "fs:read", Content: "x", AllowanceUnits: 11, MaxAttempts: 1}},
		{"stale_epoch", &Submission{ID: "stale", RootTaskID: "root", Scope: "fs:read", Content: "x", Epoch: 0, AllowanceUnits: 1, MaxAttempts: 1}}, // advanced below
	}
	if _, err := st.AdvanceEpoch("root"); err != nil { // current epoch is now 1 (T6 sweep cancels "ok")
		t.Fatal(err)
	}

	for _, tc := range cases {
		rec, err := st.Submit(tc.sub)
		if err != nil {
			t.Fatalf("%s: Submit: %v", tc.name, err)
		}
		if rec.State != StateFailed {
			t.Errorf("%s: state = %s, want Failed", tc.name, rec.State)
		}
		if !strings.HasPrefix(rec.Result, "rejected: ") {
			t.Errorf("%s: rejection not recorded, result = %q", tc.name, rec.Result)
		}
		if rec.Allowance != (Allowance{}) {
			t.Errorf("%s: rejected record must have zero allowance ledger, got %+v", tc.name, rec.Allowance)
		}
		// Identical retry replays the recorded rejection without re-admission.
		replay, err := st.Submit(tc.sub)
		if err != nil {
			t.Fatalf("%s: retry: %v", tc.name, err)
		}
		if replay.State != StateFailed || replay.Result != rec.Result {
			t.Errorf("%s: retry got %+v, want recorded rejection", tc.name, replay)
		}
	}

	// The baseline record had epoch 0: the T6 sweep moved it to Canceling and,
	// with nothing dispatched, straight to Canceled with known-zero usage.
	rec, _ := st.Get("ok")
	if rec.State != StateCanceled {
		t.Errorf("baseline record = %s, want Canceled after epoch advance (T6/T7)", rec.State)
	}
	if rec.Allowance.ReleasedUnused != 3 {
		t.Errorf("baseline released = %d, want 3", rec.Allowance.ReleasedUnused)
	}
}

// T1 authority guard.
func TestAdmission_Authority(t *testing.T) {
	clk := &testClock{now: time.Now()}
	st, err := Open(t.TempDir()+"/ledger.json", &Options{
		Now:       clk.Now,
		Authority: func(scope string) bool { return scope == "fs:read" },
	})
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)

	rec, err := st.Submit(&Submission{
		ID: "denied", RootTaskID: "root", Scope: "shell:exec", Content: "x",
		AllowanceUnits: 1, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateFailed {
		t.Errorf("state = %s, want Failed (authority)", rec.State)
	}
}

// Mapping 2: identical retry replays the recorded status exactly once
// (allowance reserved once), conflicting ID is rejected, and a terminal
// record never reopens.
func TestDedupe(t *testing.T) {
	clk := &testClock{now: time.Now()}
	st := newTestStore(t, clk)
	setupRoot(t, st, 10)

	submitOk(t, st, baseSubmission("dup"))
	replay, err := st.Submit(baseSubmission("dup"))
	if err != nil {
		t.Fatal(err)
	}
	if replay.State != StateAccepted || replay.Allowance.Reserved != 3 {
		t.Errorf("replay = %+v", replay)
	}
	root, _ := st.Root("root")
	if root.Available != 7 {
		t.Errorf("allowance reserved twice: available = %d, want 7", root.Available)
	}

	// Conflicting ID reuse.
	bad := baseSubmission("dup")
	bad.Content = "different content"
	if _, err := st.Submit(bad); !errors.Is(err, ErrIDConflict) {
		t.Errorf("conflict: got %v, want ErrIDConflict", err)
	}

	// Expired reopen: expire the record, resubmit identically — the recorded
	// terminal status replays; nothing reopens.
	rec, _ := st.Get("dup")
	if rec.State != StateAccepted {
		t.Fatalf("state = %s, want Accepted before expiry", rec.State)
	}
	st2 := newTestStore(t, clk)
	setupRoot(t, st2, 10)
	expSub := baseSubmission("exp")
	expSub.TTL = time.Minute
	submitOk(t, st2, expSub)
	clk.Advance(2 * time.Minute)
	if err := st2.Expire(); err != nil {
		t.Fatal(err)
	}
	rec, _ = st2.Get("exp")
	if rec.State != StateExpired {
		t.Fatalf("state = %s, want Expired", rec.State)
	}
	reopened, err := st2.Submit(expSub)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != StateExpired {
		t.Errorf("terminal record reopened: state = %s", reopened.State)
	}
}

// Accounting-model anchor (mapping 1): reserve 3, consume 2 → at most 1
// reusable unit, stable across restart and repeated reconciliation.
func TestAllowanceAnchor(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	two := 2
	exec := NewSimExecutor()
	exec.Outcome = Outcome{Success: true, Result: "done", Usage: &two, Effect: EffectNone}
	if err := st.Dispatch(context.Background(), "work", "do something", exec); err != nil {
		t.Fatal(err)
	}

	assertLedger := func(st *Store, where string) {
		t.Helper()
		rec, _ := st.Get("work")
		if rec.State != StateCompleted {
			t.Errorf("%s: state = %s", where, rec.State)
		}
		if got := rec.Allowance; got.Consumed != 2 || got.ReleasedUnused != 1 || got.Locked != 0 || got.Reserved != 3 {
			t.Errorf("%s: allowance = %+v, want reserved 3, consumed 2, released 1, locked 0", where, got)
		}
		root, _ := st.Root("root")
		if root.Available != 8 || root.Consumed != 2 {
			t.Errorf("%s: root = %+v, want available 8, consumed 2", where, root)
		}
	}
	assertLedger(st, "after settlement")

	// Restart against the same file: numbers unchanged.
	st2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertLedger(st2, "after restart")

	// Repeated reconciliation is a no-op.
	if err := st2.Reconcile(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	assertLedger(st2, "after repeated reconcile")
	if err := st2.Reconcile(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	assertLedger(st2, "after second reconcile")
}

// Table T9/T10 + key invariant: Expired only when nothing was dispatched;
// TTL elapsed with an attempt in flight routes to UnknownEffect with the
// remainder locked.
func TestT9_T10_Boundary(t *testing.T) {
	clk := &testClock{now: time.Now()}
	st := newTestStore(t, clk)
	setupRoot(t, st, 10)

	sub := baseSubmission("idle")
	sub.TTL = time.Minute
	submitOk(t, st, sub) // Accepted, never dispatched

	gated := baseSubmission("flight")
	gated.TTL = time.Minute
	submitOk(t, st, gated)

	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "flight", "do something", exec) }()
	for {
		rec, _ := st.Get("flight")
		if rec.State == StateInProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}

	clk.Advance(2 * time.Minute)
	if err := st.Expire(); err != nil {
		t.Fatal(err)
	}

	rec, _ := st.Get("idle")
	if rec.State != StateExpired || rec.Effect != EffectNone {
		t.Errorf("idle: state=%s effect=%s, want Expired/none (T9)", rec.State, rec.Effect)
	}
	if rec.Allowance.ReleasedUnused != 3 {
		t.Errorf("idle: released = %d, want 3 (known-zero)", rec.Allowance.ReleasedUnused)
	}

	rec, _ = st.Get("flight")
	if rec.State != StateUnknownEffect {
		t.Errorf("flight: state = %s, want UnknownEffect (T10)", rec.State)
	}
	if rec.Allowance.Locked != 3 {
		t.Errorf("flight: locked = %d, want 3", rec.Allowance.Locked)
	}

	close(exec.Gate) // unblock the executor; the store already declared uncertainty
	<-done
}

// Accounting model: a Completed record may still have unknown usage — the
// remainder locks until validated usage evidence arrives.
func TestUsageUnknown_LockedThenEvidence(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor() // Outcome.Usage nil = unknown usage
	if err := st.Dispatch(context.Background(), "work", "do something", exec); err != nil {
		t.Fatal(err)
	}

	rec, _ := st.Get("work")
	if rec.State != StateCompleted {
		t.Fatalf("state = %s", rec.State)
	}
	if rec.Allowance.Locked != 3 || rec.Allowance.Consumed != 0 {
		t.Fatalf("allowance = %+v, want locked 3, consumed 0", rec.Allowance)
	}

	// Over-claim is an integrity error.
	if err := st.RecordUsageEvidence("work", 4, "executor audit"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("over-claim: got %v, want ErrIntegrity", err)
	}

	if err := st.RecordUsageEvidence("work", 2, "executor audit"); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get("work")
	if got := rec.Allowance; got.Consumed != 2 || got.Locked != 0 || got.ReleasedUnused != 1 {
		t.Errorf("allowance = %+v, want consumed 2, locked 0, released 1", got)
	}
	root, _ := st.Root("root")
	if root.Available != 8 || root.Consumed != 2 {
		t.Errorf("root = %+v", root)
	}
	// Idempotent: nothing left locked.
	if err := st.RecordUsageEvidence("work", 2, "executor audit"); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get("work")
	if rec.Allowance.Consumed != 2 {
		t.Errorf("double-settled: consumed = %d", rec.Allowance.Consumed)
	}
}

// Table T12: UnknownEffect resolves only with validated evidence; no blind
// re-dispatch from any state.
func TestT12_RequiresEvidence(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.LoseResult = true // executed, but the result was lost
	if err := st.Dispatch(context.Background(), "work", "do something", exec); err == nil {
		t.Fatal("dispatch with lost result must report an error")
	}
	rec, _ := st.Get("work")
	if rec.State != StateUnknownEffect || rec.Allowance.Locked != 3 {
		t.Fatalf("state = %s, allowance = %+v (want UnknownEffect, locked 3)", rec.State, rec.Allowance)
	}

	// Resolution without evidence is prohibited.
	if err := st.Resolve("work", &Evidence{Outcome: StateCompleted}); !errors.Is(err, ErrEvidence) {
		t.Errorf("unvalidated resolve: got %v, want ErrEvidence", err)
	}
	if err := st.Resolve("work", &Evidence{Outcome: StateCompleted, Validated: true}); !errors.Is(err, ErrEvidence) {
		t.Errorf("resolve without provenance: got %v, want ErrEvidence", err)
	}

	// No blind re-dispatch, ever.
	if err := st.Dispatch(context.Background(), "work", "do something", exec); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("re-dispatch: got %v, want ErrInvalidTransition", err)
	}

	// Validated evidence (authenticated executor query) resolves it.
	two := 2
	if err := st.Resolve("work", &Evidence{
		Outcome: StateCompleted, Result: "done", Usage: &two, Effect: EffectNone,
		Provenance: "authenticated query", Validated: true,
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get("work")
	if rec.State != StateCompleted {
		t.Fatalf("state = %s, want Completed", rec.State)
	}
	if got := rec.Allowance; got.Consumed != 2 || got.Locked != 0 || got.ReleasedUnused != 1 {
		t.Errorf("allowance = %+v, want consumed 2, released 1", got)
	}
}

// Table T12 → Canceled: a confirmed stop without completion resolves to
// Canceled. Usage still unknown stays locked (outcome evidence is not usage
// evidence).
func TestT12_ToCanceled_UsageStaysLocked(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.LoseResult = true
	_ = st.Dispatch(context.Background(), "work", "do something", exec)

	if err := st.Resolve("work", &Evidence{
		Outcome: StateCanceled, Effect: EffectPartial,
		Provenance: "operator resolution", Validated: true,
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("work")
	if rec.State != StateCanceled || rec.Effect != EffectPartial {
		t.Fatalf("state = %s effect = %s", rec.State, rec.Effect)
	}
	if rec.Allowance.Locked != 3 {
		t.Errorf("locked = %d, want 3 (operator resolution refunds nothing uncertain)", rec.Allowance.Locked)
	}
	root, _ := st.Root("root")
	if root.Available != 7 {
		t.Errorf("available = %d, want 7: locked units stay out of the pool", root.Available)
	}
}
