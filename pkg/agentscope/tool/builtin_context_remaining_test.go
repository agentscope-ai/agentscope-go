package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestContextRemainingTool_Values(t *testing.T) {
	tool2 := NewContextRemainingTool(func(context.Context) (int64, bool) { return 42000, true })
	resp, err := tool2.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("empty response")
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content block is %T, want TextBlock", resp.Content[0])
	}
	text := tb.Text
	if !strings.Contains(text, `"tokens_left":42000`) {
		t.Errorf("wrong payload: %s", text)
	}
}

func TestContextRemainingTool_Unknown(t *testing.T) {
	tool2 := NewContextRemainingTool(func(context.Context) (int64, bool) { return 0, false })
	resp, _ := tool2.Execute(context.Background(), map[string]any{})
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content block is %T, want TextBlock", resp.Content[0])
	}
	text := tb.Text
	if !strings.Contains(text, `"tokens_left": null`) {
		t.Errorf("unknown budget must render null, got %s", text)
	}
}
