package main

import (
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/wasm"
)

// This example demonstrates the WASM sandbox configuration and API.
// It shows how to:
// 1. Configure a Sandbox with memory limits and timeouts
// 2. Build an ExecRequest for running a WASM module
// 3. Attempt to auto-discover a WASM runtime on the system
// 4. Create a Sandbox instance (even without a real runtime, the config is valid)
//
// Note: Running actual WASM modules requires wasmtime, wasmer, or wasm3
// installed on the system. This example gracefully handles the case where
// no runtime is available.

func main() {
	fmt.Println("=== WASM Sandbox Configuration Example ===")
	fmt.Println()

	// Step 1: Show the sandbox configuration options.
	fmt.Println("--- Sandbox Defaults ---")
	fmt.Printf("  Default memory limit: %d MB\n", wasm.DefaultMemoryMax/(1024*1024))
	fmt.Printf("  Default timeout:      %v\n", wasm.DefaultTimeout)
	fmt.Printf("  Default fuel limit:   %d instructions\n", wasm.DefaultFuel)
	fmt.Println()

	// Step 2: Try to auto-discover a WASM runtime.
	fmt.Println("--- Runtime Discovery ---")
	runtime, err := wasm.AutoDiscover()
	if err != nil {
		fmt.Printf("  No WASM runtime found: %v\n", err)
		fmt.Println("  (Install wasmtime, wasmer, or wasm3 to enable WASM execution)")
		fmt.Println()
	} else {
		fmt.Printf("  Found runtime: %s at %s\n", runtime.Name(), runtime.BinaryPath())
		fmt.Println()
	}

	// Step 3: Configure a sandbox (works even without a runtime for demonstration).
	cfg := wasm.SandboxConfig{
		Runtime:      runtime, // may be nil if no runtime found
		AllowedPaths: []string{"/tmp", "/data"},
		MaxMemory:    128 * 1024 * 1024, // 128 MB
		MaxDuration:  10 * time.Second,
		MaxFuel:      5_000_000,
	}

	sandbox := wasm.NewSandbox(cfg)
	fmt.Println("--- Configured Sandbox ---")
	fmt.Printf("  Memory limit:  %d MB\n", cfg.MaxMemory/(1024*1024))
	fmt.Printf("  Timeout:       %v\n", cfg.MaxDuration)
	fmt.Printf("  Fuel limit:    %d\n", cfg.MaxFuel)
	fmt.Printf("  Allowed paths: %v\n", cfg.AllowedPaths)
	fmt.Println()

	// Step 4: Show how to build an ExecRequest.
	req := &wasm.ExecRequest{
		ModulePath: "/path/to/plugin.wasm",
		Function:   "_start",
		Stdin:      []byte(`{"prompt": "Hello from Go!"}`),
		Env: map[string]string{
			"AGENT_NAME": "sandbox-agent",
			"LOG_LEVEL":  "info",
		},
		Args:      []string{"--mode", "interactive"},
		Timeout:   5 * time.Second,
		MemoryMax: 32 * 1024 * 1024, // 32 MB for this specific module
	}

	fmt.Println("--- Example ExecRequest ---")
	fmt.Printf("  Module:    %s\n", req.ModulePath)
	fmt.Printf("  Function:  %s\n", req.Function)
	fmt.Printf("  Stdin:     %s\n", string(req.Stdin))
	fmt.Printf("  Env:       %v\n", req.Env)
	fmt.Printf("  Args:      %v\n", req.Args)
	fmt.Printf("  Timeout:   %v\n", req.Timeout)
	fmt.Printf("  MemoryMax: %d MB\n", req.MemoryMax/(1024*1024))
	fmt.Println()

	// Step 5: Demonstrate that the sandbox is ready (even if runtime is nil).
	_ = sandbox
	if runtime != nil {
		fmt.Printf("Sandbox is ready to execute WASM modules via %s.\n", runtime.Name())
	} else {
		fmt.Println("Sandbox configured but no runtime available.")
		fmt.Println("To run WASM modules, install one of: wasmtime, wasmer, wasm3")
	}
}
