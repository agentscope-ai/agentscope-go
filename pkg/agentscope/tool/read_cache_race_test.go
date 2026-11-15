package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// GetCacheWithMtime used to return a pointer INTO its own entries slice.
// removeAt shifts elements within the shared backing array, so a caller still
// reading that pointer raced with any concurrent Remove — and Write/Edit call
// Remove (upstream #2092). Concurrent tool batches make Read+Write on the same
// file a realistic pairing, so this must be race-free under -race.
func TestReadCacheConcurrentGetAndRemoveIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	const files = 8
	paths := make([]string, files)
	for i := range paths {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte("line1\nline2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
	}

	rc := NewReadCache(files, 1024*1024)
	for _, p := range paths {
		rc.CacheFile(p, []string{"line1", "line2"})
	}

	// Fixed iteration counts rather than a wall-clock window: the race detector
	// only reports unsynchronized access it actually OBSERVES, so a duration
	// bound lets a slow CI machine execute too few operations to trip it and
	// pass for the wrong reason.
	const rounds = 400

	var wg sync.WaitGroup

	// Readers hold on to the returned entry and keep reading its Lines after
	// the lock is released — that is what used to alias the backing array.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for _, p := range paths {
					if e := rc.GetCache(p); e != nil {
						for _, line := range e.Lines {
							_ = len(line)
						}
						_ = e.FilePath + e.UpdatedAt.String()
					}
				}
			}
		}()
	}

	// Writers invalidate entries the way Write/Edit do.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for _, p := range paths {
					rc.Remove(p)
					rc.CacheFile(p, []string{"line1", "line2"})
				}
			}
		}()
	}

	// Eviction churns the backing array from the front.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			rc.CacheFile(filepath.Join(dir, "churn.txt"), []string{"x"})
		}
	}()

	wg.Wait()
}

// The returned entry must be a snapshot: mutating the cache afterwards cannot
// change what an earlier caller received.
func TestReadCacheGetReturnsSnapshot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "snap.txt")
	if err := os.WriteFile(p, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := NewReadCache(4, 1024)
	rc.CacheFile(p, []string{"a", "b"})

	got := rc.GetCache(p)
	if got == nil {
		t.Fatal("expected a cache hit")
	}
	if got.FilePath != p {
		t.Errorf("FilePath = %q", got.FilePath)
	}

	// Evict everything else so removeAt shifts the backing array around the
	// entry the caller is still holding.
	for i := 0; i < 10; i++ {
		rc.CacheFile(filepath.Join(dir, fmt.Sprintf("other%d.txt", i)), []string{"y"})
	}
	rc.Remove(p)

	if got.FilePath != p {
		t.Errorf("the returned entry changed after eviction: FilePath = %q", got.FilePath)
	}
	if len(got.Lines) != 2 || got.Lines[0] != "a" {
		t.Errorf("the returned entry's lines changed after eviction: %v", got.Lines)
	}
}

