package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newSkillTestApp(t *testing.T) *httptest.Server {
	t.Helper()
	app, err := CreateApp(&AppConfig{
		WorkspaceDir: t.TempDir(),
		AgentFactory: testAgentFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func doSkillReq(t *testing.T, method, url, body string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func TestWorkspaceSkillRoutes(t *testing.T) {
	srv := newSkillTestApp(t)
	base := srv.URL + "/api/workspace/skill?session_id=s1"

	// Missing session_id → 400.
	if resp, _ := doSkillReq(t, http.MethodGet, srv.URL+"/api/workspace/skill", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing session_id must be 400, got %d", resp.StatusCode)
	}

	// Empty partition lists as [].
	resp, raw := doSkillReq(t, http.MethodGet, base, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list must be 200, got %d: %s", resp.StatusCode, raw)
	}
	if string(bytes.TrimSpace(raw)) != "[]" {
		t.Errorf("expected empty list, got %s", raw)
	}

	// Add a skill.
	addBody := `{"name":"Deploy Helper","description":"Deploys things","instructions":"Run the deploy."}`
	resp, raw = doSkillReq(t, http.MethodPost, base, addBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add must be 201, got %d: %s", resp.StatusCode, raw)
	}

	// Duplicate → 409.
	resp, _ = doSkillReq(t, http.MethodPost, base, addBody)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate add must be 409, got %d", resp.StatusCode)
	}

	// Invalid body → 400.
	resp, _ = doSkillReq(t, http.MethodPost, base, `{"name":"","instructions":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid add must be 400, got %d", resp.StatusCode)
	}

	// List shows the skill.
	resp, raw = doSkillReq(t, http.MethodGet, base, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list must be 200, got %d", resp.StatusCode)
	}
	var skills []workspaceSkillView
	if err := json.Unmarshal(raw, &skills); err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "Deploy Helper" || skills[0].Description != "Deploys things" {
		t.Errorf("list wrong: %+v", skills)
	}

	// Partitions are isolated by agent_id.
	resp, raw = doSkillReq(t, http.MethodGet, base+"&agent_id=other", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent list must be 200, got %d", resp.StatusCode)
	}
	if string(bytes.TrimSpace(raw)) != "[]" {
		t.Errorf("other agent partition must be empty, got %s", raw)
	}

	// Remove it.
	resp, _ = doSkillReq(t, http.MethodDelete, srv.URL+"/api/workspace/skill/Deploy%20Helper?session_id=s1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("remove must be 204, got %d", resp.StatusCode)
	}

	// Removing again → 404.
	resp, _ = doSkillReq(t, http.MethodDelete, srv.URL+"/api/workspace/skill/Deploy%20Helper?session_id=s1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second remove must be 404, got %d", resp.StatusCode)
	}

	// Escaping agent_id → 400 on every method.
	for _, u := range []string{
		srv.URL + "/api/workspace/skill?session_id=s1&agent_id=..",
		srv.URL + "/api/workspace/skill?session_id=s1&agent_id=a%2Fb",
	} {
		resp, _ := doSkillReq(t, http.MethodGet, u, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("escaping agent_id GET %s must be 400, got %d", u, resp.StatusCode)
		}
	}
	resp, _ = doSkillReq(t, http.MethodDelete, srv.URL+"/api/workspace/skill/x?session_id=s1&agent_id=..", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("escaping agent_id DELETE must be 400, got %d", resp.StatusCode)
	}
}

func TestWorkspaceSkillRoutes_SessionActiveSkills(t *testing.T) {
	app, err := CreateApp(&AppConfig{
		WorkspaceDir: t.TempDir(),
		AgentFactory: testAgentFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}

	session := app.sessionSvc.Create(CreateSessionRequest{AgentName: "agent"})
	if err := app.sessionSvc.SetActiveSkills(session.ID, []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	resp := session.ToResponse()
	if len(resp.ActiveSkills) != 2 || resp.ActiveSkills[0] != "one" {
		t.Errorf("active skills not recorded: %+v", resp.ActiveSkills)
	}

	// PATCH /api/session/{id} can replace the selection.
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	body := `{"active_skills":["three"]}`
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/api/session/"+session.ID, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	patchResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch must be 200, got %d", patchResp.StatusCode)
	}
	var patched SessionResponse
	if err := json.NewDecoder(patchResp.Body).Decode(&patched); err != nil {
		t.Fatal(err)
	}
	if len(patched.ActiveSkills) != 1 || patched.ActiveSkills[0] != "three" {
		t.Errorf("patched active skills wrong: %+v", patched.ActiveSkills)
	}
}
