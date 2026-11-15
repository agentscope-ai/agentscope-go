package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// SessionState represents the persisted state of a session.
type SessionState struct {
	ID        string         `json:"id"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Messages  []*message.Msg `json:"messages,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func cloneSessionState(state *SessionState) *SessionState {
	if state == nil {
		return nil
	}
	cp := *state
	cp.Messages = append([]*message.Msg(nil), state.Messages...)
	if state.Metadata != nil {
		cp.Metadata = make(map[string]any, len(state.Metadata))
		for key, value := range state.Metadata {
			cp.Metadata[key] = value
		}
	}
	return &cp
}

// SessionStore defines the interface for session persistence.
type SessionStore interface {
	Save(id string, state *SessionState) error
	Load(id string) (*SessionState, error)
	Delete(id string) error
	List() ([]string, error)
}

// InMemorySessionStore is a SessionStore backed by an in-memory map.
// Suitable for testing and short-lived processes.
type InMemorySessionStore struct {
	mu    sync.RWMutex
	store map[string]*SessionState
}

// NewInMemorySessionStore creates a new in-memory session store.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{store: make(map[string]*SessionState)}
}

// Save stores a copy of the session state.
func (s *InMemorySessionStore) Save(_ string, state *SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[state.ID] = cloneSessionState(state)
	return nil
}

// Load retrieves the session state by ID.
func (s *InMemorySessionStore) Load(id string) (*SessionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.store[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return cloneSessionState(state), nil
}

// Delete removes the session state by ID.
func (s *InMemorySessionStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.store, id)
	return nil
}

// List returns all session IDs in the store.
func (s *InMemorySessionStore) List() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.store))
	for id := range s.store {
		ids = append(ids, id)
	}
	return ids, nil
}

// FileSessionStore is a SessionStore backed by the local filesystem.
// Each session is stored as a JSON file in a subdirectory of baseDir.
type FileSessionStore struct {
	baseDir string
}

// NewFileSessionStore creates a new file-based session store rooted at baseDir.
func NewFileSessionStore(baseDir string) *FileSessionStore {
	return &FileSessionStore{baseDir: baseDir}
}

// Save persists the session state to disk as JSON.
func (s *FileSessionStore) Save(_ string, state *SessionState) error {
	dir := filepath.Join(s.baseDir, state.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session store: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("session store: marshal: %w", err)
	}
	path := filepath.Join(dir, "session.json")
	return fsutil.WriteFileAtomic(path, data, 0o644)
}

// Load reads the session state from disk.
func (s *FileSessionStore) Load(id string) (*SessionState, error) {
	path := filepath.Join(s.baseDir, id, "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session store: read %q: %w", id, err)
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("session store: unmarshal %q: %w", id, err)
	}
	return &state, nil
}

// Delete removes the session directory from disk.
func (s *FileSessionStore) Delete(id string) error {
	dir := filepath.Join(s.baseDir, id)
	return os.RemoveAll(dir)
}

// List returns all session IDs that have a session.json on disk.
// Returns (nil, nil) if the base directory does not exist.
func (s *FileSessionStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session store: list: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			path := filepath.Join(s.baseDir, e.Name(), "session.json")
			if _, err := os.Stat(path); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	return ids, nil
}
