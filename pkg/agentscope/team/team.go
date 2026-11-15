package team

import (
	"context"
	"fmt"
	"strings"
	"sync"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// Team represents a group of agents working together under a leader.
type Team struct {
	ID          string
	Name        string
	Description string
	LeaderName  string
	members     map[string]*Member
	mu          sync.RWMutex
}

// Member is a worker agent in the team.
type Member struct {
	Name        string
	Description string
	Inbox       chan *message.Msg
	done        chan struct{}
}

// NewTeam creates a team with the given leader.
func NewTeam(name, description, leaderName string) *Team {
	return &Team{
		ID:          agentscope.GenerateID(),
		Name:        name,
		Description: description,
		LeaderName:  leaderName,
		members:     make(map[string]*Member),
	}
}

// AddMember registers a new worker in the team.
// Returns an error if the name conflicts with the leader or an existing member.
func (t *Team) AddMember(name, description string) (*Member, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if strings.EqualFold(name, t.LeaderName) {
		return nil, fmt.Errorf("member name %q conflicts with leader name", name)
	}
	if _, exists := t.members[name]; exists {
		return nil, fmt.Errorf("member %q already exists in team %q", name, t.Name)
	}

	m := &Member{
		Name:        name,
		Description: description,
		Inbox:       make(chan *message.Msg, 32),
		done:        make(chan struct{}),
	}
	t.members[name] = m
	return m, nil
}

// RemoveMember removes a member from the team.
func (t *Team) RemoveMember(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	m, ok := t.members[name]
	if !ok {
		return fmt.Errorf("member %q not found in team %q", name, t.Name)
	}
	close(m.done)
	delete(t.members, name)
	return nil
}

// GetMember returns a member by name, or nil.
func (t *Team) GetMember(name string) *Member {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.members[name]
}

// Members returns the list of member names.
func (t *Team) Members() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	names := make([]string, 0, len(t.members))
	for name := range t.members {
		names = append(names, name)
	}
	return names
}

// Send delivers a message to a specific member's inbox.
// If the recipient is the leader name, returns ErrLeaderRecipient.
func (t *Team) Send(from, to string, content string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if from == to {
		return fmt.Errorf("cannot send message to self")
	}

	msg := message.NewMsg(from, message.RoleUser, fmt.Sprintf("<team-message from=%q>%s</team-message>", from, content))

	if to == t.LeaderName {
		return ErrLeaderRecipient
	}

	m, ok := t.members[to]
	if !ok {
		return fmt.Errorf("member %q not found in team %q", to, t.Name)
	}

	select {
	case m.Inbox <- msg:
		return nil
	default:
		return fmt.Errorf("member %q inbox full", to)
	}
}

// Broadcast sends a message to all members (except the sender).
func (t *Team) Broadcast(from, content string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	msg := message.NewMsg(from, message.RoleUser, fmt.Sprintf("<team-message from=%q>%s</team-message>", from, content))

	for name, m := range t.members {
		if name == from {
			continue
		}
		select {
		case m.Inbox <- msg:
		default:
			// skip if inbox full
		}
	}
	return nil
}

// Disband removes all members and cleans up.
func (t *Team) Disband() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, m := range t.members {
		close(m.done)
	}
	t.members = make(map[string]*Member)
}

// Receive blocks until a message arrives in the member's inbox or the context is canceled.
func (m *Member) Receive(ctx context.Context) (*message.Msg, error) {
	select {
	case msg := <-m.Inbox:
		return msg, nil
	case <-m.done:
		return nil, fmt.Errorf("member removed from team")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ErrLeaderRecipient is returned when trying to route a message to the leader.
// The caller should handle leader messages through their own channel.
var ErrLeaderRecipient = fmt.Errorf("recipient is the team leader")

type teamContextKey struct{}

// WithTeam attaches a Team to a Go context.
func WithTeam(ctx context.Context, t *Team) context.Context {
	return context.WithValue(ctx, teamContextKey{}, t)
}

// GetTeam retrieves the Team from a Go context, or nil.
func GetTeam(ctx context.Context) *Team {
	v, _ := ctx.Value(teamContextKey{}).(*Team)
	return v
}
