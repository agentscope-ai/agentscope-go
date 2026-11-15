package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// fakeWorkspace records the commands StatFile builds and returns canned output.
type fakeWorkspace struct {
	base    string
	cmds    []string
	stdout  string
	stderr  string
	exit    int
	execErr error
	readErr error
}

func (w *fakeWorkspace) WriteFile(context.Context, string, []byte) error { return nil }
func (w *fakeWorkspace) ReadFile(context.Context, string) ([]byte, error) {
	if w.readErr != nil {
		return nil, w.readErr
	}
	return []byte(w.stdout), nil
}
func (w *fakeWorkspace) ListFiles(context.Context, string) ([]FileInfo, error) { return nil, nil }
func (w *fakeWorkspace) RemoveFile(context.Context, string) error              { return nil }
func (w *fakeWorkspace) Execute(_ context.Context, command string) (*ExecResult, error) {
	w.cmds = append(w.cmds, command)
	if w.execErr != nil {
		return nil, w.execErr
	}
	return &ExecResult{Stdout: w.stdout, Stderr: w.stderr, ExitCode: w.exit}, nil
}
func (w *fakeWorkspace) BasePath() string { return w.base }

// StatFile shells out to `stat`, whose flag syntax differs between GNU
// (Linux containers) and BSD/busybox (macOS, Alpine). Both forms must be
// attempted in one command so the same code works on either.
func TestStatFileTriesBothStatFlavour(t *testing.T) {
	w := &fakeWorkspace{base: "/ws", stdout: "1757318400\n"}
	b := NewToolBackend(w)

	info, err := b.StatFile(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if len(w.cmds) != 1 {
		t.Fatalf("execute calls = %d, want 1", len(w.cmds))
	}
	cmd := w.cmds[0]
	if !strings.Contains(cmd, "stat -c %Y") {
		t.Errorf("GNU form missing from %q", cmd)
	}
	if !strings.Contains(cmd, "stat -f %m") {
		t.Errorf("BSD form missing from %q", cmd)
	}
	if !strings.Contains(cmd, "||") {
		t.Errorf("the two forms must be alternatives: %q", cmd)
	}
	if want := time.Unix(1757318400, 0); !info.ModTime.Equal(want) {
		t.Errorf("ModTime = %v, want %v", info.ModTime, want)
	}
}

// The path is interpolated into a shell command, so quoting is the only thing
// standing between a filename and command injection.
func TestStatFileQuotesHostilePaths(t *testing.T) {
	// The fake workspace does not implement ExecPathResolver, so the command
	// carries the caller-relative path — the form K8s, E2B, OpenSandbox and
	// bubblewrap need. See TestStatFileUsesBackendExecPath for the absolute
	// form.
	cases := []struct {
		path string
		want string // the exact quoted form that must appear
	}{
		{"plain.txt", `'plain.txt'`},
		{"with space.txt", `'with space.txt'`},
		{"back`tick.txt", `'back` + "`" + `tick.txt'`},
		{"dollar$(id).txt", `'dollar$(id).txt'`},
		{"semi;rm.txt", `'semi;rm.txt'`},
		{"star*.txt", `'star*.txt'`},
		{"../escaped.txt", ``}, // rejected before it reaches the shell
		// POSIX single-quote escape: close, escaped quote, reopen.
		{"quo'te.txt", `'quo'\''te.txt'`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			w := &fakeWorkspace{base: "/ws", stdout: "1700000000"}
			b := NewToolBackend(w)
			_, err := b.StatFile(context.Background(), tc.path)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("stat %q should have been rejected", tc.path)
				}
				if len(w.cmds) != 0 {
					t.Errorf("a rejected path reached the shell: %q", w.cmds[0])
				}
				return
			}
			if err != nil {
				t.Fatalf("stat %q: %v", tc.path, err)
			}
			cmd := w.cmds[0]
			if !strings.Contains(cmd, tc.want) {
				t.Errorf("command %q does not contain %s", cmd, tc.want)
			}
			// Nothing dangerous may appear outside single quotes.
			if strings.Contains(cmd, "$(id)") && !strings.Contains(cmd, `'dollar$(id).txt'`) {
				t.Errorf("command substitution escaped quoting: %q", cmd)
			}
		})
	}
}

