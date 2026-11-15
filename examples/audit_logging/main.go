// Example: audit_logging demonstrates the structured audit logging and
// sandbox policy enforcement capabilities.
//
// It configures an Orchestrator with:
//   - A sandbox policy (read-only filesystem, network disabled)
//   - An in-memory audit logger that records every decision
//
// Then it shows how tool calls are blocked by policy and how every action
// is recorded in the audit trail.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/audit"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/sandbox"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func main() {
	// 1. Create a toolkit with common tools.
	tk := tool.NewToolkit(
		tool.BashTool(),
		tool.ReadTool(),
		tool.WriteTool(),
	)

	// 2. Define a restrictive sandbox policy.
	policy := &sandbox.Policy{
		FileSystem: sandbox.FileSystemPolicy{
			Mode:      sandbox.FSReadOnly,
			DenyPaths: []string{"/etc/secrets", "/var/private"},
		},
		Network: sandbox.NetworkPolicy{
			Mode: sandbox.NetDisabled,
		},
		Process: sandbox.ProcessPolicy{
			AllowExec: true, // bash allowed, but only read-only commands
		},
		Resources: sandbox.ResourcePolicy{
			TimeoutSec: 30,
		},
	}

	// 3. Set up an in-memory audit logger.
	auditLog := audit.NewInMemoryLogger()

	// 4. Create the orchestrator with policy + audit.
	orch := tool.NewOrchestrator(tool.OrchestratorConfig{
		Toolkit:     tk,
		Policy:      policy,
		AuditLogger: auditLog,
	})

	ctx := context.Background()

	// 5. Simulate tool calls and observe policy enforcement.
	calls := []struct {
		name  string
		input string
	}{
		// This should succeed (read-only bash command).
		{"Bash", `{"command": "ls -la /tmp"}`},
		// This should be blocked (write under read-only policy).
		{"Write", `{"file_path": "/workspace/output.txt", "content": "hello"}`},
		// This should be blocked (non-read-only bash).
		{"Bash", `{"command": "echo 'hi' > /tmp/test.txt"}`},
		// This would work without policy, but Read on denied path is blocked.
		{"Read", `{"file_path": "/etc/secrets/api_key.txt"}`},
	}

	fmt.Println("=== Tool Execution Results ===")
	fmt.Println()

	for _, c := range calls {
		call := message.ToolCallBlock{
			Type:  "tool_call",
			ID:    "call-" + c.name,
			Name:  c.name,
			Input: c.input,
		}

		resp, err := orch.Execute(ctx, call)
		status := "SUCCESS"
		detail := ""
		if err != nil {
			status = "ERROR"
			detail = err.Error()
		} else if resp.State == message.ToolResultError {
			status = "DENIED"
			if len(resp.Content) > 0 {
				if tb, ok := resp.Content[0].(message.TextBlock); ok {
					detail = tb.Text
				}
			}
		}

		fmt.Printf("[%s] %s(%s)\n", status, c.name, c.input)
		if detail != "" {
			fmt.Printf("        → %s\n", detail)
		}
		fmt.Println()
	}

	// 6. Print the full audit trail.
	fmt.Println("=== Audit Trail ===")
	fmt.Println()

	entries := auditLog.Entries()
	for i := range entries {
		data, _ := json.MarshalIndent(&entries[i], "", "  ")
		fmt.Printf("Entry %d:\n%s\n\n", i+1, string(data))
	}

	fmt.Printf("Total audit entries: %d\n", len(entries))

	// 7. File-based audit logger example (writes to /tmp).
	filePath := "/tmp/agentscope-audit.jsonl"
	fileLog, err := audit.NewFileLogger(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create file logger: %v\n", err)
		return
	}
	defer fileLog.Close()

	// Log a sample entry to file.
	_ = fileLog.Log(ctx, &audit.Entry{
		Action:   audit.ActionToolExecute,
		ToolName: "Bash",
		Input:    "ls -la",
		Decision: "allowed",
	})
	fmt.Printf("\nFile audit log written to: %s\n", filePath)
}
