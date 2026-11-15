package app

import (
	"encoding/json"
	"fmt"
	"net/http"

	"errors"
	"os"
	"path"
	"strings"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/skill"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/storage"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tts"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/workspace"
)

// registerExtendedRoutes adds CRUD routes for agents, credentials, schedules,
// sessions (update/messages/stream), workspace MCP/skill, and TTS models.
func (a *App) registerExtendedRoutes() {
	// Agent CRUD
	a.mux.HandleFunc("GET /api/agent", a.handleListAgents)
	a.mux.HandleFunc("GET /api/agent/schema", a.handleAgentSchema)
	a.mux.HandleFunc("POST /api/agent", a.handleCreateAgent)
	a.mux.HandleFunc("GET /api/agent/{id}", a.handleGetAgent)
	a.mux.HandleFunc("PATCH /api/agent/{id}", a.handleUpdateAgent)
	a.mux.HandleFunc("DELETE /api/agent/{id}", a.handleDeleteAgent)

	// Credential CRUD
	a.mux.HandleFunc("GET /api/credential", a.handleListCredentials)
	a.mux.HandleFunc("POST /api/credential", a.handleCreateCredential)
	a.mux.HandleFunc("GET /api/credential/{id}", a.handleGetCredential)
	a.mux.HandleFunc("PATCH /api/credential/{id}", a.handleUpdateCredential)
	a.mux.HandleFunc("DELETE /api/credential/{id}", a.handleDeleteCredential)

	// Schedule CRUD
	a.mux.HandleFunc("GET /api/schedule", a.handleListSchedules)
	a.mux.HandleFunc("POST /api/schedule", a.handleCreateSchedule)
	a.mux.HandleFunc("GET /api/schedule/{id}", a.handleGetSchedule)
	a.mux.HandleFunc("PATCH /api/schedule/{id}", a.handleUpdateSchedule)
	a.mux.HandleFunc("DELETE /api/schedule/{id}", a.handleDeleteSchedule)
	a.mux.HandleFunc("GET /api/schedule/{id}/sessions", a.handleListScheduleSessions)

	// Session extended
	a.mux.HandleFunc("PATCH /api/session/{id}", a.handleUpdateSession)
	a.mux.HandleFunc("GET /api/session/{id}/messages", a.handleListMessages)

	// Workspace sharing + artifacts (read-only file access)
	a.mux.HandleFunc("POST /api/workspace/share", a.handleShareWorkspace)
	a.mux.HandleFunc("GET /api/workspace/{id}/list_dir", a.handleWorkspaceListDir)
	a.mux.HandleFunc("GET /api/workspace/{id}/read_file", a.handleWorkspaceReadFile)

	// Workspace MCP/Skill management
	a.mux.HandleFunc("GET /api/workspace/mcp", a.handleListWorkspaceMCPs)
	a.mux.HandleFunc("POST /api/workspace/mcp", a.handleAddWorkspaceMCP)
	a.mux.HandleFunc("DELETE /api/workspace/mcp/{name}", a.handleRemoveWorkspaceMCP)
	a.mux.HandleFunc("GET /api/workspace/skill", a.handleListWorkspaceSkills)
	a.mux.HandleFunc("POST /api/workspace/skill", a.handleAddWorkspaceSkill)
	a.mux.HandleFunc("DELETE /api/workspace/skill/{name}", a.handleRemoveWorkspaceSkill)

	// TTS models
	a.mux.HandleFunc("GET /api/tts-model", a.handleListTTSModels)
}

// --- Agent CRUD ---

func (a *App) handleListAgents(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	userID := r.Header.Get("X-User-ID")
	agents, err := fs.ListAgents(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

func (a *App) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	var req CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	record := &storage.AgentRecord{
		ID:           agentscope.GenerateID(),
		UserID:       r.Header.Get("X-User-ID"),
		Name:         req.Name,
		SystemPrompt: req.SystemPrompt,
		ModelName:    req.Type,
	}
	if err := fs.SaveAgent(r.Context(), record); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (a *App) handleAgentSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"properties": map[string]any{
			"name":          map[string]string{"type": "string"},
			"system_prompt": map[string]string{"type": "string"},
			"type":          map[string]string{"type": "string"},
		},
		"required": []string{"name"},
	})
}

