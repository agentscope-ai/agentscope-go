package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestRecordMode(t *testing.T) {
	recorder := NewRecorder()

	input := &middleware.ModelCallInput{
		AgentName: "test-agent",
		ModelName: "gpt-4",
		Messages: []*message.Msg{
			{Role: message.RoleUser, Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "hello"},
			}},
		},
	}

	expectedResp := &model.ChatResponse{
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "world"},
		},
		ID:     "resp-1",
		IsLast: true,
	}

	// Fake next handler that returns a known response
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return expectedResp, nil
	}

	ctx := context.Background()
	resp, err := recorder.OnModelCall(ctx, input, next)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "resp-1" {
		t.Fatalf("expected response ID resp-1, got %s", resp.ID)
	}

	tape := recorder.Tape()
	if len(tape.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(tape.Entries))
	}

	entry := tape.Entries[0]
	if entry.AgentName != "test-agent" {
		t.Errorf("expected agent_name test-agent, got %s", entry.AgentName)
	}
	if entry.ModelName != "gpt-4" {
		t.Errorf("expected model_name gpt-4, got %s", entry.ModelName)
	}
	if entry.Index != 0 {
		t.Errorf("expected index 0, got %d", entry.Index)
	}
	if entry.Error != "" {
		t.Errorf("expected no error, got %s", entry.Error)
	}
	if len(entry.Response) == 0 {
		t.Error("expected non-empty response")
	}
}

func TestReplayMode(t *testing.T) {
	// Pre-build a tape with one entry
	resp := &model.ChatResponse{
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "replayed response"},
		},
		ID:     "resp-replay",
		IsLast: true,
	}
	respJSON, _ := json.Marshal(resp)

	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{
				Index:     0,
				AgentName: "test-agent",
				ModelName: "gpt-4",
				Messages:  json.RawMessage(`[{"role":"user"}]`),
				Response:  respJSON,
			},
		},
	}

	replayer := NewReplayer(tape)

	input := &middleware.ModelCallInput{
		AgentName: "test-agent",
		ModelName: "gpt-4",
	}

	// next should NOT be called in replay mode
	nextCalled := false
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		nextCalled = true
		return nil, fmt.Errorf("should not be called")
	}

	ctx := context.Background()
	got, err := replayer.OnModelCall(ctx, input, next)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nextCalled {
		t.Fatal("next handler was called in replay mode")
	}
	if got.ID != "resp-replay" {
		t.Errorf("expected ID resp-replay, got %s", got.ID)
	}
	if got.GetTextContent() != "replayed response" {
		t.Errorf("expected text replayed response, got %q", got.GetTextContent())
	}
}

func TestReplayExhaustion(t *testing.T) {
	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{
				Index:    0,
				Response: json.RawMessage(`{"id":"r1","is_last":true,"content":[]}`),
			},
		},
	}

	replayer := NewReplayer(tape)
	ctx := context.Background()
	input := &middleware.ModelCallInput{}
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return nil, nil
	}

	// First call should succeed
	_, err := replayer.OnModelCall(ctx, input, next)
	if err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}

	// Second call should fail (tape exhausted)
	_, err = replayer.OnModelCall(ctx, input, next)
	if err == nil {
		t.Fatal("expected error on tape exhaustion")
	}
	if expected := "replay: tape exhausted at index 1 (tape has 1 entries)"; err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}
}

func TestReplayError(t *testing.T) {
	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{
				Index: 0,
				Error: "model: rate limit exceeded",
			},
		},
	}

	replayer := NewReplayer(tape)
	ctx := context.Background()
	input := &middleware.ModelCallInput{}
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return nil, nil
	}

	resp, err := replayer.OnModelCall(ctx, input, next)
	if err == nil {
		t.Fatal("expected error from replay")
	}
	if err.Error() != "model: rate limit exceeded" {
		t.Errorf("expected model: rate limit exceeded, got %q", err.Error())
	}
	if resp != nil {
		t.Error("expected nil response for error-only entry")
	}
}

func TestFileStoreSaveLoad(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "replay-test-"+t.Name())
	defer os.RemoveAll(dir)

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	tape := &Tape{
		Version:  "1.0",
		Metadata: map[string]string{"test": "value"},
		Entries: []Entry{
			{
				Index:     0,
				AgentName: "agent-a",
				ModelName: "gpt-4",
				Messages:  json.RawMessage(`[{"role":"user"}]`),
				Response:  json.RawMessage(`{"id":"r1","content":[],"is_last":true}`),
			},
			{
				Index:     1,
				AgentName: "agent-b",
				ModelName: "claude-sonnet",
				Messages:  json.RawMessage(`[{"role":"user"}]`),
				Response:  json.RawMessage(`{"id":"r2","content":[],"is_last":true}`),
				Error:     "timeout",
			},
		},
	}

	ctx := context.Background()

	if err := store.Save(ctx, "test-tape", tape); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "test-tape.json")); err != nil {
		t.Fatalf("tape file not found: %v", err)
	}

	// Load and verify
	loaded, err := store.Load(ctx, "test-tape")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != "1.0" {
		t.Errorf("expected version 1.0, got %s", loaded.Version)
	}
	if loaded.Metadata["test"] != "value" {
		t.Errorf("expected metadata test=value, got %v", loaded.Metadata)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded.Entries))
	}
	if loaded.Entries[0].AgentName != "agent-a" {
		t.Errorf("entry 0 agent_name: expected agent-a, got %s", loaded.Entries[0].AgentName)
	}
	if loaded.Entries[1].Error != "timeout" {
		t.Errorf("entry 1 error: expected timeout, got %s", loaded.Entries[1].Error)
	}
}

func TestFileStoreList(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "replay-test-"+t.Name())
	defer os.RemoveAll(dir)

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	ctx := context.Background()
	tape := NewTape()

	_ = store.Save(ctx, "alpha", tape)
	_ = store.Save(ctx, "beta", tape)
	_ = store.Save(ctx, "gamma", tape)

	names, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 tapes, got %d", len(names))
	}

	// Verify all names are present (order may vary by filesystem)
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	for _, expected := range []string{"alpha", "beta", "gamma"} {
		if !found[expected] {
			t.Errorf("expected tape %q in list", expected)
		}
	}
}

func TestRecordWithError(t *testing.T) {
	recorder := NewRecorder()

	input := &middleware.ModelCallInput{
		AgentName: "err-agent",
		ModelName: "gpt-4",
	}

	// next handler returns an error
	next := func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return nil, fmt.Errorf("connection timeout")
	}

	ctx := context.Background()
	_, err := recorder.OnModelCall(ctx, input, next)
	if err == nil {
		t.Fatal("expected error to propagate")
	}

	tape := recorder.Tape()
	if len(tape.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(tape.Entries))
	}
	if tape.Entries[0].Error != "connection timeout" {
		t.Errorf("expected error connection timeout, got %q", tape.Entries[0].Error)
	}
}
