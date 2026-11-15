package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

// RedisFullStorage implements FullStorage backed by Redis.
// Records are stored as JSON values under structured key prefixes:
//
//	{prefix}:session:{id}       — AgentState
//	{prefix}:cred:{userID}:{id} — CredentialRecord
//	{prefix}:agent:{userID}:{id} — AgentRecord
//	{prefix}:sess:{id}          — SessionRecord
//	{prefix}:sched:{id}         — ScheduleRecord
//	{prefix}:msg:{sessionID}:{id} — MessageRecord
//	{prefix}:msgidx:{sessionID} — message ID list (JSON array)
//	{prefix}:team:{id}          — TeamRecord
type RedisFullStorage struct {
	client    RedisClient
	keyPrefix string
	ttl       time.Duration
	mu        sync.Mutex
}

// NewRedisFullStorage creates a RedisFullStorage using the same RedisConfig and
// RedisClient interface as RedisStorage. The Dial function in cfg is called to
// create the connection.
func NewRedisFullStorage(cfg RedisConfig) (*RedisFullStorage, error) {
	if cfg.Dial == nil {
		return nil, fmt.Errorf("redis full storage: Dial function is required")
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "agentscope"
	}
	client, err := cfg.Dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("redis full storage: dial failed: %w", err)
	}
	return &RedisFullStorage{
		client:    client,
		keyPrefix: cfg.KeyPrefix,
		ttl:       cfg.TTL,
	}, nil
}

// Close closes the underlying Redis connection.
func (s *RedisFullStorage) Close() error { return s.client.Close() }

// ---------- helpers ----------

func (s *RedisFullStorage) key(parts ...string) string {
	return s.keyPrefix + ":" + strings.Join(parts, ":")
}

func (s *RedisFullStorage) putJSON(ctx context.Context, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("redis: marshal: %w", err)
	}
	return s.client.Set(ctx, key, data, s.ttl)
}

func getJSON[T any](ctx context.Context, client RedisClient, key string) (*T, error) {
	data, err := client.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("redis: unmarshal: %w", err)
	}
	return &v, nil
}

func scanJSON[T any](ctx context.Context, client RedisClient, pattern string) ([]*T, error) {
	keys, err := client.Scan(ctx, pattern)
	if err != nil {
		return nil, fmt.Errorf("redis: scan: %w", err)
	}
	result := make([]*T, 0, len(keys))
	for _, k := range keys {
		v, err := getJSON[T](ctx, client, k)
		if err != nil {
			continue // skip corrupted entries
		}
		result = append(result, v)
	}
	return result, nil
}

// ---------- StateSaver ----------

func (s *RedisFullStorage) SaveState(ctx context.Context, sessionID string, state *agent.AgentState) error {
	return s.putJSON(ctx, s.key("session", sessionID), state)
}

func (s *RedisFullStorage) LoadState(ctx context.Context, sessionID string) (*agent.AgentState, error) {
	v, err := getJSON[agent.AgentState](ctx, s.client, s.key("session", sessionID))
	if err != nil {
		return nil, fmt.Errorf("redis: load state %q: %w", sessionID, err)
	}
	return v, nil
}

func (s *RedisFullStorage) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	keys, err := s.client.Scan(ctx, s.key("session", "*"))
	if err != nil {
		return nil, fmt.Errorf("redis: scan sessions: %w", err)
	}
	infos := make([]agent.SessionInfo, 0, len(keys))
	for _, k := range keys {
		id := strings.TrimPrefix(k, s.key("session")+":")
		infos = append(infos, agent.SessionInfo{SessionID: id})
	}
	return infos, nil
}

func (s *RedisFullStorage) DeleteSession(ctx context.Context, sessionID string) error {
	return s.client.Del(ctx, s.key("session", sessionID))
}

// ---------- Credentials ----------

func (s *RedisFullStorage) SaveCredential(ctx context.Context, r *CredentialRecord) error {
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	return s.putJSON(ctx, s.key("cred", r.UserID, r.ID), r)
}

