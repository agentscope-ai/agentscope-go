// pkg/agentscope/errors/errors_test.go
package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestAgentErrorImplementsError(t *testing.T) {
	var err error = &AgentError{
		Category: CategoryModel,
		Code:     "model.rate_limited",
		Message:  "rate limited",
	}
	if err.Error() != "rate limited" {
		t.Errorf("Error() = %q, want %q", err.Error(), "rate limited")
	}
}

func TestAgentErrorUnwrap(t *testing.T) {
	cause := fmt.Errorf("connection refused")
	err := &AgentError{
		Category: CategoryNetwork,
		Code:     "network.refused",
		Message:  "model API unreachable",
		Cause:    cause,
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is should find the cause via Unwrap")
	}
}

func TestAgentErrorAs(t *testing.T) {
	cause := &AgentError{
		Category:  CategoryModel,
		Code:      "model.rate_limited",
		Message:   "rate limited",
		Retryable: true,
	}
	wrapped := fmt.Errorf("agent failed: %w", cause)

	var ae *AgentError
	if !errors.As(wrapped, &ae) {
		t.Fatal("errors.As should find AgentError")
	}
	if ae.Category != CategoryModel {
		t.Errorf("Category = %v, want CategoryModel", ae.Category)
	}
	if !ae.Retryable {
		t.Error("Retryable should be true")
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := &AgentError{Retryable: true, Message: "retry me"}
	notRetryable := &AgentError{Retryable: false, Message: "no retry"}
	plainErr := fmt.Errorf("plain error")

	if !IsRetryable(retryable) {
		t.Error("should be retryable")
	}
	if IsRetryable(notRetryable) {
		t.Error("should not be retryable")
	}
	if IsRetryable(plainErr) {
		t.Error("plain error should not be retryable")
	}
	if IsRetryable(fmt.Errorf("wrapped: %w", retryable)) != true {
		t.Error("wrapped retryable should still be retryable")
	}
}

func TestCategoryString(t *testing.T) {
	cases := []struct {
		cat  Category
		want string
	}{
		{CategoryModel, "model"},
		{CategoryTool, "tool"},
		{CategoryPermission, "permission"},
		{CategoryContext, "context"},
		{CategoryConfig, "config"},
		{CategoryPlatform, "platform"},
		{CategoryNetwork, "network"},
	}
	for _, tc := range cases {
		if got := tc.cat.String(); got != tc.want {
			t.Errorf("Category(%d).String() = %q, want %q", tc.cat, got, tc.want)
		}
	}
}

func TestNewf(t *testing.T) {
	err := Newf(CategoryTool, "tool.timeout", "tool %q timed out after %ds", "bash", 30)
	if err.Code != "tool.timeout" {
		t.Errorf("Code = %q, want %q", err.Code, "tool.timeout")
	}
	want := `tool "bash" timed out after 30s`
	if err.Message != want {
		t.Errorf("Message = %q, want %q", err.Message, want)
	}
}

func TestWrap(t *testing.T) {
	cause := fmt.Errorf("connection reset")
	err := Wrap(cause, CategoryNetwork, "network.reset", "API call failed")
	if err.Cause != cause {
		t.Error("Cause should be the original error")
	}
	if !errors.Is(err, cause) {
		t.Error("should unwrap to cause")
	}
}

func TestAgentError_Is(t *testing.T) {
	err := &AgentError{
		Category: CategoryResource,
		Code:     "budget.exceeded",
		Message:  "spend cap reached: $1.00 >= $0.50",
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Error("errors.Is should match sentinel by Code")
	}
	if errors.Is(err, ErrToolDenied) {
		t.Error("errors.Is should not match a different sentinel Code")
	}
}

func TestAgentError_Is_Wrapped(t *testing.T) {
	inner := &AgentError{
		Category: CategoryModel,
		Code:     "model.rate_limited",
		Message:  "throttled",
	}
	wrapped := fmt.Errorf("outer: %w", inner)
	if !errors.Is(wrapped, ErrModelRateLimited) {
		t.Error("errors.Is should find AgentError through fmt.Errorf(%%w)")
	}
}

func TestAgentMessage(t *testing.T) {
	// When AgentMsg is set, prefer it.
	err := &AgentError{
		Code:     "guardrail.blocked",
		Message:  "operator message",
		AgentMsg: "LLM-facing message",
	}
	if got := err.AgentMessage(); got != "LLM-facing message" {
		t.Errorf("AgentMessage() = %q, want LLM-facing message", got)
	}

	// When AgentMsg is empty, fall back to Message.
	err2 := &AgentError{
		Code:    "tool.timeout",
		Message: "tool timed out",
	}
	if got := err2.AgentMessage(); got != "tool timed out" {
		t.Errorf("AgentMessage() = %q, want %q", got, "tool timed out")
	}
}

func TestGetAgentMessage_Nil(t *testing.T) {
	got := GetAgentMessage(nil)
	if got != "" {
		t.Errorf("GetAgentMessage(nil) = %q, want empty string", got)
	}
}

func TestIsAgentError_Nil(t *testing.T) {
	if IsAgentError(nil) {
		t.Error("IsAgentError(nil) should return false")
	}
}
