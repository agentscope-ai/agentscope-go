package tierrouter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fault test F1 (R1): normal route to the preferred tier. Grading reads the
// decision log and the providers' request records — never a provider's
// self-report.
func TestFaultF1_RecordedDecision(t *testing.T) {
	device := NewFakeProvider(TierDevice)
	edge := NewFakeProvider(TierEdge)
	cloud := NewFakeProvider(TierCloud)
	cloud.Reply = "cloud-answer"
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierCloud, TierEdge, TierDevice}}, device, edge, cloud)

	op := &Operation{ID: "f1", TaskEpoch: 1, Kind: "chat", Content: "hello"}
	rec, err := r.Route(context.Background(), op)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if rec.State != StateCompleted {
		t.Fatalf("state: %s", rec.State)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].Tier != TierCloud ||
		rec.Attempts[0].Reason != "policy-preferred" ||
		rec.Attempts[0].Outcome != AttemptCompleted || !rec.Attempts[0].Settled {
		t.Fatalf("attempt record: %+v", rec.Attempts)
	}
	if got := cloud.Requests(); len(got) != 1 || got[0].ID != "f1" {
		t.Fatalf("cloud record: %+v", got)
	}
	if len(edge.Requests())+len(device.Requests()) != 0 {
		t.Fatal("edge/device must not be touched on a preferred-tier success")
	}
	if res, _ := rec.Result(); res != "cloud-answer" {
		t.Fatalf("result: %q", res)
	}
}

// Fault test F2 (R2): a stale-epoch operation is rejected at the decision
// point and the rejection is recorded; a fresh operation under the advanced
// epoch routes normally.
func TestFaultF2_StaleEpochRejected(t *testing.T) {
	device := NewFakeProvider(TierDevice)
	epoch := int64(1)
	r := newHarness(t, "", func() int64 { return epoch },
		StaticPolicy{Default: []Tier{TierDevice}}, device)

	if _, err := r.Route(context.Background(), &Operation{ID: "f2-a", TaskEpoch: 1, Content: "a"}); err != nil {
		t.Fatalf("route under epoch 1: %v", err)
	}
	epoch = 2

	stale := &Operation{ID: "f2-b", TaskEpoch: 1, Content: "b"}
	rec, err := r.Route(context.Background(), stale)
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("want ErrStaleEpoch, got %v", err)
	}
	if rec.State != StateRejected || rec.RejectCode != rejectStaleEpoch || len(rec.Attempts) != 0 {
		t.Fatalf("rejection record: %+v", rec)
	}
	if n := len(device.Requests()); n != 1 {
		t.Fatalf("device record holds %d requests, want 1 (stale op must not dispatch)", n)
	}

	fresh := &Operation{ID: "f2-c", TaskEpoch: 2, Content: "c"}
	if _, err := r.Route(context.Background(), fresh); err != nil {
		t.Fatalf("fresh op under epoch 2: %v", err)
	}
	if n := len(device.Requests()); n != 2 {
		t.Fatalf("device record holds %d requests, want 2", n)
	}
}

