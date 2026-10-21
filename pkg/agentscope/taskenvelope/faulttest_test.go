package taskenvelope

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Terminal-state membership is part of the contract (forbidden transitions).
func TestState_Terminal(t *testing.T) {
	for _, s := range []State{StateCompleted, StateFailed, StateCanceled, StateExpired} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	for _, s := range []State{StateProposed, StateAccepted, StateInProgress, StateCanceling, StateUnknownEffect} {
		if s.Terminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}

// dispatchRecover runs Dispatch, absorbing a simulated-crash panic from a
// store hook; the test then continues with a fresh Store over the same ledger
// file (a process restart), abandoning the old in-memory instance.
func dispatchRecover(st *Store, id string, exec Executor) {
	defer func() { _ = recover() }()
	_ = st.Dispatch(context.Background(), id, "do something", exec)
}

// Table "Commit boundary and crash windows", window 1 + reconciliation rule 2:
// crash after the reservation write but before the call was sent. Recovery
// queries the same attempt ID; the executor cannot account for it — which does
// NOT imply the call never happened — so the record goes to UnknownEffect and
// is never re-dispatched.
func TestCrashWindow_AfterReservationBeforeSend(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	edge, err := NewFakeEdge(t.TempDir() + "/edge.json")
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	st.beforeExternalCall = func() { panic("simulated crash") } // reservation persisted, call never sent
	dispatchRecover(st, "work", edge)

	// Restart: fresh store over the same file.
	st2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := st2.Get("work")
	if rec.State != StateInProgress || len(rec.Attempts) != 1 {
		t.Fatalf("after crash: state = %s, attempts = %v", rec.State, rec.Attempts)
	}

	if err := st2.Reconcile(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	rec, _ = st2.Get("work")
	if rec.State != StateUnknownEffect {
		t.Fatalf("state = %s, want UnknownEffect (no account ≠ never happened)", rec.State)
	}
	if rec.Allowance.Locked != 3 {
		t.Errorf("locked = %d, want 3", rec.Allowance.Locked)
	}
	// Never re-dispatched.
	if err := st2.Dispatch(context.Background(), "work", "do something", edge); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("re-dispatch: got %v, want ErrInvalidTransition", err)
	}
}

// Table "Commit boundary and crash windows", window 2 + reconciliation rule 2:
// crash after the call was sent but before the outcome was recorded. The
// executor can account for the attempt, so recovery lands Completed with the
// recorded effect.
func TestCrashWindow_AfterSendBeforeRecord(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	edge, err := NewFakeEdge(t.TempDir() + "/edge.json")
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	st.beforeOutcomeRecord = func() { panic("simulated crash") } // call done, outcome never recorded
	dispatchRecover(st, "work", edge)

	st2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := st2.Get("work")
	if rec.State != StateInProgress {
		t.Fatalf("after crash: state = %s", rec.State)
	}

	// The edge service restarts against the same files (rule 6: reconnect
	// alone is not restart recovery — the fixture kills and recreates).
	if err := edge.Restart(); err != nil {
		t.Fatal(err)
	}
	if err := st2.Reconcile(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	rec, _ = st2.Get("work")
	if rec.State != StateCompleted || rec.Result != "done" {
		t.Fatalf("state = %s result = %q, want Completed/done", rec.State, rec.Result)
	}
}

// Mapping 5: concurrent duplicate submissions serialize at the decision
// point — exactly one wins admission, later ones get the recorded status.
func TestConcurrentDuplicateSubmission(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)

	const n = 16
	var wg sync.WaitGroup
	states := make([]State, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := st.Submit(baseSubmission("dup"))
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			states[i] = rec.State
		}(i)
	}
	wg.Wait()

	for i, s := range states {
		if s != StateAccepted {
			t.Errorf("submission %d got state %s, want the recorded Accepted", i, s)
		}
	}
	root, _ := st.Root("root")
	if root.Available != 7 {
		t.Errorf("available = %d, want 7: allowance reserved exactly once", root.Available)
	}
}

