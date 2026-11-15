package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
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
	Plan            []tool.PlanItem     `json:"plan,omitempty"`
	SummaryOffer    *SummaryOffer       `json:"summary_offer,omitempty"`
	// Thread state (schema version 3).
	PeerSummary  *PeerSummary `json:"peer_summary,omitempty"`
	ThreadCursor uint64       `json:"thread_cursor,omitempty"`
}

// MarshalJSON serializes AgentState to JSON.
func (s *AgentState) MarshalJSON() ([]byte, error) {
	return json.Marshal(agentStateJSON{
		ReplyRecovery:   s.ReplyRecovery,
		SchemaVersion:   s.SchemaVersion,
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
		Plan:            s.Plan,
		SummaryOffer:    s.SummaryOffer,
		PeerSummary:     s.PeerSummary,
		ThreadCursor:    s.ThreadCursor,
	})
}

// usesThreadState reports whether any thread field is set. Such a state needs
// schema version 3: an older loader would silently drop the cursor and could
// re-import entries the agent already holds.
func (s *AgentState) usesThreadState() bool {
	return s.PeerSummary != nil || s.ThreadCursor != 0
}

// checkpointSchemaVersion returns the lowest schema version able to carry s:
// 1 by default, 2 with reply-recovery counters, 3 with thread state.
func (s *AgentState) checkpointSchemaVersion() int {
	switch {
	case s.usesThreadState():
		return 3
	case s.ReplyRecovery != nil:
		return 2
	default:
		return 1
	}
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
	s.Plan = raw.Plan
	s.SummaryOffer = raw.SummaryOffer
	s.PeerSummary = raw.PeerSummary
	s.ThreadCursor = raw.ThreadCursor
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
