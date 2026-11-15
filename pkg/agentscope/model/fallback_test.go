package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

type mockChatModel struct {
	chatFn func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error)
}

func (m *mockChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	return m.chatFn(ctx, msgs, opts...)
}

func (m *mockChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	return nil, ErrStreamNotSupported
}

func (m *mockChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return 0
}

func TestFallbackChatModel_PrimarySucceeds(t *testing.T) {
	primary := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		return &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "primary"}}}, nil
	}}
	fallback := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		return &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "fallback"}}}, nil
	}}

	fm := NewFallbackChatModel(primary, fallback)
	resp, err := fm.Chat(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTextContent() != "primary" {
		t.Errorf("got %q, want primary", resp.GetTextContent())
	}
}

func TestFallbackChatModel_PrimaryFails(t *testing.T) {
	primary := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		return nil, fmt.Errorf("primary down")
	}}
	fallback := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		return &ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "fallback"}}}, nil
	}}

	fm := NewFallbackChatModel(primary, fallback)
	resp, err := fm.Chat(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTextContent() != "fallback" {
		t.Errorf("got %q, want fallback", resp.GetTextContent())
	}
}

func TestFallbackChatModel_ContextCancelled(t *testing.T) {
	primary := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		return nil, context.Canceled
	}}
	fallback := &mockChatModel{chatFn: func(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
		t.Fatal("fallback should not be called on context cancel")
		return nil, nil
	}}

	fm := NewFallbackChatModel(primary, fallback)
	_, err := fm.Chat(context.Background(), nil)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