// Table T8 + mapping 4: a cancel-ACK is never proof of stop by itself — no
// attestation means UnknownEffect, and the outcome arriving later is not
// recorded without validated evidence (T12 covers that path separately).
func TestCancelACKNotProof_T8(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	exec.StopAttested = false // ACK received, but no stop attestation
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "work", "do something", exec) }()
	for {
		rec, _ := st.Get("work")
		if rec.State == StateInProgress {
			break
		}
	}

	if err := st.Cancel("work"); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("work")
	if rec.State != StateCanceling {
		t.Fatalf("state = %s, want Canceling (T6)", rec.State)
	}

	if err := st.ProcessCancel(context.Background(), "work", exec); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get("work")
	if rec.State != StateUnknownEffect {
		t.Fatalf("state = %s, want UnknownEffect (T8: ACK is not proof)", rec.State)
	}

	close(exec.Gate)
	<-done // the executor did complete — but the record stays UnknownEffect here
	rec, _ = st.Get("work")
	if rec.State != StateUnknownEffect {
		t.Fatalf("state = %s after late outcome, want UnknownEffect until T12 evidence", rec.State)
	}
}

// Table T2b + mapping 4: cancellation while still Proposed — nothing
// dispatched, stop trivially attested, usage known-zero.
func TestCancelWhileProposed_T2b(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	if err := st.CancelRoot("root"); err != nil {
		t.Fatal(err)
	}

	rec, err := st.Submit(baseSubmission("late"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateCanceled || rec.Effect != EffectNone {
		t.Fatalf("state = %s effect = %s, want Canceled/none (T2b)", rec.State, rec.Effect)
	}
	if rec.Allowance != (Allowance{}) {
		t.Errorf("allowance = %+v, want zero ledger", rec.Allowance)
	}
}

// Mapping 4 (stated bound) + table T7: cancellation after dispatch cannot
// guarantee the executor stops; when the stop IS attested with an effect, the
// record settles as Canceled with the effect recorded — not a clean outcome.
func TestCancelAfterDispatch_AttestedPartial_T7(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	exec.StopEffect = EffectPartial
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "work", "do something", exec) }()
	for {
		rec, _ := st.Get("work")
		if rec.State == StateInProgress {
			break
		}
	}

	if err := st.Cancel("work"); err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessCancel(context.Background(), "work", exec); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("work")
	if rec.State != StateCanceled || rec.Effect != EffectPartial {
		t.Fatalf("state = %s effect = %s, want Canceled/partial (T7)", rec.State, rec.Effect)
	}
	close(exec.Gate)
	<-done
}

// Table T11: completion evidence is recordable whenever it occurred — here
// after the cancel was processed but before any stop took effect. Provenance
// is recorded.
func TestT11_CompletionAfterCancelProcessed(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	exec.Outcome = Outcome{Success: true, Result: "finished", Effect: EffectPartial}
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "work", "do something", exec) }()
	for {
		rec, _ := st.Get("work")
		if rec.State == StateInProgress {
			break
		}
	}

	if err := st.Cancel("work"); err != nil { // cancel processed; no stop attested yet
		t.Fatal(err)
	}
	close(exec.Gate) // the completion lands now, while Canceling
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	rec, _ := st.Get("work")
	if rec.State != StateCompleted || rec.Result != "finished" {
		t.Fatalf("state = %s result = %q, want Completed (T11)", rec.State, rec.Result)
	}
	found := false
	for _, p := range rec.Provenance {
		if p == "T11: dispatch work#1" {
			found = true
		}
	}
	if !found {
		t.Errorf("T11 provenance not recorded: %v", rec.Provenance)
	}
}

// Table boundary rule: a completion reported after an attested stop
// contradicts the attestation — an executor-integrity event, never recorded.
func TestBoundary_CompletionAfterAttestedStop(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "work", "do something", exec) }()
	for {
		rec, _ := st.Get("work")
		if rec.State == StateInProgress {
			break
		}
	}

	if err := st.Cancel("work"); err != nil {
		t.Fatal(err)
	}
	if err := st.ProcessCancel(context.Background(), "work", exec); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("work")
	if rec.State != StateCanceled {
		t.Fatalf("state = %s, want Canceled (T7)", rec.State)
	}

	close(exec.Gate) // the completion arrives after the attested stop
	if err := <-done; !errors.Is(err, ErrIntegrity) {
		t.Fatalf("dispatch returned %v, want ErrIntegrity", err)
	}
	rec, _ = st.Get("work")
	if rec.State != StateCanceled || rec.Result != "" {
		t.Errorf("record mutated by a post-attestation completion: state=%s result=%q", rec.State, rec.Result)
	}
}

