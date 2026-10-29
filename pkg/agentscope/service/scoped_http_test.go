package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpsec"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type scopedModel struct {
	text  string
	calls *atomic.Int64
}

func (m scopedModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.calls.Add(1)
	owner, _ := ctx.Value(ownerKey{}).(string)
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: m.text + "|request=" + owner}}, IsLast: true}, nil
}
func (scopedModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (scopedModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func echoScopedService(t *testing.T, calls *atomic.Int64) *Service {
	t.Helper()
	s, err := NewScoped(Config{}, nil, testScope, func(ctx context.Context, name, prompt string, _ model.ChatModel) (*agent.UnifiedAgent, error) {
		owner, _ := ctx.Value(ownerKey{}).(string)
		return agent.NewUnifiedAgent(name, prompt, scopedModel{owner, calls}, agent.WithModelStreaming(false)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func scopedList(t *testing.T, s *Service, owner string) []SessionResponse {
	t.Helper()
	w := scopedRequest(s, owner, "GET", "/api/sessions", "")
	var out []SessionResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return out
}

func TestScopedConstructorValidation(t *testing.T) {
	factory := func(_ context.Context, name, prompt string, cm model.ChatModel) (*agent.UnifiedAgent, error) {
		return testFactory(name, prompt, cm), nil
	}
	for _, tc := range []struct {
		name     string
		resolver ScopeResolver
		factory  ContextAgentFactory
	}{
		{"resolver", nil, factory},
		{"factory", testScope, nil},
		{"both", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewScoped(Config{}, nil, tc.resolver, tc.factory)
			if err == nil || s != nil {
				t.Fatalf("service=%v error=%v", s, err)
			}
		})
	}
}

func TestScopedAllSessionRoutes(t *testing.T) {
	var calls atomic.Int64
	s := echoScopedService(t, &calls)
	owners := []string{"tenant-A/shared-user", "tenant-B/shared-user", "tenant-A/other-user", " opaque ", "opaque"}
	ids := make(map[string]string)
	for _, owner := range owners {
		id := createScoped(t, s, owner)
		ids[owner] = id
		if w := scopedRequest(s, owner, "GET", "/api/session/"+id, ""); w.Code != 200 || strings.Contains(w.Body.String(), owner) {
			t.Fatalf("own get/metadata: %d %s", w.Code, w.Body.String())
		}
		for _, stream := range []bool{false, true} {
			method, path, body := "POST", "/api/chat", `{"session_id":"`+id+`","message":"hello"}`
			if stream {
				method, path, body = "GET", "/api/chat/stream?session_id="+id+"&message=hello", ""
			}
			w := scopedRequest(s, owner, method, path, body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), owner+"|request="+owner) {
				t.Fatalf("own chat/stream: %d %s", w.Code, w.Body.String())
			}
		}
		if w := scopedRequest(s, owner, "POST", "/api/confirm", `{"session_id":"`+id+`","tool_calls":[]}`); w.Code != 400 {
			t.Fatalf("own confirm route: %d %s", w.Code, w.Body.String())
		}
	}
	for _, owner := range owners {
		list := scopedList(t, s, owner)
		if len(list) != 1 || list[0].SessionID != ids[owner] {
			t.Fatalf("%q list=%+v", owner, list)
		}
	}
	before := calls.Load()
	a := ids[owners[0]]
	for _, route := range []string{"get", "delete", "chat", "stream", "confirm"} {
		t.Run(route, func(t *testing.T) {
			var responses [2]*httptest.ResponseRecorder
			for i, id := range []string{a, "missing-session"} {
				method, path, body := "GET", "/api/session/"+id, ""
				switch route {
				case "delete":
					method = "DELETE"
				case "chat":
					method, path, body = "POST", "/api/chat", `{"session_id":"`+id+`","message":"hello"}`
				case "stream":
					path = "/api/chat/stream?session_id=" + id + "&message=hello"
				case "confirm":
					method, path, body = "POST", "/api/confirm", `{"session_id":"`+id+`","tool_calls":[]}`
				}
				responses[i] = scopedRequest(s, owners[1], method, path, body)
				if responses[i].Code != 404 || strings.Contains(responses[i].Header().Get("Content-Type"), "event-stream") {
					t.Fatalf("foreign/unknown: %d %s", responses[i].Code, responses[i].Body.String())
				}
			}
			if responses[0].Body.String() != responses[1].Body.String() {
				t.Fatal("foreign and unknown sessions differ")
			}
		})
	}
	if calls.Load() != before {
		t.Fatal("rejected session invoked model")
	}
	if w := scopedRequest(s, owners[0], "GET", "/api/session/"+a, ""); w.Code != 200 {
		t.Fatal("foreign delete removed the owner's session")
	}
	if w := scopedRequest(s, owners[0], "DELETE", "/api/session/"+a, ""); w.Code != 204 {
		t.Fatalf("own delete=%d", w.Code)
	}
	if w := scopedRequest(s, owners[0], "GET", "/api/session/"+a, ""); w.Code != 404 {
		t.Fatalf("deleted session=%d", w.Code)
	}
}

type observedBody struct {
	r          io.Reader
	reads      int
	beforeRead func()
}

func (b *observedBody) Read(p []byte) (int, error) {
	b.reads++
	if b.beforeRead != nil {
		b.beforeRead()
	}
	return b.r.Read(p)
}
func (*observedBody) Close() error { return nil }

func TestScopedIdentityBeforeBodyAndLimits(t *testing.T) {
	for _, outcome := range []string{"empty", "blank", "error"} {
		for _, route := range []struct{ method, path string }{
			{"POST", "/api/session"}, {"POST", "/api/chat"}, {"POST", "/api/confirm"},
			{"GET", "/api/session/x"}, {"DELETE", "/api/session/x"},
			{"GET", "/api/sessions"}, {"GET", "/api/chat/stream?session_id=x&message=m"},
		} {
			t.Run(outcome+route.method+route.path, func(t *testing.T) {
				resolved, constructed := 0, 0
				s, err := NewScoped(Config{}, nil, func(context.Context) (string, error) {
					resolved++
					switch outcome {
					case "blank":
						return " \t", nil
					case "error":
						return "owner", errors.New("private resolver failure")
					default:
						return "", nil
					}
				}, func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error) {
					constructed++
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				body := &observedBody{r: strings.NewReader("invalid JSON")}
				r := httptest.NewRequest(route.method, route.path, nil)
				r.Body, r.ContentLength = body, httpsec.DefaultMaxBodyBytes+1
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 401 || resolved != 1 || constructed != 0 || body.reads != 0 || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("status=%d resolved=%d factory=%d reads=%d body=%s", w.Code, resolved, constructed, body.reads, w.Body.String())
				}
			})
		}
	}
}

func TestScopedBoundedJSON(t *testing.T) {
	largeTail := `{}` + strings.Repeat(" ", int(httpsec.DefaultMaxBodyBytes))
	largeValue := `{"message":"` + strings.Repeat("x", int(httpsec.DefaultMaxBodyBytes)) + `"}`
	for _, path := range []string{"/api/session", "/api/chat", "/api/confirm"} {
		for _, tc := range []struct {
			name, body string
			known      bool
			status     int
		}{
			{"unknown-length-tail", largeTail, false, 413},
			{"known-length-tail", largeTail, true, 413},
			{"unknown-length-value", largeValue, false, 413},
			{"two-documents", `{} {}`, false, 400},
			{"invalid-tail", `{} invalid`, false, 400},
			{"invalid-first", `invalid`, false, 400},
		} {
			t.Run(path+tc.name, func(t *testing.T) {
				resolved, constructed := false, 0
				s, err := NewScoped(Config{}, nil, func(context.Context) (string, error) { resolved = true; return "owner", nil }, func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error) {
					constructed++
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				body := &observedBody{r: strings.NewReader(tc.body), beforeRead: func() {
					if !resolved {
						t.Error("body read before identity")
					}
				}}
				r := httptest.NewRequest("POST", path, nil)
				r.Body, r.ContentLength = body, -1
				if tc.known {
					r.ContentLength = int64(len(tc.body))
				}
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != tc.status || constructed != 0 {
					t.Fatalf("status=%d factory=%d body=%s", w.Code, constructed, w.Body.String())
				}
				if tc.known && body.reads != 0 {
					t.Fatal("known oversized body was read")
				}
			})
		}
	}
	var calls atomic.Int64
	s := echoScopedService(t, &calls)
	if w := scopedRequest(s, "owner", "POST", "/api/session", "{} \n\t"); w.Code != 201 {
		t.Fatalf("bounded whitespace=%d %s", w.Code, w.Body.String())
	}
	// Legacy single-Decode behavior remains compatible.
	legacy := newTestService()
	w := httptest.NewRecorder()
	legacy.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/api/session", strings.NewReader(largeTail)))
	if w.Code != 201 {
		t.Fatalf("legacy trailing data behavior changed: %d", w.Code)
	}
}

func TestScopedNonSessionRoutesAndPreflight(t *testing.T) {
	var resolutions atomic.Int64
	s, err := NewScoped(Config{AllowedOrigins: []string{"https://example.test"}}, nil, func(context.Context) (string, error) { resolutions.Add(1); return "", errors.New("no identity") }, func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/healthz", "/readyz", "/api/models"} {
		if w := scopedRequest(s, "", "GET", path, ""); w.Code != 200 {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
	r := httptest.NewRequest("OPTIONS", "/api/session", nil)
	r.Header.Set("Origin", "https://example.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "https://example.test" || resolutions.Load() != 0 {
		t.Fatalf("preflight=%d resolves=%d", w.Code, resolutions.Load())
	}
}

func TestScopedFactoryFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"error", "nil", "agent-and-error"} {
		t.Run(mode, func(t *testing.T) {
			cm := stubChatModel{}
			s, err := NewScoped(Config{}, cm, testScope, func(ctx context.Context, name, prompt string, passed model.ChatModel) (*agent.UnifiedAgent, error) {
				if ctx.Value(ownerKey{}) != "owner" || name != "custom" || prompt != "custom prompt" || passed != cm {
					t.Error("factory lost request/configuration")
				}
				if mode == "nil" {
					return nil, nil
				}
				if mode == "agent-and-error" {
					return testFactory(name, prompt, cm), errors.New("private failure")
				}
				return nil, errors.New("private failure")
			})
			if err != nil {
				t.Fatal(err)
			}
			w := scopedRequest(s, "owner", "POST", "/api/session", `{"agent_name":"custom","system_prompt":"custom prompt"}`)
			if w.Code != 500 || strings.Contains(w.Body.String(), "private") || len(scopedList(t, s, "owner")) != 0 {
				t.Fatalf("failed factory: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, successAfterCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled-error", true: "canceled-success"}[successAfterCancel], func(t *testing.T) {
			entered, release, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			s, err := NewScoped(Config{}, nil, testScope, func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error) {
				close(entered)
				<-release
				// The factory owns this cleanup, including discarded successful results.
				close(cleaned)
				if successAfterCancel {
					return testFactory("a", "p", nil), nil
				}
				return nil, context.Canceled
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, "owner"))
			defer cancel()
			r := httptest.NewRequest("POST", "/api/session", strings.NewReader(`{}`)).WithContext(ctx)
			w, done := httptest.NewRecorder(), make(chan struct{})
			go func() { defer close(done); s.Handler().ServeHTTP(w, r) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("factory did not enter")
			}
			cancel()
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("construction did not return")
			}
			if w.Code != 408 || len(scopedList(t, s, "owner")) != 0 {
				t.Fatalf("canceled factory: %d %s", w.Code, w.Body.String())
			}
			select {
			case <-cleaned:
			default:
				t.Fatal("factory cleanup did not finish")
			}
		})
	}
}

func TestScopedCanceledBeforeConstructionAndRequestLifetime(t *testing.T) {
	for _, cancelDuringRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-body", true: "during-body"}[cancelDuringRead], func(t *testing.T) {
			constructed := 0
			s, err := NewScoped(Config{}, nil, testScope, func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error) {
				constructed++
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, "owner"))
			defer cancel()
			body := &observedBody{r: strings.NewReader(`{}`)}
			if cancelDuringRead {
				body.beforeRead = cancel
			} else {
				cancel()
			}
			r := httptest.NewRequest("POST", "/api/session", nil).WithContext(ctx)
			r.Body = body
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 408 || constructed != 0 || (!cancelDuringRead && body.reads != 0) {
				t.Fatalf("status=%d factory=%d reads=%d", w.Code, constructed, body.reads)
			}
		})
	}
	var calls atomic.Int64
	s := echoScopedService(t, &calls)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, "owner"))
	r := httptest.NewRequest("POST", "/api/session", strings.NewReader(`{}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	cancel()
	var session SessionResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &session) != nil {
		t.Fatal(w.Body.String())
	}
	w = scopedRequest(s, "owner", "POST", "/api/chat", `{"session_id":"`+session.SessionID+`","message":"new request"}`)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("creation context governed later reply: %d %s", w.Code, w.Body.String())
	}
}

func TestScopedConcurrentConstruction(t *testing.T) {
	var entered atomic.Int64
	both, release := make(chan struct{}), make(chan struct{})
	s, err := NewScoped(Config{}, nil, testScope, func(_ context.Context, name, prompt string, cm model.ChatModel) (*agent.UnifiedAgent, error) {
		if entered.Add(1) == 2 {
			close(both)
		}
		<-release
		return testFactory(name, prompt, cm), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 12)
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "A"
			if i%2 != 0 {
				owner = "B"
			}
			results <- scopedRequest(s, owner, "POST", "/api/session", `{}`)
		}(i)
	}
	select {
	case <-both:
	case <-time.After(5 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("factory calls serialized under registry lock")
	}
	close(release)
	wg.Wait()
	close(results)
	for w := range results {
		if w.Code != 201 {
			t.Errorf("concurrent create: %d %s", w.Code, w.Body.String())
		}
	}
	for _, owner := range []string{"A", "B"} {
		if n := len(scopedList(t, s, owner)); n != 6 {
			t.Fatalf("%s sessions=%d", owner, n)
		}
	}
}
