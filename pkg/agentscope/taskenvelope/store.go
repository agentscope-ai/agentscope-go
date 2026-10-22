package taskenvelope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
)

// Submission carries a new envelope. Content is passed to the executor at
// dispatch and hashed into the record for dedupe; it is not persisted.
type Submission struct {
	ID             string // root-allocated
	RootTaskID     string
	ParentID       string
	Scope          string
	Content        string
	Epoch          int64
	TTL            time.Duration // zero = no expiry
	MaxAttempts    int           // dispatch attempts permitted; 0 = never dispatchable. Values above 1 are currently unreachable: a record dispatches once and never returns to Accepted (DESIGN.md "Implementation interpretations" e)
	AllowanceUnits int           // units reserved at admission
}

// Attempt is handed to the executor at dispatch.
type Attempt struct {
	ID       string // persisted reservation ID
	RecordID string
	Scope    string
	Content  string
	Epoch    int64
}

// Outcome is the executor's attested result of an attempt. A nil Usage means
// usage is unknown; outcome evidence is not usage evidence.
type Outcome struct {
	Success bool // true = completed, false = failed (attested failure)
	Result  string
	Usage   *int   // usage evidence when present
	Effect  Effect // attested effect; a reported failure does not imply none
}

// StopAttestation is the executor's answer to a stop request. A cancel-ACK is
// never proof by itself: Attested must mean the stop actually took effect.
type StopAttestation struct {
	Attested bool
	Effect   Effect // required when Attested
	Usage    *int   // usage evidence when present
}

// Executor is the device-side action authority the store dispatches through.
type Executor interface {
	// Do runs the attempt. A non-nil error means the outcome is unknown
	// (result/ACK lost), not that the attempt failed.
	Do(ctx context.Context, a Attempt) (Outcome, error)
	// QueryAttempt returns the recorded outcome for an attempt ID, and
	// whether the executor can account for it at all. No account does not
	// imply the call never happened.
	QueryAttempt(ctx context.Context, attemptID string) (Outcome, bool, error)
	// AttestStop answers whether the attempt actually stopped, with effect.
	AttestStop(ctx context.Context, attemptID string) (StopAttestation, error)
}

// Evidence is validated outcome evidence for resolving UnknownEffect (T12).
type Evidence struct {
	Outcome    State // target: Completed, Failed, or Canceled
	Result     string
	Usage      *int // validated usage accounting of the locked remainder; nil = usage still unknown
	Effect     Effect
	Provenance string // required: where the evidence came from
	Validated  bool   // required: an authenticated executor query or operator resolution
}

// RootAccount is the per-root allowance pool. Child records never mutate its
// Total: usage flows up as accounting (Consumed grows, released-unused units
// return to Available), never as a limit reset.
type RootAccount struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Consumed  int `json:"consumed"`
}

// Options configure a Store.
type Options struct {
	// Authority decides whether a scope may be dispatched; checked at
	// admission (T1) and again at the dispatch decision point (T3). Nil
	// allows everything (testbed default).
	Authority func(scope string) bool
	// Now supplies the clock; nil uses time.Now. Tests inject a fixed clock.
	Now func() time.Time
}

// Store is the single-writer, single-snapshot envelope ledger. Every
// transition serializes through one mutex-guarded decision point; the file
// layer (fsutil.WriteFileAtomic) only provides atomic snapshot replacement —
// it offers no transactions or compare-and-swap (DESIGN.md "Commit boundary").
type Store struct {
	mu   sync.Mutex
	path string
	auth func(string) bool
	now  func() time.Time

	records       map[string]*Record
	roots         map[string]*RootAccount
	epochs        map[string]int64
	canceledRoots map[string]bool

	// Crash-injection hooks for the fault tests (same package only): invoked
	// after the attempt reservation is persisted and before the external call,
	// and after the call returns but before the outcome is recorded. A hook
	// that panics simulates a process crash in that window.
	beforeExternalCall  func()
	beforeOutcomeRecord func()
}

type snapshot struct {
	Records       map[string]*Record      `json:"records"`
	Roots         map[string]*RootAccount `json:"roots"`
	Epochs        map[string]int64        `json:"epochs"`
	CanceledRoots map[string]bool         `json:"canceled_roots"`
}

