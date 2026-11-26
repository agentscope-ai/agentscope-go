package model

import (
	"context"
	"fmt"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
)

// PolicyPhase identifies which side of a request was blocked.
type PolicyPhase string

const (
	PolicyPhaseInput   PolicyPhase = "input"
	PolicyPhaseOutput  PolicyPhase = "output"
	PolicyPhaseUnknown PolicyPhase = "unknown"
)

// ErrModelContentPolicy identifies content policy errors through errors.Is.
var ErrModelContentPolicy = &agenterrors.AgentError{Category: agenterrors.CategoryModel, Code: "model.content_policy", Message: "model content policy"}

// ContentPolicyError describes an explicit provider content policy signal.
type ContentPolicyError struct {
	*agenterrors.AgentError
	Phase          PolicyPhase
	Provider       string
	OriginalCode   string
	RequestID      string
	PartialOutput  bool
	RequiresReview bool
}

// Unwrap exposes the structured error and its original cause.
func (e *ContentPolicyError) Unwrap() error { return e.AgentError }

// RetrySamePayload reports whether an output may be sampled again.
func (e *ContentPolicyError) RetrySamePayload() bool {
	return e.Phase == PolicyPhaseOutput && !e.PartialOutput && !e.RequiresReview
}

// AllowFailover reports whether the policy permits trying another model.
func (e *ContentPolicyError) AllowFailover() bool { return !e.PartialOutput && !e.RequiresReview }

func classifyContentPolicy(provider, code, id string, partial bool, cause error) *ContentPolicyError {
	blocked := provider == "openai" && code == "content_filter" || provider == "anthropic" && code == "refusal" || provider == "gemini" && (code == "SAFETY" || code == "RECITATION" || code == "BLOCKLIST" || code == "PROHIBITED_CONTENT" || code == "SPII" || code == "IMAGE_SAFETY")
	if !blocked {
		return nil
	}
	return &ContentPolicyError{AgentError: &agenterrors.AgentError{Category: agenterrors.CategoryModel, Code: ErrModelContentPolicy.Code, Message: fmt.Sprintf("%s: content policy (%s)", provider, code), Cause: cause}, Phase: PolicyPhaseOutput, Provider: provider, OriginalCode: code, RequestID: id, PartialOutput: partial}
}

// observeContentPolicy preserves the provider stream and classifies its terminal
// response. PartialOutput counts chunks successfully sent to the caller.
func observeContentPolicy(ctx context.Context, cancel context.CancelFunc, input <-chan ChatResponse, reason <-chan string, provider string) <-chan ChatResponse {
	out := make(chan ChatResponse, 16)
	go func() {
		defer close(out)
		defer cancel()
		partial := false
		for resp := range input {
			if resp.IsLast {
				select {
				case code := <-reason:
					if e := classifyContentPolicy(provider, code, "", partial, resp.Error); e != nil {
						resp.Error = e
					}
				case <-ctx.Done():
					return
				}
			}
			select {
			case out <- resp:
				if !resp.IsLast && len(resp.Content) != 0 {
					partial = true
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