// Mapping 6 + table T10/T12: ACK lost but the action executed — UnknownEffect
// with locked usage, then validated evidence (authenticated query) resolves
// to Completed. The device keeps action authority throughout: nothing in the
// record authorizes a re-dispatch.
func TestAckLostAction_UnknownEffect_T12(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.LoseResult = true // ACK lost, action executed
	if err := st.Dispatch(context.Background(), "work", "do something", exec); err == nil {
		t.Fatal("expected unknown-outcome error")
	}
	rec, _ := st.Get("work")
	if rec.State != StateUnknownEffect {
		t.Fatalf("state = %s, want UnknownEffect (T10)", rec.State)
	}

	// The executor can account for the attempt — that is validated evidence.
	attemptID := rec.Attempts[0].ID
	outcome, accounted, err := exec.QueryAttempt(context.Background(), attemptID)
	if err != nil || !accounted {
		t.Fatalf("query: %v, accounted=%v", err, accounted)
	}
	if err := st.Resolve("work", &Evidence{
		Outcome: StateCompleted, Result: outcome.Result, Effect: outcome.Effect,
		Provenance: "authenticated query " + attemptID, Validated: true,
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get("work")
	if rec.State != StateCompleted {
		t.Fatalf("state = %s, want Completed (T12)", rec.State)
	}
	// Usage evidence was absent, so the allowance stays locked.
	if rec.Allowance.Locked != 3 {
		t.Errorf("locked = %d, want 3: outcome evidence is not usage evidence", rec.Allowance.Locked)
	}
}

// Reconciliation rules 1+5: restart with uncertain allowance — a Completed
// record with unknown usage keeps its locked units across restart, and
// outcome reconciliation never auto-unlocks them.
func TestRestart_UncertainAllowanceStaysLocked(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor() // usage unknown (nil)
	if err := st.Dispatch(context.Background(), "work", "do something", exec); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Reconcile(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	rec, _ := st2.Get("work")
	if rec.State != StateCompleted || rec.Allowance.Locked != 3 {
		t.Errorf("state = %s, locked = %d, want Completed with locked 3", rec.State, rec.Allowance.Locked)
	}
}

// Reconciliation rule 3: Accepted at restart re-runs the T3 guard set; if the
// deadline passed it settles as Expired, if authority no longer covers the
// scope it settles as Canceled (nothing dispatched).
func TestRestart_AcceptedRerunsGuards(t *testing.T) {
	clk := &testClock{now: time.Now()}
	path := t.TempDir() + "/ledger.json"
	st, err := Open(path, &Options{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)

	old := baseSubmission("deadline")
	old.TTL = time.Minute
	submitOk(t, st, old)
	submitOk(t, st, baseSubmission("authority"))

	clk.Advance(2 * time.Minute) // deadline record's TTL elapsed while persisted

	// Restart with the same clock but a narrower authority.
	st2, err := Open(path, &Options{Now: clk.Now, Authority: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Reconcile(context.Background(), NewSimExecutor()); err != nil {
		t.Fatal(err)
	}
	rec, _ := st2.Get("deadline")
	if rec.State != StateExpired {
		t.Errorf("deadline: state = %s, want Expired (rule 3)", rec.State)
	}
	rec, _ = st2.Get("authority")
	if rec.State != StateCanceled {
		t.Errorf("authority: state = %s, want Canceled (rule 3)", rec.State)
	}
}

// Reconciliation rule 4: Canceling at restart stays Canceling until T7/T8
// resolves via the executor.
func TestRestart_CancelingStays(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "work", "do something", exec) }()
	for {
		rec, _ := st.Get("work")
		if rec.State == StateInProgress {
			break
		}
	}
	if err := st.Cancel("work"); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Reconcile(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	rec, _ := st2.Get("work")
	if rec.State != StateCanceling {
		t.Errorf("state = %s, want Canceling (rule 4)", rec.State)
	}
	close(exec.Gate)
	<-done
}

// Table T6 + mapping 3: parent epoch advance moves Accepted records to
// Canceled (nothing dispatched) and InProgress to Canceling; a stale-epoch
// submission is rejected at T1.
func TestEpochAdvance_T6(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("accepted"))

	flightSub := baseSubmission("flight")
	submitOk(t, st, flightSub)
	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "flight", "do something", exec) }()
	for {
		rec, _ := st.Get("flight")
		if rec.State == StateInProgress {
			break
		}
	}

	if _, err := st.AdvanceEpoch("root"); err != nil {
		t.Fatal(err)
	}

	rec, _ := st.Get("accepted")
	if rec.State != StateCanceled {
		t.Errorf("accepted: state = %s, want Canceled (T6+T7, nothing dispatched)", rec.State)
	}
	rec, _ = st.Get("flight")
	if rec.State != StateCanceling {
		t.Errorf("flight: state = %s, want Canceling (T6)", rec.State)
	}

	// Stale-epoch rejection at T1.
	rec, err := st.Submit(baseSubmission("stale")) // epoch 0, current is 1
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateFailed {
		t.Errorf("stale submission: state = %s, want Failed (T1/T2)", rec.State)
	}

	close(exec.Gate)
	<-done
}

// Mapping 3 (T4's guard): a completion recorded truthfully never authorizes
// by itself — the record carries its epoch and the root's current epoch is
// separately observable, so a stale result is detectable before use. The
// root-side recheck is the caller's duty; the record only refuses to lie.
func TestT4_RecordNeverAuthorizes(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))

	if err := st.Dispatch(context.Background(), "work", "do something", NewSimExecutor()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceEpoch("root"); err != nil {
		t.Fatal(err)
	}

	rec, _ := st.Get("work")
	if rec.State != StateCompleted {
		t.Fatalf("state = %s, want Completed (T4 records truthfully)", rec.State)
	}
	if rec.TaskEpoch == st.Epoch("root") {
		t.Error("record epoch must keep its submission epoch so staleness is observable")
	}
	// The root revalidates separately: current epoch 1 ≠ record epoch 0.
	if st.Epoch("root") != 1 {
		t.Errorf("current epoch = %d, want 1", st.Epoch("root"))
	}
}

// Table T11 + boundary (push path): RecordOutcome ingests completion evidence
// for in-flight and Canceling records, and refuses terminal/UnknownEffect
// records (T12 requires Resolve).
func TestRecordOutcome_PushPaths(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)

	submitOk(t, st, baseSubmission("direct"))
	exec := NewSimExecutor()
	exec.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- st.Dispatch(context.Background(), "direct", "do something", exec) }()
	for {
		rec, _ := st.Get("direct")
		if rec.State == StateInProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Push completion while InProgress (pull-equivalent of T4).
	if err := st.RecordOutcome("direct", Outcome{Success: true, Result: "pushed", Effect: EffectNone}, "edge push"); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("direct")
	if rec.State != StateCompleted || rec.Result != "pushed" {
		t.Errorf("state = %s result = %q, want Completed/pushed", rec.State, rec.Result)
	}
	close(exec.Gate) // the duplicate pull outcome now contradicts a terminal record
	<-done

	// Push completion while Canceling (T11).
	submitOk(t, st, baseSubmission("late"))
	exec2 := NewSimExecutor()
	exec2.Gate = make(chan struct{})
	done2 := make(chan error, 1)
	go func() { done2 <- st.Dispatch(context.Background(), "late", "do something", exec2) }()
	for {
		rec, _ := st.Get("late")
		if rec.State == StateInProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := st.Cancel("late"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordOutcome("late", Outcome{Success: true, Result: "finished", Effect: EffectNone}, "edge push"); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.Get("late")
	if rec.State != StateCompleted {
		t.Errorf("state = %s, want Completed (T11)", rec.State)
	}
	found := false
	for _, p := range rec.Provenance {
		if p == "T11: edge push" {
			found = true
		}
	}
	if !found {
		t.Errorf("T11 push provenance missing: %v", rec.Provenance)
	}
	close(exec2.Gate)
	// The duplicate pull outcome now lands on a terminal record.
	if err := <-done2; !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("duplicate completion on terminal record: got %v, want ErrInvalidTransition", err)
	}

	// Push to a terminal record → invalid transition.
	if err := st.RecordOutcome("direct", Outcome{Success: true}, "late push"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("terminal push: got %v, want ErrInvalidTransition", err)
	}
	// Push to UnknownEffect → rejected; T12 requires Resolve.
	submitOk(t, st, baseSubmission("unclear"))
	exec3 := NewSimExecutor()
	exec3.LoseResult = true
	_ = st.Dispatch(context.Background(), "unclear", "do something", exec3)
	if err := st.RecordOutcome("unclear", Outcome{Success: true}, "push"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("UnknownEffect push: got %v, want ErrInvalidTransition", err)
	}
}

// Cancel/ProcessCancel idempotence on states where cancel is a no-op.
func TestCancel_NoOpStates(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("work"))
	if err := st.Dispatch(context.Background(), "work", "do something", NewSimExecutor()); err != nil {
		t.Fatal(err)
	}
	// Terminal record: cancel is a no-op.
	if err := st.Cancel("work"); err != nil {
		t.Errorf("cancel on Completed: %v", err)
	}
	// ProcessCancel on a non-Canceling record is a no-op.
	if err := st.ProcessCancel(context.Background(), "work", NewSimExecutor()); err != nil {
		t.Errorf("ProcessCancel on Completed: %v", err)
	}
	if err := st.Cancel("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancel missing: %v, want ErrNotFound", err)
	}
}

