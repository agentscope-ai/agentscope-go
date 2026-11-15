package device

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchdog_KickPreventsTriggering(t *testing.T) {
	var triggered atomic.Int32
	wd := NewWatchdog(200*time.Millisecond, func() {
		triggered.Add(1)
	})

	wd.Start()
	defer wd.Stop()

	// Kick several times within the timeout
	for i := 0; i < 5; i++ {
		time.Sleep(80 * time.Millisecond)
		wd.Kick()
	}

	// Wait a bit more (but not past timeout since last kick)
	time.Sleep(100 * time.Millisecond)

	if triggered.Load() != 0 {
		t.Error("watchdog should not have triggered while being kicked")
	}
}

func TestWatchdog_TriggersOnTimeout(t *testing.T) {
	var triggered atomic.Int32
	done := make(chan struct{}, 1)
	wd := NewWatchdog(100*time.Millisecond, func() {
		triggered.Add(1)
		select {
		case done <- struct{}{}:
		default:
		}
	})

	wd.Start()

	// Wait for the callback to actually execute instead of relying on
	// time.Sleep, which races with goroutine scheduling on loaded CI
	// runners (especially Windows with -race overhead).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not trigger within 5 s")
	}

	if triggered.Load() != 1 {
		t.Errorf("expected watchdog to trigger once, got %d", triggered.Load())
	}

	if wd.Running() {
		t.Error("watchdog should not be running after trigger")
	}
}

func TestWatchdog_StopPreventsTriggering(t *testing.T) {
	var triggered atomic.Int32
	wd := NewWatchdog(200*time.Millisecond, func() {
		triggered.Add(1)
	})

	wd.Start()
	time.Sleep(50 * time.Millisecond)
	wd.Stop()

	// Wait past the original timeout
	time.Sleep(300 * time.Millisecond)

	if triggered.Load() != 0 {
		t.Error("watchdog should not trigger after Stop()")
	}
}

func TestWatchdog_DoubleStart(t *testing.T) {
	var triggered atomic.Int32
	wd := NewWatchdog(200*time.Millisecond, func() {
		triggered.Add(1)
	})

	wd.Start()
	wd.Start() // should be no-op
	wd.Stop()

	time.Sleep(300 * time.Millisecond)
	if triggered.Load() != 0 {
		t.Error("double start should not create extra timers")
	}
}
