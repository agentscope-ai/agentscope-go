package messagebus

import (
	"context"
	"fmt"
	"time"
)

// RedisMessageBusConfig configures a Redis-backed message bus.
type RedisMessageBusConfig struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string // default: "agentscope:bus"

	// Dial returns a RedisBusClient implementation.
	Dial func(cfg RedisMessageBusConfig) (RedisBusClient, error)
}

// RedisBusClient is the minimal Redis interface needed by RedisMessageBus.
type RedisBusClient interface {
	// Pub/Sub
	Publish(ctx context.Context, channel string, message []byte) error
	Subscribe(ctx context.Context, channel string) (<-chan []byte, error)

	// List (FIFO queue)
	LPush(ctx context.Context, key string, value []byte) error
	BRPop(ctx context.Context, key string, timeout time.Duration) ([]byte, error)

	// Stream (append-only log)
	XAdd(ctx context.Context, stream string, values map[string]any) (string, error)
	XRange(ctx context.Context, stream string, start, end string) ([]StreamEntry, error)

	// Hash (registry)
	HSet(ctx context.Context, key, field string, value []byte) error
	HGet(ctx context.Context, key, field string) ([]byte, error)
	HGetAll(ctx context.Context, key string) (map[string][]byte, error)
	HDel(ctx context.Context, key string, fields ...string) error
	Del(ctx context.Context, keys ...string) error

	Close() error
}

// StreamEntry represents a Redis Stream entry.
type StreamEntry struct {
	ID     string
	Values map[string]string
}

// RedisMessageBus implements MessageBus backed by Redis.
type RedisMessageBus struct {
	client    RedisBusClient
	keyPrefix string
}

// NewRedisMessageBus creates a Redis-backed message bus.
func NewRedisMessageBus(cfg RedisMessageBusConfig) (*RedisMessageBus, error) {
	if cfg.Dial == nil {
		return nil, fmt.Errorf("redis message bus: Dial function is required")
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "agentscope:bus"
	}

	client, err := cfg.Dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("redis message bus: dial failed: %w", err)
	}

	return &RedisMessageBus{
		client:    client,
		keyPrefix: cfg.KeyPrefix,
	}, nil
}

// --- Pub/Sub (transient broadcast) ---

func (b *RedisMessageBus) Publish(ctx context.Context, topic string, data []byte) error {
	channel := b.keyPrefix + ":pubsub:" + topic
	return b.client.Publish(ctx, channel, data)
}

func (b *RedisMessageBus) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	channel := b.keyPrefix + ":pubsub:" + topic
	return b.client.Subscribe(ctx, channel)
}

// --- Queue (FIFO single-consumer) ---

func (b *RedisMessageBus) QueuePush(ctx context.Context, queue string, data []byte) error {
	key := b.keyPrefix + ":queue:" + queue
	return b.client.LPush(ctx, key, data)
}

func (b *RedisMessageBus) QueueDrain(ctx context.Context, queue string, timeout time.Duration) ([]byte, error) {
	key := b.keyPrefix + ":queue:" + queue
	return b.client.BRPop(ctx, key, timeout)
}

// --- Log (append-only replay) ---

func (b *RedisMessageBus) LogAppend(ctx context.Context, log string, data []byte) (string, error) {
	stream := b.keyPrefix + ":log:" + log
	return b.client.XAdd(ctx, stream, map[string]any{"data": string(data)})
}

func (b *RedisMessageBus) LogRead(ctx context.Context, log string, fromID string) ([][]byte, error) {
	stream := b.keyPrefix + ":log:" + log
	if fromID == "" {
		fromID = "0"
	}
	entries, err := b.client.XRange(ctx, stream, fromID, "+")
	if err != nil {
		return nil, err
	}
	var results [][]byte
	for _, e := range entries {
		if data, ok := e.Values["data"]; ok {
			results = append(results, []byte(data))
		}
	}
	return results, nil
}

// --- Registry (hash-based key-value) ---

func (b *RedisMessageBus) RegistrySet(key, field string, value []byte) error {
	k := b.keyPrefix + ":reg:" + key
	return b.client.HSet(context.Background(), k, field, value)
}

func (b *RedisMessageBus) RegistryGet(key, field string) ([]byte, error) {
	k := b.keyPrefix + ":reg:" + key
	return b.client.HGet(context.Background(), k, field)
}

func (b *RedisMessageBus) RegistryGetAll(key string) (map[string][]byte, error) {
	k := b.keyPrefix + ":reg:" + key
	return b.client.HGetAll(context.Background(), k)
}

func (b *RedisMessageBus) RegistryDel(key string, fields ...string) error {
	k := b.keyPrefix + ":reg:" + key
	return b.client.HDel(context.Background(), k, fields...)
}

func (b *RedisMessageBus) RegistryDrop(key string) error {
	k := b.keyPrefix + ":reg:" + key
	return b.client.Del(context.Background(), k)
}

// Close closes the underlying Redis client.
func (b *RedisMessageBus) Close() error {
	return b.client.Close()
}
