package tool

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func skipShellOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("persistent shell sessions require a POSIX shell")
	}
}

func newTestShellManager(t *testing.T) *ShellManager {
	t.Helper()
	skipShellOnWindows(t)
	m, err := NewShellManager(WithShellManagerCwd(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestShellSessionStatePersistsAcrossCalls(t *testing.T) {
	m := newTestShellManager(t)
	ctx := context.Background()

	if _, err := m.Exec(ctx, "", "export ASG_MARKER=hello && cd /tmp", 5*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	res, err := m.Exec(ctx, "", "pwd && echo $ASG_MARKER", 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusDone || res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "/tmp") || !strings.Contains(res.Output, "hello") {
		t.Fatalf("cwd/env did not persist: %q", res.Output)
	}
}

func TestShellSessionNamedSessionsAreIndependent(t *testing.T) {
	m := newTestShellManager(t)
	ctx := context.Background()

	if _, err := m.Exec(ctx, "alpha", "export WHO=alpha", 5*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Exec(ctx, "beta", "export WHO=beta", 5*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	a, _ := m.Exec(ctx, "alpha", "echo $WHO", 5*time.Second, 0)
	b, _ := m.Exec(ctx, "beta", "echo $WHO", 5*time.Second, 0)
	if !strings.Contains(a.Output, "alpha") || !strings.Contains(b.Output, "beta") {
		t.Fatalf("sessions leaked state: alpha=%q beta=%q", a.Output, b.Output)
	}
}

func TestShellSessionYieldThenPoll(t *testing.T) {
	m := newTestShellManager(t)
	ctx := context.Background()

	res, err := m.Exec(ctx, "", "sleep 2 && echo finished", 200*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusRunning {
		t.Fatalf("status = %s, want running", res.Status)
	}
	// Busy while running.
	if _, err := m.Exec(ctx, "", "echo nope", time.Second, 0); err == nil ||
		!strings.Contains(err.Error(), "busy") {
		t.Fatalf("concurrent exec on busy session: err=%v", err)
	}
	// Poll to completion.
	res, err = m.WriteStdin(ctx, "", "", 10*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusDone || res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "finished") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestShellSessionInteractiveStdin(t *testing.T) {
	m := newTestShellManager(t)
	ctx := context.Background()

	res, err := m.Exec(ctx, "", "read answer && echo got-$answer", 300*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusRunning {
		t.Fatalf("status = %s, want running", res.Status)
	}
	res, err = m.WriteStdin(ctx, "", "yes\n", 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusDone || !strings.Contains(res.Output, "got-yes") {
		t.Fatalf("res = %+v", res)
	}
}

func TestShellSessionExitCodePropagates(t *testing.T) {
	m := newTestShellManager(t)
	res, err := m.Exec(context.Background(), "", "false", 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusDone || res.ExitCode != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestShellSessionCancelKillsAndRecreates(t *testing.T) {
	m := newTestShellManager(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan *ShellExecResult, 1)
	go func() {
		res, _ := m.Exec(ctx, "", "sleep 60", 30*time.Second, 0)
		done <- res
	}()
	time.Sleep(300 * time.Millisecond) // let the command start
	cancel()
	select {
	case res := <-done:
		if !strings.Contains(res.Output, "interrupted") {
			t.Fatalf("output = %q", res.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not release the exec")
	}

	// The killed session is dropped; the next Exec recreates it.
	res, err := m.Exec(context.Background(), "", "echo alive", 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "alive") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestShellSessionHeadTailTruncation(t *testing.T) {
	m := newTestShellManager(t)
	// 200 numbered lines; small budget keeps head and tail only.
	res, err := m.Exec(context.Background(), "", "seq 1 200", 5*time.Second, 512)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "<<<TRUNCATED:") {
		t.Fatalf("no truncation marker in %q", res.Output)
	}
	if !strings.HasPrefix(res.Output, "1\n2\n") || !strings.Contains(res.Output, "200\n") {
		t.Fatalf("head/tail not preserved: %q", res.Output)
	}
}

func TestShellManagerKillAndClose(t *testing.T) {
	m := newTestShellManager(t)
	ctx := context.Background()
	if _, err := m.Exec(ctx, "victim", "sleep 60", 200*time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill("victim"); err != nil {
		t.Fatal(err)
	}
	res, err := m.WriteStdin(ctx, "victim", "", time.Second, 0)
	if err == nil {
		t.Fatalf("write to killed session: %+v", res)
	}
	// Recreated on demand.
	res2, err := m.Exec(ctx, "victim", "echo back", 5*time.Second, 0)
	if err != nil || !strings.Contains(res2.Output, "back") {
		t.Fatalf("recreate: res=%+v err=%v", res2, err)
	}
}

// The Shell tool reuses the bash permission pipeline under its own name.
func TestShellToolReusesBashPermissions(t *testing.T) {
	skipShellOnWindows(t)
	m := newTestShellManager(t)
	st := ShellTool(m)

	// Dangerous commands hit the same heuristics as Bash.
	d := st.CheckPermissions(map[string]any{"command": "rm -rf /"}, &permission.Context{})
	if d.Behavior == permission.BehaviorAllow || d.Behavior == permission.BehaviorPassthrough {
		t.Fatalf("dangerous command not intercepted: %+v", d)
	}
	// Rule matching/suggestions are keyed on the Shell tool name.
	sug := st.GenerateSuggestions(map[string]any{"command": "git status"})
	if len(sug) == 0 || sug[0].ToolName != "Shell" {
		t.Fatalf("suggestions = %+v", sug)
	}
	if !st.MatchRule("git status:*", map[string]any{"command": "git status -s"}) {
		t.Fatal("prefix rule did not match")
	}
}

func TestShellToolExecAndResponse(t *testing.T) {
	skipShellOnWindows(t)
	m := newTestShellManager(t)
	st := ShellTool(m)

	resp, err := st.Execute(context.Background(), map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Fatalf("state = %v", resp.State)
	}
	text := resp.Content[0].(message.TextBlock).Text
	if !strings.Contains(text, "hi") || !strings.Contains(text, "\"exit_code\":0") {
		t.Fatalf("response = %s", text)
	}

	resp, err = st.Execute(context.Background(), map[string]any{"command": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("nonzero exit should mark error, state = %v", resp.State)
	}

	// Empty command rejected.
	resp, _ = st.Execute(context.Background(), map[string]any{"command": "  "})
	if resp.State != message.ToolResultError {
		t.Fatal("empty command accepted")
	}
}

func TestShellManagerRejectsNonPOSIX(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("POSIX hosts always have a usable shell")
	}
	if _, err := NewShellManager(); err == nil {
		t.Fatal("expected failure without POSIX shell")
	}
}

// A daemonized grandchild escapes the process-group kill but can hold the
// stdout pipe open; Kill must still return (bounded by the grace period plus
// the forced read-end close), not hang.
func TestShellKillBoundedWithDaemonGrandchild(t *testing.T) {
	m := newTestShellManager(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	ctx := context.Background()
	res, err := m.Exec(ctx, "", `python3 -c "import os,time; os.setsid(); time.sleep(15)" &`, 300*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The backgrounded launcher completes immediately (its exit status only
	// reports the "&" launch); the daemonized python survives it and keeps
	// holding the session's stdout pipe.
	if res.Status != ShellStatusDone {
		t.Fatalf("status = %s, want done: %+v", res.Status, res)
	}
	time.Sleep(500 * time.Millisecond) // let python setsid and detach

	start := time.Now()
	if err := m.Kill(""); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Fatalf("Kill hung for %v with a pipe-holding daemon grandchild", elapsed)
	}
	// The session is dropped and transparently recreated.
	res, err = m.Exec(ctx, "", "echo revived", 5*time.Second, 0)
	if err != nil || !strings.Contains(res.Output, "revived") {
		t.Fatalf("recreate: res=%+v err=%v", res, err)
	}
}

// A single output line far beyond any scan buffer must not kill the session
// or lose the completion record.
func TestShellSessionOverlongLine(t *testing.T) {
	m := newTestShellManager(t)
	res, err := m.Exec(context.Background(), "", `awk 'BEGIN{printf "%1200000s","x"}'`, 10*time.Second, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusDone || res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "<<<TRUNCATED:") {
		t.Fatalf("output not truncated: %q", res.Output[:min(len(res.Output), 200)])
	}
	// Session survives and keeps working.
	res, err = m.Exec(context.Background(), "", "echo still-alive", 5*time.Second, 0)
	if err != nil || !strings.Contains(res.Output, "still-alive") {
		t.Fatalf("session died: res=%+v err=%v", res, err)
	}
}

// Multi-line commands (incl. heredocs with separate terminator lines) are
// rejected with guidance instead of wedging the session.
func TestShellToolRejectsMultiline(t *testing.T) {
	skipShellOnWindows(t)
	m := newTestShellManager(t)
	st := ShellTool(m)
	resp, err := st.Execute(context.Background(), map[string]any{"command": "cat <<EOF\nhello\nEOF"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("state = %v, want error", resp.State)
	}
	if !strings.Contains(resp.Content[0].(message.TextBlock).Text, "single-line") {
		t.Fatalf("error lacks guidance: %s", resp.Content[0])
	}
	// The session is unaffected.
	res, err := m.Exec(context.Background(), "", "echo fine", 5*time.Second, 0)
	if err != nil || !strings.Contains(res.Output, "fine") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A single-line heredoc opener works: feed the body via stdin.
func TestShellSessionSingleLineHeredoc(t *testing.T) {
	m := newTestShellManager(t)
	ctx := context.Background()
	res, err := m.Exec(ctx, "", "cat <<EOF", 300*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusRunning {
		t.Fatalf("status = %s, want running", res.Status)
	}
	res, err = m.WriteStdin(ctx, "", "body-line\nEOF\n", 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ShellStatusDone || res.ExitCode != 0 || !strings.Contains(res.Output, "body-line") {
		t.Fatalf("res = %+v", res)
	}
}
