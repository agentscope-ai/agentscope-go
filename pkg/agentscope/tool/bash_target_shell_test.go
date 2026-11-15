package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// Residual of upstream #2366: shell-specific permission checks must follow
// the execution target, not the host OS.
func TestBashCheckPermissionsTargetShellPin(t *testing.T) {
	bt := &bashTool{}
	input := map[string]any{"command": "Set-ExecutionPolicy Unrestricted"}

	// Pinned to PowerShell: the dangerous-pattern check applies even on a
	// non-Windows host.
	pctx := permission.NewContext(permission.ModeDefault)
	pctx.TargetShell = "powershell"
	d := bt.CheckPermissions(input, pctx)
	if d.Behavior != permission.BehaviorAsk || !strings.Contains(d.Message, "PowerShell") {
		t.Errorf("powershell pin: decision = %v (%q), want Ask/PowerShell", d.Behavior, d.Message)
	}
	if !d.BypassImmune {
		t.Error("PowerShell dangerous decision must be bypass-immune")
	}

	// Pinned to POSIX: the PowerShell pattern check must not fire.
	pctx.TargetShell = "posix"
	d = bt.CheckPermissions(input, pctx)
	if strings.Contains(d.Message, "PowerShell") {
		t.Errorf("posix pin must skip PowerShell checks, got %q", d.Message)
	}
}

type stubPosixBackend struct{}

func (stubPosixBackend) ExecShell(_ context.Context, _ string, _ time.Duration) (*ExecResult, error) {
	return &ExecResult{}, nil
}
func (stubPosixBackend) ReadFile(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (stubPosixBackend) WriteFile(_ context.Context, _ string, _ []byte) error {
	return nil
}
func (stubPosixBackend) FileExists(_ context.Context, _ string) (bool, error) { return false, nil }
func (stubPosixBackend) ListDir(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (stubPosixBackend) Glob(_ context.Context, _ string) ([]string, error) { return nil, nil }

func TestBackendPermissionContext(t *testing.T) {
	base := permission.NewContext(permission.ModeDefault)

	// Workspace backend in ctx → POSIX override.
	ctx := WithBackend(context.Background(), stubPosixBackend{})
	over := BackendPermissionContext(ctx, base)
	if over == nil || over.TargetShell != "posix" {
		t.Fatalf("override = %+v, want TargetShell=posix", over)
	}
	if over == base {
		t.Error("override must be a copy, not the shared base")
	}
	if base.TargetShell != "" {
		t.Error("base context must not be mutated")
	}

	// Host-local backend → no override (host shell applies).
	ctx = WithBackend(context.Background(), &LocalBackend{})
	if over := BackendPermissionContext(ctx, base); over != nil {
		t.Errorf("local backend should not override, got %+v", over)
	}

	// No backend → nil.
	if over := BackendPermissionContext(context.Background(), base); over != nil {
		t.Error("no backend should not override")
	}

	// Explicit pin already set → respected.
	pinned := permission.NewContext(permission.ModeDefault)
	pinned.TargetShell = "powershell"
	ctx = WithBackend(context.Background(), stubPosixBackend{})
	if over := BackendPermissionContext(ctx, pinned); over != nil {
		t.Error("explicit TargetShell pin must win over backend inference")
	}
}

func TestEngineCheckPermissionInContext(t *testing.T) {
	engine := permission.NewEngine(permission.NewContext(permission.ModeDefault))
	bt := &bashTool{}
	input := map[string]any{"command": "Set-ExecutionPolicy Unrestricted"}

	// Base (host) check on a Unix host: no PowerShell ask.
	base, err := engine.CheckPermission(bt, input)
	if err != nil {
		t.Fatalf("base check: %v", err)
	}
	if strings.Contains(base.Message, "PowerShell") {
		t.Skip("host is PowerShell; pin semantics covered elsewhere")
	}

	// Override pins PowerShell → Ask, without mutating the engine context.
	pctx := permission.NewContext(permission.ModeDefault)
	pctx.TargetShell = "powershell"
	d, err := engine.CheckPermissionInContext(bt, input, pctx)
	if err != nil {
		t.Fatalf("override check: %v", err)
	}
	if d.Behavior != permission.BehaviorAsk || !strings.Contains(d.Message, "PowerShell") {
		t.Errorf("override decision = %v (%q), want Ask/PowerShell", d.Behavior, d.Message)
	}
	if engine.Context.TargetShell != "" {
		t.Error("engine context must not be mutated by an override check")
	}

	// Nil override falls back to the engine context.
	d2, err := engine.CheckPermissionInContext(bt, input, nil)
	if err != nil || d2.Behavior != base.Behavior {
		t.Errorf("nil override = %v/%v, want parity with base %v", d2.Behavior, err, base.Behavior)
	}
}
