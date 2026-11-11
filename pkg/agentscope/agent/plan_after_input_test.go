package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// planServer is a Responses API server for two replies: the first answers
// with text, the second updates the plan twice and then answers. It records
// the input items of every request.
type planServer struct {
	mu     sync.Mutex
	inputs [][]any
}

var planUpdates = map[int]string{
	1: `{"plan":[{"step":"read","status":"in_progress"},{"step":"write","status":"pending"}]}`,
	2: `{"plan":[{"step":"read","status":"completed"},{"step":"write","status":"in_progress"}]}`,
}

func (s *planServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		s.mu.Lock()
		n := len(s.inputs)
		items, _ := body["input"].([]any)
		s.inputs = append(s.inputs, items)
		s.mu.Unlock()
		stream, _ := body["stream"].(bool)
		if args, ok := planUpdates[n]; ok {
			id := fmt.Sprintf("fc_%d", n)
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":%q,\"name\":\"update_plan\"}}\n", id)
				fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":%q,\"delta\":%q}\n", id, args)
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			out, _ := json.Marshal(map[string]any{"id": "r", "status": "completed", "output": []any{map[string]any{
				"type": "function_call", "id": id, "call_id": id, "name": "update_plan", "arguments": args}}})
			_, _ = w.Write(out)
			return
		}
		text := fmt.Sprintf("answer %d", n)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n", text)
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":%q}]}]}`, text)
	}
}

func itemJSON(t *testing.T, items []any) []string {
	t.Helper()
	out := make([]string, len(items))
	for i, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(b)
	}
	return out
}

// sharedPrefix is the number of leading items two requests have in common.
func sharedPrefix(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func indexOf(items []string, sub string) int {
	for i, it := range items {
		if strings.Contains(it, sub) {
			return i
		}
	}
	return -1
}

// TestPlanAfterLatestInputKeepsTheHistoryPrefix drives two replies through
// the real Responses adapter; the second updates its plan twice. With the
// plan after the newest user message, the request after an update repeats
// the previous one through that message, so the first reply's history stays
// a cached prefix, and every request still ends with the reply's own tool
// output, never with a user message. With the plan at the head, the update
// breaks the prefix before the first reply's history.
func TestPlanAfterLatestInputKeepsTheHistoryPrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		after  bool
		stream bool
	}{
		{"after input stream", true, true},
		{"after input buffered", true, false},
		{"head stream", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := &planServer{}
			srv := httptest.NewServer(ps.handler(t))
			t.Cleanup(srv.Close)
			m, err := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{APIKey: "k", Model: "gpt-4.1", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			opts := []AgentOption{WithPlanTool(), WithModelStreaming(tc.stream), WithToolkit(tool.NewToolkit())}
			if tc.after {
				opts = append(opts, WithPlanAfterLatestInput())
			}
			a := NewUnifiedAgent("assistant", "sys", m, opts...)
			for _, q := range []string{"first question", "do the task"} {
				if _, err := a.Reply(context.Background(), q); err != nil {
					t.Fatalf("Reply(%q): %v", q, err)
				}
			}

			ps.mu.Lock()
			defer ps.mu.Unlock()
			if len(ps.inputs) != 4 {
				t.Fatalf("requests = %d, want 4", len(ps.inputs))
			}
			r1, r2, r3 := itemJSON(t, ps.inputs[1]), itemJSON(t, ps.inputs[2]), itemJSON(t, ps.inputs[3])
			if indexOf(r1, "[Current plan]") != -1 {
				t.Fatal("the first request of the second reply has no plan yet")
			}
			p2, p3 := indexOf(r2, "[Current plan]"), indexOf(r3, "[Current plan]")
			if p2 < 0 || p3 < 0 || !strings.Contains(r2[p2], "[in_progress] read") || !strings.Contains(r3[p3], "[in_progress] write") {
				t.Fatalf("plans not sent as updated: %q / %q", r2, r3)
			}
			history := indexOf(r2, "answer 0") // the first reply's answer
			query := indexOf(r2, "do the task")
			if history < 0 || query < 0 {
				t.Fatalf("request 2 lacks the history: %q", r2)
			}
			if !tc.after {
				if p2 > history {
					t.Fatal("the default layout must keep the plan ahead of the history")
				}
				if got := sharedPrefix(r2, r3); got > history {
					t.Fatalf("head layout: shared prefix %d covers the history at %d", got, history)
				}
				return
			}
			if p2 != query+1 || p3 != query+1 {
				t.Fatalf("the plan is not right after the newest user message (%d): at %d and %d", query, p2, p3)
			}
			for _, r := range [][]string{r2, r3} {
				if last := r[len(r)-1]; !strings.Contains(last, "function_call_output") {
					t.Fatalf("a request must end with the reply's tool output, not %q", last)
				}
			}
			if got := sharedPrefix(r1, r2); got != len(r1) {
				t.Fatalf("request 2 shares %d of request 1's %d items", got, len(r1))
			}
			if got := sharedPrefix(r2, r3); got != p2 {
				t.Fatalf("request 3 shares %d items with request 2, want everything before the plan (%d)", got, p2)
			}
		})
	}
}

