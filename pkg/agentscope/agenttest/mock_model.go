// Package agenttest provides in-memory helpers for testing agents without
// making real model API calls. It is deliberately named agenttest (not
// testing) to avoid colliding with the standard library testing package.
package agenttest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// MockResponse is a canned model response returned when a MockRule matches.
type MockResponse struct {
	content []message.ContentBlock
	usage   *model.ChatUsage
	err     error
}

// WithUsage attaches token usage to a response.
func (r MockResponse) WithUsage(u *model.ChatUsage) MockResponse {
	r.usage = u
	return r
}

// RespondWithText builds a response containing a single text block.
func RespondWithText(text string) MockResponse {
	return MockResponse{
		content: []message.ContentBlock{
			message.TextBlock{Type: "text", ID: agentscope.GenerateID(), Text: text},
		},
	}
}

// RespondWithToolCall builds a response containing a single tool call block.
// The input map is marshaled to a JSON string for ToolCallBlock.Input.
func RespondWithToolCall(name string, input map[string]any) MockResponse {
	raw, err := json.Marshal(input)
	if err != nil {
		raw = []byte("{}")
	}
	return MockResponse{
		content: []message.ContentBlock{
			message.ToolCallBlock{
				Type:  "tool_call",
				ID:    agentscope.GenerateID(),
				Name:  name,
				Input: string(raw),
				State: message.ToolCallPending,
			},
		},
	}
}

// RespondWithError builds a response that makes the mock return an error.
func RespondWithError(err error) MockResponse {
	return MockResponse{err: err}
}

// MockRule pairs a match predicate with the response to return on a match.
// Rules are evaluated in order; the first matching rule wins.
type MockRule struct {
	// nth, when > 0, matches on the nth model call (1-indexed) regardless of msgs.
	nth      int
	match    func(msgs []*message.Msg) bool
	response MockResponse
}

// OnPromptContaining matches when any message's text content contains substr.
func OnPromptContaining(substr string, resp MockResponse) MockRule {
	return MockRule{
		match: func(msgs []*message.Msg) bool {
			for _, m := range msgs {
				if m == nil {
					continue
				}
				for _, b := range m.Content {
					if tb, ok := b.(message.TextBlock); ok && strings.Contains(tb.Text, substr) {
						return true
					}
				}
			}
			return false
		},
		response: resp,
	}
}

// OnToolCall matches when the conversation contains a tool result produced by
// the named tool, i.e. the model is being asked to respond after that tool ran.
func OnToolCall(toolName string, resp MockResponse) MockRule {
	return MockRule{
		match: func(msgs []*message.Msg) bool {
			for _, m := range msgs {
				if m == nil {
					continue
				}
				for _, b := range m.Content {
					if trb, ok := b.(message.ToolResultBlock); ok && trb.Name == toolName {
						return true
					}
				}
			}
			return false
		},
		response: resp,
	}
}

// OnNthCall matches on the nth model call (1-indexed), regardless of content.
func OnNthCall(n int, resp MockResponse) MockRule {
	return MockRule{nth: n, response: resp}
}

// Default matches unconditionally. Place it last so it acts as a fallback.
func Default(resp MockResponse) MockRule {
	return MockRule{match: func([]*message.Msg) bool { return true }, response: resp}
}

// MockCall records a single invocation of the mock model.
type MockCall struct {
	Messages []*message.Msg
	Response *model.ChatResponse
	Err      error
}

// MockModel is an in-memory model.ChatModel driven by a list of rules.
type MockModel struct {
	rules []MockRule

	mu        sync.Mutex
	calls     []*MockCall
	callCount int
}

// NewMockModel builds a MockModel from the given rules, evaluated in order.
func NewMockModel(rules ...MockRule) *MockModel {
	return &MockModel{rules: rules}
}

// Chat returns the response of the first matching rule.
func (m *MockModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.mu.Lock()
	m.callCount++
	callIdx := m.callCount
	m.mu.Unlock()

	var resp MockResponse
	matched := false
	for _, r := range m.rules {
		if r.nth > 0 {
			if r.nth == callIdx {
				resp, matched = r.response, true
				break
			}
			continue
		}
		if r.match != nil && r.match(msgs) {
			resp, matched = r.response, true
			break
		}
	}

	call := &MockCall{Messages: copyMsgs(msgs)}

	if !matched {
		resp = RespondWithText("mock: no matching rule")
	}

	if resp.err != nil {
		call.Err = resp.err
		m.record(call)
		return nil, resp.err
	}

	chatResp := &model.ChatResponse{
		Content: resp.content,
		IsLast:  true,
		ID:      agentscope.GenerateID(),
		Usage:   resp.usage,
	}
	call.Response = chatResp
	m.record(call)
	return chatResp, nil
}

// ChatStream returns the matched response as a single final chunk.
func (m *MockModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	resp, err := m.Chat(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.ChatResponse, 1)
	ch <- *resp
	close(ch)
	return ch, nil
}

// CountTokens returns a rough byte-based estimate.
func (m *MockModel) CountTokens(msgs []*message.Msg, _ []model.ToolSchema) int {
	total := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, b := range msg.Content {
			switch blk := b.(type) {
			case message.TextBlock:
				total += len(blk.Text)
			case message.ToolCallBlock:
				total += len(blk.Input)
			case message.ToolResultBlock:
				total += len(blk.GetOutputText())
			}
		}
	}
	return total / 4
}

// Calls returns the recorded calls in invocation order.
func (m *MockModel) Calls() []*MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*MockCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *MockModel) record(call *MockCall) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
}

func copyMsgs(msgs []*message.Msg) []*message.Msg {
	out := make([]*message.Msg, len(msgs))
	copy(out, msgs)
	return out
}
