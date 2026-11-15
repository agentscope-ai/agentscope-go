package middleware

import (
	"context"
	"errors"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func makeResp(text string) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Text: text}},
		IsLast:  true,
	}
}

func passThroughHandler(resp *model.ChatResponse) ModelCallHandler {
	return func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return resp, nil
	}
}

func TestGuardrail_Block(t *testing.T) {
	gm := NewGuardrailMiddleware(
		KeywordBlockRule("profanity", "badword", "offensive"),
	)

	// Clean content passes
	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("This is fine.")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}

	// Content with blocked keyword
	_, err = gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("This contains a badword.")))
	if err == nil {
		t.Fatal("expected error for blocked content")
	}
	var ae *agenterrors.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AgentError, got %T", err)
	}
	if ae.Code != "guardrail.blocked" {
		t.Errorf("expected code guardrail.blocked, got %s", ae.Code)
	}
}

func TestGuardrail_Redact(t *testing.T) {
	gm := NewGuardrailMiddleware(
		KeywordRedactRule("secrets", "[REDACTED]", "password", "secret"),
	)

	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("The password is 12345.")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := extractResponseText(resp)
	if text != "[REDACTED]" {
		t.Errorf("expected [REDACTED], got %q", text)
	}
	if resp.Metadata["guardrail_redacted"] != "secrets" {
		t.Error("expected guardrail_redacted metadata")
	}
}

func TestGuardrail_Warn(t *testing.T) {
	gm := NewGuardrailMiddleware(
		CustomRule("length_warn", GuardrailWarn, func(text string) (bool, string) {
			if len(text) > 10 {
				return true, "long response"
			}
			return false, ""
		}),
	)

	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("This is a longer response that exceeds ten characters.")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	warnings, ok := resp.Metadata["guardrail_warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatal("expected guardrail_warnings metadata")
	}
	if warnings[0] != "length_warn: long response" {
		t.Errorf("unexpected warning: %s", warnings[0])
	}
}

func TestGuardrail_MaxLength(t *testing.T) {
	gm := NewGuardrailMiddleware(
		MaxLengthRule("max_len", 20, GuardrailBlock),
	)

	_, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("This response is way too long and should be blocked.")))
	if err == nil {
		t.Fatal("expected error for max length violation")
	}
}

func TestGuardrail_MultipleRules(t *testing.T) {
	gm := NewGuardrailMiddleware(
		CustomRule("warn_first", GuardrailWarn, func(text string) (bool, string) {
			return true, "always warns"
		}),
		KeywordBlockRule("block_second", "danger"),
	)

	// Warn fires, then block fires
	_, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("There is danger ahead.")))
	if err == nil {
		t.Fatal("expected block error despite prior warn")
	}
}

func TestGuardrail_MaxLength_Unicode(t *testing.T) {
	// MaxLengthRule counts runes, not bytes. "你好世界" is 4 runes, 12 bytes.
	gm := NewGuardrailMiddleware(
		MaxLengthRule("max_len", 5, GuardrailBlock),
	)

	// 4 CJK characters → 4 runes ≤ 5 → should pass.
	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("你好世界")))
	if err != nil {
		t.Fatalf("unexpected error for 4-rune string: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}

	// 6 CJK characters → 6 runes > 5 → should block.
	_, err = gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("你好世界再见")))
	if err == nil {
		t.Fatal("expected error for 6-rune string exceeding limit of 5")
	}
}

func TestGuardrail_NilResponse(t *testing.T) {
	gm := NewGuardrailMiddleware(
		KeywordBlockRule("test", "forbidden"),
	)

	nilHandler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return nil, nil
	}

	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{}, nilHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Error("expected nil response")
	}
}

func TestGuardrail_ErrorPassThrough(t *testing.T) {
	gm := NewGuardrailMiddleware(
		KeywordBlockRule("test", "forbidden"),
	)

	errHandler := func(_ context.Context, _ *ModelCallInput) (*model.ChatResponse, error) {
		return nil, errors.New("upstream error")
	}

	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{}, errHandler)
	if err == nil || err.Error() != "upstream error" {
		t.Fatalf("expected upstream error, got %v", err)
	}
	if resp != nil {
		t.Error("expected nil response on error")
	}
}

func TestGuardrail_EmptyRules(t *testing.T) {
	gm := NewGuardrailMiddleware() // no rules

	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("anything goes")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if extractResponseText(resp) != "anything goes" {
		t.Error("response should pass through unchanged with no rules")
	}
}

func TestGuardrail_NonTextContent(t *testing.T) {
	gm := NewGuardrailMiddleware(
		KeywordBlockRule("test", "forbidden"),
	)

	// Response with only a ToolResultBlock (no text blocks).
	resp := &model.ChatResponse{
		Content: []message.ContentBlock{
			message.ToolResultBlock{
				Type:   "tool_result",
				ID:     "t1",
				Name:   "bash",
				Output: "forbidden content here",
			},
		},
		IsLast: true,
	}

	got, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(resp))
	if err != nil {
		t.Fatalf("unexpected error: non-text content should bypass guardrails, got %v", err)
	}
	if got == nil {
		t.Fatal("expected response")
	}
}

func TestGuardrail_CleanPassThrough(t *testing.T) {
	gm := NewGuardrailMiddleware(
		KeywordBlockRule("test", "forbidden"),
	)

	resp, err := gm.OnModelCall(context.Background(), &ModelCallInput{},
		passThroughHandler(makeResp("Perfectly safe content.")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if extractResponseText(resp) != "Perfectly safe content." {
		t.Error("content should pass through unchanged")
	}
}
