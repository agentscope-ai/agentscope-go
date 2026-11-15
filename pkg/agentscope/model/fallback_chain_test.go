package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// TestFallbackChain_TriesInOrder proves an ordered chain of >2 models is tried in
// order until one succeeds.
func TestFallbackChain_TriesInOrder(t *testing.T) {
	failing := func(name string) *mockChatModel {
		return &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
			return nil, fmt.Errorf("%s down", name)
		}}
	}
	third := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		return &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "third"}}}, nil
	}}

	fm := NewFallbackChain(failing("first"), failing("second"), third)
	resp, err := fm.Chat(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected success from third model, got %v", err)
	}
	if resp.GetTextContent() != "third" {
		t.Errorf("got %q, want third", resp.GetTextContent())
	}
}

// TestFallbackChain_AllFail returns the last error.
func TestFallbackChain_AllFail(t *testing.T) {
	mk := func(name string) *mockChatModel {
		return &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
			return nil, fmt.Errorf("%s down", name)
		}}
	}
	fm := NewFallbackChain(mk("a"), mk("b"))
	if _, err := fm.Chat(context.Background(), nil); err == nil {
		t.Fatal("expected error when all models fail")
	}
}
