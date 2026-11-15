package middleware

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// GuardrailAction specifies what to do when a guardrail rule triggers.
type GuardrailAction int

const (
	// GuardrailBlock rejects the response entirely, returning an error.
	GuardrailBlock GuardrailAction = iota
	// GuardrailRedact replaces the offending content with a placeholder.
	GuardrailRedact
	// GuardrailWarn allows the response but sets a metadata flag.
	GuardrailWarn
)

// GuardrailRule defines a single content safety check.
type GuardrailRule struct {
	// Name identifies the rule (e.g., "pii_filter", "code_injection").
	Name string
	// Check inspects the response text and returns true if the content
	// violates this rule. The second return value is a human-readable reason.
	Check func(text string) (violated bool, reason string)
	// Action determines what happens when the rule triggers.
	Action GuardrailAction
	// Replacement is the text used when Action == GuardrailRedact.
	// Defaults to "[content filtered]" if empty.
	Replacement string
}

// GuardrailMiddleware intercepts model responses and applies content safety
// rules. It hooks into OnModelCall, inspecting the response text after the
// model returns.
//
// Rules are evaluated in order. On the first Block rule that triggers, the
// response is rejected with ErrGuardrailBlocked. Redact rules modify the
// content in-place. Warn rules set metadata flags without modifying content.
type GuardrailMiddleware struct {
	BaseMiddleware
	rules []GuardrailRule
}

// NewGuardrailMiddleware creates an output guardrail with the given rules.
func NewGuardrailMiddleware(rules ...GuardrailRule) *GuardrailMiddleware {
	return &GuardrailMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "guardrail"},
		rules:          rules,
	}
}

// OnModelCall inspects model responses against all guardrail rules.
func (m *GuardrailMiddleware) OnModelCall(
	ctx context.Context,
	input *ModelCallInput,
	next ModelCallHandler,
) (*model.ChatResponse, error) {
	resp, err := next(ctx, input)
	if err != nil || resp == nil {
		return resp, err
	}

	text := extractResponseText(resp)
	if text == "" {
		return resp, nil
	}

	for _, rule := range m.rules {
		violated, reason := rule.Check(text)
		if !violated {
			continue
		}

		switch rule.Action {
		case GuardrailBlock:
			return nil, &agenterrors.AgentError{
				Category: agenterrors.CategoryPermission,
				Code:     "guardrail.blocked",
				Message:  fmt.Sprintf("guardrail %q blocked response: %s", rule.Name, reason),
				AgentMsg: fmt.Sprintf("Your response was blocked by safety filter %q: %s. Please rephrase.", rule.Name, reason),
			}

		case GuardrailRedact:
			replacement := rule.Replacement
			if replacement == "" {
				replacement = "[content filtered]"
			}
			resp.Content = []message.ContentBlock{
				message.TextBlock{Text: replacement},
			}
			if resp.Metadata == nil {
				resp.Metadata = make(map[string]any)
			}
			resp.Metadata["guardrail_redacted"] = rule.Name
			resp.Metadata["guardrail_reason"] = reason
			// After redaction, stop checking further rules
			return resp, nil

		case GuardrailWarn:
			if resp.Metadata == nil {
				resp.Metadata = make(map[string]any)
			}
			warnings, _ := resp.Metadata["guardrail_warnings"].([]string)
			warnings = append(warnings, fmt.Sprintf("%s: %s", rule.Name, reason))
			resp.Metadata["guardrail_warnings"] = warnings
		}
	}

	return resp, nil
}

// extractResponseText concatenates all text blocks in a ChatResponse.
func extractResponseText(resp *model.ChatResponse) string {
	var sb strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.(message.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// ---------- Built-in guardrail rules ----------

// KeywordBlockRule blocks responses containing any of the given keywords
// (case-insensitive).
func KeywordBlockRule(name string, keywords ...string) GuardrailRule {
	return GuardrailRule{
		Name:   name,
		Action: GuardrailBlock,
		Check: func(text string) (bool, string) {
			lower := strings.ToLower(text)
			for _, kw := range keywords {
				if strings.Contains(lower, strings.ToLower(kw)) {
					return true, fmt.Sprintf("contains prohibited keyword %q", kw)
				}
			}
			return false, ""
		},
	}
}

// KeywordRedactRule redacts responses containing any of the given keywords.
func KeywordRedactRule(name string, replacement string, keywords ...string) GuardrailRule {
	r := KeywordBlockRule(name, keywords...)
	r.Action = GuardrailRedact
	r.Replacement = replacement
	return r
}

// MaxLengthRule blocks or redacts responses exceeding a characters (runes) limit.
func MaxLengthRule(name string, maxChars int, action GuardrailAction) GuardrailRule {
	return GuardrailRule{
		Name:   name,
		Action: action,
		Check: func(text string) (bool, string) {
			n := utf8.RuneCountInString(text)
			if n > maxChars {
				return true, fmt.Sprintf("response length %d exceeds limit %d", n, maxChars)
			}
			return false, ""
		},
	}
}

// RegexRule checks response text against a custom matching function.
// This is a general-purpose rule constructor for patterns that don't
// fit the keyword or length models (e.g., regex, PII detection).
func CustomRule(name string, action GuardrailAction, check func(string) (bool, string)) GuardrailRule {
	return GuardrailRule{
		Name:   name,
		Action: action,
		Check:  check,
	}
}
