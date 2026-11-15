package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

// RedisConfig configures a Redis-backed storage.
type RedisConfig struct {
	Addr      string // "host:port"
	Password  string
	DB        int
	KeyPrefix string        // default: "agentscope"
	TTL       time.Duration // 0 = no expiry

	// Dial is a factory that returns a RedisClient.
	// This allows users to inject any Redis client implementation
	// (e.g. go-redis, miniredis) without importing a specific library.
	Dial func(cfg RedisConfig) (RedisClient, error)
}

// RedisClient is a minimal interface for Redis operations.
// Users can implement this with go-redis or any other client.
type RedisClient interface {
	Set(ctx context.Context, key string, value []byte, expiration time.Duration) error
	Get(ctx context.Context, key string) ([]byte, error)
	Del(ctx context.Context, key string) error
	Scan(ctx context.Context, pattern string) ([]string, error)
	Close() error
}

// RedisStorage implements agent.StateSaver backed by Redis.
type RedisStorage struct {
	client    RedisClient
	keyPrefix string
	ttl       time.Duration
}

// NewRedisStorage creates a Redis-backed storage using the provided Dial function.
func NewRedisStorage(cfg RedisConfig) (*RedisStorage, error) {
	if cfg.Dial == nil {
		return nil, fmt.Errorf("redis storage: Dial function is required")
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "agentscope"
	}

	client, err := cfg.Dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("redis storage: dial failed: %w", err)
	}

	return &RedisStorage{
		client:    client,
		keyPrefix: cfg.KeyPrefix,
		ttl:       cfg.TTL,
	}, nil
}

func (s *RedisStorage) SaveState(ctx context.Context, sessionID string, state *agent.AgentState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("redis: marshal state: %w", err)
	}
	key := s.sessionKey(sessionID)
	return s.client.Set(ctx, key, data, s.ttl)
}

func (s *RedisStorage) LoadState(ctx context.Context, sessionID string) (*agent.AgentState, error) {
	key := s.sessionKey(sessionID)
	data, err := s.client.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("redis: get state: %w", err)
	}
	var state agent.AgentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("redis: unmarshal state: %w", err)
	}
	return &state, nil
}

func (s *RedisStorage) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	pattern := s.keyPrefix + ":session:*"
	keys, err := s.client.Scan(ctx, pattern)
	if err != nil {
		return nil, fmt.Errorf("redis: scan sessions: %w", err)
	}

	var sessions []agent.SessionInfo
	for _, key := range keys {
		sessionID := strings.TrimPrefix(key, s.keyPrefix+":session:")
		sessions = append(sessions, agent.SessionInfo{SessionID: sessionID})
	}
	return sessions, nil
}

func (s *RedisStorage) DeleteSession(ctx context.Context, sessionID string) error {
	key := s.sessionKey(sessionID)
	return s.client.Del(ctx, key)
}

// Close closes the underlying Redis client.
func (s *RedisStorage) Close() error {
	return s.client.Close()
}

func (s *RedisStorage) sessionKey(sessionID string) string {
	return s.keyPrefix + ":session:" + sessionID
}

// Compile-time check.
var _ agent.StateSaver = (*RedisStorage)(nil)
