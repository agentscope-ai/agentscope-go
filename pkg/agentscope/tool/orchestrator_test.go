package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/sandbox"
)

func makeEchoTool(name string) *FunctionTool {
	return NewFunctionTool(name, "echoes input", json.RawMessage(`{"type":"object","properties":{}}`),
		func(ctx context.Context, input map[string]any) (any, error) {
			return "ok:" + name, nil
		},
	)
}

func makeToolCall(name, inputJSON string) message.ToolCallBlock {
	return message.ToolCallBlock{
		Type:  "tool_call",
		ID:    "call-" + name,
		Name:  name,
		Input: inputJSON,
	}
}

func TestOrchestratorExecuteSuccess(t *testing.T) {
	tk := NewToolkit(makeEchoTool("greet"))
	o := NewOrchestrator(OrchestratorConfig{Toolkit: tk})

	resp, err := o.Execute(context.Background(), makeToolCall("greet", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success state, got %v", resp.State)
	}
	if len(resp.Content) == 0 {
		t.Fatal("expected non-empty content")
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", resp.Content[0])
	}
	if tb.Text != "ok:greet" {
		t.Fatalf("expected 'ok:greet', got %q", tb.Text)
	}
}

func TestOrchestratorExecutePermissionDenied(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Bash"))
	permCtx := permission.NewContext(permission.ModeDefault)
	engine := permission.NewEngine(permCtx)
	engine.AddRule(permission.Rule{
		ToolName: "Bash",
		Behavior: permission.BehaviorDeny,
		Source:   "test",
	})

	var deniedTool string
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit:    tk,
		PermEngine: engine,
		OnPermissionDenied: func(name string, d permission.Decision) {
			deniedTool = name
		},
	})

	resp, err := o.Execute(context.Background(), makeToolCall("Bash", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state, got %v", resp.State)
	}
	if deniedTool != "Bash" {
		t.Fatalf("expected OnPermissionDenied called with 'Bash', got %q", deniedTool)
	}
}

func TestOrchestratorExecutePermissionAsk(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Write"))
	permCtx := permission.NewContext(permission.ModeDefault)
	engine := permission.NewEngine(permCtx)
	// In ModeDefault with no allow rules, the engine returns BehaviorAsk.
	// No explicit ask rule needed — the default behavior is Ask.

	var deniedTool string
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit:    tk,
		PermEngine: engine,
		OnPermissionDenied: func(name string, d permission.Decision) {
			deniedTool = name
		},
	})

	resp, err := o.Execute(context.Background(), makeToolCall("Write", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state, got %v", resp.State)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", resp.Content[0])
	}
	if len(tb.Text) < len("Permission required: ") {
		t.Fatalf("expected 'Permission required: ...' prefix, got %q", tb.Text)
	}
	if tb.Text[:len("Permission required: ")] != "Permission required: " {
		t.Fatalf("expected 'Permission required: ...' prefix, got %q", tb.Text)
	}
	if deniedTool != "Write" {
		t.Fatalf("expected OnPermissionDenied called with 'Write', got %q", deniedTool)
	}
}

func TestOrchestratorExecuteToolNotFound(t *testing.T) {
	tk := NewToolkit() // empty toolkit
	o := NewOrchestrator(OrchestratorConfig{Toolkit: tk})

	_, err := o.Execute(context.Background(), makeToolCall("nonexistent", `{}`))
	if err == nil {
		t.Fatal("expected error for missing tool")
	}
	if _, ok := err.(*agenterrors.ToolNotFoundError); !ok {
		t.Fatalf("expected ToolNotFoundError, got %T: %v", err, err)
	}
}

