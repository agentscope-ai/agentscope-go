package thread

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrLeaseHeld is returned when a resource is leased to another holder.
var ErrLeaseHeld = errors.New("thread: resource is leased to another holder")

// Lease grants one holder a shared resource, such as a device or a file,
// until it expires or is released.
type Lease struct {
	Resource, Holder string
	Expires          time.Time
}

// Leases tracks the leases on shared resources two participants might touch
// at the same time. Expired leases are free. It is safe for concurrent use.
type Leases struct {
	mu     sync.Mutex
	clock  func() time.Time
	leases map[string]Lease
}

// NewLeases returns an empty lease table. A nil clock uses time.Now.
func NewLeases(clock func() time.Time) *Leases {
	if clock == nil {
		clock = time.Now
	}
	return &Leases{clock: clock, leases: make(map[string]Lease)}
}

// Acquire leases resource to holder for ttl. The same holder acquiring again
// extends its lease; an expired lease is free. A resource another holder
// leases fails with ErrLeaseHeld.
func (l *Leases) Acquire(resource, holder string, ttl time.Duration) (Lease, error) {
	if resource == "" || holder == "" {
		return Lease{}, errors.New("thread: a lease needs a resource and a holder")
	}
	if ttl <= 0 {
		return Lease{}, fmt.Errorf("thread: lease ttl %v is not positive", ttl)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if cur, ok := l.active(resource, now); ok && cur.Holder != holder {
		return Lease{}, fmt.Errorf("%w: %s holds %s until %s", ErrLeaseHeld, cur.Holder, resource, cur.Expires.Format(time.RFC3339))
	}
	lease := Lease{Resource: resource, Holder: holder, Expires: now.Add(ttl)}
	l.leases[resource] = lease
	return lease, nil
}

// Release ends holder's lease on resource. Releasing a resource nobody
// leases, or whose lease expired, is not an error; releasing someone else's
// lease is.
func (l *Leases) Release(resource, holder string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.active(resource, l.clock())
	if !ok {
		return nil
	}
	if cur.Holder != holder {
		return fmt.Errorf("%w: %s holds %s, not %s", ErrLeaseHeld, cur.Holder, resource, holder)
	}
	delete(l.leases, resource)
	return nil
}

// Holder returns the holder of the live lease on resource.
func (l *Leases) Holder(resource string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.active(resource, l.clock())
	return cur.Holder, ok
}

// ReleaseAll ends every lease holder has and returns how many were live.
func (l *Leases) ReleaseAll(holder string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	n := 0
	for res, cur := range l.leases {
		if cur.Holder != holder {
			continue
		}
		if now.Before(cur.Expires) {
			n++
		}
		delete(l.leases, res)
	}
	return n
}

// active returns the live lease on resource, dropping an expired one.
func (l *Leases) active(resource string, now time.Time) (Lease, bool) {
	cur, ok := l.leases[resource]
	if !ok {
		return Lease{}, false
	}
	if !now.Before(cur.Expires) {
		delete(l.leases, resource)
		return Lease{}, false
	}
	return cur, true
}
