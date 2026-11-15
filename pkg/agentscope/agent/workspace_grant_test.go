package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// writeOutsideFile creates a file outside the workspace root and returns its path.
func writeOutsideFile(t *testing.T, content string) (root, outside string) {
	t.Helper()
	root = t.TempDir()
	outsideDir := t.TempDir()
	outside = filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func readCallResponse(id, path string) model.ChatResponse {
	input, _ := json.Marshal(map[string]any{"file_path": path})
	return model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: id, Name: "Read", Input: string(input), State: message.ToolCallPending},
		},
		IsLast: true,
	}
}

func finalTextResponse(text string) model.ChatResponse {
	return model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", ID: "fin", Text: text}},
		IsLast:  true,
	}
}

// runGrantReply drives one reply, answering the first confirm request with
// the given rules (nil = bare confirm). It returns the tool-result texts and
// whether a confirm was requested.
func runGrantReply(t *testing.T, a *UnifiedAgent, ctx context.Context, confirm bool, rules []any) (resultTexts []string, asked bool) {
	t.Helper()
	ch, err := a.ReplyStream(ctx, "read it")
	if err != nil {
		t.Fatal(err)
	}
	answered := false
	for evt := range ch {
		switch e := evt.(type) {
		case event.RequireUserConfirmEvent:
			asked = true
			if !answered {
				answered = true
				tc := e.ToolCalls[0]
				tc.State = message.ToolCallAllowed
				a.SubmitUserConfirm(&event.UserConfirmResultEvent{
					ConfirmResults: []event.ConfirmResult{{Confirmed: confirm, ToolCall: tc, Rules: rules}},
				})
			}
		case event.ToolResultEndEvent:
			_ = e
		}
	}
	// Pull tool-result texts from the recorded context.
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, msg := range a.state.Context {
		for _, blk := range msg.Content {
			if tr, ok := blk.(message.ToolResultBlock); ok {
				if s, ok := tr.Output.(string); ok {
					resultTexts = append(resultTexts, s)
				}
			}
		}
	}
	return resultTexts, asked
}

func TestWorkspaceEscape_GrantApproved_Retries(t *testing.T) {
	root, outside := writeOutsideFile(t, "s3cr3t content")
	mock := &mockChatModel{responses: []model.ChatResponse{
		readCallResponse("tc_1", outside),
		finalTextResponse("read complete"),
	}}
	permCtx := permission.NewContext(permission.ModeBypass)
	a := NewUnifiedAgent("grant-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permCtx),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ctx := tool.WithWorkspaceRoot(context.Background(), root)

	texts, asked := runGrantReply(t, a, ctx, true, nil)
	if !asked {
		t.Fatal("expected a workspace-grant confirm request")
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "s3cr3t content") {
		t.Errorf("expected retried read to return file content, got: %s", joined)
	}
	// The grant targets the existing file itself (narrower than its parent
	// directory) and is session-scoped.
	grantDir := outside
	found := false
	for _, d := range a.engine.GrantedDirectories() {
		if d == grantDir {
			found = true
		}
	}
	if !found {
		t.Errorf("expected granted dir %s, got %v", grantDir, a.engine.GrantedDirectories())
	}
	if wd, ok := a.engine.Context.WorkingDirectories[grantDir]; !ok || wd.Source != "granted" {
		t.Errorf("expected WorkingDirectories[%s] source=granted, got %+v", grantDir, wd)
	}
}

func TestWorkspaceEscape_EchoedGrantRule_Intercepted(t *testing.T) {
	root, outside := writeOutsideFile(t, "echo rule content")
	mock := &mockChatModel{responses: []model.ChatResponse{
		readCallResponse("tc_1", outside),
		finalTextResponse("done"),
	}}
	a := NewUnifiedAgent("grant-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ctx := tool.WithWorkspaceRoot(context.Background(), root)

	grantDir := outside
	rules := []any{permission.NewGrantDirRule(grantDir)}
	texts, asked := runGrantReply(t, a, ctx, true, rules)
	if !asked {
		t.Fatal("expected a workspace-grant confirm request")
	}
	if !strings.Contains(strings.Join(texts, "\n"), "echo rule content") {
		t.Errorf("expected retried read to succeed, got: %v", texts)
	}
	// The grant rule must NOT land in the permission rule lists.
	if got := a.engine.Context.AllowRules[permission.GrantRuleToolName]; len(got) > 0 {
		t.Errorf("grant rule leaked into allow rules: %v", got)
	}
	if _, ok := a.engine.Context.WorkingDirectories[grantDir]; !ok {
		t.Errorf("grant rule was not applied to working directories")
	}
}

func TestWorkspaceEscape_Denied(t *testing.T) {
	root, outside := writeOutsideFile(t, "denied content")
	mock := &mockChatModel{responses: []model.ChatResponse{
		readCallResponse("tc_1", outside),
		finalTextResponse("gave up"),
	}}
	a := NewUnifiedAgent("grant-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ctx := tool.WithWorkspaceRoot(context.Background(), root)

	texts, asked := runGrantReply(t, a, ctx, false, nil)
	if !asked {
		t.Fatal("expected a workspace-grant confirm request")
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "declined to grant access") {
		t.Errorf("expected denial note in tool result, got: %s", joined)
	}
	if len(a.engine.GrantedDirectories()) > 0 {
		t.Errorf("no grant should be recorded on denial, got %v", a.engine.GrantedDirectories())
	}
}

func TestWorkspaceEscape_ConcurrentBatch_NoPrompt(t *testing.T) {
	root, outside1 := writeOutsideFile(t, "one")
	_, outside2 := writeOutsideFile(t, "two")
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			mustToolCall(t, "tc_1", outside1),
			mustToolCall(t, "tc_2", outside2),
		}, IsLast: true},
		finalTextResponse("both failed"),
	}}
	a := NewUnifiedAgent("grant-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ctx := tool.WithWorkspaceRoot(context.Background(), root)

	// Must complete without any confirm prompt: a concurrent batch has no
	// event channel, and waiting there would deadlock the reply.
	texts, asked := runGrantReply(t, a, ctx, true, nil)
	if asked {
		t.Error("concurrent batch must not prompt for a grant")
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "escapes workspace root") {
		t.Errorf("expected escape errors to stand, got: %s", joined)
	}
	if len(a.engine.GrantedDirectories()) > 0 {
		t.Errorf("no grant should be recorded from a batch escape, got %v", a.engine.GrantedDirectories())
	}
}

