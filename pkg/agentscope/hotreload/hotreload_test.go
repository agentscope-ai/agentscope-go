package hotreload

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper: write a temp file and return its path.
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWatcherDetectsFileCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.json")

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	var called atomic.Int32
	err := w.Watch(path, func(e ChangeEvent, data []byte) error {
		called.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// File does not exist yet - ForceCheck should not fire.
	errs := w.ForceCheck()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if called.Load() != 0 {
		t.Fatal("handler should not fire for non-existent file")
	}

	// Create the file.
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0644); err != nil {
		t.Fatal(err)
	}

	errs = w.ForceCheck()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if called.Load() != 1 {
		t.Fatalf("expected handler to fire once, got %d", called.Load())
	}
}

func TestWatcherDetectsFileModification(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "cfg.json", `{"v":1}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	var lastData atomic.Value
	err := w.Watch(path, func(e ChangeEvent, data []byte) error {
		lastData.Store(string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// First check - file just registered, state is snapshotted at Watch time,
	// so no change expected.
	w.ForceCheck()
	if lastData.Load() != nil {
		t.Fatal("should not fire on initial snapshot")
	}

	// Modify the file - ensure mtime differs.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"v":2}`), 0644); err != nil {
		t.Fatal(err)
	}

	w.ForceCheck()
	got, ok := lastData.Load().(string)
	if !ok || got != `{"v":2}` {
		t.Fatalf("expected modified content, got %q", got)
	}
}

func TestWatcherDoesNotFireOnUnchangedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "stable.json", `{"x":1}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	var count atomic.Int32
	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		count.Add(1)
		return nil
	})

	// Check multiple times - should never fire because file hasn't changed
	// since Watch() snapshotted it.
	for range 5 {
		w.ForceCheck()
	}

	if count.Load() != 0 {
		t.Fatalf("handler fired %d times on unchanged file", count.Load())
	}
}

func TestMultipleHandlersForSameFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "multi.json", `{"a":1}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	var c1, c2 atomic.Int32
	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		c1.Add(1)
		return nil
	})
	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		c2.Add(1)
		return nil
	})

	// Modify.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"a":2}`), 0644); err != nil {
		t.Fatal(err)
	}

	w.ForceCheck()

	if c1.Load() != 1 || c2.Load() != 1 {
		t.Fatalf("expected both handlers fired once, got c1=%d c2=%d", c1.Load(), c2.Load())
	}
}

func TestUnwatchStopsNotifications(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "unsub.json", `{}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	var count atomic.Int32
	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		count.Add(1)
		return nil
	})

	w.Unwatch(path)

	// Modify file.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"new":true}`), 0644); err != nil {
		t.Fatal(err)
	}

	w.ForceCheck()

	if count.Load() != 0 {
		t.Fatal("handler fired after Unwatch")
	}
}

func TestForceCheckSynchronous(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "sync.json", `{"v":0}`)

	w := NewWatcher(WatcherConfig{PollInterval: time.Hour}) // very long poll

	var mu sync.Mutex
	var history []string
	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		mu.Lock()
		history = append(history, string(data))
		mu.Unlock()
		return nil
	})

	// Two rapid modifications + force checks.
	for i := 1; i <= 3; i++ {
		time.Sleep(50 * time.Millisecond)
		content := fmt.Sprintf(`{"v":%d}`, i)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		errs := w.ForceCheck()
		if len(errs) != 0 {
			t.Fatalf("errors on iteration %d: %v", i, errs)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(history) != 3 {
		t.Fatalf("expected 3 handler calls, got %d: %v", len(history), history)
	}
}

func TestStartStop(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "loop.json", `{"x":0}`)

	w := NewWatcher(WatcherConfig{PollInterval: 30 * time.Millisecond})

	var count atomic.Int32
	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		count.Add(1)
		return nil
	})

	ctx := context.Background()
	go w.Start(ctx)

	// Modify file and wait for poll to pick it up.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"x":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	w.Stop()

	if count.Load() < 1 {
		t.Fatal("expected at least 1 handler call from polling loop")
	}
}

// --- Reloader[T] tests ---

type testConfig struct {
	Name    string `json:"name"`
	Workers int    `json:"workers"`
}

func TestReloaderLoadsInitialValue(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "init.json", `{"name":"alpha","workers":4}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	r, err := NewReloader[testConfig](w, path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := r.Get()
	if cfg.Name != "alpha" || cfg.Workers != 4 {
		t.Fatalf("unexpected initial config: %+v", cfg)
	}
}

func TestReloaderUpdatesOnFileChange(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "reload.json", `{"name":"v1","workers":2}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	r, err := NewReloader[testConfig](w, path)
	if err != nil {
		t.Fatal(err)
	}

	// Modify file.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"name":"v2","workers":8}`), 0644); err != nil {
		t.Fatal(err)
	}

	w.ForceCheck()

	cfg := r.Get()
	if cfg.Name != "v2" || cfg.Workers != 8 {
		t.Fatalf("expected updated config, got %+v", cfg)
	}
}

