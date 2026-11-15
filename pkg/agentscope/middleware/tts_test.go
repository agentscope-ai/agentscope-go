package middleware

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tts"
)

// mockTTSModel returns fixed audio data for any text input.
type mockTTSModel struct {
	audio     []byte
	mediaType string
	streaming bool
	chunks    int
}

func (m *mockTTSModel) Synthesize(_ context.Context, _ string) (*tts.Response, error) {
	return &tts.Response{
		Content:   m.audio,
		MediaType: m.mediaType,
		IsLast:    true,
	}, nil
}

func (m *mockTTSModel) SynthesizeStream(_ context.Context, _ string) (<-chan tts.Response, error) {
	if !m.streaming {
		return nil, tts.ErrStreamNotSupported
	}
	ch := make(chan tts.Response, m.chunks)
	chunkSize := len(m.audio) / m.chunks
	for i := 0; i < m.chunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if i == m.chunks-1 {
			end = len(m.audio)
		}
		ch <- tts.Response{
			Content:   m.audio[start:end],
			MediaType: m.mediaType,
			IsLast:    i == m.chunks-1,
		}
	}
	close(ch)
	return ch, nil
}

func (m *mockTTSModel) ModelName() string { return "mock-tts" }

func makeTextEvents(replyID, text string) []event.Event {
	blockID := "blk1"
	return []event.Event{
		event.NewReplyStartEvent("s1", replyID, "agent", "assistant"),
		event.NewTextBlockStartEvent(replyID, blockID),
		event.NewTextBlockDeltaEvent(replyID, blockID, text),
		event.NewTextBlockEndEvent(replyID, blockID),
		event.NewReplyEndEvent("s1", replyID),
	}
}

func collectEvents(ch <-chan event.Event) []event.Event {
	var events []event.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func TestTTSMiddleware_SynthesizesAudio(t *testing.T) {
	model := &mockTTSModel{
		audio:     []byte{0x01, 0x02, 0x03, 0x04},
		mediaType: "audio/wav",
	}
	mw := NewTTSMiddleware(model)

	inputEvents := makeTextEvents("r1", "Hello world")
	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(inputEvents))
		for _, ev := range inputEvents {
			ch <- ev
		}
		close(ch)
		return ch
	}

	outCh := mw.OnReply(context.Background(), ReplyInput{UserInput: "test"}, next)
	events := collectEvents(outCh)

	// Original 5 events + 3 audio events (start, delta, end)
	if len(events) != 8 {
		var types []string
		for _, e := range events {
			types = append(types, string(e.GetEventType()))
		}
		t.Fatalf("expected 8 events, got %d: %v", len(events), types)
	}

	// Audio events are injected between text_block_end (3) and reply_end (7)
	if events[4].GetEventType() != event.EventDataBlockStart {
		t.Errorf("event 4 should be data_block_start, got %s", events[4].GetEventType())
	}
	if events[5].GetEventType() != event.EventDataBlockDelta {
		t.Errorf("event 5 should be data_block_delta, got %s", events[5].GetEventType())
	}
	if events[6].GetEventType() != event.EventDataBlockEnd {
		t.Errorf("event 6 should be data_block_end, got %s", events[6].GetEventType())
	}
	if events[7].GetEventType() != event.EventReplyEnd {
		t.Errorf("event 7 should be reply_end, got %s", events[7].GetEventType())
	}
}

func TestTTSMiddleware_StreamingSynthesis(t *testing.T) {
	model := &mockTTSModel{
		audio:     []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06},
		mediaType: "audio/wav",
		streaming: true,
		chunks:    3,
	}
	mw := NewTTSMiddleware(model)

	inputEvents := makeTextEvents("r1", "Hello")
	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(inputEvents))
		for _, ev := range inputEvents {
			ch <- ev
		}
		close(ch)
		return ch
	}

	outCh := mw.OnReply(context.Background(), ReplyInput{}, next)
	events := collectEvents(outCh)

	// 5 original + 1 start + 3 deltas + 1 end = 10
	if len(events) != 10 {
		var types []string
		for _, e := range events {
			types = append(types, string(e.GetEventType()))
		}
		t.Fatalf("expected 10 events, got %d: %v", len(events), types)
	}

	// Count data block deltas
	deltaCount := 0
	for _, ev := range events {
		if ev.GetEventType() == event.EventDataBlockDelta {
			deltaCount++
		}
	}
	if deltaCount != 3 {
		t.Errorf("expected 3 data_block_delta events for 3 chunks, got %d", deltaCount)
	}
}

