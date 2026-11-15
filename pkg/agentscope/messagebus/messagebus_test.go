package messagebus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPublishSubscribe(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	ch, cancel, err := bus.Subscribe(ctx, "topic-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	_ = bus.Publish(ctx, "topic-1", []byte("hello"))

	select {
	case msg := <-ch:
		if string(msg) != "hello" {
			t.Errorf("got %q, want %q", string(msg), "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestPublishNoSubscribers(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	err := bus.Publish(context.Background(), "nobody", []byte("hello"))
	if err != nil {
		t.Fatalf("Publish to empty topic: %v", err)
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	ch1, cancel1, _ := bus.Subscribe(ctx, "t")
	ch2, cancel2, _ := bus.Subscribe(ctx, "t")
	defer cancel1()
	defer cancel2()

	_ = bus.Publish(ctx, "t", []byte("msg"))

	for _, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case msg := <-ch:
			if string(msg) != "msg" {
				t.Errorf("got %q, want %q", string(msg), "msg")
			}
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	ch, cancel, _ := bus.Subscribe(ctx, "t")

	cancel()

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after cancel")
	}
}

func TestTopicIsolation(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	ch1, cancel1, _ := bus.Subscribe(ctx, "a")
	ch2, cancel2, _ := bus.Subscribe(ctx, "b")
	defer cancel1()
	defer cancel2()

	_ = bus.Publish(ctx, "a", []byte("for-a"))

	select {
	case <-ch1:
	case <-time.After(time.Second):
		t.Fatal("timeout on ch1")
	}

	select {
	case <-ch2:
		t.Error("ch2 should not receive messages from topic a")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestQueuePushAndDrain(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	_ = bus.QueuePush(ctx, "q", []byte("a"))
	_ = bus.QueuePush(ctx, "q", []byte("b"))
	_ = bus.QueuePush(ctx, "q", []byte("c"))

	items, err := bus.QueueDrain(ctx, "q", 2)
	if err != nil {
		t.Fatalf("QueueDrain: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("drained %d, want 2", len(items))
	}
	if string(items[0]) != "a" || string(items[1]) != "b" {
		t.Errorf("items = %v, want [a, b]", items)
	}

	items, _ = bus.QueueDrain(ctx, "q", 10)
	if len(items) != 1 || string(items[0]) != "c" {
		t.Errorf("remaining = %v, want [c]", items)
	}
}

func TestQueueDrainEmpty(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	items, err := bus.QueueDrain(context.Background(), "empty", 10)
	if err != nil {
		t.Fatalf("QueueDrain: %v", err)
	}
	if items != nil {
		t.Errorf("expected nil for empty queue, got %v", items)
	}
}

func TestQueueDelete(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	_ = bus.QueuePush(ctx, "q", []byte("x"))
	_ = bus.QueueDelete(ctx, "q")

	items, _ := bus.QueueDrain(ctx, "q", 10)
	if items != nil {
		t.Errorf("expected nil after delete, got %v", items)
	}
}

func TestLogAppendAndRead(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	id1, _ := bus.LogAppend(ctx, "log1", []byte("event-1"), 0)
	id2, _ := bus.LogAppend(ctx, "log1", []byte("event-2"), 0)
	_, _ = bus.LogAppend(ctx, "log1", []byte("event-3"), 0)

	// Read all.
	entries, err := bus.LogRead(ctx, "log1", "", 0)
	if err != nil {
		t.Fatalf("LogRead: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}

	// Read since id1.
	entries, _ = bus.LogRead(ctx, "log1", id1, 0)
	if len(entries) != 2 {
		t.Fatalf("entries since id1 = %d, want 2", len(entries))
	}
	if string(entries[0].Payload) != "event-2" {
		t.Errorf("first entry = %q, want %q", string(entries[0].Payload), "event-2")
	}

	// Read since id2 with limit.
	entries, _ = bus.LogRead(ctx, "log1", id2, 1)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if string(entries[0].Payload) != "event-3" {
		t.Errorf("entry = %q, want %q", string(entries[0].Payload), "event-3")
	}
}

func TestLogReadPayloadIsolation(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	cursor, err := bus.LogAppend(ctx, "log", []byte("earlier"), 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := bus.LogAppend(ctx, "log", []byte("terminal"), 0)
	if err != nil {
		t.Fatal(err)
	}

	first, err := bus.LogRead(ctx, "log", cursor, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first read = %v, %v; want one entry", first, err)
	}
	if first[0].ID != id || string(first[0].Payload) != "terminal" {
		t.Fatalf("first read = %+v; want entry %q with the original payload", first[0], id)
	}
	first[0].Payload[0] = 'X'

	second, err := bus.LogRead(ctx, "log", cursor, 1)
	if err != nil || len(second) != 1 {
		t.Fatalf("second read = %v, %v; want one entry", second, err)
	}
	if second[0].ID != id || string(second[0].Payload) != "terminal" {
		t.Errorf("second read = %q, %q; want %q, %q", second[0].ID, second[0].Payload, id, "terminal")
	}
}

func TestLogMaxLen(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _ = bus.LogAppend(ctx, "bounded", []byte("x"), 3)
	}

	entries, _ := bus.LogRead(ctx, "bounded", "", 0)
	if len(entries) != 3 {
		t.Errorf("entries = %d, want 3 (bounded by maxLen)", len(entries))
	}
}

func TestLogReadEmpty(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	entries, err := bus.LogRead(context.Background(), "empty", "", 0)
	if err != nil {
		t.Fatalf("LogRead: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil for empty log, got %v", entries)
	}
}

func TestCloseRejectsOps(t *testing.T) {
	bus := NewInMemoryMessageBus()
	bus.Close()

	ctx := context.Background()

	if err := bus.Publish(ctx, "t", []byte("x")); err == nil {
		t.Error("Publish after close should fail")
	}
	if _, _, err := bus.Subscribe(ctx, "t"); err == nil {
		t.Error("Subscribe after close should fail")
	}
	if err := bus.QueuePush(ctx, "q", []byte("x")); err == nil {
		t.Error("QueuePush after close should fail")
	}
}

func TestConcurrentPubSub(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	ch, cancel, _ := bus.Subscribe(ctx, "concurrent")
	defer cancel()

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = bus.Publish(ctx, "concurrent", []byte("msg"))
		}()
	}
	wg.Wait()

	received := 0
	for {
		select {
		case <-ch:
			received++
		default:
			goto done
		}
	}
done:
	if received == 0 {
		t.Error("expected at least some messages")
	}
}

func TestPayloadIsolation(t *testing.T) {
	bus := NewInMemoryMessageBus()
	defer bus.Close()

	ctx := context.Background()
	original := []byte("original")
	_ = bus.QueuePush(ctx, "q", original)

	original[0] = 'X'

	items, _ := bus.QueueDrain(ctx, "q", 1)
	if string(items[0]) != "original" {
		t.Errorf("payload mutated: got %q, want %q", string(items[0]), "original")
	}
}
