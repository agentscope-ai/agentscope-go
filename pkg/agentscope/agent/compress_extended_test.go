package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// --- TruncateToolResultBlocks tests ---

func TestTruncateToolResultBlocks_TextExceedsLimit(t *testing.T) {
	// A single text block that exceeds the token limit should be truncated.
	longText := strings.Repeat("abcd", 1000) // 4000 chars = ~1000 tokens
	blocks := []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "tb1", Text: longText},
	}

	// Set a limit of 100 tokens (= 400 chars).
	result := TruncateToolResultBlocks(blocks, 100)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}

	tb, ok := result[0].(message.TextBlock)
	if !ok {
		t.Fatal("expected TextBlock")
	}
	if !strings.HasSuffix(tb.Text, "<<<TRUNCATED>>>") {
		t.Errorf("truncated text should end with <<<TRUNCATED>>>, got suffix: %q",
			tb.Text[len(tb.Text)-min(50, len(tb.Text)):])
	}
	// The character limit for 100 tokens is 400 chars. Truncated text = 400 chars + "\n<<<TRUNCATED>>>".
	expectedLen := 400 + len("\n<<<TRUNCATED>>>")
	if len(tb.Text) != expectedLen {
		t.Errorf("truncated text length = %d, want %d", len(tb.Text), expectedLen)
	}
}

func TestTruncateToolResultBlocks_TextUnderLimit(t *testing.T) {
	// A text block under the limit should not be modified.
	shortText := "Hello, world!"
	blocks := []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "tb1", Text: shortText},
	}

	result := TruncateToolResultBlocks(blocks, 100)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}

	tb, ok := result[0].(message.TextBlock)
	if !ok {
		t.Fatal("expected TextBlock")
	}
	if tb.Text != shortText {
		t.Errorf("text should not be modified, got %q", tb.Text)
	}
}

func TestTruncateToolResultBlocks_MultipleTextBlocks(t *testing.T) {
	// Multiple text blocks where total tokens exceed limit.
	longText1 := strings.Repeat("x", 2000) // 500 tokens
	longText2 := strings.Repeat("y", 2000) // 500 tokens
	blocks := []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "tb1", Text: longText1},
		message.TextBlock{Type: "text", ID: "tb2", Text: longText2},
	}

	// Limit of 200 tokens = 800 chars per block
	result := TruncateToolResultBlocks(blocks, 200)
	if len(result) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(result))
	}

	for i, b := range result {
		tb, ok := b.(message.TextBlock)
		if !ok {
			t.Fatalf("block %d: expected TextBlock", i)
		}
		if !strings.HasSuffix(tb.Text, "<<<TRUNCATED>>>") {
			t.Errorf("block %d: expected truncation marker", i)
		}
	}
}

func TestTruncateToolResultBlocks_DataBlockBase64Replacement(t *testing.T) {
	// A DataBlock with Base64Source larger than 1000 chars should have its data
	// replaced with an empty string.
	largeBase64 := strings.Repeat("A", 2000) // > 1000 chars
	blocks := []message.ContentBlock{
		message.DataBlock{
			Type: "data",
			ID:   "db1",
			Source: message.Base64Source{
				Type:      "base64",
				Data:      largeBase64,
				MediaType: "image/png",
			},
		},
	}

	// The total tokens from the base64 data: 2000 * 3/16 = 375 tokens.
	// Set limit below that to trigger truncation.
	result := TruncateToolResultBlocks(blocks, 100)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}

	db, ok := result[0].(message.DataBlock)
	if !ok {
		t.Fatal("expected DataBlock")
	}

	src, ok := db.Source.(message.Base64Source)
	if !ok {
		t.Fatal("expected Base64Source")
	}
	if src.Data != "" {
		t.Errorf("base64 data should be replaced with empty string, got len=%d", len(src.Data))
	}
	if src.MediaType != "image/png" {
		t.Errorf("media type should be preserved, got %q", src.MediaType)
	}
}

