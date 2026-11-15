package thread

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Head is the log's tail state.
type Head struct {
	// LastSeq is the seq of the newest entry, 0 for an empty log.
	LastSeq uint64
	// Owner is the target of the last owner_change, "" before any.
	Owner string
	// Epoch is the number of owner_change entries so far.
	Epoch uint64
}

// Filter narrows a Read. Zero values mean no filter.
type Filter struct {
	// Kinds keeps only entries of these kinds.
	Kinds []Kind
	// To keeps only entries whose To names this participant.
	To string
	// ExcludeOrigin skips entries whose Origin.Participant equals this.
	ExcludeOrigin string
}

func (f *Filter) matches(e *Entry) bool {
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, e.Kind) {
		return false
	}
	if f.To != "" && !e.Addressed(f.To) {
		return false
	}
	if f.ExcludeOrigin != "" && e.Origin.Participant == f.ExcludeOrigin {
		return false
	}
	return true
}

// Store is the append-only Main Thread log. Implementations are safe for
// concurrent use; Read and Get return deep copies.
type Store interface {
	// Append writes a batch atomically and returns the seq of its last entry.
	//
	// It assigns Seq in batch order, ignoring any caller-set Seq, sets a zero
	// Time from the store's clock and sets Epoch to the epoch in force when
	// the entry is written: an owner_change starts a new epoch that it and
	// every later entry carry. A batch holding a reply or an owner_change
	// needs a guard; any guard given must match the head's owner and epoch
	// before the batch, otherwise the error wraps ErrNotOwner. A batch whose
	// entries all carry dedup keys already in the log is a retry: Append
	// writes nothing and returns the seq of the last of those entries. A batch
	// that repeats only some keys fails with ErrDuplicate.
	Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error)
	// Read returns up to limit entries with Seq > after that match f, in
	// order. A limit <= 0 means no limit.
	Read(ctx context.Context, after uint64, f Filter, limit int) ([]Entry, error)
	// Get returns the entry with this seq, and false when there is none.
	Get(ctx context.Context, seq uint64) (Entry, bool, error)
	// Head returns the log's tail state.
	Head(ctx context.Context) (Head, error)
	// Close releases the store. Every later call returns ErrClosed.
	Close() error
}

// StoreOption configures a MemoryStore or a FileStore.
type StoreOption func(*storeConfig)

type storeConfig struct {
	clock  func() time.Time
	noSync bool
}

func newStoreConfig(opts []StoreOption) storeConfig {
	cfg := storeConfig{clock: time.Now}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.clock == nil {
		cfg.clock = time.Now
	}
	return cfg
}

// WithClock sets the clock that stamps entries appended with a zero Time.
// A nil clock keeps time.Now.
func WithClock(clock func() time.Time) StoreOption {
	return func(c *storeConfig) { c.clock = clock }
}

// WithoutSync makes a FileStore skip the fsync after each Append. It is
// meant for tests; a crash can then lose acknowledged batches. MemoryStore
// ignores it.
func WithoutSync() StoreOption {
	return func(c *storeConfig) { c.noSync = true }
}

// logState is the in-memory log both stores share. It is not locked; the
// owning store serializes access.
type logState struct {
	clock   func() time.Time
	entries []Entry // entries[i].Seq == i+1
	head    Head
	dedup   map[string]uint64
}

func newLogState(clock func() time.Time) *logState {
	return &logState{clock: clock, dedup: make(map[string]uint64)}
}

// plan validates a batch against the current state and returns normalized
// copies with Seq, Epoch and Time assigned. For a retry of a batch already
// in the log it returns no entries and the seq of the last repeated entry.
func (s *logState) plan(batch []Entry, g *Guard) ([]Entry, uint64, error) {
	if len(batch) == 0 {
		return nil, 0, errors.New("thread: append needs at least one entry")
	}
	planned := make([]Entry, len(batch))
	guarded := false
	keyed := 0
	keys := make(map[string]struct{})
	for i := range batch {
		e := &batch[i]
		if err := validateEntry(e); err != nil {
			return nil, 0, fmt.Errorf("thread: batch entry %d: %w", i, err)
		}
		c, err := normalizeEntry(e)
		if err != nil {
			return nil, 0, fmt.Errorf("thread: batch entry %d: %w", i, err)
		}
		planned[i] = c
		if e.Kind == KindReply || e.Kind == KindOwnerChange {
			guarded = true
		}
		if e.DedupKey != "" {
			if _, dup := keys[e.DedupKey]; dup {
				return nil, 0, fmt.Errorf("thread: dedup key %q repeats within the batch", e.DedupKey)
			}
			keys[e.DedupKey] = struct{}{}
			keyed++
		}
	}

	if keyed > 0 {
		present := 0
		var last uint64
		for k := range keys {
			if seq, ok := s.dedup[k]; ok {
				present++
				last = max(last, seq)
			}
		}
		switch {
		case present == len(batch):
			return nil, last, nil
		case present > 0:
			return nil, 0, fmt.Errorf("%w: %d of %d entries are already in the log", ErrDuplicate, present, len(batch))
		}
	}

	if guarded && g == nil {
		return nil, 0, fmt.Errorf("%w: a batch with a reply or an owner_change needs a guard", ErrNotOwner)
	}
	if g != nil && (g.Owner != s.head.Owner || g.Epoch != s.head.Epoch) {
		return nil, 0, fmt.Errorf("%w: guard names %q at epoch %d, the log has %q at epoch %d",
			ErrNotOwner, g.Owner, g.Epoch, s.head.Owner, s.head.Epoch)
	}

	now := s.clock().Round(0)
	seq, epoch := s.head.LastSeq, s.head.Epoch
	for i := range planned {
		e := &planned[i]
		seq++
		if e.Kind == KindOwnerChange {
			epoch++
		}
		e.Seq = seq
		e.Epoch = epoch
		if e.Time.IsZero() {
			e.Time = now
		}
	}
	return planned, 0, nil
}

