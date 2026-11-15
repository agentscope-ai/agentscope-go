package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/audit"
	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/sandbox"
)

// Orchestrator composes permission checking, sandbox policy enforcement and
// tool execution into a pipeline. It can be adapted to loop.ToolExecutor via
// AsToolExecutor.
type Orchestrator struct {
	toolkit            *Toolkit
	permEngine         *permission.Engine
	sandbox            sandbox.Sandbox
	policy             *sandbox.Policy
	auditLogger        audit.Logger
	onPermDenied       func(string, permission.Decision)
	defaultToolTimeout time.Duration
	maxToolResultBytes int
}

// OrchestratorConfig holds the configuration for creating an Orchestrator.
type OrchestratorConfig struct {
	Toolkit            *Toolkit
	PermEngine         *permission.Engine
	Sandbox            sandbox.Sandbox
	OnPermissionDenied func(toolName string, decision permission.Decision)

	// Policy configures sandbox restrictions enforced before every tool call.
	// When set the orchestrator blocks tool calls that violate the policy
	// (e.g. writes under FSReadOnly, bash under AllowExec=false) and injects
	// the policy into the execution context so individual tools can read it.
	Policy *sandbox.Policy

	// AuditLogger receives a structured [audit.Entry] for every tool
	// execution, permission decision, and policy denial. Nil disables audit.
	AuditLogger audit.Logger

	// DefaultToolTimeout bounds each tool execution. Zero means no timeout (a
	// blocking custom/MCP tool could otherwise hang the whole loop).
	DefaultToolTimeout time.Duration
	// MaxToolResultBytes caps the text size of a tool result. Zero means no cap.
	MaxToolResultBytes int
}

// OrchestratorResult pairs a tool call with its execution result or error.
type OrchestratorResult struct {
	Call     message.ToolCallBlock
	Response *ToolResponse
	Err      error
}

// NewOrchestrator creates an Orchestrator from the given config.
func NewOrchestrator(cfg OrchestratorConfig) *Orchestrator { //nolint:gocritic // config structs are passed by value at construction
	al := cfg.AuditLogger
	if al == nil {
		al = audit.NopLogger{}
	}
	o := &Orchestrator{
		toolkit:            cfg.Toolkit,
		permEngine:         cfg.PermEngine,
		sandbox:            cfg.Sandbox,
		policy:             cfg.Policy,
		auditLogger:        al,
		onPermDenied:       cfg.OnPermissionDenied,
		defaultToolTimeout: cfg.DefaultToolTimeout,
		maxToolResultBytes: cfg.MaxToolResultBytes,
	}
	// If a policy specifies a resource timeout and no explicit tool timeout
	// was configured, adopt the policy timeout.
	if o.policy != nil && o.policy.Resources.TimeoutSec > 0 && o.defaultToolTimeout == 0 {
		o.defaultToolTimeout = time.Duration(o.policy.Resources.TimeoutSec) * time.Second
	}
	return o
}

