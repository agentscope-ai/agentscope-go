package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// localWriter is a local model that streams one whole-file write in its
// own argument order, then text if it is ever called again.
type localWriter struct {
	mu    sync.Mutex
	calls int
}

func (l *localWriter) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	l.mu.Lock()
	l.calls++
	n := l.calls
	l.mu.Unlock()
	ch := make(chan model.ChatResponse, 1)
	if n == 1 {
		ch <- model.ChatResponse{IsLast: true, StopReason: model.StopReasonToolCalls, Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "call_local", Name: "write_file", Input: `{"path": "a.go", "content": "package a\n"}`},
		}}
	} else {
		ch <- model.ChatResponse{IsLast: true, Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "local again"}}}
	}
	close(ch)
	return ch, nil
}

func (l *localWriter) Chat(ctx context.Context, msgs []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	ch, _ := l.ChatStream(ctx, msgs, opts...)
	r := <-ch
	return &r, nil
}

func (l *localWriter) CountTokens([]*message.Msg, []model.ToolSchema) int { return 0 }

// TestCanonicalReplayThroughTheAgent runs a UnifiedAgent on a
// ConnectivityAwareModel whose cloud is the real Responses adapter. The
// cloud refuses the first round, the local model writes a file, and the
// cloud's next request carries that call with sorted keys while the agent's
// stored message keeps the local model's form and the writer mark.
func TestCanonicalReplayThroughTheAgent(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		n++
		first := n == 1
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if !first {
			bodies = append(bodies, body)
		}
		mu.Unlock()
		if first {
			http.Error(w, `{"error":{"message":"unavailable"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n")
	}))
	t.Cleanup(srv.Close)
	cloud, err := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{APIKey: "k", Model: "gpt-4.1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cam := model.NewConnectivityAwareModel(&localWriter{}, cloud,
		model.WithFailureThreshold(1), model.WithRecoveryTimeout(time.Nanosecond), model.WithCanonicalReplay())

	write := tool.NewFunctionTool("write_file", "write a file", nil, func(context.Context, map[string]any) (any, error) {
		// Hold the round until the cloud probe is due, so the next model
		// call goes to the cloud even on a coarse clock.
		for deadline := time.Now().Add(5 * time.Second); cam.ActiveModel() != "cloud"; {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("the circuit never became half-open")
			}
			time.Sleep(time.Millisecond)
		}
		return "written", nil
	})
	a := NewUnifiedAgent("assistant", "sys", cam, WithToolkit(tool.NewToolkit(write)))
	msg, err := a.Reply(context.Background(), "write a.go")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := msg.GetTextContent(""); got == nil || *got != "done" {
		t.Fatalf("reply text = %v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("cloud requests after the refusal = %d, want 1", len(bodies))
	}
	var args string
	items, _ := bodies[0]["input"].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m["type"] == "function_call" && m["call_id"] == "call_local" {
			args, _ = m["arguments"].(string)
		}
	}
	if args != `{"content":"package a\n","path":"a.go"}` {
		t.Fatalf("the local call reached the cloud as %q", args)
	}

	var stored *message.ToolCallBlock
	for _, m := range a.state.Context {
		for _, b := range m.Content {
			if c, ok := b.(message.ToolCallBlock); ok && c.ID == "call_local" {
				stored = &c
			}
		}
	}
	if stored == nil {
		t.Fatal("the local call is not in the agent's context")
	}
	if stored.Input != `{"path": "a.go", "content": "package a\n"}` {
		t.Fatalf("the stored call changed: %q", stored.Input)
	}
	if _, ok := stored.Extra["agentscope_writer_rank"]; !ok {
		t.Fatalf("the stored call lost its writer mark: %v", stored.Extra)
	}
}