// apply adds entries that plan or replay accepted against this state.
func (s *logState) apply(entries []Entry) {
	for i := range entries {
		e := &entries[i]
		s.entries = append(s.entries, *e)
		if e.DedupKey != "" {
			s.dedup[e.DedupKey] = e.Seq
		}
		if e.Kind == KindOwnerChange {
			s.head.Owner = e.Owner.To
		}
		s.head.LastSeq = e.Seq
		s.head.Epoch = e.Epoch
	}
}

// replay checks a batch read back from storage and applies it.
func (s *logState) replay(batch []Entry) error {
	if len(batch) == 0 {
		return errors.New("empty batch")
	}
	seq, epoch := s.head.LastSeq, s.head.Epoch
	keys := make(map[string]struct{})
	for i := range batch {
		e := &batch[i]
		if err := validateEntry(e); err != nil {
			return fmt.Errorf("entry %d: %w", e.Seq, err)
		}
		seq++
		if e.Seq != seq {
			return fmt.Errorf("entry has seq %d, want %d", e.Seq, seq)
		}
		if e.Kind == KindOwnerChange {
			epoch++
		}
		if e.Epoch != epoch {
			return fmt.Errorf("entry %d has epoch %d, want %d", e.Seq, e.Epoch, epoch)
		}
		if e.DedupKey != "" {
			_, inLog := s.dedup[e.DedupKey]
			_, inBatch := keys[e.DedupKey]
			if inLog || inBatch {
				return fmt.Errorf("entry %d repeats dedup key %q", e.Seq, e.DedupKey)
			}
			keys[e.DedupKey] = struct{}{}
		}
	}
	s.apply(batch)
	return nil
}

