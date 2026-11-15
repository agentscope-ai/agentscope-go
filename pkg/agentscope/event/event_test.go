package event

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestEventInterface(t *testing.T) {
	// Verify all event types satisfy the Event interface.
	var events []Event
	events = append(events,
		NewReplyStartEvent("s1", "r1", "bot", message.RoleAssistant),
		NewReplyEndEvent("s1", "r1"),
		NewModelCallStartEvent("r1", "qwen-plus"),
		NewModelCallEndEvent("r1", 100, 50),
		NewTextBlockStartEvent("r1", "b1"),
		NewTextBlockDeltaEvent("r1", "b1", "hello"),
		NewTextBlockEndEvent("r1", "b1"),
		NewThinkingBlockStartEvent("r1", "b2"),
		NewThinkingBlockDeltaEvent("r1", "b2", "thinking..."),
		NewThinkingBlockEndEvent("r1", "b2"),
		NewDataBlockStartEvent("r1", "b3", "image/png"),
		NewDataBlockDeltaEvent("r1", "b3", "base64data", "image/png"),
		NewDataBlockEndEvent("r1", "b3"),
		NewToolCallStartEvent("r1", "tc1", "bash"),
		NewToolCallDeltaEvent("r1", "tc1", `{"cmd":"ls"}`),
		NewToolCallEndEvent("r1", "tc1"),
		NewToolResultStartEvent("r1", "tc1", "bash"),
		NewToolResultTextDeltaEvent("r1", "tc1", "file1.go"),
		NewToolResultDataDeltaEvent("r1", "tc1", "b4", "image/png", "data", ""),
		NewToolResultEndEvent("r1", "tc1", message.ToolResultSuccess),
		NewHintBlockEvent("r1", "h1", "budget", "Token budget exceeded"),
		NewRequireUserConfirmEvent("r1", nil),
		NewUserConfirmResultEvent("r1", nil),
		NewRequireExternalExecutionEvent("r1", nil),
		NewExternalExecutionResultEvent("r1", nil),
		NewExceedMaxItersEvent("r1", "bot"),
		NewCustomEvent("r1", "my_event", map[string]any{"key": "value"}),
	)

	if len(events) != 27 {
		t.Fatalf("expected 27 events, got %d", len(events))
	}

	for _, ev := range events {
		if ev.GetEventID() == "" {
			t.Errorf("event %s has empty ID", ev.GetEventType())
		}
		if ev.EventTypeString() == "" {
			t.Errorf("event has empty type string")
		}
		if string(ev.GetEventType()) != ev.EventTypeString() {
			t.Errorf("EventType mismatch: %s vs %s", ev.GetEventType(), ev.EventTypeString())
		}
	}
}

func TestAppendEvent_TextStreaming(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:      replyID,
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	msg.AppendEvent(NewTextBlockStartEvent(replyID, "b1"))
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 block after start, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "" {
		t.Fatalf("expected empty text after start, got %q", tb.Text)
	}

	msg.AppendEvent(NewTextBlockDeltaEvent(replyID, "b1", "hello "))
	msg.AppendEvent(NewTextBlockDeltaEvent(replyID, "b1", "world"))
	tb = msg.Content[0].(message.TextBlock)
	if tb.Text != "hello world" {
		t.Fatalf("expected 'hello world', got %q", tb.Text)
	}

	msg.AppendEvent(NewTextBlockEndEvent(replyID, "b1"))
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 block after end, got %d", len(msg.Content))
	}
}

func TestAppendEvent_ToolCallStreaming(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:      replyID,
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	msg.AppendEvent(NewToolCallStartEvent(replyID, "tc1", "bash"))
	msg.AppendEvent(NewToolCallDeltaEvent(replyID, "tc1", `{"cmd"`))
	msg.AppendEvent(NewToolCallDeltaEvent(replyID, "tc1", `:"ls"}`))
	msg.AppendEvent(NewToolCallEndEvent(replyID, "tc1"))

	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(msg.Content))
	}
	tc, ok := msg.Content[0].(message.ToolCallBlock)
	if !ok {
		t.Fatalf("expected ToolCallBlock, got %T", msg.Content[0])
	}
	if tc.Name != "bash" {
		t.Fatalf("expected name bash, got %s", tc.Name)
	}
	if tc.Input != `{"cmd":"ls"}` {
		t.Fatalf("expected input {\"cmd\":\"ls\"}, got %s", tc.Input)
	}
}

func TestAppendEvent_ToolResultStreaming(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:   replyID,
		Name: "bot",
		Role: message.RoleAssistant,
		Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "bash", State: message.ToolCallAllowed},
		},
	}

	msg.AppendEvent(NewToolResultStartEvent(replyID, "tc1", "bash"))
	msg.AppendEvent(NewToolResultTextDeltaEvent(replyID, "tc1", "file1.go\n"))
	msg.AppendEvent(NewToolResultTextDeltaEvent(replyID, "tc1", "file2.go\n"))
	msg.AppendEvent(NewToolResultEndEvent(replyID, "tc1", message.ToolResultSuccess))

	// Should have 2 blocks: ToolCallBlock + ToolResultBlock
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(msg.Content))
	}

	tr, ok := msg.Content[1].(message.ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock, got %T", msg.Content[1])
	}
	if tr.State != message.ToolResultSuccess {
		t.Fatalf("expected success state, got %s", tr.State)
	}
	if tr.GetOutputText() != "file1.go\nfile2.go\n" {
		t.Fatalf("expected output text, got %q", tr.GetOutputText())
	}

	// ToolCallBlock should be marked Finished
	tc := msg.Content[0].(message.ToolCallBlock)
	if tc.State != message.ToolCallFinished {
		t.Fatalf("expected ToolCall to be finished, got %s", tc.State)
	}
}

