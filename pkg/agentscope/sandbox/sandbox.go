package sandbox

import (
	"context"
	"sort"
	"sync"
	"time"
)

type policyCtxKey struct{}

// WithPolicy returns a copy of ctx carrying the given sandbox Policy.
func WithPolicy(ctx context.Context, p *Policy) context.Context {
	return context.WithValue(ctx, policyCtxKey{}, p)
}

// GetPolicy returns the sandbox Policy attached to ctx, or nil.
func GetPolicy(ctx context.Context) *Policy {
	if v, ok := ctx.Value(policyCtxKey{}).(*Policy); ok {
		return v
	}
	return nil
}

// Sandbox defines the interface for sandboxed command execution.
type Sandbox interface {
	Type() string
	Available() bool
	Execute(ctx context.Context, req *ExecRequest) (*ExecResult, error)
	Setup(ctx context.Context, policy Policy) error
	Teardown(ctx context.Context) error
}

// ExecRequest describes a command to execute in a sandbox.
type ExecRequest struct {
	Command string
	Args    []string
	Dir     string
	Env     map[string]string
	Timeout time.Duration
}

// ExecResult holds the outcome of a sandboxed command execution.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// FSMode controls filesystem access within a sandbox.
type FSMode int

const (
	// FSReadOnly restricts the sandbox to read-only filesystem access.
	FSReadOnly FSMode = iota
	// FSWorkspaceOnly allows writes only within the workspace root.
	FSWorkspaceOnly
	// FSFullAccess allows unrestricted filesystem access.
	FSFullAccess
)

// NetMode controls network access within a sandbox.
type NetMode int

const (
	// NetDisabled blocks all network access from the sandbox.
	NetDisabled NetMode = iota
	// NetAllowList permits network access only to explicitly allowed hosts/ports.
	NetAllowList
	// NetFullAccess allows unrestricted network access.
	NetFullAccess
)

// FileSystemPolicy configures filesystem restrictions for a sandbox.
type FileSystemPolicy struct {
	Mode          FSMode
	WritableRoots []string
	DenyPaths     []string
}

// NetworkPolicy configures network restrictions for a sandbox.
type NetworkPolicy struct {
	Mode         NetMode
	AllowedHosts []string
	AllowedPorts []int
}

// ProcessPolicy configures process execution restrictions for a sandbox.
type ProcessPolicy struct {
	AllowExec    bool
	MaxProcesses int
}

// ResourcePolicy configures resource limits for a sandbox.
type ResourcePolicy struct {
	MaxMemoryMB   int
	MaxCPUPercent int
	MaxDiskMB     int
	TimeoutSec    int
}

// Policy aggregates all sandbox restriction policies.
type Policy struct {
	FileSystem FileSystemPolicy
	Network    NetworkPolicy
	Process    ProcessPolicy
	Resources  ResourcePolicy
}

// SandboxProvider creates Sandbox instances.
type SandboxProvider interface {
	Name() string
	Priority() int
	Available() bool
	Create(cfg map[string]any) (Sandbox, error)
}

var (
	registryMu sync.RWMutex
	providers  []SandboxProvider
)

// RegisterProvider adds a SandboxProvider to the global registry.
func RegisterProvider(p SandboxProvider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	providers = append(providers, p)
}

// ListProviders returns a copy of all registered providers.
func ListProviders() []SandboxProvider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]SandboxProvider, len(providers))
	copy(out, providers)
	return out
}

// AutoSelect picks the highest-priority available provider, creates a sandbox,
// and sets it up with the given policy.
func AutoSelect(policy Policy) (Sandbox, error) { //nolint:gocritic // public API
	registryMu.RLock()
	sorted := make([]SandboxProvider, len(providers))
	copy(sorted, providers)
	registryMu.RUnlock()

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Priority() > sorted[j].Priority()
	})

	for _, p := range sorted {
		if p.Available() {
			sb, err := p.Create(nil)
			if err != nil {
				continue
			}
			if err := sb.Setup(context.Background(), policy); err != nil {
				continue
			}
			return sb, nil
		}
	}
	return nil, &NoAvailableSandboxError{}
}

// NoAvailableSandboxError is returned when no sandbox provider is available.
type NoAvailableSandboxError struct{}

func (e *NoAvailableSandboxError) Error() string {
	return "sandbox: no available provider"
}

// ResetProviders clears the global provider registry. Intended for test cleanup.
func ResetProviders() {
	registryMu.Lock()
	providers = nil
	registryMu.Unlock()
}