func TestTTSMiddleware_EmptyText_NoAudio(t *testing.T) {
	model := &mockTTSModel{audio: []byte{0x01}, mediaType: "audio/wav"}
	mw := NewTTSMiddleware(model)

	// Text block with empty delta
	inputEvents := []event.Event{
		event.NewReplyStartEvent("s1", "r1", "agent", "assistant"),
		event.NewTextBlockStartEvent("r1", "b1"),
		event.NewTextBlockEndEvent("r1", "b1"),
		event.NewReplyEndEvent("s1", "r1"),
	}

	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(inputEvents))
		for _, ev := range inputEvents {
			ch <- ev
		}
		close(ch)
		return ch
	}

	outCh := mw.OnReply(context.Background(), ReplyInput{}, next)
	events := collectEvents(outCh)

	// Only original events, no audio
	if len(events) != 4 {
		t.Errorf("expected 4 events (no audio for empty text), got %d", len(events))
	}
}

func TestTTSMiddleware_PassthroughEvents(t *testing.T) {
	model := &mockTTSModel{audio: []byte{0x01}, mediaType: "audio/wav"}
	mw := NewTTSMiddleware(model)

	// Events without text (no text block delta)
	inputEvents := []event.Event{
		event.NewReplyStartEvent("s1", "r1", "agent", "assistant"),
		event.NewReplyEndEvent("s1", "r1"),
	}

	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(inputEvents))
		for _, ev := range inputEvents {
			ch <- ev
		}
		close(ch)
		return ch
	}

	outCh := mw.OnReply(context.Background(), ReplyInput{}, next)
	events := collectEvents(outCh)

	if len(events) != 2 {
		t.Errorf("expected 2 pass-through events, got %d", len(events))
	}
}

func TestTTSMiddleware_MultipleTextBlocks(t *testing.T) {
	model := &mockTTSModel{
		audio:     []byte{0xAA, 0xBB},
		mediaType: "audio/wav",
	}
	mw := NewTTSMiddleware(model)

	inputEvents := []event.Event{
		event.NewReplyStartEvent("s1", "r1", "agent", "assistant"),
		event.NewTextBlockStartEvent("r1", "b1"),
		event.NewTextBlockDeltaEvent("r1", "b1", "first"),
		event.NewTextBlockEndEvent("r1", "b1"),
		event.NewTextBlockStartEvent("r1", "b2"),
		event.NewTextBlockDeltaEvent("r1", "b2", "second"),
		event.NewTextBlockEndEvent("r1", "b2"),
		event.NewReplyEndEvent("s1", "r1"),
	}

	next := func(_ context.Context, _ ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(inputEvents))
		for _, ev := range inputEvents {
			ch <- ev
		}
		close(ch)
		return ch
	}

	outCh := mw.OnReply(context.Background(), ReplyInput{}, next)
	events := collectEvents(outCh)

	// 8 original + 2 * 3 audio events = 14
	if len(events) != 14 {
		var types []string
		for _, e := range events {
			types = append(types, string(e.GetEventType()))
		}
		t.Fatalf("expected 14 events for 2 text blocks, got %d: %v", len(events), types)
	}
}

func TestTTSMiddleware_Key(t *testing.T) {
	mw := NewTTSMiddleware(tts.DummyModel{})
	if mw.Key() != "tts" {
		t.Errorf("expected key tts, got %s", mw.Key())
	}
}
