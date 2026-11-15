// Package messagebus provides event pub/sub, queue, and replay log primitives
// for inter-agent and service-layer communication.
//
// The interface supports three consumption patterns:
//   - Publish/Subscribe: transient broadcast to all listeners
//   - Drain Queue: single-consumer, FIFO, ack-on-read
//   - Replay Log: append-only log that supports catch-up reads
package messagebus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// MessageBus defines the transport interface for event routing.
type MessageBus interface {
	// Publish broadcasts a payload to all active subscribers of a topic.
	Publish(ctx context.Context, topic string, payload []byte) error

	// Subscribe returns a channel that receives messages published to the topic.
	// Close the returned cancel func to unsubscribe.
	Subscribe(ctx context.Context, topic string) (<-chan []byte, func(), error)

	// QueuePush appends a payload to a named queue.
	QueuePush(ctx context.Context, queue string, payload []byte) error

	// QueueDrain reads and removes up to maxCount items from a queue.
	QueueDrain(ctx context.Context, queue string, maxCount int) ([][]byte, error)

	// QueueDelete removes a queue entirely.
	QueueDelete(ctx context.Context, queue string) error

	// LogAppend appends a payload to a named replay log, returning the entry ID.
	LogAppend(ctx context.Context, log string, payload []byte, maxLen int) (string, error)

	// LogRead reads log entries with ID greater than sinceID (exclusive).
	// Pass "" for sinceID to read from the beginning.
	LogRead(ctx context.Context, log string, sinceID string, maxCount int) ([]LogEntry, error)

	// RegistrySet sets a field in a hash-like registry key.
	RegistrySet(key, field string, value []byte) error

	// RegistryGet retrieves a single field from a registry key.
	RegistryGet(key, field string) ([]byte, error)

	// RegistryGetAll retrieves all fields from a registry key.
	RegistryGetAll(key string) (map[string][]byte, error)

	// RegistryDel removes one or more fields from a registry key.
	RegistryDel(key string, fields ...string) error

	// RegistryDrop removes an entire registry key and all its fields.
	RegistryDrop(key string) error

	// Close shuts down the bus and releases resources.
	Close() error
}

// LogEntry is a single entry in a replay log.
type LogEntry struct {
	ID      string `json:"id"`
	Payload []byte `json:"payload"`
}

// InMemoryMessageBus is a process-local implementation suitable for
// development and testing. Not safe for multi-process deployments.
type InMemoryMessageBus struct {
	mu          sync.RWMutex
	subscribers map[string]map[uint64]chan []byte
	queues      map[string][][]byte
	logs        map[string][]LogEntry
	registry    map[string]map[string][]byte
	nextSubID   uint64
	nextLogSeq  atomic.Int64
	closed      bool
}

// NewInMemoryMessageBus creates an in-memory bus.
func NewInMemoryMessageBus() *InMemoryMessageBus {
	return &InMemoryMessageBus{
		subscribers: make(map[string]map[uint64]chan []byte),
		queues:      make(map[string][][]byte),
		logs:        make(map[string][]LogEntry),
		registry:    make(map[string]map[string][]byte),
	}
}

func (b *InMemoryMessageBus) Publish(_ context.Context, topic string, payload []byte) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return fmt.Errorf("messagebus: closed")
	}

	subs := b.subscribers[topic]
	for _, ch := range subs {
		select {
		case ch <- copyBytes(payload):
		default:
			// subscriber too slow, drop
		}
	}
	return nil
}

func (b *InMemoryMessageBus) Subscribe(_ context.Context, topic string) (<-chan []byte, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, nil, fmt.Errorf("messagebus: closed")
	}

	ch := make(chan []byte, 64)
	b.nextSubID++
	id := b.nextSubID

	if b.subscribers[topic] == nil {
		b.subscribers[topic] = make(map[uint64]chan []byte)
	}
	b.subscribers[topic][id] = ch

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subscribers[topic], id)
		close(ch)
	}

	return ch, cancel, nil
}