func (a *App) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	record, err := fs.LoadAgent(r.Context(), r.Header.Get("X-User-ID"), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if v, ok := patch["name"].(string); ok {
		record.Name = v
	}
	if v, ok := patch["system_prompt"].(string); ok {
		record.SystemPrompt = v
	}
	if v, ok := patch["model_name"].(string); ok {
		record.ModelName = v
	}
	if err := fs.SaveAgent(r.Context(), record); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *App) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	record, err := fs.LoadAgent(r.Context(), r.Header.Get("X-User-ID"), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *App) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = fs.DeleteAgent(r.Context(), r.Header.Get("X-User-ID"), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// --- Credential CRUD ---

func (a *App) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	creds, err := fs.ListCredentials(r.Context(), r.Header.Get("X-User-ID"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, creds)
}

func (a *App) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	var req CreateCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	record := &storage.CredentialRecord{
		ID:       agentscope.GenerateID(),
		UserID:   r.Header.Get("X-User-ID"),
		Provider: req.Provider,
		Data:     req.Config,
	}
	if err := fs.SaveCredential(r.Context(), record); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, CredentialResponse{ID: record.ID, Provider: record.Provider})
}

func (a *App) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	record, err := fs.LoadCredential(r.Context(), r.Header.Get("X-User-ID"), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, CredentialResponse{ID: record.ID, Provider: record.Provider})
}

func (a *App) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	record, err := fs.LoadCredential(r.Context(), r.Header.Get("X-User-ID"), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var patch map[string]string
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if record.Data == nil {
		record.Data = make(map[string]string)
	}
	for k, v := range patch {
		record.Data[k] = v
	}
	if err := fs.SaveCredential(r.Context(), record); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, CredentialResponse{ID: record.ID, Provider: record.Provider})
}

func (a *App) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = fs.DeleteCredential(r.Context(), r.Header.Get("X-User-ID"), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// --- Schedule CRUD ---

func (a *App) handleListSchedules(w http.ResponseWriter, _ *http.Request) {
	if a.schedulerMgr == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, a.schedulerMgr.List())
}

