package tool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLocalBackend_ExecShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	b := &LocalBackend{}
	result, err := b.ExecShell(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
}

func TestLocalBackend_ExecShell_Failure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	b := &LocalBackend{}
	result, err := b.ExecShell(context.Background(), "false", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code for 'false'")
	}
}

func TestLocalBackend_ExecShell_WorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	dir := t.TempDir()
	b := &LocalBackend{WorkDir: dir}
	result, err := b.ExecShell(context.Background(), "pwd", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := filepath.Clean(result.Stdout[:len(result.Stdout)-1]) // trim newline
	if got != dir {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestLocalBackend_ExecShell_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	b := &LocalBackend{}
	result, err := b.ExecShell(context.Background(), "sleep 10", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for timeout", result.ExitCode)
	}
}

func TestLocalBackend_ReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{}
	ctx := context.Background()

	path := filepath.Join(dir, "test.txt")
	err := b.WriteFile(ctx, path, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := b.ReadFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("ReadFile = %q, want %q", string(data), "hello world")
	}
}

func TestLocalBackend_WriteFile_CreatesParent(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{}
	path := filepath.Join(dir, "sub", "dir", "file.txt")
	err := b.WriteFile(context.Background(), path, []byte("nested"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := b.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "nested" {
		t.Errorf("ReadFile = %q", string(data))
	}
}

func TestLocalBackend_FileExists(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{}
	ctx := context.Background()

	path := filepath.Join(dir, "exists.txt")
	_ = os.WriteFile(path, []byte("x"), 0o644)

	exists, err := b.FileExists(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected file to exist")
	}

	exists, err = b.FileExists(ctx, filepath.Join(dir, "nope.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected file to not exist")
	}
}

func TestLocalBackend_ListDir(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{}

	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	_ = os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	names, err := b.ListDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("ListDir returned %d entries, want 3", len(names))
	}
}

func TestLocalBackend_Glob(t *testing.T) {
	dir := t.TempDir()
	b := &LocalBackend{}

	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.go"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c"), 0o644)

	matches, err := b.Glob(context.Background(), filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("Glob returned %d matches, want 2", len(matches))
	}
}

func TestBackendContext(t *testing.T) {
	b := &LocalBackend{WorkDir: "/tmp"}
	ctx := WithBackend(context.Background(), b)

	got := GetBackend(ctx)
	if got != b {
		t.Error("GetBackend returned wrong backend")
	}
}

func TestGetBackend_Default(t *testing.T) {
	got := GetBackend(context.Background())
	if got == nil {
		t.Fatal("GetBackend should return default LocalBackend, got nil")
	}
	if _, ok := got.(*LocalBackend); !ok {
		t.Errorf("default backend type = %T, want *LocalBackend", got)
	}
}
