package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpsec"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Config configures the agent HTTP service.
type Config struct {
	Addr           string // listen address (default: ":8080")
	AllowedOrigins []string
	Storage        agent.StateSaver
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// AgentFactory creates agents by name/configuration.
type AgentFactory func(name, systemPrompt string, cm model.ChatModel) *agent.UnifiedAgent

var sessionCounter atomic.Int64

// Service is the HTTP agent service.
type Service struct {
	cfg            Config
	factory        AgentFactory
	scopeResolver  ScopeResolver
	contextFactory ContextAgentFactory
	model          model.ChatModel
	mux            *http.ServeMux

	mu       sync.RWMutex
	sessions map[string]*sessionState
	srv      *http.Server
}

type sessionState struct {
	agent       *agent.UnifiedAgent
	createdAt   time.Time
	ownerScope  string
	pending     *scopedPendingConfirmation
	activeReply bool
}

// New creates a new agent service.
func New(cfg Config, cm model.ChatModel, factory AgentFactory) *Service {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 120 * time.Second
	}

	s := &Service{
		cfg:      cfg,
		factory:  factory,
		model:    cm,
		mux:      http.NewServeMux(),
		sessions: make(map[string]*sessionState),
	}

	s.registerRoutes()
	return s
}

// Handler returns the HTTP handler for the service.
func (s *Service) Handler() http.Handler {
	if len(s.cfg.AllowedOrigins) > 0 {
		return s.corsMiddleware(s.mux)
	}
	return s.mux
}

// ListenAndServe starts the HTTP server.
func (s *Service) ListenAndServe() error {
	srv := &http.Server{
		Addr:         s.cfg.Addr,
		Handler:      httpsec.LimitBody(s.Handler(), 0),
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
	}
	httpsec.Harden(srv)
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()
	return srv.ListenAndServe()
}

// Shutdown gracefully stops the server, draining in-flight requests.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	srv := s.srv
	s.mu.RUnlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *Service) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("POST /api/chat", s.sessionRoute(s.handleChat))
	s.mux.HandleFunc("GET /api/chat/stream", s.sessionRoute(s.handleChatStream))
	s.mux.HandleFunc("POST /api/session", s.sessionRoute(s.handleCreateSession))
	s.mux.HandleFunc("GET /api/session/", s.sessionRoute(s.handleGetSession))
	s.mux.HandleFunc("GET /api/sessions", s.sessionRoute(s.handleListSessions))
	s.mux.HandleFunc("DELETE /api/session/", s.sessionRoute(s.handleDeleteSession))
	s.mux.HandleFunc("GET /api/models", s.handleListModels)
	s.mux.HandleFunc("POST /api/confirm", s.sessionRoute(s.handleConfirm))
}

// --- Request/Response types ---

