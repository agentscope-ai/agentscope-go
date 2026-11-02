package tierrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Tier identifies a deployment tier of a model endpoint.
type Tier string

const (
	TierDevice Tier = "device"
	TierEdge   Tier = "edge"
	TierCloud  Tier = "cloud"
)

// Operation is one routable inference unit (DESIGN.md "Model"). ID is
// caller-allocated and is the idempotency key; the dedupe hash covers Kind,
// LocalOnly and Content. A zero SubmittedAt is stamped at the decision point.
type Operation struct {
	ID          string
	TaskEpoch   int64
	Kind        string
	LocalOnly   bool
	Content     string
	TTL         time.Duration // zero = no expiry
	SubmittedAt time.Time
}

// contentHash computes the dedupe hash of the operation identity.
func (op *Operation) contentHash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%t\x00%s", op.Kind, op.LocalOnly, op.Content)))
	return hex.EncodeToString(sum[:])
}

// copyOperation isolates the router from the caller's struct.
func copyOperation(op *Operation) *Operation {
	if op == nil {
		return &Operation{}
	}
	cp := *op
	return &cp
}

// Deadline returns the operation's dispatch deadline; zero TTL means none.
func (op *Operation) Deadline() (time.Time, bool) {
	if op.TTL <= 0 || op.SubmittedAt.IsZero() {
		return time.Time{}, false
	}
	return op.SubmittedAt.Add(op.TTL), true
}

// AttemptOutcome attests what a single dispatch to a tier produced
// (DESIGN.md "Attempt outcomes").
type AttemptOutcome string

const (
	AttemptCompleted AttemptOutcome = "completed" // provider returned a result
	AttemptFailed    AttemptOutcome = "failed"    // clean failure; fallback allowed
	AttemptUnknown   AttemptOutcome = "unknown"   // timeout/cancel mid-call; fallback forbidden
)

// OpState is the lifecycle state of a decision record (DESIGN.md "Operation
// states").
type OpState string

const (
	StatePending    OpState = "pending"    // admitted; an attempt may be in flight
	StateCompleted  OpState = "completed"  // an attempt completed (terminal)
	StateRejected   OpState = "rejected"   // never dispatched (terminal)
	StateExhausted  OpState = "exhausted"  // every candidate tier failed cleanly (terminal)
	StateSuperseded OpState = "superseded" // admitted, then the guards ceased to hold before a later dispatch (terminal)
	StateUncertain  OpState = "uncertain"  // an attempt outcome is unknown
)

// Terminal reports whether the state is terminal.
func (s OpState) Terminal() bool {
	switch s {
	case StateCompleted, StateRejected, StateExhausted, StateSuperseded:
		return true
	}
	return false
}

// Rejection codes recorded on a rejected record; they map back to the
// sentinel errors on replay (DESIGN.md interpretation b).
const (
	rejectStaleEpoch = "stale-epoch"
	rejectExpired    = "expired"
	rejectNoRoute    = "no-route"
)

// Attempt records one dispatch decision and its outcome.
type Attempt struct {
	Seq        int            `json:"seq"`
	Tier       Tier           `json:"tier"`
	Reason     string         `json:"reason"`
	Outcome    AttemptOutcome `json:"outcome"`
	Result     string         `json:"result,omitempty"`
	Err        string         `json:"error,omitempty"`
	RecordedAt time.Time      `json:"recorded_at"`
	Settled    bool           `json:"settled"`
}

