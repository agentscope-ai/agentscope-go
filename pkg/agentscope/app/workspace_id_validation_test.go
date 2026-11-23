package app

import (
	"errors"
	"net/http"
	"testing"
)

func TestWorkspaceManager_GetOrCreateRejectsInvalidID(t *testing.T) {
	m := NewWorkspaceManager(t.TempDir(), "local")
	for _, id := range []string{"", "..", "../escaped", ".hidden", "a/b", "a\\b"} {
		if _, err := m.GetOrCreate(id); !errors.Is(err, errInvalidWorkspaceID) {
			t.Errorf("GetOrCreate(%q) must reject with errInvalidWorkspaceID, got %v", id, err)
		}
	}
	if _, err := m.GetOrCreate("valid-session"); err != nil {
		t.Errorf("valid session id must be accepted: %v", err)
	}
}

func TestWorkspaceSkillRoutes_RejectsTraversalSessionID(t *testing.T) {
	srv := newSkillTestApp(t)
	for _, id := range []string{"../escaped", "..", ".hidden", "%2e%2e%2fescaped"} {
		resp, raw := doSkillReq(t, http.MethodGet, srv.URL+"/api/workspace/skill?session_id="+id, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("traversal session_id %q must be 400, got %d: %s", id, resp.StatusCode, raw)
		}
	}
	// The POST write path shares the same guarded helper.
	if resp, raw := doSkillReq(t, http.MethodPost, srv.URL+"/api/workspace/skill?session_id=../escaped", `{"name":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST traversal session_id must be 400, got %d: %s", resp.StatusCode, raw)
	}
}
