package taskenvelope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
)

// SimExecutor is an in-memory scriptable Executor for fault tests. The zero
// value reports successful, known-zero outcomes and attests stops.
type SimExecutor struct {
	mu       sync.Mutex
	attempts map[string]Outcome // what the executor actually did, by attempt ID

	// Outcome is what Do reports on success.
	Outcome Outcome
	// LoseResult makes Do execute (recorded internally, so QueryAttempt can
	// still account for the attempt) but report a lost result/ACK.
	LoseResult bool
	// ForgetAttempts makes QueryAttempt unable to account for anything
	// (executor lost its log; does NOT imply the call never happened).
	ForgetAttempts bool
	// Gate, when non-nil, blocks Do until it is closed (late-completion
	// scenarios).
	Gate chan struct{}
	// StopAttested controls what AttestStop reports; StopEffect is the effect
	// reported with an attested stop.
	StopAttested bool
	StopEffect   Effect
}

var _ Executor = (*SimExecutor)(nil)

// NewSimExecutor returns a SimExecutor with a successful empty outcome.
func NewSimExecutor() *SimExecutor {
	return &SimExecutor{
		attempts:     make(map[string]Outcome),
		Outcome:      Outcome{Success: true, Effect: EffectNone},
		StopAttested: true,
		StopEffect:   EffectNone,
	}
}

// Do implements Executor.
func (e *SimExecutor) Do(ctx context.Context, a Attempt) (Outcome, error) {
	if e.Gate != nil {
		select {
		case <-e.Gate:
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	e.mu.Lock()
	e.attempts[a.ID] = e.Outcome
	e.mu.Unlock()
	if e.LoseResult {
		return Outcome{}, errors.New("sim: result/ACK lost")
	}
	return e.Outcome, nil
}

// QueryAttempt implements Executor.
func (e *SimExecutor) QueryAttempt(_ context.Context, attemptID string) (Outcome, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ForgetAttempts {
		return Outcome{}, false, nil
	}
	outcome, ok := e.attempts[attemptID]
	return outcome, ok, nil
}

// AttestStop implements Executor. The knobs are the script: StopAttested
// decides whether a stop is attested, StopEffect is the reported effect.
func (e *SimExecutor) AttestStop(_ context.Context, _ string) (StopAttestation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return StopAttestation{Attested: e.StopAttested, Effect: e.StopEffect}, nil
}

// FakeEdge is a file-backed fake edge service for fault tests. Its attempt log
// survives process recreation against the same files; Restart simulates a kill
// and recreate (reconciliation rule 6: reconnect alone is not restart).
type FakeEdge struct {
	mu   sync.Mutex
	path string
	log  map[string]Outcome

	// LoseResult makes Do persist the outcome but report a lost result/ACK.
	LoseResult bool
	// StopEffect is the effect reported with an attested stop.
	StopEffect Effect
}

var _ Executor = (*FakeEdge)(nil)

// NewFakeEdge opens (or creates) a fake edge service ledger at path.
func NewFakeEdge(path string) (*FakeEdge, error) {
	e := &FakeEdge{path: path, log: make(map[string]Outcome), StopEffect: EffectNone}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fakeedge: read log: %w", err)
	}
	if err := json.Unmarshal(data, &e.log); err != nil {
		return nil, fmt.Errorf("fakeedge: decode log: %w", err)
	}
	return e, nil
}

// Restart discards in-memory state and reloads from disk: a process kill and
// recreate against the same files.
func (e *FakeEdge) Restart() error {
	fresh, err := NewFakeEdge(e.path)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = fresh.log
	return nil
}

// Do implements Executor. The outcome is persisted before any result is
// reported, so a crash between the two leaves the attempt accounted for.
func (e *FakeEdge) Do(_ context.Context, a Attempt) (Outcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	outcome := Outcome{Success: true, Result: "done", Effect: EffectNone}
	e.log[a.ID] = outcome
	if err := e.persistLocked(); err != nil {
		return Outcome{}, err
	}
	if e.LoseResult {
		return Outcome{}, errors.New("fakeedge: result/ACK lost")
	}
	return outcome, nil
}

// QueryAttempt implements Executor.
func (e *FakeEdge) QueryAttempt(_ context.Context, attemptID string) (Outcome, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	outcome, ok := e.log[attemptID]
	return outcome, ok, nil
}

// AttestStop implements Executor: an attested stop requires an accounted
// attempt.
func (e *FakeEdge) AttestStop(_ context.Context, attemptID string) (StopAttestation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.log[attemptID]; !ok {
		return StopAttestation{Attested: false}, nil
	}
	return StopAttestation{Attested: true, Effect: e.StopEffect}, nil
}

func (e *FakeEdge) persistLocked() error {
	data, err := json.Marshal(e.log)
	if err != nil {
		return fmt.Errorf("fakeedge: encode log: %w", err)
	}
	if err := fsutil.WriteFileAtomic(e.path, data, 0o644); err != nil {
		return fmt.Errorf("fakeedge: write log: %w", err)
	}
	return nil
}
