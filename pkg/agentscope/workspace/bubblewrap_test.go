package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBubblewrapInterfaceCompliance(t *testing.T) {
	var _ Workspace = (*BubblewrapWorkspace)(nil)
}

func TestNewBubblewrapWorkspace_EmptyRootDir(t *testing.T) {
	_, err := NewBubblewrapWorkspace(BubblewrapConfig{})
	if err == nil {
		t.Fatal("expected error for empty root dir")
	}
	if !strings.Contains(err.Error(), "root dir") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewBubblewrapWorkspace_CreatesRootDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "workspace")
	ws, err := NewBubblewrapWorkspace(BubblewrapConfig{RootDir: dir})
	if err != nil {
		t.Fatalf("NewBubblewrapWorkspace: %v", err)
	}

	info, err := os.Stat(ws.BasePath())
	if err != nil {
		t.Fatalf("stat root dir: %v", err)
	}
	if !info.IsDir() {
		t.Error("root dir is not a directory")
	}
}

func TestBubblewrapWorkspace_BasePath(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewBubblewrapWorkspace(BubblewrapConfig{RootDir: dir})
	if err != nil {
		t.Fatalf("NewBubblewrapWorkspace: %v", err)
	}
	if ws.BasePath() != dir {
		t.Errorf("BasePath = %q, want %q", ws.BasePath(), dir)
	}
}

func TestBubblewrapWriteAndReadFile(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	content := []byte("hello bubblewrap world")
	err := ws.WriteFile(ctx, "test.txt", content)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, err := ws.ReadFile(ctx, "test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Errorf("content = %q, want %q", string(data), string(content))
	}
}

