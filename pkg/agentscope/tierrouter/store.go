package tierrouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
)

// Log is the persisted decision log: the evidence source for grading (R1).
// Use Open; the zero value is not usable. An empty path gives an in-memory
// log (unit tests only — restart evidence requires a file).
type Log struct {
	mu      sync.Mutex
	path    string
	records map[string]*Record
}

// snapshot is the on-disk form.
type snapshot struct {
	Records map[string]*Record `json:"records"`
}

// Open loads the log from path (or starts empty when the file does not
// exist) and reconciles crash windows: any record still pending — the
// process died mid-decision, whether before the first attempt, between the
// commit boundary and the outcome, or between attempts — is marked uncertain
// and is never silently re-dispatched (R6). Settled attempt outcomes are
// preserved as evidence; unsettled attempts are recorded unknown.
func Open(path string) (*Log, error) {
	l := &Log{path: path, records: make(map[string]*Record)}
	if path == "" {
		return l, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return l, nil
	case err != nil:
		return nil, fmt.Errorf("tierrouter: read log %s: %w", path, err)
	case len(data) == 0:
		return l, nil
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("tierrouter: corrupt log %s: %w", path, err)
	}
	if snap.Records != nil {
		l.records = snap.Records
	}
	if l.reconcile() {
		if err := l.persistLocked(); err != nil {
			return nil, fmt.Errorf("tierrouter: persist reconciliation: %w", err)
		}
	}
	return l, nil
}

// reconcile marks every pending record uncertain: nothing can legitimately
// be in flight across a restart, so a pending record on disk is always crash
// residue (R6). Unsettled attempts within it are recorded unknown. It
// reports whether any record changed.
func (l *Log) reconcile() bool {
	dirty := false
	for _, rec := range l.records {
		if rec.State != StatePending {
			continue
		}
		for i := range rec.Attempts {
			if !rec.Attempts[i].Settled {
				rec.Attempts[i].Settled = true
				rec.Attempts[i].Outcome = AttemptUnknown
				rec.Attempts[i].Err = "process lost before outcome was recorded"
			}
		}
		rec.State = StateUncertain
		dirty = true
	}
	return dirty
}

// Get returns a clone of the recorded decision for id, if any.
func (l *Log) Get(id string) (*Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[id]
	if !ok {
		return nil, false
	}
	return rec.Clone(), true
}

// persistLocked writes the snapshot atomically; the caller must hold mu.
func (l *Log) persistLocked() error {
	if l.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(snapshot{Records: l.records}, "", "  ")
	if err != nil {
		return fmt.Errorf("tierrouter: encode log: %w", err)
	}
	return fsutil.WriteFileAtomic(l.path, data, 0o600)
}
