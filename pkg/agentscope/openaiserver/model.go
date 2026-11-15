package openaiserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// requestPolicy is shared by the primary and fallback wrappers. The wrapper
// observes provider-reported usage; turn_stats remains the source of the
// reply's aggregated output usage.
type requestPolicy struct {
	req       *chatRequest
	mu        sync.Mutex
	err       error
	lastUsage *model.ChatUsage
}
type requestModel struct {
	inner  model.ChatModel
	policy *requestPolicy
}

func (m *requestModel) Unwrap() model.ChatModel { return m.inner }
func (m *requestModel) CountTokens(msgs []*message.Msg, tools []model.ToolSchema) int {
	return m.inner.CountTokens(msgs, tools)
}

func (m *requestModel) options(opts []model.CallOption) ([]model.CallOption, map[string]bool, error) {
	var o model.CallOptions
	for _, opt := range opts {
		opt(&o)
	}
	r := m.policy.req
	registered := make(map[string]model.ToolSchema, len(o.Tools))
	for _, t := range o.Tools {
		registered[t.Function.Name] = t
	}
	forcedNone := o.ToolChoice != nil && o.ToolChoice.Mode == "none"
	// Forced finalization deliberately offers no tools, even when the
	// request selected a subset for its ordinary reasoning rounds.
	selected := o.Tools
	if r.Tools != nil && !forcedNone {
		selected = nil
		for _, requested := range r.Tools {
			if _, ok := registered[requested.Function.Name]; !ok {
				return nil, nil, badRequest("requested tool is not registered and active on this agent")
			}
			// Client schemas describe this call only. Execution still validates
			// against the host tool's schema in the ordinary toolkit pipeline.
			selected = append(selected, requested)
		}
	}
	var choice string
	if present(r.ToolChoice) {
		_ = json.Unmarshal(r.ToolChoice, &choice)
	}
	// Preserve the agent's forced-finalization none choice.
	if forcedNone {
		choice = "none"
	}
	allowed := make(map[string]bool)
	if choice == "none" {
		selected = nil
	} else {
		for _, t := range selected {
			allowed[t.Function.Name] = true
		}
	}
	if r.Temperature != nil {
		opts = append(opts, model.WithTemperature(*r.Temperature))
	}
	if r.TopP != nil {
		opts = append(opts, model.WithTopP(*r.TopP))
	}
	if r.MaxTokens != nil {
		opts = append(opts, model.WithMaxTokens(*r.MaxTokens))
	}
	opts = append(opts, model.WithTools(selected))
	if choice != "" {
		opts = append(opts, model.WithToolChoice(&model.ToolChoice{Mode: choice}))
	}
	return opts, allowed, nil
}

func (m *requestModel) observe(resp *model.ChatResponse, err error, allowed map[string]bool) error {
	if err == nil && resp == nil {
		err = errors.New("model returned no response")
	}
	if err == nil {
		err = resp.Error
	}
	// The stateless server does not opt into internal compaction calls or
	// detached precompression. Keep provider size failures out of the core's
	// automatic overflow recovery, whose synthetic tools are not host tools.
	if model.IsContextLimitExceeded(err) {
		err = badRequest("provider rejected conversation size; shorten messages")
	}
	if err == nil {
		for _, b := range resp.Content {
			if call, ok := b.(message.ToolCallBlock); ok && !allowed[call.Name] {
				err = errors.New("provider returned a tool call outside the allowed request tools")
				break
			}
		}
	}
	m.policy.mu.Lock()
	defer m.policy.mu.Unlock()
	m.policy.err = err
	if err == nil {
		m.policy.lastUsage = nil
		if resp.Usage != nil {
			u := *resp.Usage
			m.policy.lastUsage = &u
		}
	}
	return err
}

func (m *requestModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	opts, allowed, err := m.options(opts)
	if err != nil {
		return nil, m.observe(nil, err, nil)
	}
	resp, err := m.inner.Chat(ctx, msgs, opts...)
	if err := m.observe(resp, err, allowed); err != nil {
		return nil, err
	}
	return resp, nil
}

func (m *requestModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (<-chan model.ChatResponse, error) {
	opts, allowed, err := m.options(opts)
	if err != nil {
		return nil, m.observe(nil, err, nil)
	}
	input, err := m.inner.ChatStream(ctx, msgs, opts...)
	if err != nil {
		return nil, m.observe(nil, err, nil)
	}
	if input == nil {
		err = errors.New("model returned no stream")
		_ = m.observe(nil, err, nil)
		return nil, err
	}
	out := make(chan model.ChatResponse)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case resp, ok := <-input:
				if !ok {
					return
				}
				// Tool deltas may have incomplete names; only a successful final
				// assembled response can reach the agent's execution path.
				if resp.IsLast || resp.Error != nil {
					if err := m.observe(&resp, nil, allowed); err != nil {
						resp.Error = err
					}
				}
				select {
				case out <- resp:
				case <-ctx.Done():
					return
				}
				if resp.Error != nil || resp.IsLast {
					return
				}
			}
		}
	}()
	return out, nil
}

func (p *requestPolicy) failure() error { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *requestPolicy) usage(outputs int) *usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastUsage == nil {
		return nil
	}
	input := p.lastUsage.EffectiveInputTokens()
	return &usage{PromptTokens: input, CompletionTokens: outputs, TotalTokens: input + outputs}
}