func TestTruncateToolResultBlocks_SmallBase64Preserved(t *testing.T) {
	// A DataBlock with small Base64Source (<= 1000 chars) should be preserved.
	smallBase64 := strings.Repeat("A", 500) // <= 1000 chars
	blocks := []message.ContentBlock{
		message.DataBlock{
			Type: "data",
			ID:   "db1",
			Source: message.Base64Source{
				Type:      "base64",
				Data:      smallBase64,
				MediaType: "image/png",
			},
		},
		// Add a large text block to force total tokens over limit
		message.TextBlock{Type: "text", ID: "tb1", Text: strings.Repeat("x", 2000)},
	}

	result := TruncateToolResultBlocks(blocks, 100)
	if len(result) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(result))
	}

	db, ok := result[0].(message.DataBlock)
	if !ok {
		t.Fatal("expected DataBlock as first block")
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok {
		t.Fatal("expected Base64Source")
	}
	if src.Data != smallBase64 {
		t.Errorf("small base64 data should be preserved, got len=%d", len(src.Data))
	}
}

func TestTruncateToolResultBlocks_MixedBlocks(t *testing.T) {
	// Mix of TextBlock, DataBlock, and other block types.
	largeBase64 := strings.Repeat("B", 2000)
	blocks := []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "tb1", Text: strings.Repeat("z", 2000)},
		message.DataBlock{
			Type: "data",
			ID:   "db1",
			Source: message.Base64Source{
				Type:      "base64",
				Data:      largeBase64,
				MediaType: "audio/wav",
			},
		},
		message.HintBlock{Type: "hint", ID: "h1", Hint: "some hint"},
	}

	result := TruncateToolResultBlocks(blocks, 100)
	if len(result) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(result))
	}

	// Text block should be truncated
	tb, ok := result[0].(message.TextBlock)
	if !ok {
		t.Fatal("block 0: expected TextBlock")
	}
	if !strings.HasSuffix(tb.Text, "<<<TRUNCATED>>>") {
		t.Error("text block should be truncated")
	}

	// DataBlock should have data replaced
	db, ok := result[1].(message.DataBlock)
	if !ok {
		t.Fatal("block 1: expected DataBlock")
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok {
		t.Fatal("expected Base64Source")
	}
	if src.Data != "" {
		t.Error("large base64 data should be replaced with empty string")
	}

	// HintBlock should pass through unchanged
	hb, ok := result[2].(message.HintBlock)
	if !ok {
		t.Fatal("block 2: expected HintBlock")
	}
	if hb.GetHintText() != "some hint" {
		t.Errorf("hint block should be unchanged, got %q", hb.GetHintText())
	}
}

// --- splitMessageAtBlock tests ---

func TestSplitMessageAtBlock_MiddleSplit(t *testing.T) {
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
		message.TextBlock{Type: "text", ID: "t2", Text: "second"},
		message.TextBlock{Type: "text", ID: "t3", Text: "third"},
	})

	first, second := splitMessageAtBlock(msg, 1)
	if first == nil {
		t.Fatal("first half should not be nil")
	}
	if second == nil {
		t.Fatal("second half should not be nil for middle split")
	}

	if len(first.Content) != 1 {
		t.Errorf("first half should have 1 block, got %d", len(first.Content))
	}
	if len(second.Content) != 2 {
		t.Errorf("second half should have 2 blocks, got %d", len(second.Content))
	}

	// Verify content
	if tb, ok := first.Content[0].(message.TextBlock); !ok || tb.Text != "first" {
		t.Error("first half should contain 'first'")
	}
	if tb, ok := second.Content[0].(message.TextBlock); !ok || tb.Text != "second" {
		t.Error("second half first block should be 'second'")
	}
	if tb, ok := second.Content[1].(message.TextBlock); !ok || tb.Text != "third" {
		t.Error("second half second block should be 'third'")
	}
}

