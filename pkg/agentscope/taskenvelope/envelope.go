package taskenvelope

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a subtask record (DESIGN.md "States").
type State string

const (
	StateProposed      State = "Proposed"      // recorded; admission checks not finished
	StateAccepted      State = "Accepted"      // admission passed; allowance reserved; work not started
	StateInProgress    State = "InProgress"    // attempt reservation persisted; external call may be in flight
	StateCanceling     State = "Canceling"     // cancel requested or epoch advanced; stop not yet attested
	StateCompleted     State = "Completed"     // result recorded (terminal)
	StateFailed        State = "Failed"        // failure recorded; says nothing by itself about effect (terminal)
	StateCanceled      State = "Canceled"      // stop attested (terminal)
	StateExpired       State = "Expired"       // TTL elapsed before any attempt reservation existed (terminal)
	StateUnknownEffect State = "UnknownEffect" // outcome or effect uncertain; resolvable only with validated evidence
)

// Terminal reports whether the state is terminal: no exit, work never reopens
// (DESIGN.md "Forbidden transitions").
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateExpired:
		return true
	}
	return false
}

// Effect attests what physically happened, as reported by the executor.
// Outcome (Completed/Failed) and effect are different statements: a failure
// may be reported after a partial effect occurred. Note the scope difference:
// the value "unknown" describes un-attested effect on a record, while the
// UnknownEffect state is record-level uncertainty.
type Effect string

const (
	EffectNone    Effect = "none"
	EffectPartial Effect = "partial"
	EffectUnknown Effect = "unknown"
)

// Bounds limit the envelope's lifetime and attempts.
type Bounds struct {
	TTL         time.Duration `json:"ttl"` // zero = no expiry
	MaxAttempts int           `json:"max_attempts"`
}

// Allowance is the record's three-way usage ledger (DESIGN.md "Allowance
// accounting model"). Consumed is never refunded; uncertain usage stays
// Locked until validated usage evidence arrives; ReleasedUnused flows back to
// the root account as accounting, never as a limit reset.
type Allowance struct {
	Reserved       int `json:"reserved"`
	Consumed       int `json:"consumed"`
	ReleasedUnused int `json:"released_unused"`
	Locked         int `json:"locked"`
}

// AttemptReservation is persisted before any external call (commit boundary).
type AttemptReservation struct {
	ID         string    `json:"id"`
	ReservedAt time.Time `json:"reserved_at"`
}

// Record is a subtask envelope record (DESIGN.md "Record, identity, and
// attempt reservations").
type Record struct {
	ID          string `json:"id"` // root-allocated
	RootTaskID  string `json:"root_task_id"`
	ParentID    string `json:"parent_id"`
	TaskEpoch   int64  `json:"task_epoch"`
	Scope       string `json:"scope"`
	ContentHash string `json:"content_hash"`

	Bounds Bounds `json:"bounds"`

	State  State  `json:"state"`
	Result string `json:"result,omitempty"`
	Effect Effect `json:"effect"`

	Allowance Allowance            `json:"allowance"`
	Attempts  []AttemptReservation `json:"attempts,omitempty"`

	// Provenance logs every evidence-driven resolution.
	Provenance []string `json:"provenance,omitempty"`

	SubmittedAt time.Time `json:"submitted_at"`

	// Settled marks terminal settlement of the allowance ledger as recorded;
	// re-settlement is a no-op, so consumed never double-counts across restart
	// and repeated reconciliation.
	Settled bool `json:"settled,omitempty"`
}

// Deadline returns the record's dispatch/admission deadline; zero TTL means
// no deadline.
func (r *Record) Deadline() (time.Time, bool) {
	if r.Bounds.TTL <= 0 {
		return time.Time{}, false
	}
	return r.SubmittedAt.Add(r.Bounds.TTL), true
}

// Clone returns a deep copy safe for callers to inspect.
func (r *Record) Clone() *Record {
	out := *r
	out.Attempts = append([]AttemptReservation(nil), r.Attempts...)
	out.Provenance = append([]string(nil), r.Provenance...)
	return &out
}

// Sentinel errors. All are wrapped with context; use errors.Is to test.
var (
	// ErrInvalidTransition rejects a transition outside the DESIGN.md table
	// (including any exit from a terminal state and Proposed→Completed).
	ErrInvalidTransition = errors.New("taskenvelope: invalid state transition")
	// ErrIDConflict rejects a submission whose ID was already used with
	// different content.
	ErrIDConflict = errors.New("taskenvelope: id reused with different content")
	// ErrNotFound reports an unknown record ID.
	ErrNotFound = errors.New("taskenvelope: record not found")
	// ErrIntegrity marks an executor-integrity event: completion reported
	// after an attested stop (DESIGN.md boundary rule). It is logged and
	// returned, never recorded into the record.
	ErrIntegrity = errors.New("taskenvelope: executor integrity event")
	// ErrEvidence rejects an UnknownEffect resolution without validated
	// evidence (T12).
	ErrEvidence = errors.New("taskenvelope: resolution requires validated evidence")
)

// contentHash computes the dedupe hash of the submitted content.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// allowedTransitions mirrors the DESIGN.md transition table (T1–T12 + T2b).
// Every legal state change is listed here; anything else is forbidden.
var allowedTransitions = map[[2]State]string{
	{StateProposed, StateAccepted}:        "T1",
	{StateProposed, StateFailed}:          "T2",
	{StateProposed, StateCanceled}:        "T2b",
	{StateProposed, StateExpired}:         "T9",
	{StateAccepted, StateInProgress}:      "T3",
	{StateAccepted, StateCanceling}:       "T6",
	{StateAccepted, StateExpired}:         "T9",
	{StateInProgress, StateCompleted}:     "T4",
	{StateInProgress, StateFailed}:        "T5",
	{StateInProgress, StateCanceling}:     "T6",
	{StateInProgress, StateUnknownEffect}: "T10",
	{StateCanceling, StateCanceled}:       "T7",
	{StateCanceling, StateUnknownEffect}:  "T8",
	{StateCanceling, StateCompleted}:      "T11",
	{StateUnknownEffect, StateCompleted}:  "T12",
	{StateUnknownEffect, StateFailed}:     "T12",
	{StateUnknownEffect, StateCanceled}:   "T12",
}

// checkTransition enforces the transition table; the returned error names the
// table entry when the transition is legal.
func checkTransition(from, to State) (string, error) {
	entry, ok := allowedTransitions[[2]State{from, to}]
	if !ok {
		return "", fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}
	return entry, nil
}
