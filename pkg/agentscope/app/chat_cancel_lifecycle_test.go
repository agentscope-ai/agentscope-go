package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agenttest"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type closingReplyMiddleware struct {
	middleware.BaseMiddleware
	closed chan struct{}
	once   sync.Once
}

func (m *closingReplyMiddleware) OnReply(ctx context.Context, in middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	source := next(ctx, in)
	out := make(chan event.Event)
	go func() {
		defer m.once.Do(func() { close(m.closed) })
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-source:
				if !ok {
					return
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func TestChatCancelJoinsToolAfterForwarderCloses(t *testing.T) {
	for _, endpoint := range []string{"", "/stream"} {
		t.Run("chat"+endpoint, func(t *testing.T) {
			entered, canceled, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(cleanup) }) }
			hold := tool.NewFunctionTool("hold", "fixture", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ map[string]any) (any, error) {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-cleanup
				return "tool cleanup complete", nil
			})
			m := agenttest.NewMockModel(agenttest.OnNthCall(1, agenttest.RespondWithToolCall("hold", map[string]any{})), agenttest.Default(agenttest.RespondWithText("done")))
			mw := &closingReplyMiddleware{closed: make(chan struct{})}
			a, err := CreateApp(&AppConfig{AgentFactory: func(s *SessionRecord) (*agent.UnifiedAgent, error) {
				return agent.NewUnifiedAgent(s.AgentName, "test", m, agent.WithToolkit(tool.NewToolkit(hold)), agent.WithMiddlewares(mw)), nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			s := a.sessionSvc.Create(CreateSessionRequest{AgentName: "test"})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat/"+s.ID+endpoint, strings.NewReader(`{"message":"hold"}`)).WithContext(ctx))
			}()
			t.Cleanup(func() { cancel(); unblock(); awaitChatSignal(t, done, "tool handler cleanup") })
			awaitChatSignal(t, entered, "tool entry")
			chatCancelRequest(a, s.ID)
			awaitChatSignal(t, canceled, "tool cancellation")
			awaitChatSignal(t, mw.closed, "middleware stream closing before tool exits")
			select {
			case <-done:
				t.Fatal("handler released execution while tool still owns state")
			default:
			}
			next := httptest.NewRecorder()
			a.Handler().ServeHTTP(next, httptest.NewRequest(http.MethodPost, "/api/chat/"+s.ID, strings.NewReader(`{"message":"overlap"}`)))
			if next.Code != http.StatusConflict {
				t.Fatalf("overlap status = %d", next.Code)
			}
			unblock()
			awaitChatSignal(t, done, "tool and handler completion")
			if a.chatRegistry.IsRunning(s.ID) {
				t.Fatal("finished tool retained ownership")
			}
		})
	}
}

func TestChatCancelSessionIsolationAndBackgroundTasks(t *testing.T) {
	a, err := CreateApp(&AppConfig{})
	if err != nil {
		t.Fatal(err)
	}
	first, finishFirst, _ := a.acquireChat(context.Background(), "first")
	defer finishFirst()
	other, finishOther, _ := a.acquireChat(context.Background(), "other")
	defer finishOther()
	bgFirst, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	bgOther, bgOtherCancel := context.WithCancel(context.Background())
	defer bgOtherCancel()
	a.bgManager.Track("first", bgCancel)
	a.bgManager.Track("other", bgOtherCancel)
	if got := chatCancelRequest(a, "first"); got != http.StatusOK {
		t.Fatalf("cancel status = %d", got)
	}
	if first.Err() == nil || bgFirst.Err() == nil {
		t.Fatal("session chat/background task was not canceled")
	}
	if other.Err() != nil || bgOther.Err() != nil {
		t.Fatal("another session was canceled")
	}
	if got := chatCancelRequest(a, "idle"); got != http.StatusOK {
		t.Fatalf("idle cancel status = %d", got)
	}
}

func TestChatRetainsParentCancellation(t *testing.T) {
	for _, endpoint := range []string{"", "/stream"} {
		for _, deadline := range []bool{false, true} {
			name := "chat" + endpoint + "/cancel"
			if deadline {
				name = "chat" + endpoint + "/deadline"
			}
			t.Run(name, func(t *testing.T) {
				m := &cancelChatModel{started: make(chan context.Context, 1), canceled: make(chan struct{}), cleanup: make(chan struct{})}
				m.unblock()
				a, err := CreateApp(&AppConfig{AgentFactory: func(s *SessionRecord) (*agent.UnifiedAgent, error) {
					return agent.NewUnifiedAgent(s.AgentName, "test", m, agent.WithModelStreaming(false)), nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				s := a.sessionSvc.Create(CreateSessionRequest{AgentName: "test"})
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				}
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					a.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat/"+s.ID+endpoint, strings.NewReader(`{"message":"wait"}`)).WithContext(ctx))
				}()
				t.Cleanup(func() { cancel(); awaitChatSignal(t, done, "parent canceled handler") })
				select {
				case <-m.started:
				case <-time.After(3 * time.Second):
					t.Fatal("model not started")
				}
				if !deadline {
					cancel()
				}
				awaitChatSignal(t, m.canceled, "parent cancellation")
				awaitChatSignal(t, done, "handler completion")
				if a.chatRegistry.IsRunning(s.ID) {
					t.Fatal("parent cancellation retained session")
				}
			})
		}
	}
}

func TestChatSetupFailureReleasesAdmission(t *testing.T) {
	for _, endpoint := range []string{"", "/stream"} {
		a, err := CreateApp(&AppConfig{AgentFactory: func(*SessionRecord) (*agent.UnifiedAgent, error) { return nil, errors.New("fixture factory failure") }})
		if err != nil {
			t.Fatal(err)
		}
		s := a.sessionSvc.Create(CreateSessionRequest{AgentName: "test"})
		for range 2 {
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/chat/"+s.ID+endpoint, strings.NewReader(`{"message":"hello"}`)))
			if w.Code != http.StatusInternalServerError || a.chatRegistry.IsRunning(s.ID) {
				t.Fatalf("setup failure status=%d, retained=%t", w.Code, a.chatRegistry.IsRunning(s.ID))
			}
		}
	}
}

type establishedCancelModel struct{ *cancelChatModel }

func (m *establishedCancelModel) ChatStream(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	out := make(chan model.ChatResponse, 1)
	out <- model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}}
	m.started <- ctx
	go func() { defer close(out); <-ctx.Done(); close(m.canceled) }()
	return out, nil
}

func TestChatCancelEstablishedProviderStream(t *testing.T) {
	for _, endpoint := range []string{"", "/stream"} {
		m := &establishedCancelModel{&cancelChatModel{started: make(chan context.Context, 1), canceled: make(chan struct{})}}
		a, err := CreateApp(&AppConfig{AgentFactory: func(s *SessionRecord) (*agent.UnifiedAgent, error) {
			return agent.NewUnifiedAgent(s.AgentName, "test", m), nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		s := a.sessionSvc.Create(CreateSessionRequest{AgentName: "test"})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat/"+s.ID+endpoint, strings.NewReader(`{"message":"wait"}`)).WithContext(ctx))
		}()
		t.Cleanup(func() { cancel(); awaitChatSignal(t, done, "established stream handler cleanup") })
		select {
		case <-m.started:
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("stream did not start")
		}
		chatCancelRequest(a, s.ID)
		awaitChatSignal(t, m.canceled, "established stream cancellation")
		awaitChatSignal(t, done, "stream handler cleanup")
		cancel()
		if a.chatRegistry.IsRunning(s.ID) {
			t.Fatal("established stream retained session")
		}
	}
}
