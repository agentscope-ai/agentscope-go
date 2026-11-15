package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// --- Mock model for compression tests ---

type compressionMockModel struct {
	chatCallCount int64
	tokenCount    int
	contextSize   int
	chatResponse  *model.ChatResponse
	chatErr       error
}

func (m *compressionMockModel) Chat(_ context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	atomic.AddInt64(&m.chatCallCount, 1)
	if m.chatErr != nil {
		return nil, m.chatErr
	}
	if m.chatResponse != nil {
		return m.chatResponse, nil
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "tc_summary",
				Name:  "generate_structured_output",
				Input: `{"task_overview":"Test task","current_state":"In progress","important_discoveries":"None","next_steps":"Continue","context_to_preserve":"Nothing special"}`,
				State: message.ToolCallPending,
			},
		},
		IsLast: true,
	}, nil
}

func (m *compressionMockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *compressionMockModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int {
	return m.tokenCount
}

func (m *compressionMockModel) ContextSize() int {
	return m.contextSize
}

func (m *compressionMockModel) ModelName() string {
	return "test-model"
}

// --- Tests ---

func TestContextConfigDefaults(t *testing.T) {
	cfg := ContextConfig{}
	got := cfg.withDefaults()

	if got.TriggerRatio != 0.8 {
		t.Errorf("TriggerRatio = %v, want 0.8", got.TriggerRatio)
	}
	if got.ReserveRatio != 0.1 {
		t.Errorf("ReserveRatio = %v, want 0.1", got.ReserveRatio)
	}
	if got.ToolResultLimit != 50000 {
		t.Errorf("ToolResultLimit = %v, want 50000", got.ToolResultLimit)
	}
	if got.CompressionPrompt == "" {
		t.Error("CompressionPrompt should not be empty")
	}
	if got.SummaryTemplate == "" {
		t.Error("SummaryTemplate should not be empty")
	}
	if got.SummarySchema == nil {
		t.Error("SummarySchema should not be nil")
	}
}

func TestContextConfigCustomValues(t *testing.T) {
	cfg := ContextConfig{
		TriggerRatio:      0.7,
		ReserveRatio:      0.2,
		CompressionPrompt: "custom prompt",
		ToolResultLimit:   10000,
	}
	got := cfg.withDefaults()

	if got.TriggerRatio != 0.7 {
		t.Errorf("TriggerRatio = %v, want 0.7", got.TriggerRatio)
	}
	if got.ReserveRatio != 0.2 {
		t.Errorf("ReserveRatio = %v, want 0.2", got.ReserveRatio)
	}
	if got.CompressionPrompt != "custom prompt" {
		t.Errorf("CompressionPrompt = %v, want 'custom prompt'", got.CompressionPrompt)
	}
	if got.ToolResultLimit != 10000 {
		t.Errorf("ToolResultLimit = %v, want 10000", got.ToolResultLimit)
	}
}

func TestCompressContext_NoConfigSkips(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 100000}
	agent := NewUnifiedAgent("test", "prompt", mock)

	err := agent.compressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Error("should not call model when contextCfg is nil")
	}
}

func TestCompressContext_BelowThresholdSkips(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount:  5000,
		contextSize: 32000,
	}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}),
	)
	agent.state.Context = []*message.Msg{
		message.UserMsg("user", "hello"),
		message.AssistantMsg("bot", "hi"),
	}

	err := agent.compressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&mock.chatCallCount) != 0 {
		t.Error("should not call model when below threshold")
	}
}

func TestCompressContext_TriggersAboveThreshold(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
	}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{
			TriggerRatio: 0.8,
			ReserveRatio: 0.1,
		}),
	)

	for i := 0; i < 10; i++ {
		agent.state.Context = append(agent.state.Context,
			message.UserMsg("user", fmt.Sprintf("message %d", i)),
			message.AssistantMsg("bot", fmt.Sprintf("reply %d", i)),
		)
	}

	err := agent.compressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if atomic.LoadInt64(&mock.chatCallCount) == 0 {
		t.Error("should have called model for compression")
	}

	if agent.state.Summary == "" {
		t.Error("summary should be set after compression")
	}

	if !strings.Contains(agent.state.Summary, "Test task") {
		t.Errorf("summary should contain structured output, got: %s", agent.state.Summary)
	}
}