// Execute runs a single tool call through the permission + execution pipeline.
func (o *Orchestrator) Execute(ctx context.Context, call message.ToolCallBlock) (*ToolResponse, error) { //nolint:gocritic // public API
	start := time.Now()

	// 1. Look up tool
	t := o.toolkit.Get(call.Name)
	if t == nil {
		return nil, &agenterrors.ToolNotFoundError{ToolName: call.Name}
	}

	// 2. Parse input
	input, err := call.ParseInput()
	if err != nil {
		return NewErrorResponse(fmt.Errorf("parse input: %w", err)), nil
	}

	// 3. Permission check. When a workspace backend will execute the call,
	// shell-specific checks follow the backend (POSIX containers) instead of
	// the host OS.
	if o.permEngine != nil {
		var decision permission.Decision
		var permErr error
		if pctx := BackendPermissionContext(ctx, o.permEngine.Context); pctx != nil {
			decision, permErr = o.permEngine.CheckPermissionInContext(t, input, pctx)
		} else {
			decision, permErr = o.permEngine.CheckPermission(t, input)
		}
		if permErr != nil {
			return nil, fmt.Errorf("permission check: %w", permErr)
		}

		switch decision.Behavior {
		case permission.BehaviorDeny:
			if o.onPermDenied != nil {
				o.onPermDenied(call.Name, decision)
			}
			_ = o.auditLogger.Log(ctx, &audit.Entry{
				Timestamp:  start,
				ReplyID:    audit.ReplyIDFromCtx(ctx),
				ToolCallID: call.ID,
				Action:     audit.ActionPermissionAsk,
				ToolName:   call.Name,
				Input:      call.Input,
				Decision:   "ask",
				Reason:     decision.Message,
			})

			return &ToolResponse{
				Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: decision.Message}},
				State:   message.ToolResultError,
			}, nil
		case permission.BehaviorAsk:
			if o.onPermDenied != nil {
				o.onPermDenied(call.Name, decision)
			}
			_ = o.auditLogger.Log(ctx, &audit.Entry{
				Timestamp:  start,
				ReplyID:    audit.ReplyIDFromCtx(ctx),
				ToolCallID: call.ID,
				Action:     audit.ActionPermissionAsk,
				ToolName:   call.Name,
				Input:      call.Input,
				Decision:   "ask",
				Reason:     decision.Message,
			})
			return &ToolResponse{
				Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "Permission required: " + decision.Message}},
				State:   message.ToolResultError,
			}, nil
		}
	}

	// 4. Sandbox policy enforcement — reject calls that violate the policy
	// before any execution happens.
	if o.policy != nil {
		if resp := o.enforceSandboxPolicy(call.Name, input); resp != nil {
			_ = o.auditLogger.Log(ctx, &audit.Entry{
				Timestamp:  start,
				ReplyID:    audit.ReplyIDFromCtx(ctx),
				ToolCallID: call.ID,
				Action:     audit.ActionPolicyDenied,
				ToolName:   call.Name,
				Input:      call.Input,
				Decision:   "policy_denied",
				Reason:     resp.Content[0].(message.TextBlock).Text,
			})
			return resp, nil
		}
	}

	// 5. Execute via toolkit (handles validation + middleware), bounded by an
	// optional per-tool timeout and result-size cap. If a sandbox policy is
	// configured, inject it into the context so individual tools can read it.
	execCtx := ctx
	if o.policy != nil {
		execCtx = sandbox.WithPolicy(execCtx, o.policy)
	}
	if o.defaultToolTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, o.defaultToolTimeout)
		defer cancel()
	}
	resp, err := o.toolkit.callResolvedTool(execCtx, call.Name, input, t)
	elapsed := time.Since(start)

	// 6. Audit the execution result.
	auditEntry := &audit.Entry{
		Timestamp:  start,
		ReplyID:    audit.ReplyIDFromCtx(ctx),
		ToolCallID: call.ID,
		Action:     audit.ActionToolExecute,
		ToolName:   call.Name,
		Input:      call.Input,
		Decision:   "allowed",
		Duration:   elapsed,
	}
	if err != nil {
		auditEntry.Error = err.Error()
	}
	if resp != nil {
		auditEntry.Output = truncateForAudit(resp)
	}
	_ = o.auditLogger.Log(ctx, auditEntry)

	if err != nil {
		return resp, err
	}
	return truncateToolResponse(resp, o.maxToolResultBytes), nil
}

// truncateToolResponse caps the total text length of a tool response at max
// bytes (max <= 0 disables the cap), appending a truncation notice.
func truncateToolResponse(resp *ToolResponse, max int) *ToolResponse {
	if resp == nil || max <= 0 {
		return resp
	}
	total := 0
	for i, b := range resp.Content {
		tb, ok := b.(message.TextBlock)
		if !ok {
			continue
		}
		if total+len(tb.Text) <= max {
			total += len(tb.Text)
			continue
		}
		keep := max - total
		if keep < 0 {
			keep = 0
		}
		over := total + len(tb.Text) - max
		tb.Text = tb.Text[:keep] + fmt.Sprintf("\n... [truncated, %d bytes over %d-byte limit]", over, max)
		resp.Content[i] = tb
		resp.Content = resp.Content[:i+1] // drop any blocks past the cap
		break
	}
	return resp
}

