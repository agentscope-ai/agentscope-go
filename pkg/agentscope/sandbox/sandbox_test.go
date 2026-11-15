package sandbox

import (
	"context"
	"runtime"
	"testing"
)

func TestNoopSandboxType(t *testing.T) {
	s := NoopSandbox{}
	if s.Type() != "noop" {
		t.Errorf("Type() = %q, want %q", s.Type(), "noop")
	}
	if !s.Available() {
		t.Error("Available() = false, want true")
	}
}

func TestNoopSandboxExecute(t *testing.T) {
	s := NoopSandbox{}

	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "cmd"
		args = []string{"/c", "echo", "hello"}
	} else {
		cmd = "echo"
		args = []string{"hello"}
	}

	result, err := s.Execute(context.Background(), &ExecRequest{
		Command: cmd,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Duration <= 0 {
		t.Error("Duration should be > 0")
	}
}

func TestNoopSandboxSetupTeardown(t *testing.T) {
	s := NoopSandbox{}
	if err := s.Setup(context.Background(), Policy{}); err != nil {
		t.Errorf("Setup error: %v", err)
	}
	if err := s.Teardown(context.Background()); err != nil {
		t.Errorf("Teardown error: %v", err)
	}
}

func TestProviderRegistry(t *testing.T) {
	ResetProviders()
	defer func() {
		ResetProviders()
		RegisterProvider(noopProvider{})
	}()

	if len(ListProviders()) != 0 {
		t.Fatal("expected empty registry after reset")
	}

	RegisterProvider(noopProvider{})
	providers := ListProviders()
	if len(providers) != 1 {
		t.Fatalf("got %d providers, want 1", len(providers))
	}
	if providers[0].Name() != "noop" {
		t.Errorf("Name = %q, want %q", providers[0].Name(), "noop")
	}
}

func TestAutoSelectReturnsNoop(t *testing.T) {
	ResetProviders()
	RegisterProvider(noopProvider{})
	defer func() {
		ResetProviders()
		RegisterProvider(noopProvider{})
	}()

	sb, err := AutoSelect(Policy{})
	if err != nil {
		t.Fatalf("AutoSelect error: %v", err)
	}
	if sb.Type() != "noop" {
		t.Errorf("AutoSelect returned %q, want noop", sb.Type())
	}
}

func TestAutoSelectNoProviders(t *testing.T) {
	ResetProviders()
	defer func() {
		ResetProviders()
		RegisterProvider(noopProvider{})
	}()

	_, err := AutoSelect(Policy{})
	if err == nil {
		t.Fatal("expected error when no providers")
	}
	if _, ok := err.(*NoAvailableSandboxError); !ok {
		t.Errorf("expected NoAvailableSandboxError, got %T", err)
	}
}

func TestAutoSelectPriority(t *testing.T) {
	ResetProviders()
	defer func() {
		ResetProviders()
		RegisterProvider(noopProvider{})
	}()

	RegisterProvider(noopProvider{})
	RegisterProvider(&highPriorityProvider{})

	sb, err := AutoSelect(Policy{})
	if err != nil {
		t.Fatalf("AutoSelect error: %v", err)
	}
	if sb.Type() != "high" {
		t.Errorf("AutoSelect returned %q, want high (highest priority)", sb.Type())
	}
}

type highPrioritySandbox struct{ NoopSandbox }

func (highPrioritySandbox) Type() string { return "high" }

type highPriorityProvider struct{}

func (highPriorityProvider) Name() string    { return "high" }
func (highPriorityProvider) Priority() int   { return 100 }
func (highPriorityProvider) Available() bool { return true }
func (highPriorityProvider) Create(_ map[string]any) (Sandbox, error) {
	return highPrioritySandbox{}, nil
}