func TestCompressContext_EmptyContextError(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
	}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}),
	)

	err := agent.compressContext(context.Background())
	if err == nil {
		t.Error("should error when context is empty but threshold exceeded")
	}
	if !strings.Contains(err.Error(), "cannot compress") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompressContext_PreservesSummaryInInput(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
	}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}),
	)
	agent.state.Summary = "previous summary content"
	agent.state.Context = []*message.Msg{
		message.UserMsg("user", "msg1"),
		message.AssistantMsg("bot", "reply1"),
	}

	err := agent.compressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if agent.state.Summary == "" || agent.state.Summary == "previous summary content" {
		t.Error("summary should be updated with new structured output")
	}
}

func TestSplitContextForCompression_AllReserved(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount: 100,
	}
	agent := NewUnifiedAgent("test", "prompt", mock)
	agent.state.Context = []*message.Msg{
		message.UserMsg("user", "hello"),
		message.AssistantMsg("bot", "hi"),
	}

	compress, reserve := agent.splitContextForCompression(10000, nil)
	if len(compress) != 0 {
		t.Errorf("expected 0 messages to compress, got %d", len(compress))
	}
	if len(reserve) != 2 {
		t.Errorf("expected 2 messages to reserve, got %d", len(reserve))
	}
}

func TestSplitContextForCompression_ZeroBudget(t *testing.T) {
	mock := &compressionMockModel{tokenCount: 500}
	agent := NewUnifiedAgent("test", "prompt", mock)
	agent.state.Context = []*message.Msg{
		message.UserMsg("user", "hello"),
		message.AssistantMsg("bot", "hi"),
		message.UserMsg("user", "bye"),
	}

	compress, reserve := agent.splitContextForCompression(0, nil)
	if len(compress) != 3 {
		t.Errorf("expected 3 messages to compress, got %d", len(compress))
	}
	if len(reserve) != 0 {
		t.Errorf("expected 0 messages to reserve, got %d", len(reserve))
	}
}

func TestAdjustSplitForToolPairs(t *testing.T) {
	msgs := []*message.Msg{
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc1", Name: "foo"},
		}),
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "tc1", Name: "foo", Output: "result1"},
		}),
		message.UserMsg("user", "next message"),
	}

	// splitIdx=0: reserved includes all three messages. Tool call "tc1"
	// is at msg[0] and result "tc1" at msg[1] — both in reserved, no orphan.
	idx := adjustSplitForToolPairs(msgs, 0)
	if idx != 0 {
		t.Errorf("both call and result in reserved, split should stay at 0, got %d", idx)
	}

	// splitIdx=1: reserved = [msgs[1], msgs[2]]. Result "tc1" at idx 1 has
	// no matching call in reserved, so it's orphaned. Split pushes to 2.
	idx2 := adjustSplitForToolPairs(msgs, 1)
	if idx2 != 2 {
		t.Errorf("should push split past orphan result, got %d", idx2)
	}

	// splitIdx=2: reserved = [msgs[2]]. No tool results → no adjustment.
	idx3 := adjustSplitForToolPairs(msgs, 2)
	if idx3 != 2 {
		t.Errorf("no tool pairs in reserved, split should stay at 2, got %d", idx3)
	}
}

func TestAdjustSplitForToolPairs_OrphanResult(t *testing.T) {
	msgs := []*message.Msg{
		message.UserMsg("user", "question"),
		message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
			message.ToolResultBlock{Type: "tool_result", ID: "tc_orphan", Name: "bar", Output: "orphan result"},
		}),
		message.UserMsg("user", "follow up"),
	}

	idx := adjustSplitForToolPairs(msgs, 1)
	if idx != 2 {
		t.Errorf("should push split past orphan result, got %d", idx)
	}
}

