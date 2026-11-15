package storage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

// Record types for multi-entity storage.

// CredentialRecord stores a provider credential.
type CredentialRecord struct {
	ID        string            `json:"id"`
	UserID    string            `json:"user_id"`
	Provider  string            `json:"provider"`
	Data      map[string]string `json:"data"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// AgentRecord stores an agent configuration.
type AgentRecord struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Name         string    `json:"name"`
	SystemPrompt string    `json:"system_prompt"`
	ModelName    string    `json:"model_name,omitempty"`
	ToolGroups   []string  `json:"tool_groups,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ScheduleRecord stores a scheduled task configuration.
type ScheduleRecord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	SessionID string    `json:"session_id"`
	CronExpr  string    `json:"cron_expr,omitempty"`
	Input     string    `json:"input"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MessageRecord stores a chat message.
type MessageRecord struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// TeamRecord stores a team configuration.
type TeamRecord struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	LeaderID    string    `json:"leader_id"`
	MemberIDs   []string  `json:"member_ids"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SessionRecord stores a session configuration (separate from AgentState runtime data).
type SessionRecord struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	TeamID     string    `json:"team_id,omitempty"`
	ScheduleID string    `json:"schedule_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// FullStorage extends StateSaver with CRUD for credentials, agents,
// schedules, messages, and teams.
type FullStorage interface {
	agent.StateSaver

	// Credentials
	SaveCredential(ctx context.Context, record *CredentialRecord) error
	LoadCredential(ctx context.Context, userID, id string) (*CredentialRecord, error)
	ListCredentials(ctx context.Context, userID string) ([]*CredentialRecord, error)
	DeleteCredential(ctx context.Context, userID, id string) error

	// Agents
	SaveAgent(ctx context.Context, record *AgentRecord) error
	LoadAgent(ctx context.Context, userID, id string) (*AgentRecord, error)
	ListAgents(ctx context.Context, userID string) ([]*AgentRecord, error)
	DeleteAgent(ctx context.Context, userID, id string) error

	// Sessions (extended)
	SaveSession(ctx context.Context, record *SessionRecord) error
	LoadSession(ctx context.Context, id string) (*SessionRecord, error)
	SetSessionTeamID(ctx context.Context, sessionID, teamID string) error
	ListSessionsBySchedule(ctx context.Context, scheduleID string) ([]*SessionRecord, error)

	// Schedules
	SaveSchedule(ctx context.Context, record *ScheduleRecord) error
	LoadSchedule(ctx context.Context, id string) (*ScheduleRecord, error)
	ListSchedules(ctx context.Context, userID string) ([]*ScheduleRecord, error)
	ListAllSchedules(ctx context.Context) ([]*ScheduleRecord, error)
	DeleteSchedule(ctx context.Context, id string) error

	// Messages
	AppendMessage(ctx context.Context, record *MessageRecord) error
	LoadMessage(ctx context.Context, id string) (*MessageRecord, error)
	ListMessages(ctx context.Context, sessionID string) ([]*MessageRecord, error)

	// Teams
	SaveTeam(ctx context.Context, record *TeamRecord) error
	LoadTeam(ctx context.Context, id string) (*TeamRecord, error)
	ListTeams(ctx context.Context) ([]*TeamRecord, error)
	DeleteTeam(ctx context.Context, id string) error
}

// InMemoryFullStorage is a process-local implementation of FullStorage.
type InMemoryFullStorage struct {
	InMemoryStorage
	mu          sync.RWMutex
	credentials map[string]*CredentialRecord // key: userID:id
	agents      map[string]*AgentRecord      // key: userID:id
	sessions    map[string]*SessionRecord    // key: id
	schedules   map[string]*ScheduleRecord   // key: id
	messages    map[string][]*MessageRecord  // key: sessionID
	teams       map[string]*TeamRecord       // key: id
}

// NewInMemoryFullStorage creates an empty full storage.
func NewInMemoryFullStorage() *InMemoryFullStorage {
	return &InMemoryFullStorage{
		InMemoryStorage: *NewInMemoryStorage(),
		credentials:     make(map[string]*CredentialRecord),
		agents:          make(map[string]*AgentRecord),
		sessions:        make(map[string]*SessionRecord),
		schedules:       make(map[string]*ScheduleRecord),
		messages:        make(map[string][]*MessageRecord),
		teams:           make(map[string]*TeamRecord),
	}
}

func credKey(userID, id string) string { return userID + ":" + id }

// --- Credentials ---

func (s *InMemoryFullStorage) SaveCredential(_ context.Context, r *CredentialRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	s.credentials[credKey(r.UserID, r.ID)] = r
	return nil
}

func (s *InMemoryFullStorage) LoadCredential(_ context.Context, userID, id string) (*CredentialRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.credentials[credKey(userID, id)]
	if !ok {
		return nil, fmt.Errorf("credential %q not found", id)
	}
	return r, nil
}

func (s *InMemoryFullStorage) ListCredentials(_ context.Context, userID string) ([]*CredentialRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*CredentialRecord
	for _, r := range s.credentials {
		if r.UserID == userID {
			result = append(result, r)
		}
	}
	return result, nil
}

func (s *InMemoryFullStorage) DeleteCredential(_ context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.credentials, credKey(userID, id))
	return nil
}

// --- Agents ---

func (s *InMemoryFullStorage) SaveAgent(_ context.Context, r *AgentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	s.agents[credKey(r.UserID, r.ID)] = r
	return nil
}

func (s *InMemoryFullStorage) LoadAgent(_ context.Context, userID, id string) (*AgentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.agents[credKey(userID, id)]
	if !ok {
		return nil, fmt.Errorf("agent %q not found", id)
	}
	return r, nil
}

func (s *InMemoryFullStorage) ListAgents(_ context.Context, userID string) ([]*AgentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*AgentRecord
	for _, r := range s.agents {
		if r.UserID == userID {
			result = append(result, r)
		}
	}
	return result, nil
}

func (s *InMemoryFullStorage) DeleteAgent(_ context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, credKey(userID, id))
	return nil
}

// --- Schedules ---

func (s *InMemoryFullStorage) SaveSchedule(_ context.Context, r *ScheduleRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	s.schedules[r.ID] = r
	return nil
}

func (s *InMemoryFullStorage) LoadSchedule(_ context.Context, id string) (*ScheduleRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.schedules[id]
	if !ok {
		return nil, fmt.Errorf("schedule %q not found", id)
	}
	return r, nil
}

func (s *InMemoryFullStorage) ListSchedules(_ context.Context, userID string) ([]*ScheduleRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*ScheduleRecord
	for _, r := range s.schedules {
		if r.UserID == userID {
			result = append(result, r)
		}
	}
	return result, nil
}

func (s *InMemoryFullStorage) DeleteSchedule(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.schedules, id)
	return nil
}

// --- Messages ---

func (s *InMemoryFullStorage) AppendMessage(_ context.Context, r *MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	s.messages[r.SessionID] = append(s.messages[r.SessionID], r)
	return nil
}

func (s *InMemoryFullStorage) LoadMessage(_ context.Context, id string) (*MessageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, msgs := range s.messages {
		for _, m := range msgs {
			if m.ID == id {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("message %q not found", id)
}

func (s *InMemoryFullStorage) ListMessages(_ context.Context, sessionID string) ([]*MessageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.messages[sessionID], nil
}

// --- Teams ---

func (s *InMemoryFullStorage) SaveTeam(_ context.Context, r *TeamRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	s.teams[r.ID] = r
	return nil
}

func (s *InMemoryFullStorage) LoadTeam(_ context.Context, id string) (*TeamRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.teams[id]
	if !ok {
		return nil, fmt.Errorf("team %q not found", id)
	}
	return r, nil
}

func (s *InMemoryFullStorage) ListTeams(_ context.Context) ([]*TeamRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*TeamRecord, 0, len(s.teams))
	for _, r := range s.teams {
		result = append(result, r)
	}
	return result, nil
}

func (s *InMemoryFullStorage) DeleteTeam(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.teams, id)
	return nil
}

// --- Sessions ---

func (s *InMemoryFullStorage) SaveSession(_ context.Context, r *SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.UpdatedAt
	}
	s.sessions[r.ID] = r
	return nil
}

func (s *InMemoryFullStorage) LoadSession(_ context.Context, id string) (*SessionRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return r, nil
}

func (s *InMemoryFullStorage) SetSessionTeamID(_ context.Context, sessionID, teamID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %q not found", sessionID)
	}
	r.TeamID = teamID
	r.UpdatedAt = time.Now()
	return nil
}

func (s *InMemoryFullStorage) ListSessionsBySchedule(_ context.Context, scheduleID string) ([]*SessionRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*SessionRecord
	for _, r := range s.sessions {
		if r.ScheduleID == scheduleID {
			result = append(result, r)
		}
	}
	return result, nil
}

func (s *InMemoryFullStorage) ListAllSchedules(_ context.Context) ([]*ScheduleRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ScheduleRecord, 0, len(s.schedules))
	for _, r := range s.schedules {
		result = append(result, r)
	}
	return result, nil
}

// Compile-time check
var _ FullStorage = (*InMemoryFullStorage)(nil)
