package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// responsesStatusObject builds a Responses API response object in the shape
// the API documents (object, status, error, incomplete_details, output,
// usage). fields holds the status-specific members.
func responsesStatusObject(fields string) string {
	return `{"id":"resp_status","object":"response","created_at":1741476777,` + fields + `,` +
		`"model":"gpt-4.1-2025-04-14","max_output_tokens":16,` +
		`"output":[{"type":"message","id":"msg_1","status":"incomplete","role":"assistant",` +
		`"content":[{"type":"output_text","text":"partial answer","annotations":[]}]}],` +
		`"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":0},` +
		`"output_tokens":16,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":28}}`
}

// responsesStatusServer serves one fixed body and checks the captured
// request: POST /v1/responses with the expected stream flag and the
// configured max_output_tokens.
func responsesStatusServer(t *testing.T, wantStream bool, contentType, body string) ChatModel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream          bool `json:"stream"`
			MaxOutputTokens int  `json:"max_output_tokens"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s, want POST /v1/responses", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Stream != wantStream || req.MaxOutputTokens != 16 {
			t.Errorf("request stream=%t max_output_tokens=%d, want stream=%t max_output_tokens=16", req.Stream, req.MaxOutputTokens, wantStream)
		}
		w.Header().Set("Content-Type", contentType)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "k", Model: "gpt-4.1", BaseURL: srv.URL, MaxOutputTokens: 16})
	if err != nil {
		t.Fatalf("NewOpenAIResponseModel: %v", err)
	}
	return m
}

func responsesText(resp *ChatResponse) string {
	var sb strings.Builder
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func checkStatusError(t *testing.T, got error, want string) {
	t.Helper()
	switch {
	case want == "" && got != nil:
		t.Fatalf("unexpected status error: %v", got)
	case want != "" && (got == nil || !strings.Contains(got.Error(), want)):
		t.Fatalf("Error = %v, want it to contain %q", got, want)
	}
}

// A buffered Responses call cut at max_output_tokens (or failed) must not look
// complete: Error names the status and reason, and content, ID and usage stay.
func TestOpenAIResponseChatReportsStatus(t *testing.T) {
	for _, tc := range []struct {
		name, fields, wantErr string
	}{
		{"incomplete at max_output_tokens", `"status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"}`, "response incomplete: max_output_tokens"},
		{"incomplete by content filter", `"status":"incomplete","error":null,"incomplete_details":{"reason":"content_filter"}`, "response incomplete: content_filter"},
		{"failed with code and message", `"status":"failed","error":{"code":"server_error","message":"upstream failed"},"incomplete_details":null`, "response failed: server_error: upstream failed"},
		{"failed with code only", `"status":"failed","error":{"code":"server_error"},"incomplete_details":null`, "response failed: server_error"},
		{"failed with message only", `"status":"failed","error":{"message":"upstream failed"},"incomplete_details":null`, "response failed: upstream failed"},
		{"canceled", `"status":"cancelled","error":null,"incomplete_details":null`, "response cancelled"}, //nolint:misspell // The API spells this status "cancelled".
		{"completed", `"status":"completed","error":null,"incomplete_details":null`, ""},
		// Compatible servers may omit status; such a response is accepted as before.
		{"no status", `"error":null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := responsesStatusServer(t, false, "application/json", responsesStatusObject(tc.fields))
			resp, err := m.Chat(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if responsesText(resp) != "partial answer" || resp.ID != "resp_status" {
				t.Fatalf("content and ID must be kept: %+v", resp)
			}
			if resp.Usage == nil || resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 16 {
				t.Fatalf("usage must be kept: %+v", resp.Usage)
			}
			checkStatusError(t, resp.Error, tc.wantErr)
		})
	}
}

// responsesSSE frames one Responses stream event the way the API sends it.
func responsesSSE(data string) string {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &probe)
	return "event: " + probe.Type + "\ndata: " + data + "\n\n"
}