func TestTruncateToolResult(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		limit     int
		wantTrunc bool
	}{
		{"under limit", "short text", 100, false},
		{"at limit", strings.Repeat("x", 400), 100, false},
		{"over limit", strings.Repeat("x", 500), 100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, truncated := TruncateToolResult(tt.text, tt.limit)
			if truncated != tt.wantTrunc {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTrunc)
			}
			if !truncated && result != tt.text {
				t.Error("non-truncated result should equal input")
			}
		})
	}
}

func TestTruncateToolResultKeepsHeadAndTail(t *testing.T) {
	head := strings.Repeat("h", 200)
	middle := strings.Repeat("m", 600)
	tail := strings.Repeat("t", 200)
	text := head + middle + tail

	result, truncated := TruncateToolResult(text, 100) // 400-char budget
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !strings.HasPrefix(result, strings.Repeat("h", 100)) {
		t.Error("truncated result should keep the head")
	}
	if !strings.HasSuffix(result, tail) {
		t.Error("truncated result should keep the tail")
	}
	if !strings.Contains(result, "<<<TRUNCATED:") {
		t.Error("truncated result should carry an elision marker")
	}
	if strings.Contains(result, strings.Repeat("m", 400)) {
		t.Error("middle content should be elided")
	}
}

func TestTruncateToolResultRuneSafe(t *testing.T) {
	// Multi-byte content must not be cut mid-rune: providers reject invalid
	// UTF-8 with a 400, which would turn a truncation into a dead reply.
	text := strings.Repeat("中", 300)                   // 900 bytes, 300 runes
	result, truncated := TruncateToolResult(text, 100) // 400-byte budget
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(result) {
		t.Error("truncated result is not valid UTF-8")
	}
	if !strings.HasPrefix(result, "中") || !strings.HasSuffix(result, "中") {
		t.Error("head and tail should both survive multi-byte truncation")
	}
}

func TestFormatSummary(t *testing.T) {
	template := "Overview: {task_overview}, State: {current_state}"
	data := json.RawMessage(`{"task_overview":"Build X","current_state":"Done"}`)

	result, err := formatSummary(template, data)
	if err != nil {
		t.Fatal(err)
	}
	if result != "Overview: Build X, State: Done" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestFormatSummary_InvalidJSON(t *testing.T) {
	_, err := formatSummary("{foo}", json.RawMessage(`not valid`))
	if err == nil {
		t.Error("should error on invalid JSON")
	}
}

func TestResolveContextSize_ContextSizer(t *testing.T) {
	mock := &compressionMockModel{contextSize: 64000}
	got := model.ResolveContextSize(mock, 128000)
	if got != 64000 {
		t.Errorf("ResolveContextSize = %d, want 64000", got)
	}
}

func TestResolveContextSize_Fallback(t *testing.T) {
	mock := &mockChatModel{}
	got := model.ResolveContextSize(mock, 128000)
	if got != 128000 {
		t.Errorf("ResolveContextSize = %d, want 128000 fallback", got)
	}
}

func TestBuildCompressionMessages(t *testing.T) {
	compress := []*message.Msg{
		message.UserMsg("user", "old msg 1"),
		message.AssistantMsg("bot", "old reply"),
	}

	msgs := buildCompressionMessages("system prompt", "existing summary", compress, "compress now")
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}

	if msgs[0].Role != message.RoleSystem {
		t.Error("first message should be system")
	}
	if msgs[1].Role != message.RoleUser {
		t.Error("second message should be user (summary)")
	}
	if msgs[4].Role != message.RoleUser {
		t.Error("last message should be user (compression prompt)")
	}
}

func TestBuildCompressionMessages_NoSummary(t *testing.T) {
	compress := []*message.Msg{message.UserMsg("user", "old msg")}
	msgs := buildCompressionMessages("prompt", "", compress, "compress now")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (no summary), got %d", len(msgs))
	}
}

