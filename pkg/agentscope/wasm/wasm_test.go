package wasm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Mock Runtime ---

type mockRuntime struct {
	result  *ExecResult
	err     error
	lastReq *ExecRequest
}

func (m *mockRuntime) Execute(_ context.Context, req *ExecRequest) (*ExecResult, error) {
	m.lastReq = req
	return m.result, m.err
}

func (m *mockRuntime) Name() string {
	return "mock"
}

// --- CLIRuntime Command Construction Tests ---

func TestCLIRuntime_CommandConstruction_Wasmtime(t *testing.T) {
	rt := &CLIRuntime{
		binaryPath:  "/usr/local/bin/wasmtime",
		runtimeName: "wasmtime",
	}

	req := &ExecRequest{
		ModulePath: "/tmp/module.wasm",
		Function:   "process",
		Args:       []string{"--input", "data.json"},
		Env:        map[string]string{"KEY": "value"},
		Fuel:       42,
		AllowedPaths: []string{
			"/tmp/input",
		},
	}

	args := rt.BuildArgs(req, 64*1024*1024)

	// Should start with "run".
	if args[0] != "run" {
		t.Errorf("expected first arg to be run, got %q", args[0])
	}

	joined := strings.Join(args, " ")

	// Should contain fuel flag.
	if !strings.Contains(joined, "fuel=42") {
		t.Error("expected fuel limit in wasmtime args")
	}

	// Should contain max-wasm-stack in the grouped -W options.
	if !strings.Contains(joined, "max-wasm-stack=1048576") {
		t.Error("expected max-wasm-stack option in wasmtime args")
	}

	// Should contain memory limit.
	if !strings.Contains(joined, "max-memory-size=") {
		t.Error("expected max-memory option in wasmtime args")
	}

	// Should invoke the specific function.
	if !strings.Contains(joined, "--invoke process") {
		t.Errorf("expected --invoke process in args, got: %s", joined)
	}

	// Should contain the module path.
	if !strings.Contains(joined, "/tmp/module.wasm") {
		t.Error("expected module path in args")
	}
	if !strings.Contains(joined, "--dir "+filepath.Clean("/tmp/input")) {
		t.Errorf("expected configured allowed path in args, got: %s", joined)
	}

	// Should contain env.
	if !strings.Contains(joined, "--env KEY=value") {
		t.Errorf("expected --env KEY=value in args, got: %s", joined)
	}

	// Should have separator before module args.
	separatorIdx := -1
	modulePathIdx := -1
	for i, a := range args {
		if a == "--" {
			separatorIdx = i
		}
		if a == "/tmp/module.wasm" {
			modulePathIdx = i
		}
	}
	if separatorIdx == -1 {
		t.Error("expected -- separator for module args")
	}
	if modulePathIdx == -1 || separatorIdx <= modulePathIdx {
		t.Error("expected -- separator after module path")
	}

	// Module args should come after --.
	if separatorIdx+1 >= len(args) || args[separatorIdx+1] != "--input" {
		t.Error("expected module args after --")
	}
}