// ChatRequest is the body of POST /api/chat.
type ChatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// ChatResponse is a non-streaming chat response.
type ChatResponse struct {
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

// SessionRequest is the body of POST /api/session.
type SessionRequest struct {
	AgentName    string `json:"agent_name"`
	SystemPrompt string `json:"system_prompt"`
}

// SessionResponse describes a session.
type SessionResponse struct {
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ConfirmRequest is the body of POST /api/confirm.
type ConfirmRequest struct {
	SessionID string `json:"session_id"`
	ToolCalls []struct {
		ID        string `json:"id"`
		Confirmed bool   `json:"confirmed"`
	} `json:"tool_calls"`
}

// --- Handlers ---

func (s *Service) handleChat(w http.ResponseWriter, r *http.Request, scope string) {
	var req ChatRequest
	if !s.decodeRequest(w, r, &req) {
		return
	}

	sess := s.getSession(req.SessionID, scope)
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	ctx := r.Context()
	if s.scopeResolver != nil {
		owned, finish, ok := s.beginScopedReply(w, r, sess)
		if !ok {
			return
		}
		defer finish()
		ctx = owned
	}
	reply, err := sess.agent.Reply(ctx, req.Message)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	text := ""
	if t := reply.GetTextContent("\n"); t != nil {
		text = *t
	}
	writeJSON(w, http.StatusOK, ChatResponse{SessionID: req.SessionID, Reply: text})
}

func (s *Service) handleChatStream(w http.ResponseWriter, r *http.Request, scope string) {
	sessionID := r.URL.Query().Get("session_id")
	msg := r.URL.Query().Get("message")
	if sessionID == "" || msg == "" {
		writeError(w, http.StatusBadRequest, "session_id and message are required")
		return
	}

	sess := s.getSession(sessionID, scope)
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if s.scopeResolver != nil {
		s.handleScopedStream(w, r, sess, msg)
		return
	}

	sse, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ch, err := sess.agent.ReplyStream(r.Context(), msg)
	if err != nil {
		_ = sse.WriteEvent("error", map[string]string{"error": err.Error()})
		return
	}

	for evt := range ch {
		_ = sse.WriteEvent(string(evt.GetEventType()), evt)
	}
}

func (s *Service) handleCreateSession(w http.ResponseWriter, r *http.Request, scope string) {
	var req SessionRequest
	if !s.decodeRequest(w, r, &req) {
		return
	}

	name := req.AgentName
	if name == "" {
		name = "assistant"
	}
	prompt := req.SystemPrompt
	if prompt == "" {
		prompt = "You are a helpful assistant."
	}

	var a *agent.UnifiedAgent
	if s.contextFactory != nil {
		if requestCanceled(w, r) {
			return
		}
		var err error
		a, err = s.contextFactory(r.Context(), name, prompt, s.model)
		if requestCanceled(w, r) {
			return
		}
		if err != nil || a == nil {
			writeError(w, http.StatusInternalServerError, "agent construction failed")
			return
		}
	} else {
		a = s.factory(name, prompt, s.model)
	}

	sessionID := fmt.Sprintf("sess_%d", sessionCounter.Add(1))

	s.mu.Lock()
	// The final cancellation check is the scoped construction admission point.
	// Cancellation after this check does not revoke an admitted session.
	if s.scopeResolver != nil && r.Context().Err() != nil {
		s.mu.Unlock()
		requestCanceled(w, r)
		return
	}
	s.sessions[sessionID] = &sessionState{
		agent:      a,
		createdAt:  time.Now(),
		ownerScope: scope,
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, SessionResponse{
		SessionID: sessionID,
		AgentName: name,
		CreatedAt: time.Now().Format(time.RFC3339),
	})
}

func (s *Service) handleGetSession(w http.ResponseWriter, r *http.Request, scope string) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/session/")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	sess := s.getSession(sessionID, scope)
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	writeJSON(w, http.StatusOK, SessionResponse{
		SessionID: sessionID,
		CreatedAt: sess.createdAt.Format(time.RFC3339),
	})
}

func (s *Service) handleListSessions(w http.ResponseWriter, _ *http.Request, scope string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]SessionResponse, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.ownerScope != scope {
			continue
		}
		sessions = append(sessions, SessionResponse{
			SessionID: id,
			CreatedAt: sess.createdAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Service) handleDeleteSession(w http.ResponseWriter, r *http.Request, scope string) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/session/")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	ok = ok && sess.ownerScope == scope
	if ok {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleListModels(w http.ResponseWriter, r *http.Request) {
	models := model.ListModels()
	writeJSON(w, http.StatusOK, models)
}

func (s *Service) handleConfirm(w http.ResponseWriter, r *http.Request, scope string) {
	var req ConfirmRequest
	if !s.decodeRequest(w, r, &req) {
		return
	}

	sess := s.getSession(req.SessionID, scope)
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if s.scopeResolver != nil {
		s.handleScopedConfirmation(w, r, sess, req)
		return
	}

	var results []event.ConfirmResult
	for _, tc := range req.ToolCalls {
		results = append(results, event.ConfirmResult{
			Confirmed: tc.Confirmed,
			ToolCall:  message.ToolCallBlock{ID: tc.ID},
		})
	}

	confirmResult := event.NewUserConfirmResultEvent("", results)
	sess.agent.SubmitUserConfirm(&confirmResult)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) getSession(id, scope string) *sessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess := s.sessions[id]
	if sess != nil && sess.ownerScope != scope {
		return nil
	}
	return sess
}

// --- CORS ---

func (s *Service) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, allowed := range s.cfg.AllowedOrigins {
			if allowed == "*" || allowed == origin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-User-ID")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