// responsesStatusStream publishes "partial answer", then the terminal event,
// then one more delta that a stream ended by the terminal event never reads.
func responsesStatusStream(terminal string) string {
	return responsesSSE(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_status","object":"response","status":"in_progress","output":[],"usage":null}}`) +
		responsesSSE(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`) +
		responsesSSE(`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial "}`) +
		responsesSSE(`{"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"answer"}`) +
		responsesSSE(terminal) +
		responsesSSE(`{"type":"response.output_text.delta","sequence_number":9,"item_id":"msg_1","output_index":0,"content_index":0,"delta":" AFTER_TERMINAL"}`)
}

// response.incomplete and response.failed end the stream with the status on
// Error, keeping the accumulated content, the response ID and usage. A
// response.completed event reporting another status is treated the same way.
func TestOpenAIResponseStreamReportsStatus(t *testing.T) {
	incomplete := `"status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"}`
	for _, tc := range []struct {
		name, terminal, wantErr, wantID string
		wantUsage, wantContextLimit     bool
	}{
		{name: "incomplete", terminal: `{"type":"response.incomplete","sequence_number":4,"response":` + responsesStatusObject(incomplete) + `}`,
			wantErr: "response incomplete: max_output_tokens", wantID: "resp_status", wantUsage: true},
		{name: "incomplete without response object", terminal: `{"type":"response.incomplete","sequence_number":4}`,
			wantErr: "response incomplete"},
		{name: "incomplete event with a completed object", terminal: `{"type":"response.incomplete","sequence_number":4,"response":{"id":"resp_status","status":"completed"}}`,
			wantErr: "response incomplete", wantID: "resp_status"},
		{name: "failed", terminal: `{"type":"response.failed","sequence_number":4,"response":` + responsesStatusObject(`"status":"failed","error":{"code":"server_error","message":"upstream failed"},"incomplete_details":null`) + `}`,
			wantErr: "response failed: server_error: upstream failed", wantID: "resp_status", wantUsage: true},
		{name: "failed with code only", terminal: `{"type":"response.failed","sequence_number":4,"response":{"id":"resp_status","status":"failed","error":{"code":"server_error","message":""}}}`,
			wantErr: "response failed: server_error", wantID: "resp_status"},
		// The agent's context-overflow recovery matches the provider code.
		{name: "failed on context window", terminal: `{"type":"response.failed","sequence_number":4,"response":{"id":"resp_status","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."},"usage":null}}`,
			wantErr: "response failed: context_length_exceeded: Your input exceeds", wantID: "resp_status", wantContextLimit: true},
		{name: "completed event reporting incomplete", terminal: `{"type":"response.completed","sequence_number":4,"response":` + responsesStatusObject(incomplete) + `}`,
			wantErr: "response incomplete: max_output_tokens", wantID: "resp_status", wantUsage: true},
		{name: "completed", terminal: `{"type":"response.completed","sequence_number":4,"response":` + responsesStatusObject(`"status":"completed","error":null,"incomplete_details":null`) + `}`,
			wantID: "resp_status", wantUsage: true},
		{name: "completed without status", terminal: `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_status"}}`,
			wantID: "resp_status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := responsesStatusServer(t, true, "text/event-stream", responsesStatusStream(tc.terminal))
			ch, err := m.ChatStream(context.Background(), []*message.Msg{message.UserMsg("u", "hi")})
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			var final *ChatResponse
			var deltas strings.Builder
			lasts := 0
			for r := range ch {
				if !r.IsLast {
					deltas.WriteString(responsesText(&r))
					continue
				}
				lasts++
				cp := r
				final = &cp
			}
			if lasts != 1 {
				t.Fatalf("IsLast responses = %d, want exactly 1", lasts)
			}
			if deltas.String() != "partial answer" || responsesText(final) != "partial answer" {
				t.Fatalf("stream must end at the terminal event keeping its content: deltas=%q final=%q", deltas.String(), responsesText(final))
			}
			if final.ID != tc.wantID {
				t.Fatalf("final ID = %q, want %q", final.ID, tc.wantID)
			}
			if tc.wantUsage && (final.Usage == nil || final.Usage.InputTokens != 12 || final.Usage.OutputTokens != 16) {
				t.Fatalf("usage must be kept: %+v", final.Usage)
			}
			checkStatusError(t, final.Error, tc.wantErr)
			if got := IsContextLimitExceeded(final.Error); got != tc.wantContextLimit {
				t.Fatalf("IsContextLimitExceeded(%v) = %t, want %t", final.Error, got, tc.wantContextLimit)
			}
		})
	}
}

type responsesTransport func(*http.Request) (*http.Response, error)

func (f responsesTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The terminal status events use the same cancellation-aware send as the
// other stream paths. The stream fills the channel buffer with deltas, so the
// producer blocks on its final send; a consumer that stops reading and
// cancels must see the channel close without a final response.
func TestOpenAIResponseStreamStatusCancel(t *testing.T) {
	const buffered = 32 // capacity of the ChatStream channel
	for _, terminal := range []string{
		`{"type":"response.incomplete","response":{"id":"resp_status","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
		`{"type":"response.failed","response":{"id":"resp_status","status":"failed","error":{"code":"server_error","message":"upstream failed"}}}`,
	} {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(terminal), &probe)
		t.Run(probe.Type, func(t *testing.T) {
			var body strings.Builder
			for i := 0; i < buffered; i++ {
				body.WriteString(sseDeltaLine("x"))
			}
			body.WriteString(responsesSSE(terminal))
			client := &http.Client{Transport: responsesTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body.String())),
				}, nil
			})}
			synctest.Test(t, func(t *testing.T) {
				m, err := NewOpenAIResponseModel(&OpenAIResponseConfig{APIKey: "k", HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ch, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("u", "hi")})
				if err != nil {
					t.Fatalf("ChatStream: %v", err)
				}
				if cap(ch) != buffered {
					t.Fatalf("channel capacity = %d, want %d", cap(ch), buffered)
				}
				// Every delta is buffered and the producer is blocked sending
				// the terminal response.
				synctest.Wait()
				cancel()
				n := 0
				for r := range ch {
					if r.IsLast {
						t.Fatalf("final response delivered after cancellation: %+v", r)
					}
					n++
				}
				if n != buffered {
					t.Fatalf("deltas = %d, want %d", n, buffered)
				}
			})
		})
	}
}