func TestSandboxPassesAllLimitsToRuntime(t *testing.T) {
	module := filepath.Join(t.TempDir(), "module.wasm")
	if err := os.WriteFile(module, []byte("wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &mockRuntime{result: &ExecResult{}}
	s := NewSandbox(SandboxConfig{
		Runtime:        rt,
		AllowedPaths:   []string{"/safe/input"},
		MaxMemory:      32 * 1024 * 1024,
		MaxDuration:    2 * time.Second,
		MaxFuel:        1234,
		MaxOutputBytes: 4096,
	})
	if _, err := s.Run(context.Background(), module, nil); err != nil {
		t.Fatal(err)
	}
	if rt.lastReq.Fuel != 1234 || rt.lastReq.MaxOutputBytes != 4096 {
		t.Fatalf("request limits not propagated: %#v", rt.lastReq)
	}
	if len(rt.lastReq.AllowedPaths) != 1 || rt.lastReq.AllowedPaths[0] != "/safe/input" {
		t.Fatalf("allowed paths not propagated: %#v", rt.lastReq.AllowedPaths)
	}
}

func TestCLIRuntimeRejectsUnsupportedStrictLimits(t *testing.T) {
	for _, runtimeName := range []string{"wasm3", "wasmer"} {
		rt := &CLIRuntime{runtimeName: runtimeName}
		err := rt.validateLimits(&ExecRequest{Fuel: 1, MemoryMax: 1024})
		if !errors.Is(err, ErrUnsupportedLimits) {
			t.Errorf("%s: expected ErrUnsupportedLimits, got %v", runtimeName, err)
		}
	}
}

func TestCLIRuntime_CommandConstruction_Wasm3(t *testing.T) {
	rt := &CLIRuntime{
		binaryPath:  "/usr/local/bin/wasm3",
		runtimeName: "wasm3",
	}

	req := &ExecRequest{
		ModulePath: "/tmp/module.wasm",
		Function:   "main",
		Args:       []string{"arg1", "arg2"},
	}

	args := rt.BuildArgs(req, 64*1024*1024)

	joined := strings.Join(args, " ")

	// Should contain --func for non-_start functions.
	if !strings.Contains(joined, "--func main") {
		t.Errorf("expected --func main in wasm3 args, got: %s", joined)
	}

	// Should contain module path.
	if !strings.Contains(joined, "/tmp/module.wasm") {
		t.Error("expected module path in wasm3 args")
	}

	// Args should follow module path.
	moduleIdx := -1
	for i, a := range args {
		if a == "/tmp/module.wasm" {
			moduleIdx = i
			break
		}
	}
	if moduleIdx == -1 || moduleIdx+1 >= len(args) || args[moduleIdx+1] != "arg1" {
		t.Errorf("expected args after module path, got: %v", args)
	}
}

func TestCLIRuntime_CommandConstruction_Wasmtime_StartFunction(t *testing.T) {
	rt := &CLIRuntime{
		binaryPath:  "/usr/local/bin/wasmtime",
		runtimeName: "wasmtime",
	}

	req := &ExecRequest{
		ModulePath: "/tmp/module.wasm",
		Function:   "_start", // default, should NOT emit --invoke
	}

	args := rt.BuildArgs(req, 64*1024*1024)
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--invoke") {
		t.Error("should not include --invoke for _start function")
	}
}

func TestCLIRuntime_CommandConstruction_Wasmer(t *testing.T) {
	rt := &CLIRuntime{
		binaryPath:  "/usr/local/bin/wasmer",
		runtimeName: "wasmer",
	}

	req := &ExecRequest{
		ModulePath: "/tmp/module.wasm",
		Args:       []string{"hello"},
		Env:        map[string]string{"FOO": "bar"},
	}

	args := rt.BuildArgs(req, 128*1024*1024)

	if args[0] != "run" {
		t.Errorf("expected wasmer args to start with run, got %q", args[0])
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--env FOO=bar") {
		t.Errorf("expected --env FOO=bar in wasmer args, got: %s", joined)
	}
	if !strings.Contains(joined, "/tmp/module.wasm") {
		t.Error("expected module path in wasmer args")
	}
}

// --- AutoDiscover Tests ---

func TestAutoDiscover_NotFound(t *testing.T) {
	// Temporarily override PATH to ensure no runtime is found.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	_, err := AutoDiscover()
	if err == nil {
		t.Fatal("expected error when no runtime in PATH")
	}
	if err != ErrRuntimeNotFound {
		t.Errorf("expected ErrRuntimeNotFound, got: %v", err)
	}
}

func TestNewCLIRuntime_NotFound(t *testing.T) {
	_, err := NewCLIRuntime("/nonexistent/path/to/wasmtime-xyz-fake")
	if err == nil {
		t.Fatal("expected error for non-existent binary")
	}
	if !strings.Contains(err.Error(), "runtime binary not found") {
		t.Errorf("expected runtime not found error, got: %v", err)
	}
}

// --- Sandbox Tests ---

func TestSandbox_Run(t *testing.T) {
	// Create a temp file to act as our "module".
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "test.wasm")
	if err := os.WriteFile(modulePath, []byte("fake wasm"), 0644); err != nil {
		t.Fatal(err)
	}

	expected := &ExecResult{
		Stdout:   []byte("hello world"),
		Stderr:   nil,
		ExitCode: 0,
		Duration: 100 * time.Millisecond,
	}

	mock := &mockRuntime{result: expected}
	sandbox := NewSandbox(SandboxConfig{
		Runtime: mock,
	})

	result, err := sandbox.Run(context.Background(), modulePath, []byte("input data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(result.Stdout) != "hello world" {
		t.Errorf("expected stdout hello world, got %q", string(result.Stdout))
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}

	// Verify the request was constructed correctly.
	if mock.lastReq == nil {
		t.Fatal("expected mock to receive a request")
	}
	if string(mock.lastReq.Stdin) != "input data" {
		t.Errorf("expected stdin input data, got %q", string(mock.lastReq.Stdin))
	}
}

func TestSandbox_ModuleNotFound(t *testing.T) {
	mock := &mockRuntime{result: &ExecResult{}}
	sandbox := NewSandbox(SandboxConfig{
		Runtime: mock,
	})

	_, err := sandbox.Run(context.Background(), "/nonexistent/module.wasm", nil)
	if err == nil {
		t.Fatal("expected error for non-existent module")
	}
	if !strings.Contains(err.Error(), "module not found") {
		t.Errorf("expected module not found error, got: %v", err)
	}
}

func TestSandbox_DefaultTimeoutApplied(t *testing.T) {
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "test.wasm")
	if err := os.WriteFile(modulePath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockRuntime{result: &ExecResult{}}
	sandbox := NewSandbox(SandboxConfig{
		Runtime: mock,
	})

	_, err := sandbox.Run(context.Background(), modulePath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.lastReq.Timeout != DefaultTimeout {
		t.Errorf("expected default timeout %v, got %v", DefaultTimeout, mock.lastReq.Timeout)
	}
}

func TestSandbox_CustomTimeoutApplied(t *testing.T) {
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "test.wasm")
	if err := os.WriteFile(modulePath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockRuntime{result: &ExecResult{}}
	customTimeout := 10 * time.Second
	sandbox := NewSandbox(SandboxConfig{
		Runtime:     mock,
		MaxDuration: customTimeout,
	})

	_, err := sandbox.Run(context.Background(), modulePath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.lastReq.Timeout != customTimeout {
		t.Errorf("expected custom timeout %v, got %v", customTimeout, mock.lastReq.Timeout)
	}
}

func TestSandbox_MaxMemoryApplied(t *testing.T) {
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "test.wasm")
	if err := os.WriteFile(modulePath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockRuntime{result: &ExecResult{}}
	customMem := uint64(128 * 1024 * 1024) // 128MB
	sandbox := NewSandbox(SandboxConfig{
		Runtime:   mock,
		MaxMemory: customMem,
	})

	_, err := sandbox.Run(context.Background(), modulePath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.lastReq.MemoryMax != customMem {
		t.Errorf("expected max memory %d, got %d", customMem, mock.lastReq.MemoryMax)
	}
}

func TestSandbox_DefaultMemoryApplied(t *testing.T) {
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "test.wasm")
	if err := os.WriteFile(modulePath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockRuntime{result: &ExecResult{}}
	sandbox := NewSandbox(SandboxConfig{
		Runtime: mock,
	})

	_, err := sandbox.Run(context.Background(), modulePath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.lastReq.MemoryMax != DefaultMemoryMax {
		t.Errorf("expected default max memory %d, got %d", DefaultMemoryMax, mock.lastReq.MemoryMax)
	}
}

func TestSandbox_RunWithArgs(t *testing.T) {
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "test.wasm")
	if err := os.WriteFile(modulePath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockRuntime{result: &ExecResult{Stdout: []byte("ok")}}
	sandbox := NewSandbox(SandboxConfig{
		Runtime: mock,
	})

	args := []string{"--verbose", "--output=json"}
	result, err := sandbox.RunWithArgs(context.Background(), modulePath, args, []byte("input"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(result.Stdout) != "ok" {
		t.Errorf("expected stdout ok, got %q", string(result.Stdout))
	}

	if len(mock.lastReq.Args) != 2 || mock.lastReq.Args[0] != "--verbose" {
		t.Errorf("expected args [--verbose --output=json], got %v", mock.lastReq.Args)
	}
}

func TestSandbox_NoRuntime(t *testing.T) {
	sandbox := NewSandbox(SandboxConfig{})

	_, err := sandbox.Run(context.Background(), "/tmp/module.wasm", nil)
	if err == nil {
		t.Fatal("expected error when no runtime configured")
	}
	if !strings.Contains(err.Error(), "no runtime configured") {
		t.Errorf("expected no runtime configured error, got: %v", err)
	}
}

func TestRuntimeNameFromPath(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/usr/local/bin/wasmtime", "wasmtime"},
		{"/usr/bin/wasm3", "wasm3"},
		{"/opt/wasmer/bin/wasmer", "wasmer"},
		{"/usr/local/bin/unknown-runtime", "wasmtime"}, // default
	}

	for _, tt := range tests {
		got := runtimeNameFromPath(tt.path)
		if got != tt.expected {
			t.Errorf("runtimeNameFromPath(%q) = %q, want %q", tt.path, got, tt.expected)
		}
	}
}
