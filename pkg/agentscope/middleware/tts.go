package middleware

import (
	"context"
	"encoding/base64"
	"strings"

	agentscope "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tts"
)

// TTSMiddleware intercepts the agent's text output events and synthesizes
// audio, injecting DataBlock events into the stream. It wraps the OnReply
// hook and passes all other hooks through unchanged.
type TTSMiddleware struct {
	BaseMiddleware
	Model tts.Model
}

// NewTTSMiddleware creates a TTS middleware that will synthesize audio from
// the agent's text output using the given TTS model.
func NewTTSMiddleware(model tts.Model) *TTSMiddleware {
	return &TTSMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "tts"},
		Model:          model,
	}
}

// OnReply wraps the reply event stream, accumulating text and synthesizing
// audio after each text block completes.
func (m *TTSMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	innerCh := next(ctx, input)
	outCh := make(chan event.Event, 16)

	go func() {
		defer close(outCh)
		var textBuf strings.Builder
		var replyID string

		for ev := range innerCh {
			outCh <- ev

			switch e := ev.(type) {
			case event.ReplyStartEvent:
				replyID = e.ReplyID
			case event.TextBlockDeltaEvent:
				textBuf.WriteString(e.Delta)
			case event.TextBlockEndEvent:
				text := textBuf.String()
				textBuf.Reset()
				if text == "" {
					continue
				}
				m.synthesizeAndEmit(ctx, outCh, replyID, text)
			}
		}
	}()

	return outCh
}

func (m *TTSMiddleware) synthesizeAndEmit(ctx context.Context, outCh chan<- event.Event, replyID, text string) {
	blockID := agentscope.GenerateID()

	streamCh, err := m.Model.SynthesizeStream(ctx, text)
	if err == nil {
		first := true
		for resp := range streamCh {
			if first {
				outCh <- event.NewDataBlockStartEvent(replyID, blockID, resp.MediaType)
				first = false
			}
			outCh <- event.NewDataBlockDeltaEvent(
				replyID, blockID,
				base64.StdEncoding.EncodeToString(resp.Content),
				resp.MediaType,
			)
		}
		if !first {
			outCh <- event.NewDataBlockEndEvent(replyID, blockID)
		}
		return
	}

	resp, err := m.Model.Synthesize(ctx, text)
	if err != nil || resp == nil || len(resp.Content) == 0 {
		return
	}

	outCh <- event.NewDataBlockStartEvent(replyID, blockID, resp.MediaType)
	outCh <- event.NewDataBlockDeltaEvent(
		replyID, blockID,
		base64.StdEncoding.EncodeToString(resp.Content),
		resp.MediaType,
	)
	outCh <- event.NewDataBlockEndEvent(replyID, blockID)
}
