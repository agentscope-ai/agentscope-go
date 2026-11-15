// Package storage provides session state persistence backends.
//
// All implementations satisfy agent.StateSaver. Two are included:
//   - InMemoryStorage: process-local map, suitable for testing and short-lived sessions
//   - FileStorage: JSON file persistence, one file per session
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/fsutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

// InMemoryStorage stores agent states in a process-local map.
type InMemoryStorage struct {
	mu     sync.RWMutex
	states map[string]*agent.AgentState
}

// NewInMemoryStorage creates an empty in-memory store.
func NewInMemoryStorage() *InMemoryStorage {
	return &InMemoryStorage{states: make(map[string]*agent.AgentState)}
}

func (s *InMemoryStorage) SaveState(_ context.Context, sessionID string, state *agent.AgentState) error {
	if sessionID == "" {
		return fmt.Errorf("storage: session ID is required")
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("storage: marshal state: %w", err)
	}
	var clone agent.AgentState
	if err := json.Unmarshal(data, &clone); err != nil {
		return fmt.Errorf("storage: clone state: %w", err)
	}
	clone.SessionID = sessionID

	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[sessionID] = &clone
	return nil
}

func (s *InMemoryStorage) LoadState(_ context.Context, sessionID string) (*agent.AgentState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state, ok := s.states[sessionID]
	if !ok {
		return nil, fmt.Errorf("storage: session %q not found", sessionID)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("storage: marshal: %w", err)
	}
	var clone agent.AgentState
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, fmt.Errorf("storage: clone: %w", err)
	}
	return &clone, nil
}

func (s *InMemoryStorage) ListSessions(_ context.Context) ([]agent.SessionInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	infos := make([]agent.SessionInfo, 0, len(s.states))
	for id, st := range s.states {
		infos = append(infos, agent.SessionInfo{
			SessionID: id,
			Summary:   st.Summary,
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].SessionID < infos[j].SessionID
	})
	return infos, nil
}

func (s *InMemoryStorage) DeleteSession(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.states[sessionID]; !ok {
		return fmt.Errorf("storage: session %q not found", sessionID)
	}
	delete(s.states, sessionID)
	return nil
}

// FileStorage persists agent states as JSON files in a directory,
// one file per session (named <session_id>.json).
type FileStorage struct {
	dir string
}

// NewFileStorage creates a file-based store. The directory is created if needed.
func NewFileStorage(dir string) (*FileStorage, error) {
	if dir == "" {
		return nil, fmt.Errorf("storage: directory path is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create dir: %w", err)
	}
	return &FileStorage{dir: abs}, nil
}

func (s *FileStorage) SaveState(_ context.Context, sessionID string, state *agent.AgentState) error {
	if sessionID == "" {
		return fmt.Errorf("storage: session ID is required")
	}
	state.SessionID = sessionID

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: marshal: %w", err)
	}
	return fsutil.WriteFileAtomic(s.path(sessionID), data, 0o644)
}

func (s *FileStorage) LoadState(_ context.Context, sessionID string) (*agent.AgentState, error) {
	data, err := os.ReadFile(s.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("storage: session %q not found", sessionID)
		}
		return nil, fmt.Errorf("storage: read: %w", err)
	}
	var state agent.AgentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("storage: unmarshal: %w", err)
	}
	return &state, nil
}

func (s *FileStorage) ListSessions(_ context.Context) ([]agent.SessionInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("storage: list dir: %w", err)
	}

	var infos []agent.SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sessionID := strings.TrimSuffix(e.Name(), ".json")

		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var state agent.AgentState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		infos = append(infos, agent.SessionInfo{
			SessionID: sessionID,
			Summary:   state.Summary,
		})
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].SessionID < infos[j].SessionID
	})
	return infos, nil
}

func (s *FileStorage) DeleteSession(_ context.Context, sessionID string) error {
	path := s.path(sessionID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("storage: session %q not found", sessionID)
	}
	return os.Remove(path)
}

func (s *FileStorage) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".json")
}

// Compile-time interface checks.
var _ agent.StateSaver = (*InMemoryStorage)(nil)
var _ agent.StateSaver = (*FileStorage)(nil)
