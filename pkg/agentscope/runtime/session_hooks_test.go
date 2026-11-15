package runtime

import (
	"errors"
	"sync"
	"testing"
)

func TestSessionHookManagerFiresSubscribedEvents(t *testing.T) {
	m := NewSessionHookManager()
	var received []SessionHookEvent
	var mu sync.Mutex

	m.Register(&FuncHook{
		Fn: func(e SessionHookEvent, _ any) error {
			mu.Lock()
			received = append(received, e)
			mu.Unlock()
			return nil
		},
		Evts: []SessionHookEvent{HookPreTurn, HookPostTurn},
	})

	m.Fire(HookPreTurn, nil)
	m.Fire(HookSessionStart, nil) // not subscribed
	m.Fire(HookPostTurn, nil)

	if len(received) != 2 {
		t.Fatalf("got %d events, want 2", len(received))
	}
	if received[0] != HookPreTurn {
		t.Errorf("received[0] = %v, want HookPreTurn", received[0])
	}
	if received[1] != HookPostTurn {
		t.Errorf("received[1] = %v, want HookPostTurn", received[1])
	}
}

func TestSessionHookManagerMultipleHooks(t *testing.T) {
	m := NewSessionHookManager()
	var order []string

	m.Register(&FuncHook{
		Fn:   func(_ SessionHookEvent, _ any) error { order = append(order, "first"); return nil },
		Evts: []SessionHookEvent{HookSessionStart},
	})
	m.Register(&FuncHook{
		Fn:   func(_ SessionHookEvent, _ any) error { order = append(order, "second"); return nil },
		Evts: []SessionHookEvent{HookSessionStart},
	})

	m.Fire(HookSessionStart, nil)

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("order = %v, want [first, second]", order)
	}
}

func TestSessionHookManagerErrorStopsChain(t *testing.T) {
	m := NewSessionHookManager()
	errTest := errors.New("hook error")

	m.Register(&FuncHook{
		Fn:   func(_ SessionHookEvent, _ any) error { return errTest },
		Evts: []SessionHookEvent{HookPreTurn},
	})
	m.Register(&FuncHook{
		Fn:   func(_ SessionHookEvent, _ any) error { t.Fatal("should not be called"); return nil },
		Evts: []SessionHookEvent{HookPreTurn},
	})

	err := m.Fire(HookPreTurn, nil)
	if err != errTest {
		t.Errorf("error = %v, want %v", err, errTest)
	}
}

func TestSessionHookManagerPayload(t *testing.T) {
	m := NewSessionHookManager()
	var gotPayload any

	m.Register(&FuncHook{
		Fn:   func(_ SessionHookEvent, p any) error { gotPayload = p; return nil },
		Evts: []SessionHookEvent{HookBudgetWarning},
	})

	m.Fire(HookBudgetWarning, "90% used")

	if gotPayload != "90% used" {
		t.Errorf("payload = %v, want %q", gotPayload, "90% used")
	}
}

func TestSessionHookManagerNoHooks(t *testing.T) {
	m := NewSessionHookManager()
	if err := m.Fire(HookPreTurn, nil); err != nil {
		t.Errorf("Fire with no hooks should not error: %v", err)
	}
}

func TestSessionHookEventString(t *testing.T) {
	if HookPreTurn.String() != "pre_turn" {
		t.Errorf("HookPreTurn.String() = %q, want %q", HookPreTurn.String(), "pre_turn")
	}
	if HookSessionStart.String() != "session_start" {
		t.Errorf("HookSessionStart.String() = %q, want %q", HookSessionStart.String(), "session_start")
	}
}

func TestSessionHookManagerHookCount(t *testing.T) {
	m := NewSessionHookManager()
	if m.HookCount() != 0 {
		t.Errorf("HookCount = %d, want 0", m.HookCount())
	}
	m.Register(&FuncHook{Fn: func(_ SessionHookEvent, _ any) error { return nil }, Evts: []SessionHookEvent{HookPreTurn}})
	if m.HookCount() != 1 {
		t.Errorf("HookCount = %d, want 1", m.HookCount())
	}
}
