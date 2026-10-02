package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event/streamcheck"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

func TestReplyStreamDeepSeekIncremental(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var streaming atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		streaming.Store(body.Stream)
		if !body.Stream {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","reasoning_content":"first second","content":"answer"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{\"reasoning_content\":\"first \"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{\"reasoning_content\":\"second\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m, err := model.NewDeepSeekChatModel(model.DeepSeekConfig{Model: "fixture", APIKey: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	a := NewUnifiedAgent("bot", "test", m)
	ch, err := a.ReplyStream(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	var events []event.Event
	var thinking, text strings.Builder
	var deltas, ends int
	for e := range ch {
		events = append(events, e)
		switch v := e.(type) {
		case event.ThinkingBlockDeltaEvent:
			if ends != 0 {
				t.Error("model ended before thinking delta")
			}
			deltas++
			thinking.WriteString(v.Delta)
			if deltas == 1 {
				if v.Delta != "first " {
					t.Errorf("first delta = %q", v.Delta)
				}
				unblock()
			}
		case event.TextBlockDeltaEvent:
			text.WriteString(v.Delta)
		case event.ModelCallEndEvent:
			ends++
			if v.InputTokens != 9 || v.OutputTokens != 4 || v.CacheReadTokens != 3 {
				t.Errorf("usage = %+v", v)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	if !streaming.Load() || deltas != 2 || ends != 1 || thinking.String() != "first second" || text.String() != "answer" {
		t.Fatalf("stream=%v deltas=%d ends=%d thinking=%q text=%q", streaming.Load(), deltas, ends, thinking.String(), text.String())
	}
	if err := streamcheck.Validate(events); err != nil {
		t.Fatal(err)
	}
	last := a.state.Context[len(a.state.Context)-1]
	if last.Usage == nil || last.Usage.InputTokens != 9 {
		t.Fatalf("stored usage = %+v", last.Usage)
	}
	if got := last.GetTextContent(""); got == nil || *got != "answer" {
		t.Fatalf("stored text = %v", got)
	}
}

func TestReplyStreamDeepSeekTruncatedTool(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"partial\",\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"type\":\"function\",\"function\":{\"name\":\"write\",\"arguments\":\"{}\"}}]}}]}\n\n")
	}))
	defer srv.Close()
	m, err := model.NewDeepSeekChatModel(model.DeepSeekConfig{Model: "fixture", APIKey: "fixture", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	a := NewUnifiedAgent("bot", "test", m, WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Millisecond}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := a.ReplyStream(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	var end event.ReplyEndEvent
	for e := range ch {
		if v, ok := e.(event.ReplyEndEvent); ok {
			end = v
		}
		if _, ok := e.(event.ToolCallStartEvent); ok {
			t.Error("partial tool executed")
		}
	}
	if end.FinishedReason != types.ReplyError {
		t.Fatalf("end = %+v", end)
	}
	if calls.Load() != 1 {
		t.Fatalf("partial stream retried: %d requests", calls.Load())
	}
	for _, msg := range a.state.Context {
		for _, b := range msg.Content {
			if _, ok := b.(message.ToolCallBlock); ok {
				t.Fatal("partial tool stored for later execution")
			}
		}
	}
}
