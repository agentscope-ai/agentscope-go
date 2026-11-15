package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpsec"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/service"
	"github.com/sirupsen/logrus"
)

// AppConfig configures the full application.
type AppConfig struct {
	Addr              string
	AllowedOrigins    []string
	Storage           agent.StateSaver
	CredentialFactory *credential.Factory
	DefaultModel      string
	SystemPrompt      string
	AgentMiddlewares  []middleware.Middleware
	AgentFactory      AgentFactory
	WorkspaceDir      string // base directory for per-session workspaces

	// WorkspaceAgentFactory, when set, creates agents with the session's
	// workspace handed in — the hook for filesystem-backed middleware such
	// as memory.NewAgenticMemory (parity with Python's workspace-aware
	// agent middleware factories). Requires WorkspaceDir and takes
	// precedence over AgentFactory.
	WorkspaceAgentFactory WorkspaceAgentFactory
	MessageBus            messagebus.MessageBus
	OnStartup             []func() // hooks run after server starts
	OnShutdown            []func() // hooks run before server stops
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration

	// MetricsHandler, if set, is mounted at GET /metrics. Pass, e.g., a
	// prometheus provider's Handler(); the app itself stays dependency-free.
	MetricsHandler http.Handler
}

// App is the top-level application that assembles all components.
type App struct {
	cfg          AppConfig
	mux          *http.ServeMux
	srv          *http.Server
	credFact     *credential.Factory
	sessionSvc   *SessionService
	chatSvc      *ChatService
	bgManager    *BackgroundTaskManager
	cancelDisp   *CancelDispatcher
	wakeupDisp   *WakeupDispatcher
	chatRegistry *ChatRunRegistry
	wsMgr        *WorkspaceManager
	schedulerMgr *SchedulerManager
}

// CreateApp assembles and returns a ready-to-serve App.
func CreateApp(cfg *AppConfig) (*App, error) {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 120 * time.Second
	}
	if cfg.CredentialFactory == nil {
		cfg.CredentialFactory = credential.NewFactory()
	}

	app := &App{
		cfg:      *cfg,
		mux:      http.NewServeMux(),
		credFact: cfg.CredentialFactory,
	}

	if cfg.WorkspaceAgentFactory != nil && cfg.WorkspaceDir == "" {
		return nil, fmt.Errorf("app: WorkspaceAgentFactory requires WorkspaceDir")
	}

	app.sessionSvc = NewSessionService(cfg.AgentFactory)
	app.bgManager = NewBackgroundTaskManager()
	app.cancelDisp = NewCancelDispatcher(app.bgManager)
	app.wakeupDisp = NewWakeupDispatcher()
	app.chatRegistry = NewChatRunRegistry()
	app.chatSvc = NewChatService(app)

	if cfg.WorkspaceDir != "" {
		app.wsMgr = NewWorkspaceManager(cfg.WorkspaceDir, "local")
	}
	if cfg.WorkspaceAgentFactory != nil {
		app.sessionSvc.SetWorkspaceFactory(cfg.WorkspaceAgentFactory, app.wsMgr.GetOrCreate)
	}

	app.registerRoutes()
	app.registerExtendedRoutes()
	return app, nil
}

func (a *App) registerRoutes() {
	// Liveness/readiness for load balancers and orchestrators.
	a.mux.HandleFunc("GET /healthz", healthzHandler)
	a.mux.HandleFunc("GET /readyz", healthzHandler)

	// Optional metrics scrape endpoint (e.g. Prometheus).
	if a.cfg.MetricsHandler != nil {
		a.mux.Handle("GET /metrics", a.cfg.MetricsHandler)
	}

	// Session management
	a.mux.HandleFunc("POST /api/session", a.handleCreateSession)
	a.mux.HandleFunc("GET /api/session", a.handleListSessions)
	a.mux.HandleFunc("GET /api/session/{id}", a.handleGetSession)
	a.mux.HandleFunc("DELETE /api/session/{id}", a.handleDeleteSession)

	// Session agents / members
	a.mux.HandleFunc("GET /api/session/{id}/members", a.handleListMembers)

	// Chat
	a.mux.HandleFunc("POST /api/chat/{sessionID}", a.handleChat)
	a.mux.HandleFunc("POST /api/chat/{sessionID}/stream", a.handleChatStream)
	a.mux.HandleFunc("POST /api/chat/{sessionID}/cancel", a.handleCancelChat)
	a.mux.HandleFunc("POST /api/chat/{sessionID}/confirm", a.handleConfirm)

	// Credentials
	a.mux.HandleFunc("GET /api/credential/schemas", a.handleListCredentialSchemas)

	// Models
	a.mux.HandleFunc("GET /api/model", a.handleListModels)

	// Background tasks
	a.mux.HandleFunc("GET /api/task", a.handleListTasks)
	a.mux.HandleFunc("DELETE /api/task/{id}", a.handleCancelTask)

	// Workspace
	if a.wsMgr != nil {
		a.mux.HandleFunc("GET /api/workspace", a.handleListWorkspaces)
	}
}