// Fault test F3 (R3): duplicate id with same content replays without
// re-dispatch; conflicting content is rejected; both hold across restart.
func TestFaultF3_IdempotentIdentityAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")
	epoch := func() int64 { return 1 }
	policy := StaticPolicy{Default: []Tier{TierCloud}}
	cloud := NewFakeProvider(TierCloud)
	cloud.Reply = "r1"

	r := newHarness(t, path, epoch, policy, cloud)
	op := &Operation{ID: "f3", TaskEpoch: 1, Content: "same"}
	if _, err := r.Route(context.Background(), op); err != nil {
		t.Fatalf("route: %v", err)
	}
	rec, err := r.Route(context.Background(), op)
	if err != nil {
		t.Fatalf("duplicate replay must succeed, got %v", err)
	}
	if res, _ := rec.Result(); res != "r1" {
		t.Fatalf("replayed result: %q", res)
	}
	if n := len(cloud.Requests()); n != 1 {
		t.Fatalf("provider record holds %d requests, want 1 (replay must not re-dispatch)", n)
	}

	conflicting := *op
	conflicting.Content = "different"
	if _, err := r.Route(context.Background(), &conflicting); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("want ErrIDConflict, got %v", err)
	}

	// Restart: recreate the router against the same log file with fresh
	// providers. The recorded outcome must still replay.
	cloud2 := NewFakeProvider(TierCloud)
	r2 := newHarness(t, path, epoch, policy, cloud2)
	rec, err = r2.Route(context.Background(), op)
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if res, _ := rec.Result(); res != "r1" {
		t.Fatalf("replayed result after restart: %q", res)
	}
	if n := len(cloud2.Requests()); n != 0 {
		t.Fatalf("provider received %d requests after restart, want 0", n)
	}
	if _, err := r2.Route(context.Background(), &conflicting); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("conflict after restart: want ErrIDConflict, got %v", err)
	}
}

// Fault test F4 (R4): a clean cloud failure falls back to the edge as a new
// recorded attempt carrying the failure evidence; a local-only operation
// whose device tier fails is terminal even when the policy prefers edge.
func TestFaultF4_GuardedFallback(t *testing.T) {
	device := NewFakeProvider(TierDevice)
	edge := NewFakeProvider(TierEdge)
	edge.Reply = "edge-answer"
	cloud := NewFakeProvider(TierCloud)
	cloud.FailNext = 1
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierCloud, TierEdge, TierDevice}}, device, edge, cloud)

	rec, err := r.Route(context.Background(), &Operation{ID: "f4-a", TaskEpoch: 1, Content: "x"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if rec.State != StateCompleted {
		t.Fatalf("state: %s", rec.State)
	}
	if len(rec.Attempts) != 2 {
		t.Fatalf("attempts: %+v", rec.Attempts)
	}
	failed, fallback := rec.Attempts[0], rec.Attempts[1]
	if failed.Tier != TierCloud || failed.Outcome != AttemptFailed || failed.Err == "" {
		t.Fatalf("failure evidence attempt: %+v", failed)
	}
	if fallback.Tier != TierEdge || fallback.Reason != "fallback-after-failure:cloud" ||
		fallback.Outcome != AttemptCompleted {
		t.Fatalf("fallback attempt: %+v", fallback)
	}
	if len(cloud.Requests()) != 1 || len(edge.Requests()) != 1 || len(device.Requests()) != 0 {
		t.Fatalf("provider records: cloud=%d edge=%d device=%d",
			len(cloud.Requests()), len(edge.Requests()), len(device.Requests()))
	}

	// local_only with a failing device: terminal, and the guard binds even
	// though the policy prefers edge.
	device2 := NewFakeProvider(TierDevice)
	device2.FailNext = 1
	edge2 := NewFakeProvider(TierEdge)
	r2 := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierEdge, TierDevice}}, device2, edge2)
	rec, err = r2.Route(context.Background(), &Operation{ID: "f4-b", TaskEpoch: 1, LocalOnly: true, Content: "y"})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	if rec.State != StateExhausted || len(rec.Attempts) != 1 || rec.Attempts[0].Tier != TierDevice {
		t.Fatalf("record: %+v", rec)
	}
	if len(edge2.Requests()) != 0 {
		t.Fatal("local-only operation reached the edge tier")
	}
	// Replaying the exhausted operation replays the recorded exhaustion.
	if _, err := r2.Route(context.Background(), &Operation{ID: "f4-b", TaskEpoch: 1, LocalOnly: true, Content: "y"}); !errors.Is(err, ErrExhausted) {
		t.Fatalf("replay: want ErrExhausted, got %v", err)
	}
	if len(device2.Requests()) != 1 {
		t.Fatal("exhausted replay must not re-dispatch")
	}
}

