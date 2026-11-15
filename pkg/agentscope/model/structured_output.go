package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/jsonx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

const structuredOutputToolName = "generate_structured_output"

// ThinkingDisabler is implemented by models whose provider exposes a
// thinking/reasoning toggle. GenerateStructuredOutput uses it for the
// "no_think" fallback strategy (upstream #2140).
type ThinkingDisabler interface {
	DisableThinkingOptions() []CallOption
}

// StructuredFallbackClassifier is implemented by models that can identify
// their own request-shape rejections (e.g. a provider bad-request type).
// When present it replaces the generic marker heuristic, mirroring Python's
// per-provider _get_structured_output_fallback_exceptions (upstream #2140).
type StructuredFallbackClassifier interface {
	IsStructuredOutputFallbackError(err error) bool
}

// soStrategy is one rung of the structured-output fallback ladder.
type soStrategy struct {
	name    string
	choice  *ToolChoice // nil = no tool_choice ("none" strategy)
	noThink bool        // overlay DisableThinkingOptions
}

// GenerateStructuredOutput uses a model to produce JSON conforming to the
// given schema, trying fallback strategies in order (upstream #2140):
//
//  1. forced   — synthetic tool + tool_choice=required
//  2. auto     — synthetic tool + tool_choice=auto
//  3. no_think — thinking disabled + tool_choice=required (only when the
//     model implements ThinkingDisabler)
//  4. none     — no tool_choice; JSON is extracted from a tool call if the
//     model still emits one, otherwise parsed out of the text response
//
// An error classified as a structured-output compatibility failure (the
// model did not produce valid structured output, or the provider rejected
// the request shape) advances the ladder; any other error is returned
// immediately. When every strategy fails, the returned error wraps
// errors.ErrStructuredOutput.
func GenerateStructuredOutput(ctx context.Context, model ChatModel, msgs []*message.Msg, schema json.RawMessage) (json.RawMessage, error) {
	result, _, err := GenerateStructuredOutputWithUsage(ctx, model, msgs, schema)
	return result, err
}

// GenerateStructuredOutputWithUsage is GenerateStructuredOutput plus the
// token usage accumulated across EVERY strategy attempt (upstream #2433:
// compression calls burn tokens and must be accounted even when an early
// strategy is rejected and a later one succeeds).
func GenerateStructuredOutputWithUsage(ctx context.Context, model ChatModel, msgs []*message.Msg, schema json.RawMessage) (json.RawMessage, *ChatUsage, error) {
	var totalUsage *ChatUsage
	addUsage := func(u *ChatUsage) {
		if u == nil {
			return
		}
		if totalUsage == nil {
			cp := *u
			totalUsage = &cp
			return
		}
		totalUsage.InputTokens += u.InputTokens
		totalUsage.OutputTokens += u.OutputTokens
		totalUsage.CacheCreationInputTokens += u.CacheCreationInputTokens
		totalUsage.CacheInputTokens += u.CacheInputTokens
	}
	tool := ToolSchema{
		Type: "function",
		Function: ToolFunction{
			Name:        structuredOutputToolName,
			Description: "Generate a structured JSON output conforming to the provided schema. You MUST call this tool with the result.",
			Parameters:  schema,
		},
	}

	strategies := []soStrategy{
		{"forced", &ToolChoice{Mode: "required"}, false},
		{"auto", &ToolChoice{Mode: "auto"}, false},
	}
	disabler, canDisable := model.(ThinkingDisabler)
	if canDisable {
		strategies = append(strategies, soStrategy{"no_think", &ToolChoice{Mode: "required"}, true})
	}
	strategies = append(strategies, soStrategy{"none", nil, false})

	var firstErr error
	for _, st := range strategies {
		opts := []CallOption{WithTools([]ToolSchema{tool})}
		if st.choice != nil {
			opts = append(opts, WithToolChoice(st.choice))
		}
		if st.noThink && canDisable {
			opts = append(opts, disabler.DisableThinkingOptions()...)
		}

		resp, err := model.Chat(ctx, msgs, opts...)
		if resp != nil {
			addUsage(resp.Usage)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if isStructuredOutputFallbackErr(model, err) {
				logrus.WithError(err).Warnf("structured output: strategy %q rejected; trying next", st.name)
				continue
			}
			return nil, totalUsage, fmt.Errorf("structured output: model call failed: %w", err)
		}

		if result, ok := extractStructuredResult(resp); ok {
			return result, totalUsage, nil
		}
		extractErr := fmt.Errorf("structured output: strategy %q produced no valid structured result", st.name)
		if firstErr == nil {
			firstErr = extractErr
		}
		logrus.WithError(extractErr).Warn("structured output: trying next strategy")
	}

	if firstErr == nil {
		firstErr = fmt.Errorf("structured output: no strategies available")
	}
	return nil, totalUsage, fmt.Errorf("%w: %v", errors.ErrStructuredOutput, firstErr)
}

// isStructuredOutputFallbackErr reports whether err indicates the request
// shape was rejected (so another strategy may succeed) rather than a hard
// failure. Models implementing StructuredFallbackClassifier use their own
// classification; otherwise a narrow marker heuristic covers the classic
// case of a provider refusing forced tool_choice while thinking is enabled.
func isStructuredOutputFallbackErr(m ChatModel, err error) bool {
	if err == nil {
		return false
	}
	if c, ok := m.(StructuredFallbackClassifier); ok {
		return c.IsStructuredOutputFallbackError(err)
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "tool_choice") ||
		strings.Contains(s, "thinking")
}

// extractStructuredResult pulls the schema-conforming JSON out of a
// response: a generate_structured_output tool call wins; otherwise JSON is
// parsed (with repair) out of the accumulated text content.
//
// Note: text extraction is a Go-side convenience beyond upstream parity. It
// takes the FIRST balanced JSON object, which prose decoys can shadow, and
// repair may complete truncated objects; results are not schema-validated
// (matching the rest of the Go structured-output path).
func extractStructuredResult(resp *ChatResponse) (json.RawMessage, bool) {
	if resp == nil {
		return nil, false
	}

	for _, block := range resp.Content {
		tc, ok := block.(message.ToolCallBlock)
		if !ok || tc.Name != structuredOutputToolName {
			continue
		}
		if raw, ok := validOrRepairJSON(tc.Input); ok {
			return raw, true
		}
		return nil, false
	}

	// No tool call: try to parse JSON out of the text (the "none" strategy).
	var text strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.(message.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	if raw, ok := validOrRepairJSON(extractJSONObject(text.String())); ok {
		return raw, true
	}
	return nil, false
}

// validOrRepairJSON validates raw JSON, falling back to jsonx repair.
func validOrRepairJSON(s string) (json.RawMessage, bool) {
	raw := json.RawMessage(s)
	if json.Valid(raw) {
		return raw, true
	}
	var repaired any
	if err := jsonx.RepairAndUnmarshal([]byte(s), &repaired); err == nil {
		if b, err := json.Marshal(repaired); err == nil {
			return json.RawMessage(b), true
		}
	}
	return nil, false
}

// extractJSONObject returns the first balanced {...} substring, so JSON
// embedded in prose ("Here you go: {...}") still parses.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}
