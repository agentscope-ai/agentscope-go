package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// The first call holds cleanup open after cancellation. Later calls finish
// normally, allowing the test to verify admission after the old run exits.
type cancelChatModel struct {
	started  chan context.Context
	canceled chan struct{}
	cleanup  chan struct{}
	once     sync.Once
	calls    atomic.Int32
}

func (m *cancelChatModel) unblock() { m.once.Do(func() { close(m.cleanup) }) }

func (m *cancelChatModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		m.started <- ctx
		<-ctx.Done()
		close(m.canceled)
		<-m.cleanup
		return nil, ctx.Err()
	}
	return &model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}}, nil
}

func (m *cancelChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	// Model stream setup can itself block while a connection is established.
	// Keep this cleanup synchronous; hidden provider goroutines are not part
	// of the agent core's completion contract.
	resp, err := m.Chat(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	out := make(chan model.ChatResponse, 1)
	out <- *resp
	close(out)
	return out, nil
}

func (*cancelChatModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func awaitChatSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for " + what)
	}
}

func chatCancelRequest(a *App, sessionID string) int {
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/chat/"+sessionID+"/cancel", nil))
	return w.Code
}

func TestChatCancelCancelsActiveRun(t *testing.T) {
	for _, endpoint := range []string{"", "/stream"} {
		for _, streamingModel := range []bool{false, true} {
			name := "chat" + endpoint + "/buffered-model"
			if streamingModel {
				name = "chat" + endpoint + "/stream-setup"
			}
			t.Run(name, func(t *testing.T) {
				m := &cancelChatModel{started: make(chan context.Context, 1), canceled: make(chan struct{}), cleanup: make(chan struct{})}
				a, err := CreateApp(&AppConfig{AgentFactory: func(s *SessionRecord) (*agent.UnifiedAgent, error) {
					return agent.NewUnifiedAgent(s.AgentName, "test", m, agent.WithModelStreaming(streamingModel)), nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				session := a.sessionSvc.Create(CreateSessionRequest{AgentName: "test"})
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/api/chat/"+session.ID+endpoint, strings.NewReader(`{"message":"first"}`)).WithContext(ctx)
				go func() { defer close(done); a.Handler().ServeHTTP(w, req) }()
				t.Cleanup(func() { cancel(); m.unblock(); awaitChatSignal(t, done, "handler cleanup") })
				select {
				case <-m.started:
				case <-time.After(3 * time.Second):
					t.Fatal("model did not start")
				}
				if got := chatCancelRequest(a, session.ID); got != http.StatusOK {
					t.Fatalf("cancel status = %d", got)
				}
				awaitChatSignal(t, m.canceled, "model cancellation from endpoint")
				if ctx.Err() != nil {
					t.Fatal("cancel endpoint canceled the parent request context")
				}
				if got := chatCancelRequest(a, session.ID); got != http.StatusOK {
					t.Fatalf("repeated cancel status = %d", got)
				}
				// A canceled stream may already have stopped forwarding. Actual
				// execution is still held at cleanup and must remain exclusive.
				next := httptest.NewRecorder()
				a.Handler().ServeHTTP(next, httptest.NewRequest(http.MethodPost, "/api/chat/"+session.ID, strings.NewReader(`{"message":"too early"}`)))
				if next.Code != http.StatusConflict {
					t.Fatalf("chat during cleanup = %d, want 409", next.Code)
				}
				m.unblock()
				awaitChatSignal(t, done, "canceled handler completion")
				if a.chatRegistry.IsRunning(session.ID) {
					t.Fatal("session still occupied after cleanup")
				}
				next = httptest.NewRecorder()
				a.Handler().ServeHTTP(next, httptest.NewRequest(http.MethodPost, "/api/chat/"+session.ID+endpoint, strings.NewReader(`{"message":"next"}`)))
				if next.Code != http.StatusOK {
					t.Fatalf("chat after cleanup = %d: %s", next.Code, next.Body.String())
				}
			})
		}
	}
}
