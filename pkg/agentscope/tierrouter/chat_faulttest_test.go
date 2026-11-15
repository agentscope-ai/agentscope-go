package tierrouter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestChatFaultUnknownDoesNotFallback(t *testing.T) {
	cloud := NewFakeChatModel(TierCloud)
	cloud.HangNext = 1
	edge := NewFakeChatModel(TierEdge)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, func() int64 { return 1 }, cloud, edge)
	ctx, cancel := context.WithTimeout(WithChatOperation(context.Background(), &Operation{ID: "chat-unknown", TaskEpoch: 1}), 30*time.Millisecond)
	defer cancel()
	if _, err := r.Chat(ctx, chatMessages("hello")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("want uncertain, got %v", err)
	}
	if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
		t.Fatalf("cloud=%d edge=%d", len(cloud.Requests()), len(edge.Requests()))
	}
	if _, err := r.Chat(WithChatOperation(context.Background(), &Operation{ID: "chat-unknown", TaskEpoch: 1}), chatMessages("hello")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay: %v", err)
	}
}

func TestChatFaultStaleSuccessIsNotAccepted(t *testing.T) {
	epoch := int64(1)
	cloud := NewFakeChatModel(TierCloud)
	cloud.Reply = "old answer"
	cloud.BeforeReturn = func() { epoch = 2 }
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud}}, func() int64 { return epoch }, cloud)
	op := &Operation{ID: "chat-stale-success", TaskEpoch: 1}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("want stale epoch, got %v", err)
	}
	rec, _ := r.ChatSnapshot(op.ID)
	if rec.State != StateSuperseded || len(rec.Attempts) != 1 || rec.Attempts[0].Outcome != AttemptCompleted || rec.ResponseAvailable {
		t.Fatalf("record: %+v", rec)
	}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale replay: %v", err)
	}
	if len(cloud.Requests()) != 1 {
		t.Fatalf("stale result redispatched: %d", len(cloud.Requests()))
	}
}

func TestChatFaultReplayUsesOriginalDeadline(t *testing.T) {
	now := time.Now()
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	r.now = func() time.Time { return now }
	op := &Operation{ID: "chat-original-deadline", TaskEpoch: 1, TTL: time.Minute}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); err != nil {
		t.Fatalf("first chat: %v", err)
	}
	now = now.Add(2 * time.Minute)
	replay := &Operation{ID: op.ID, TaskEpoch: 1, TTL: time.Minute, SubmittedAt: now}
	if _, err := r.Chat(WithChatOperation(context.Background(), replay), chatMessages("hello")); !errors.Is(err, ErrExpired) {
		t.Fatalf("replay renewed deadline: %v", err)
	}
	if len(device.Requests()) != 1 {
		t.Fatalf("replay dispatched: %d", len(device.Requests()))
	}
}

func TestChatFaultInvalidResponseDoesNotFallback(t *testing.T) {
	for name, response := range map[string]*model.ChatResponse{
		"nil":           nil,
		"in-band error": {Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}, IsLast: true, Error: errors.New("broken response")},
		"non-text":      {Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", Name: "move"}}, IsLast: true},
		"truncated":     {Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}, IsLast: true, StopReason: model.StopReasonLength},
		"not final":     {Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "partial"}}},
		"no text":       {IsLast: true},
		"bad metadata":  {Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "answer"}}, IsLast: true, Metadata: map[string]any{"bad": func() {}}},
	} {
		t.Run(name, func(t *testing.T) {
			cloud := NewFakeChatModel(TierCloud)
			cloud.Response = response
			cloud.ReturnResponse = true
			edge := NewFakeChatModel(TierEdge)
			r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, func() int64 { return 1 }, cloud, edge)
			op := &Operation{ID: "chat-invalid-" + name, TaskEpoch: 1}
			if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrChatInvalidResponse) {
				t.Fatalf("want invalid response, got %v", err)
			}
			if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
				t.Fatalf("cloud=%d edge=%d", len(cloud.Requests()), len(edge.Requests()))
			}
			rec, _ := r.ChatSnapshot(op.ID)
			if rec.State != StateUncertain || rec.ResponseAvailable {
				t.Fatalf("record: %+v", rec)
			}
		})
	}
}

func TestChatFaultEpochStopsFallback(t *testing.T) {
	epoch := int64(1)
	cloud := NewFakeChatModel(TierCloud)
	cloud.FailNext = 1
	cloud.BeforeReturn = func() { epoch = 2 }
	edge := NewFakeChatModel(TierEdge)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierCloud, TierEdge}}, func() int64 { return epoch }, cloud, edge)
	op := &Operation{ID: "chat-stale-fallback", TaskEpoch: 1}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("want stale epoch, got %v", err)
	}
	if len(cloud.Requests()) != 1 || len(edge.Requests()) != 0 {
		t.Fatalf("cloud=%d edge=%d", len(cloud.Requests()), len(edge.Requests()))
	}
}

func TestChatFaultConcurrentDuplicate(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	device.HangNext = 1
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	op := &Operation{ID: "chat-concurrent", TaskEpoch: 1}
	ctx, cancel := context.WithCancel(WithChatOperation(context.Background(), op))
	done := make(chan error, 1)
	go func() {
		_, err := r.Chat(ctx, chatMessages("hello"))
		done <- err
	}()
	for {
		rec, ok := r.ChatSnapshot(op.ID)
		if ok && len(rec.Attempts) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrInFlight) {
		t.Fatalf("duplicate: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrUncertain) {
		t.Fatalf("first call: %v", err)
	}
	if len(device.Requests()) != 1 {
		t.Fatalf("requests: %d", len(device.Requests()))
	}
}

func TestChatFaultCompletedWithoutResponseFailsClosed(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	op := &Operation{ID: "chat-missing-response", TaskEpoch: 1}
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); err != nil {
		t.Fatalf("first chat: %v", err)
	}
	r.mu.Lock()
	r.operations[op.ID].response = nil
	r.mu.Unlock()
	if _, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello")); !errors.Is(err, ErrResponseUnavailable) {
		t.Fatalf("want response unavailable, got %v", err)
	}
	if len(device.Requests()) != 1 {
		t.Fatalf("missing response redispatched: %d", len(device.Requests()))
	}
}

func TestChatFaultConcurrentReplaySettlement(t *testing.T) {
	device := NewFakeChatModel(TierDevice)
	device.Delay = 20 * time.Millisecond
	r := newChatHarness(t, StaticPolicy{Default: []Tier{TierDevice}}, func() int64 { return 1 }, device)
	op := &Operation{ID: "chat-settlement", TaskEpoch: 1}
	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Chat(WithChatOperation(context.Background(), op), chatMessages("hello"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	uncertain := 0
	for err := range errs {
		if err != nil && !errors.Is(err, ErrInFlight) {
			t.Fatalf("unexpected replay error: %v", err)
		}
		if errors.Is(err, ErrUncertain) {
			uncertain++
		}
	}
	if uncertain != 0 || len(device.Requests()) != 1 {
		t.Fatalf("uncertain=%d requests=%d", uncertain, len(device.Requests()))
	}
}
