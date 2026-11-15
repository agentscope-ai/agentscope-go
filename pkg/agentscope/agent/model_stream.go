package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

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

// published reports whether any presentation delta has been emitted. A
// failed stream with nothing published can be replayed invisibly.
func (s *replyContentStream) published() bool {
	return len(s.blocks) > 0
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

// errNilStream marks a provider client that returned a nil stream channel.
var errNilStream = errors.New("agent: model returned a nil stream")

// terminalStreamError wraps errors from a completed provider response (in-band
// partial errors, length/content-filter truncation, final-content mismatch).
// The provider finished answering; retrying the identical request is futile.
type terminalStreamError struct{ err error }

func (e *terminalStreamError) Error() string { return e.err.Error() }
func (e *terminalStreamError) Unwrap() error { return e.err }

// transportStreamError wraps mid-stream transport failures (connection cut
// before a final response arrived). Safe to replay while nothing was
// published to consumers.
type transportStreamError struct{ err error }

func (e *transportStreamError) Error() string { return e.err.Error() }
func (e *transportStreamError) Unwrap() error { return e.err }

// callModelStreaming retries transient failures. Setup failures are retried
// when they look transient (IsRetryableError). A stream that fails after being
// established is retried only while it has published nothing: replay is then
// invisible to consumers. Once text or thinking deltas have been published the
// failure terminates the operation, because no attempt can replay deltas the
// consumer already rendered. Completed-but-errored provider responses
// (terminalStreamError) are never retried: the provider answered as designed.
// Context-limit rejections are never retried here; the reply loop shrinks the
// context before re-attempting.
func (a *UnifiedAgent) callModelStreaming(ctx context.Context, msgs []*message.Msg, opts []model.CallOption, out chan<- event.Event, replyID string) (*model.ChatResponse, error) {
	s := &replyContentStream{ctx: ctx, out: out, replyID: replyID}
	defer s.close()
	attempts := a.modelCfg.MaxRetries
	if attempts <= 0 {
		attempts = defaultModelAttempts
	}
	delay := a.modelCfg.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	targets := []model.ChatModel{a.model}
	if a.modelCfg.FallbackModel != nil {
		targets = append(targets, a.modelCfg.FallbackModel)
	}
	idleTimeout := a.modelCfg.StreamIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultStreamIdleTimeout
	}
	if idleTimeout < 0 {
		idleTimeout = 0
	}
	var lastErr error
	for index, target := range targets {
		n := attempts
		if index > 0 {
			n = 1
		}
		for attempt := 0; attempt < n; attempt++ {
			if attempt > 0 {
				timer := time.NewTimer(retryBackoff(delay, attempt))
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
			resp, established, err := streamModelAttempt(ctx, target, msgs, opts, s, idleTimeout)
			if ctx.Err() != nil {
				return resp, ctx.Err()
			}
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return resp, err
			}
			var tse *terminalStreamError
			if errors.As(err, &tse) {
				return resp, err
			}
			if errors.Is(err, errNilStream) {
				// Provider-client defect; neither retry nor fallback helps.
				return resp, err
			}
			if established && s.published() {
				// The consumer already rendered partial output; no attempt
				// or fallback can replay that invisibly.
				return resp, err
			}
			if model.IsContextLimitExceeded(err) {
				// Not transient: the reply loop shrinks the context before
				// re-attempting. A fallback may have a larger window. Safe
				// past the published-guard: nothing was rendered yet.
				lastErr = err
				break
			}
			var transportErr *transportStreamError
			if !errors.As(err, &transportErr) && !model.IsRetryableError(err) {
				// Permanent setup failure (400/401/404): do not burn
				// retries on it; a fallback may still succeed.
				lastErr = err
				break
			}
			lastErr = err
		}
	}
	return nil, lastErr
}

// retryBackoff returns a full-jitter exponential backoff for the given
// 1-based retry attempt: base*2^(attempt-1), capped at 30s, jittered into
// [0, ceil). Mirrors the httpx transport policy at the agent level.
func retryBackoff(base time.Duration, attempt int) time.Duration {
	ceil := base << (attempt - 1)
	if ceil > 30*time.Second || ceil <= 0 {
		ceil = 30 * time.Second
	}
	return time.Duration(rand.Int63n(int64(ceil)))
}

func streamModelAttempt(ctx context.Context, target model.ChatModel, msgs []*message.Msg, opts []model.CallOption, s *replyContentStream, idleTimeout time.Duration) (*model.ChatResponse, bool, error) {
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
		// A nil stream is a provider-client defect, not a transient failure:
		// report it at setup level so it is never retried.
		return nil, false, errNilStream
	}
	// Idle guard: a stream silent for idleTimeout is treated as a transport
	// failure, so the retry policy above replays it while nothing was
	// published instead of hanging the reply forever. The timer also covers
	// the setup->first-chunk gap; the blocking ChatStream setup call itself
	// relies on the provider client's own transport timeouts.
	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if idleTimeout > 0 {
		idleTimer = time.NewTimer(idleTimeout)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case <-idleC:
			return nil, true, &transportStreamError{fmt.Errorf("agent: model stream idle for %s", idleTimeout)}
		case chunk, ok := <-ch:
			if idleTimer != nil {
				idleTimer.Reset(idleTimeout)
			}
			if !ok {
				return nil, true, &transportStreamError{fmt.Errorf("agent: model stream closed without a final response")}
			}
			if chunk.Error != nil {
				return &chunk, true, chunk.Error
			}
			if chunk.IsLast {
				for _, block := range chunk.Content {
					if call, ok := block.(message.ToolCallBlock); ok {
						var input map[string]json.RawMessage
						switch {
						case call.Name == "":
							// A nameless tool call can neither be
							// executed nor paired with an error result
							// in history: treat it as stream corruption.
							return &chunk, true, fmt.Errorf("agent: stream returned a tool call without a name")
						case json.Unmarshal([]byte(call.Input), &input) != nil || input == nil:
							// Broken arguments (truncated stream,
							// provider glitch): keep the block so
							// execution feeds a parse error back and
							// the model can re-issue the call, instead
							// of killing the whole reply. Formatters
							// repair the arguments for wire validity
							// on later rounds.
							logrus.WithField("tool", call.Name).Warn(
								"agent: streamed tool arguments are not valid JSON; feeding a parse error back")
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
		return nil, true, &terminalStreamError{fmt.Errorf("agent: model returned no response")}
	}
	if resp.Error != nil {
		return resp, true, &terminalStreamError{resp.Error}
	}
	if resp.StopReason == model.StopReasonLength || resp.StopReason == model.StopReasonContentFilter {
		return resp, true, &terminalStreamError{fmt.Errorf("agent: incomplete model response (%s)", resp.StopReason)}
	}
	if err := s.ctx.Err(); err != nil {
		return resp, true, err
	}
	if err := s.finish(resp); err != nil {
		return resp, true, &terminalStreamError{err}
	}
	return resp, true, nil
}

func emitModelCallEnd(ctx context.Context, out chan<- event.Event, replyID string, resp *model.ChatResponse) {
	var usage model.ChatUsage
	if resp != nil && resp.Usage != nil {
		usage = *resp.Usage
	}
	emit(ctx, out, event.NewModelCallEndEventWithCache(replyID, usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheInputTokens))
}
