package app

import (
	"fmt"
	"sync"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/workspace"
)

// AgentRecord tracks a named agent within a session.
type AgentRecord struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionRecord tracks a managed session with its members.
type SessionRecord struct {
	ID           string                 `json:"id"`
	AgentName    string                 `json:"agent_name"`
	SystemPrompt string                 `json:"system_prompt"`
	ModelName    string                 `json:"model_name,omitempty"`
	Members      map[string]AgentRecord `json:"members,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`

	// ActiveSkills lists the workspace skills this session equips, by
	// skill name. Empty means no selection recorded (agent factories may
	// treat that as "all skills in the partition", Python #2283 style).
	ActiveSkills []string `json:"active_skills,omitempty"`
}

// SessionService manages session lifecycle, agent creation, and member tracking.
type SessionService struct {
	mu         sync.RWMutex
	sessions   map[string]*SessionRecord
	agents     map[string]*agent.UnifiedAgent // sessionID -> agent
	factory    AgentFactory
	wsFactory  WorkspaceAgentFactory
	wsProvider func(sessionID string) (workspace.Workspace, error)
}

// AgentFactory creates agents for sessions. Implement this to customize agent
// creation (e.g., with specific models, tools, or middleware).
type AgentFactory func(session *SessionRecord) (*agent.UnifiedAgent, error)

// WorkspaceAgentFactory creates agents with the session's workspace handed
// in, enabling filesystem-backed middleware (e.g. agentic memory rooted in
// the workspace). Parity with Python's four-argument workspace-aware agent
// middleware factories.
type WorkspaceAgentFactory func(session *SessionRecord, ws workspace.Workspace) (*agent.UnifiedAgent, error)

// NewSessionService creates a session service with the given agent factory.
func NewSessionService(factory AgentFactory) *SessionService {
	return &SessionService{
		sessions: make(map[string]*SessionRecord),
		agents:   make(map[string]*agent.UnifiedAgent),
		factory:  factory,
	}
}

// Create creates a new session.
func (s *SessionService) Create(req CreateSessionRequest) *SessionRecord {
	session := &SessionRecord{
		ID:           agentscope.GenerateID(),
		AgentName:    req.AgentName,
		SystemPrompt: req.SystemPrompt,
		ModelName:    req.ModelName,
		Members:      make(map[string]AgentRecord),
		CreatedAt:    time.Now(),
	}
	if session.AgentName == "" {
		session.AgentName = "agent"
	}

	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()
	return session
}

// Get returns a session by ID.
func (s *SessionService) Get(id string) (*SessionRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	return session, ok
}

// List returns all sessions.
func (s *SessionService) List() []*SessionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*SessionRecord, 0, len(s.sessions))
	for _, sess := range s.sessions {
		result = append(result, sess)
	}
	return result
}

// Delete removes a session and its agent.
func (s *SessionService) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	delete(s.agents, id)
	s.mu.Unlock()
}

// GetOrCreateAgent returns the agent for a session, creating it if needed.
func (s *SessionService) GetOrCreateAgent(sessionID string) (*agent.UnifiedAgent, error) {
	s.mu.RLock()
	a, ok := s.agents[sessionID]
	s.mu.RUnlock()
	if ok {
		return a, nil
	}

	s.mu.RLock()
	session, exists := s.sessions[sessionID]
	s.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	if s.factory == nil && s.wsFactory == nil {
		return nil, fmt.Errorf("no agent factory configured")
	}

	var err error
	if s.wsFactory != nil && s.wsProvider != nil {
		var ws workspace.Workspace
		ws, err = s.wsProvider(sessionID)
		if err == nil {
			a, err = s.wsFactory(session, ws)
		}
	} else {
		a, err = s.factory(session)
	}
	if err != nil {
		return nil, fmt.Errorf("create agent for session %s: %w", sessionID, err)
	}

	s.mu.Lock()
	s.agents[sessionID] = a
	// Add as primary member
	session.Members[a.Name()] = AgentRecord{
		Name:      a.Name(),
		Type:      "unified",
		CreatedAt: time.Now(),
	}
	s.mu.Unlock()
	return a, nil
}

// SetWorkspaceFactory installs a workspace-aware agent factory plus the
// provider that resolves a session's workspace (takes precedence over the
// plain AgentFactory). A nil factory or provider clears the pairing — they
// are only ever used together.
func (s *SessionService) SetWorkspaceFactory(f WorkspaceAgentFactory, provider func(sessionID string) (workspace.Workspace, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil || provider == nil {
		s.wsFactory = nil
		s.wsProvider = nil
		return
	}
	s.wsFactory = f
	s.wsProvider = provider
}

// AddMember adds a named agent to a session's member list.
func (s *SessionService) AddMember(sessionID string, a *agent.UnifiedAgent, agentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	session.Members[a.Name()] = AgentRecord{
		Name:      a.Name(),
		Type:      agentType,
		CreatedAt: time.Now(),
	}
	return nil
}

// ListMembers returns all agents in a session.
func (s *SessionService) ListMembers(sessionID string) ([]AgentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	members := make([]AgentRecord, 0, len(session.Members))
	for _, m := range session.Members {
		members = append(members, m)
	}
	return members, nil
}

// SetActiveSkills records which workspace skills the session equips.
func (s *SessionService) SetActiveSkills(sessionID string, names []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	session.ActiveSkills = append([]string(nil), names...)
	return nil
}

// ToResponse converts a SessionRecord to a SessionResponse.
func (s *SessionRecord) ToResponse() SessionResponse {
	members := make([]string, 0, len(s.Members))
	for name := range s.Members {
		members = append(members, name)
	}
	return SessionResponse{
		ID:           s.ID,
		AgentName:    s.AgentName,
		SystemPrompt: s.SystemPrompt,
		ModelName:    s.ModelName,
		Members:      members,
		CreatedAt:    s.CreatedAt,
		ActiveSkills: append([]string(nil), s.ActiveSkills...),
	}
}
