package tierrouter

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ChatRequest is a deep-copied fake deployment request used as grading evidence.
type ChatRequest struct {
	Messages []*message.Msg
	Options  model.CallOptions
}

// FakeChatModel is a deterministic single-attempt ChatModel for offline fixtures.
type FakeChatModel struct {
	tier Tier

	Reply          string
	FailErr        error
	FailNext       int
	HangNext       int
	Delay          time.Duration
	BeforeReturn   func()
	Response       *model.ChatResponse
	ReturnResponse bool
	ContextWindow  int
	TokenEstimate  int

	mu       sync.Mutex
	requests []ChatRequest
}

var _ model.ChatModel = (*FakeChatModel)(nil)

// NewFakeChatModel returns a fake that completes with a short text response.
func NewFakeChatModel(tier Tier) *FakeChatModel {
	return &FakeChatModel{tier: tier, Reply: "ok"}
}

// Tier returns the deployment tier claimed by the fake.
func (f *FakeChatModel) Tier() Tier { return f.tier }

// Chat records the request and executes the configured deterministic script.
func (f *FakeChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	var options model.CallOptions
	for _, opt := range opts {
		opt(&options)
	}
	messages, err := cloneTextMessages(msgs)
	if err != nil {
		return nil, err
	}
	options, err = cloneCallOptions(&options)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.requests = append(f.requests, ChatRequest{Messages: messages, Options: options})
	fail := f.FailNext > 0
	if fail {
		f.FailNext--
	}
	hang := f.HangNext > 0
	if hang {
		f.HangNext--
	}
	failErr := f.FailErr
	reply := f.Reply
	delay := f.Delay
	beforeReturn := f.BeforeReturn
	response := f.Response
	returnResponse := f.ReturnResponse
	f.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if beforeReturn != nil {
		beforeReturn()
	}
	if fail {
		if failErr == nil {
			failErr = errors.New("fake chat model: scripted clean failure")
		}
		return nil, failErr
	}
	if returnResponse {
		return response, nil
	}
	return &model.ChatResponse{
		Content:    []message.ContentBlock{message.TextBlock{Type: "text", Text: reply}},
		IsLast:     true,
		ModelName:  string(f.tier),
		StopReason: model.StopReasonStop,
		Usage:      &model.ChatUsage{InputTokens: 1, OutputTokens: 1},
		Metadata: map[string]any{
			"nested": []any{map[string]any{"value": "original"}},
		},
	}, nil
}

// ChatStream reports that the fake is buffered-only.
func (f *FakeChatModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

// CountTokens returns the configured synthetic estimate.
func (f *FakeChatModel) CountTokens([]*message.Msg, []model.ToolSchema) int {
	return f.TokenEstimate
}

// ContextSize returns the configured synthetic context window.
func (f *FakeChatModel) ContextSize() int {
	return f.ContextWindow
}

// Requests returns deep-copied deployment requests in arrival order.
func (f *FakeChatModel) Requests() []ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ChatRequest, len(f.requests))
	for i := range f.requests {
		req := &f.requests[i]
		messages, err := cloneTextMessages(req.Messages)
		if err != nil {
			panic(err)
		}
		options, err := cloneCallOptions(&req.Options)
		if err != nil {
			panic(err)
		}
		out[i] = ChatRequest{Messages: messages, Options: options}
	}
	return out
}