// Record is a routing decision record (DESIGN.md "Model").
type Record struct {
	ID          string  `json:"id"`
	ContentHash string  `json:"content_hash"`
	TaskEpoch   int64   `json:"task_epoch"`
	State       OpState `json:"state"`
	RejectCode  string  `json:"reject_code,omitempty"`
	// StopReason records why a superseded record stopped ("stale-epoch" or
	// "expired"); prior attempts remain as evidence.
	StopReason  string    `json:"stop_reason,omitempty"`
	Attempts    []Attempt `json:"attempts,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Clone returns a deep copy safe for callers to inspect.
func (r *Record) Clone() *Record {
	out := *r
	out.Attempts = append([]Attempt(nil), r.Attempts...)
	return &out
}

// Result returns the completed attempt's result, if any.
func (r *Record) Result() (string, bool) {
	for _, a := range r.Attempts {
		if a.Outcome == AttemptCompleted {
			return a.Result, true
		}
	}
	return "", false
}

// Policy answers, for an operation, which tiers are allowed and in what
// order of preference (DESIGN.md "Model").
type Policy interface {
	Prefer(op *Operation) []Tier
}

// StaticPolicy is a table-driven Policy for the harness: Rules maps an
// operation kind to its ordered tiers; Default applies to unlisted kinds.
type StaticPolicy struct {
	Rules   map[string][]Tier
	Default []Tier
}

// Prefer implements Policy. The returned slice is a copy.
func (p StaticPolicy) Prefer(op *Operation) []Tier {
	if tiers, ok := p.Rules[op.Kind]; ok {
		return append([]Tier(nil), tiers...)
	}
	return append([]Tier(nil), p.Default...)
}

// Provider is a model endpoint on one tier. FakeProvider (fakes.go) is the
// deterministic reference implementation used by the fault tests.
type Provider interface {
	Tier() Tier
	Complete(ctx context.Context, op *Operation) (string, error)
}

// Sentinel errors. All are wrapped with context; use errors.Is to test.
var (
	// ErrIDConflict rejects an operation whose ID was already used with
	// different content (R3).
	ErrIDConflict = errors.New("tierrouter: id reused with different content")
	// ErrStaleEpoch rejects an operation whose task_epoch is not current at
	// the decision point (R2).
	ErrStaleEpoch = errors.New("tierrouter: operation task_epoch is stale")
	// ErrExpired rejects an operation whose TTL elapsed before dispatch (R2).
	ErrExpired = errors.New("tierrouter: operation TTL elapsed before dispatch")
	// ErrNoRoute rejects an operation the policy allows no tier for (after
	// the local-only guard).
	ErrNoRoute = errors.New("tierrouter: policy allows no tier for operation")
	// ErrInFlight reports that an identical operation is being decided by
	// another call right now; replay once it settles (R3).
	ErrInFlight = errors.New("tierrouter: identical operation already in flight")
	// ErrUncertain reports an attempt with unknown outcome; the operation is
	// never retried automatically (R5).
	ErrUncertain = errors.New("tierrouter: attempt outcome unknown; not retried")
	// ErrExhausted reports that every candidate tier failed cleanly (R4).
	ErrExhausted = errors.New("tierrouter: all candidate tiers failed")
)

// Router routes operations to tier providers under a policy, persisting
// every decision in the log. The zero Router is not usable; use NewRouter.
type Router struct {
	log       *Log
	policy    Policy
	epoch     func() int64
	now       func() time.Time // replaceable in tests
	providers map[Tier]Provider
}

// NewRouter wires a router. epoch supplies the current task epoch; each
// provider must claim a distinct tier.
func NewRouter(log *Log, policy Policy, epoch func() int64, providers ...Provider) (*Router, error) {
	if log == nil {
		return nil, errors.New("tierrouter: nil decision log")
	}
	if policy == nil {
		return nil, errors.New("tierrouter: nil policy")
	}
	if epoch == nil {
		return nil, errors.New("tierrouter: nil epoch source")
	}
	r := &Router{
		log:       log,
		policy:    policy,
		epoch:     epoch,
		now:       time.Now,
		providers: make(map[Tier]Provider, len(providers)),
	}
	for _, p := range providers {
		if p == nil {
			return nil, errors.New("tierrouter: nil provider")
		}
		t := p.Tier()
		if _, dup := r.providers[t]; dup {
			return nil, fmt.Errorf("tierrouter: duplicate provider for tier %s", t)
		}
		r.providers[t] = p
	}
	return r, nil
}

// Route decides and dispatches an operation per the policy, recording every
// step in the decision log. It returns the decision record (a clone) and a
// nil error only when an attempt completed; otherwise the record carries the
// evidence and the error classifies the outcome.
func (r *Router) Route(ctx context.Context, op *Operation) (*Record, error) {
	if op == nil || op.ID == "" {
		return nil, errors.New("tierrouter: operation ID required")
	}
	op = copyOperation(op)
	hash := op.contentHash()
	if op.SubmittedAt.IsZero() {
		op.SubmittedAt = r.now()
	}

	rec, err := r.admit(op, hash)
	if err != nil || rec.State != StatePending {
		return rec, err
	}

	candidates := r.candidatesFor(op)
	for i, tier := range candidates {
		p, attached := r.providers[tier]
		if !attached {
			if cerr := r.recordMissedTier(rec, op, tier, i); cerr != nil {
				return rec.Clone(), cerr
			}
			continue
		}
		reason := "policy-preferred"
		if i > 0 {
			reason = "fallback-after-failure:" + string(candidates[i-1])
		}
		if cerr := r.commitAttempt(rec, op, tier, reason); cerr != nil {
			return rec.Clone(), cerr
		}

		result, callErr := p.Complete(ctx, op)

		settled := r.settleAttempt(rec, op, tier, result, callErr)
		if settled != nil {
			return rec.Clone(), settled
		}
		switch rec.State {
		case StateCompleted:
			return rec.Clone(), nil
		case StateUncertain:
			return rec.Clone(), fmt.Errorf("%w: tier %s: %s", ErrUncertain, tier, callErr)
		}
		// Clean failure: fall through to the next candidate (R4).
	}

	r.log.mu.Lock()
	rec.State = StateExhausted
	perr := r.log.persistLocked()
	out := rec.Clone()
	r.log.mu.Unlock()
	if perr != nil {
		return out, fmt.Errorf("tierrouter: persist exhaustion: %w", perr)
	}
	return out, fmt.Errorf("%w: %s", ErrExhausted, op.ID)
}

// guardLocked checks the dispatch guards (R2): the operation's task epoch
// must equal the current epoch and its TTL must not have elapsed. The caller
// must hold log.mu. admit checks it for new operations and commitAttempt
// rechecks it before every dispatch, so an epoch advance or TTL expiry
// between admission and a later attempt blocks the dispatch.
func (r *Router) guardLocked(op *Operation) (string, error) {
	if current := r.epoch(); op.TaskEpoch != current {
		return rejectStaleEpoch, fmt.Errorf("%w: %s (op epoch %d, current %d)",
			ErrStaleEpoch, op.ID, op.TaskEpoch, current)
	}
	if deadline, ok := op.Deadline(); ok && !r.now().Before(deadline) {
		return rejectExpired, fmt.Errorf("%w: %s", ErrExpired, op.ID)
	}
	return "", nil
}

// admit replays an existing record or admits a new one under the serialized
// decision point (R2, R3). On admission the record is pending with no
// attempts; the caller then runs the attempt loop.
func (r *Router) admit(op *Operation, hash string) (*Record, error) {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()

	if rec, ok := r.log.records[op.ID]; ok {
		out := rec.Clone()
		if rec.ContentHash != hash {
			return nil, fmt.Errorf("%w: %s", ErrIDConflict, op.ID)
		}
		return out, replayError(rec)
	}

	reject := func(code string, err error) (*Record, error) {
		rec := &Record{
			ID:          op.ID,
			ContentHash: hash,
			TaskEpoch:   op.TaskEpoch,
			State:       StateRejected,
			RejectCode:  code,
			SubmittedAt: op.SubmittedAt,
		}
		r.log.records[op.ID] = rec
		if perr := r.log.persistLocked(); perr != nil {
			delete(r.log.records, op.ID)
			return nil, fmt.Errorf("tierrouter: persist rejection: %w", perr)
		}
		return rec.Clone(), err
	}

	if code, err := r.guardLocked(op); err != nil {
		return reject(code, err)
	}
	if len(r.candidatesFor(op)) == 0 {
		return reject(rejectNoRoute, fmt.Errorf("%w: %s (kind %q)", ErrNoRoute, op.ID, op.Kind))
	}

	rec := &Record{
		ID:          op.ID,
		ContentHash: hash,
		TaskEpoch:   op.TaskEpoch,
		State:       StatePending,
		SubmittedAt: op.SubmittedAt,
	}
	r.log.records[op.ID] = rec
	if err := r.log.persistLocked(); err != nil {
		delete(r.log.records, op.ID)
		return nil, fmt.Errorf("tierrouter: commit boundary: %w", err)
	}
	return rec, nil
}

// candidatesFor applies the policy and then the local-only guard (R4). It is
// pure; the decision point in admit is what serializes it.
func (r *Router) candidatesFor(op *Operation) []Tier {
	tiers := r.policy.Prefer(op)
	if !op.LocalOnly {
		return tiers
	}
	out := tiers[:0:0]
	for _, t := range tiers {
		if t == TierDevice {
			out = append(out, t)
		}
	}
	return out
}

// recordMissedTier records a clean failed attempt for a policy tier with no
// attached provider (DESIGN.md interpretation d).
func (r *Router) recordMissedTier(rec *Record, op *Operation, tier Tier, idx int) error {
	reason := "policy-preferred"
	if idx > 0 {
		reason = "fallback-after-failure:" + string(rec.Attempts[len(rec.Attempts)-1].Tier)
	}
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	if code, err := r.guardLocked(op); err != nil {
		return r.supersedeLocked(rec, code, err)
	}
	rec.Attempts = append(rec.Attempts, Attempt{
		Seq:        len(rec.Attempts) + 1,
		Tier:       tier,
		Reason:     reason,
		Outcome:    AttemptFailed,
		Err:        "provider not attached",
		RecordedAt: r.now(),
		Settled:    true,
	})
	if err := r.log.persistLocked(); err != nil {
		rec.Attempts = rec.Attempts[:len(rec.Attempts)-1]
		return fmt.Errorf("tierrouter: persist missed tier: %w", err)
	}
	return nil
}

// supersedeLocked stops the record because the guards ceased to hold after
// admission; prior attempts remain as evidence and no further dispatch
// happens. The caller must hold log.mu. There is no rollback: the guard
// outcome is re-derivable, so memory and disk cannot diverge in substance
// (DESIGN.md interpretation f).
func (r *Router) supersedeLocked(rec *Record, code string, guardErr error) error {
	rec.State = StateSuperseded
	rec.StopReason = code
	if perr := r.log.persistLocked(); perr != nil {
		return fmt.Errorf("tierrouter: persist supersession: %w", perr)
	}
	return guardErr
}

// commitAttempt persists the attempt record before the provider call
// (commit boundary, R1). It rechecks the dispatch guards (R2): if the epoch
// advanced or the TTL elapsed since admission, the record is superseded —
// prior attempts remain as evidence and no further dispatch happens.
func (r *Router) commitAttempt(rec *Record, op *Operation, tier Tier, reason string) error {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	if code, err := r.guardLocked(op); err != nil {
		return r.supersedeLocked(rec, code, err)
	}
	rec.Attempts = append(rec.Attempts, Attempt{
		Seq:        len(rec.Attempts) + 1,
		Tier:       tier,
		Reason:     reason,
		RecordedAt: r.now(),
	})
	if err := r.log.persistLocked(); err != nil {
		rec.Attempts = rec.Attempts[:len(rec.Attempts)-1]
		return fmt.Errorf("tierrouter: commit boundary: %w", err)
	}
	return nil
}

// settleAttempt records the provider call outcome and settles the attempt
// (R4, R5). A non-nil returned error is a persistence failure or a guard
// outcome, never a clean-failure routing signal. On success it rechecks the
// dispatch guards (R2): if the epoch advanced or the TTL elapsed while the
// call was in flight, the attempt stays as completed evidence and the
// operation settles superseded, so a stale success is never returned as a
// routing result — the same alignment ChatRouter applies. Unlike
// admit/commitAttempt there is no rollback here: the provider call already
// happened, so the in-memory outcome is the most truthful evidence available
// and must survive to block silent re-dispatch.
func (r *Router) settleAttempt(rec *Record, op *Operation, tier Tier, result string, callErr error) error {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	at := &rec.Attempts[len(rec.Attempts)-1]
	at.Settled = true
	switch {
	case callErr == nil:
		at.Outcome = AttemptCompleted
		at.Result = result
		if code, err := r.guardLocked(op); err != nil {
			return r.supersedeLocked(rec, code, err)
		}
		rec.State = StateCompleted
	case unknownOutcome(callErr):
		at.Outcome = AttemptUnknown
		at.Err = callErr.Error()
		rec.State = StateUncertain
	default:
		at.Outcome = AttemptFailed
		at.Err = callErr.Error()
	}
	if err := r.log.persistLocked(); err != nil {
		return fmt.Errorf("tierrouter: persist outcome: %w", err)
	}
	return nil
}

// unknownOutcome classifies a provider error whose effect is uncertain:
// cancellation or deadline mean the request may have been processed (R5 and
// DESIGN.md interpretation a).
func unknownOutcome(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// replayError maps a recorded state back to the error a duplicate Route
// observes; the record itself is the outcome evidence (DESIGN.md
// interpretation b).
func replayError(rec *Record) error {
	switch rec.State {
	case StateCompleted:
		return nil
	case StateRejected:
		switch rec.RejectCode {
		case rejectStaleEpoch:
			return fmt.Errorf("%w: %s", ErrStaleEpoch, rec.ID)
		case rejectExpired:
			return fmt.Errorf("%w: %s", ErrExpired, rec.ID)
		default:
			return fmt.Errorf("%w: %s", ErrNoRoute, rec.ID)
		}
	case StatePending:
		return fmt.Errorf("%w: %s", ErrInFlight, rec.ID)
	case StateExhausted:
		return fmt.Errorf("%w: %s", ErrExhausted, rec.ID)
	case StateSuperseded:
		if rec.StopReason == rejectExpired {
			return fmt.Errorf("%w: %s", ErrExpired, rec.ID)
		}
		return fmt.Errorf("%w: %s", ErrStaleEpoch, rec.ID)
	default:
		return fmt.Errorf("%w: %s", ErrUncertain, rec.ID)
	}
}
