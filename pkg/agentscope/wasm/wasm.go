package wasm

import (
	"context"
	"errors"
	"time"
)

// Runtime abstracts a WASM execution engine.
type Runtime interface {
	// Execute runs a WASM module with the given input and returns stdout.
	Execute(ctx context.Context, req *ExecRequest) (*ExecResult, error)
	// Name returns the runtime implementation name.
	Name() string
}

// ExecRequest describes what to execute.
type ExecRequest struct {
	ModulePath     string            // path to .wasm file
	Function       string            // exported function name (default: "_start")
	Stdin          []byte            // data piped to stdin
	Env            map[string]string // environment variables
	Args           []string          // command-line arguments
	AllowedPaths   []string          // host paths exposed to WASI; empty exposes none
	Timeout        time.Duration     // execution timeout (default: 30s)
	MemoryMax      uint64            // max memory in bytes (default: 64MB)
	Fuel           int64             // instruction fuel limit
	MaxOutputBytes int64             // combined stdout/stderr capture bound
}

// ExecResult holds the output of a WASM execution.
type ExecResult struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int
	Duration        time.Duration
	OutputTruncated bool
}

// Default values.
const (
	DefaultTimeout   = 30 * time.Second
	DefaultMemoryMax = 64 * 1024 * 1024 // 64MB
	DefaultFuel      = int64(1_000_000)
	DefaultOutputMax = int64(1024 * 1024)
)

// Errors.
var (
	ErrTimeout           = errors.New("wasm: execution timed out")
	ErrMemoryExceeded    = errors.New("wasm: memory limit exceeded")
	ErrModuleNotFound    = errors.New("wasm: module not found")
	ErrRuntimeNotFound   = errors.New("wasm: runtime binary not found")
	ErrUnsupportedLimits = errors.New("wasm: runtime cannot enforce requested limits")
)