func TestLatestInput(t *testing.T) {
	result := message.NewMsg("u", message.RoleUser, []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: "c"}})
	q := message.UserMsg("u", "q")
	reply := message.AssistantMsg("a", "answer")
	for _, tc := range []struct {
		ctx  []*message.Msg
		want int
	}{
		{nil, -1},
		{[]*message.Msg{reply}, -1},
		{[]*message.Msg{q, reply, result, nil}, 0},
		{[]*message.Msg{reply, q}, 1},
	} {
		if got := latestInput(tc.ctx); got != tc.want {
			t.Errorf("latestInput = %d, want %d", got, tc.want)
		}
	}
}

// TestPlanAfterLatestInputForkAndStructuredLayout: the structured response
// request uses the same layout as the reply (and falls back to the head
// without a user message), and a fork snapshot starts with the captured
// request.
func TestPlanAfterLatestInputForkAndStructuredLayout(t *testing.T) {
	plan := []tool.PlanItem{{Step: "read", Status: "in_progress"}}
	var got []string
	m := &structuredUsageModel{call: func(_ context.Context, msgs []*message.Msg, _ model.CallOptions) (*model.ChatResponse, error) {
		got = got[:0]
		for _, msg := range msgs {
			got = append(got, msgText(msg))
		}
		return &model.ChatResponse{Content: []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", Name: "generate_structured_output", Input: `{"report":"done"}`}},
			Usage: &model.ChatUsage{InputTokens: 3, OutputTokens: 4}}, nil
	}}
	for _, tc := range []struct {
		ctx  []*message.Msg
		want []string
	}{
		{[]*message.Msg{message.UserMsg("u", "hello"), message.AssistantMsg("a", "hi")},
			[]string{"system", "[Previous context summary]: own", "hello", "[Current plan]:\n1. [in_progress] read", "hi"}},
		{[]*message.Msg{message.AssistantMsg("a", "hi")},
			[]string{"system", "[Previous context summary]: own", "[Current plan]:\n1. [in_progress] read", "hi"}},
	} {
		state := &AgentState{SessionID: "s", Summary: "own", Plan: plan, Context: tc.ctx}
		a := NewUnifiedAgent("a", "system", m, WithState(state), WithPlanAfterLatestInput())
		if _, err := a.GenerateStructuredResponseWithUsage(context.Background(), json.RawMessage(`{"type":"object"}`)); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("structured request = %q, want %q", got, tc.want)
		}
	}

	sm := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return seamText("answer"), nil
	}}
	b := NewUnifiedAgent("b", "sys", sm, WithForkCapture(), WithPlanAfterLatestInput(),
		WithState(&AgentState{SessionID: "s2", Plan: plan}))
	if _, err := b.Reply(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	snap, err := b.ForkSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	sent := sm.call(0).msgs
	if len(snap.Messages) != len(sent)+1 {
		t.Fatalf("snapshot = %d messages, want the %d sent plus the answer", len(snap.Messages), len(sent))
	}
	for i := range sent {
		if msgText(snap.Messages[i]) != msgText(sent[i]) {
			t.Fatalf("snapshot message %d = %q, sent %q", i, msgText(snap.Messages[i]), msgText(sent[i]))
		}
	}
	if msgText(sent[len(sent)-2]) != "go" || !strings.HasPrefix(msgText(sent[len(sent)-1]), "[Current plan]") {
		t.Fatalf("the captured request does not carry the plan after the input: %q", msgText(sent[len(sent)-1]))
	}
}