// Open loads the ledger at path (missing file = empty ledger). Restart
// recovery of in-flight work is Reconcile; Open itself only reads disk
// (reconciliation rule 1).
func Open(path string, opts *Options) (*Store, error) {
	s := &Store{
		path:          path,
		records:       make(map[string]*Record),
		roots:         make(map[string]*RootAccount),
		epochs:        make(map[string]int64),
		canceledRoots: make(map[string]bool),
	}
	if opts != nil {
		s.auth = opts.Authority
		s.now = opts.Now
	}
	if s.now == nil {
		s.now = time.Now
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("taskenvelope: read ledger: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("taskenvelope: decode ledger: %w", err)
	}
	for k, v := range snap.Records {
		s.records[k] = v
	}
	for k, v := range snap.Roots {
		s.roots[k] = v
	}
	for k, v := range snap.Epochs {
		s.epochs[k] = v
	}
	for k, v := range snap.CanceledRoots {
		s.canceledRoots[k] = v
	}
	return s, nil
}

// persistLocked writes the whole ledger atomically. Caller must hold mu.
func (s *Store) persistLocked() error {
	data, err := json.Marshal(snapshot{
		Records:       s.records,
		Roots:         s.roots,
		Epochs:        s.epochs,
		CanceledRoots: s.canceledRoots,
	})
	if err != nil {
		return fmt.Errorf("taskenvelope: encode ledger: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.path, data, 0o644); err != nil {
		return fmt.Errorf("taskenvelope: write ledger: %w", err)
	}
	return nil
}

// setStateLocked validates the transition against the DESIGN.md table and
// records provenance. Caller holds mu.
func (s *Store) setStateLocked(rec *Record, to State, provenance string) error {
	entry, err := checkTransition(rec.State, to)
	if err != nil {
		return err
	}
	rec.State = to
	if provenance != "" {
		rec.Provenance = append(rec.Provenance, entry+": "+provenance)
	}
	return nil
}

// settleTerminalLocked performs terminal allowance settlement with known usage
// u (u == 0 covers the known-zero settlements of T2/T2b/T9). Consumed is never
// refunded; the remainder is released back to the root as accounting.
// Settlement is recorded and idempotent: re-reconciliation is a no-op.
// Unknown usage never reaches this function — it locks instead.
// Caller holds mu.
func (s *Store) settleTerminalLocked(rec *Record, u int, provenance string) {
	if rec.Settled {
		return
	}
	rec.Settled = true
	rec.Allowance.Consumed += u
	released := rec.Allowance.Reserved - rec.Allowance.Consumed - rec.Allowance.Locked
	if released > 0 {
		rec.Allowance.ReleasedUnused += released
	}
	rec.Provenance = append(rec.Provenance, provenance)
	if root, ok := s.roots[rec.RootTaskID]; ok {
		root.Consumed += u
		root.Available += released
	}
}

// lockRemainderLocked moves the unaccounted remainder into locked when a
// record enters UnknownEffect. Caller holds mu.
func (s *Store) lockRemainderLocked(rec *Record) {
	remainder := rec.Allowance.Reserved - rec.Allowance.Consumed - rec.Allowance.Locked
	if remainder > 0 {
		rec.Allowance.Locked += remainder
	}
}

// RegisterRoot declares a root task's allowance pool.
func (s *Store) RegisterRoot(rootTaskID string, allowanceUnits int) error {
	if allowanceUnits < 0 {
		return fmt.Errorf("taskenvelope: allowance must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.roots[rootTaskID]; ok {
		return fmt.Errorf("taskenvelope: root %q already registered", rootTaskID)
	}
	s.roots[rootTaskID] = &RootAccount{Total: allowanceUnits, Available: allowanceUnits}
	return s.persistLocked()
}

// Root returns a copy of the root's account.
func (s *Store) Root(rootTaskID string) (RootAccount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.roots[rootTaskID]
	if !ok {
		return RootAccount{}, false
	}
	return *r, true
}

// Epoch returns the root's current epoch. The root revalidates it separately
// before using any recorded result (T4's guard): a record never authorizes
// work by itself.
func (s *Store) Epoch(rootTaskID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epochs[rootTaskID]
}

// Get returns a copy of a record.
func (s *Store) Get(id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return nil, false
	}
	return rec.Clone(), true
}

// Submit records an envelope and runs admission (T1/T2/T2b). Identical
// retries (same ID and content) replay the recorded status — including a
// recorded rejection — without re-running admission; reusing the ID with
// different content is a conflict. Terminal records never reopen.
func (s *Store) Submit(sub *Submission) (*Record, error) {
	if sub.ID == "" || sub.RootTaskID == "" {
		return nil, fmt.Errorf("taskenvelope: ID and RootTaskID are required")
	}
	if sub.AllowanceUnits < 0 {
		return nil, fmt.Errorf("taskenvelope: AllowanceUnits must not be negative")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.records[sub.ID]; ok {
		if rec.ContentHash != contentHash(sub.Content) {
			return nil, fmt.Errorf("%w: %s", ErrIDConflict, sub.ID)
		}
		return rec.Clone(), nil
	}

	rec := &Record{
		ID:          sub.ID,
		RootTaskID:  sub.RootTaskID,
		ParentID:    sub.ParentID,
		TaskEpoch:   sub.Epoch,
		Scope:       sub.Scope,
		ContentHash: contentHash(sub.Content),
		Bounds:      Bounds{TTL: sub.TTL, MaxAttempts: sub.MaxAttempts},
		State:       StateProposed,
		Effect:      EffectUnknown,
		SubmittedAt: s.now(),
	}
	s.records[rec.ID] = rec
	// The submission record is persisted before any decision (commit boundary).
	if err := s.persistLocked(); err != nil {
		return nil, err
	}

	// T2b: the root was canceled while the record was still Proposed. Nothing
	// dispatched, stop trivially attested, usage known-zero.
	if s.canceledRoots[rec.RootTaskID] {
		rec.Effect = EffectNone
		if err := s.setStateLocked(rec, StateCanceled, "cancel while Proposed"); err != nil {
			return nil, err
		}
		s.settleTerminalLocked(rec, 0, "known-zero settlement")
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		return rec.Clone(), nil
	}

	// T1/T2: admission guards; a failure is recorded as a rejection so an
	// identical retry replays it (usage known-zero).
	if err := s.admissionGuardsLocked(rec, sub.AllowanceUnits); err != nil {
		rec.Result = "rejected: " + err.Error()
		rec.Effect = EffectNone
		if serr := s.setStateLocked(rec, StateFailed, "admission rejected"); serr != nil {
			return nil, serr
		}
		s.settleTerminalLocked(rec, 0, "known-zero settlement")
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		return rec.Clone(), nil
	}

	root := s.roots[rec.RootTaskID]
	root.Available -= sub.AllowanceUnits
	rec.Allowance.Reserved = sub.AllowanceUnits
	if err := s.setStateLocked(rec, StateAccepted, "admission passed"); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return rec.Clone(), nil
}

// admissionGuardsLocked implements the T1 guard set. Caller holds mu.
func (s *Store) admissionGuardsLocked(rec *Record, units int) error {
	if s.auth != nil && !s.auth(rec.Scope) {
		return fmt.Errorf("scope %q outside authority", rec.Scope)
	}
	if deadline, ok := rec.Deadline(); ok && !s.now().Before(deadline) {
		return fmt.Errorf("TTL elapsed")
	}
	root, ok := s.roots[rec.RootTaskID]
	if !ok {
		return fmt.Errorf("unknown root %q", rec.RootTaskID)
	}
	if root.Available < units {
		return fmt.Errorf("allowance not available")
	}
	if s.epochs[rec.RootTaskID] != rec.TaskEpoch {
		return fmt.Errorf("stale epoch %d (current %d)", rec.TaskEpoch, s.epochs[rec.RootTaskID])
	}
	if s.canceledRoots[rec.RootTaskID] {
		return fmt.Errorf("root canceled")
	}
	return nil
}

// Dispatch runs one attempt (T3): the decision point rechecks epoch,
// cancellation, authority, deadline and allowance at dispatch time (not
// relying on T1), the attempt reservation is persisted before the external
// call, and the outcome is persisted after it. content must match the
// submitted envelope. A Do error means the outcome is unknown: the record
// goes to UnknownEffect (T10) and is never re-dispatched.
func (s *Store) Dispatch(ctx context.Context, id string, content string, exec Executor) error {
	s.mu.Lock()
	rec, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.ContentHash != contentHash(content) {
		s.mu.Unlock()
		return fmt.Errorf("taskenvelope: dispatch content does not match submission %s", id)
	}
	if rec.State != StateAccepted {
		s.mu.Unlock()
		return fmt.Errorf("%w: dispatch requires Accepted, record %s is %s", ErrInvalidTransition, id, rec.State)
	}
	// T3 guard recheck at the serialized decision point.
	if err := s.dispatchGuardsLocked(rec); err != nil {
		s.mu.Unlock()
		return err
	}
	attemptID := fmt.Sprintf("%s#%d", rec.ID, len(rec.Attempts)+1)
	rec.Attempts = append(rec.Attempts, AttemptReservation{ID: attemptID, ReservedAt: s.now()})
	if err := s.setStateLocked(rec, StateInProgress, "attempt "+attemptID+" reserved"); err != nil {
		s.mu.Unlock()
		return err
	}
	// The reservation is persisted before the external call (commit boundary).
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	attempt := Attempt{ID: attemptID, RecordID: rec.ID, Scope: rec.Scope, Content: content, Epoch: rec.TaskEpoch}
	s.mu.Unlock()

	if s.beforeExternalCall != nil {
		s.beforeExternalCall()
	}
	outcome, err := exec.Do(ctx, attempt)
	if s.beforeOutcomeRecord != nil {
		s.beforeOutcomeRecord()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		// Result/ACK lost: outcome unknown (T10). Never re-dispatch.
		if serr := s.setStateLocked(rec, StateUnknownEffect, "result/ACK lost"); serr != nil {
			return serr
		}
		s.lockRemainderLocked(rec)
		if perr := s.persistLocked(); perr != nil {
			return perr
		}
		return fmt.Errorf("taskenvelope: dispatch outcome unknown for %s: %w", id, err)
	}
	if err := s.applyOutcomeLocked(rec, outcome, "dispatch "+attemptID); err != nil {
		return err
	}
	return s.persistLocked()
}

// dispatchGuardsLocked is the T3 guard set. Caller holds mu.
func (s *Store) dispatchGuardsLocked(rec *Record) error {
	if s.epochs[rec.RootTaskID] != rec.TaskEpoch {
		return fmt.Errorf("taskenvelope: stale epoch %d (current %d)", rec.TaskEpoch, s.epochs[rec.RootTaskID])
	}
	if s.canceledRoots[rec.RootTaskID] {
		return fmt.Errorf("taskenvelope: root canceled")
	}
	if s.auth != nil && !s.auth(rec.Scope) {
		return fmt.Errorf("taskenvelope: scope %q outside authority", rec.Scope)
	}
	if deadline, ok := rec.Deadline(); ok && !s.now().Before(deadline) {
		return fmt.Errorf("taskenvelope: deadline passed")
	}
	if len(rec.Attempts) >= rec.Bounds.MaxAttempts {
		return fmt.Errorf("taskenvelope: max attempts (%d) reached", rec.Bounds.MaxAttempts)
	}
	if rec.Allowance.Reserved == 0 {
		return fmt.Errorf("taskenvelope: no allowance reserved")
	}
	return nil
}

// applyOutcomeLocked applies attested outcome evidence (T4/T5; T11 when a
// cancel is being processed). Caller holds mu.
func (s *Store) applyOutcomeLocked(rec *Record, outcome Outcome, provenance string) error {
	switch rec.State {
	case StateInProgress:
		rec.Result = outcome.Result
		rec.Effect = outcome.Effect
		if outcome.Success {
			if err := s.setStateLocked(rec, StateCompleted, provenance); err != nil {
				return err
			}
		} else {
			if err := s.setStateLocked(rec, StateFailed, provenance); err != nil {
				return err
			}
		}
		s.settleOutcomeUsageLocked(rec, outcome.Usage, provenance+": usage settlement")
		return nil
	case StateCanceling:
		// T11: completion evidence is recordable whenever it occurred before
		// the stop took effect. Provenance is recorded; the result never
		// authorizes further work on the canceled root by itself.
		if outcome.Success {
			rec.Result = outcome.Result
			rec.Effect = outcome.Effect
			if err := s.setStateLocked(rec, StateCompleted, provenance); err != nil {
				return err
			}
			s.settleOutcomeUsageLocked(rec, outcome.Usage, provenance+": usage settlement")
			return nil
		}
		// A failure reported while canceling means the attempt ended: it is
		// stopped. Settle as an attested stop with the reported effect (T7).
		rec.Result = outcome.Result
		rec.Effect = outcome.Effect
		if err := s.setStateLocked(rec, StateCanceled, provenance+": failed attempt ended while canceling"); err != nil {
			return err
		}
		s.settleOutcomeUsageLocked(rec, outcome.Usage, provenance+": usage settlement")
		return nil
	case StateCanceled:
		// Boundary rule: a completion reported after an attested stop
		// contradicts the attestation — logged, never recorded.
		logrus.WithField("record", rec.ID).Error("taskenvelope: executor reported completion after attested stop")
		return fmt.Errorf("%w: record %s already Canceled", ErrIntegrity, rec.ID)
	default:
		return fmt.Errorf("%w: outcome evidence for %s in state %s", ErrInvalidTransition, rec.ID, rec.State)
	}
}

// settleOutcomeUsageLocked settles terminal usage: known evidence (non-nil)
// settles consumed; unknown usage locks the remainder — a Completed record may
// still have unknown usage. Caller holds mu.
func (s *Store) settleOutcomeUsageLocked(rec *Record, usage *int, provenance string) {
	if usage != nil {
		s.settleTerminalLocked(rec, *usage, provenance)
		return
	}
	s.lockRemainderLocked(rec)
	rec.Settled = true // outcome settled; usage may arrive later via RecordUsageEvidence
	rec.Provenance = append(rec.Provenance, provenance+": usage unknown, locked")
}

// RecordOutcome ingests push-style outcome evidence for a record (the T11 and
// boundary paths; the pull path is Dispatch/Reconcile). UnknownEffect records
// must go through Resolve (T12 requires validated evidence), not this path.
func (s *Store) RecordOutcome(id string, outcome Outcome, provenance string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	switch rec.State {
	case StateInProgress, StateCanceling, StateCanceled:
	default:
		return fmt.Errorf("%w: outcome evidence for %s in state %s", ErrInvalidTransition, id, rec.State)
	}
	if err := s.applyOutcomeLocked(rec, outcome, provenance); err != nil {
		return err
	}
	return s.persistLocked()
}

// Cancel requests a stop (T2b / T6). Proposed and Accepted records resolve
// immediately (nothing dispatched: stop trivially attested, usage known-zero);
// InProgress moves to Canceling and resolves via ProcessCancel — a cancel-ACK
// is never proof of stop by itself (T8).
func (s *Store) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	switch rec.State {
	case StateProposed:
		rec.Effect = EffectNone
		if err := s.setStateLocked(rec, StateCanceled, "cancel requested"); err != nil {
			return err
		}
		s.settleTerminalLocked(rec, 0, "known-zero settlement")
		return s.persistLocked()
	case StateAccepted:
		// T6 then T7 composed at the decision point: nothing was dispatched.
		if err := s.setStateLocked(rec, StateCanceling, "cancel requested"); err != nil {
			return err
		}
		rec.Effect = EffectNone
		if err := s.setStateLocked(rec, StateCanceled, "nothing dispatched"); err != nil {
			return err
		}
		s.settleTerminalLocked(rec, 0, "known-zero settlement")
		return s.persistLocked()
	case StateInProgress:
		if err := s.setStateLocked(rec, StateCanceling, "cancel requested"); err != nil {
			return err
		}
		return s.persistLocked()
	default:
		// Terminal, Canceling and UnknownEffect: cancel is a no-op.
		return nil
	}
}

// ProcessCancel resolves a Canceling record through the executor's stop
// attestation: attested stop with effect → Canceled (T7); no attestation →
// UnknownEffect (T8).
func (s *Store) ProcessCancel(ctx context.Context, id string, exec Executor) error {
	s.mu.Lock()
	rec, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.State != StateCanceling {
		s.mu.Unlock()
		return nil
	}
	if len(rec.Attempts) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("taskenvelope: %s is Canceling without a dispatched attempt", id)
	}
	attemptID := rec.Attempts[len(rec.Attempts)-1].ID
	s.mu.Unlock()

	att, err := exec.AttestStop(ctx, attemptID)

	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.State != StateCanceling {
		return nil // resolved meanwhile at the decision point
	}
	if err != nil || !att.Attested {
		if serr := s.setStateLocked(rec, StateUnknownEffect, "no stop attestation"); serr != nil {
			return serr
		}
		s.lockRemainderLocked(rec)
		return s.persistLocked()
	}
	rec.Effect = att.Effect
	if err := s.setStateLocked(rec, StateCanceled, "stop attested"); err != nil {
		return err
	}
	s.settleOutcomeUsageLocked(rec, att.Usage, "stop attestation usage settlement")
	return s.persistLocked()
}

// CancelRoot cancels every non-terminal record of a root and rejects later
// submissions for it (T2b/T6).
func (s *Store) CancelRoot(rootTaskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceledRoots[rootTaskID] = true
	for _, rec := range s.records {
		if rec.RootTaskID != rootTaskID {
			continue
		}
		if err := s.cancelRecordLocked(rec, "root canceled"); err != nil {
			return err
		}
	}
	return s.persistLocked()
}

// AdvanceEpoch bumps the root's epoch; records of older epochs move to
// Canceling (T6), with Accepted records resolving immediately (nothing
// dispatched). The persist error is returned: crash recovery rests on the
// ledger reaching disk, so a failed write must not pass silently.
func (s *Store) AdvanceEpoch(rootTaskID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epochs[rootTaskID]++
	current := s.epochs[rootTaskID]
	for _, rec := range s.records {
		if rec.RootTaskID != rootTaskID || rec.TaskEpoch >= current {
			continue
		}
		if err := s.cancelRecordLocked(rec, "parent epoch advanced"); err != nil {
			return current, err
		}
	}
	return current, s.persistLocked()
}

// cancelRecordLocked implements the cancel sweep shared by CancelRoot and
// AdvanceEpoch. Caller holds mu.
func (s *Store) cancelRecordLocked(rec *Record, why string) error {
	switch rec.State {
	case StateProposed:
		rec.Effect = EffectNone
		if err := s.setStateLocked(rec, StateCanceled, why); err != nil {
			return err
		}
		s.settleTerminalLocked(rec, 0, "known-zero settlement")
	case StateAccepted:
		if err := s.setStateLocked(rec, StateCanceling, why); err != nil {
			return err
		}
		rec.Effect = EffectNone
		if err := s.setStateLocked(rec, StateCanceled, "nothing dispatched"); err != nil {
			return err
		}
		s.settleTerminalLocked(rec, 0, "known-zero settlement")
	case StateInProgress:
		return s.setStateLocked(rec, StateCanceling, why)
	}
	return nil
}

// Expire applies the TTL bounds (T9/T10). Expired is reachable only when
// nothing was dispatched; in-flight uncertainty always routes to
// UnknownEffect. The persist error is returned: crash recovery rests on the
// ledger reaching disk, so a failed write must not pass silently.
func (s *Store) Expire() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, rec := range s.records {
		deadline, ok := rec.Deadline()
		if !ok || now.Before(deadline) {
			continue
		}
		switch rec.State {
		case StateProposed, StateAccepted:
			rec.Effect = EffectNone
			if err := s.setStateLocked(rec, StateExpired, "TTL elapsed before any attempt reservation"); err != nil {
				continue
			}
			s.settleTerminalLocked(rec, 0, "known-zero settlement")
		case StateInProgress, StateCanceling:
			if err := s.setStateLocked(rec, StateUnknownEffect, "TTL elapsed with an attempt in flight"); err != nil {
				continue
			}
			s.lockRemainderLocked(rec)
		}
	}
	return s.persistLocked()
}

