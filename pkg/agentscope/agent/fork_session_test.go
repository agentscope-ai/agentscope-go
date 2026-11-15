package agent

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// ForkSession persists a between-replies branch of the session under a new
// ID without disturbing the live state.
func TestForkSession_RoundTrip(t *testing.T) {
	saver := newFakeStateSaver()
	a := NewUnifiedAgent("fork-agent", "helpful", &mockChatModel{},
		WithStateSaver(saver),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
	)
	a.state.SessionID = "sess-original"
	a.state.ReplyID = "reply-9"
	a.state.Summary = "work so far"
	a.state.Context = []*message.Msg{
		{Role: message.RoleUser, Name: "user", Content: []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "hello"},
		}},
	}
	a.state.Plan = []tool.PlanItem{{Step: "step one", Status: tool.PlanStatusCompleted}}
	a.engine.AddWorkingDirectory(permission.WorkingDirectory{Path: "/granted", Source: "granted"})

	if err := a.ForkSession(context.Background(), "sess-fork"); err != nil {
		t.Fatal(err)
	}

	// The live session is untouched.
	if a.state.SessionID != "sess-original" || a.state.ReplyID != "reply-9" {
		t.Errorf("live state mutated by fork: %+v", a.state)
	}

	forked, err := LoadCheckpoint(context.Background(), saver, "sess-fork")
	if err != nil {
		t.Fatal(err)
	}
	if forked.SessionID != "sess-fork" {
		t.Errorf("fork session id: %q", forked.SessionID)
	}
	if forked.ReplyID != "" || forked.ReplyRecovery != nil {
		t.Errorf("fork should start at a clean reply boundary: reply=%q recovery=%+v", forked.ReplyID, forked.ReplyRecovery)
	}
	if forked.Summary != "work so far" {
		t.Errorf("fork summary lost: %q", forked.Summary)
	}
	if len(forked.Context) != 1 {
		t.Fatalf("fork context lost: %d messages", len(forked.Context))
	}
	if tb, ok := forked.Context[0].Content[0].(message.TextBlock); !ok || tb.Text != "hello" {
		t.Errorf("fork context degraded: %+v", forked.Context[0].Content[0])
	}
	if len(forked.Plan) != 1 || forked.Plan[0].Step != "step one" {
		t.Errorf("fork plan lost: %+v", forked.Plan)
	}
	if _, ok := forked.PermissionCtx.WorkingDirectories["/granted"]; !ok {
		t.Error("fork permission grants lost")
	}

	// Independence: mutating the forked snapshot must not leak into the live
	// agent state (the fork went through a full serialize round trip).
	forked.Context[0].Content[0] = message.TextBlock{Type: "text", Text: "tampered"}
	if tb, _ := a.state.Context[0].Content[0].(message.TextBlock); tb.Text != "hello" {
		t.Error("fork shares memory with live state")
	}

	// The original session has no checkpoint unless one was saved separately.
	if _, err = LoadCheckpoint(context.Background(), saver, "sess-original"); err == nil {
		t.Error("fork should not checkpoint the original session")
	}
}

func TestForkSession_Errors(t *testing.T) {
	noSaver := NewUnifiedAgent("bot", "prompt", &mockChatModel{})
	if err := noSaver.ForkSession(context.Background(), "x"); err == nil {
		t.Error("expected error without StateSaver")
	}
	withSaver := NewUnifiedAgent("bot", "prompt", &mockChatModel{}, WithStateSaver(newFakeStateSaver()))
	if err := withSaver.ForkSession(context.Background(), ""); err == nil {
		t.Error("expected error for empty session id")
	}
}