// StatFile has no workspace resolver to lean on (ReadFile/WriteFile do), so it
// must confine the path itself. Otherwise it is an oracle for the mtime and
// existence of any file the execution user can reach.
func TestStatFileRejectsEscapingPaths(t *testing.T) {
	cases := []struct {
		base      string
		path      string
		wantEmpty bool // rejected as an empty path rather than as an escape
	}{
		{"/ws", "../../etc/passwd", false},
		{"/ws", "../secret", false},
		{"/ws", "/etc/passwd", false},
		// Separator-aware: a bare prefix match would admit these siblings.
		{"/ws", "/ws2/sibling", false},
		{"/ws", "/wsx/also-sibling", false},
		{"", "../escape", false},
		{"", "..", false},
		{"/", "", true},
		{"/", "   ", true},
		{"/", ".", true},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("case%d_%s", i, tc.base), func(t *testing.T) {
			w := &fakeWorkspace{base: tc.base, stdout: "1700000000"}
			b := NewToolBackend(w)
			_, err := b.StatFile(context.Background(), tc.path)
			if err == nil {
				t.Fatalf("StatFile(%q) with base %q must be rejected", tc.path, tc.base)
			}
			if len(w.cmds) != 0 {
				t.Errorf("a rejected path must not reach the shell, but ran: %q", w.cmds[0])
			}
			if tc.wantEmpty {
				if !strings.Contains(err.Error(), "empty path") {
					t.Errorf("err = %v, want an empty-path rejection", err)
				}
				return
			}
			if !errors.Is(err, ErrPathEscape) {
				t.Errorf("err = %v, want ErrPathEscape", err)
			}
		})
	}
}

func TestStatFileAcceptsContainedPaths(t *testing.T) {
	cases := []struct{ base, path string }{
		{"/ws", "notes.txt"},
		{"/ws", "sub/dir/notes.txt"},
		{"/ws", "/ws/notes.txt"},
		{"/ws", "/ws/sub/notes.txt"},
		{"/ws", "./notes.txt"},
		{"/", "/anywhere/notes.txt"},
		{"", "notes.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.base+"|"+tc.path, func(t *testing.T) {
			w := &fakeWorkspace{base: tc.base, stdout: "1700000000"}
			b := NewToolBackend(w)
			if _, err := b.StatFile(context.Background(), tc.path); err != nil {
				t.Errorf("StatFile(%q) with base %q: %v", tc.path, tc.base, err)
			}
		})
	}
}

func TestStatFileSurfacesBackendErrors(t *testing.T) {
	t.Run("exec error", func(t *testing.T) {
		w := &fakeWorkspace{base: "/ws", execErr: errors.New("container gone")}
		if _, err := NewToolBackend(w).StatFile(context.Background(), "a.txt"); err == nil {
			t.Error("expected the execute error to propagate")
		}
	})
	t.Run("non-zero exit", func(t *testing.T) {
		w := &fakeWorkspace{base: "/ws", exit: 1, stderr: "No such file"}
		_, err := NewToolBackend(w).StatFile(context.Background(), "a.txt")
		if err == nil {
			t.Fatal("expected an error for a non-zero exit")
		}
		if !strings.Contains(err.Error(), "No such file") {
			t.Errorf("error should carry stderr: %v", err)
		}
	})
	t.Run("unparsable stdout", func(t *testing.T) {
		w := &fakeWorkspace{base: "/ws", stdout: "not-a-timestamp"}
		if _, err := NewToolBackend(w).StatFile(context.Background(), "a.txt"); err == nil {
			t.Error("expected a parse error")
		}
	})
}