// Resolve resolves an UnknownEffect record with validated evidence (T12). It
// never re-dispatches. Usage settles only what the evidence covers: a nil
// Usage leaves the locked remainder locked.
func (s *Store) Resolve(id string, ev *Evidence) error {
	if !ev.Validated || ev.Provenance == "" {
		return fmt.Errorf("%w: provenance and validation required", ErrEvidence)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	rec.Result = ev.Result
	rec.Effect = ev.Effect
	if err := s.setStateLocked(rec, ev.Outcome, ev.Provenance); err != nil {
		return err
	}
	if ev.Usage != nil {
		u := *ev.Usage
		if u < 0 || u > rec.Allowance.Locked {
			return fmt.Errorf("%w: usage %d exceeds locked allowance of %s", ErrIntegrity, u, id)
		}
		rec.Allowance.Consumed += u
		released := rec.Allowance.Locked - u
		rec.Allowance.ReleasedUnused += released
		rec.Allowance.Locked = 0
		rec.Settled = true
		if root, ok := s.roots[rec.RootTaskID]; ok {
			root.Consumed += u
			root.Available += released
		}
	}
	return s.persistLocked()
}

// RecordUsageEvidence settles validated usage evidence for a record whose
// usage was unknown (locked). The evidence is the complete accounting of the
// locked remainder: consumed grows by units, the rest is released unused.
func (s *Store) RecordUsageEvidence(id string, units int, provenance string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.Allowance.Locked == 0 {
		return nil // nothing uncertain; idempotent
	}
	if units < 0 || units > rec.Allowance.Locked {
		return fmt.Errorf("%w: usage %d exceeds locked allowance of %s", ErrIntegrity, units, id)
	}
	rec.Allowance.Consumed += units
	released := rec.Allowance.Locked - units
	rec.Allowance.ReleasedUnused += released
	rec.Allowance.Locked = 0
	rec.Provenance = append(rec.Provenance, "usage evidence: "+provenance)
	if root, ok := s.roots[rec.RootTaskID]; ok {
		root.Consumed += units
		root.Available += released
	}
	return s.persistLocked()
}

// Reconcile performs restart recovery (reconciliation rules 1–5): Accepted
// records re-run the T3 guard set and settle as Expired/Canceled when the
// guards no longer hold; InProgress records (including both crash windows)
// are queried by attempt ID — a confirmed outcome lands as Completed/Failed,
// no account lands as UnknownEffect and is never re-dispatched; Canceling
// stays Canceling until T7/T8 resolves. Uncertain-usage allowance stays
// locked. Reconciliation is idempotent: re-running it is a no-op.
func (s *Store) Reconcile(ctx context.Context, exec Executor) error {
	// Rule 3: Accepted at restart re-runs the guard set before any dispatch.
	s.mu.Lock()
	for _, rec := range s.records {
		if rec.State != StateAccepted {
			continue
		}
		if deadline, ok := rec.Deadline(); ok && !s.now().Before(deadline) {
			rec.Effect = EffectNone
			if err := s.setStateLocked(rec, StateExpired, "restart: TTL elapsed"); err != nil {
				continue
			}
			s.settleTerminalLocked(rec, 0, "known-zero settlement")
			continue
		}
		if s.canceledRoots[rec.RootTaskID] || s.epochs[rec.RootTaskID] != rec.TaskEpoch || (s.auth != nil && !s.auth(rec.Scope)) {
			if err := s.cancelRecordLocked(rec, "restart: guards no longer hold"); err != nil {
				continue
			}
		}
	}
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return err
	}

	// Rule 2: InProgress at restart (both crash windows) queries by attempt ID.
	var pending []*Record
	for _, rec := range s.records {
		if rec.State == StateInProgress && len(rec.Attempts) > 0 {
			pending = append(pending, rec)
		}
	}
	s.mu.Unlock()

	for _, rec := range pending {
		attemptID := rec.Attempts[len(rec.Attempts)-1].ID
		outcome, accounted, err := exec.QueryAttempt(ctx, attemptID)

		s.mu.Lock()
		// Re-read under the lock: the record may have moved meanwhile.
		cur := s.records[rec.ID]
		if cur == nil || cur.State != StateInProgress {
			s.mu.Unlock()
			continue
		}
		if err != nil || !accounted {
			if serr := s.setStateLocked(cur, StateUnknownEffect, "restart: executor cannot account for "+attemptID); serr != nil {
				s.mu.Unlock()
				return serr
			}
			s.lockRemainderLocked(cur)
		} else {
			if serr := s.applyOutcomeLocked(cur, outcome, "restart query "+attemptID); serr != nil {
				s.mu.Unlock()
				return serr
			}
		}
		if perr := s.persistLocked(); perr != nil {
			s.mu.Unlock()
			return perr
		}
		s.mu.Unlock()
	}
	return nil
}
