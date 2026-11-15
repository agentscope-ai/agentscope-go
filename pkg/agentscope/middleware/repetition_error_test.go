package middleware

import (
	"context"
	"errors"
	"strings"
	"testing"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func errResp() *tool.ToolResponse {
	return &tool.ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "boom"}},
		State:   message.ToolResultError,
	}
}

// Upstream #1816: the same call failing repeatedly is its own spin shape —
// a hint goes into the system prompt at the threshold and the call after
// the threshold is aborted.
func TestRepetitionErrorStreakHintAndAbort(t *testing.T) {
	m := NewRepetitionBreaker(WithRepetitionErrorThreshold(2))
	call := &ActingInput{AgentName: "a", ToolCall: message.ToolCallBlock{Name: "fetch", Input: `{"u":"x"}`}}
	handler := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
		return errResp(), nil
	}

	// Two failures: responses pass through untouched.
	for i := 0; i < 2; i++ {
		resp, err := m.OnActing(context.Background(), call, handler)
		if err != nil || resp == nil || resp.State != message.ToolResultError {
			t.Fatalf("call %d: resp=%v err=%v, want the error response passed through", i, resp, err)
		}
	}
	// Hint injected at the threshold.
	prompt := m.OnSystemPrompt(context.Background(), "a", "base")
	if !strings.Contains(prompt, "failed repeatedly") {
		t.Errorf("error hint not injected: %q", prompt)
	}
	// The call after the threshold aborts with the typed sentinel.
	_, err := m.OnActing(context.Background(), call, handler)
	if !errors.Is(err, agenterrors.ErrToolRepetition) {
		t.Errorf("abort error = %v, want ErrToolRepetition", err)
	}
}

// A success resets the error streak; a different call resets it too.
func TestRepetitionErrorStreakResets(t *testing.T) {
	m := NewRepetitionBreaker(WithRepetitionErrorThreshold(2))
	call := &ActingInput{AgentName: "a", ToolCall: message.ToolCallBlock{Name: "fetch", Input: `{"u":"x"}`}}
	fail := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) { return errResp(), nil }
	ok := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("fine"), nil
	}

	_, _ = m.OnActing(context.Background(), call, fail)
	_, _ = m.OnActing(context.Background(), call, ok) // success resets
	_, _ = m.OnActing(context.Background(), call, fail)
	if _, err := m.OnActing(context.Background(), call, fail); err != nil {
		t.Errorf("streak should have restarted after success, got abort: %v", err)
	}
	// Now at 2 consecutive again; next aborts.
	if _, err := m.OnActing(context.Background(), call, fail); !errors.Is(err, agenterrors.ErrToolRepetition) {
		t.Errorf("expected abort after threshold, got %v", err)
	}
}

// Handler-level failures (err != nil) feed the same error streak.
func TestRepetitionErrorHandlerErrorsCount(t *testing.T) {
	m := NewRepetitionBreaker(WithRepetitionErrorThreshold(1))
	call := &ActingInput{AgentName: "a", ToolCall: message.ToolCallBlock{Name: "t", Input: "{}"}}
	fail := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
		return nil, errors.New("transport died")
	}
	if _, err := m.OnActing(context.Background(), call, fail); err == nil || err.Error() != "transport died" {
		t.Fatalf("first failure should pass through, got %v", err)
	}
	if _, err := m.OnActing(context.Background(), call, fail); !errors.Is(err, agenterrors.ErrToolRepetition) {
		t.Errorf("second identical failure should abort, got %v", err)
	}
}

// The success-spin dimension still works and stays independent.
func TestRepetitionSuccessStreakUnaffected(t *testing.T) {
	m := NewRepetitionBreaker(WithRepetitionThreshold(2))
	call := &ActingInput{AgentName: "a", ToolCall: message.ToolCallBlock{Name: "t", Input: "{}"}}
	ok := func(_ context.Context, _ *ActingInput) (*tool.ToolResponse, error) {
		return tool.NewTextResponse("fine"), nil
	}
	for i := 0; i < 2; i++ {
		if _, err := m.OnActing(context.Background(), call, ok); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	prompt := m.OnSystemPrompt(context.Background(), "a", "base")
	if !strings.Contains(prompt, "repeated the exact same tool call") {
		t.Errorf("success hint not injected: %q", prompt)
	}
	if _, err := m.OnActing(context.Background(), call, ok); !errors.Is(err, agenterrors.ErrToolRepetition) {
		t.Errorf("expected abort, got %v", err)
	}
}
