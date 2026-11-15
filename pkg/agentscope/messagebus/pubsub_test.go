package messagebus

import (
	"testing"
)

func TestApplyPubOptions_Defaults(t *testing.T) {
	cfg := ApplyPubOptions(nil)
	if cfg.QoS() != 0 {
		t.Errorf("default QoS should be 0, got %d", cfg.QoS())
	}
	if cfg.Retain() {
		t.Error("default Retain should be false")
	}
}

func TestApplyPubOptions_WithQoS(t *testing.T) {
	for _, tc := range []struct {
		qos  byte
		want byte
	}{
		{0, 0},
		{1, 1},
		{2, 2},
		{3, 0}, // invalid, should not change default
	} {
		opts := []PubOption{WithQoS(tc.qos)}
		cfg := ApplyPubOptions(opts)
		if cfg.QoS() != tc.want {
			t.Errorf("WithQoS(%d): got %d, want %d", tc.qos, cfg.QoS(), tc.want)
		}
	}
}

func TestApplyPubOptions_WithRetain(t *testing.T) {
	cfg := ApplyPubOptions([]PubOption{WithRetain(true)})
	if !cfg.Retain() {
		t.Error("WithRetain(true) should set Retain to true")
	}

	cfg = ApplyPubOptions([]PubOption{WithRetain(false)})
	if cfg.Retain() {
		t.Error("WithRetain(false) should set Retain to false")
	}
}

func TestApplyPubOptions_Combined(t *testing.T) {
	cfg := ApplyPubOptions([]PubOption{WithQoS(2), WithRetain(true)})
	if cfg.QoS() != 2 {
		t.Errorf("expected QoS 2, got %d", cfg.QoS())
	}
	if !cfg.Retain() {
		t.Error("expected Retain true")
	}
}

func TestApplySubOptions_Defaults(t *testing.T) {
	cfg := ApplySubOptions(nil)
	if cfg.QoS() != 0 {
		t.Errorf("default QoS should be 0, got %d", cfg.QoS())
	}
	if cfg.BufferSize() != 64 {
		t.Errorf("default BufferSize should be 64, got %d", cfg.BufferSize())
	}
}

func TestApplySubOptions_WithSubQoS(t *testing.T) {
	cfg := ApplySubOptions([]SubOption{WithSubQoS(1)})
	if cfg.QoS() != 1 {
		t.Errorf("expected QoS 1, got %d", cfg.QoS())
	}
}

func TestApplySubOptions_WithBufferSize(t *testing.T) {
	cfg := ApplySubOptions([]SubOption{WithBufferSize(128)})
	if cfg.BufferSize() != 128 {
		t.Errorf("expected BufferSize 128, got %d", cfg.BufferSize())
	}

	// Invalid buffer size should keep default
	cfg = ApplySubOptions([]SubOption{WithBufferSize(0)})
	if cfg.BufferSize() != 64 {
		t.Errorf("invalid BufferSize should keep default 64, got %d", cfg.BufferSize())
	}

	cfg = ApplySubOptions([]SubOption{WithBufferSize(-1)})
	if cfg.BufferSize() != 64 {
		t.Errorf("negative BufferSize should keep default 64, got %d", cfg.BufferSize())
	}
}

func TestApplySubOptions_Combined(t *testing.T) {
	cfg := ApplySubOptions([]SubOption{WithSubQoS(2), WithBufferSize(256)})
	if cfg.QoS() != 2 {
		t.Errorf("expected QoS 2, got %d", cfg.QoS())
	}
	if cfg.BufferSize() != 256 {
		t.Errorf("expected BufferSize 256, got %d", cfg.BufferSize())
	}
}

func TestPubSubMessage_Fields(t *testing.T) {
	msg := PubSubMessage{
		Topic:   "sensors/temperature",
		Payload: []byte(`{"value": 25.3}`),
		QoS:     1,
		Retain:  true,
	}

	if msg.Topic != "sensors/temperature" {
		t.Errorf("unexpected topic: %s", msg.Topic)
	}
	if string(msg.Payload) != `{"value": 25.3}` {
		t.Errorf("unexpected payload: %s", msg.Payload)
	}
	if msg.QoS != 1 {
		t.Errorf("unexpected QoS: %d", msg.QoS)
	}
	if !msg.Retain {
		t.Error("expected Retain true")
	}
}

// TestInMemoryPubSub_ImplementsPubSub verifies that if someone builds a
// minimal in-memory PubSub (like the fleet example does), the interface
// compiles correctly. This is a compile-time check.
func TestPubSubInterface_Compile(t *testing.T) {
	// This test verifies the interface is well-formed by attempting
	// a nil interface assignment.
	var _ PubSub = (PubSub)(nil)
}
