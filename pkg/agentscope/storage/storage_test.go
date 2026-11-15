package storage

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func makeState(sessionID string) *agent.AgentState {
	return &agent.AgentState{
		SessionID: sessionID,
		Context: []*message.Msg{
			message.UserMsg("user", "hello"),
			message.AssistantMsg("bot", "hi there"),
		},
		Summary: "greeting exchange",
	}
}

// --- InMemoryStorage tests ---

func TestInMemorySaveAndLoad(t *testing.T) {
	s := NewInMemoryStorage()
	ctx := context.Background()

	state := makeState("sess-1")
	if err := s.SaveState(ctx, "sess-1", state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := s.LoadState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, "sess-1")
	}
	if loaded.Summary != "greeting exchange" {
		t.Errorf("Summary = %q, want %q", loaded.Summary, "greeting exchange")
	}
	if len(loaded.Context) != 2 {
		t.Errorf("Context len = %d, want 2", len(loaded.Context))
	}
}

func TestInMemoryLoadNotFound(t *testing.T) {
	s := NewInMemoryStorage()
	_, err := s.LoadState(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestInMemoryListSessions(t *testing.T) {
	s := NewInMemoryStorage()
	ctx := context.Background()

	_ = s.SaveState(ctx, "b", makeState("b"))
	_ = s.SaveState(ctx, "a", makeState("a"))

	sessions, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions = %d, want 2", len(sessions))
	}
	if sessions[0].SessionID != "a" {
		t.Errorf("first session = %q, want %q (sorted)", sessions[0].SessionID, "a")
	}
}

func TestInMemoryDeleteSession(t *testing.T) {
	s := NewInMemoryStorage()
	ctx := context.Background()

	_ = s.SaveState(ctx, "sess-1", makeState("sess-1"))

	if err := s.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	_, err := s.LoadState(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestInMemoryDeleteNotFound(t *testing.T) {
	s := NewInMemoryStorage()
	err := s.DeleteSession(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestInMemorySaveEmptyID(t *testing.T) {
	s := NewInMemoryStorage()
	err := s.SaveState(context.Background(), "", makeState(""))
	if err == nil {
		t.Fatal("expected error for empty session ID")
	}
}

func TestInMemoryIsolation(t *testing.T) {
	s := NewInMemoryStorage()
	ctx := context.Background()

	state := makeState("sess-1")
	_ = s.SaveState(ctx, "sess-1", state)

	state.Summary = "modified"

	loaded, _ := s.LoadState(ctx, "sess-1")
	if loaded.Summary == "modified" {
		t.Error("SaveState should deep-copy; original mutation should not affect stored state")
	}
}

func TestInMemoryOverwrite(t *testing.T) {
	s := NewInMemoryStorage()
	ctx := context.Background()

	_ = s.SaveState(ctx, "sess-1", makeState("sess-1"))

	updated := makeState("sess-1")
	updated.Summary = "updated"
	_ = s.SaveState(ctx, "sess-1", updated)

	loaded, _ := s.LoadState(ctx, "sess-1")
	if loaded.Summary != "updated" {
		t.Errorf("Summary = %q, want %q", loaded.Summary, "updated")
	}
}

// --- FileStorage tests ---

func TestFileSaveAndLoad(t *testing.T) {
	s := mustFileStorage(t)
	ctx := context.Background()

	state := makeState("sess-1")
	if err := s.SaveState(ctx, "sess-1", state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := s.LoadState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", loaded.SessionID, "sess-1")
	}
	if loaded.Summary != "greeting exchange" {
		t.Errorf("Summary = %q, want %q", loaded.Summary, "greeting exchange")
	}
	if len(loaded.Context) != 2 {
		t.Errorf("Context len = %d, want 2", len(loaded.Context))
	}
}

func TestFileLoadNotFound(t *testing.T) {
	s := mustFileStorage(t)
	_, err := s.LoadState(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestFileListSessions(t *testing.T) {
	s := mustFileStorage(t)
	ctx := context.Background()

	_ = s.SaveState(ctx, "beta", makeState("beta"))
	_ = s.SaveState(ctx, "alpha", makeState("alpha"))

	sessions, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions = %d, want 2", len(sessions))
	}
	if sessions[0].SessionID != "alpha" {
		t.Errorf("first session = %q, want %q (sorted)", sessions[0].SessionID, "alpha")
	}
}

func TestFileDeleteSession(t *testing.T) {
	s := mustFileStorage(t)
	ctx := context.Background()

	_ = s.SaveState(ctx, "sess-1", makeState("sess-1"))

	if err := s.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	_, err := s.LoadState(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestFileDeleteNotFound(t *testing.T) {
	s := mustFileStorage(t)
	err := s.DeleteSession(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestFileSaveEmptyID(t *testing.T) {
	s := mustFileStorage(t)
	err := s.SaveState(context.Background(), "", makeState(""))
	if err == nil {
		t.Fatal("expected error for empty session ID")
	}
}

func TestFileOverwrite(t *testing.T) {
	s := mustFileStorage(t)
	ctx := context.Background()

	_ = s.SaveState(ctx, "sess-1", makeState("sess-1"))

	updated := makeState("sess-1")
	updated.Summary = "v2"
	_ = s.SaveState(ctx, "sess-1", updated)

	loaded, _ := s.LoadState(ctx, "sess-1")
	if loaded.Summary != "v2" {
		t.Errorf("Summary = %q, want %q", loaded.Summary, "v2")
	}
}

func TestNewFileStorageEmptyPath(t *testing.T) {
	_, err := NewFileStorage("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func mustFileStorage(t *testing.T) *FileStorage {
	t.Helper()
	s, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStorage: %v", err)
	}
	return s
}
