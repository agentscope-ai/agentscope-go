package model

import (
	"context"
	"errors"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/sirupsen/logrus"
)

// FallbackChatModel wraps an ordered list of ChatModels with automatic failover.
// If a model fails (for non-cancellation errors), the call advances to the next
// model in the chain. The first model may additionally be retried MaxRetries
// times before advancing (back-compat with the two-model constructor).
type FallbackChatModel struct {
	Primary    ChatModel // first model (also index 0 of Models when set)
	Fallback   ChatModel // second model, when constructed via NewFallbackChatModel
	Models     []ChatModel
	MaxRetries int // extra retries on the FIRST model before advancing; 0 means try once
}

// NewFallbackChatModel creates a two-model failover (primary -> fallback).
func NewFallbackChatModel(primary, fallback ChatModel) *FallbackChatModel {
	return &FallbackChatModel{
		Primary:  primary,
		Fallback: fallback,
	}
}

// NewFallbackChain creates an ordered failover chain of any length. Models are
// tried in order until one succeeds.
func NewFallbackChain(models ...ChatModel) *FallbackChatModel {
	f := &FallbackChatModel{Models: models}
	if len(models) > 0 {
		f.Primary = models[0]
	}
	if len(models) > 1 {
		f.Fallback = models[1]
	}
	return f
}

// effectiveModels returns the ordered failover list, deriving it from
// Primary/Fallback when Models was not set explicitly.
func (f *FallbackChatModel) effectiveModels() []ChatModel {
	if len(f.Models) > 0 {
		return f.Models
	}
	var models []ChatModel
	if f.Primary != nil {
		models = append(models, f.Primary)
	}
	if f.Fallback != nil {
		models = append(models, f.Fallback)
	}
	return models
}

func isCanceled(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (f *FallbackChatModel) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (*ChatResponse, error) {
	models := f.effectiveModels()
	var lastErr error
	for idx, m := range models {
		attempts := 1
		if idx == 0 {
			attempts = f.MaxRetries + 1
		}
		for i := 0; i < attempts; i++ {
			if i > 0 {
				timer := time.NewTimer(time.Duration(i) * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
			}
			resp, err := m.Chat(ctx, msgs, opts...)
			if err == nil {
				return resp, nil
			}
			if isCanceled(ctx, err) {
				return nil, err
			}
			lastErr = err
		}
		if idx < len(models)-1 {
			logrus.WithError(lastErr).Warn("model failed, falling back to next in chain")
		}
	}
	return nil, lastErr
}

func (f *FallbackChatModel) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	// NOTE: this fails over only on stream *setup* error. Mid-stream failover
	// (buffering the first chunk to detect a failed generation) is a planned
	// enhancement; consumers should also check ChatResponse.Error on each chunk.
	models := f.effectiveModels()
	var lastErr error
	for idx, m := range models {
		ch, err := m.ChatStream(ctx, msgs, opts...)
		if err == nil {
			return ch, nil
		}
		if isCanceled(ctx, err) {
			return nil, err
		}
		lastErr = err
		if idx < len(models)-1 {
			logrus.WithError(err).Warn("model stream setup failed, falling back to next in chain")
		}
	}
	return nil, lastErr
}

func (f *FallbackChatModel) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	for _, m := range f.effectiveModels() {
		return m.CountTokens(msgs, tools)
	}
	return 0
}

// ContextSize delegates to the first model if it implements ContextSizer. It
// reports the first model only; ResolveContextSize considers the whole chain.
func (f *FallbackChatModel) ContextSize() int {
	models := f.effectiveModels()
	if len(models) > 0 {
		if cs, ok := models[0].(ContextSizer); ok {
			return cs.ContextSize()
		}
	}
	return 0
}

// ModelName delegates to the first model if it implements ModelNamer. It
// reports the first model only; ResolveContextSize considers the whole chain.
func (f *FallbackChatModel) ModelName() string {
	models := f.effectiveModels()
	if len(models) > 0 {
		if mn, ok := models[0].(ModelNamer); ok {
			return mn.ModelName()
		}
	}
	return ""
}
