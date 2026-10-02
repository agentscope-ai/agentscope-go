package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// WithModelStreaming controls provider streaming in Reply and ReplyStream.
// Enabled by default. Legacy middleware requires the complete response and
// therefore retains buffered Chat calls even when this option is enabled.
// The loop bridge and context-compression helpers are unaffected.
func WithModelStreaming(enabled bool) AgentOption {
	return func(a *UnifiedAgent) { a.modelStreaming = enabled }
}

func (a *UnifiedAgent) streamsModel() bool {
	return a.modelStreaming && len(a.middlewares) == 0
}

// replyContentStream owns presentation blocks for one model operation. Text
// and thinking deltas without provider block identity are coalesced by kind;
// canonical final content (including provider metadata) remains authoritative
// for history. Data and tool blocks are not published speculatively.
type replyContentStream struct {
	ctx     context.Context
	out     chan<- event.Event
	replyID string
	blocks  map[replyStreamKey]*replyStreamBlock
	order   []replyStreamKey
}

type replyStreamKey struct{ kind, sourceID string }

type replyStreamBlock struct {
	id   string
	text strings.Builder
}

func (s *replyContentStream) delta(key replyStreamKey, text string) {
	if text == "" {
		return
	}
	if s.blocks == nil {
		s.blocks = make(map[replyStreamKey]*replyStreamBlock)
	}
	b := s.blocks[key]
	if b == nil {
		b = &replyStreamBlock{id: agentscope.GenerateID()}
		s.blocks[key] = b
		s.order = append(s.order, key)
		if key.kind == "text" {
			emit(s.ctx, s.out, event.NewTextBlockStartEvent(s.replyID, b.id))
		} else {
			emit(s.ctx, s.out, event.NewThinkingBlockStartEvent(s.replyID, b.id))
		}
	}
	b.text.WriteString(text)
	if key.kind == "text" {
		emit(s.ctx, s.out, event.NewTextBlockDeltaEvent(s.replyID, b.id, text))
	} else {
		emit(s.ctx, s.out, event.NewThinkingBlockDeltaEvent(s.replyID, b.id, text))
	}
}

func (s *replyContentStream) close() {
	for _, key := range s.order {
		b := s.blocks[key]
		if key.kind == "text" {
			emit(s.ctx, s.out, event.NewTextBlockEndEvent(s.replyID, b.id))
		} else {
			emit(s.ctx, s.out, event.NewThinkingBlockEndEvent(s.replyID, b.id))
		}
	}
}

func (s *replyContentStream) consume(content []message.ContentBlock) {
	for _, b := range content {
		switch v := b.(type) {
		case message.TextBlock:
			s.delta(replyStreamKey{"text", v.ID}, v.Text)
		case message.ThinkingBlock:
			s.delta(replyStreamKey{"thinking", v.ID}, v.Thinking)
		}
	}
}

func (s *replyContentStream) finish(resp *model.ChatResponse) error {
	final := make(map[replyStreamKey]string)
	var order []replyStreamKey
	for _, b := range resp.Content {
		var key replyStreamKey
		var text string
		switch v := b.(type) {
		case message.TextBlock:
			key, text = replyStreamKey{"text", v.ID}, v.Text
		case message.ThinkingBlock:
			key, text = replyStreamKey{"thinking", v.ID}, v.Thinking
		default:
			continue
		}
		// Providers may assign IDs only in their assembled response. In that
		// case the anonymous presentation block covers the same-kind content.
		if s.blocks[replyStreamKey{kind: key.kind}] != nil {
			key.sourceID = ""
		}
		if _, ok := final[key]; !ok {
			order = append(order, key)
		}
		final[key] += text
	}
	// Validate every block before publishing any final-only suffix.
	for key, b := range s.blocks {
		if !strings.HasPrefix(final[key], b.text.String()) {
			return fmt.Errorf("agent: final %s differs from published stream", key.kind)
		}
	}
	for _, key := range order {
		n := 0
		if b := s.blocks[key]; b != nil {
			n = b.text.Len()
		}
		s.delta(key, final[key][n:])
	}
	for _, b := range resp.Content {
		if data, ok := b.(message.DataBlock); ok {
			if data.ID == "" {
				data.ID = agentscope.GenerateID()
			}
			emitContentEvents(s.ctx, s.out, s.replyID, []message.ContentBlock{data})
		}
	}
	return nil
}

