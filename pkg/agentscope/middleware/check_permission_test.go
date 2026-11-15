package middleware

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// --- CheckPermission test helpers ---

// denyMiddleware overrides the decision to Deny without calling next.
type denyMiddleware struct {
	BaseMiddleware
}

func (m *denyMiddleware) OnCheckPermission(_ context.Context, input *CheckPermissionInput, _ CheckPermissionHandler) (*permission.Decision, error) {
	return &permission.Decision{
		Behavior:       permission.BehaviorDeny,
		Message:        "blocked by denyMiddleware",
		DecisionReason: input.ToolCall.Name,
	}, nil
}

// orderPermMiddleware records order and delegates to next.
type orderPermMiddleware struct {
	BaseMiddleware
	tracker *orderTracker
	label   string
}

func (m *orderPermMiddleware) OnCheckPermission(ctx context.Context, input *CheckPermissionInput, next CheckPermissionHandler) (*permission.Decision, error) {
	m.tracker.order = append(m.tracker.order, m.label+":before")
	d, err := next(ctx, input)
	m.tracker.order = append(m.tracker.order, m.label+":after")
	return d, err
}

// --- Tests ---

func TestBaseMiddleware_OnCheckPermission_PassThrough(t *testing.T) {
	mw := &BaseMiddleware{MiddlewareKey: "base"}

	coreCalled := false
	core := func(ctx context.Context, input *CheckPermissionInput) (*permission.Decision, error) {
		coreCalled = true
		return &permission.Decision{
			Behavior: permission.BehaviorAllow,
			Message:  "core allowed",
		}, nil
	}

	input := &CheckPermissionInput{
		AgentName: "test-agent",
		ToolCall:  message.ToolCallBlock{Name: "Bash"},
		ToolInput: map[string]any{"command": "ls"},
	}

	d, err := mw.OnCheckPermission(context.Background(), input, core)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !coreCalled {
		t.Fatal("OnCheckPermission did not pass through to core")
	}
	if d.Behavior != permission.BehaviorAllow {
		t.Errorf("expected BehaviorAllow, got %v", d.Behavior)
	}
	if d.Message != "core allowed" {
		t.Errorf("expected message 'core allowed', got %q", d.Message)
	}
}

func TestBuildCheckPermissionChain_CustomMiddleware(t *testing.T) {
	blocker := &denyMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "blocker"}}

	coreCalled := false
	core := func(ctx context.Context, input *CheckPermissionInput) (*permission.Decision, error) {
		coreCalled = true
		return &permission.Decision{Behavior: permission.BehaviorAllow}, nil
	}

	chain := BuildCheckPermissionChain([]Middleware{blocker}, core)
	input := &CheckPermissionInput{
		AgentName: "agent1",
		ToolCall:  message.ToolCallBlock{Name: "Write"},
		ToolInput: map[string]any{"path": "/etc/passwd"},
	}

	d, err := chain(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if coreCalled {
		t.Error("core was called despite deny middleware short-circuit")
	}
	if d.Behavior != permission.BehaviorDeny {
		t.Errorf("expected BehaviorDeny, got %v", d.Behavior)
	}
	if d.DecisionReason != "Write" {
		t.Errorf("expected DecisionReason 'Write', got %q", d.DecisionReason)
	}
}

func TestBuildCheckPermissionChain_OnionOrder(t *testing.T) {
	tracker := &orderTracker{}
	mw1 := &orderPermMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw1"}, tracker: tracker, label: "outer"}
	mw2 := &orderPermMiddleware{BaseMiddleware: BaseMiddleware{MiddlewareKey: "mw2"}, tracker: tracker, label: "inner"}

	core := func(ctx context.Context, input *CheckPermissionInput) (*permission.Decision, error) {
		tracker.order = append(tracker.order, "core")
		return &permission.Decision{Behavior: permission.BehaviorAllow, Message: "ok"}, nil
	}

	chain := BuildCheckPermissionChain([]Middleware{mw1, mw2}, core)
	d, err := chain(context.Background(), &CheckPermissionInput{
		AgentName: "agent",
		ToolCall:  message.ToolCallBlock{Name: "Read"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Behavior != permission.BehaviorAllow {
		t.Errorf("expected BehaviorAllow, got %v", d.Behavior)
	}

	expected := []string{"outer:before", "inner:before", "core", "inner:after", "outer:after"}
	if len(tracker.order) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(tracker.order), tracker.order)
	}
	for i, v := range expected {
		if tracker.order[i] != v {
			t.Errorf("order[%d] = %q, want %q", i, tracker.order[i], v)
		}
	}
}

func TestBuildCheckPermissionChain_CoreError(t *testing.T) {
	mw := &BaseMiddleware{MiddlewareKey: "passthrough"}

	core := func(ctx context.Context, input *CheckPermissionInput) (*permission.Decision, error) {
		return nil, fmt.Errorf("permission engine unavailable")
	}

	chain := BuildCheckPermissionChain([]Middleware{mw}, core)
	_, err := chain(context.Background(), &CheckPermissionInput{
		AgentName: "agent",
		ToolCall:  message.ToolCallBlock{Name: "Bash"},
	})
	if err == nil {
		t.Fatal("expected error from core, got nil")
	}
	if err.Error() != "permission engine unavailable" {
		t.Errorf("unexpected error: %v", err)
	}
}