func TestSplitMessageAtBlock_AtZero(t *testing.T) {
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
		message.TextBlock{Type: "text", ID: "t2", Text: "second"},
	})

	first, second := splitMessageAtBlock(msg, 0)
	if first == nil {
		t.Fatal("first should not be nil")
	}
	if second != nil {
		t.Error("splitting at index 0 should return nil for second")
	}
	// The original message is returned as-is for out-of-range blockIdx
	if len(first.Content) != 2 {
		t.Errorf("expected original message with 2 blocks, got %d", len(first.Content))
	}
}

func TestSplitMessageAtBlock_AtEnd(t *testing.T) {
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
		message.TextBlock{Type: "text", ID: "t2", Text: "second"},
	})

	first, second := splitMessageAtBlock(msg, 2)
	if first == nil {
		t.Fatal("first should not be nil")
	}
	if second != nil {
		t.Error("splitting at end (blockIdx >= len) should return nil for second")
	}
	if len(first.Content) != 2 {
		t.Errorf("expected original message with 2 blocks, got %d", len(first.Content))
	}
}

func TestSplitMessageAtBlock_LastBlock(t *testing.T) {
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
		message.TextBlock{Type: "text", ID: "t2", Text: "second"},
		message.TextBlock{Type: "text", ID: "t3", Text: "third"},
	})

	first, second := splitMessageAtBlock(msg, 2)
	if first == nil {
		t.Fatal("first should not be nil")
	}
	if second == nil {
		t.Fatal("splitting at last valid index should return non-nil second")
	}

	if len(first.Content) != 2 {
		t.Errorf("first half should have 2 blocks, got %d", len(first.Content))
	}
	if len(second.Content) != 1 {
		t.Errorf("second half should have 1 block, got %d", len(second.Content))
	}
}

func TestSplitMessageAtBlock_NegativeIndex(t *testing.T) {
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
	})

	first, second := splitMessageAtBlock(msg, -1)
	if first == nil {
		t.Fatal("first should not be nil")
	}
	if second != nil {
		t.Error("negative index should return nil for second")
	}
}

func TestSplitMessageAtBlock_PreservesRole(t *testing.T) {
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
		message.TextBlock{Type: "text", ID: "t2", Text: "second"},
	})

	first, second := splitMessageAtBlock(msg, 1)
	if first.Role != message.RoleAssistant {
		t.Errorf("first half role = %s, want assistant", first.Role)
	}
	if second.Role != message.RoleAssistant {
		t.Errorf("second half role = %s, want assistant", second.Role)
	}
	if first.Name != "bot" {
		t.Errorf("first half name = %s, want bot", first.Name)
	}
	if second.Name != "bot" {
		t.Errorf("second half name = %s, want bot", second.Name)
	}
}

func TestSplitMessageAtBlock_IndependentSlices(t *testing.T) {
	// Verify that modifying one half doesn't affect the other.
	msg := message.NewMsg("bot", message.RoleAssistant, []message.ContentBlock{
		message.TextBlock{Type: "text", ID: "t1", Text: "first"},
		message.TextBlock{Type: "text", ID: "t2", Text: "second"},
		message.TextBlock{Type: "text", ID: "t3", Text: "third"},
	})

	first, second := splitMessageAtBlock(msg, 2)
	// Modify first half's slice
	first.Content[0] = message.TextBlock{Type: "text", ID: "t1", Text: "modified"}

	// Second half should not be affected
	if tb, ok := second.Content[0].(message.TextBlock); !ok || tb.Text != "third" {
		t.Error("modifying first half should not affect second half")
	}
	// Original should not be affected
	if tb, ok := msg.Content[0].(message.TextBlock); !ok || tb.Text != "first" {
		t.Error("modifying first half should not affect original message")
	}
}

// --- Offloader integration in compression ---

// mockOffloader records offload calls and returns a predictable path.
type mockOffloader struct {
	offloadedContent string
	offloadedName    string
	callCount        int
}