// callModelStreaming retries setup failures only. Once a stream is established,
// its failure terminates the operation; no attempt can replay published deltas.
func (a *UnifiedAgent) callModelStreaming(ctx context.Context, msgs []*message.Msg, opts []model.CallOption, out chan<- event.Event, replyID string) (*model.ChatResponse, error) {
	s := &replyContentStream{ctx: ctx, out: out, replyID: replyID}
	defer s.close()
	attempts := a.modelCfg.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}
	delay := a.modelCfg.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	targets := []model.ChatModel{a.model}
	if a.modelCfg.FallbackModel != nil {
		targets = append(targets, a.modelCfg.FallbackModel)
	}
	var lastErr error
	for index, target := range targets {
		n := attempts
		if index > 0 {
			n = 1
		}
		for attempt := 0; attempt < n; attempt++ {
			if attempt > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				}
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			resp, established, err := streamModelAttempt(ctx, target, msgs, opts, s)
			if ctx.Err() != nil {
				return resp, ctx.Err()
			}
			if err == nil || established || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return resp, err
			}
			lastErr = err
		}
	}
	return nil, lastErr
}

func streamModelAttempt(ctx context.Context, target model.ChatModel, msgs []*message.Msg, opts []model.CallOption, s *replyContentStream) (*model.ChatResponse, bool, error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := target.ChatStream(callCtx, msgs, opts...)
	if errors.Is(err, model.ErrStreamNotSupported) {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		resp, chatErr := target.Chat(callCtx, msgs, opts...)
		if chatErr != nil {
			return resp, false, chatErr
		}
		return finishModelStream(resp, s)
	}
	if err != nil {
		return nil, false, err
	}
	if ch == nil {
		return nil, true, fmt.Errorf("agent: model returned a nil stream")
	}
	for {
		select {
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case chunk, ok := <-ch:
			if !ok {
				return nil, true, fmt.Errorf("agent: model stream closed without a final response")
			}
			if chunk.Error != nil {
				return &chunk, true, chunk.Error
			}
			if chunk.IsLast {
				for _, block := range chunk.Content {
					if call, ok := block.(message.ToolCallBlock); ok {
						var input map[string]json.RawMessage
						if call.Name == "" || json.Unmarshal([]byte(call.Input), &input) != nil || input == nil {
							return &chunk, true, fmt.Errorf("agent: stream returned incomplete tool arguments")
						}
					}
				}
				return finishModelStream(&chunk, s)
			}
			s.consume(chunk.Content)
		}
	}
}

func finishModelStream(resp *model.ChatResponse, s *replyContentStream) (*model.ChatResponse, bool, error) {
	if resp == nil {
		return nil, true, fmt.Errorf("agent: model returned no response")
	}
	if resp.Error != nil {
		return resp, true, resp.Error
	}
	if resp.StopReason == model.StopReasonLength || resp.StopReason == model.StopReasonContentFilter {
		return resp, true, fmt.Errorf("agent: incomplete model response (%s)", resp.StopReason)
	}
	if err := s.ctx.Err(); err != nil {
		return resp, true, err
	}
	return resp, true, s.finish(resp)
}

func emitModelCallEnd(ctx context.Context, out chan<- event.Event, replyID string, resp *model.ChatResponse) {
	var usage model.ChatUsage
	if resp != nil && resp.Usage != nil {
		usage = *resp.Usage
	}
	emit(ctx, out, event.NewModelCallEndEventWithCache(replyID, usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheInputTokens))
}
