package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event/streamcheck"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/types"
)

// responsesCutObject is a Responses API response cut at max_output_tokens.
const responsesCutObject = `{"id":"resp_cut","object":"response","status":"incomplete","error":null,` +
	`"incomplete_details":{"reason":"max_output_tokens"},"max_output_tokens":16,` +
	`"output":[{"type":"message","id":"msg_1","status":"incomplete","role":"assistant",` +
	`"content":[{"type":"output_text","text":"partial answer","annotations":[]}]}],` +
	`"usage":{"input_tokens":12,"output_tokens":16,"total_tokens":28}}`

// streamFlags records the stream flag of each request the server receives.
type streamFlags struct {
	mu    sync.Mutex
	flags []bool
}

func (s *streamFlags) add(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags = append(s.flags, v)
}

func (s *streamFlags) get() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.flags...)
}

// newResponsesCutModel serves every request with a reply cut at
// max_output_tokens: the buffered object, or a stream that publishes the text
// and ends with response.incomplete.
func newResponsesCutModel(t *testing.T, streamed *streamFlags) model.ChatModel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s, want /v1/responses", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		streamed.add(req.Stream)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, responsesCutObject)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial answer\"}\n\n")
		fmt.Fprint(w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":"+responsesCutObject+"}\n\n")
	}))
	t.Cleanup(srv.Close)
	m, err := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{APIKey: "fixture", Model: "gpt-4.1", BaseURL: srv.URL, MaxOutputTokens: 16})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A Responses reply cut at max_output_tokens. On the buffered path the agent
// keeps the partial answer, completes the reply and emits
// model_partial_response, which TeamRun and the goal pipeline treat as
// incomplete. The streaming path ends the reply with the status as its
// error. Both keep the provider's usage.
func TestReplyResponsesIncompleteStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streaming bool
	}{
		{name: "buffered", streaming: false},
		{name: "streaming", streaming: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var streamed streamFlags
			a := NewUnifiedAgent("bot", "test", newResponsesCutModel(t, &streamed),
				WithModelStreaming(tc.streaming), WithModelConfig(ModelConfig{MaxRetries: 3, RetryDelay: time.Millisecond}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ch, err := a.ReplyStream(ctx, "question")
			if err != nil {
				t.Fatal(err)
			}
			var events []event.Event
			var partial []event.CustomEvent
			var usage []event.ModelCallEndEvent
			var end *event.ReplyEndEvent
			for e := range ch {
				events = append(events, e)
				switch v := e.(type) {
				case event.CustomEvent:
					if v.Name == "model_partial_response" {
						partial = append(partial, v)
					}
				case event.ModelCallEndEvent:
					usage = append(usage, v)
				case event.ReplyEndEvent:
					end = &v
				}
			}
			if err := ctx.Err(); err != nil {
				t.Fatal(err)
			}
			if err := streamcheck.Validate(events); err != nil {
				t.Fatal(err)
			}
			if got := streamed.get(); len(got) != 1 || got[0] != tc.streaming {
				t.Fatalf("requests (stream flags) = %v, want one with stream=%t", got, tc.streaming)
			}
			if len(usage) != 1 || usage[0].InputTokens != 12 || usage[0].OutputTokens != 16 {
				t.Fatalf("model_call_end usage = %+v, want one with 12/16", usage)
			}
			if end == nil {
				t.Fatal("no reply_end")
			}
			const want = "response incomplete: max_output_tokens"
			if !tc.streaming {
				if len(partial) != 1 {
					t.Fatalf("model_partial_response events = %d, want 1", len(partial))
				}
				if msg, _ := partial[0].Value["error"].(string); !strings.Contains(msg, want) {
					t.Fatalf("model_partial_response error = %q, want it to contain %q", msg, want)
				}
				if end.FinishedReason != types.ReplyCompleted {
					t.Fatalf("reply_end = %+v, want completed", end)
				}
				last := a.state.Context[len(a.state.Context)-1]
				if got := last.GetTextContent(""); got == nil || *got != "partial answer" {
					t.Fatalf("stored text = %v, want the partial answer", got)
				}
				return
			}
			if len(partial) != 0 {
				t.Fatalf("streaming path emitted model_partial_response: %+v", partial)
			}
			if end.FinishedReason != types.ReplyError || end.Error == nil || !strings.Contains(end.Error.Message, want) {
				t.Fatalf("reply_end = %+v, want an error naming %q", end, want)
			}
		})
	}
}
