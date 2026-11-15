package memory

import (
	"context"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Store abstracts message memory storage.
type Store interface {
	Save(ctx context.Context, key string, msg *message.Msg) error
	Load(ctx context.Context, key string) ([]*message.Msg, error)
}

// InMemoryStore is a simple in-memory Store implementation.
type InMemoryStore struct {
	mu   sync.RWMutex
	data map[string][]*message.Msg
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		data: make(map[string][]*message.Msg),
	}
}

func (s *InMemoryStore) Save(_ context.Context, key string, msg *message.Msg) error {
	if msg == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = append(s.data[key], msg)
	return nil
}

func (s *InMemoryStore) Load(_ context.Context, key string) ([]*message.Msg, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]*message.Msg, len(s.data[key]))
	copy(cp, s.data[key])
	return cp, nil
}
