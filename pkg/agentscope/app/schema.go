package app

import "time"

// --- Session schemas ---

// CreateSessionRequest is the body for POST /api/session.
type CreateSessionRequest struct {
	AgentName    string            `json:"agent_name"`
	SystemPrompt string            `json:"system_prompt"`
	ModelName    string            `json:"model_name,omitempty"`
	Credential   map[string]string `json:"credential,omitempty"`
}

// SessionResponse is the standard session representation.
type SessionResponse struct {
	ID           string    `json:"id"`
	AgentName    string    `json:"agent_name"`
	SystemPrompt string    `json:"system_prompt"`
	ModelName    string    `json:"model_name,omitempty"`
	Members      []string  `json:"members,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ActiveSkills []string  `json:"active_skills,omitempty"`
}

// --- Chat schemas ---

// ChatRequest is the body for POST /api/chat/{sessionID}.
type ChatRequest struct {
	Message string `json:"message"`
}

// --- Agent schemas ---

// CreateAgentRequest is the body for POST /api/session/{id}/agent.
type CreateAgentRequest struct {
	Name         string `json:"name"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Type         string `json:"type,omitempty"`
}

// AgentResponse describes an agent in a session.
type AgentResponse struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

// --- Confirm schemas ---

// ConfirmRequest is the body for POST /api/chat/{sessionID}/confirm.
type ConfirmRequest struct {
	ToolCallID string `json:"tool_call_id"`
	Confirmed  bool   `json:"confirmed"`
}

// --- Schedule schemas ---

// CreateScheduleRequest is the body for POST /api/schedule.
type CreateScheduleRequest struct {
	SessionID string `json:"session_id"`
	CronExpr  string `json:"cron_expr,omitempty"`
	Input     string `json:"input"`
	RunOnce   bool   `json:"run_once,omitempty"`
}

// ScheduleResponse describes a scheduled task.
type ScheduleResponse struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	CronExpr  string    `json:"cron_expr,omitempty"`
	Input     string    `json:"input"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// --- Workspace schemas ---

// WorkspaceInfoResponse describes a managed workspace. SessionID carries
// the workspace ID (private workspaces are named after their session);
// WorkspaceID is the explicit field for shared workspaces.
type WorkspaceInfoResponse struct {
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	BasePath    string `json:"base_path"`
	Type        string `json:"type"` // "local", "docker", "e2b"
}

// --- Credential schemas ---

// CreateCredentialRequest is the body for POST /api/credential.
type CreateCredentialRequest struct {
	Provider string            `json:"provider"`
	Config   map[string]string `json:"config"`
}

// CredentialResponse describes a stored credential.
type CredentialResponse struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// --- TTS Model schemas ---

// TTSModelResponse describes a TTS model.
type TTSModelResponse struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}
