package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// agentStateJSON is the JSON-friendly representation of AgentState.
type agentStateJSON struct {
	ReplyRecovery   *ReplyRecoveryState `json:"reply_recovery,omitempty"`
	SchemaVersion   int                 `json:"schema_version,omitempty"`
	SessionID       string              `json:"session_id"`
	Context         []*message.Msg      `json:"context"`
	Summary         string              `json:"summary,omitempty"`
	ReplyID         string              `json:"reply_id"`
	CurIter         int                 `json:"cur_iter"`
	PermissionCtx   *permission.Context `json:"permission_context,omitempty"`
	ToolCtx         *ToolStateContext   `json:"tool_context,omitempty"`
	TasksCtx        *TasksStateContext  `json:"tasks_context,omitempty"`
	MiddlewareState map[string]any      `json:"middleware_context,omitempty"`
	ReadCacheData   json.RawMessage     `json:"read_cache_data,omitempty"`
}

// MarshalJSON serializes AgentState to JSON.
func (s *AgentState) MarshalJSON() ([]byte, error) {
	return json.Marshal(agentStateJSON{
		SchemaVersion:   s.SchemaVersion,
		ReplyRecovery:   s.ReplyRecovery,
		SessionID:       s.SessionID,
		Context:         s.Context,
		Summary:         s.Summary,
		ReplyID:         s.ReplyID,
		CurIter:         s.CurIter,
		PermissionCtx:   s.PermissionCtx,
		ToolCtx:         s.ToolCtx,
		TasksCtx:        s.TasksCtx,
		MiddlewareState: s.MiddlewareState,
		ReadCacheData:   s.ReadCacheData,
	})
}

// UnmarshalJSON deserializes AgentState from JSON.
func (s *AgentState) UnmarshalJSON(data []byte) error {
	var raw agentStateJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal agent state: %w", err)
	}
	s.SchemaVersion = raw.SchemaVersion
	s.ReplyRecovery = raw.ReplyRecovery
	s.SessionID = raw.SessionID
	s.Context = raw.Context
	s.Summary = raw.Summary
	s.ReplyID = raw.ReplyID
	s.CurIter = raw.CurIter
	s.PermissionCtx = raw.PermissionCtx
	s.ToolCtx = raw.ToolCtx
	s.TasksCtx = raw.TasksCtx
	s.MiddlewareState = raw.MiddlewareState
	s.ReadCacheData = raw.ReadCacheData
	return nil
}

// StateSaver persists and retrieves agent states.
// Concrete implementations (file, Redis, etc.) are planned for Phase 4.
type StateSaver interface {
	SaveState(ctx context.Context, sessionID string, state *AgentState) error
	LoadState(ctx context.Context, sessionID string) (*AgentState, error)
	ListSessions(ctx context.Context) ([]SessionInfo, error)
	DeleteSession(ctx context.Context, sessionID string) error
}

// SessionInfo summarizes a persisted session.
type SessionInfo struct {
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name,omitempty"`
	Summary   string `json:"summary,omitempty"`
}
