package tierrouter

import (
	"context"
	"errors"
	"sync"
)

// FakeProvider is a deterministic scriptable Provider for fault tests. Its
// request record is grading evidence: tests assert against Requests, never
// against what the provider claims it did (DESIGN.md R1).
type FakeProvider struct {
	tier Tier

	// Reply is returned for every successful call.
	Reply string
	// FailErr is the clean-failure error; nil defaults to a generic failure.
	FailErr error
	// FailNext makes the next N calls fail cleanly (fallback allowed).
	FailNext int
	// HangNext makes the next N calls block until the caller's context ends,
	// then return the context error (unknown outcome, R5). A hang script
	// requires a cancellable or deadline context; with a non-cancellable
	// context the call blocks forever.
	HangNext int

	mu       sync.Mutex
	requests []Operation
}

var _ Provider = (*FakeProvider)(nil)

// NewFakeProvider returns a fake provider for the tier that answers every
// call with an empty result.
func NewFakeProvider(tier Tier) *FakeProvider {
	return &FakeProvider{tier: tier}
}

// Tier implements Provider.
func (f *FakeProvider) Tier() Tier { return f.tier }

// Complete implements Provider. Every call is recorded first; the script
// (FailNext/HangNext) decides the outcome.
func (f *FakeProvider) Complete(ctx context.Context, op *Operation) (string, error) {
	f.mu.Lock()
	f.requests = append(f.requests, *op)
	fail := f.FailNext > 0
	if fail {
		f.FailNext--
	}
	hang := f.HangNext > 0
	if hang {
		f.HangNext--
	}
	failErr := f.FailErr
	reply := f.Reply
	f.mu.Unlock()

	if hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if fail {
		if failErr == nil {
			failErr = errors.New("fake provider: scripted failure")
		}
		return "", failErr
	}
	return reply, nil
}

// Requests returns the operations this provider has received, in arrival
// order. It is the record tests grade against.
func (f *FakeProvider) Requests() []Operation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Operation(nil), f.requests...)
}