func (s *logState) read(after uint64, f *Filter, limit int) []Entry {
	if after >= uint64(len(s.entries)) {
		return nil
	}
	var out []Entry
	for i := int(after); i < len(s.entries); i++ {
		e := &s.entries[i]
		if !f.matches(e) {
			continue
		}
		out = append(out, cloneEntry(e))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (s *logState) get(seq uint64) (Entry, bool) {
	if seq == 0 || seq > uint64(len(s.entries)) {
		return Entry{}, false
	}
	return cloneEntry(&s.entries[seq-1]), true
}

// validateEntry checks the fields an entry's kind requires.
func validateEntry(e *Entry) error {
	switch e.Kind {
	case KindUserInput, KindReply, KindReport, KindArtifact:
	case KindOwnerChange:
		if e.Owner == nil || strings.TrimSpace(e.Owner.To) == "" {
			return errors.New("thread: owner_change needs an owner with a target")
		}
	case KindSummary:
		if e.Coverage == nil {
			return errors.New("thread: summary needs a coverage")
		}
		if e.Summary == nil || strings.TrimSpace(e.Summary.Target) == "" {
			return errors.New("thread: summary needs a target")
		}
	case KindAssign:
		if e.Task == nil || strings.TrimSpace(e.Task.ID) == "" {
			return errors.New("thread: assign needs a task with an id")
		}
	case KindMarker:
		if e.Marker == nil {
			return errors.New("thread: marker needs a marker payload")
		}
	case KindProgress:
		if strings.TrimSpace(e.Run) == "" {
			return errors.New("thread: progress needs the run it reports on")
		}
	default:
		return fmt.Errorf("thread: unknown entry kind %q", e.Kind)
	}
	if e.Task != nil && e.Task.End != "" && e.Kind != KindReport {
		return fmt.Errorf("thread: %s may not carry a task end", e.Kind)
	}
	return ValidateContent(e.Content)
}

// normalizeEntry returns a deep copy of a validated entry in the form the
// log stores: value content blocks with their type tags set, so the copy
// survives a JSON round trip.
func normalizeEntry(e *Entry) (Entry, error) {
	c := cloneEntry(e)
	blocks, err := normalizeBlocks(e.Content)
	if err != nil {
		return Entry{}, err
	}
	c.Content = blocks
	return c, nil
}

func normalizeBlocks(blocks []message.ContentBlock) ([]message.ContentBlock, error) {
	if blocks == nil {
		return nil, nil
	}
	out := make([]message.ContentBlock, len(blocks))
	for i, b := range blocks {
		n, err := normalizeBlock(b)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// normalizeBlock copies a text, data or hint block into its value form.
// Other block types cannot be stored in the log.
func normalizeBlock(b message.ContentBlock) (message.ContentBlock, error) {
	switch v := b.(type) {
	case message.TextBlock:
		v.Type = string(message.ContentBlockText)
		return v, nil
	case *message.TextBlock:
		if v == nil {
			return nil, errors.New("thread: nil text block")
		}
		return normalizeBlock(*v)
	case message.DataBlock:
		src, err := normalizeSource(v.Source)
		if err != nil {
			return nil, err
		}
		v.Type = string(message.ContentBlockData)
		v.Source = src
		return v, nil
	case *message.DataBlock:
		if v == nil {
			return nil, errors.New("thread: nil data block")
		}
		return normalizeBlock(*v)
	case message.HintBlock:
		switch h := v.Hint.(type) {
		case nil:
			v.Hint = ""
		case string:
		case []message.ContentBlock:
			blocks, err := normalizeBlocks(h)
			if err != nil {
				return nil, err
			}
			v.Hint = blocks
		default:
			return nil, fmt.Errorf("thread: hint %T cannot be stored", h)
		}
		v.Type = string(message.ContentBlockHint)
		return v, nil
	case *message.HintBlock:
		if v == nil {
			return nil, errors.New("thread: nil hint block")
		}
		return normalizeBlock(*v)
	case nil:
		return nil, errors.New("thread: nil content block")
	default:
		return nil, fmt.Errorf("thread: entry content may not hold %T", b)
	}
}

func normalizeSource(src any) (any, error) {
	switch s := src.(type) {
	case message.Base64Source:
		s.Type = "base64"
		return s, nil
	case *message.Base64Source:
		if s != nil {
			return normalizeSource(*s)
		}
	case message.URLSource:
		s.Type = "url"
		return s, nil
	case *message.URLSource:
		if s != nil {
			return normalizeSource(*s)
		}
	case map[string]any:
		// The form message.UnmarshalContentBlocks gives an unknown source.
		return maps.Clone(s), nil
	}
	return nil, fmt.Errorf("thread: data block source %T cannot be stored", src)
}

// cloneEntry deep-copies the slices and pointer payloads of an entry.
func cloneEntry(e *Entry) Entry {
	c := *e
	c.To = slices.Clone(e.To)
	c.Content = cloneBlocks(e.Content)
	c.Refs = slices.Clone(e.Refs)
	c.Steered = slices.Clone(e.Steered)
	if e.Digest != nil {
		d := *e.Digest
		d.Calls = slices.Clone(d.Calls)
		c.Digest = &d
	}
	if e.Coverage != nil {
		v := *e.Coverage
		v.Excluded = slices.Clone(v.Excluded)
		c.Coverage = &v
	}
	if e.Summary != nil {
		s := *e.Summary
		c.Summary = &s
	}
	if e.Owner != nil {
		o := *e.Owner
		o.Brief = cloneBrief(o.Brief)
		c.Owner = &o
	}
	if e.Task != nil {
		t := *e.Task
		c.Task = &t
	}
	if e.Marker != nil {
		m := *e.Marker
		c.Marker = &m
	}
	return c
}

// cloneBlocks copies content blocks. Blocks normalizeBlock cannot handle
// are kept as they are; the store never holds them.
func cloneBlocks(blocks []message.ContentBlock) []message.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]message.ContentBlock, len(blocks))
	for i, b := range blocks {
		if n, err := normalizeBlock(b); err == nil {
			out[i] = n
		} else {
			out[i] = b
		}
	}
	return out
}

func cloneBrief(b *Brief) *Brief {
	if b == nil {
		return nil
	}
	c := *b
	c.Done = slices.Clone(b.Done)
	c.Remaining = slices.Clone(b.Remaining)
	c.Constraints = slices.Clone(b.Constraints)
	c.Refs = slices.Clone(b.Refs)
	return &c
}

// MemoryStore is a Store held in memory.
type MemoryStore struct {
	mu     sync.RWMutex
	state  *logState
	closed bool
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore(opts ...StoreOption) *MemoryStore {
	cfg := newStoreConfig(opts)
	return &MemoryStore{state: newLogState(cfg.clock)}
}

// Append implements Store.
func (s *MemoryStore) Append(ctx context.Context, entries []Entry, g *Guard) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	planned, retry, err := s.state.plan(entries, g)
	if err != nil {
		return 0, err
	}
	if planned == nil {
		return retry, nil
	}
	s.state.apply(planned)
	return planned[len(planned)-1].Seq, nil
}

// Read implements Store.
func (s *MemoryStore) Read(ctx context.Context, after uint64, f Filter, limit int) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.state.read(after, &f, limit), nil
}

// Get implements Store.
func (s *MemoryStore) Get(ctx context.Context, seq uint64) (Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Entry{}, false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	e, ok := s.state.get(seq)
	return e, ok, nil
}

// Head implements Store.
func (s *MemoryStore) Head(ctx context.Context) (Head, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Head{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	return s.state.head, nil
}

// Close implements Store. A second Close returns ErrClosed.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.closed = true
	return nil
}
