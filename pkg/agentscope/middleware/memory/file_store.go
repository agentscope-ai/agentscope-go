package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
)

// FileStoreFilename is the JSON Lines file a FileStore persists memories in.
const FileStoreFilename = "memories.jsonl"

// Compile-time interface check.
var _ MemoryStore = (*FileStore)(nil)

// FileStore is a MemoryStore persisted as JSON Lines in one directory —
// the file-based store of the long-term-memory trio (InMemory / Mem0 /
// Vector / File). Point it at a workspace directory to give an agent
// durable cross-session memory without external services. One JSON record
// per line: {"id","text","user_id","agent_id","created_at"}.
//
// Search semantics match InMemoryStore (case-insensitive any-word
// substring match); the persistence format is append-friendly so a crash
// loses at most the record being written.
//
// Concurrency scope: one FileStore instance is safe for concurrent use.
// Two instances (or two processes) on the SAME file are not: a Delete's
// atomic rewrite from one instance's snapshot can lose a concurrent
// append from the other. Memory directories are per-agent by design —
// do not share one file across stores.
type FileStore struct {
	path string

	mu       sync.Mutex
	memories []Memory
	nextID   int
	loaded   bool
}

// NewFileStore creates a store backed by <dir>/memories.jsonl. The
// directory is created if needed; an existing file is loaded lazily on
// first use.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("memory: file store directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create store dir: %w", err)
	}
	return &FileStore{path: filepath.Join(dir, FileStoreFilename)}, nil
}

type fileRecord struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	UserID    string    `json:"user_id"`
	AgentID   string    `json:"agent_id"`
	CreatedAt time.Time `json:"created_at"`
}

// loadLocked reads the backing file on first use. Malformed lines are
// skipped (a torn final line from a crash must not break the store); an
// unreadable stream fails the whole load and commits nothing.
func (s *FileStore) loadLocked() error {
	if s.loaded {
		return nil
	}

	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.loaded = true
			s.nextID = 1
			return nil
		}
		return fmt.Errorf("memory: open store file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var memories []Memory
	nextID := 1
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec fileRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.ID == "" || rec.Text == "" {
			continue
		}
		memories = append(memories, Memory(rec))
		if n, ok := parseMemoryID(rec.ID); ok && n >= nextID {
			nextID = n + 1
		}
	}
	// Commit nothing unless the whole scan succeeded: an unreadable
	// stream (e.g. an over-long line) must leave a consistent state a
	// retry can rely on, not partial data.
	if err := sc.Err(); err != nil {
		return fmt.Errorf("memory: read store file: %w", err)
	}
	s.memories = memories
	s.nextID = nextID
	s.loaded = true
	return nil
}

// parseMemoryID extracts the counter from a "mem_N" id.
func parseMemoryID(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "mem_"))
	if err != nil || !strings.HasPrefix(id, "mem_") {
		return 0, false
	}
	return n, true
}

func (s *FileStore) Add(_ context.Context, text string, userID string, agentID string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}

	rec := fileRecord{
		ID:        fmt.Sprintf("mem_%d", s.nextID),
		Text:      text,
		UserID:    userID,
		AgentID:   agentID,
		CreatedAt: time.Now(),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory: marshal record: %w", err)
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("memory: open store file: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("memory: append record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memory: close store file: %w", err)
	}

	s.nextID++
	s.memories = append(s.memories, Memory(rec))
	return nil
}

func (s *FileStore) Search(_ context.Context, query string, userID string, opts *SearchOptions) ([]Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}

	topK := 5
	if opts != nil && opts.TopK > 0 {
		topK = opts.TopK
	}
	queryWords := strings.Fields(strings.ToLower(query))

	var results []Memory
	for _, m := range s.memories {
		if m.UserID != userID {
			continue
		}
		if opts != nil && opts.AgentID != "" && m.AgentID != opts.AgentID {
			continue
		}
		if matchesAny(strings.ToLower(m.Text), queryWords) {
			results = append(results, m)
			if len(results) >= topK {
				break
			}
		}
	}
	return results, nil
}

func (s *FileStore) List(_ context.Context, userID string) ([]Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	var results []Memory
	for _, m := range s.memories {
		if m.UserID == userID {
			results = append(results, m)
		}
	}
	return results, nil
}

func (s *FileStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}

	idx := -1
	for i, m := range s.memories {
		if m.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("memory %q not found", id)
	}
	remaining := make([]Memory, 0, len(s.memories)-1)
	remaining = append(remaining, s.memories[:idx]...)
	remaining = append(remaining, s.memories[idx+1:]...)
	if err := s.rewriteLocked(remaining); err != nil {
		return err
	}
	s.memories = remaining
	return nil
}

// rewriteLocked atomically rewrites the backing file from memory.
func (s *FileStore) rewriteLocked(memories []Memory) error {
	var b strings.Builder
	for _, m := range memories {
		line, err := json.Marshal(fileRecord(m))
		if err != nil {
			return fmt.Errorf("memory: marshal record: %w", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := fsutil.WriteFileAtomic(s.path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("memory: rewrite store file: %w", err)
	}
	return nil
}