// Table T3: the dispatch decision point rechecks deadline, attempts,
// allowance and authority at dispatch time — not relying on T1. (The epoch
// and cancellation rechecks sit behind the AdvanceEpoch/CancelRoot sweeps,
// which already move the record out of Accepted; they are defensive depth.)
func TestT3_DispatchGuardRecheck(t *testing.T) {
	clk := &testClock{now: time.Now()}
	exec := NewSimExecutor()

	// Deadline elapsed between admission and dispatch.
	st := newTestStore(t, clk)
	setupRoot(t, st, 10)
	sub := baseSubmission("deadline")
	sub.TTL = time.Minute
	submitOk(t, st, sub)
	clk.Advance(2 * time.Minute)
	if err := st.Dispatch(context.Background(), "deadline", "do something", exec); err == nil {
		t.Error("dispatch past deadline must fail at the decision point")
	}
	if rec, _ := st.Get("deadline"); rec.State != StateAccepted {
		t.Errorf("state = %s: a rejected dispatch does not move the record", rec.State)
	}

	// MaxAttempts = 0: admitted, never dispatchable.
	clk2 := &testClock{now: time.Now()}
	st2 := newTestStore(t, clk2)
	setupRoot(t, st2, 10)
	zeroAttempts := baseSubmission("no-attempts")
	zeroAttempts.MaxAttempts = 0
	submitOk(t, st2, zeroAttempts)
	if err := st2.Dispatch(context.Background(), "no-attempts", "do something", exec); err == nil {
		t.Error("dispatch with MaxAttempts=0 must fail")
	}

	// Allowance zero: admitted with nothing reserved, dispatch refuses.
	st3 := newTestStore(t, clk2)
	setupRoot(t, st3, 10)
	zeroAllowance := baseSubmission("no-allowance")
	zeroAllowance.AllowanceUnits = 0
	submitOk(t, st3, zeroAllowance)
	if err := st3.Dispatch(context.Background(), "no-allowance", "do something", exec); err == nil {
		t.Error("dispatch with no reserved allowance must fail")
	}

	// Authority narrowed after admission: restart with a narrower authority
	// (roots persist in the ledger) and dispatch fails the recheck.
	path := t.TempDir() + "/ledger.json"
	st4, err := Open(path, &Options{Now: clk2.Now})
	if err != nil {
		t.Fatal(err)
	}
	setupRoot(t, st4, 10)
	submitOk(t, st4, baseSubmission("authority"))
	st5, err := Open(path, &Options{Now: clk2.Now, Authority: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if err := st5.Dispatch(context.Background(), "authority", "do something", exec); err == nil {
		t.Error("dispatch outside authority must fail at the decision point")
	}
}

// FakeEdge stop attestation: only an accounted attempt can be attested.
func TestFakeEdge_AttestStop(t *testing.T) {
	edge, err := NewFakeEdge(t.TempDir() + "/edge.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edge.Do(context.Background(), Attempt{ID: "a1", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	att, err := edge.AttestStop(context.Background(), "a1")
	if err != nil || !att.Attested {
		t.Errorf("attestation for accounted attempt: %+v, %v", att, err)
	}
	att, err = edge.AttestStop(context.Background(), "ghost")
	if err != nil || att.Attested {
		t.Errorf("attestation for unknown attempt must be negative, got %+v", att)
	}
}

// CancelRoot sweeps InProgress records to Canceling (T6) and rejects later
// submissions (T2b).
func TestCancelRoot_Sweep(t *testing.T) {
	st := newTestStore(t, nil)
	setupRoot(t, st, 10)
	submitOk(t, st, baseSubmission("flight"))

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

	if err := st.CancelRoot("root"); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("flight")
	if rec.State != StateCanceling {
		t.Errorf("state = %s, want Canceling (T6 via root cancel)", rec.State)
	}
	rec, err := st.Submit(baseSubmission("later"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateCanceled {
		t.Errorf("post-cancel submission: state = %s, want Canceled (T2b)", rec.State)
	}
	close(exec.Gate)
	<-done
}