// End-to-end against a real LocalWorkspace: this exercises the actual shell and
// whichever stat flavor the platform provides.
func TestStatFileAgainstLocalWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		// StatFile builds a POSIX command (`stat -c %Y '...' || stat -f %m`),
		// and LocalWorkspace.Execute runs it through PowerShell/Cmd on
		// Windows: `stat` is not on PATH, `2>/dev/null` resolves to
		// C:\dev\null, and `||` is a syntax error in Windows PowerShell 5.1.
		// The workspace backends that matter here are POSIX containers.
		t.Skip("requires a POSIX shell")
	}
	if os.Getenv("CI_NO_SHELL") != "" {
		t.Skip("shell execution disabled")
	}
	dir := t.TempDir()
	ws, err := NewLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	b := NewToolBackend(ws)

	target := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(target, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-2 * time.Second)

	info, err := b.StatFile(context.Background(), "hello.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.ModTime.Before(before) {
		t.Errorf("ModTime %v is older than the file creation window", info.ModTime)
	}
	if info.ModTime.After(time.Now().Add(2 * time.Second)) {
		t.Errorf("ModTime %v is in the future", info.ModTime)
	}

	// A path that escapes the jail must be refused even though the workspace
	// exists and the shell works.
	if _, err := b.StatFile(context.Background(), "../../etc/hosts"); err == nil {
		t.Error("escaping path must be rejected")
	}
}

// resolvingWorkspace is a fake for the Docker/Daytona/AppleContainer shape:
// Execute resolves the caller's path against the workspace base, the way
// ReadFile does.
type resolvingWorkspace struct {
	fakeWorkspace
}

func (w *resolvingWorkspace) ExecPath(callerPath string) string {
	if strings.HasPrefix(callerPath, "/") {
		return callerPath
	}
	return strings.TrimSuffix(w.base, "/") + "/" + callerPath
}

// StatFile must measure the SAME file ReadFile reads. The spelling that names
// it is backend-specific, so it is delegated: Docker runs `docker exec` with no
// -w (a relative path resolves against the image WORKDIR, not the container
// workDir ReadFile uses), while K8s/E2B/OpenSandbox pass the caller's path
// straight through and bubblewrap binds its HOST root to "/". Getting this
// wrong is worse than a wasted exec: a stat on a different file supplies a
// freshness key for the wrong file and the read cache then serves stale content
// as fresh.
func TestStatFileUsesBackendExecPath(t *testing.T) {
	t.Run("resolver backend gets the absolute path", func(t *testing.T) {
		cases := []struct{ base, path, want string }{
			{"/workspace", "notes.md", "/workspace/notes.md"},
			{"/workspace", "sub/notes.md", "/workspace/sub/notes.md"},
			{"/workspace", "./notes.md", "/workspace/notes.md"},
			{"/workspace", "/workspace/notes.md", "/workspace/notes.md"},
			{"/var/lib/sandbox/agent-1", "a.txt", "/var/lib/sandbox/agent-1/a.txt"},
		}
		for _, tc := range cases {
			w := &resolvingWorkspace{fakeWorkspace{base: tc.base, stdout: "1700000000"}}
			if _, err := NewToolBackend(w).StatFile(context.Background(), tc.path); err != nil {
				t.Fatalf("stat %q: %v", tc.path, err)
			}
			if !strings.Contains(w.cmds[0], "'"+tc.want+"'") {
				t.Errorf("command %q does not stat %q", w.cmds[0], tc.want)
			}
			if strings.Contains(w.cmds[0], "//") {
				t.Errorf("command %q contains a doubled separator", w.cmds[0])
			}
		}
	})

	t.Run("plain backend keeps the relative path", func(t *testing.T) {
		// The K8s / E2B / OpenSandbox / bubblewrap shape: an absolute
		// BasePath-joined path would name a file that does not exist in the
		// execution namespace.
		for _, base := range []string{"/workspace", "/tmp/host-root", ""} {
			w := &fakeWorkspace{base: base, stdout: "1700000000"}
			if _, err := NewToolBackend(w).StatFile(context.Background(), "a.txt"); err != nil {
				t.Fatalf("stat with base %q: %v", base, err)
			}
			if !strings.Contains(w.cmds[0], "'a.txt'") {
				t.Errorf("base %q: command %q should stat the relative path", base, w.cmds[0])
			}
			if strings.Contains(w.cmds[0], base+"/a.txt") {
				t.Errorf("base %q: command %q leaked the host-side absolute path", base, w.cmds[0])
			}
		}
	})

	t.Run("the jail still applies to a resolver backend", func(t *testing.T) {
		w := &resolvingWorkspace{fakeWorkspace{base: "/workspace", stdout: "1700000000"}}
		if _, err := NewToolBackend(w).StatFile(context.Background(), "../../etc/passwd"); err == nil {
			t.Fatal("an escaping path must be rejected before it reaches the shell")
		}
		if len(w.cmds) != 0 {
			t.Errorf("a rejected path reached the shell: %q", w.cmds[0])
		}
	})
}