// Handler returns the HTTP handler.
func (a *App) Handler() http.Handler {
	if len(a.cfg.AllowedOrigins) > 0 {
		return corsMiddleware(a.mux, a.cfg.AllowedOrigins)
	}
	return a.mux
}

// ListenAndServe starts the HTTP server.
func (a *App) ListenAndServe() error {
	a.srv = &http.Server{
		Addr:         a.cfg.Addr,
		Handler:      httpsec.LimitBody(a.Handler(), 0),
		ReadTimeout:  a.cfg.ReadTimeout,
		WriteTimeout: a.cfg.WriteTimeout,
	}
	httpsec.Harden(a.srv)
	logrus.WithField("addr", a.cfg.Addr).Info("app: starting server")
	return a.srv.ListenAndServe()
}

// healthzHandler is a minimal liveness/readiness probe.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Shutdown gracefully shuts down the server and all managed resources.
func (a *App) Shutdown(ctx context.Context) error {
	// Run shutdown hooks
	for _, hook := range a.cfg.OnShutdown {
		hook()
	}
	if a.srv != nil {
		return a.srv.Shutdown(ctx)
	}
	return nil
}

// --- Session handlers ---

func (a *App) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SystemPrompt == "" {
		req.SystemPrompt = a.cfg.SystemPrompt
	}

	session := a.sessionSvc.Create(req)
	writeJSON(w, http.StatusCreated, session.ToResponse())
}

func (a *App) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	sessions := a.sessionSvc.List()
	result := make([]SessionResponse, len(sessions))
	for i, s := range sessions {
		result[i] = s.ToResponse()
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *App) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := a.sessionSvc.Get(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, session.ToResponse())
}

func (a *App) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.sessionSvc.Delete(id)
	if a.wsMgr != nil {
		a.wsMgr.Remove(id)
	}
	a.wakeupDisp.Unregister(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleListMembers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	members, err := a.sessionSvc.ListMembers(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, members)
}

// --- Chat handlers ---

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, finish, accepted := a.acquireChat(r.Context(), sessionID)
	if !accepted {
		http.Error(w, "chat already running for this session", http.StatusConflict)
		return
	}
	defer finish()

	resp, err := a.chatSvc.Chat(ctx, sessionID, req.Message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleChatStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, finish, accepted := a.acquireChat(r.Context(), sessionID)
	if !accepted {
		http.Error(w, "chat already running for this session", http.StatusConflict)
		return
	}
	// Defer so a panic mid-handler cannot wedge the session slot forever.
	defer finish()

	ch, err := a.chatSvc.ChatStream(ctx, sessionID, req.Message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sse, err2 := service.NewSSEWriter(w)
	if err2 != nil {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	for evt := range ch {
		if writeErr := sse.WriteEvent("event", evt); writeErr != nil {
			break
		}
	}
}

func (a *App) handleCancelChat(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	a.chatRegistry.cancel(sessionID)
	a.cancelDisp.Cancel(sessionID)
	w.WriteHeader(http.StatusOK)
}

// acquireChat couples cancellation and exclusive ownership. Forwarded streams
// may close before a canceled model/tool finishes, so release waits for cores.
func (a *App) acquireChat(parent context.Context, sessionID string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	run := a.chatRegistry.acquire(sessionID, cancel)
	if run == nil {
		cancel()
		return nil, nil, false
	}
	ctx, scope := runctx.New(ctx)
	return ctx, func() {
		cancel()
		scope.Close()
		a.chatRegistry.release(sessionID, run)
	}, true
}

func (a *App) handleConfirm(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	var req ConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ag, err := a.sessionSvc.GetOrCreateAgent(sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	confirmResult := event.NewUserConfirmResultEvent(
		"", // replyID not needed for routing
		[]event.ConfirmResult{{
			Confirmed: req.Confirmed,
			ToolCall:  message.ToolCallBlock{ID: req.ToolCallID},
		}},
	)
	ag.SubmitUserConfirm(&confirmResult)

	w.WriteHeader(http.StatusOK)
}

// --- Credential handlers ---

func (a *App) handleListCredentialSchemas(w http.ResponseWriter, _ *http.Request) {
	schemas := a.credFact.ListSchemas()
	writeJSON(w, http.StatusOK, schemas)
}

// --- Model handlers ---

func (a *App) handleListModels(w http.ResponseWriter, _ *http.Request) {
	cards := model.ListModels()
	writeJSON(w, http.StatusOK, cards)
}

// --- Task handlers ---

func (a *App) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	tasks := a.bgManager.List()
	writeJSON(w, http.StatusOK, tasks)
}

func (a *App) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.bgManager.Cancel(id)
	w.WriteHeader(http.StatusOK)
}

// --- Chat Service ---

// ChatService manages chat interactions with agents.
type ChatService struct {
	app *App
}

// NewChatService creates a new chat service.
func NewChatService(app *App) *ChatService {
	return &ChatService{app: app}
}

// Chat sends a message and returns the final response.
func (cs *ChatService) Chat(ctx context.Context, sessionID, userMessage string) (*message.Msg, error) {
	a, err := cs.app.sessionSvc.GetOrCreateAgent(sessionID)
	if err != nil {
		return nil, err
	}
	return a.Reply(ctx, userMessage)
}

// ChatStream sends a message and returns a channel of streaming events.
func (cs *ChatService) ChatStream(ctx context.Context, sessionID, userMessage string) (<-chan event.Event, error) {
	a, err := cs.app.sessionSvc.GetOrCreateAgent(sessionID)
	if err != nil {
		return nil, err
	}
	return a.ReplyStream(ctx, userMessage)
}

// --- Workspace handler ---

func (a *App) handleListWorkspaces(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.wsMgr.List())
}