// BatchExecute runs each call through Execute sequentially and collects results.
func (o *Orchestrator) BatchExecute(ctx context.Context, calls []message.ToolCallBlock) []*OrchestratorResult {
	results := make([]*OrchestratorResult, len(calls))
	for i, call := range calls {
		resp, err := o.Execute(ctx, call)
		results[i] = &OrchestratorResult{
			Call:     call,
			Response: resp,
			Err:      err,
		}
	}
	return results
}

// truncateForAudit extracts a short text summary from a ToolResponse for the
// audit log. Output is capped at 512 bytes to keep audit entries compact.
func truncateForAudit(resp *ToolResponse) string {
	if resp == nil || len(resp.Content) == 0 {
		return ""
	}
	const maxAuditOutput = 512
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			if len(tb.Text) > maxAuditOutput {
				return tb.Text[:maxAuditOutput] + "..."
			}
			return tb.Text
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Sandbox policy enforcement
// ---------------------------------------------------------------------------

// writingTools lists tool names that mutate the filesystem.
var writingTools = map[string]bool{
	"Write": true, "Edit": true, "MultiEdit": true, "ApplyPatch": true,
	"NotebookEdit": true,
}

// enforceSandboxPolicy checks the call against the configured sandbox.Policy
// and returns a denied ToolResponse if the call violates the policy. Returns
// nil when the call is acceptable.
func (o *Orchestrator) enforceSandboxPolicy(toolName string, input map[string]any) *ToolResponse {
	p := o.policy

	// ---- Process policy ----
	if !p.Process.AllowExec && toolName == "Bash" {
		return policyDeniedResponse("sandbox policy denies command execution (AllowExec=false)")
	}

	// ---- Filesystem policy ----
	switch p.FileSystem.Mode {
	case sandbox.FSReadOnly:
		if writingTools[toolName] {
			return policyDeniedResponse("sandbox policy enforces read-only filesystem")
		}
		// Bash commands that are not read-only are blocked.
		if toolName == "Bash" {
			if cmdStr, _ := input["command"].(string); cmdStr != "" {
				if !IsReadOnlyCommand(strings.TrimSpace(cmdStr)) {
					return policyDeniedResponse("sandbox policy enforces read-only filesystem; command is not read-only")
				}
			}
		}
	case sandbox.FSWorkspaceOnly:
		// workspace jail already enforces path confinement; additionally
		// check explicit DenyPaths from the policy.
	}

	// Check deny-listed paths for tools that operate on files.
	if len(p.FileSystem.DenyPaths) > 0 {
		for _, fp := range extractToolFilePaths(toolName, input) {
			clean := filepath.Clean(fp)
			for _, deny := range p.FileSystem.DenyPaths {
				denyClean := filepath.Clean(deny)
				if clean == denyClean || strings.HasPrefix(clean, denyClean+string(filepath.Separator)) {
					return policyDeniedResponse(fmt.Sprintf("path %q denied by sandbox policy", fp))
				}
			}
		}
	}

	// ---- Network policy ----
	if p.Network.Mode == sandbox.NetDisabled && toolName == "WebFetch" {
		return policyDeniedResponse("sandbox policy denies network access (NetDisabled)")
	}

	return nil // no violation
}

// extractToolFilePaths returns file paths referenced by a tool call's input.
func extractToolFilePaths(toolName string, input map[string]any) []string {
	switch toolName {
	case "Read", "Write", "Edit", "MultiEdit", "ApplyPatch":
		if p, ok := input["file_path"].(string); ok && p != "" {
			return []string{p}
		}
	case "Glob":
		if p, ok := input["pattern"].(string); ok && p != "" {
			return []string{p}
		}
	case "Bash":
		if cmdStr, ok := input["command"].(string); ok {
			return ExtractFilePaths(cmdStr)
		}
	}
	return nil
}

// policyDeniedResponse creates a ToolResponse indicating a sandbox policy denial.
func policyDeniedResponse(msg string) *ToolResponse {
	return &ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: msg}},
		State:   message.ToolResultError,
	}
}

// AsToolExecutor returns the orchestrator itself. loop.ToolResult aliases
// OrchestratorResult, so the orchestrator satisfies loop.ToolExecutor without
// a circular import or a signature-changing adapter.
func (o *Orchestrator) AsToolExecutor() *Orchestrator { return o }
