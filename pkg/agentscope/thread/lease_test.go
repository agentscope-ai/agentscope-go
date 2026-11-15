package thread

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time                     { return c.now }
func (c *fakeClock) Advance(d time.Duration)            { c.now = c.now.Add(d) }
func newFakeClock() *fakeClock                          { return &fakeClock{now: testTime} }
func expiresIn(c *fakeClock, d time.Duration) time.Time { return c.now.Add(d) }

func TestLeasesAcquire(t *testing.T) {
	c := newFakeClock()
	l := NewLeases(c.Now)
	lease, err := l.Acquire("camera", "edge", time.Minute)
	if err != nil || lease != (Lease{Resource: "camera", Holder: "edge", Expires: expiresIn(c, time.Minute)}) {
		t.Fatalf("Acquire = %+v, %v", lease, err)
	}
	if _, err := l.Acquire("camera", "cloud", time.Minute); !errors.Is(err, ErrLeaseHeld) || !strings.Contains(err.Error(), "edge holds camera") {
		t.Fatalf("second holder: %v, want ErrLeaseHeld naming edge", err)
	}
	if h, ok := l.Holder("camera"); !ok || h != "edge" {
		t.Fatalf("Holder = %q, %v", h, ok)
	}

	// The holder extends its own lease.
	c.Advance(50 * time.Second)
	lease, err = l.Acquire("camera", "edge", time.Minute)
	if err != nil || !lease.Expires.Equal(expiresIn(c, time.Minute)) {
		t.Fatalf("extend = %+v, %v", lease, err)
	}
	c.Advance(30 * time.Second) // past the first expiry, inside the extension
	if _, err := l.Acquire("camera", "cloud", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("extended lease not held: %v", err)
	}

	// An expired lease is free.
	c.Advance(30 * time.Second)
	if _, ok := l.Holder("camera"); ok {
		t.Fatal("expired lease still has a holder")
	}
	if _, err := l.Acquire("camera", "cloud", time.Minute); err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
	if h, _ := l.Holder("camera"); h != "cloud" {
		t.Fatalf("holder %q, want cloud", h)
	}
}

func TestLeasesAcquireInvalid(t *testing.T) {
	l := NewLeases(nil)
	for _, tc := range []struct {
		resource, holder string
		ttl              time.Duration
	}{
		{"", "edge", time.Second},
		{"camera", "", time.Second},
		{"camera", "edge", 0},
		{"camera", "edge", -time.Second},
	} {
		if _, err := l.Acquire(tc.resource, tc.holder, tc.ttl); err == nil || errors.Is(err, ErrLeaseHeld) {
			t.Errorf("Acquire(%q, %q, %v) = %v, want a usage error", tc.resource, tc.holder, tc.ttl, err)
		}
	}
	// The default clock is the wall clock.
	lease, err := l.Acquire("camera", "edge", time.Hour)
	if err != nil || time.Until(lease.Expires) <= 0 {
		t.Errorf("wall clock lease %+v, %v", lease, err)
	}
}

func TestLeasesRelease(t *testing.T) {
	c := newFakeClock()
	l := NewLeases(c.Now)
	if _, err := l.Acquire("disk", "edge", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Release("disk", "cloud"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("releasing another holder's lease: %v", err)
	}
	if err := l.Release("disk", "edge"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := l.Holder("disk"); ok {
		t.Fatal("released lease still held")
	}
	if err := l.Release("disk", "edge"); err != nil {
		t.Errorf("releasing a free resource: %v", err)
	}
	if _, err := l.Acquire("disk", "edge", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if err := l.Release("disk", "cloud"); err != nil {
		t.Errorf("releasing an expired lease: %v", err)
	}
}

func TestLeasesReleaseAll(t *testing.T) {
	c := newFakeClock()
	l := NewLeases(c.Now)
	for _, res := range []string{"a", "b"} {
		if _, err := l.Acquire(res, "edge", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Acquire("c", "edge", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("d", "cloud", time.Minute); err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Second) // "c" has expired
	if n := l.ReleaseAll("edge"); n != 2 {
		t.Fatalf("ReleaseAll released %d live leases, want 2", n)
	}
	for _, res := range []string{"a", "b", "c"} {
		if _, ok := l.Holder(res); ok {
			t.Errorf("%s still held", res)
		}
	}
	if h, ok := l.Holder("d"); !ok || h != "cloud" {
		t.Errorf("another holder's lease was released: %q %v", h, ok)
	}
	if n := l.ReleaseAll("nobody"); n != 0 {
		t.Errorf("ReleaseAll for nobody = %d", n)
	}
}
