package app

import (
	"sync"
	"testing"
)

// TestWakeupUnregisterConcurrentNoPanic hammers Wakeup against Unregister/
// Register from multiple goroutines. Before the fix, Wakeup loaded the
// channel under the lock but sent after releasing it, so a concurrent
// Unregister could close the channel mid-send and panic with
// "send on closed channel" (upstream #2476 class). Run under -race.
func TestWakeupUnregisterConcurrentNoPanic(t *testing.T) {
	d := NewWakeupDispatcher()
	const workers = 4
	const iterations = 2000

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch w % 3 {
				case 0:
					d.Wakeup("session")
				case 1:
					d.Unregister("session")
				case 2:
					d.Register("session")
				}
			}
		}(w)
	}
	wg.Wait()
	d.Unregister("session")
}

func TestWakeupDeliversOnceAndNeverBlocks(t *testing.T) {
	d := NewWakeupDispatcher()
	ch := d.Register("s1")
	d.Wakeup("s1")
	d.Wakeup("s1") // buffered 1: second signal coalesces, must not block
	select {
	case <-ch:
	default:
		t.Fatal("expected a wakeup signal")
	}
	select {
	case <-ch:
		t.Fatal("expected coalesced single signal")
	default:
	}
	// Wakeup after unregister is a no-op, not a panic.
	d.Unregister("s1")
	d.Wakeup("s1")
}