// Fault test F5 (R5): a mid-call deadline records the attempt unknown and the
// operation uncertain; no fallback is attempted; retry means a new id.
func TestFaultF5_UnknownOutcomeNotRetried(t *testing.T) {
	edge := NewFakeProvider(TierEdge)
	cloud := NewFakeProvider(TierCloud)
	cloud.HangNext = 1
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, edge, cloud)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec, err := r.Route(ctx, &Operation{ID: "f5-a", TaskEpoch: 1, Content: "x"})
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("want ErrUncertain, got %v", err)
	}
	if rec.State != StateUncertain {
		t.Fatalf("state: %s", rec.State)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptUnknown {
		t.Fatalf("attempt: %+v", rec.Attempts)
	}
	if len(edge.Requests()) != 0 {
		t.Fatal("fallback after an unknown outcome is forbidden")
	}
	if n := len(cloud.Requests()); n != 1 {
		t.Fatalf("cloud record holds %d requests, want 1", n)
	}

	// Replaying the same id returns the recorded uncertainty.
	if _, err := r.Route(context.Background(), &Operation{ID: "f5-a", TaskEpoch: 1, Content: "x"}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay: want ErrUncertain, got %v", err)
	}
	// A new operation id retries cleanly and the cloud answers.
	if _, err := r.Route(context.Background(), &Operation{ID: "f5-b", TaskEpoch: 1, Content: "x"}); err != nil {
		t.Fatalf("retry under a new id: %v", err)
	}
	if n := len(cloud.Requests()); n != 2 {
		t.Fatalf("cloud record holds %d requests, want 2", n)
	}
}

// Fault test F6 (R2): an operation whose TTL elapsed before it reached the
// decision point is rejected and never dispatched.
func TestFaultF6_ExpiredBeforeDispatch(t *testing.T) {
	device := NewFakeProvider(TierDevice)
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, device)

	op := &Operation{
		ID: "f6", TaskEpoch: 1, Content: "x",
		TTL: time.Second, SubmittedAt: time.Now().Add(-time.Minute),
	}
	rec, err := r.Route(context.Background(), op)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	if rec.State != StateRejected || rec.RejectCode != rejectExpired {
		t.Fatalf("rejection record: %+v", rec)
	}
	if len(device.Requests()) != 0 {
		t.Fatal("expired operation must not dispatch")
	}
}

// Fault test F7 (R6): the process dies between the commit boundary and the
// outcome. The persisted form is an unsettled attempt; reopening reconciles
// it to uncertain and never silently re-dispatches. The fixture constructs
// exactly the crash-window state — taskenvelope's FakeEdge kills a process;
// here the log file is the only carry-over, so writing the crash state
// directly is equivalent and deterministic.
func TestFaultF7_CrashWindowReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")

	l, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	l.records["f7"] = &Record{
		ID: "f7", ContentHash: (&Operation{Kind: "chat", Content: "x"}).contentHash(),
		TaskEpoch: 1, State: StatePending, SubmittedAt: time.Now(),
		Attempts: []Attempt{{Seq: 1, Tier: TierCloud, Reason: "policy-preferred", RecordedAt: time.Now()}},
	}
	l.mu.Lock()
	if err := l.persistLocked(); err != nil {
		l.mu.Unlock()
		t.Fatalf("persist crash state: %v", err)
	}
	l.mu.Unlock()

	cloud := NewFakeProvider(TierCloud)
	cloud.Reply = "late-answer"
	r := newHarness(t, path, func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierCloud}}, cloud)

	rec, ok := r.log.Get("f7")
	if !ok {
		t.Fatal("record lost across reopen")
	}
	if rec.State != StateUncertain {
		t.Fatalf("reconciled state: %s", rec.State)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptUnknown || !rec.Attempts[0].Settled {
		t.Fatalf("reconciled attempt: %+v", rec.Attempts)
	}

	op := &Operation{ID: "f7", TaskEpoch: 1, Kind: "chat", Content: "x"}
	if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay: want ErrUncertain, got %v", err)
	}
	if len(cloud.Requests()) != 0 {
		t.Fatal("a crash-window record must never be silently re-dispatched")
	}
}