func TestOrchestratorBatchExecute(t *testing.T) {
	tk := NewToolkit(makeEchoTool("allowed"), makeEchoTool("denied"))
	permCtx := permission.NewContext(permission.ModeDefault)
	engine := permission.NewEngine(permCtx)
	engine.AddRule(permission.Rule{
		ToolName: "allowed",
		Behavior: permission.BehaviorAllow,
		Source:   "test",
	})
	engine.AddRule(permission.Rule{
		ToolName: "denied",
		Behavior: permission.BehaviorDeny,
		Source:   "test",
	})

	o := NewOrchestrator(OrchestratorConfig{
		Toolkit:    tk,
		PermEngine: engine,
	})

	calls := []message.ToolCallBlock{
		makeToolCall("allowed", `{}`),
		makeToolCall("denied", `{}`),
	}

	results := o.BatchExecute(context.Background(), calls)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// First call should succeed
	if results[0].Err != nil {
		t.Fatalf("expected no error for 'allowed', got %v", results[0].Err)
	}
	if results[0].Response.State != message.ToolResultSuccess {
		t.Fatalf("expected success for 'allowed', got %v", results[0].Response.State)
	}

	// Second call should be denied
	if results[1].Err != nil {
		t.Fatalf("expected no error (denial is a response, not an error), got %v", results[1].Err)
	}
	if results[1].Response.State != message.ToolResultError {
		t.Fatalf("expected error state for 'denied', got %v", results[1].Response.State)
	}
}

