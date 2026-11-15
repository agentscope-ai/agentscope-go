package tool

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// ToolChunk represents a piece of streaming output from a tool execution.
type ToolChunk struct {
	// Content contains the content blocks for this chunk.
	Content []message.ContentBlock

	// Metadata holds per-chunk metadata (e.g. progress info).
	Metadata map[string]any

	// IsFinal marks this as the last chunk in the stream.
	IsFinal bool

	// State indicates the tool result state (set on the final chunk).
	State message.ToolResultState
}

// StreamingTool is an optional interface that tools can implement to support
// streaming responses. Tools that implement this interface can yield partial
// results as they become available.
type StreamingTool interface {
	Tool

	// ExecuteStream starts a streaming execution and returns a channel of
	// ToolChunks. The channel is closed when the execution completes.
	// The final chunk should have IsFinal=true and State set.
	ExecuteStream(ctx context.Context, input map[string]any) (<-chan ToolChunk, error)
}

// WrapNonStreamingTool adapts a non-streaming Tool into a StreamingTool by
// calling Execute once and returning the result as a single final chunk.
func WrapNonStreamingTool(t Tool) StreamingTool {
	return &nonStreamingAdapter{Tool: t}
}

type nonStreamingAdapter struct {
	Tool
}

func (a *nonStreamingAdapter) ExecuteStream(ctx context.Context, input map[string]any) (<-chan ToolChunk, error) {
	ch := make(chan ToolChunk, 1)
	go func() {
		defer close(ch)
		resp, err := a.Tool.Execute(ctx, input)
		if err != nil {
			ch <- ToolChunk{
				Content: []message.ContentBlock{message.TextBlock{
					Type: "text",
					Text: err.Error(),
				}},
				IsFinal: true,
				State:   message.ToolResultError,
			}
			return
		}
		ch <- ToolChunk{
			Content:  resp.Content,
			Metadata: resp.Metadata,
			IsFinal:  true,
			State:    resp.State,
		}
	}()
	return ch, nil
}

// IsStreamingTool reports whether a Tool also implements StreamingTool.
func IsStreamingTool(t Tool) bool {
	_, ok := t.(StreamingTool)
	return ok
}

// CollectStream reads all chunks from a streaming tool execution and assembles
// them into a single ToolResponse. TextBlocks with the same ID are merged
// (text appended) rather than kept as separate blocks. This produces a cleaner
// result when a tool streams incremental text deltas for the same logical block.
func CollectStream(ch <-chan ToolChunk) *ToolResponse {
	var allContent []message.ContentBlock
	// Track TextBlock positions by ID so we can merge subsequent deltas.
	textBlockIndex := make(map[string]int) // ID -> index in allContent
	var lastMeta map[string]any
	var lastState message.ToolResultState

	for chunk := range ch {
		for _, block := range chunk.Content {
			tb, isText := block.(message.TextBlock)
			if isText && tb.ID != "" {
				if idx, exists := textBlockIndex[tb.ID]; exists {
					// Merge: append text to existing block with same ID
					existing := allContent[idx].(message.TextBlock)
					existing.Text += tb.Text
					allContent[idx] = existing
					continue
				}
				// First time seeing this ID; record its position
				textBlockIndex[tb.ID] = len(allContent)
			}
			allContent = append(allContent, block)
		}
		if chunk.Metadata != nil {
			lastMeta = chunk.Metadata
		}
		if chunk.IsFinal {
			// Preserve an ERROR state: a trailing INTERRUPTED/DENIED final
			// chunk must not overwrite it (upstream fix #2178).
			if lastState == message.ToolResultError &&
				(chunk.State == message.ToolResultInterrupted || chunk.State == message.ToolResultDenied) {
				continue
			}
			lastState = chunk.State
		}
	}

	if lastState == "" {
		lastState = message.ToolResultSuccess
	}

	return &ToolResponse{
		Content:  allContent,
		State:    lastState,
		Metadata: lastMeta,
	}
}
