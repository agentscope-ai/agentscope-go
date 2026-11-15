package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestSSEWriter(t *testing.T) {
	w := httptest.NewRecorder()
	sse, err := NewSSEWriter(w)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}

	_ = sse.WriteEvent("test", map[string]string{"key": "value"})

	body := w.Body.String()
	if !strings.Contains(body, "event: test") {
		t.Errorf("missing event line: %q", body)
	}
	if !strings.Contains(body, `"key":"value"`) {
		t.Errorf("missing data: %q", body)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
}

func TestSSEWriterComment(t *testing.T) {
	w := httptest.NewRecorder()
	sse, _ := NewSSEWriter(w)
	sse.WriteComment("keepalive")

	body := w.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Errorf("missing comment: %q", body)
	}
}

func TestCreateSession(t *testing.T) {
	svc := newTestService()

	body := `{"agent_name": "test-agent", "system_prompt": "Hello"}`
	req := httptest.NewRequest("POST", "/api/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp SessionResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.SessionID == "" {
		t.Error("expected non-empty session ID")
	}
	if resp.AgentName != "test-agent" {
		t.Errorf("agent = %q, want %q", resp.AgentName, "test-agent")
	}
}

func TestListSessions(t *testing.T) {
	svc := newTestService()
	createSession(t, svc)
	createSession(t, svc)

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var sessions []SessionResponse
	_ = json.NewDecoder(w.Body).Decode(&sessions)
	if len(sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(sessions))
	}
}

func TestGetSessionNotFound(t *testing.T) {
	svc := newTestService()

	req := httptest.NewRequest("GET", "/api/session/nonexistent", nil)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDeleteSession(t *testing.T) {
	svc := newTestService()
	id := createSession(t, svc)

	req := httptest.NewRequest("DELETE", "/api/session/"+id, nil)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}

	req = httptest.NewRequest("GET", "/api/session/"+id, nil)
	w = httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status after delete = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	svc := newTestService()

	req := httptest.NewRequest("DELETE", "/api/session/nonexistent", nil)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestChatSessionNotFound(t *testing.T) {
	svc := newTestService()

	body := `{"session_id": "nonexistent", "message": "hello"}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestChatInvalidBody(t *testing.T) {
	svc := newTestService()

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestListModels(t *testing.T) {
	svc := newTestService()

	req := httptest.NewRequest("GET", "/api/models", nil)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body, _ := io.ReadAll(w.Body)
	if len(body) == 0 {
		t.Error("expected non-empty model list")
	}
}

func TestCORS(t *testing.T) {
	svc := newTestServiceWithCORS()

	req := httptest.NewRequest("OPTIONS", "/api/sessions", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if acao := w.Header().Get("Access-Control-Allow-Origin"); acao == "" {
		t.Error("expected ACAO header")
	}
}

// --- helpers ---

// stubChatModel is a no-op ChatModel so the session factory has a non-nil model
// (NewUnifiedAgent fails fast on nil). The service tests never drive a reply.
type stubChatModel struct{}

func (stubChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{}, nil
}

func (stubChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	ch := make(chan model.ChatResponse)
	close(ch)
	return ch, nil
}

func (stubChatModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int { return 0 }

func testFactory(name, prompt string, cm model.ChatModel) *agent.UnifiedAgent {
	if cm == nil {
		cm = stubChatModel{}
	}
	return agent.NewUnifiedAgent(name, prompt, cm)
}

func newTestService() *Service {
	return New(Config{Addr: ":0"}, nil, testFactory)
}

func newTestServiceWithCORS() *Service {
	return New(Config{Addr: ":0", AllowedOrigins: []string{"*"}}, nil, testFactory)
}

func createSession(t *testing.T, svc *Service) string {
	t.Helper()
	body := `{"agent_name":"a","system_prompt":"p"}`
	req := httptest.NewRequest("POST", "/api/session", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)
	var resp SessionResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return resp.SessionID
}
