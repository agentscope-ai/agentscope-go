package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

var shellSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"command": {
			"type": "string",
			"description": "The shell command to execute in the persistent session"
		},
		"session": {
			"type": "string",
			"description": "Session name (default \"main\"). Named sessions keep independent working directories, environments and running commands."
		},
		"yield_time_ms": {
			"type": "number",
			"description": "Milliseconds to wait for completion before returning a running status (default 1000, max 30000). Poll a running command with ShellStdin."
		},
		"max_output_bytes": {
			"type": "number",
			"description": "Output budget in bytes; head and tail are kept, the middle elided (default 30720)"
		}
	},
	"required": ["command"]
}`)

var shellStdinSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session": {
			"type": "string",
			"description": "Session name of the running command"
		},
		"chars": {
			"type": "string",
			"description": "Characters to write to the foreground command's stdin (e.g. \"y\\n\"); empty to only poll output"
		},
		"yield_time_ms": {
			"type": "number",
			"description": "Milliseconds to wait for completion before returning a running status (default 1000, max 30000)"
		},
		"max_output_bytes": {
			"type": "number",
			"description": "Output budget in bytes; head and tail are kept, the middle elided (default 30720)"
		}
	},
	"required": ["session"]
}`)

// shellTool executes commands in persistent shell sessions (codex
// unified_exec parity). Unlike Bash (one process per call), a session's
// working directory, exported variables and foreground command survive
// across calls, and long-running commands are polled or fed via ShellStdin.
type shellTool struct {
	BaseTool
	manager *ShellManager
	// perms reuses the bash permission pipeline (injection/danger heuristics,
	// rule matching, suggestions) under this tool's own name, so learned
	// rules for Shell never silently widen Bash and vice versa.
	perms *bashTool
}

// ShellTool returns the persistent-shell tool. The manager owns the session
// processes; keep it alive for the toolkit's lifetime and Close it on host
// shutdown.
func ShellTool(manager *ShellManager) Tool {
	return &shellTool{
		BaseTool: BaseTool{
			ToolName: "Shell",
			ToolDescription: "Execute a shell command in a persistent session. " +
				"State (cwd, exported variables, background processes) survives across calls. " +
				"Long-running commands return a running status; poll or interact via ShellStdin. " +
				"Commands must be single-line (no heredocs with separate terminator lines). " +
				"\"exit\" and \"exec\" terminate the session and reset its state.",
			ToolSchema: shellSchema,
		},
		manager: manager,
		perms:   &bashTool{BaseTool: BaseTool{ToolName: "Shell"}},
	}
}

func (t *shellTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return NewErrorResponse(fmt.Errorf("command is required")), nil
	}
	if strings.Contains(command, "\n") {
		// Multi-line input (incl. heredocs whose terminator must own its
		// line) wedges the completion sentinel, which shares the command's
		// final line. Guide the model to the supported shapes.
		return NewErrorResponse(fmt.Errorf("multi-line commands are not supported in Shell sessions; write files with the Write tool, chain commands with &&, or run a single-line heredoc opener (e.g. \"cat <<EOF\") and feed its body via ShellStdin")), nil
	}
	session, _ := args["session"].(string)
	yield := time.Duration(numberArg(args, "yield_time_ms", 1000)) * time.Millisecond
	maxOutput := numberArg(args, "max_output_bytes", defaultShellMaxOutput)

	res, err := t.manager.Exec(ctx, session, command, yield, maxOutput)
	if err != nil {
		return NewErrorResponse(err), nil
	}
	return shellResultResponse(res), nil
}

// Permission pipeline: identical semantics to Bash (R-5a), keyed on the
// shared "command" argument.
func (t *shellTool) CheckPermissions(input map[string]any, ctx *permission.Context) permission.Decision {
	return t.perms.CheckPermissions(input, ctx)
}

func (t *shellTool) CheckReadOnly(input map[string]any) bool {
	return t.perms.CheckReadOnly(input)
}

func (t *shellTool) MatchRule(ruleContent string, input map[string]any) bool {
	return t.perms.MatchRule(ruleContent, input)
}

// MatchAllSubcommands and MatchAnySubcommand delegate the compound matchers so
// the engine's CompoundRuleMatcher dispatch applies to Shell exactly as to
// Bash — without them Shell silently falls back to single-rule matching,
// where a denied subcommand can ride on an innocent compound head.
func (t *shellTool) MatchAllSubcommands(rules []string, input map[string]any) bool {
	return t.perms.MatchAllSubcommands(rules, input)
}

func (t *shellTool) MatchAnySubcommand(rules []string, input map[string]any) bool {
	return t.perms.MatchAnySubcommand(rules, input)
}

func (t *shellTool) GenerateSuggestions(input map[string]any) []permission.Rule {
	return t.perms.GenerateSuggestions(input)
}

// shellStdinTool writes to or polls a running session command.
type shellStdinTool struct {
	BaseTool
	manager *ShellManager
}

// ShellStdinTool returns the stdin/poll companion tool for ShellTool.
func ShellStdinTool(manager *ShellManager) Tool {
	return &shellStdinTool{
		BaseTool: BaseTool{
			ToolName: "ShellStdin",
			ToolDescription: "Write characters to a running Shell session command's stdin, or poll its output. " +
				"There is no PTY: control bytes like \\x03 (Ctrl-C) are literal input, not signals; " +
				"abort a command by interrupting the turn or killing the session.",
			ToolSchema: shellStdinSchema,
		},
		manager: manager,
	}
}

func (t *shellStdinTool) Execute(ctx context.Context, args map[string]any) (*ToolResponse, error) {
	session, _ := args["session"].(string)
	if strings.TrimSpace(session) == "" {
		return NewErrorResponse(fmt.Errorf("session is required")), nil
	}
	chars, _ := args["chars"].(string)
	yield := time.Duration(numberArg(args, "yield_time_ms", 1000)) * time.Millisecond
	maxOutput := numberArg(args, "max_output_bytes", defaultShellMaxOutput)

	res, err := t.manager.WriteStdin(ctx, session, chars, yield, maxOutput)
	if err != nil {
		return NewErrorResponse(err), nil
	}
	return shellResultResponse(res), nil
}

// Permission passthrough: the command was already checked when the session
// executed it; stdin carries no new command surface.
func (t *shellStdinTool) CheckPermissions(map[string]any, *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorPassthrough}
}

func shellResultResponse(res *ShellExecResult) *ToolResponse {
	b, _ := json.Marshal(res)
	resp := NewTextResponse(string(b))
	if res.Status == ShellStatusDone && res.ExitCode != 0 {
		resp.State = message.ToolResultError
	}
	return resp
}

func numberArg(args map[string]any, key string, fallback int) int {
	v, ok := args[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return fallback
}