func mustToolCall(t *testing.T, id, path string) message.ToolCallBlock {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"file_path": path})
	return message.ToolCallBlock{Type: "tool_call", ID: id, Name: "Read", Input: string(input), State: message.ToolCallPending}
}

func TestWorkspaceGrant_CheckpointRoundTrip(t *testing.T) {
	saver := newFakeStateSaver()
	permCtx := permission.NewContext(permission.ModeBypass)
	a := NewUnifiedAgent("grant-agent", "helpful", &mockChatModel{},
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permCtx),
		WithStateSaver(saver),
	)
	a.state.SessionID = "sess-grant"
	grantDir := t.TempDir()
	a.engine.AddWorkingDirectory(permission.WorkingDirectory{Path: grantDir, Source: "granted"})

	if err := a.SaveCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(context.Background(), saver, "sess-grant")
	if err != nil {
		t.Fatal(err)
	}
	wd, ok := loaded.PermissionCtx.WorkingDirectories[grantDir]
	if !ok || wd.Source != "granted" {
		t.Fatalf("grant missing from checkpoint: %+v", loaded.PermissionCtx.WorkingDirectories)
	}

	// A fresh engine (host-configured) picks the grant up from the restored state.
	fresh := NewUnifiedAgent("grant-agent", "helpful", &mockChatModel{},
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithState(loaded),
	)
	found := false
	for _, d := range fresh.engine.GrantedDirectories() {
		if d == grantDir {
			found = true
		}
	}
	if !found {
		t.Errorf("restored agent lacks granted dir %s", grantDir)
	}
}

func TestSuggestedGrantDir(t *testing.T) {
	dir := t.TempDir()
	if got := suggestedGrantDir(dir); got != dir {
		t.Errorf("existing dir should grant itself, got %s", got)
	}
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An existing file is granted itself: the jail treats a grant as covering
	// itself and anything under it, so there is no reason to widen to the
	// parent directory.
	if got := suggestedGrantDir(file); got != file {
		t.Errorf("existing file should grant itself, got %s", got)
	}
	missing := filepath.Join(dir, "new.txt")
	if got := suggestedGrantDir(missing); got != dir {
		t.Errorf("missing file should grant its parent, got %s", got)
	}
}

func TestWorkspaceEscapeFromMeta(t *testing.T) {
	if _, ok := workspaceEscapeFromMeta(nil); ok {
		t.Error("nil metadata must not parse")
	}
	if _, ok := workspaceEscapeFromMeta(map[string]any{"other": 1}); ok {
		t.Error("unrelated metadata must not parse")
	}
	if _, ok := workspaceEscapeFromMeta(map[string]any{tool.WorkspaceEscapeMetaKey: "junk"}); ok {
		t.Error("wrong-typed metadata must not parse")
	}
	esc, ok := workspaceEscapeFromMeta(map[string]any{
		tool.WorkspaceEscapeMetaKey: tool.WorkspaceEscapeInfo{Path: "/x", Root: "/r"},
	})
	if !ok || esc.Path != "/x" || esc.Root != "/r" {
		t.Errorf("valid escape metadata not parsed: %+v ok=%v", esc, ok)
	}
}

// Grant prompts must carry a human-meaningful suggested rule so clients can
// render "allow access to <dir>".
func TestWorkspaceEscape_SuggestedRuleCarriesDir(t *testing.T) {
	root, outside := writeOutsideFile(t, "rule content")
	mock := &mockChatModel{responses: []model.ChatResponse{
		readCallResponse("tc_1", outside),
		finalTextResponse("done"),
	}}
	a := NewUnifiedAgent("grant-agent", "helpful", mock,
		WithToolkit(tool.NewToolkit(tool.ReadTool())),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)
	ctx := tool.WithWorkspaceRoot(context.Background(), root)

	ch, err := a.ReplyStream(ctx, "read it")
	if err != nil {
		t.Fatal(err)
	}
	grantDir := outside
	seen := false
	answered := false
	for evt := range ch {
		e, ok := evt.(event.RequireUserConfirmEvent)
		if !ok {
			continue
		}
		for _, r := range e.ToolCalls[0].SuggestedRules {
			rule, isRule := r.(permission.Rule)
			if !isRule {
				continue
			}
			if dir, isGrant := permission.ParseGrantDirRule(rule); isGrant && dir == grantDir {
				seen = true
			}
		}
		if !answered {
			answered = true
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{
				ConfirmResults: []event.ConfirmResult{{Confirmed: false, ToolCall: e.ToolCalls[0]}},
			})
		}
	}
	if !seen {
		t.Errorf("expected suggested grant rule for %s", grantDir)
	}
}
