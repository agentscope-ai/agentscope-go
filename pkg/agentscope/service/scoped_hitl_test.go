package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/runctx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type ownedToolModel struct{ calls atomic.Int64 }

func (m *ownedToolModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "same-call", Name: "read_owned", Input: `{"marker":"server"}`, State: message.ToolCallPending, Extra: map[string]any{"fixture": "preserved"}}}, IsLast: true}, nil
	}
	text := "missing tool result"
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if result, ok := block.(message.ToolResultBlock); ok {
				text = result.GetOutputText()
			}
		}
	}
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: text}}, IsLast: true}, nil
}

type failedConfirmWriter struct{ *httptest.ResponseRecorder }

func (w failedConfirmWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "event: "+string(event.EventRequireUserConfirm)) {
		return 0, fmt.Errorf("fixture SSE failure")
	}
	return w.ResponseRecorder.Write(p)
}

func TestScopedSSEFailureCancelsAndClearsPending(t *testing.T) {
	var executions atomic.Int64
	s, err := NewScoped(Config{}, nil, testScope, func(_ context.Context, name, prompt string, _ model.ChatModel) (*agent.UnifiedAgent, error) {
		read := tool.NewFunctionTool("read_owned", "fixture", json.RawMessage(`{"type":"object","properties":{"marker":{"type":"string"}}}`), func(context.Context, map[string]any) (any, error) { executions.Add(1); return "should not run", nil })
		return agent.NewUnifiedAgent(name, prompt, &ownedToolModel{}, agent.WithToolkit(tool.NewToolkit(read)), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)), agent.WithModelStreaming(false)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id := createScoped(t, s, "owner")
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ownerKey{}, "owner"), 5*time.Second)
	defer cancel()
	ctx, cores := runctx.New(ctx)
	r := httptest.NewRequest("GET", "/api/chat/stream?session_id="+id+"&message=read", nil).WithContext(ctx)
	w := failedConfirmWriter{httptest.NewRecorder()}
	s.Handler().ServeHTTP(w, r)
	cores.Close()
	if ctx.Err() != nil || executions.Load() != 0 {
		t.Fatalf("SSE failure did not cancel promptly: ctx=%v executions=%d", ctx.Err(), executions.Load())
	}
	s.mu.RLock()
	pending, active := s.sessions[id].pending, s.sessions[id].activeReply
	s.mu.RUnlock()
	if pending != nil || active {
		t.Fatal("failed stream retained pending input or reply ownership")
	}
	confirm := scopedRequest(s, "owner", "POST", "/api/confirm", `{"session_id":"`+id+`","tool_calls":[{"id":"same-call","confirmed":true}]}`)
	if confirm.Code != 400 {
		t.Fatalf("closed stream accepted confirmation: %d", confirm.Code)
	}
}
func (*ownedToolModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*ownedToolModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

// Fixture authentication maps fixed credentials to a trusted context value.
// This does not implement or test a real identity provider.
func fixtureAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := map[string]string{"fixture-A": "tenant-A/user", "fixture-B": "tenant-B/user"}[r.Header.Get("Authorization")]
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerKey{}, owner)))
	})
}

func fixtureHTTP(t *testing.T, ctx context.Context, client *http.Client, token, method, url, body string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", token)
	r.Header.Set("X-User-ID", "tenant-A/user")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pendingTool(t *testing.T, resp *http.Response) (*bufio.Scanner, string) {
	t.Helper()
	if resp.StatusCode != 200 {
		t.Fatalf("stream=%d %s", resp.StatusCode, responseBody(t, resp))
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if scanner.Text() != "event: "+string(event.EventRequireUserConfirm) {
			continue
		}
		if !scanner.Scan() {
			break
		}
		var confirm event.RequireUserConfirmEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &confirm); err != nil || len(confirm.ToolCalls) != 1 {
			t.Fatalf("confirmation=%q err=%v", scanner.Text(), err)
		}
		return scanner, confirm.ToolCalls[0].ID
	}
	t.Fatalf("no pending tool confirmation: %v", scanner.Err())
	return nil, ""
}

