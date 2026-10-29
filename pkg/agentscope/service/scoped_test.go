package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type ownerKey struct{}

func testScope(ctx context.Context) (string, error) {
	s, _ := ctx.Value(ownerKey{}).(string)
	if s == "" {
		return "", fmt.Errorf("missing fixture identity")
	}
	return s, nil
}

func scopedRequest(s *Service, owner, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), ownerKey{}, owner))
	r.Header.Set("X-User-ID", "other-owner")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func createScoped(t *testing.T, s *Service, owner string) string {
	t.Helper()
	w := scopedRequest(s, owner, "POST", "/api/session", `{"agent_name":"same-name","tenant_id":"other-owner"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.SessionID == "" {
		t.Fatalf("create response: %s err=%v", w.Body.String(), err)
	}
	return resp.SessionID
}

func TestScopedOwnerLookupAndListing(t *testing.T) {
	s, err := NewScoped(Config{}, nil, testScope, func(_ context.Context, name, prompt string, cm model.ChatModel) (*agent.UnifiedAgent, error) {
		return testFactory(name, prompt, cm), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a := createScoped(t, s, "tenant-A/same-user")
	createScoped(t, s, "tenant-B/same-user")
	w := scopedRequest(s, "tenant-B/same-user", "GET", "/api/session/"+a, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign session: status=%d want=404 body=%s", w.Code, w.Body.String())
	}
	w = scopedRequest(s, "tenant-B/same-user", "GET", "/api/sessions", "")
	var sessions []SessionResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sessions) != nil || len(sessions) != 1 {
		t.Fatalf("scoped listing: %d %s", w.Code, w.Body.String())
	}
}
