package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadCache_BasicCRUD(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(file1, []byte("hello"), 0o644)

	rc := NewReadCache(10, 100*1024)
	rc.CacheFile(file1, []string{"hello"})

	if rc.Len() != 1 {
		t.Fatalf("cache len = %d, want 1", rc.Len())
	}

	entry := rc.GetCache(file1)
	if entry == nil {
		t.Fatal("expected cache hit")
	}
	if len(entry.Lines) != 1 || entry.Lines[0] != "hello" {
		t.Errorf("lines = %v, want [hello]", entry.Lines)
	}
	if entry.FilePath != file1 {
		t.Errorf("FilePath = %s, want %s", entry.FilePath, file1)
	}
}

func TestReadCache_MissOnUnknownFile(t *testing.T) {
	rc := NewReadCache(10, 100*1024)
	if entry := rc.GetCache("/nonexistent"); entry != nil {
		t.Error("expected nil for unknown file")
	}
}

func TestReadCache_StalenessEviction(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(file1, []byte("v1"), 0o644)

	rc := NewReadCache(10, 100*1024)
	rc.CacheFile(file1, []string{"v1"})

	// Modify file to change mtime
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(file1, []byte("v2"), 0o644)

	entry := rc.GetCache(file1)
	if entry != nil {
		t.Error("expected nil (stale entry should be evicted)")
	}
	if rc.Len() != 0 {
		t.Errorf("stale entry should be removed, len = %d", rc.Len())
	}
}

func TestReadCache_EvictionByCount(t *testing.T) {
	dir := t.TempDir()
	rc := NewReadCache(3, 1024*1024)

	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, string(rune('a'+i))+".txt")
		_ = os.WriteFile(path, []byte("x"), 0o644)
		rc.CacheFile(path, []string{"x"})
	}

	if rc.Len() != 3 {
		t.Errorf("cache len = %d, want 3 (max)", rc.Len())
	}

	// Oldest files (a, b) should have been evicted
	if entry := rc.GetCache(filepath.Join(dir, "a.txt")); entry != nil {
		t.Error("a.txt should have been evicted")
	}
	if entry := rc.GetCache(filepath.Join(dir, "b.txt")); entry != nil {
		t.Error("b.txt should have been evicted")
	}
}

func TestReadCache_EvictionBySize(t *testing.T) {
	dir := t.TempDir()
	rc := NewReadCache(100, 20)

	file1 := filepath.Join(dir, "big1.txt")
	_ = os.WriteFile(file1, []byte("12345678"), 0o644)
	rc.CacheFile(file1, []string{"12345678"})

	file2 := filepath.Join(dir, "big2.txt")
	_ = os.WriteFile(file2, []byte("abcdefgh"), 0o644)
	rc.CacheFile(file2, []string{"abcdefgh"})

	file3 := filepath.Join(dir, "big3.txt")
	_ = os.WriteFile(file3, []byte("xyzwvuts"), 0o644)
	rc.CacheFile(file3, []string{"xyzwvuts"})

	if rc.Len() > 2 {
		t.Errorf("cache len = %d, should evict to fit size limit", rc.Len())
	}
}

func TestReadCache_DeduplicationOnReinsert(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "dup.txt")
	_ = os.WriteFile(file1, []byte("v1"), 0o644)

	rc := NewReadCache(10, 100*1024)
	rc.CacheFile(file1, []string{"v1"})
	rc.CacheFile(file1, []string{"v1"})

	if rc.Len() != 1 {
		t.Errorf("cache len = %d, want 1 (dedup)", rc.Len())
	}
}

func TestReadCache_HasBeenRead(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "has.txt")
	_ = os.WriteFile(file1, []byte("content"), 0o644)

	rc := NewReadCache(10, 100*1024)

	if rc.HasBeenRead(file1) {
		t.Error("should not have been read yet")
	}

	rc.CacheFile(file1, []string{"content"})

	if !rc.HasBeenRead(file1) {
		t.Error("should have been read")
	}
}

func TestReadCache_CleanFileCache(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 3)
	for i := range files {
		files[i] = filepath.Join(dir, string(rune('a'+i))+".txt")
		_ = os.WriteFile(files[i], []byte("x"), 0o644)
	}

	rc := NewReadCache(10, 100*1024)
	for _, f := range files {
		rc.CacheFile(f, []string{"x"})
	}

	keep := map[string]bool{files[1]: true}
	rc.CleanFileCache(keep)

	if rc.Len() != 1 {
		t.Errorf("cache len = %d, want 1", rc.Len())
	}
	if entry := rc.GetCache(files[1]); entry == nil {
		t.Error("kept file should still be cached")
	}
}

func TestReadCache_CleanAll(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "clean.txt")
	_ = os.WriteFile(file1, []byte("x"), 0o644)

	rc := NewReadCache(10, 100*1024)
	rc.CacheFile(file1, []string{"x"})

	rc.CleanFileCache(nil)

	if rc.Len() != 0 {
		t.Errorf("cache should be empty, len = %d", rc.Len())
	}
}

func TestReadCache_DeletedFileEviction(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "del.txt")
	_ = os.WriteFile(file1, []byte("x"), 0o644)

	rc := NewReadCache(10, 100*1024)
	rc.CacheFile(file1, []string{"x"})

	os.Remove(file1)

	if entry := rc.GetCache(file1); entry != nil {
		t.Error("deleted file should return nil")
	}
	if rc.Len() != 0 {
		t.Error("deleted file entry should be removed")
	}
}

func TestReadCache_ContextInjection(t *testing.T) {
	rc := NewReadCache(10, 100*1024)
	ctx := WithReadCache(context.Background(), rc)

	got := GetReadCache(ctx)
	if got != rc {
		t.Error("should retrieve the same ReadCache from context")
	}

	if GetReadCache(context.Background()) != nil {
		t.Error("empty context should return nil")
	}
}

func TestReadCache_DefaultLimits(t *testing.T) {
	rc := NewReadCache(0, 0)
	if rc.maxCacheFiles != DefaultMaxCacheFiles {
		t.Errorf("maxCacheFiles = %d, want %d", rc.maxCacheFiles, DefaultMaxCacheFiles)
	}
	if rc.maxCacheBytes != DefaultMaxCacheBytes {
		t.Errorf("maxCacheBytes = %d, want %d", rc.maxCacheBytes, DefaultMaxCacheBytes)
	}
}

func TestReadCache_HitRefreshesRecency(t *testing.T) {
	// Upstream #1811: a cache hit must refresh recency, otherwise a
	// repeatedly-read hot file is evicted first (FIFO by insertion).
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("content-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b, c := mk("a.txt"), mk("b.txt"), mk("c.txt")

	rc := NewReadCache(2, 1<<20)
	rc.CacheFile(a, []string{"a"})
	rc.CacheFile(b, []string{"b"})

	// Touch a so it becomes most recent: order must now be [b, a].
	if rc.GetCache(a) == nil {
		t.Fatal("expected cache hit for a")
	}

	// Caching c must evict b (least recent), not a.
	rc.CacheFile(c, []string{"c"})
	if rc.GetCache(a) == nil {
		t.Error("hot file a was evicted despite a more recent access (upstream #1811)")
	}
	if rc.GetCache(c) == nil {
		t.Error("expected cache hit for c")
	}
	if rc.GetCache(b) != nil {
		t.Error("expected b to be evicted as the least recently used")
	}
}
