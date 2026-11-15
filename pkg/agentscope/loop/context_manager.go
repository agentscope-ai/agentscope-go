// pkg/agentscope/loop/context_manager.go
package loop

import (
	"context"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// DefaultContextManager is a simple in-memory message buffer.
// It does not support compression -- Compress is a no-op.
type DefaultContextManager struct {
	mu       sync.RWMutex
	messages []*message.Msg
}

// NewDefaultContextManager returns a new empty DefaultContextManager.
func NewDefaultContextManager() *DefaultContextManager {
	return &DefaultContextManager{}
}

// Append adds a message to the history.
func (m *DefaultContextManager) Append(msg *message.Msg) {
	m.mu.Lock()
	m.messages = append(m.messages, msg)
	m.mu.Unlock()
}

// Messages returns a shallow copy of the message history.
func (m *DefaultContextManager) Messages() []*message.Msg {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make([]*message.Msg, len(m.messages))
	copy(cp, m.messages)
	return cp
}

// Compress is a no-op for the default manager.
func (m *DefaultContextManager) Compress(_ context.Context) error {
	return nil
}

// TokenCount returns a rough estimate of the token count across all messages.
// It uses a simple heuristic of ~4 characters per token on text blocks only.
func (m *DefaultContextManager) TokenCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, msg := range m.messages {
		for _, b := range msg.Content {
			if tb, ok := b.(message.TextBlock); ok {
				count += len(tb.Text) / 4
			}
		}
	}
	return count
}