// --- Background Task Manager ---

// TaskInfo describes a background task.
type TaskInfo struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// BackgroundTaskManager tracks running background tasks.
type BackgroundTaskManager struct {
	mu    sync.RWMutex
	tasks map[string]*bgTask
}

type bgTask struct {
	info   TaskInfo
	cancel context.CancelFunc
}

// NewBackgroundTaskManager creates a new task manager.
func NewBackgroundTaskManager() *BackgroundTaskManager {
	return &BackgroundTaskManager{
		tasks: make(map[string]*bgTask),
	}
}

// Track registers a new task.
func (m *BackgroundTaskManager) Track(sessionID string, cancel context.CancelFunc) string {
	id := agentscope.GenerateID()
	m.mu.Lock()
	m.tasks[id] = &bgTask{
		info: TaskInfo{
			ID:        id,
			SessionID: sessionID,
			Status:    "running",
			CreatedAt: time.Now(),
		},
		cancel: cancel,
	}
	m.mu.Unlock()
	return id
}

// Complete marks a task as completed.
func (m *BackgroundTaskManager) Complete(id string) {
	m.mu.Lock()
	if t, ok := m.tasks[id]; ok {
		t.info.Status = "completed"
	}
	m.mu.Unlock()
}

// Cancel cancels a task.
func (m *BackgroundTaskManager) Cancel(id string) {
	m.mu.Lock()
	if t, ok := m.tasks[id]; ok {
		t.cancel()
		t.info.Status = "canceled"
	}
	m.mu.Unlock()
}

// List returns all tasks.
func (m *BackgroundTaskManager) List() []TaskInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]TaskInfo, 0, len(m.tasks))
	for _, t := range m.tasks {
		result = append(result, t.info)
	}
	return result
}

// --- Cancel Dispatcher ---

// CancelDispatcher cancels running chat sessions.
type CancelDispatcher struct {
	bgMgr *BackgroundTaskManager
}

// NewCancelDispatcher creates a new cancel dispatcher.
func NewCancelDispatcher(bgMgr *BackgroundTaskManager) *CancelDispatcher {
	return &CancelDispatcher{bgMgr: bgMgr}
}

// Cancel cancels all tasks for a given session.
func (d *CancelDispatcher) Cancel(sessionID string) {
	var cancels []context.CancelFunc
	d.bgMgr.mu.Lock()
	for _, t := range d.bgMgr.tasks {
		if t.info.SessionID == sessionID && t.info.Status == "running" {
			cancels = append(cancels, t.cancel)
			t.info.Status = "canceled"
		}
	}
	d.bgMgr.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func corsMiddleware(next http.Handler, origins []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, o := range origins {
			if o == "*" || o == origin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				break
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