func (s *RedisFullStorage) LoadCredential(ctx context.Context, userID, id string) (*CredentialRecord, error) {
	v, err := getJSON[CredentialRecord](ctx, s.client, s.key("cred", userID, id))
	if err != nil {
		return nil, fmt.Errorf("credential %q not found: %w", id, err)
	}
	return v, nil
}

func (s *RedisFullStorage) ListCredentials(ctx context.Context, userID string) ([]*CredentialRecord, error) {
	return scanJSON[CredentialRecord](ctx, s.client, s.key("cred", userID, "*"))
}

func (s *RedisFullStorage) DeleteCredential(ctx context.Context, userID, id string) error {
	return s.client.Del(ctx, s.key("cred", userID, id))
}

// ---------- Agents ----------

func (s *RedisFullStorage) SaveAgent(ctx context.Context, r *AgentRecord) error {
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	return s.putJSON(ctx, s.key("agent", r.UserID, r.ID), r)
}

func (s *RedisFullStorage) LoadAgent(ctx context.Context, userID, id string) (*AgentRecord, error) {
	v, err := getJSON[AgentRecord](ctx, s.client, s.key("agent", userID, id))
	if err != nil {
		return nil, fmt.Errorf("agent %q not found: %w", id, err)
	}
	return v, nil
}

func (s *RedisFullStorage) ListAgents(ctx context.Context, userID string) ([]*AgentRecord, error) {
	return scanJSON[AgentRecord](ctx, s.client, s.key("agent", userID, "*"))
}

func (s *RedisFullStorage) DeleteAgent(ctx context.Context, userID, id string) error {
	return s.client.Del(ctx, s.key("agent", userID, id))
}

// ---------- Sessions (extended) ----------

func (s *RedisFullStorage) SaveSession(ctx context.Context, r *SessionRecord) error {
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	return s.putJSON(ctx, s.key("sess", r.ID), r)
}

func (s *RedisFullStorage) LoadSession(ctx context.Context, id string) (*SessionRecord, error) {
	v, err := getJSON[SessionRecord](ctx, s.client, s.key("sess", id))
	if err != nil {
		return nil, fmt.Errorf("session record %q not found: %w", id, err)
	}
	return v, nil
}

func (s *RedisFullStorage) SetSessionTeamID(ctx context.Context, sessionID, teamID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.LoadSession(ctx, sessionID)
	if err != nil {
		return err
	}
	r.TeamID = teamID
	r.UpdatedAt = time.Now()
	return s.putJSON(ctx, s.key("sess", r.ID), r)
}

func (s *RedisFullStorage) ListSessionsBySchedule(ctx context.Context, scheduleID string) ([]*SessionRecord, error) {
	all, err := scanJSON[SessionRecord](ctx, s.client, s.key("sess", "*"))
	if err != nil {
		return nil, err
	}
	var result []*SessionRecord
	for _, r := range all {
		if r.ScheduleID == scheduleID {
			result = append(result, r)
		}
	}
	return result, nil
}

// ---------- Schedules ----------

func (s *RedisFullStorage) SaveSchedule(ctx context.Context, r *ScheduleRecord) error {
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	return s.putJSON(ctx, s.key("sched", r.ID), r)
}

func (s *RedisFullStorage) LoadSchedule(ctx context.Context, id string) (*ScheduleRecord, error) {
	v, err := getJSON[ScheduleRecord](ctx, s.client, s.key("sched", id))
	if err != nil {
		return nil, fmt.Errorf("schedule %q not found: %w", id, err)
	}
	return v, nil
}

func (s *RedisFullStorage) ListSchedules(ctx context.Context, userID string) ([]*ScheduleRecord, error) {
	all, err := scanJSON[ScheduleRecord](ctx, s.client, s.key("sched", "*"))
	if err != nil {
		return nil, err
	}
	var result []*ScheduleRecord
	for _, r := range all {
		if r.UserID == userID {
			result = append(result, r)
		}
	}
	return result, nil
}