// A cache hit must refresh recency (upstream #1811) without handing out the
// internal pointer.
func TestReadCacheHitRefreshesRecency(t *testing.T) {
	dir := t.TempDir()
	hot := filepath.Join(dir, "hot.txt")
	if err := os.WriteFile(hot, []byte("hot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := NewReadCache(2, 1024*1024)
	rc.CacheFile(hot, []string{"hot"})
	for i := 0; i < 2; i++ {
		cold := filepath.Join(dir, fmt.Sprintf("cold%d.txt", i))
		if err := os.WriteFile(cold, []byte("cold\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rc.CacheFile(cold, []string{"cold"})
		if rc.GetCache(hot) == nil {
			t.Fatalf("hot entry evicted after %d cold inserts", i+1)
		}
	}
}

// Freshness must be judged by the caller-supplied mtime: a stale entry is
// dropped, a matching one is served.
func TestReadCacheWithExplicitMtime(t *testing.T) {
	rc := NewReadCache(4, 1024)
	cached := time.Unix(1700000000, 0)
	rc.CacheFileWithMtime("rel/path.txt", []string{"v1"}, &cached)

	same := cached
	if got := rc.GetCacheWithMtime("rel/path.txt", &same); got == nil {
		t.Fatal("a matching backend mtime must serve the cached copy")
	}
	newer := cached.Add(time.Second)
	if got := rc.GetCacheWithMtime("rel/path.txt", &newer); got != nil {
		t.Errorf("a newer backend mtime must invalidate the entry, got %v", got.Lines)
	}
	// Invalidating must have dropped it rather than leaving a stale entry.
	if rc.HasBeenRead("rel/path.txt") {
		t.Error("the stale entry should have been removed")
	}
}

// countingBackend is a Backend whose files live "off-host". It counts the
// round-trips a read costs so a test can prove the cache actually saves work
// instead of adding a stat per call.
type countingBackend struct {
	files     map[string][]byte
	mtime     time.Time
	statCalls int
	readCalls int
}

func (b *countingBackend) ExecShell(context.Context, string, time.Duration) (*ExecResult, error) {
	return &ExecResult{}, nil
}
func (b *countingBackend) WriteFile(_ context.Context, path string, data []byte) error {
	b.files[path] = data
	return nil
}
func (b *countingBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	b.readCalls++
	data, ok := b.files[path]
	if !ok {
		return nil, fmt.Errorf("no such file: %s", path)
	}
	return data, nil
}
func (b *countingBackend) FileExists(_ context.Context, path string) (bool, error) {
	_, ok := b.files[path]
	return ok, nil
}
func (b *countingBackend) ListDir(context.Context, string) ([]string, error) { return nil, nil }
func (b *countingBackend) Glob(context.Context, string) ([]string, error)    { return nil, nil }

// statterBackend adds the optional BackendStatter capability.
type statterBackend struct{ countingBackend }

func (b *statterBackend) StatFile(_ context.Context, path string) (BackendFileInfo, error) {
	b.statCalls++
	data, ok := b.files[path]
	if !ok {
		return BackendFileInfo{}, fmt.Errorf("no such file: %s", path)
	}
	return BackendFileInfo{ModTime: b.mtime, Size: int64(len(data))}, nil
}

// A backend that cannot vouch for freshness must not leave the cache holding an
// entry keyed on a meaningless host stat; the read path skips caching instead.
func TestBackendReadWithoutStatDoesNotCache(t *testing.T) {
	b := &countingBackend{files: map[string][]byte{"a.txt": []byte("hello\n")}}
	rc := NewReadCache(4, 1024)
	ctx := WithBackend(WithReadCache(context.Background(), rc), b)

	rt := &readTool{}
	for i := 0; i < 3; i++ {
		resp, err := rt.Execute(ctx, map[string]any{"file_path": "a.txt"})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if resp.State != message.ToolResultSuccess {
			t.Fatalf("state = %v", resp.State)
		}
	}
	if rc.Len() != 0 {
		t.Errorf("cache holds %d entries; without a backend mtime nothing may be cached", rc.Len())
	}
	if b.readCalls != 3 {
		t.Errorf("read calls = %d, want 3 (every read must hit the backend)", b.readCalls)
	}
}

// A Statter backend must pay for exactly one stat per cache probe — never one
// per read just to build a key nobody validates — and a cache hit must avoid
// re-reading the file.
func TestBackendReadStatsOnlyWhenAnEntryExists(t *testing.T) {
	b := &statterBackend{countingBackend: countingBackend{
		files: map[string][]byte{"a.txt": []byte("hello\nworld\n")},
		mtime: time.Unix(1700000000, 0),
	}}
	rc := NewReadCache(4, 1024)
	ctx := WithBackend(WithReadCache(context.Background(), rc), b)

	rt := &readTool{}
	first, err := rt.Execute(ctx, map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first.State != message.ToolResultSuccess {
		t.Fatalf("first read state = %v", first.State)
	}
	if b.statCalls != 1 {
		t.Errorf("first read made %d stat calls, want 1 (to cache with a real mtime)", b.statCalls)
	}
	if b.readCalls != 1 {
		t.Errorf("first read made %d read calls, want 1", b.readCalls)
	}
	if rc.Len() != 1 {
		t.Fatalf("cache entries = %d, want 1", rc.Len())
	}

	second, err := rt.Execute(ctx, map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second.State != message.ToolResultSuccess {
		t.Fatalf("second read state = %v", second.State)
	}
	if b.statCalls != 2 {
		t.Errorf("total stat calls = %d, want 2 (one probe for the cache hit)", b.statCalls)
	}
	if b.readCalls != 1 {
		t.Errorf("total read calls = %d, want 1 (the second read must come from the cache)", b.readCalls)
	}
	firstText := first.Content[0].(message.TextBlock).Text
	secondText := second.Content[0].(message.TextBlock).Text
	if firstText != secondText {
		t.Errorf("cached read differs:\n%s\n---\n%s", firstText, secondText)
	}

	// A backend-side change must invalidate the cached copy.
	b.files["a.txt"] = []byte("changed\n")
	b.mtime = time.Unix(1700000999, 0)
	third, err := rt.Execute(ctx, map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatalf("third read: %v", err)
	}
	if got := third.Content[0].(message.TextBlock).Text; got != "1\tchanged" {
		t.Errorf("third read = %q, want the changed content", got)
	}
	if b.readCalls != 2 {
		t.Errorf("total read calls = %d, want 2 (a changed file must be re-read)", b.readCalls)
	}
}
