package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewLocalWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	if ws.BasePath() != dir {
		t.Errorf("BasePath = %q, want %q", ws.BasePath(), dir)
	}
}

func TestNewLocalWorkspaceCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	ws, err := NewLocalWorkspace(LocalConfig{BasePath: dir})
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	info, err := os.Stat(ws.BasePath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestNewLocalWorkspaceEmptyPath(t *testing.T) {
	_, err := NewLocalWorkspace(LocalConfig{})
	if err == nil {
		t.Fatal("expected error for empty base path")
	}
}

func TestWriteAndReadFile(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	err := ws.WriteFile(ctx, "hello.txt", []byte("hello world"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, err := ws.ReadFile(ctx, "hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("ReadFile = %q, want %q", string(data), "hello world")
	}
}

func TestWriteFileCreatesParent(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	err := ws.WriteFile(ctx, "sub/dir/file.txt", []byte("nested"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, err := ws.ReadFile(ctx, "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "nested" {
		t.Errorf("ReadFile = %q, want %q", string(data), "nested")
	}
}

func TestReadFileNotFound(t *testing.T) {
	ws := mustWorkspace(t)
	_, err := ws.ReadFile(context.Background(), "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestListFiles(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "a.txt", []byte("a"))
	_ = ws.WriteFile(ctx, "b.txt", []byte("b"))
	_ = os.Mkdir(filepath.Join(ws.BasePath(), "subdir"), 0o755)

	files, err := ws.ListFiles(ctx, ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	if len(files) != 3 {
		t.Fatalf("ListFiles = %d entries, want 3", len(files))
	}

	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	for _, want := range []string{"a.txt", "b.txt", "subdir"} {
		if !names[want] {
			t.Errorf("missing entry %q", want)
		}
	}
}

func TestRemoveFile(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "tmp.txt", []byte("temp"))

	err := ws.RemoveFile(ctx, "tmp.txt")
	if err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}

	_, err = ws.ReadFile(ctx, "tmp.txt")
	if err == nil {
		t.Fatal("expected error after removal")
	}
}

func TestRemoveFileRejectsDir(t *testing.T) {
	ws := mustWorkspace(t)
	_ = os.Mkdir(filepath.Join(ws.BasePath(), "mydir"), 0o755)

	err := ws.RemoveFile(context.Background(), "mydir")
	if err == nil {
		t.Fatal("expected error when removing directory")
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	_, err := ws.ReadFile(ctx, "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}

	err = ws.WriteFile(ctx, "../../escape.txt", []byte("bad"))
	if err == nil {
		t.Fatal("expected error for path traversal on write")
	}
}

func TestAbsolutePathOutsideBlocked(t *testing.T) {
	ws := mustWorkspace(t)
	_, err := ws.ReadFile(context.Background(), "/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute path outside workspace")
	}
}

func TestAbsolutePathInsideAllowed(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "inside.txt", []byte("ok"))

	absPath := filepath.Join(ws.BasePath(), "inside.txt")
	data, err := ws.ReadFile(ctx, absPath)
	if err != nil {
		t.Fatalf("ReadFile with absolute inside path: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("ReadFile = %q, want %q", string(data), "ok")
	}
}

func TestExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	ws := mustWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "test.txt", []byte("content"))

	result, err := ws.Execute(ctx, "ls test.txt")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout == "" {
		t.Error("expected non-empty stdout")
	}
}

func TestExecuteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	ws := mustWorkspace(t)

	result, err := ws.Execute(context.Background(), "false")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
}

func TestExecuteWorkingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	ws := mustWorkspace(t)
	ctx := context.Background()

	result, err := ws.Execute(ctx, "pwd")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := filepath.Clean(result.Stdout[:len(result.Stdout)-1]); got != ws.BasePath() {
		t.Errorf("pwd = %q, want %q", got, ws.BasePath())
	}
}

func TestOffloadContent(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := context.Background()

	path, err := ws.OffloadContent(ctx, "large content here", "summary.txt")
	if err != nil {
		t.Fatalf("OffloadContent: %v", err)
	}

	if path != filepath.Join("_offloaded", "summary.txt") {
		t.Errorf("path = %q, want %q", path, filepath.Join("_offloaded", "summary.txt"))
	}

	data, err := ws.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "large content here" {
		t.Errorf("content = %q, want %q", string(data), "large content here")
	}
}

func TestOffloadContentAutoName(t *testing.T) {
	ws := mustWorkspace(t)
	path, err := ws.OffloadContent(context.Background(), "auto", "")
	if err != nil {
		t.Fatalf("OffloadContent: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}

func TestContextInjection(t *testing.T) {
	ws := mustWorkspace(t)
	ctx := WithWorkspace(context.Background(), ws)

	got := GetWorkspace(ctx)
	if got != ws {
		t.Error("GetWorkspace returned wrong workspace")
	}

	if GetWorkspace(context.Background()) != nil {
		t.Error("GetWorkspace on bare context should return nil")
	}
}

func mustWorkspace(t *testing.T) *LocalWorkspace {
	t.Helper()
	ws, err := NewLocalWorkspace(LocalConfig{BasePath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	return ws
}

func TestSiblingPrefixEscapeBlocked(t *testing.T) {
	// A bare prefix check admits siblings: base <tmp>/ws1 must not admit
	// <tmp>/ws123/... Craft the pair explicitly.
	parent := t.TempDir()
	base := filepath.Join(parent, "ws1")
	sibling := filepath.Join(parent, "ws123")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret.txt"), []byte("sibling secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := NewLocalWorkspace(LocalConfig{BasePath: base})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := ws.ReadFile(ctx, "../ws123/secret.txt"); err == nil {
		t.Fatal("sibling escape via relative path must be blocked")
	}
	if _, err := ws.ReadFile(ctx, filepath.Join(sibling, "secret.txt")); err == nil {
		t.Fatal("sibling escape via absolute path must be blocked")
	}
	if err := ws.WriteFile(ctx, "../ws123/pwned.txt", []byte("x")); err == nil {
		t.Fatal("sibling write escape must be blocked")
	}
	if _, err := os.Stat(filepath.Join(sibling, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatal("no file may land in the sibling")
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(parent, "ws")

	ws, err := NewLocalWorkspace(LocalConfig{BasePath: wsDir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// File symlink pointing out.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(wsDir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ws.ReadFile(ctx, "link"); err == nil {
		t.Fatal("read through an escaping symlink must be blocked")
	}

	// Directory symlink pointing out.
	if err := os.Symlink(outside, filepath.Join(wsDir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ListFiles(ctx, "dirlink"); err == nil {
		t.Fatal("listing an escaping symlinked dir must be blocked")
	}
	if err := ws.WriteFile(ctx, "dirlink/pwned.txt", []byte("x")); err == nil {
		t.Fatal("write through an escaping symlinked dir must be blocked")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatal("no file may land outside via symlink")
	}

	// A symlink staying inside the workspace remains usable.
	if err := os.WriteFile(filepath.Join(wsDir, "real.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(wsDir, "real.txt"), filepath.Join(wsDir, "inner")); err != nil {
		t.Fatal(err)
	}
	data, err := ws.ReadFile(ctx, "inner")
	if err != nil || string(data) != "inside" {
		t.Errorf("contained symlink must stay usable, got %q %v", data, err)
	}
}