// --- Middleware integration ---

type compressCounterMiddleware struct {
	middleware.BaseMiddleware
	count int64
}

func (m *compressCounterMiddleware) OnCompressContext(ctx context.Context, input middleware.CompressInput, next middleware.CompressHandler) error {
	atomic.AddInt64(&m.count, 1)
	return next(ctx, input)
}

func TestCompressContext_WithMiddleware(t *testing.T) {
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
	}

	mw := &compressCounterMiddleware{
		BaseMiddleware: middleware.BaseMiddleware{MiddlewareKey: "compress-counter"},
	}

	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8}),
		WithMiddlewares(mw),
	)
	agent.state.Context = []*message.Msg{
		message.UserMsg("user", "hello"),
		message.AssistantMsg("bot", "hi"),
	}

	err := agent.compressContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if atomic.LoadInt64(&mw.count) != 1 {
		t.Errorf("OnCompressContext called %d times, want 1", atomic.LoadInt64(&mw.count))
	}
}

func TestCompressContext_ToolResultTruncationInReply(t *testing.T) {
	toolCallResp := model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "call_1",
				Name:  "big_tool",
				Input: `{}`,
				State: message.ToolCallPending,
			},
		},
		IsLast: true,
	}
	finalResp := model.ChatResponse{
		Content: []message.ContentBlock{
			message.TextBlock{Type: "text", ID: "t1", Text: "Done"},
		},
		IsLast: true,
	}

	mock := &mockChatModel{
		responses: []model.ChatResponse{toolCallResp, finalResp},
	}

	bigOutput := strings.Repeat("x", 1000)
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	bigTool := tool.NewFunctionTool("big_tool", "Returns big output", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			return bigOutput, nil
		},
	)

	tk := tool.NewToolkit(bigTool)
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithToolkit(tk),
		WithContextConfig(&ContextConfig{ToolResultLimit: 50}),
	)

	reply, err := agent.Reply(context.Background(), "run the big tool")
	if err != nil {
		t.Fatal(err)
	}
	txt := reply.GetTextContent("\n")
	if txt == nil || *txt != "Done" {
		t.Fatalf("unexpected reply: %v", txt)
	}

	// Check that tool result in context was truncated
	found := false
	for _, m := range agent.state.Context {
		for _, b := range m.GetContentBlocks(message.ContentBlockToolResult) {
			if tr, ok := b.(message.ToolResultBlock); ok && tr.Name == "big_tool" {
				if output, ok := tr.Output.(string); ok {
					if strings.Contains(output, "<<<TRUNCATED:") {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("tool result should have been truncated")
	}
}

func TestContextConfig_TriggerRatioAllowsPointNine(t *testing.T) {
	// Upstream #2396: 0.9 is a legal trigger ratio; only values above 0.9
	// (or <= 0) fall back to the 0.8 default.
	c1 := ContextConfig{TriggerRatio: 0.9}
	if cfg := c1.withDefaults(); cfg.TriggerRatio != 0.9 {
		t.Errorf("TriggerRatio = %v, want 0.9 kept", cfg.TriggerRatio)
	}
	c2 := ContextConfig{TriggerRatio: 0.95}
	if cfg := c2.withDefaults(); cfg.TriggerRatio != 0.8 {
		t.Errorf("TriggerRatio = %v, want default 0.8 for >0.9", cfg.TriggerRatio)
	}
	c3 := ContextConfig{TriggerRatio: 0.5}
	if cfg := c3.withDefaults(); cfg.TriggerRatio != 0.5 {
		t.Errorf("TriggerRatio = %v, want 0.5 kept", cfg.TriggerRatio)
	}
}

func TestCompressContext_SummaryFailureFallsBackToTruncation(t *testing.T) {
	// Upstream #2140: when summary generation fails, compression must fall
	// back to truncating the context to the reserve set while keeping the
	// previous summary — not leave the context wedged above the threshold.
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
		chatErr:     fmt.Errorf("400: tool_choice unsupported"),
	}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{
			TriggerRatio: 0.8,
			ReserveRatio: 0.1,
		}),
	)
	agent.state.Summary = "previous summary"

	for i := 0; i < 10; i++ {
		agent.state.Context = append(agent.state.Context,
			message.UserMsg("user", fmt.Sprintf("message %d", i)),
			message.AssistantMsg("bot", fmt.Sprintf("reply %d", i)),
		)
	}

	if err := agent.compressContext(context.Background()); err != nil {
		t.Fatalf("compression should fall back gracefully, got error: %v", err)
	}
	if agent.state.Summary != "previous summary" {
		t.Errorf("previous summary must be kept, got %q", agent.state.Summary)
	}
	if len(agent.state.Context) >= 20 {
		t.Errorf("context must be truncated, still has %d messages", len(agent.state.Context))
	}
}