func (o *mockOffloader) OffloadContent(_ context.Context, content string, filename string) (string, error) {
	o.offloadedContent = content
	o.offloadedName = filename
	o.callCount++
	return "/workspace/" + filename, nil
}

func (o *mockOffloader) OffloadToolResult(_ context.Context, content string, toolCallID string) (string, error) {
	filename := "tool_result_" + toolCallID + ".txt"
	o.offloadedContent = content
	o.offloadedName = filename
	o.callCount++
	return "/workspace/" + filename, nil
}

func TestCompressContext_OffloaderIntegration(t *testing.T) {
	// When an offloader is set, compression should offload the summary.
	mock := &compressionMockModel{
		tokenCount:  30000,
		contextSize: 32000,
	}

	offloader := &mockOffloader{}

	agent := NewUnifiedAgent("test", "prompt", mock,
		WithContextConfig(&ContextConfig{
			TriggerRatio: 0.8,
			ReserveRatio: 0.1,
		}),
		WithOffloader(offloader),
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

	if offloader.callCount != 1 {
		t.Errorf("offloader should be called once, got %d", offloader.callCount)
	}
	if offloader.offloadedName != "compressed_context.txt" {
		t.Errorf("offloaded filename = %q, want 'compressed_context.txt'", offloader.offloadedName)
	}
	if !strings.Contains(agent.state.Summary, "compressed_context.txt") {
		t.Error("summary should contain offloaded path reference")
	}
	if !strings.Contains(agent.state.Summary, "system-reminder") {
		t.Error("summary should contain system-reminder tag about offloaded content")
	}
}

func TestCompressContext_ToolResultTruncationWithOffloader(t *testing.T) {
	// When tool result exceeds limit and offloader is set, result should be
	// truncated and offloaded path appended.
	toolCallResp := model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    "call_big",
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

	bigOutput := strings.Repeat("x", 2000)
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	bigTool := tool.NewFunctionTool("big_tool", "Returns big output", schema,
		func(ctx context.Context, input map[string]any) (any, error) {
			return bigOutput, nil
		},
	)

	offloader := &mockOffloader{}

	tk := tool.NewToolkit(bigTool)
	agent := NewUnifiedAgent("test", "prompt", mock,
		WithToolkit(tk),
		WithContextConfig(&ContextConfig{ToolResultLimit: 50}),
		WithOffloader(offloader),
	)

	reply, err := agent.Reply(context.Background(), "run big tool")
	if err != nil {
		t.Fatal(err)
	}
	txt := reply.GetTextContent("\n")
	if txt == nil || *txt != "Done" {
		t.Fatalf("unexpected reply: %v", txt)
	}

	if offloader.callCount != 1 {
		t.Errorf("offloader should be called once for truncated tool result, got %d", offloader.callCount)
	}

	// Check the tool result in context has both truncation marker and offload reference
	for _, m := range agent.state.Context {
		for _, b := range m.GetContentBlocks(message.ContentBlockToolResult) {
			if tr, ok := b.(message.ToolResultBlock); ok && tr.Name == "big_tool" {
				output, ok := tr.Output.(string)
				if !ok {
					t.Fatal("expected string output")
				}
				if !strings.Contains(output, "<<<TRUNCATED:") {
					t.Error("output should contain truncation marker")
				}
				if !strings.Contains(output, "offloaded") {
					t.Error("output should reference offloaded content")
				}
			}
		}
	}
}

func TestCompressContext_NoOffloader_NoOffloadReference(t *testing.T) {
	// Without an offloader, compression should produce a summary without
	// offload references.
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
		t.Error("model should have been called for compression")
	}
	if strings.Contains(agent.state.Summary, "offloaded") {
		t.Error("summary should not contain offload references when no offloader is set")
	}
}

// min returns the smaller of a and b.
// Included here for Go 1.22 compatibility (builtin min is 1.21+).
