// An offline shared Agent Service with fixed authentication fixtures and
// owner-specific read-only tools. No model credentials or external server needed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/service"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type principal struct{ Tenant, User string }
type principalKey struct{}

type fixtureModel struct{ calls atomic.Int64 }

func (m *fixtureModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "read", Name: "read_owned", Input: `{}`, State: message.ToolCallPending}}, IsLast: true}, nil
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
func (*fixtureModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (*fixtureModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

func request(client *http.Client, base, token, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, 0, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	// Deliberately conflicting raw claims are ignored by the resolver.
	r.Header.Set("X-User-ID", "someone-else")
	resp, err := client.Do(r)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func run() error {
	dir, err := os.MkdirTemp("", "scoped-service-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, tenant := range []string{"A", "B"} {
		if err := os.WriteFile(filepath.Join(dir, tenant+".txt"), []byte("fixture for tenant "+tenant), 0o600); err != nil {
			return err
		}
	}
	// The host, not the Service, owns the fixture files and their cleanup.
	resolve := func(ctx context.Context) (string, error) {
		p, ok := ctx.Value(principalKey{}).(principal)
		if !ok || p.Tenant == "" || p.User == "" {
			return "", fmt.Errorf("missing authenticated principal")
		}
		b, err := json.Marshal(p)
		return string(b), err
	}
	factory := func(ctx context.Context, name, prompt string, _ model.ChatModel) (*agent.UnifiedAgent, error) {
		p := ctx.Value(principalKey{}).(principal)
		path := filepath.Join(dir, p.Tenant+".txt")
		read := tool.NewFunctionTool("read_owned", "Read the owner's fixed fixture", json.RawMessage(`{"type":"object","properties":{}}`), func(context.Context, map[string]any) (any, error) { b, err := os.ReadFile(path); return string(b), err })
		read.ReadOnly = true
		// Capture immutable configuration, never the construction request context.
		return agent.NewUnifiedAgent(name, prompt, &fixtureModel{}, agent.WithToolkit(tool.NewToolkit(read)), agent.WithPermissionContext(permission.NewContext(permission.ModeExplore)), agent.WithModelStreaming(false)), nil
	}
	svc, err := service.NewScoped(service.Config{}, nil, resolve, factory)
	if err != nil {
		return err
	}
	credentials := map[string]principal{"fixture-A": {"A", "same-user"}, "fixture-B": {"B", "same-user"}}
	// Fixed tokens stand in for verified host authentication. A real host verifies
	// its JWT/session/mTLS identity before putting its own principal in context.
	authenticated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := credentials[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		svc.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
	srv := httptest.NewServer(authenticated)
	defer srv.Close()
	ids := make(map[string]string)
	for _, tenant := range []string{"A", "B"} {
		b, status, err := request(srv.Client(), srv.URL, "fixture-"+tenant, "POST", "/api/session", map[string]string{"agent_name": "assistant"})
		if err != nil {
			return err
		}
		var session service.SessionResponse
		if status != 201 {
			return fmt.Errorf("create=%d %s", status, b)
		}
		if err := json.Unmarshal(b, &session); err != nil {
			return err
		}
		ids[tenant] = session.SessionID
	}
	for _, tenant := range []string{"A", "B"} {
		b, status, err := request(srv.Client(), srv.URL, "fixture-"+tenant, "POST", "/api/chat", map[string]string{"session_id": ids[tenant], "message": "read my fixture"})
		if err != nil {
			return err
		}
		var reply service.ChatResponse
		if err := json.Unmarshal(b, &reply); err != nil {
			return err
		}
		if status != 200 || reply.Reply != "fixture for tenant "+tenant {
			return fmt.Errorf("own chat=%d %s", status, b)
		}
		fmt.Printf("Tenant %s: %s\n", tenant, reply.Reply)
	}
	b, status, err := request(srv.Client(), srv.URL, "fixture-B", "GET", "/api/session/"+ids["A"], nil)
	if err != nil {
		return err
	}
	if status != 404 {
		return fmt.Errorf("foreign session=%d %s", status, b)
	}
	fmt.Println("Tenant B -> tenant A session: 404")
	b, status, err = request(srv.Client(), srv.URL, "fixture-A", "GET", "/api/sessions", nil)
	if err != nil {
		return err
	}
	var sessions []service.SessionResponse
	if err := json.Unmarshal(b, &sessions); err != nil {
		return err
	}
	if status != 200 || len(sessions) != 1 || sessions[0].SessionID != ids["A"] {
		return fmt.Errorf("scoped list=%d %s", status, b)
	}
	fmt.Println("Tenant A session list: 1 owned session")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