func TestBubblewrapWriteFile_CreatesParentDirs(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	err := ws.WriteFile(ctx, "a/b/c/deep.txt", []byte("deep"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, err := ws.ReadFile(ctx, "a/b/c/deep.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "deep" {
		t.Errorf("content = %q, want %q", string(data), "deep")
	}
}

func TestBubblewrapReadFile_NotFound(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	_, err := ws.ReadFile(context.Background(), "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestBubblewrapListFiles(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "alpha.txt", []byte("a"))
	_ = ws.WriteFile(ctx, "beta.go", []byte("b"))
	_ = os.Mkdir(filepath.Join(ws.BasePath(), "subdir"), 0o755)

	files, err := ws.ListFiles(ctx, ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d entries, want 3", len(files))
	}

	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	for _, want := range []string{"alpha.txt", "beta.go", "subdir"} {
		if !names[want] {
			t.Errorf("missing entry %q", want)
		}
	}

	for _, f := range files {
		if f.Name == "subdir" && !f.IsDir {
			t.Error("subdir should have IsDir=true")
		}
		if f.Name == "alpha.txt" && f.IsDir {
			t.Error("alpha.txt should not be a directory")
		}
	}
}

func TestBubblewrapListFiles_SubDir(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "pkg/main.go", []byte("main"))
	_ = ws.WriteFile(ctx, "pkg/util.go", []byte("util"))

	files, err := ws.ListFiles(ctx, "pkg")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d entries, want 2", len(files))
	}
}

func TestBubblewrapListFiles_Empty(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	files, err := ws.ListFiles(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d entries, want 0 for empty dir", len(files))
	}
}

func TestBubblewrapListFiles_NotFound(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	_, err := ws.ListFiles(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestBubblewrapRemoveFile(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "doomed.txt", []byte("bye"))

	err := ws.RemoveFile(ctx, "doomed.txt")
	if err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}

	_, err = ws.ReadFile(ctx, "doomed.txt")
	if err == nil {
		t.Fatal("expected error after removal")
	}
}

func TestBubblewrapRemoveFile_NotFound(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	err := ws.RemoveFile(context.Background(), "ghost.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestBubblewrapRemoveFile_RejectsDirectory(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	_ = os.Mkdir(filepath.Join(ws.BasePath(), "mydir"), 0o755)

	err := ws.RemoveFile(context.Background(), "mydir")
	if err == nil {
		t.Fatal("expected error when removing directory")
	}
	if !strings.Contains(err.Error(), "cannot remove directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBubblewrapPathTraversal_Relative(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	_, err := ws.ReadFile(ctx, "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "escapes workspace") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBubblewrapPathTraversal_Write(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	err := ws.WriteFile(context.Background(), "../../escape.txt", []byte("bad"))
	if err == nil {
		t.Fatal("expected error for path traversal on write")
	}
}

func TestBubblewrapPathTraversal_AbsoluteOutside(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	// Use an absolute path clearly outside the workspace root.
	outsidePath := filepath.Join(os.TempDir(), "not_in_workspace.txt")
	_, err := ws.ReadFile(context.Background(), outsidePath)
	if err == nil {
		t.Fatal("expected error for absolute path outside workspace")
	}
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBubblewrapPathTraversal_AbsoluteInside(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	ctx := context.Background()

	_ = ws.WriteFile(ctx, "inside.txt", []byte("safe"))

	absPath := filepath.Join(ws.BasePath(), "inside.txt")
	data, err := ws.ReadFile(ctx, absPath)
	if err != nil {
		t.Fatalf("ReadFile with absolute inside path: %v", err)
	}
	if string(data) != "safe" {
		t.Errorf("content = %q, want %q", string(data), "safe")
	}
}

func TestBubblewrapPathTraversal_ListDir(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	_, err := ws.ListFiles(context.Background(), "../../..")
	if err == nil {
		t.Fatal("expected error for path traversal in ListFiles")
	}
}

func TestBubblewrapPathTraversal_RemoveFile(t *testing.T) {
	ws := mustBubblewrapWorkspace(t)
	err := ws.RemoveFile(context.Background(), "../escape.txt")
	if err == nil {
		t.Fatal("expected error for path traversal in RemoveFile")
	}
}

func TestBubblewrapArgs_NoNetwork(t *testing.T) {
	ws := &BubblewrapWorkspace{
		rootDir:      "/tmp/test",
		allowNetwork: false,
	}
	args := ws.bwrapArgs()

	hasUnshareNet := false
	for _, arg := range args {
		if arg == "--unshare-net" {
			hasUnshareNet = true
		}
	}
	if !hasUnshareNet {
		t.Error("expected --unshare-net when AllowNetwork=false")
	}
}

func TestBubblewrapArgs_WithNetwork(t *testing.T) {
	ws := &BubblewrapWorkspace{
		rootDir:      "/tmp/test",
		allowNetwork: true,
	}
	args := ws.bwrapArgs()

	for _, arg := range args {
		if arg == "--unshare-net" {
			t.Error("should not have --unshare-net when AllowNetwork=true")
		}
	}
}

func TestBubblewrapArgs_ContainsBindMount(t *testing.T) {
	ws := &BubblewrapWorkspace{
		rootDir:      "/my/workspace",
		allowNetwork: false,
	}
	args := ws.bwrapArgs()

	foundBind := false
	for i, arg := range args {
		if arg == "--bind" && i+2 < len(args) {
			if args[i+1] == "/my/workspace" && args[i+2] == "/" {
				foundBind = true
			}
		}
	}
	if !foundBind {
		t.Errorf("expected --bind /my/workspace /, got args: %v", args)
	}
}

func TestBubblewrapArgs_HasProcAndDev(t *testing.T) {
	ws := &BubblewrapWorkspace{rootDir: "/tmp/ws", allowNetwork: false}
	args := ws.bwrapArgs()

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--proc /proc") {
		t.Error("expected --proc /proc in bwrap args")
	}
	if !strings.Contains(joined, "--dev /dev") {
		t.Error("expected --dev /dev in bwrap args")
	}
}

func mustBubblewrapWorkspace(t *testing.T) *BubblewrapWorkspace {
	t.Helper()
	ws, err := NewBubblewrapWorkspace(BubblewrapConfig{RootDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBubblewrapWorkspace: %v", err)
	}
	return ws
}
