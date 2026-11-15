package agent

import (
	"testing"
)

// TestNewUnifiedAgent_FailFast proves the constructor rejects the two invalid
// inputs that otherwise surface as a confusing nil-dereference much later in the
// reply loop: a nil model and an empty name. An empty system prompt is valid.
func TestNewUnifiedAgent_FailFast(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("%s: expected panic, got none", name)
			}
		}()
		fn()
	}

	assertPanics("nil model", func() {
		NewUnifiedAgent("bot", "prompt", nil)
	})
	assertPanics("empty name", func() {
		NewUnifiedAgent("", "prompt", &mockChatModel{})
	})

	// Empty system prompt must NOT panic — it is a supported configuration.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("empty system prompt should be valid, but panicked: %v", r)
		}
	}()
	_ = NewUnifiedAgent("bot", "", &mockChatModel{})
}