func TestCompressContext_FallbackEmptySummaryGetsNotice(t *testing.T) {
	// Upstream #2140: when there is no previous summary, the truncation
	// fallback must install a notice so the model knows history was dropped.
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
		chatErr:     fmt.Errorf("400: tool_choice unsupported"),
	}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
	)
	// No previous summary.
	for i := 0; i < 6; i++ {
		agent.state.Context = append(agent.state.Context,
			message.UserMsg("user", fmt.Sprintf("message %d", i)),
			message.AssistantMsg("bot", fmt.Sprintf("reply %d", i)),
		)
	}
	if err := agent.compressContext(context.Background()); err != nil {
		t.Fatalf("fallback should be graceful, got: %v", err)
	}
	if !strings.Contains(agent.state.Summary, "truncated") {
		t.Errorf("empty summary must be replaced by a truncation notice, got %q", agent.state.Summary)
	}
}

type fakeOffloader struct {
	mu       sync.Mutex
	contents []string
	filename string
}

func (f *fakeOffloader) OffloadContent(_ context.Context, content string, filename string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contents = append(f.contents, content)
	f.filename = filename
	return fmt.Sprintf("/workspace/offloaded/%s", filename), nil
}

func (f *fakeOffloader) OffloadToolResult(_ context.Context, content string, toolCallID string) (string, error) {
	return f.OffloadContent(context.Background(), content, "tool_"+toolCallID+".txt")
}

func TestCompressContext_FallbackOffloadsDroppedMessages(t *testing.T) {
	// Upstream #2140: the truncation fallback must offload the dropped
	// messages even when no previous summary exists, and the reminder must
	// not duplicate across repeated fallbacks.
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
		chatErr:     fmt.Errorf("400: tool_choice unsupported"),
	}
	off := &fakeOffloader{}
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1}),
		WithOffloader(off),
	)
	for i := 0; i < 6; i++ {
		agent.state.Context = append(agent.state.Context,
			message.UserMsg("user", fmt.Sprintf("message %d", i)),
			message.AssistantMsg("bot", fmt.Sprintf("reply %d", i)),
		)
	}

	if err := agent.compressContext(context.Background()); err != nil {
		t.Fatalf("first fallback: %v", err)
	}
	if err := agent.compressContext(context.Background()); err != nil {
		t.Fatalf("second fallback: %v", err)
	}

	off.mu.Lock()
	defer off.mu.Unlock()
	if len(off.contents) == 0 {
		t.Fatal("fallback must offload dropped messages")
	}
	if !strings.Contains(off.contents[0], "message 0") {
		t.Errorf("offloaded content should contain dropped messages, got %.120s", off.contents[0])
	}
	if got := strings.Count(agent.state.Summary, "compressed_context_fallback.json"); got != 1 {
		t.Errorf("offload reminder must appear exactly once across repeated fallbacks, got %d (summary: %q)", got, agent.state.Summary)
	}
	if !strings.Contains(agent.state.Summary, "truncated") {
		t.Errorf("empty summary must still get the truncation notice, got %q", agent.state.Summary)
	}
}
