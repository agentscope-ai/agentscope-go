package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// escapeRetriedKey marks a tool-execution context as the single sanctioned
// re-execution after a workspace-jail grant, so a pathological second escape
// surfaces as a plain error instead of re-entering the grant prompt.
type escapeRetriedKey struct{}

// workspaceEscapeFromMeta extracts workspace-escape info from tool response
// metadata (see tool.WorkspaceEscapeMetaKey).
func workspaceEscapeFromMeta(md map[string]any) (tool.WorkspaceEscapeInfo, bool) {
	if md == nil {
		return tool.WorkspaceEscapeInfo{}, false
	}
	esc, ok := md[tool.WorkspaceEscapeMetaKey].(tool.WorkspaceEscapeInfo)
	if !ok || esc.Path == "" {
		return tool.WorkspaceEscapeInfo{}, false
	}
	return esc, true
}

// suggestedGrantDir picks the narrowest grant covering a jailed path: an
// existing path (file or directory) is granted itself — the jail check treats
// a grant as covering itself and anything under it, so granting the file is
// sufficient and avoids widening to its whole parent directory (consider a
// read of /etc/passwd). A non-existent path (e.g. a file about to be written)
// grants its parent.
func suggestedGrantDir(path string) string {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Dir(path)
	}
	if _, statErr := os.Stat(abs); statErr == nil {
		return abs
	}
	return filepath.Dir(abs)
}

// handleWorkspaceEscape asks the user to grant access to a jailed path (the
// path itself when it exists, otherwise its parent directory) and, on
// approval, records the grant and re-executes the tool
// call once with the widened jail. On rejection the original escape error is
// returned with a denial note. Concurrent tool batches never reach this: they
// run with a nil event channel, where a confirm prompt would be dropped and
// the wait would deadlock; the escape error stands there and the model can
// retry the call by itself.
func (a *UnifiedAgent) handleWorkspaceEscape(
	ctx context.Context,
	ch chan<- event.Event,
	replyID string,
	tc *message.ToolCallBlock,
	actingHandler middleware.ActingHandler,
	esc tool.WorkspaceEscapeInfo,
	errText string,
) toolOutcome {
	dir := suggestedGrantDir(esc.Path)
	grantRule := permission.NewGrantDirRule(dir)

	a.updateToolCallState(tc.ID, message.ToolCallAsking)
	tc.SuggestedRules = rulesToAny([]permission.Rule{grantRule})
	emit(ctx, ch, event.NewRequireUserConfirmEvent(replyID, []message.ToolCallBlock{*tc}))

	confirmed, resultTC := a.waitForConfirmation(ctx, answerIDs{reply: replyID}, tc.ID)
	if !confirmed {
		a.updateToolCallState(tc.ID, message.ToolCallFinished)
		return a.emitToolResult(ctx, ch, replyID, tc, message.ToolResultError,
			fmt.Sprintf("%s (user declined to grant access to %s)", errText, dir))
	}
	a.updateToolCallState(tc.ID, message.ToolCallAllowed)
	*tc = resultTC

	// A bare "yes" must still unlock the retry: the grant applies on any
	// confirmation, not only when the client echoes the suggested rule back.
	// Echoed grant rules reach the same place via waitForConfirmation, and
	// AddWorkingDirectory is idempotent per path.
	a.engine.AddWorkingDirectory(permission.WorkingDirectory{Path: dir, Source: "granted"})
	retryCtx := context.WithValue(
		tool.WithWorkspaceGrants(ctx, a.engine.GrantedDirectories()),
		escapeRetriedKey{}, true,
	)
	return a.executeTool(retryCtx, ch, replyID, tc, actingHandler)
}