// Fault test F8 (R3): a concurrent duplicate observes the in-flight record
// (ErrInFlight) instead of double-dispatching; once the first call settles,
// the duplicate replays the recorded outcome.
func TestFaultF8_ConcurrentDuplicate(t *testing.T) {
	device := NewFakeProvider(TierDevice)
	device.HangNext = 1
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, device)

	op := &Operation{ID: "f8", TaskEpoch: 1, Content: "x"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Route(ctx, op)
		done <- err
	}()

	// Wait until the first call has committed its attempt, then submit the
	// identical operation concurrently.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok := r.log.Get("f8")
		if ok && len(rec.Attempts) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first call never committed its attempt")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrInFlight) {
		t.Fatalf("want ErrInFlight, got %v", err)
	}
	if n := len(device.Requests()); n != 1 {
		t.Fatalf("device record holds %d requests, want 1", n)
	}

	cancel()
	if err := <-done; !errors.Is(err, ErrUncertain) {
		t.Fatalf("first call: want ErrUncertain, got %v", err)
	}
	if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrUncertain) {
		t.Fatalf("settled replay: want ErrUncertain, got %v", err)
	}
}

// Fault test F9 (interpretation d): a policy tier with no attached provider
// is a clean failed attempt carrying configuration evidence; the chain
// continues to the next candidate.
func TestFaultF9_ProviderNotAttached(t *testing.T) {
	edge := NewFakeProvider(TierEdge)
	edge.Reply = "edge-answer"
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, edge)

	rec, err := r.Route(context.Background(), &Operation{ID: "f9", TaskEpoch: 1, Content: "x"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if rec.State != StateCompleted || len(rec.Attempts) != 2 {
		t.Fatalf("record: %+v", rec)
	}
	missed := rec.Attempts[0]
	if missed.Tier != TierCloud || missed.Outcome != AttemptFailed || missed.Err != "provider not attached" {
		t.Fatalf("missed-tier attempt: %+v", missed)
	}
	if rec.Attempts[1].Tier != TierEdge || rec.Attempts[1].Reason != "fallback-after-failure:cloud" {
		t.Fatalf("fallback attempt: %+v", rec.Attempts[1])
	}
}

// A corrupt log file fails Open loudly instead of silently starting empty.
func TestFaultCorruptLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt log: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt log opened without error")
	}
}

// Fault test F10 (R6): the process dies after admission but before the first
// attempt — the log holds a pending record with zero attempts. Reopening
// must reconcile it to uncertain; nothing is in flight after a restart.
func TestFaultF10_CrashBeforeFirstAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")

	l, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	l.records["f10"] = &Record{
		ID: "f10", ContentHash: (&Operation{Content: "x"}).contentHash(),
		TaskEpoch: 1, State: StatePending, SubmittedAt: time.Now(),
	}
	l.mu.Lock()
	if err := l.persistLocked(); err != nil {
		l.mu.Unlock()
		t.Fatalf("persist crash state: %v", err)
	}
	l.mu.Unlock()

	device := NewFakeProvider(TierDevice)
	r := newHarness(t, path, func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, device)

	rec, ok := r.log.Get("f10")
	if !ok {
		t.Fatal("record lost across reopen")
	}
	if rec.State != StateUncertain {
		t.Fatalf("reconciled state: %s, want %s", rec.State, StateUncertain)
	}
	if _, err := r.Route(context.Background(), &Operation{ID: "f10", TaskEpoch: 1, Content: "x"}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay: want ErrUncertain, got %v", err)
	}
	if len(device.Requests()) != 0 {
		t.Fatal("crash residue must never be silently re-dispatched")
	}
}

