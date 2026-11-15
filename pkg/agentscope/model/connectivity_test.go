package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// connMockModel is a simple mock for connectivity testing.
type connMockModel struct {
	name      string
	failCount int // number of calls that will fail before succeeding
	calls     int
}

func (m *connMockModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	m.calls++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if m.failCount > 0 {
		m.failCount--
		return nil, fmt.Errorf("mock %s: connection refused", m.name)
	}
	return &ChatResponse{
		Content:   []message.ContentBlock{message.TextBlock{Type: "text", Text: "reply from " + m.name}},
		IsLast:    true,
		ModelName: m.name,
	}, nil
}

func (m *connMockModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	m.calls++
	if m.failCount > 0 {
		m.failCount--
		return nil, fmt.Errorf("mock %s: connection refused", m.name)
	}
	ch := make(chan ChatResponse, 1)
	ch <- ChatResponse{
		Content:   []message.ContentBlock{message.TextBlock{Type: "text", Text: "stream from " + m.name}},
		IsLast:    true,
		ModelName: m.name,
	}
	close(ch)
	return ch, nil
}

func (m *connMockModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return len(msgs) * 10
}

func TestConnectivityAwareModel_CloudSuccess(t *testing.T) {
	local := &connMockModel{name: "local"}
	cloud := &connMockModel{name: "cloud"}

	cam := NewConnectivityAwareModel(local, cloud,
		WithFailureThreshold(2),
		WithRecoveryTimeout(100*time.Millisecond),
	)

	ctx := context.Background()
	msgs := []*message.Msg{message.UserMsg("user", "hello")}

	resp, err := cam.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ModelName != "cloud" {
		t.Errorf("expected cloud model, got %q", resp.ModelName)
	}
	if cloud.calls != 1 {
		t.Errorf("expected 1 cloud call, got %d", cloud.calls)
	}
	if local.calls != 0 {
		t.Errorf("expected 0 local calls, got %d", local.calls)
	}
}

func TestConnectivityAwareModel_FallbackToLocal(t *testing.T) {
	local := &connMockModel{name: "local"}
	cloud := &connMockModel{name: "cloud", failCount: 100} // always fail

	cam := NewConnectivityAwareModel(local, cloud,
		WithFailureThreshold(2),
		WithRecoveryTimeout(100*time.Millisecond),
	)

	ctx := context.Background()
	msgs := []*message.Msg{message.UserMsg("user", "hello")}

	// First call: cloud fails, falls back to local (failure count = 1)
	resp, err := cam.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ModelName != "local" {
		t.Errorf("expected local model on first failure, got %q", resp.ModelName)
	}

	// Second call: cloud fails again, circuit should trip (failure count = 2 >= threshold)
	resp, err = cam.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ModelName != "local" {
		t.Errorf("expected local model on second failure, got %q", resp.ModelName)
	}

	// Third call: circuit is open, should go directly to local without trying cloud
	cloudCallsBefore := cloud.calls
	resp, err = cam.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ModelName != "local" {
		t.Errorf("expected local model when circuit open, got %q", resp.ModelName)
	}
	if cloud.calls != cloudCallsBefore {
		t.Errorf("cloud should not be called when circuit is open")
	}
}

func TestConnectivityAwareModel_Recovery(t *testing.T) {
	local := &connMockModel{name: "local"}
	cloud := &connMockModel{name: "cloud", failCount: 2} // fail twice then succeed

	cam := NewConnectivityAwareModel(local, cloud,
		WithFailureThreshold(2),
		WithRecoveryTimeout(50*time.Millisecond),
	)

	ctx := context.Background()
	msgs := []*message.Msg{message.UserMsg("user", "hello")}

	// Trip the circuit: 2 failures
	for i := 0; i < 2; i++ {
		_, _ = cam.Chat(ctx, msgs)
	}

	// Circuit is now open; verify local is used
	if cam.ActiveModel() != "local" {
		t.Fatalf("expected circuit open (local active), got %q", cam.ActiveModel())
	}

	// Wait for recovery timeout
	time.Sleep(60 * time.Millisecond)

	// Now cloud has no more failures; probe should succeed and close the circuit
	resp, err := cam.Chat(ctx, msgs)
	if err != nil {
		t.Fatalf("unexpected error on recovery: %v", err)
	}
	if resp.ModelName != "cloud" {
		t.Errorf("expected cloud model after recovery, got %q", resp.ModelName)
	}
	if cam.ActiveModel() != "cloud" {
		t.Errorf("expected circuit closed after recovery, got %q", cam.ActiveModel())
	}
}

func TestConnectivityAwareModel_StreamFallback(t *testing.T) {
	local := &connMockModel{name: "local"}
	cloud := &connMockModel{name: "cloud", failCount: 1}

	cam := NewConnectivityAwareModel(local, cloud,
		WithFailureThreshold(1),
		WithRecoveryTimeout(100*time.Millisecond),
	)

	ctx := context.Background()
	msgs := []*message.Msg{message.UserMsg("user", "hello")}

	// Cloud stream setup fails → fallback to local
	ch, err := cam.ChatStream(ctx, msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp := <-ch
	if resp.ModelName != "local" {
		t.Errorf("expected local stream, got %q", resp.ModelName)
	}
}

func TestConnectivityAwareModel_CountTokens(t *testing.T) {
	local := &connMockModel{name: "local"}
	cloud := &connMockModel{name: "cloud", failCount: 100}

	cam := NewConnectivityAwareModel(local, cloud,
		WithFailureThreshold(1),
		WithRecoveryTimeout(100*time.Millisecond),
	)

	msgs := []*message.Msg{message.UserMsg("user", "hello")}

	// Circuit closed → count from cloud
	count := cam.CountTokens(msgs, nil)
	if count != 10 {
		t.Errorf("expected 10 tokens, got %d", count)
	}

	// Trip the circuit
	ctx := context.Background()
	_, _ = cam.Chat(ctx, msgs)

	// Circuit open → count from local
	count = cam.CountTokens(msgs, nil)
	if count != 10 {
		t.Errorf("expected 10 tokens from local, got %d", count)
	}
}

func TestConnectivityAwareModel_ContextCanceled(t *testing.T) {
	local := &connMockModel{name: "local"}
	cloud := &connMockModel{name: "cloud"}

	cam := NewConnectivityAwareModel(local, cloud)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	msgs := []*message.Msg{message.UserMsg("user", "hello")}

	// With canceled context, should return context error (not fall back)
	_, err := cam.Chat(ctx, msgs)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}