func finishToolStream(t *testing.T, scanner *bufio.Scanner, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var out strings.Builder
	for scanner.Scan() {
		out.WriteString(scanner.Text())
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestScopedPendingConfirmationAndOwnedResources(t *testing.T) {
	owners := []string{"tenant-A/user", "tenant-B/user"}
	paths := make(map[string]string)
	counts := map[string]*atomic.Int64{owners[0]: {}, owners[1]: {}}
	for i, owner := range owners {
		path := filepath.Join(t.TempDir(), "resource.txt")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("owned-resource-%d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[owner] = path
	}
	s, err := NewScoped(Config{}, nil, testScope, func(ctx context.Context, name, prompt string, _ model.ChatModel) (*agent.UnifiedAgent, error) {
		owner, _ := ctx.Value(ownerKey{}).(string)
		read := tool.NewFunctionTool("read_owned", "Read this owner's fixture", json.RawMessage(`{"type":"object","properties":{"marker":{"type":"string"}},"required":["marker"]}`), func(callCtx context.Context, input map[string]any) (any, error) {
			if callCtx.Value(ownerKey{}) != owner {
				return nil, fmt.Errorf("tool received another owner's context")
			}
			if input["marker"] != "server" {
				return nil, fmt.Errorf("confirmation changed tool arguments")
			}
			counts[owner].Add(1)
			b, err := os.ReadFile(paths[owner])
			return string(b), err
		})
		return agent.NewUnifiedAgent(name, prompt, &ownedToolModel{}, agent.WithToolkit(tool.NewToolkit(read)), agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)), agent.WithModelStreaming(false), agent.WithReactConfig(agent.ReactConfig{MaxIters: 3})), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a := createScoped(t, s, owners[0])
	b := createScoped(t, s, owners[1])
	srv := httptest.NewServer(fixtureAuthenticated(s.Handler()))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streamA := fixtureHTTP(t, ctx, srv.Client(), "fixture-A", "GET", srv.URL+"/api/chat/stream?session_id="+a+"&message=read", "")
	scanA, callA := pendingTool(t, streamA)
	streamB := fixtureHTTP(t, ctx, srv.Client(), "fixture-B", "GET", srv.URL+"/api/chat/stream?session_id="+b+"&message=read", "")
	scanB, callB := pendingTool(t, streamB)
	if callA != callB {
		t.Fatal("fixture must exercise identical pending call IDs across owners")
	}
	busy := fixtureHTTP(t, ctx, srv.Client(), "fixture-A", "GET", srv.URL+"/api/chat/stream?session_id="+a+"&message=overlap", "")
	if body := responseBody(t, busy); busy.StatusCode != 409 {
		t.Fatalf("overlapping stream=%d %s", busy.StatusCode, body)
	}
	for _, answers := range []string{`[]`, `[{"id":"missing","confirmed":true}]`, `[{"id":"same-call","confirmed":true},{"id":"same-call","confirmed":true}]`, `[{"id":"same-call","confirmed":true},{"id":"missing","confirmed":true}]`} {
		bad := fixtureHTTP(t, ctx, srv.Client(), "fixture-A", "POST", srv.URL+"/api/confirm", fmt.Sprintf(`{"session_id":%q,"tool_calls":%s}`, a, answers))
		if body := responseBody(t, bad); bad.StatusCode != 400 {
			t.Fatalf("invalid pending set=%d %s", bad.StatusCode, body)
		}
	}
	submit := func(token, id, call string, allow bool) *http.Response {
		body := fmt.Sprintf(`{"session_id":%q,"tool_calls":[{"id":%q,"confirmed":%t,"name":"forged","input":"forged"}]}`, id, call, allow)
		return fixtureHTTP(t, ctx, srv.Client(), token, "POST", srv.URL+"/api/confirm", body)
	}
	for _, id := range []string{a, "missing-session"} {
		r := submit("fixture-B", id, callA, true)
		body := responseBody(t, r)
		if r.StatusCode != 404 {
			t.Fatalf("foreign confirmation=%d %s", r.StatusCode, body)
		}
	}
	type confirmationReply struct {
		status int
		err    error
	}
	replies := make(chan confirmationReply, 2)
	for range 2 {
		request, err := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/confirm", strings.NewReader(fmt.Sprintf(`{"session_id":%q,"tool_calls":[{"id":%q,"confirmed":true}]}`, b, callB)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "fixture-B")
		go func() {
			response, err := srv.Client().Do(request)
			if err != nil {
				replies <- confirmationReply{err: err}
				return
			}
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, response.Body)
			replies <- confirmationReply{response.StatusCode, err}
		}()
	}
	statuses := make(map[int]int)
	for range 2 {
		response := <-replies
		if response.err != nil {
			t.Fatal(response.err)
		}
		statuses[response.status]++
	}
	if statuses[200] != 1 || statuses[400] != 1 {
		t.Fatalf("concurrent valid confirmations=%v", statuses)
	}
	if out := finishToolStream(t, scanB, streamB); !strings.Contains(out, "owned-resource-1") {
		t.Fatalf("B lost scoped tool output: %s", out)
	}
	// Owner denial after the foreign approval attempt makes unintended execution
	// observable even if an incorrectly submitted approval were scheduled later.
	r := submit("fixture-A", a, callA, false)
	if body := responseBody(t, r); r.StatusCode != 200 {
		t.Fatalf("A denial=%d %s", r.StatusCode, body)
	}
	finishToolStream(t, scanA, streamA)
	if counts[owners[0]].Load() != 0 || counts[owners[1]].Load() != 1 {
		t.Fatalf("foreign confirmation advanced a tool: A=%d B=%d", counts[owners[0]].Load(), counts[owners[1]].Load())
	}
	a2 := createScoped(t, s, owners[0])
	streamA2 := fixtureHTTP(t, ctx, srv.Client(), "fixture-A", "GET", srv.URL+"/api/chat/stream?session_id="+a2+"&message=read", "")
	scanA2, callA2 := pendingTool(t, streamA2)
	r = submit("fixture-A", a2, callA2, true)
	if body := responseBody(t, r); r.StatusCode != 200 {
		t.Fatalf("A approval=%d %s", r.StatusCode, body)
	}
	if out := finishToolStream(t, scanA2, streamA2); !strings.Contains(out, "owned-resource-0") {
		t.Fatalf("A lost scoped tool output: %s", out)
	}
	if counts[owners[0]].Load() != 1 || counts[owners[1]].Load() != 1 {
		t.Fatal("tool invocation counts differ")
	}
}

type heldScopedModel struct {
	entered chan context.Context
	release chan struct{}
	calls   atomic.Int64
}

type cancelReplyForwarder struct{ middleware.BaseMiddleware }

func (*cancelReplyForwarder) OnReply(ctx context.Context, in middleware.ReplyInput, next middleware.ReplyHandler) <-chan event.Event {
	source := next(ctx, in)
	out := make(chan event.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-source:
				if !ok {
					return
				}
				select {
				case out <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (m *heldScopedModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		m.entered <- ctx
		<-m.release
	}
	return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "released"}}, IsLast: true}, nil
}
func (*heldScopedModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*heldScopedModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func TestScopedCanceledReplyRetainsSlotUntilCoreExit(t *testing.T) {
	m := &heldScopedModel{entered: make(chan context.Context, 1), release: make(chan struct{})}
	var otherCalls atomic.Int64
	s, err := NewScoped(Config{}, nil, testScope, func(ctx context.Context, name, prompt string, _ model.ChatModel) (*agent.UnifiedAgent, error) {
		var cm model.ChatModel = m
		if ctx.Value(ownerKey{}) != "tenant-A/user" {
			cm = scopedModel{"other-owner", &otherCalls}
		}
		opts := []agent.AgentOption{agent.WithModelStreaming(false)}
		if ctx.Value(ownerKey{}) == "tenant-A/user" {
			// A public forwarder may finish on cancellation before the accepted
			// core returns. The scoped handler must still retain core ownership.
			opts = append(opts, agent.WithMiddlewares(&cancelReplyForwarder{}))
		}
		return agent.NewUnifiedAgent(name, prompt, cm, opts...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := createScoped(t, s, "tenant-A/user"), createScoped(t, s, "tenant-B/user")
	firstDone := make(chan struct{})
	h := fixtureAuthenticated(s.Handler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.URL.Query().Get("message") == "hold" {
			close(firstDone)
		}
	}))
	defer srv.Close()
	var once sync.Once
	release := func() { once.Do(func() { close(m.release) }) }
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := fixtureHTTP(t, ctx, srv.Client(), "fixture-A", "GET", srv.URL+"/api/chat/stream?session_id="+a+"&message=hold", "")
	var coreCtx context.Context
	select {
	case coreCtx = <-m.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("held core did not enter")
	}
	probe, accepted := runctx.Start(coreCtx)
	if !accepted {
		t.Fatal("core scope was already sealed")
	}
	probe()
	cancel()
	_ = resp.Body.Close()
	// Observe cleanup sealing its execution scope before asserting the HTTP
	// slot. This makes dropping cores.Close fail without a sleep-based race.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		probe, accepted = runctx.Start(coreCtx)
		if !accepted {
			break
		}
		probe()
		select {
		case <-tick.C:
		case <-firstDone:
			t.Fatal("handler returned with an admitted core held")
		case <-deadline.C:
			t.Fatal("cleanup never sealed its owned core scope")
		}
	}
	if w := scopedRequest(s, "tenant-A/user", "POST", "/api/chat", `{"session_id":"`+a+`","message":"overlap"}`); w.Code != 409 {
		t.Fatalf("held session admitted overlap: %d %s", w.Code, w.Body.String())
	}
	if w := scopedRequest(s, "tenant-B/user", "POST", "/api/chat", `{"session_id":"`+b+`","message":"independent"}`); w.Code != 200 || otherCalls.Load() != 1 {
		t.Fatalf("held core blocked another owner: %d %s", w.Code, w.Body.String())
	}
	release()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish after core release")
	}
	if w := scopedRequest(s, "tenant-A/user", "POST", "/api/chat", `{"session_id":"`+a+`","message":"after cleanup"}`); w.Code != 200 {
		t.Fatalf("session slot was not released: %d %s", w.Code, w.Body.String())
	}
}

type scopedCancelModel struct{ entered, exited chan struct{} }

func (m scopedCancelModel) Chat(ctx context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	close(m.entered)
	<-ctx.Done()
	close(m.exited)
	return nil, ctx.Err()
}
func (scopedCancelModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (scopedCancelModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func TestScopedHTTPDisconnect(t *testing.T) {
	m := scopedCancelModel{make(chan struct{}), make(chan struct{})}
	s, err := NewScoped(Config{}, m, testScope, func(_ context.Context, name, prompt string, cm model.ChatModel) (*agent.UnifiedAgent, error) {
		return agent.NewUnifiedAgent(name, prompt, cm, agent.WithModelStreaming(false)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id := createScoped(t, s, "tenant-A/user")
	handlerDone := make(chan struct{})
	h := fixtureAuthenticated(s.Handler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r); close(handlerDone) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/chat", strings.NewReader(`{"session_id":"`+id+`","message":"wait"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "fixture-A")
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := srv.Client().Do(r)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-m.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	cancel()
	for label, done := range map[string]<-chan struct{}{"model": m.exited, "handler": handlerDone, "client": clientDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not exit", label)
		}
	}
	if len(scopedList(t, s, "tenant-A/user")) != 1 {
		t.Fatal("reply cancellation revoked the session")
	}
}
