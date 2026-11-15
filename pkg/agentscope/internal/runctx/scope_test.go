package runctx

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestScopeJoinsChildrenAndRejectsLateWork(t *testing.T) {
	ctx, scope := New(context.Background())
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	first, ok := Start(ctx)
	if !ok {
		t.Fatal("first core rejected")
	}
	second, ok := Start(child)
	if !ok {
		t.Fatal("child core rejected")
	}
	done := make(chan struct{})
	go func() { scope.Close(); close(done) }()
	// Synchronize on sealing, without interpreting a sleep as completion.
	deadline := time.Now().Add(3 * time.Second)
	for {
		release, accepted := Start(ctx)
		if !accepted {
			break
		}
		release()
		if time.Now().After(deadline) {
			t.Fatal("scope was not sealed")
		}
		runtime.Gosched()
	}
	first()
	first() // releases must not undercount live children
	select {
	case <-done:
		t.Fatal("scope finished before child")
	default:
	}
	second()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scope did not finish")
	}
	scope.Close() // repeated close is safe
}

func TestScopeConcurrentStartAndClose(t *testing.T) {
	for range 100 {
		ctx, scope := New(context.Background())
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if done, ok := Start(ctx); ok {
					done()
				}
			}()
		}
		scope.Close()
		wg.Wait()
		if _, ok := Start(ctx); ok {
			t.Fatal("sealed scope accepted work")
		}
	}
}

func TestStartWithoutScope(t *testing.T) {
	release, ok := Start(context.Background())
	if !ok {
		t.Fatal("unmanaged core rejected")
	}
	release()
	release()
}