func TestAppendEvent_ModelCallEnd_Usage(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:      replyID,
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	msg.AppendEvent(NewModelCallEndEvent(replyID, 100, 50))
	if msg.Usage == nil {
		t.Fatal("expected usage to be set")
	}
	if msg.Usage.InputTokens != 100 || msg.Usage.OutputTokens != 50 {
		t.Fatalf("expected 100/50, got %d/%d", msg.Usage.InputTokens, msg.Usage.OutputTokens)
	}

	// Second model call should accumulate
	msg.AppendEvent(NewModelCallEndEvent(replyID, 200, 100))
	if msg.Usage.InputTokens != 300 || msg.Usage.OutputTokens != 150 {
		t.Fatalf("expected 300/150, got %d/%d", msg.Usage.InputTokens, msg.Usage.OutputTokens)
	}
}

func TestAppendEvent_ReplyEnd(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:      replyID,
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	if msg.FinishedAt != "" {
		t.Fatal("expected empty FinishedAt before ReplyEnd")
	}
	msg.AppendEvent(NewReplyEndEvent("s1", replyID))
	if msg.FinishedAt == "" {
		t.Fatal("expected FinishedAt set after ReplyEnd")
	}
}

func TestAppendEvent_IgnoreWrongReplyID(t *testing.T) {
	msg := &message.Msg{
		ID:      "r1",
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	msg.AppendEvent(NewTextBlockStartEvent("wrong_reply_id", "b1"))
	if len(msg.Content) != 0 {
		t.Fatal("expected no blocks for wrong reply ID")
	}
}

func TestAppendEvent_HintBlock(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:      replyID,
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	msg.AppendEvent(NewHintBlockEvent(replyID, "h1", "budget", "Token budget exceeded"))
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(msg.Content))
	}
	hb, ok := msg.Content[0].(message.HintBlock)
	if !ok {
		t.Fatalf("expected HintBlock, got %T", msg.Content[0])
	}
	if hb.Source != "budget" || hb.Hint != "Token budget exceeded" {
		t.Fatalf("unexpected hint block: %+v", hb)
	}
}

func TestAppendEvent_ThinkingStreaming(t *testing.T) {
	replyID := "r1"
	msg := &message.Msg{
		ID:      replyID,
		Name:    "bot",
		Role:    message.RoleAssistant,
		Content: []message.ContentBlock{},
	}

	msg.AppendEvent(NewThinkingBlockStartEvent(replyID, "th1"))
	msg.AppendEvent(NewThinkingBlockDeltaEvent(replyID, "th1", "let me "))
	msg.AppendEvent(NewThinkingBlockDeltaEvent(replyID, "th1", "think..."))
	msg.AppendEvent(NewThinkingBlockEndEvent(replyID, "th1"))

	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(message.ThinkingBlock)
	if !ok {
		t.Fatalf("expected ThinkingBlock, got %T", msg.Content[0])
	}
	if tb.Thinking != "let me think..." {
		t.Fatalf("expected 'let me think...', got %q", tb.Thinking)
	}
}

func TestToolResultDataDeltaEvent_Validate(t *testing.T) {
	// Upstream #2370: exactly one of Data/URL must be set.
	valid := []ToolResultDataDeltaEvent{
		NewToolResultDataDeltaEvent("r1", "tc1", "b1", "image/png", "AAAA", ""),
		NewToolResultDataDeltaEvent("r1", "tc1", "b1", "image/png", "", "https://x/y.png"),
	}
	for i, ev := range valid {
		if err := ev.Validate(); err != nil {
			t.Errorf("valid case %d: unexpected error: %v", i, err)
		}
	}
	invalid := []ToolResultDataDeltaEvent{
		NewToolResultDataDeltaEvent("r1", "tc1", "b1", "image/png", "", ""),
		NewToolResultDataDeltaEvent("r1", "tc1", "b1", "image/png", "AAAA", "https://x/y.png"),
	}
	for i, ev := range invalid {
		if err := ev.Validate(); err == nil {
			t.Errorf("invalid case %d: expected validation error", i)
		}
	}
}

func TestModelCallEndEventWithCache_CarriesCacheTokens(t *testing.T) {
	// Upstream #2318 class: prompt-cache accounting must ride the event so
	// observability/consumers see cache creation and read tokens.
	evt := NewModelCallEndEventWithCache("r1", 100, 50, 20, 30)
	if evt.InputTokens != 100 || evt.OutputTokens != 50 {
		t.Errorf("token counts = %d/%d, want 100/50", evt.InputTokens, evt.OutputTokens)
	}
	if evt.CacheCreationTokens != 20 {
		t.Errorf("CacheCreationTokens = %d, want 20", evt.CacheCreationTokens)
	}
	if evt.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, want 30", evt.CacheReadTokens)
	}
	if evt.GetEventType() != EventModelCallEnd {
		t.Errorf("event type = %v, want %v", evt.GetEventType(), EventModelCallEnd)
	}
}
