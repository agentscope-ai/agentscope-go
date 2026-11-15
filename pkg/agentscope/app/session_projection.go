package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus"
)

// SessionProjection mirrors UI cards from one session onto another session's
// event stream. This enables features like team HITL where a worker session's
// permission prompt needs to be visible to the leader session.
type SessionProjection struct {
	bus      messagebus.MessageBus
	fallback sync.Map // fallback in-memory store: registryKey -> map[field][]byte
}

// NewSessionProjection creates a new session projection.
func NewSessionProjection(bus messagebus.MessageBus) *SessionProjection {
	return &SessionProjection{bus: bus}
}

// ProjectedEntry is a UI card projected from a source session onto a target.
type ProjectedEntry struct {
	EntryID         string `json:"entry_id"`
	Kind            string `json:"kind"` // e.g., "hitl", "progress", "error"
	SourceSessionID string `json:"source_session_id"`
	Payload         any    `json:"payload"`
}

// Publish projects an entry onto the target session.
func (p *SessionProjection) Publish(targetSessionID string, entry ProjectedEntry) error {
	if p.bus == nil {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	evtData, err2 := json.Marshal(map[string]any{
		"type": "custom",
		"name": "session_projection." + entry.Kind,
		"data": json.RawMessage(data),
	})
	if err2 != nil {
		return err2
	}
	return p.bus.Publish(context.TODO(), "session:"+targetSessionID+":events", evtData)
}

// Set stores a projected entry in the target session's projection feed.
func (p *SessionProjection) Set(targetSessionID, kind, entryID string, payload any) error {
	entry := ProjectedEntry{
		EntryID: entryID,
		Kind:    kind,
		Payload: payload,
	}
	return p.Publish(targetSessionID, entry)
}

// Remove removes a projected entry from the target session's feed.
func (p *SessionProjection) Remove(targetSessionID, kind, entryID string) error {
	entry := ProjectedEntry{
		EntryID: entryID,
		Kind:    kind,
		Payload: nil, // nil payload signals removal
	}
	return p.Publish(targetSessionID, entry)
}

// registryKey builds the registry key for a projection kind within a session.
func registryKey(targetSID, kind string) string {
	return "projection:" + targetSID + ":" + kind
}

// SetEntry stores a projection entry. If the MessageBus is nil, it falls back
// to an in-memory sync.Map.
func (p *SessionProjection) SetEntry(targetSID, kind, entryID string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	key := registryKey(targetSID, kind)

	if p.bus != nil {
		return p.bus.RegistrySet(key, entryID, data)
	}

	// Fallback to in-memory.
	actual, _ := p.fallback.LoadOrStore(key, &sync.Map{})
	actual.(*sync.Map).Store(entryID, data)
	return nil
}

// DeleteEntry removes a single projection entry.
func (p *SessionProjection) DeleteEntry(targetSID, kind, entryID string) error {
	key := registryKey(targetSID, kind)

	if p.bus != nil {
		return p.bus.RegistryDel(key, entryID)
	}

	if v, ok := p.fallback.Load(key); ok {
		v.(*sync.Map).Delete(entryID)
	}
	return nil
}

// ListEntries returns all entries for a given session and kind. Each entry is
// a map deserialized from the stored JSON payload.
func (p *SessionProjection) ListEntries(targetSID, kind string) (map[string]map[string]any, error) {
	key := registryKey(targetSID, kind)

	if p.bus != nil {
		raw, err := p.bus.RegistryGetAll(key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, nil
		}
		result := make(map[string]map[string]any, len(raw))
		for field, data := range raw {
			var m map[string]any
			if err2 := json.Unmarshal(data, &m); err2 != nil {
				continue // skip corrupted entries
			}
			result[field] = m
		}
		return result, nil
	}

	// Fallback.
	v, ok := p.fallback.Load(key)
	if !ok {
		return nil, nil
	}
	result := make(map[string]map[string]any)
	v.(*sync.Map).Range(func(k, val any) bool {
		var m map[string]any
		if err := json.Unmarshal(val.([]byte), &m); err == nil {
			result[k.(string)] = m
		}
		return true
	})
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// PurgeEntries removes all entries for a given session and kind.
func (p *SessionProjection) PurgeEntries(targetSID, kind string) error {
	key := registryKey(targetSID, kind)

	if p.bus != nil {
		return p.bus.RegistryDrop(key)
	}

	p.fallback.Delete(key)
	return nil
}

// SubagentHitlProjector projects HITL permission requests from sub-agent
// (worker) sessions onto the parent (leader) session. This allows a client
// watching the leader session to see and respond to worker permission prompts.
type SubagentHitlProjector struct {
	projection *SessionProjection
	mu         sync.RWMutex
	mappings   map[string]string // workerSessionID -> leaderSessionID
}

// NewSubagentHitlProjector creates a projector.
func NewSubagentHitlProjector(projection *SessionProjection) *SubagentHitlProjector {
	return &SubagentHitlProjector{
		projection: projection,
		mappings:   make(map[string]string),
	}
}

// Register maps a worker session to its leader session.
func (p *SubagentHitlProjector) Register(workerSessionID, leaderSessionID string) {
	p.mu.Lock()
	p.mappings[workerSessionID] = leaderSessionID
	p.mu.Unlock()
}

// Unregister removes a worker-to-leader mapping.
func (p *SubagentHitlProjector) Unregister(workerSessionID string) {
	p.mu.Lock()
	delete(p.mappings, workerSessionID)
	p.mu.Unlock()
}

// ProjectConfirmRequest projects a RequireUserConfirmEvent from a worker
// onto the leader session so the leader's client can respond.
func (p *SubagentHitlProjector) ProjectConfirmRequest(workerSessionID string, evt *event.RequireUserConfirmEvent) error {
	p.mu.RLock()
	leaderID, ok := p.mappings[workerSessionID]
	p.mu.RUnlock()
	if !ok {
		return nil // no mapping — worker is standalone
	}

	return p.projection.Set(leaderID, "hitl", evt.GetEventID(), map[string]any{
		"type":              "require_user_confirm",
		"worker_session_id": workerSessionID,
		"reply_id":          evt.ReplyID,
		"tool_calls":        evt.ToolCalls,
	})
}

// ResolveConfirmRequest removes a projected HITL card after it's resolved.
func (p *SubagentHitlProjector) ResolveConfirmRequest(workerSessionID, eventID string) error {
	p.mu.RLock()
	leaderID, ok := p.mappings[workerSessionID]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	return p.projection.Remove(leaderID, "hitl", eventID)
}

// subagentHitlKind is the projection kind constant for subagent HITL entries.
const subagentHitlKind = "subagent_hitl"

// MaybeProject inspects an event and, if it is HITL-relevant, upserts or
// removes it from the leader session's projection hash. A notification is
// published to the leader session's event stream for each mutation.
func (p *SubagentHitlProjector) MaybeProject(evt event.Event, workerSID string, workerAgentName string) error {
	p.mu.RLock()
	leaderSID, ok := p.mappings[workerSID]
	p.mu.RUnlock()
	if !ok {
		return nil // worker is standalone
	}

	replyID := evt.GetReplyID()
	entryID := workerSID + ":" + replyID

	switch evt.GetEventType() {
	case event.EventRequireUserConfirm,
		event.EventRequireExternalExecution:
		// Request events: upsert into the leader's projection hash.
		payload := map[string]any{
			"worker_session_id": workerSID,
			"worker_agent_name": workerAgentName,
			"reply_id":          replyID,
			"event_type":        string(evt.GetEventType()),
			"created_at":        time.Now().Format(time.RFC3339),
		}
		if err := p.projection.SetEntry(leaderSID, subagentHitlKind, entryID, payload); err != nil {
			return err
		}
		return p.projection.Set(leaderSID, subagentHitlKind, entryID, payload)

	case event.EventUserConfirmResult,
		event.EventExternalExecutionResult,
		event.EventReplyEnd:
		// Result/end events: delete from the leader's projection hash.
		if err := p.projection.DeleteEntry(leaderSID, subagentHitlKind, entryID); err != nil {
			return err
		}
		return p.projection.Remove(leaderSID, subagentHitlKind, entryID)

	default:
		return nil // not HITL-relevant
	}
}

// Resolve scans the leader's projection entries to find which worker session
// owns a given replyID. It returns the worker session ID and true if found.
func (p *SubagentHitlProjector) Resolve(leaderSID, replyID string) (workerSID string, found bool) {
	entries, err := p.projection.ListEntries(leaderSID, subagentHitlKind)
	if err != nil || entries == nil {
		return "", false
	}

	for entryID, payload := range entries {
		// entryID format: "{workerSID}:{replyID}"
		rid, _ := payload["reply_id"].(string)
		if rid == replyID {
			// Extract workerSID from entryID.
			if idx := strings.Index(entryID, ":"); idx >= 0 {
				return entryID[:idx], true
			}
			// Fallback: read from payload.
			if wsid, _ := payload["worker_session_id"].(string); wsid != "" {
				return wsid, true
			}
		}
	}
	return "", false
}

// DropWorker removes all projection entries belonging to a specific worker
// from the leader's hash.
func (p *SubagentHitlProjector) DropWorker(leaderSID, workerSID string) error {
	entries, err := p.projection.ListEntries(leaderSID, subagentHitlKind)
	if err != nil {
		return err
	}
	if entries == nil {
		return nil
	}

	var toDelete []string
	prefix := workerSID + ":"
	for entryID := range entries {
		if strings.HasPrefix(entryID, prefix) {
			toDelete = append(toDelete, entryID)
		}
	}

	for _, entryID := range toDelete {
		if err := p.projection.DeleteEntry(leaderSID, subagentHitlKind, entryID); err != nil {
			return err
		}
	}
	return nil
}
