package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event/streamcheck"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// Ollama streams thinking in a string "reasoning" delta field. ReplyStream
// must publish it as thinking before the answer text, and the published
// deltas must match the final assembled response.
func TestReplyStreamOllamaReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(choices string) {
			fmt.Fprintf(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"qwen3:14b","choices":%s}`+"\n\n", choices)
		}
		chunk(`[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"first "},"finish_reason":null}]`)
		chunk(`[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"second"},"finish_reason":null}]`)
		chunk(`[{"index":0,"delta":{"role":"assistant","content":"answer"},"finish_reason":null}]`)
		chunk(`[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]`)
		fmt.Fprint(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"qwen3:14b","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m, err := model.NewOllamaChatModel(model.OllamaConfig{Model: "qwen3:14b", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	a := NewUnifiedAgent("bot", "test", m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	var events []event.Event
	var thinking, text strings.Builder
	var end event.ReplyEndEvent
	for e := range ch {
		events = append(events, e)
		switch v := e.(type) {
		case event.ThinkingBlockDeltaEvent:
			if text.Len() > 0 {
				t.Error("thinking delta published after answer text")
			}
			thinking.WriteString(v.Delta)
		case event.TextBlockDeltaEvent:
			text.WriteString(v.Delta)
		case event.ReplyEndEvent:
			end = v
		}
	}
	if end.FinishedReason == types.ReplyError {
		t.Fatalf("reply failed: %+v", end.Error)
	}
	if thinking.String() != "first second" || text.String() != "answer" {
		t.Fatalf("thinking=%q text=%q", thinking.String(), text.String())
	}
	if err := streamcheck.Validate(events); err != nil {
		t.Fatal(err)
	}
	last := a.state.Context[len(a.state.Context)-1]
	blocks := last.GetContentBlocks(message.ContentBlockThinking)
	if len(blocks) != 1 || blocks[0].(message.ThinkingBlock).Thinking != "first second" {
		t.Fatalf("stored thinking = %v", blocks)
	}
}