func (b *InMemoryMessageBus) QueuePush(_ context.Context, queue string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("messagebus: closed")
	}

	b.queues[queue] = append(b.queues[queue], copyBytes(payload))
	return nil
}

func (b *InMemoryMessageBus) QueueDrain(_ context.Context, queue string, maxCount int) ([][]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("messagebus: closed")
	}

	items := b.queues[queue]
	if len(items) == 0 {
		return nil, nil
	}

	n := maxCount
	if n <= 0 || n > len(items) {
		n = len(items)
	}

	drained := make([][]byte, n)
	copy(drained, items[:n])
	b.queues[queue] = items[n:]

	if len(b.queues[queue]) == 0 {
		delete(b.queues, queue)
	}

	return drained, nil
}

func (b *InMemoryMessageBus) QueueDelete(_ context.Context, queue string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.queues, queue)
	return nil
}

func (b *InMemoryMessageBus) LogAppend(_ context.Context, logName string, payload []byte, maxLen int) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return "", fmt.Errorf("messagebus: closed")
	}

	seq := b.nextLogSeq.Add(1)
	id := fmt.Sprintf("%d-0", seq)

	entry := LogEntry{ID: id, Payload: copyBytes(payload)}
	b.logs[logName] = append(b.logs[logName], entry)

	if maxLen > 0 && len(b.logs[logName]) > maxLen {
		b.logs[logName] = b.logs[logName][len(b.logs[logName])-maxLen:]
	}

	return id, nil
}

// LogRead returns replay entries with payloads owned by the caller.
func (b *InMemoryMessageBus) LogRead(_ context.Context, logName string, sinceID string, maxCount int) ([]LogEntry, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entries := b.logs[logName]
	if len(entries) == 0 {
		return nil, nil
	}

	startIdx := 0
	if sinceID != "" {
		for i, e := range entries {
			if e.ID == sinceID {
				startIdx = i + 1
				break
			}
		}
	}

	if startIdx >= len(entries) {
		return nil, nil
	}

	result := entries[startIdx:]
	if maxCount > 0 && len(result) > maxCount {
		result = result[:maxCount]
	}

	out := make([]LogEntry, len(result))
	for i, entry := range result {
		out[i] = LogEntry{ID: entry.ID, Payload: copyBytes(entry.Payload)}
	}
	return out, nil
}

func (b *InMemoryMessageBus) RegistrySet(key, field string, value []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("messagebus: closed")
	}

	if b.registry[key] == nil {
		b.registry[key] = make(map[string][]byte)
	}
	b.registry[key][field] = copyBytes(value)
	return nil
}

func (b *InMemoryMessageBus) RegistryGet(key, field string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return nil, fmt.Errorf("messagebus: closed")
	}

	fields := b.registry[key]
	if fields == nil {
		return nil, nil
	}
	v, ok := fields[field]
	if !ok {
		return nil, nil
	}
	return copyBytes(v), nil
}

func (b *InMemoryMessageBus) RegistryGetAll(key string) (map[string][]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return nil, fmt.Errorf("messagebus: closed")
	}

	fields := b.registry[key]
	if fields == nil {
		return nil, nil
	}

	result := make(map[string][]byte, len(fields))
	for k, v := range fields {
		result[k] = copyBytes(v)
	}
	return result, nil
}

func (b *InMemoryMessageBus) RegistryDel(key string, fields ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("messagebus: closed")
	}

	bucket := b.registry[key]
	if bucket == nil {
		return nil
	}
	for _, f := range fields {
		delete(bucket, f)
	}
	if len(bucket) == 0 {
		delete(b.registry, key)
	}
	return nil
}

func (b *InMemoryMessageBus) RegistryDrop(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("messagebus: closed")
	}

	delete(b.registry, key)
	return nil
}

func (b *InMemoryMessageBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}
	b.closed = true

	for topic, subs := range b.subscribers {
		for id, ch := range subs {
			close(ch)
			delete(subs, id)
		}
		delete(b.subscribers, topic)
	}

	return nil
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// Compile-time interface check.
var _ MessageBus = (*InMemoryMessageBus)(nil)
