package tool

import (
	"context"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// stubStatterBackend is a Backend whose files live "off-host": it implements
// BackendStatter with a controllable mtime and never touches the real FS.
type stubStatterBackend struct {
	mtime   time.Time
	statErr error
	files   map[string][]byte
}

func (b *stubStatterBackend) ExecShell(_ context.Context, _ string, _ time.Duration) (*ExecResult, error) {
	return &ExecResult{}, nil
}
func (b *stubStatterBackend) WriteFile(_ context.Context, path string, data []byte) error {
	b.files[path] = data
	return nil
}
func (b *stubStatterBackend) FileExists(_ context.Context, path string) (bool, error) {
	_, ok := b.files[path]
	return ok, nil
}
func (b *stubStatterBackend) ListDir(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (b *stubStatterBackend) Glob(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (b *stubStatterBackend) StatFile(_ context.Context, path string) (BackendFileInfo, error) {
	if b.statErr != nil {
		return BackendFileInfo{}, b.statErr
	}
	data, ok := b.files[path]
	if !ok {
		return BackendFileInfo{}, context.DeadlineExceeded // stand-in for "not found"
	}
	return BackendFileInfo{ModTime: b.mtime, Size: int64(len(data))}, nil
}

func okState() message.ToolResultState { return message.ToolResultSuccess }

func (b *stubStatterBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := b.files[path]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return data, nil
}

// Upstream #2092: Read through a workspace backend must populate the cache
// using the BACKEND's mtime — the old host os.Stat of a workspace-relative
// path always failed, so Read->Edit never worked in workspaces.
func TestReadToolBackendPopulatesCache(t *testing.T) {
	mt := time.Now().Truncate(time.Second)
	b := &stubStatterBackend{
		mtime: mt,
		files: map[string][]byte{"notes.md": []byte("line one\nline two\n")},
	}
	rc := NewReadCache(10, 1024*1024)
	ctx := WithReadCache(WithBackend(context.Background(), b), rc)

	rt := &readTool{}
	resp, err := rt.Execute(ctx, map[string]any{"file_path": "notes.md"})
	if err != nil || resp.State != okState() {
		t.Fatalf("read: err=%v state=%v", err, resp.State)
	}
	if !rc.HasBeenRead("notes.md") {
		t.Fatal("backend read must populate the cache (Read->Edit guard)")
	}
	// Freshness now validates against the backend mtime.
	if rc.GetCacheWithMtime("notes.md", &mt) == nil {
		t.Error("cache entry should validate against the same backend mtime")
	}
	later := mt.Add(2 * time.Second)
	if rc.GetCacheWithMtime("notes.md", &later) != nil {
		t.Error("cache entry must be invalidated when the backend mtime moves")
	}
	if rc.HasBeenRead("notes.md") {
		t.Error("stale entry should have been removed")
	}
}

// After a backend write the cached copy must be dropped so the next
// Read/Edit sees fresh content (host stat cannot see backend files).
func TestWriteBackendInvalidatesCache(t *testing.T) {
	mt := time.Now().Truncate(time.Second)
	b := &stubStatterBackend{
		mtime: mt,
		files: map[string][]byte{"f.txt": []byte("old\n")},
	}
	rc := NewReadCache(10, 1024*1024)
	ctx := WithReadCache(WithBackend(context.Background(), b), rc)

	rt := &readTool{}
	if _, err := rt.Execute(ctx, map[string]any{"file_path": "f.txt"}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !rc.HasBeenRead("f.txt") {
		t.Fatal("precondition: cache populated")
	}

	wt := &writeTool{}
	if _, err := wt.Execute(ctx, map[string]any{"file_path": "f.txt", "content": "new\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rc.HasBeenRead("f.txt") {
		t.Error("backend write must invalidate the cached copy")
	}
}

func TestReadCacheMtimeVariants(t *testing.T) {
	rc := NewReadCache(10, 1024*1024)
	mt := time.Unix(1700000000, 0)

	// A path that does not exist on the host still caches with explicit mtime.
	rc.CacheFileWithMtime("/nonexistent/backend/path.txt", []string{"a", "b"}, &mt)
	if rc.GetCacheWithMtime("/nonexistent/backend/path.txt", &mt) == nil {
		t.Error("explicit-mtime cache should validate without host stat")
	}
	// Nil mtime falls back to host stat → miss for a nonexistent path.
	if rc.GetCache("/nonexistent/backend/path.txt") != nil {
		t.Error("host-stat fallback must miss for nonexistent paths")
	}
	// Remove works.
	rc.CacheFileWithMtime("/nonexistent/backend/path.txt", []string{"a"}, &mt)
	rc.Remove("/nonexistent/backend/path.txt")
	if rc.GetCacheWithMtime("/nonexistent/backend/path.txt", &mt) != nil {
		t.Error("Remove did not drop the entry")
	}
}
