package runtime

import (
	"sync"
	"testing"
	"time"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

func TestBudgetTrackerTurnLimit(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxTurns: 3})

	for i := 0; i < 3; i++ {
		if err := bt.AddTurn(); err != nil {
			t.Fatalf("AddTurn %d: %v", i+1, err)
		}
	}
	if err := bt.AddTurn(); err == nil {
		t.Fatal("expected error on 4th turn")
	}
	if bt.TurnsUsed() != 3 {
		t.Errorf("TurnsUsed = %d, want 3", bt.TurnsUsed())
	}
}

func TestBudgetTrackerTokenLimit(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxTokens: 1000})

	if err := bt.AddTokens(500); err != nil {
		t.Fatal(err)
	}
	if err := bt.AddTokens(600); err == nil {
		t.Fatal("expected error when exceeding token limit")
	}
	if bt.TokensUsed() != 1100 {
		t.Errorf("TokensUsed = %d, want 1100", bt.TokensUsed())
	}
}

func TestBudgetTrackerConcurrency(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxConcurrency: 2})

	if err := bt.AcquireAgent(); err != nil {
		t.Fatal(err)
	}
	if err := bt.AcquireAgent(); err != nil {
		t.Fatal(err)
	}
	if err := bt.AcquireAgent(); err == nil {
		t.Fatal("expected error on 3rd agent")
	}
	if bt.ActiveAgents() != 2 {
		t.Errorf("ActiveAgents = %d, want 2", bt.ActiveAgents())
	}

	bt.ReleaseAgent()
	if bt.ActiveAgents() != 1 {
		t.Errorf("ActiveAgents after release = %d, want 1", bt.ActiveAgents())
	}
	if err := bt.AcquireAgent(); err != nil {
		t.Fatal("should succeed after release:", err)
	}
}

func TestBudgetTrackerDuration(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxDuration: 50 * time.Millisecond})

	if err := bt.AddTurn(); err != nil {
		t.Fatal(err)
	}
	if bt.Exceeded() {
		t.Fatal("should not be exceeded immediately")
	}
	time.Sleep(60 * time.Millisecond)
	if !bt.Exceeded() {
		t.Fatal("should be exceeded after duration")
	}
}

func TestBudgetTrackerUnlimited(t *testing.T) {
	bt := NewBudgetTracker(Budget{})

	for i := 0; i < 100; i++ {
		if err := bt.AddTurn(); err != nil {
			t.Fatal(err)
		}
	}
	if err := bt.AddTokens(1000000); err != nil {
		t.Fatal(err)
	}
	if bt.Exceeded() {
		t.Fatal("unlimited budget should never be exceeded")
	}
}

func TestBudgetTrackerReset(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxTurns: 2})
	if err := bt.AddTurn(); err != nil {
		t.Fatal(err)
	}
	if err := bt.AddTurn(); err != nil {
		t.Fatal(err)
	}

	bt.Reset()

	if bt.TurnsUsed() != 0 {
		t.Errorf("TurnsUsed after reset = %d, want 0", bt.TurnsUsed())
	}
	if err := bt.AddTurn(); err != nil {
		t.Fatal("AddTurn after reset should succeed:", err)
	}
}

func TestBudgetTrackerConcurrentAccess(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxTurns: 1000, MaxTokens: 100000})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bt.AddTurn()
			bt.AddTokens(100)
		}()
	}
	wg.Wait()
	if bt.TurnsUsed() != 50 {
		t.Errorf("TurnsUsed = %d, want 50", bt.TurnsUsed())
	}
	if bt.TokensUsed() != 5000 {
		t.Errorf("TokensUsed = %d, want 5000", bt.TokensUsed())
	}
}

func TestBudgetTrackerErrorType(t *testing.T) {
	bt := NewBudgetTracker(Budget{MaxTurns: 1})
	if err := bt.AddTurn(); err != nil {
		t.Fatal(err)
	}
	err := bt.AddTurn()
	if err == nil {
		t.Fatal("expected error")
	}
	// ErrBudgetExceeded has Retryable: false
	if agenterrors.IsRetryable(err) {
		t.Fatal("ErrBudgetExceeded should not be retryable")
	}
}
