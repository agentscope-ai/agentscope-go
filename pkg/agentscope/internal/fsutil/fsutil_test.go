package fsutil

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteFileAtomic_WritesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "state.json")

	if err := WriteFileAtomic(p, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "v1" {
		t.Fatalf("got %q, want v1", b)
	}

	if err := WriteFileAtomic(p, []byte("v2-longer"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "v2-longer" {
		t.Fatalf("got %q, want v2-longer", b)
	}

	// No leftover temp files in the directory.
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}

// TestWriteFileAtomic_ConcurrentNoTornReads writes concurrently and asserts a
// reader never observes a partial file (rename is atomic).
func TestWriteFileAtomic_ConcurrentNoTornReads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	full := make([]byte, 4096)
	for i := range full {
		full[i] = 'A'
	}
	if err := WriteFileAtomic(p, full, 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = WriteFileAtomic(p, full, 0o644)
		}
	}()

	for i := 0; i < 500; i++ {
		b, err := os.ReadFile(p)
		if err == nil && len(b) != 0 && len(b) != len(full) {
			t.Fatalf("torn read: got %d bytes, want 0 or %d", len(b), len(full))
		}
	}
	close(stop)
	wg.Wait()
}