// The three backends whose Execute resolves against a base must advertise it,
// and the ones that pass the caller's path through must NOT — an incorrect
// ExecPath is worse than none, because it silently points the stat at a
// different file than ReadFile read.
func TestExecPathResolverImplementations(t *testing.T) {
	resolvers := map[string]any{
		"Docker":         &DockerWorkspace{workDir: "/workspace"},
		"Daytona":        &DaytonaWorkspace{},
		"AppleContainer": &AppleContainerWorkspace{},
	}
	for name, ws := range resolvers {
		r, ok := ws.(ExecPathResolver)
		if !ok {
			t.Errorf("%s must implement ExecPathResolver", name)
			continue
		}
		if got := r.ExecPath("a.txt"); !strings.HasPrefix(got, "/") || !strings.HasSuffix(got, "/a.txt") {
			t.Errorf("%s.ExecPath(a.txt) = %q, want an absolute in-sandbox path", name, got)
		}
		if got := r.ExecPath("/abs/a.txt"); got != "/abs/a.txt" {
			t.Errorf("%s.ExecPath(/abs/a.txt) = %q, want it passed through", name, got)
		}
	}

	// These must stay non-resolvers: their ReadFile/Execute use the caller's
	// path as given (or, for bubblewrap, a host base that is "/" inside).
	nonResolvers := map[string]any{
		"K8s":         &K8sWorkspace{},
		"E2B":         &E2BWorkspace{},
		"OpenSandbox": &OpenSandboxWorkspace{},
		"Bubblewrap":  &BubblewrapWorkspace{rootDir: "/tmp/host-root"},
	}
	for name, ws := range nonResolvers {
		if _, ok := ws.(ExecPathResolver); ok {
			t.Errorf("%s must NOT implement ExecPathResolver: its Execute uses the "+
				"caller-relative path, so an absolute rewrite would stat the wrong file", name)
		}
	}
}

// The invariant that actually matters: ExecPath must agree with the path
// ReadFile resolves to, for the backends that implement it.
func TestExecPathMatchesReadFileResolution(t *testing.T) {
	d := &DockerWorkspace{workDir: "/workspace"}
	if got, want := d.ExecPath("notes.md"), d.resolvePath("notes.md"); got != want {
		t.Errorf("Docker ExecPath = %q, resolvePath = %q; they must agree", got, want)
	}
	a := &AppleContainerWorkspace{}
	if got, want := a.ExecPath("notes.md"), a.resolvePath("notes.md"); got != want {
		t.Errorf("AppleContainer ExecPath = %q, resolvePath = %q; they must agree", got, want)
	}
	dy := &DaytonaWorkspace{}
	if got, want := dy.ExecPath("notes.md"), dy.resolvePath("notes.md"); got != want {
		t.Errorf("Daytona ExecPath = %q, resolvePath = %q; they must agree", got, want)
	}
}

// The backend must satisfy the optional interface the read cache looks for.
func TestToolBackendImplementsBackendStatter(t *testing.T) {
	var _ tool.BackendStatter = NewToolBackend(&fakeWorkspace{base: "/ws"})
}
