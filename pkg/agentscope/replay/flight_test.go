package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// cannedHandler returns a canned response with usage.
func cannedHandler() middleware.ModelCallHandler {
	return func(_ context.Context, _ *middleware.ModelCallInput) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ok"}},
			IsLast:  true,
			Usage:   &model.ChatUsage{InputTokens: 11, OutputTokens: 7},
		}, nil
	}
}

func TestRecorder_RecordsReplyIDAndUsage(t *testing.T) {
	rec := NewRecorder()
	mc := middleware.MiddleContext{}
	ctx := middleware.WithMiddleContext(context.Background(), mc)
	mc.Set("agent", "reply_id", "reply-123")

	input := &middleware.ModelCallInput{AgentName: "a", ModelName: "m",
		Messages: []*message.Msg{message.UserMsg("u", "hi")}}
	if _, err := rec.OnModelCall(ctx, input, cannedHandler()); err != nil {
		t.Fatal(err)
	}
	entries := rec.Tape().Entries
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].ReplyID != "reply-123" {
		t.Errorf("ReplyID = %q, want reply-123", entries[0].ReplyID)
	}
	if entries[0].Usage == nil || entries[0].Usage.InputTokens != 11 || entries[0].Usage.OutputTokens != 7 {
		t.Errorf("Usage not recorded: %+v", entries[0].Usage)
	}
}

func TestRecorder_RingLimitDropsOldest(t *testing.T) {
	rec := NewRecorder(WithRingLimit(3, 0))
	ctx := context.Background()
	input := &middleware.ModelCallInput{AgentName: "a",
		Messages: []*message.Msg{message.UserMsg("u", "hi")}}
	for i := 0; i < 5; i++ {
		if _, err := rec.OnModelCall(ctx, input, cannedHandler()); err != nil {
			t.Fatal(err)
		}
	}
	entries := rec.Tape().Entries
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want ring cap 3", len(entries))
	}
	if entries[0].Index != 2 || entries[2].Index != 4 {
		t.Errorf("ring kept wrong entries: %+v", []int{entries[0].Index, entries[2].Index})
	}
}

func TestRecorder_RecordSizeLimitSummarizes(t *testing.T) {
	rec := NewRecorder(WithRecordSizeLimit(200))
	ctx := context.Background()
	big := strings.Repeat("x", 5000)
	input := &middleware.ModelCallInput{AgentName: "a",
		Messages: []*message.Msg{message.UserMsg("u", big)}}
	if _, err := rec.OnModelCall(ctx, input, cannedHandler()); err != nil {
		t.Fatal(err)
	}
	raw := rec.Tape().Entries[0].Messages
	if len(raw) >= 5000 {
		t.Fatalf("oversized message stored verbatim (%d bytes)", len(raw))
	}
	var summary []map[string]any
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatalf("summary not valid JSON: %v", err)
	}
	if len(summary) != 1 || summary[0]["summarized"] != true {
		t.Errorf("expected summarized marker, got %s", raw)
	}
}

func TestRecorder_DumpOnErrorWritesFlightFile(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(WithDumpOnError(dir))

	replyEvents := []event.Event{
		event.NewReplyStartEvent("s1", "reply-err", "agent", message.RoleAssistant),
		event.NewTextBlockStartEvent("reply-err", "b1"),
		event.NewCustomEvent("reply-err", "loop.error", map[string]any{"error": "boom"}),
	}
	core := func(_ context.Context, _ middleware.ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(replyEvents))
		for _, e := range replyEvents {
			ch <- e
		}
		close(ch)
		return ch
	}

	ctx := context.Background()
	mc := middleware.MiddleContext{}
	ctx = middleware.WithMiddleContext(ctx, mc)
	mc.Set("agent", "reply_id", "reply-err")

	input := &middleware.ModelCallInput{AgentName: "a",
		Messages: []*message.Msg{message.UserMsg("u", "hi")}}
	if _, err := rec.OnModelCall(ctx, input, cannedHandler()); err != nil {
		t.Fatal(err)
	}

	out := rec.OnReply(ctx, middleware.ReplyInput{AgentName: "a"}, core)
	for range out {
	}

	files, _ := filepath.Glob(filepath.Join(dir, "flight-reply-err*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("expected one flight file, got %v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "flight_meta") || !strings.Contains(s, "reply-err") {
		t.Errorf("flight file missing meta: %.200s", s)
	}
	if !strings.Contains(s, "model_call") {
		t.Error("flight file missing tape entries")
	}
	if !strings.Contains(s, "loop.error") {
		t.Error("flight file missing event tail")
	}
}

func TestRecorder_NoDumpOnCleanReply(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(WithDumpOnError(dir))
	core := func(_ context.Context, _ middleware.ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, 2)
		ch <- event.NewReplyStartEvent("s1", "reply-ok", "agent", message.RoleAssistant)
		ch <- event.NewReplyEndEvent("s1", "reply-ok")
		close(ch)
		return ch
	}
	out := rec.OnReply(context.Background(), middleware.ReplyInput{AgentName: "a"}, core)
	for range out {
	}
	files, _ := filepath.Glob(filepath.Join(dir, "flight-*.jsonl"))
	if len(files) != 0 {
		t.Errorf("clean reply must not dump, got %v", files)
	}
}

func TestRecorder_RedactorAppliedOnDump(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(WithDumpOnError(dir), WithRedactor(func(s string) string {
		return strings.ReplaceAll(s, "SECRET", "***")
	}))
	replyEvents := []event.Event{
		event.NewReplyStartEvent("s1", "reply-r", "agent", message.RoleAssistant),
		event.NewCustomEvent("reply-r", "loop.error", map[string]any{"error": "x"}),
	}
	core := func(_ context.Context, _ middleware.ReplyInput) <-chan event.Event {
		ch := make(chan event.Event, len(replyEvents))
		for _, e := range replyEvents {
			ch <- e
		}
		close(ch)
		return ch
	}
	ctx := context.Background()
	input := &middleware.ModelCallInput{AgentName: "a",
		Messages: []*message.Msg{message.UserMsg("u", "my SECRET token")}}
	if _, err := rec.OnModelCall(ctx, input, cannedHandler()); err != nil {
		t.Fatal(err)
	}
	out := rec.OnReply(ctx, middleware.ReplyInput{AgentName: "a"}, core)
	for range out {
	}
	files, _ := filepath.Glob(filepath.Join(dir, "flight-reply-r*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("expected flight file, got %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if strings.Contains(string(data), "SECRET") {
		t.Error("redactor not applied to dumped content")
	}
	if !strings.Contains(string(data), "***") {
		t.Error("redacted marker missing")
	}
}