func (s *RedisFullStorage) ListAllSchedules(ctx context.Context) ([]*ScheduleRecord, error) {
	return scanJSON[ScheduleRecord](ctx, s.client, s.key("sched", "*"))
}

func (s *RedisFullStorage) DeleteSchedule(ctx context.Context, id string) error {
	return s.client.Del(ctx, s.key("sched", id))
}

// ---------- Messages ----------

func (s *RedisFullStorage) AppendMessage(ctx context.Context, r *MessageRecord) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	// Store individual message
	if err := s.putJSON(ctx, s.key("msg", r.SessionID, r.ID), r); err != nil {
		return err
	}
	// Store reverse-lookup key for O(1) LoadMessage by ID
	revKey := s.key("msgrev", r.ID)
	fullKey := s.key("msg", r.SessionID, r.ID)
	if err := s.client.Set(ctx, revKey, []byte(fullKey), s.ttl); err != nil {
		return err
	}
	// Maintain ordered ID index per session (lock to prevent race on read-modify-write)
	s.mu.Lock()
	defer s.mu.Unlock()
	idxKey := s.key("msgidx", r.SessionID)
	var ids []string
	data, err := s.client.Get(ctx, idxKey)
	if err == nil {
		_ = json.Unmarshal(data, &ids)
	}
	ids = append(ids, r.ID)
	return s.putJSON(ctx, idxKey, ids)
}

func (s *RedisFullStorage) LoadMessage(ctx context.Context, id string) (*MessageRecord, error) {
	// Use reverse-lookup key for O(1) access by message ID.
	revKey := s.key("msgrev", id)
	fullKeyData, err := s.client.Get(ctx, revKey)
	if err == nil && len(fullKeyData) > 0 {
		return getJSON[MessageRecord](ctx, s.client, string(fullKeyData))
	}
	// Fallback: scan all message keys (for messages stored before the reverse index).
	keys, err := s.client.Scan(ctx, s.key("msg", "*"))
	if err != nil {
		return nil, fmt.Errorf("redis: scan message: %w", err)
	}
	idxPrefix := s.key("msgidx")
	suffix := ":" + id
	for _, k := range keys {
		if strings.HasPrefix(k, idxPrefix) {
			continue
		}
		if strings.HasSuffix(k, suffix) {
			return getJSON[MessageRecord](ctx, s.client, k)
		}
	}
	return nil, fmt.Errorf("message %q not found", id)
}

func (s *RedisFullStorage) ListMessages(ctx context.Context, sessionID string) ([]*MessageRecord, error) {
	idxKey := s.key("msgidx", sessionID)
	data, err := s.client.Get(ctx, idxKey)
	if err != nil {
		return nil, nil // no messages yet
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, nil
	}
	result := make([]*MessageRecord, 0, len(ids))
	for _, id := range ids {
		r, err := getJSON[MessageRecord](ctx, s.client, s.key("msg", sessionID, id))
		if err != nil {
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

// ---------- Teams ----------

func (s *RedisFullStorage) SaveTeam(ctx context.Context, r *TeamRecord) error {
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	return s.putJSON(ctx, s.key("team", r.ID), r)
}

func (s *RedisFullStorage) LoadTeam(ctx context.Context, id string) (*TeamRecord, error) {
	v, err := getJSON[TeamRecord](ctx, s.client, s.key("team", id))
	if err != nil {
		return nil, fmt.Errorf("team %q not found: %w", id, err)
	}
	return v, nil
}

func (s *RedisFullStorage) ListTeams(ctx context.Context) ([]*TeamRecord, error) {
	return scanJSON[TeamRecord](ctx, s.client, s.key("team", "*"))
}

func (s *RedisFullStorage) DeleteTeam(ctx context.Context, id string) error {
	return s.client.Del(ctx, s.key("team", id))
}

// Compile-time check.
var _ FullStorage = (*RedisFullStorage)(nil)