func TestReloaderGetReturnsLatest(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "latest.json", `{"name":"a","workers":1}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	r, err := NewReloader[testConfig](w, path)
	if err != nil {
		t.Fatal(err)
	}

	// Several updates.
	for i := range 3 {
		time.Sleep(50 * time.Millisecond)
		content := fmt.Sprintf(`{"name":"iter%d","workers":%d}`, i, i+10)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		w.ForceCheck()
	}

	cfg := r.Get()
	if cfg.Name != "iter2" || cfg.Workers != 12 {
		t.Fatalf("expected latest config, got %+v", cfg)
	}
}

func TestOnChangeCallbackReceivesOldAndNew(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "cb.json", `{"name":"old","workers":1}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	var mu sync.Mutex
	var oldName, newName string
	r, err := NewReloader[testConfig](w, path,
		WithOnChange[testConfig](func(old, new_ *testConfig) {
			mu.Lock()
			oldName = old.Name
			newName = new_.Name
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = r // keep reference

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"name":"new","workers":2}`), 0644); err != nil {
		t.Fatal(err)
	}

	w.ForceCheck()

	mu.Lock()
	defer mu.Unlock()
	if oldName != "old" || newName != "new" {
		t.Fatalf("expected old='old' new='new', got old=%q new=%q", oldName, newName)
	}
}

func TestCustomParser(t *testing.T) {
	dir := t.TempDir()
	// Simple "key=value" format, one per line.
	path := writeTempFile(t, dir, "custom.cfg", "name=beta\nworkers=6\n")

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	kvParser := func(data []byte) (*testConfig, error) {
		cfg := &testConfig{}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid line: %s", line)
			}
			switch parts[0] {
			case "name":
				cfg.Name = parts[1]
			case "workers":
				var n int
				if _, err := fmt.Sscanf(parts[1], "%d", &n); err != nil {
					return nil, err
				}
				cfg.Workers = n
			}
		}
		return cfg, nil
	}

	r, err := NewReloader[testConfig](w, path, WithParser[testConfig](kvParser))
	if err != nil {
		t.Fatal(err)
	}

	cfg := r.Get()
	if cfg.Name != "beta" || cfg.Workers != 6 {
		t.Fatalf("unexpected config from custom parser: %+v", cfg)
	}

	// Update with same format.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte("name=gamma\nworkers=12\n"), 0644); err != nil {
		t.Fatal(err)
	}
	w.ForceCheck()

	cfg = r.Get()
	if cfg.Name != "gamma" || cfg.Workers != 12 {
		t.Fatalf("expected updated custom config, got %+v", cfg)
	}
}

func TestHandlerErrorReported(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "err.json", `{}`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	_ = w.Watch(path, func(e ChangeEvent, data []byte) error {
		return fmt.Errorf("boom")
	})

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"x":1}`), 0644); err != nil {
		t.Fatal(err)
	}

	errs := w.ForceCheck()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "boom") {
		t.Fatalf("expected boom error, got %v", errs)
	}
}

func TestWatchValidation(t *testing.T) {
	w := NewWatcher(WatcherConfig{})

	if err := w.Watch("", func(ChangeEvent, []byte) error { return nil }); err == nil {
		t.Fatal("expected error for empty path")
	}
	if err := w.Watch("/tmp/x", nil); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestReloaderWithInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeTempFile(t, dir, "bad.json", `not json`)

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	_, err := NewReloader[testConfig](w, path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReloaderMissingFile(t *testing.T) {
	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	_, err := NewReloader[testConfig](w, "/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDefaultPollInterval(t *testing.T) {
	w := NewWatcher(WatcherConfig{})
	if w.cfg.PollInterval != 2*time.Second {
		t.Fatalf("expected default 2s, got %v", w.cfg.PollInterval)
	}
}

// Verify Reloader properly marshals a more complex type.
func TestReloaderComplexType(t *testing.T) {
	type nested struct {
		Endpoints []string          `json:"endpoints"`
		Meta      map[string]string `json:"meta"`
	}

	dir := t.TempDir()
	data, _ := json.Marshal(nested{
		Endpoints: []string{"http://a", "http://b"},
		Meta:      map[string]string{"env": "test"},
	})
	path := writeTempFile(t, dir, "complex.json", string(data))

	w := NewWatcher(WatcherConfig{PollInterval: 50 * time.Millisecond})

	r, err := NewReloader[nested](w, path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := r.Get()
	if len(cfg.Endpoints) != 2 || cfg.Meta["env"] != "test" {
		t.Fatalf("unexpected complex config: %+v", cfg)
	}
}
