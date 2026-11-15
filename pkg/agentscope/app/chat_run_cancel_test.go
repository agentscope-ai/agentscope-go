package app

import (
	"context"
	"sync"
	"testing"
)

func TestChatRunCancellationAndLeaseIdentity(t *testing.T) {
	r := NewChatRunRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := r.acquire("s", cancel)
	if first == nil {
		t.Fatal("run not admitted")
	}
	if r.acquire("s", cancel) != nil {
		t.Fatal("overlapping run admitted")
	}
	r.cancel("s")
	r.cancel("s")
	if ctx.Err() == nil {
		t.Fatal("run was not canceled")
	}
	if !r.IsRunning("s") {
		t.Fatal("cancel prematurely released ownership")
	}
	r.release("s", first)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	second := r.acquire("s", cancel2)
	r.release("s", first)
	if !r.IsRunning("s") || ctx2.Err() != nil {
		t.Fatal("stale cleanup affected replacement")
	}
	r.release("s", second)
	r.cancel("s")
	r.cancel("unknown")
	if !r.TryAcquire("legacy") {
		t.Fatal("legacy admission failed")
	}
	r.cancel("legacy") // legacy entries have no cancel handle
	r.Release("legacy")
}

func TestChatRunCancelCallbackOutsideLock(t *testing.T) {
	r := NewChatRunRegistry()
	var first *chatRun
	first = r.acquire("s", func() { r.release("s", first) })
	done := make(chan struct{})
	go func() { r.cancel("s"); close(done) }()
	awaitChatSignal(t, done, "reentrant cancel")
}

func TestChatRunCancelRacesWithCompletion(t *testing.T) {
	r := NewChatRunRegistry()
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		first := r.acquire("s", cancel)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.cancel("s") }()
		go func() { defer wg.Done(); r.release("s", first) }()
		wg.Wait()
		cancel()
		if ctx.Err() == nil || r.IsRunning("s") {
			t.Fatal("completed run retained")
		}
	}
}

func TestBackgroundCancelConcurrentAndReentrant(t *testing.T) {
	m := NewBackgroundTaskManager()
	d := NewCancelDispatcher(m)
	var calls int
	m.Track("s", func() { _ = m.List(); calls++ })
	m.Track("other", func() { t.Error("other session canceled") })
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); d.Cancel("s"); _ = m.List() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	awaitChatSignal(t, done, "background cancellation")
	if calls != 1 {
		t.Fatalf("cancel callback calls = %d, want 1", calls)
	}
}