func (a *App) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if a.schedulerMgr == nil {
		http.Error(w, "scheduler not configured", http.StatusNotImplemented)
		return
	}
	var req CreateScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	record, err := a.schedulerMgr.Create(r.Context(), req)
	if err != nil {
		// Upstream #2442: a rejected schedule is a client error, not a
		// server failure.
		var verr *CronValidationError
		if errors.As(err, &verr) {
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (a *App) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	if a.schedulerMgr == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	record, ok := a.schedulerMgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *App) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if a.schedulerMgr == nil {
		http.Error(w, "scheduler not configured", http.StatusNotImplemented)
		return
	}
	record, ok := a.schedulerMgr.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Get returns a copy, so the patch has to be applied through Update or it
	// is silently dropped: the old code mutated its local copy and answered
	// 200 with a body the store never saw.
	updated, ok := a.schedulerMgr.Update(record.ID, func(rec *ScheduleRecord) {
		if v, ok := patch["input"].(string); ok {
			rec.Input = v
		}
		if v, ok := patch["status"].(string); ok {
			rec.Status = v
		}
	})
	if !ok {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	// Update refuses a transition that would advertise a schedule which can
	// never fire again (resuming a stopped chain, or resurrecting a canceled
	// one). Say so instead of answering 200 with a body that silently
	// disagrees with the request.
	if want, isStatusPatch := patch["status"].(string); isStatusPatch && updated.Status != want {
		// Phrased from the client's point of view: it asked for `want` and
		// the record stays at its previous status. Printing the pair the
		// other way round reads like the refused transition was applied.
		http.Error(w, fmt.Sprintf(
			"cannot set status to %q: a stopped or canceled schedule cannot be resumed, it stays %q; create a new one",
			want, updated.Status), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *App) handleListScheduleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

func (a *App) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if a.schedulerMgr == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = a.schedulerMgr.Cancel(r.Context(), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// --- Session extended ---

func (a *App) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := a.sessionSvc.Get(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	var req struct {
		SystemPrompt *string   `json:"system_prompt,omitempty"`
		ModelName    *string   `json:"model_name,omitempty"`
		ActiveSkills *[]string `json:"active_skills,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SystemPrompt != nil {
		session.SystemPrompt = *req.SystemPrompt
	}
	if req.ModelName != nil {
		session.ModelName = *req.ModelName
	}
	if req.ActiveSkills != nil {
		session.ActiveSkills = append([]string(nil), *req.ActiveSkills...)
	}
	writeJSON(w, http.StatusOK, session.ToResponse())
}

func (a *App) handleListMessages(w http.ResponseWriter, r *http.Request) {
	fs := a.getFullStorage()
	if fs == nil {
		writeJSON(w, http.StatusOK, CursorPage[*storage.MessageRecord]{Items: []*storage.MessageRecord{}})
		return
	}
	sessionID := r.PathValue("id")
	msgs, err := fs.ListMessages(r.Context(), sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse cursor pagination params from query string.
	cursor := r.URL.Query().Get("cursor")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &limit)
	}
	page := ParseCursorRequest(cursor, limit)

	// Apply cursor: skip messages up to and including the cursor ID.
	start := 0
	if page.Cursor != "" {
		for i, m := range msgs {
			if m.ID == page.Cursor {
				start = i + 1
				break
			}
		}
	}

	end := start + page.Limit
	hasMore := false
	if end < len(msgs) {
		hasMore = true
	} else {
		end = len(msgs)
	}

	subset := msgs[start:end]
	nextCursor := ""
	if hasMore && len(subset) > 0 {
		nextCursor = subset[len(subset)-1].ID
	}

	writeJSON(w, http.StatusOK, CursorPage[*storage.MessageRecord]{
		Items:      subset,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	})
}

// --- Workspace MCP/Skill ---

func (a *App) handleListWorkspaceMCPs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

func (a *App) handleAddWorkspaceMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "status": "added"})
}

func (a *App) handleRemoveWorkspaceMCP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// workspaceSkillStore resolves the session's workspace skill store from
// the session_id query parameter; agent_id selects the partition (empty
// means the default one).
func (a *App) workspaceSkillStore(r *http.Request) (*skill.Store, string, int, error) {
	if a.wsMgr == nil {
		return nil, "", http.StatusNotImplemented, fmt.Errorf("workspace not configured")
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		return nil, "", http.StatusBadRequest, fmt.Errorf("session_id query parameter is required")
	}
	ws, err := a.wsMgr.GetOrCreate(sessionID)
	if err != nil {
		return nil, "", http.StatusInternalServerError, fmt.Errorf("workspace for session %s: %w", sessionID, err)
	}
	return skill.NewStore(ws.BasePath()), r.URL.Query().Get("agent_id"), 0, nil
}

type workspaceSkillView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
	Dir         string `json:"dir"`
}

// skillErrorStatus maps skill store errors onto HTTP status codes.
func skillErrorStatus(err error) int {
	switch {
	case errors.Is(err, skill.ErrAlreadyExists):
		return http.StatusConflict
	case errors.Is(err, skill.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, skill.ErrInvalidAgentID):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (a *App) handleListWorkspaceSkills(w http.ResponseWriter, r *http.Request) {
	store, agentID, status, err := a.workspaceSkillStore(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	skills, err := store.List(agentID)
	if err != nil {
		http.Error(w, err.Error(), skillErrorStatus(err))
		return
	}
	views := make([]workspaceSkillView, 0, len(skills))
	for _, s := range skills {
		views = append(views, workspaceSkillView{
			Name:        s.Name,
			Description: s.Description,
			Category:    s.Category,
			Dir:         s.Dir,
		})
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *App) handleAddWorkspaceSkill(w http.ResponseWriter, r *http.Request) {
	store, agentID, status, err := a.workspaceSkillStore(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	var req struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Category     string `json:"category"`
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dirName, err := store.Add(agentID, req.Name, req.Description, req.Category, req.Instructions)
	if err != nil {
		if errors.Is(err, skill.ErrInvalidInput) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), skillErrorStatus(err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"name":   req.Name,
		"dir":    dirName,
		"status": "added",
	})
}

func (a *App) handleRemoveWorkspaceSkill(w http.ResponseWriter, r *http.Request) {
	store, agentID, status, err := a.workspaceSkillStore(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if err := store.Remove(agentID, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), skillErrorStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Workspace sharing + artifacts ---

// maxArtifactSize caps read_file responses; larger files are refused before
// their content is read.
const maxArtifactSize = 10 << 20

// handleShareWorkspace binds a session to a named workspace that multiple
// sessions can share (Python #1951 semantics).
func (a *App) handleShareWorkspace(w http.ResponseWriter, r *http.Request) {
	if a.wsMgr == nil {
		http.Error(w, "workspace not configured", http.StatusNotImplemented)
		return
	}
	var req struct {
		SessionID   string `json:"session_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.WorkspaceID == "" {
		http.Error(w, "session_id and workspace_id are required", http.StatusBadRequest)
		return
	}
	ws, err := a.wsMgr.Share(req.SessionID, req.WorkspaceID)
	if err != nil {
		if errors.Is(err, errInvalidWorkspaceID) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"workspace_id": req.WorkspaceID,
		"base_path":    ws.BasePath(),
	})
}

// workspaceForArtifact resolves a live workspace by path id.
func (a *App) workspaceForArtifact(w http.ResponseWriter, r *http.Request) (ws workspace.Workspace, ok bool) {
	if a.wsMgr == nil {
		http.Error(w, "workspace not configured", http.StatusNotImplemented)
		return nil, false
	}
	ws = a.wsMgr.GetByID(r.PathValue("id"))
	if ws == nil {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return nil, false
	}
	return ws, true
}

// workspacePathErrorStatus maps workspace path errors onto status codes.
func workspacePathErrorStatus(err error) int {
	if errors.Is(err, workspace.ErrPathEscape) {
		return http.StatusBadRequest
	}
	if os.IsNotExist(err) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// handleWorkspaceListDir lists a directory inside a live workspace
// (artifact browsing, Python #2187).
func (a *App) handleWorkspaceListDir(w http.ResponseWriter, r *http.Request) {
	ws, ok := a.workspaceForArtifact(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		rel = "."
	}
	files, err := ws.ListFiles(r.Context(), rel)
	if err != nil {
		http.Error(w, err.Error(), workspacePathErrorStatus(err))
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// handleWorkspaceReadFile reads one file from a live workspace. Read-only;
// the size is checked via the directory listing BEFORE the content is read.
func (a *App) handleWorkspaceReadFile(w http.ResponseWriter, r *http.Request) {
	ws, ok := a.workspaceForArtifact(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}

	if strings.HasPrefix(rel, "/") {
		http.Error(w, "path must be workspace-relative", http.StatusBadRequest)
		return
	}
	// Pre-read size check through the interface (no direct filesystem
	// access at the app layer): list the parent and find the entry.
	dir, name := path.Split(path.Clean(rel))
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	entries, err := ws.ListFiles(r.Context(), dir)
	if err != nil {
		http.Error(w, err.Error(), workspacePathErrorStatus(err))
		return
	}
	var size int64 = -1
	isDir := false
	for _, e := range entries {
		if e.Name == name {
			size = e.Size
			isDir = e.IsDir
			break
		}
	}
	if size < 0 {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if isDir {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}
	if size > maxArtifactSize {
		http.Error(w, "file exceeds the read limit", http.StatusRequestEntityTooLarge)
		return
	}

	data, err := ws.ReadFile(r.Context(), rel)
	if err != nil {
		http.Error(w, err.Error(), workspacePathErrorStatus(err))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// --- TTS models ---

func (a *App) handleListTTSModels(w http.ResponseWriter, _ *http.Request) {
	cards := tts.ListTTSModels()
	writeJSON(w, http.StatusOK, cards)
}

// --- Model listing ---

// getFullStorage attempts to cast the StateSaver to FullStorage.
func (a *App) getFullStorage() storage.FullStorage {
	if fs, ok := a.cfg.Storage.(storage.FullStorage); ok {
		return fs
	}
	return nil
}