// Fault test F11 (R6): the process dies between attempts — the log holds a
// pending record whose attempts are all settled (a clean failure, fallback
// never dispatched). Reopening must reconcile it to uncertain.
func TestFaultF11_CrashBetweenAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")

	l, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	l.records["f11"] = &Record{
		ID: "f11", ContentHash: (&Operation{Content: "x"}).contentHash(),
		TaskEpoch: 1, State: StatePending, SubmittedAt: time.Now(),
		Attempts: []Attempt{{
			Seq: 1, Tier: TierCloud, Reason: "policy-preferred",
			Outcome: AttemptFailed, Err: "connection refused",
			RecordedAt: time.Now(), Settled: true,
		}},
	}
	l.mu.Lock()
	if err := l.persistLocked(); err != nil {
		l.mu.Unlock()
		t.Fatalf("persist crash state: %v", err)
	}
	l.mu.Unlock()

	edge := NewFakeProvider(TierEdge)
	r := newHarness(t, path, func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, edge)

	rec, ok := r.log.Get("f11")
	if !ok {
		t.Fatal("record lost across reopen")
	}
	if rec.State != StateUncertain {
		t.Fatalf("reconciled state: %s, want %s", rec.State, StateUncertain)
	}
	// The settled clean failure survives as evidence.
	if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptFailed {
		t.Fatalf("settled attempt must be preserved: %+v", rec.Attempts)
	}
	if _, err := r.Route(context.Background(), &Operation{ID: "f11", TaskEpoch: 1, Content: "x"}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay: want ErrUncertain, got %v", err)
	}
	if len(edge.Requests()) != 0 {
		t.Fatal("the undispatched fallback must not fire after reopen")
	}
}

// hookPolicy runs hook when the policy is consulted — after admit's guard
// check, before the first commit boundary — which is how a test interposes
// an epoch advance into that window.
type hookPolicy struct {
	inner Policy
	hook  func()
}

func (p hookPolicy) Prefer(op *Operation) []Tier {
	p.hook()
	return p.inner.Prefer(op)
}

// hookProvider runs hook as the provider call returns, modeling an epoch
// advance or clock jump while the preferred provider is in flight.
type hookProvider struct {
	inner *FakeProvider
	hook  func()
}

func (p hookProvider) Tier() Tier { return p.inner.Tier() }

func (p hookProvider) Complete(ctx context.Context, op *Operation) (string, error) {
	defer p.hook()
	return p.inner.Complete(ctx, op)
}

// Fault test F12 (R2): the epoch advances after admission but before the
// first dispatch. The commit boundary rechecks the guards, supersedes the
// operation, and nothing is dispatched.
func TestFaultF12_EpochAdvanceBeforeFirstDispatch(t *testing.T) {
	epoch := int64(1)
	device := NewFakeProvider(TierDevice)
	policy := hookPolicy{
		inner: StaticPolicy{Default: []Tier{TierDevice}},
		hook:  func() { epoch = 2 },
	}
	r := newHarness(t, "", func() int64 { return epoch }, policy, device)

	rec, err := r.Route(context.Background(), &Operation{ID: "f12", TaskEpoch: 1, Content: "x"})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("want ErrStaleEpoch, got %v", err)
	}
	if rec.State != StateSuperseded || rec.StopReason != rejectStaleEpoch {
		t.Fatalf("record: %+v", rec)
	}
	if len(rec.Attempts) != 0 {
		t.Fatalf("superseded before any attempt, got %+v", rec.Attempts)
	}
	if len(device.Requests()) != 0 {
		t.Fatal("stale operation must not dispatch")
	}
	// Replay reports the recorded supersession, not a fresh admission.
	if _, err := r.Route(context.Background(), &Operation{ID: "f12", TaskEpoch: 1, Content: "x"}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("replay: want ErrStaleEpoch, got %v", err)
	}
	if len(device.Requests()) != 0 {
		t.Fatal("superseded replay must not dispatch")
	}
}

