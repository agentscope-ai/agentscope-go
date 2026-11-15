package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type mockRunnable struct {
	fn func(ctx context.Context) error
}

func (m *mockRunnable) Run(ctx context.Context) error { return m.fn(ctx) }

func TestRun_NormalExit(t *testing.T) {
	r := &mockRunnable{fn: func(ctx context.Context) error { return nil }}
	err := Run(context.Background(), r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_ErrorPropagation(t *testing.T) {
	want := errors.New("boom")
	r := &mockRunnable{fn: func(ctx context.Context) error { return want }}
	err := Run(context.Background(), r)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &mockRunnable{fn: func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_PIDFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "test.pid")

	r := &mockRunnable{fn: func(ctx context.Context) error {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			t.Errorf("PID file not found during run: %v", err)
			return nil
		}
		if len(data) == 0 {
			t.Error("PID file is empty")
		}
		return nil
	}}

	err := Run(context.Background(), r, WithPIDFile(pidPath))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// PID file should be cleaned up
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should be removed after Run exits")
	}
}

func TestRun_PanicRecovery(t *testing.T) {
	var count int32

	r := &mockRunnable{fn: func(ctx context.Context) error {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			panic("test panic")
		}
		// Second call — exit cleanly by checking context
		return ctx.Err()
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := Run(ctx, r, WithPanicRecovery())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c := atomic.LoadInt32(&count); c < 2 {
		t.Fatalf("expected at least 2 iterations (panic + retry), got %d", c)
	}
}

func TestRun_OnShutdown(t *testing.T) {
	called := false
	r := &mockRunnable{fn: func(ctx context.Context) error { return nil }}
	err := Run(context.Background(), r, WithOnShutdown(func() { called = true }))
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("OnShutdown callback was not called")
	}
}

func TestRun_HealthProbe(t *testing.T) {
	r := &mockRunnable{fn: func(ctx context.Context) error {
		// Give the health server a moment to start
		time.Sleep(50 * time.Millisecond)
		return nil
	}}

	err := Run(context.Background(), r, WithHealthProbe("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