func TestOrchestratorAsToolExecutor(t *testing.T) {
	// Define a local interface mirroring loop.ToolExecutor.Execute signature
	// to verify structural compatibility without importing loop (which would
	// create a circular dependency).
	type executor interface {
		Execute(ctx context.Context, call message.ToolCallBlock) (*ToolResponse, error)
	}

	tk := NewToolkit(makeEchoTool("ping"))
	o := NewOrchestrator(OrchestratorConfig{Toolkit: tk})

	adapter := o.AsToolExecutor()

	// Compile-time check: adapter satisfies the executor interface.
	var _ executor = adapter

	// Runtime check: Execute works through the adapter.
	resp, err := adapter.Execute(context.Background(), makeToolCall("ping", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success, got %v", resp.State)
	}

	// Verify BatchExecute also works through the adapter.
	results := adapter.BatchExecute(context.Background(), []message.ToolCallBlock{
		makeToolCall("ping", `{}`),
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("unexpected error: %v", results[0].Err)
	}
}

// ---------------------------------------------------------------------------
// Sandbox policy enforcement tests
// ---------------------------------------------------------------------------

func TestOrchestratorPolicy_ReadOnlyBlocksWrite(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Write"), makeEchoTool("Read"))
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit: tk,
		Policy: &sandbox.Policy{
			FileSystem: sandbox.FileSystemPolicy{Mode: sandbox.FSReadOnly},
			Process:    sandbox.ProcessPolicy{AllowExec: true},
		},
	})

	// Write should be blocked.
	resp, err := o.Execute(context.Background(), makeToolCall("Write", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state for Write under read-only policy, got %v", resp.State)
	}
	tb := resp.Content[0].(message.TextBlock)
	if !strings.Contains(tb.Text, "read-only") {
		t.Fatalf("expected read-only denial message, got %q", tb.Text)
	}

	// Read should still work.
	resp, err = o.Execute(context.Background(), makeToolCall("Read", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success for Read under read-only policy, got %v", resp.State)
	}
}

func TestOrchestratorPolicy_AllowExecFalseBlocksBash(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Bash"), makeEchoTool("Read"))
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit: tk,
		Policy: &sandbox.Policy{
			Process: sandbox.ProcessPolicy{AllowExec: false},
		},
	})

	// Bash should be blocked.
	resp, err := o.Execute(context.Background(), makeToolCall("Bash", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state for Bash with AllowExec=false, got %v", resp.State)
	}
	tb := resp.Content[0].(message.TextBlock)
	if !strings.Contains(tb.Text, "AllowExec") {
		t.Fatalf("expected AllowExec denial message, got %q", tb.Text)
	}

	// Read should still work.
	resp, err = o.Execute(context.Background(), makeToolCall("Read", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success for Read, got %v", resp.State)
	}
}

func TestOrchestratorPolicy_NetDisabledBlocksWebFetch(t *testing.T) {
	tk := NewToolkit(makeEchoTool("WebFetch"), makeEchoTool("Read"))
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit: tk,
		Policy: &sandbox.Policy{
			Process: sandbox.ProcessPolicy{AllowExec: true},
			Network: sandbox.NetworkPolicy{Mode: sandbox.NetDisabled},
		},
	})

	resp, err := o.Execute(context.Background(), makeToolCall("WebFetch", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error state for WebFetch with NetDisabled, got %v", resp.State)
	}
	tb := resp.Content[0].(message.TextBlock)
	if !strings.Contains(tb.Text, "NetDisabled") {
		t.Fatalf("expected NetDisabled denial message, got %q", tb.Text)
	}
}

func TestOrchestratorPolicy_DenyPathsBlocksAccess(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Read"))
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit: tk,
		Policy: &sandbox.Policy{
			Process: sandbox.ProcessPolicy{AllowExec: true},
			FileSystem: sandbox.FileSystemPolicy{
				Mode:      sandbox.FSFullAccess,
				DenyPaths: []string{"/etc/secrets", "/var/private"},
			},
		},
	})

	// Path under a denied prefix should be blocked.
	resp, err := o.Execute(context.Background(),
		makeToolCall("Read", `{"file_path":"/etc/secrets/key.pem"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected error for denied path, got %v", resp.State)
	}
	tb := resp.Content[0].(message.TextBlock)
	if !strings.Contains(tb.Text, "denied by sandbox policy") {
		t.Fatalf("expected deny message, got %q", tb.Text)
	}

	// Path NOT under a denied prefix should work.
	resp, err = o.Execute(context.Background(),
		makeToolCall("Read", `{"file_path":"/home/user/notes.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("expected success for non-denied path, got %v", resp.State)
	}
}

func TestOrchestratorPolicy_NilPolicyNoEffect(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Write"), makeEchoTool("Bash"))
	o := NewOrchestrator(OrchestratorConfig{Toolkit: tk}) // no policy

	// Both should succeed with no policy.
	for _, name := range []string{"Write", "Bash"} {
		resp, err := o.Execute(context.Background(), makeToolCall(name, `{}`))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if resp.State != message.ToolResultSuccess {
			t.Fatalf("%s: expected success, got %v", name, resp.State)
		}
	}
}

func TestOrchestratorPolicy_ResourceTimeoutApplied(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Bash"))
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit: tk,
		Policy: &sandbox.Policy{
			Process:   sandbox.ProcessPolicy{AllowExec: true},
			Resources: sandbox.ResourcePolicy{TimeoutSec: 30},
		},
	})

	// The resource timeout should have been adopted as defaultToolTimeout.
	if o.defaultToolTimeout != 30*1e9 { // 30s in nanoseconds
		t.Fatalf("expected defaultToolTimeout=30s, got %v", o.defaultToolTimeout)
	}
}

func TestOrchestratorPolicy_ExplicitTimeoutTakesPrecedence(t *testing.T) {
	tk := NewToolkit(makeEchoTool("Bash"))
	o := NewOrchestrator(OrchestratorConfig{
		Toolkit:            tk,
		DefaultToolTimeout: 60 * 1e9, // 60s
		Policy: &sandbox.Policy{
			Process:   sandbox.ProcessPolicy{AllowExec: true},
			Resources: sandbox.ResourcePolicy{TimeoutSec: 30},
		},
	})

	// Explicit config timeout should not be overridden by policy.
	if o.defaultToolTimeout != 60*1e9 {
		t.Fatalf("expected defaultToolTimeout=60s (explicit), got %v", o.defaultToolTimeout)
	}
}
