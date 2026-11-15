package tierrouter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaticPolicyPrefer(t *testing.T) {
	p := StaticPolicy{
		Rules:   map[string][]Tier{"chat": {TierCloud, TierEdge}},
		Default: []Tier{TierDevice},
	}
	got := p.Prefer(&Operation{Kind: "chat"})
	if len(got) != 2 || got[0] != TierCloud || got[1] != TierEdge {
		t.Fatalf("rule lookup: got %v", got)
	}
	got[0] = TierDevice // mutate the copy
	if p.Rules["chat"][0] != TierCloud {
		t.Fatal("Prefer must return a copy")
	}
	if got := p.Prefer(&Operation{Kind: "embed"}); len(got) != 1 || got[0] != TierDevice {
		t.Fatalf("default: got %v", got)
	}
}

func TestNewRouterValidation(t *testing.T) {
	log := mustOpen(t, "")
	p := StaticPolicy{Default: []Tier{TierDevice}}
	epoch := func() int64 { return 1 }
	fp := NewFakeProvider(TierDevice)

	if _, err := NewRouter(nil, p, epoch, fp); err == nil {
		t.Fatal("nil log accepted")
	}
	if _, err := NewRouter(log, nil, epoch, fp); err == nil {
		t.Fatal("nil policy accepted")
	}
	if _, err := NewRouter(log, p, nil, fp); err == nil {
		t.Fatal("nil epoch accepted")
	}
	if _, err := NewRouter(log, p, epoch, fp, nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	if _, err := NewRouter(log, p, epoch, fp, NewFakeProvider(TierDevice)); err == nil {
		t.Fatal("duplicate tier accepted")
	}
}

func TestRouteRequiresID(t *testing.T) {
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, NewFakeProvider(TierDevice))
	if _, err := r.Route(context.Background(), &Operation{}); err == nil {
		t.Fatal("empty ID accepted")
	}
}

func TestLocalOnlyGuardFiltersToDevice(t *testing.T) {
	device := NewFakeProvider(TierDevice)
	edge := NewFakeProvider(TierEdge)
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierEdge, TierDevice}}, device, edge)

	// Policy prefers edge, but the guard must bind regardless of policy order.
	rec, err := r.Route(context.Background(), &Operation{ID: "op-guard", TaskEpoch: 1, LocalOnly: true, Content: "x"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if rec.State != StateCompleted || len(rec.Attempts) != 1 || rec.Attempts[0].Tier != TierDevice {
		t.Fatalf("expected single device attempt, got %+v", rec)
	}
	if n := len(edge.Requests()); n != 0 {
		t.Fatalf("edge received %d requests for a local-only operation", n)
	}
}

func TestLocalOnlyWithoutDeviceTierIsNoRoute(t *testing.T) {
	edge := NewFakeProvider(TierEdge)
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierEdge, TierCloud}}, edge, NewFakeProvider(TierCloud))

	rec, err := r.Route(context.Background(), &Operation{ID: "op-noroute", TaskEpoch: 1, LocalOnly: true, Content: "x"})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
	if rec.State != StateRejected || rec.RejectCode != rejectNoRoute {
		t.Fatalf("expected recorded no-route rejection, got %+v", rec)
	}
	if len(edge.Requests()) != 0 {
		t.Fatal("rejected operation must not dispatch")
	}
}

func TestRejectedReplayReturnsRecordedRejection(t *testing.T) {
	now := time.Now()
	clock := &now
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, NewFakeProvider(TierDevice))
	r.now = func() time.Time { return *clock }

	op := &Operation{
		ID: "op-expired", TaskEpoch: 1, Content: "x",
		TTL: time.Minute, SubmittedAt: now.Add(-2 * time.Minute),
	}
	if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// Time passes; replaying the duplicate must replay the recorded
	// rejection, not re-run the guards (DESIGN.md interpretation b).
	*clock = now.Add(time.Hour)
	if _, err := r.Route(context.Background(), op); !errors.Is(err, ErrExpired) {
		t.Fatalf("replay: want ErrExpired, got %v", err)
	}
}

func TestRecordCloneIsolation(t *testing.T) {
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, NewFakeProvider(TierDevice))
	rec, err := r.Route(context.Background(), &Operation{ID: "op-clone", TaskEpoch: 1, Content: "x"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	rec.Attempts[0].Result = "tampered"
	again, ok := r.log.Get("op-clone")
	if !ok {
		t.Fatal("record missing")
	}
	if again.Attempts[0].Result == "tampered" {
		t.Fatal("caller mutation leaked into the log")
	}
}

func TestResultAccessor(t *testing.T) {
	fp := NewFakeProvider(TierDevice)
	fp.Reply = "hello"
	r := newHarness(t, "", func() int64 { return 1 },
		StaticPolicy{Default: []Tier{TierDevice}}, fp)
	rec, err := r.Route(context.Background(), &Operation{ID: "op-res", TaskEpoch: 1, Content: "x"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res, ok := rec.Result(); !ok || res != "hello" {
		t.Fatalf("Result: got %q, %v", res, ok)
	}
	if _, ok := (&Record{}).Result(); ok {
		t.Fatal("empty record reports a result")
	}
}

// helpers

func mustOpen(t *testing.T, path string) *Log {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	return l
}

func newHarness(t *testing.T, path string, epoch func() int64, policy Policy, providers ...Provider) *Router {
	t.Helper()
	r, err := NewRouter(mustOpen(t, path), policy, epoch, providers...)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return r
}

// A persist failure before any external effect rolls the in-memory state
// back (DESIGN.md interpretation f): the operation ID is not poisoned, and a
// retry observes the same persist error rather than a phantom in-flight
// record.
func TestPersistFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	log := mustOpen(t, filepath.Join(dir, "sub", "decisions.json"))
	r, err := NewRouter(log, StaticPolicy{Default: []Tier{TierDevice}},
		func() int64 { return 1 }, NewFakeProvider(TierDevice))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	// Portable persist failure: a regular file occupies the log's parent
	// directory path, so MkdirAll fails on every OS (a read-only-directory
	// chmod is a no-op on Windows).
	if err := os.WriteFile(filepath.Join(dir, "sub"), []byte("x"), 0o600); err != nil {
		t.Fatalf("plant blocker: %v", err)
	}

	op := &Operation{ID: "persist", TaskEpoch: 1, Content: "x"}
	if _, err := r.Route(context.Background(), op); err == nil {
		t.Fatal("route succeeded against an unpersistable log")
	}
	if _, ok := log.Get("persist"); ok {
		t.Fatal("failed admission left an in-memory record")
	}
	// Retry: same honest persist failure, never ErrInFlight.
	if _, err := r.Route(context.Background(), op); err == nil || errors.Is(err, ErrInFlight) {
		t.Fatalf("retry: want persist error, got %v", err)
	}
}
