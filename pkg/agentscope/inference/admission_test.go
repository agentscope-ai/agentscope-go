package inference

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testDeployment(t *testing.T, active, queued int, work int64) *Deployment {
	t.Helper()
	c := NewController(NewLedger(32))
	if err := c.RegisterPool(PoolConfig{ID: "shared", MaxActive: active, MaxQueued: queued, MaxQueuedWork: work}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&DeploymentConfig{PoolID: "shared", Descriptor: Descriptor{ID: "fixture", Provider: "fixture", API: "openai-chat", Model: "fixture", Endpoint: "http://localhost"}, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return d
}
func waitQueued(t *testing.T, d *Deployment, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for d.Stats().Queued != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue never reached %d: %+v", n, d.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}
func TestAdmissionFIFOAndWorkBound(t *testing.T) {
	d := testDeployment(t, 1, 2, 3)
	release, err := d.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan func(), 1)
	second := make(chan func(), 1)
	go func() { r, _ := d.acquire(context.Background(), 2); first <- r }()
	waitQueued(t, d, 1)
	if _, err := d.acquire(context.Background(), 2); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queued work must reject: %v", err)
	}
	go func() { r, _ := d.acquire(context.Background(), 1); second <- r }()
	waitQueued(t, d, 2)
	release()
	r1 := <-first
	select {
	case <-second:
		t.Fatal("FIFO permit escaped before first release")
	default:
	}
	r1()
	r2 := <-second
	r2()
	r2()
	if s := d.Stats(); s.Active != 0 || s.Queued != 0 || s.QueuedWork != 0 {
		t.Fatalf("leaked admission: %+v", s)
	}
}
func TestAdmissionCancelDeadlineAndShutdown(t *testing.T) {
	d := testDeployment(t, 1, 1, 5)
	release, _ := d.acquire(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := d.acquire(ctx, 1); done <- err }()
	waitQueued(t, d, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := d.acquire(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	go func() { _, err := d.acquire(context.Background(), 1); done <- err }()
	waitQueued(t, d, 1)
	d.close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	release()
	if _, err := d.acquire(context.Background(), 1); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
