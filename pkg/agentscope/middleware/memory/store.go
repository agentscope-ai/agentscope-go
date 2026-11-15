package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrStoreUnsupported marks MemoryStore operations a backend does not
// implement. Returned wrapped, so errors.Is(err, ErrStoreUnsupported)
// distinguishes an unsupported operation from a transport or server failure.
var ErrStoreUnsupported = errors.New("memory: operation not supported by this store")

// Memory represents a single stored memory entry.
type Memory struct {
	ID        string
	Text      string
	UserID    string
	AgentID   string
	CreatedAt time.Time
}

// SearchOptions controls how memories are searched.
type SearchOptions struct {
	AgentID   string  // filter by agent; empty = all agents
	TopK      int     // max results; <=0 = use store default
	Threshold float64 // minimum relevance; 0 = no threshold
}

// MemoryStore is the backend interface for long-term memory persistence.
// Implementations may use simple text matching, embedding vectors, or
// external services (e.g. mem0, vector databases).
type MemoryStore interface {
	Search(ctx context.Context, query string, userID string, opts *SearchOptions) ([]Memory, error)
	Add(ctx context.Context, text string, userID string, agentID string) error
	List(ctx context.Context, userID string) ([]Memory, error)
	Delete(ctx context.Context, id string) error
}

// InMemoryStore is a simple in-memory MemoryStore that uses case-insensitive
// substring matching for search. Suitable for testing and lightweight use.
type InMemoryStore struct {
	mu       sync.RWMutex
	memories []Memory
	nextID   int
}

// NewInMemoryStore creates an empty in-memory store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

func (s *InMemoryStore) Add(_ context.Context, text string, userID string, agentID string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	s.memories = append(s.memories, Memory{
		ID:        fmt.Sprintf("mem_%d", s.nextID),
		Text:      text,
		UserID:    userID,
		AgentID:   agentID,
		CreatedAt: time.Now(),
	})
	return nil
}

func (s *InMemoryStore) Search(_ context.Context, query string, userID string, opts *SearchOptions) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	topK := 5
	if opts != nil && opts.TopK > 0 {
		topK = opts.TopK
	}

	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)

	var results []Memory
	for _, m := range s.memories {
		if m.UserID != userID {
			continue
		}
		if opts != nil && opts.AgentID != "" && m.AgentID != opts.AgentID {
			continue
		}
		textLower := strings.ToLower(m.Text)
		if matchesAny(textLower, queryWords) {
			results = append(results, m)
			if len(results) >= topK {
				break
			}
		}
	}
	return results, nil
}

func (s *InMemoryStore) List(_ context.Context, userID string) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []Memory
	for _, m := range s.memories {
		if m.UserID == userID {
			results = append(results, m)
		}
	}
	return results, nil
}

func (s *InMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, m := range s.memories {
		if m.ID == id {
			s.memories = append(s.memories[:i], s.memories[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("memory %q not found", id)
}

// matchesAny returns true if any query word is a substring of text.
func matchesAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}