// Fault test F13 (R2): the epoch advances, or the TTL elapses, while the
// preferred provider is in flight; the in-flight attempt settles truthfully
// and the fallback is never dispatched.
func TestFaultF13_GuardStopsFallback(t *testing.T) {
	t.Run("epoch advance", func(t *testing.T) {
		epoch := int64(1)
		cloud := NewFakeProvider(TierCloud)
		cloud.FailNext = 1
		edge := NewFakeProvider(TierEdge)
		r := newHarness(t, "", func() int64 { return epoch },
			StaticPolicy{Default: []Tier{TierCloud, TierEdge}},
			hookProvider{inner: cloud, hook: func() { epoch = 2 }}, edge)

		rec, err := r.Route(context.Background(), &Operation{ID: "f13-e", TaskEpoch: 1, Content: "x"})
		if !errors.Is(err, ErrStaleEpoch) {
			t.Fatalf("want ErrStaleEpoch, got %v", err)
		}
		if rec.State != StateSuperseded || rec.StopReason != rejectStaleEpoch {
			t.Fatalf("record: %+v", rec)
		}
		// The in-flight attempt's clean failure survives as evidence.
		if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptFailed {
			t.Fatalf("attempts: %+v", rec.Attempts)
		}
		if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
			t.Fatalf("cloud=%d edge=%d, want 1/0", len(cloud.Requests()), len(edge.Requests()))
		}
	})

	t.Run("ttl expiry", func(t *testing.T) {
		now := time.Now()
		clock := &now
		cloud := NewFakeProvider(TierCloud)
		cloud.FailNext = 1
		edge := NewFakeProvider(TierEdge)
		r := newHarness(t, "", func() int64 { return 1 },
			StaticPolicy{Default: []Tier{TierCloud, TierEdge}},
			hookProvider{inner: cloud, hook: func() { *clock = now.Add(2 * time.Minute) }}, edge)
		r.now = func() time.Time { return *clock }

		op := &Operation{ID: "f13-t", TaskEpoch: 1, Content: "x", TTL: time.Minute, SubmittedAt: now}
		rec, err := r.Route(context.Background(), op)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("want ErrExpired, got %v", err)
		}
		if rec.State != StateSuperseded || rec.StopReason != rejectExpired {
			t.Fatalf("record: %+v", rec)
		}
		if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
			t.Fatalf("cloud=%d edge=%d, want 1/0", len(cloud.Requests()), len(edge.Requests()))
		}
		if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrExpired) {
			t.Fatalf("replay: want ErrExpired, got %v", err)
		}
	})
}

// Fault test F13, success variant (R2): the epoch advances, or the TTL
// elapses, while the preferred provider is in flight and the call then
// completes successfully. The attempt stays as completed evidence, the
// operation settles superseded, and the stale success is never returned
// as a routing result — the base Router aligns with ChatRouter here.
func TestFaultF13_StaleSuccessIsSuperseded(t *testing.T) {
	t.Run("epoch advance", func(t *testing.T) {
		epoch := int64(1)
		cloud := NewFakeProvider(TierCloud)
		edge := NewFakeProvider(TierEdge)
		r := newHarness(t, "", func() int64 { return epoch },
			StaticPolicy{Default: []Tier{TierCloud, TierEdge}},
			hookProvider{inner: cloud, hook: func() { epoch = 2 }}, edge)

		rec, err := r.Route(context.Background(), &Operation{ID: "f13-es", TaskEpoch: 1, Content: "x"})
		if !errors.Is(err, ErrStaleEpoch) {
			t.Fatalf("want ErrStaleEpoch, got %v", err)
		}
		if rec.State != StateSuperseded || rec.StopReason != rejectStaleEpoch {
			t.Fatalf("record: %+v", rec)
		}
		// The in-flight attempt's success survives as evidence.
		if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptCompleted {
			t.Fatalf("attempts: %+v", rec.Attempts)
		}
		if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
			t.Fatalf("cloud=%d edge=%d, want 1/0", len(cloud.Requests()), len(edge.Requests()))
		}
	})

	t.Run("ttl expiry", func(t *testing.T) {
		now := time.Now()
		clock := &now
		cloud := NewFakeProvider(TierCloud)
		edge := NewFakeProvider(TierEdge)
		r := newHarness(t, "", func() int64 { return 1 },
			StaticPolicy{Default: []Tier{TierCloud, TierEdge}},
			hookProvider{inner: cloud, hook: func() { *clock = now.Add(2 * time.Minute) }}, edge)
		r.now = func() time.Time { return *clock }

		op := &Operation{ID: "f13-ts", TaskEpoch: 1, Content: "x", TTL: time.Minute, SubmittedAt: now}
		rec, err := r.Route(context.Background(), op)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("want ErrExpired, got %v", err)
		}
		if rec.State != StateSuperseded || rec.StopReason != rejectExpired {
			t.Fatalf("record: %+v", rec)
		}
		if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptCompleted {
			t.Fatalf("attempts: %+v", rec.Attempts)
		}
		if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
			t.Fatalf("cloud=%d edge=%d, want 1/0", len(cloud.Requests()), len(edge.Requests()))
		}
		// Replay returns the guard error, never the stale result.
		if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrExpired) {
			t.Fatalf("replay: want ErrExpired, got %v", err)
		}
	})
}

