package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
)

type chatHTTPResult struct {
	status int
	body   string
	err    error
}

func chatHTTPRequest(ctx context.Context, client *http.Client, url, body string) chatHTTPResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return chatHTTPResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return chatHTTPResult{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return chatHTTPResult{status: resp.StatusCode, body: string(data), err: err}
}

func requireChatHTTPStatus(t *testing.T, result chatHTTPResult, want int) {
	t.Helper()
	if result.err != nil || result.status != want {
		t.Fatalf("HTTP response = %d %q, error %v; want %d", result.status, result.body, result.err, want)
	}
}

// These tests cross a real localhost TCP connection. The provider is a local
// fixture so cancellation and delayed cleanup can be observed deterministically.
func TestChatCancelOverHTTP(t *testing.T) {
	for _, endpoint := range []string{"", "/stream"} {
		for _, streamingModel := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat%s/model-streaming=%t", endpoint, streamingModel), func(t *testing.T) {
				m := &cancelChatModel{started: make(chan context.Context, 1), canceled: make(chan struct{}), cleanup: make(chan struct{})}
				a, err := CreateApp(&AppConfig{AgentFactory: func(s *SessionRecord) (*agent.UnifiedAgent, error) {
					return agent.NewUnifiedAgent(s.AgentName, "test", m, agent.WithModelStreaming(streamingModel)), nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(a.Handler())
				t.Cleanup(server.Close)
				client := server.Client()
				client.Timeout = 5 * time.Second
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				result := make(chan chatHTTPResult, 1)
				created := chatHTTPRequest(ctx, client, server.URL+"/api/session", `{"agent_name":"test"}`)
				requireChatHTTPStatus(t, created, http.StatusCreated)
				var session struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal([]byte(created.body), &session); err != nil || session.ID == "" {
					t.Fatalf("invalid session response: %q (%v)", created.body, err)
				}
				chatURL := server.URL + "/api/chat/" + session.ID
				go func() {
					defer close(done)
					result <- chatHTTPRequest(ctx, client, chatURL+endpoint, `{"message":"first"}`)
				}()
				t.Cleanup(func() { cancel(); m.unblock(); awaitChatSignal(t, done, "HTTP client completion") })
				select {
				case <-m.started:
				case <-time.After(3 * time.Second):
					t.Fatal("HTTP chat did not reach the model")
				}
				requireChatHTTPStatus(t, chatHTTPRequest(ctx, client, chatURL+"/cancel", ""), http.StatusOK)
				awaitChatSignal(t, m.canceled, "HTTP cancel reaching the model")
				if ctx.Err() != nil {
					t.Fatal("client request context was canceled before model cancellation was observed")
				}
				requireChatHTTPStatus(t, chatHTTPRequest(ctx, client, chatURL+"/cancel", ""), http.StatusOK)
				requireChatHTTPStatus(t, chatHTTPRequest(ctx, client, chatURL+endpoint, `{"message":"too early"}`), http.StatusConflict)
				select {
				case <-done:
					t.Fatal("HTTP response completed before model cleanup")
				default:
				}
				m.unblock()
				awaitChatSignal(t, done, "canceled HTTP response completion")
				first := <-result
				if endpoint == "" {
					requireChatHTTPStatus(t, first, http.StatusInternalServerError)
				} else {
					requireChatHTTPStatus(t, first, http.StatusOK)
				}
				next := chatHTTPRequest(ctx, client, chatURL+endpoint, `{"message":"next"}`)
				requireChatHTTPStatus(t, next, http.StatusOK)
				if !strings.Contains(next.body, "done") {
					t.Fatalf("next HTTP chat did not return model output: %q", next.body)
				}
			})
		}
	}
}

func TestChatStreamDisconnectOverHTTP(t *testing.T) {
	m := &cancelChatModel{started: make(chan context.Context, 1), canceled: make(chan struct{}), cleanup: make(chan struct{})}
	a, err := CreateApp(&AppConfig{AgentFactory: func(s *SessionRecord) (*agent.UnifiedAgent, error) {
		return agent.NewUnifiedAgent(s.AgentName, "test", m, agent.WithModelStreaming(false)), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Observe server handler completion even though the first client disconnects.
	var first atomic.Bool
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") && first.CompareAndSwap(false, true) {
			defer close(handlerDone)
		}
		a.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	created := chatHTTPRequest(ctx, client, server.URL+"/api/session", `{"agent_name":"test"}`)
	requireChatHTTPStatus(t, created, http.StatusCreated)
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.body), &session); err != nil || session.ID == "" {
		t.Fatalf("invalid session response: %q (%v)", created.body, err)
	}
	chatURL := server.URL + "/api/chat/" + session.ID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL+"/stream", strings.NewReader(`{"message":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.unblock(); awaitChatSignal(t, handlerDone, "disconnected HTTP handler completion") })
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response = %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	select {
	case <-m.started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP stream did not reach the model")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	awaitChatSignal(t, m.canceled, "client disconnect reaching the model")
	requireChatHTTPStatus(t, chatHTTPRequest(ctx, client, chatURL, `{"message":"too early"}`), http.StatusConflict)
	m.unblock()
	awaitChatSignal(t, handlerDone, "disconnected HTTP handler cleanup")
	requireChatHTTPStatus(t, chatHTTPRequest(ctx, client, chatURL, `{"message":"next"}`), http.StatusOK)
}