// Fault test F14 (R2, interpretation h): replaying a completed record after
// the root epoch advanced returns the historical record unchanged — replay
// preserves evidence, and using the result requires a separate current-state
// check by the caller.
func TestFaultF14_ReplayAfterEpochAdvancePreservesHistory(t *testing.T) {
	epoch := int64(1)
	cloud := NewFakeProvider(TierCloud)
	cloud.Reply = "epoch-1-answer"
	r := newHarness(t, "", func() int64 { return epoch },
		StaticPolicy{Default: []Tier{TierCloud}}, cloud)

	op := &Operation{ID: "f14", TaskEpoch: 1, Content: "x"}
	if _, err := r.Route(context.Background(), op); err != nil {
		t.Fatalf("route: %v", err)
	}
	epoch = 2

	rec, err := r.Route(context.Background(), op)
	if err != nil {
		t.Fatalf("replay must return the historical record, got %v", err)
	}
	if rec.State != StateCompleted || rec.TaskEpoch != 1 {
		t.Fatalf("record: %+v", rec)
	}
	if res, _ := rec.Result(); res != "epoch-1-answer" {
		t.Fatalf("historical result: %q", res)
	}
	if n := len(cloud.Requests()); n != 1 {
		t.Fatalf("replay re-dispatched: %d requests", n)
	}
}

// Fault test F15 (R2): the guards cease to hold after admission and the
// remaining candidates are all unattached — even the no-dispatch path must
// supersede rather than recording further attempts or reporting exhaustion.
func TestFaultF15_MissedTierAlsoSupersedes(t *testing.T) {
	epoch := int64(1)
	device := NewFakeProvider(TierDevice)
	device.FailNext = 1
	// Policy: device (attached, fails cleanly), then edge (NOT attached).
	// The epoch advances as the device call returns, before the missed-tier
	// record for edge would be written.
	devHook := hookProvider{inner: device, hook: func() { epoch = 2 }}
	r, err := NewRouter(mustOpen(t, ""), StaticPolicy{Default: []Tier{TierDevice, TierEdge}},
		func() int64 { return epoch }, devHook)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	rec, err := r.Route(context.Background(), &Operation{ID: "f15", TaskEpoch: 1, Content: "x"})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("want ErrStaleEpoch, got %v", err)
	}
	if rec.State != StateSuperseded || rec.StopReason != rejectStaleEpoch {
		t.Fatalf("record: %+v", rec)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptFailed {
		t.Fatalf("only the settled device attempt may be recorded: %+v", rec.Attempts)
	}
}
