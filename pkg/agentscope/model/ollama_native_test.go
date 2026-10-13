package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// readBody captures the raw request body for wire-format assertions.
func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return body
}

func newNativeModel(t *testing.T, srv *httptest.Server, numCtx int) *OllamaNativeChatModel {
	t.Helper()
	m, err := NewOllamaNativeChatModel(OllamaNativeConfig{
		BaseURL:    srv.URL,
		Model:      "qwen2.5:0.5b",
		NumCtx:     numCtx,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

func TestOllamaNativeConfig_MissingFields(t *testing.T) {
	_, err := NewOllamaNativeChatModel(OllamaNativeConfig{})
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("expected Model error, got %v", err)
	}
}

func TestOllamaNativeConfig_NegativeNumCtx(t *testing.T) {
	_, err := NewOllamaNativeChatModel(OllamaNativeConfig{Model: "m", NumCtx: -1})
	if err == nil || !strings.Contains(err.Error(), "NumCtx must not be negative") {
		t.Fatalf("expected NumCtx error, got %v", err)
	}
}

func TestOllamaNativeConfig_DefaultBaseURL(t *testing.T) {
	m, err := NewOllamaNativeChatModel(OllamaNativeConfig{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if m.baseURL != defaultOllamaBaseURL {
		t.Errorf("baseURL = %q, want %q", m.baseURL, defaultOllamaBaseURL)
	}
}

// TestOllamaNative_ContextSize proves ContextSizer is implemented with
// OllamaNativeConfig.NumCtx (zero meaning unknown), and that the model does
// NOT implement ModelNamer (model card lookup must not kick in).
func TestOllamaNative_ContextSize(t *testing.T) {
	m, err := NewOllamaNativeChatModel(OllamaNativeConfig{Model: "m", NumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	cs, ok := interface{}(m).(ContextSizer)
	if !ok {
		t.Fatal("OllamaNativeChatModel should implement ContextSizer")
	}
	if cs.ContextSize() != 8192 {
		t.Errorf("ContextSize() = %d, want 8192", cs.ContextSize())
	}

	m0, err := NewOllamaNativeChatModel(OllamaNativeConfig{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if m0.ContextSize() != 0 {
		t.Errorf("ContextSize() = %d, want 0 (unknown)", m0.ContextSize())
	}

	if _, ok := interface{}(m).(ModelNamer); ok {
		t.Error("OllamaNativeChatModel must not implement ModelNamer")
	}
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

// TestOllamaNative_WireFormat asserts the exact /api/chat request body: stream
// flag, options (num_ctx/num_predict/temperature/top_p/seed), think, and both
// format shapes.
func TestOllamaNative_WireFormat(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 4096)

	_, err := m.Chat(context.Background(), testMessages(),
		WithMaxTokens(64),
		WithTemperature(0.7),
		WithTopP(0.9),
		WithSeed(42),
		WithThinking(true, 0),
		WithResponseFormat(&ResponseFormat{Type: "json_object"}),
	)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("captured body is not JSON: %v\n%s", err, captured)
	}

	if body["model"] != "qwen2.5:0.5b" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != false {
		t.Errorf("stream = %v, want false for Chat", body["stream"])
	}
	if body["think"] != true {
		t.Errorf("think = %v, want true", body["think"])
	}
	if body["format"] != "json" {
		t.Errorf("format = %v (%T), want the string \"json\"", body["format"], body["format"])
	}

	options, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("options missing or not an object: %s", captured)
	}
	if options["num_ctx"] != float64(4096) {
		t.Errorf("num_ctx = %v", options["num_ctx"])
	}
	if options["num_predict"] != float64(64) {
		t.Errorf("num_predict = %v", options["num_predict"])
	}
	if options["temperature"] != 0.7 {
		t.Errorf("temperature = %v", options["temperature"])
	}
	if options["top_p"] != 0.9 {
		t.Errorf("top_p = %v", options["top_p"])
	}
	if options["seed"] != float64(42) {
		t.Errorf("seed = %v", options["seed"])
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "Hello" {
		t.Errorf("message[0] = %v", first)
	}
}

// TestOllamaNative_WireFormat_Minimal proves num_ctx and the whole options
// object are omitted when nothing is set (zero NumCtx = server default), and
// that format/think stay off the wire.
func TestOllamaNative_WireFormat_Minimal(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	if _, err := m.Chat(context.Background(), testMessages()); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	raw := string(captured)
	for _, key := range []string{`"options"`, `"num_ctx"`, `"format"`, `"think"`} {
		if strings.Contains(raw, key) {
			t.Errorf("unset %s must be omitted from the wire: %s", key, raw)
		}
	}
}

// TestOllamaNative_WireFormat_JSONSchema proves json_schema maps to the
// embedded schema object, not a string.
func TestOllamaNative_WireFormat_JSONSchema(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	m := newNativeModel(t, srv, 0)
	_, err := m.Chat(context.Background(), testMessages(),
		WithResponseFormat(&ResponseFormat{Type: "json_schema", JSONSchema: schema}),
	)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	format, ok := body["format"].(map[string]any)
	if !ok {
		t.Fatalf("format = %v (%T), want an embedded schema object", body["format"], body["format"])
	}
	if format["type"] != "object" {
		t.Errorf("format.type = %v", format["type"])
	}
}

// TestOllamaNative_WireFormat_Stream proves ChatStream sends stream:true.
func TestOllamaNative_WireFormat_Stream(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, ollamaNativeStreamNDJSON)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for range ch {
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true for ChatStream", body["stream"])
	}
}

// TestOllamaNative_MessageRoleMapping proves system/user/assistant roles stay
// verbatim on the wire.
func TestOllamaNative_MessageRoleMapping(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	msgs := []*message.Msg{
		message.SystemMsg("sys", "be brief"),
		message.UserMsg("u", "hi"),
		message.AssistantMsg("a", "hello"),
	}

	m := newNativeModel(t, srv, 0)
	if _, err := m.Chat(context.Background(), msgs); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body struct {
		Messages []ollamaNativeMessage `json:"messages"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	wantRoles := []string{"system", "user", "assistant"}
	if len(body.Messages) != len(wantRoles) {
		t.Fatalf("messages = %+v", body.Messages)
	}
	for i, want := range wantRoles {
		if body.Messages[i].Role != want {
			t.Errorf("messages[%d].role = %q, want %q", i, body.Messages[i].Role, want)
		}
	}
}

// TestOllamaNative_ToolBlocksNotReplayed proves history tool exchanges are not
// replayed half-way: an assistant ToolCallBlock produces no tool_calls field,
// an orphan ToolResultBlock produces no role "tool" message, and a message
// holding only skipped blocks does not turn into an empty wire message.
func TestOllamaNative_ToolBlocksNotReplayed(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	msgs := []*message.Msg{
		message.UserMsg("u", "what is the weather?"),
		message.AssistantMsg("a", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "let me check"},
			message.ToolCallBlock{Type: "tool_call", ID: "call_1", Name: "search", Input: `{"q":"weather"}`},
		}),
		{
			ID:   "toolres",
			Role: message.RoleUser,
			Name: "u",
			Content: []message.ContentBlock{
				message.ToolResultBlock{Type: "tool_result", ID: "call_1", Name: "search", Output: "found it"},
			},
		},
		message.UserMsg("u", "thanks"),
	}

	m := newNativeModel(t, srv, 0)
	if _, err := m.Chat(context.Background(), msgs); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	raw := string(captured)
	if strings.Contains(raw, "tool_calls") || strings.Contains(raw, `"role":"tool"`) || strings.Contains(raw, "found it") {
		t.Errorf("tool protocol artifacts must not reach the wire: %s", raw)
	}

	var body struct {
		Messages []ollamaNativeMessage `json:"messages"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	// The tool-result-only message collapses to nothing; what remains is
	// user, assistant(text only), user.
	wantRoles := []string{"user", "assistant", "user"}
	if len(body.Messages) != len(wantRoles) {
		t.Fatalf("messages = %+v", body.Messages)
	}
	for i, want := range wantRoles {
		if body.Messages[i].Role != want {
			t.Errorf("messages[%d].role = %q, want %q", i, body.Messages[i].Role, want)
		}
	}
	if body.Messages[1].Content != "let me check" {
		t.Errorf("assistant text = %q", body.Messages[1].Content)
	}
}

// TestOllamaNative_ImageDataBlock proves a base64 image DataBlock rides the
// native message's images array alongside its text.
func TestOllamaNative_ImageDataBlock(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	msgs := []*message.Msg{
		message.UserMsg("u", []message.ContentBlock{
			message.TextBlock{Type: "text", Text: "what is in this image?"},
			message.DataBlock{
				Type:   "data",
				ID:     "img1",
				Source: message.Base64Source{Type: "base64", MediaType: "image/png", Data: "aGVsbG8="},
			},
		}),
	}

	m := newNativeModel(t, srv, 0)
	if _, err := m.Chat(context.Background(), msgs); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body struct {
		Messages []ollamaNativeMessage `json:"messages"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	got := body.Messages[0]
	if got.Role != "user" || got.Content != "what is in this image?" {
		t.Errorf("message = %+v", got)
	}
	if len(got.Images) != 1 || got.Images[0] != "aGVsbG8=" {
		t.Errorf("images = %v, want [aGVsbG8=]", got.Images)
	}
}

// TestOllamaNative_WireFormat_ThinkFalse proves WithThinkingDisabled maps to
// an explicit "think":false on the wire (not an omitted field).
func TestOllamaNative_WireFormat_ThinkFalse(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	if _, err := m.Chat(context.Background(), testMessages(), WithThinkingDisabled()); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	think, ok := body["think"]
	if !ok {
		t.Fatalf("think missing from wire: %s", captured)
	}
	if think != false {
		t.Errorf("think = %v, want false", think)
	}
}

// ---------------------------------------------------------------------------
// Non-streaming response parsing
// ---------------------------------------------------------------------------

const ollamaNativeNonStreamJSON = `{
	"model": "qwen2.5:0.5b",
	"created_at": "2026-10-12T08:46:10.122296Z",
	"message": {"role": "assistant", "content": "Go is a portable language."},
	"done": true,
	"done_reason": "stop",
	"total_duration": 786307375,
	"load_duration": 567714625,
	"prompt_eval_count": 33,
	"prompt_eval_cached_count": 0,
	"prompt_eval_duration": 150107000,
	"eval_count": 25,
	"eval_duration": 66360000
}`

func TestOllamaNative_Chat_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaNativeNonStreamJSON)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	resp, err := m.Chat(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !resp.IsLast {
		t.Error("IsLast should be true for non-streaming Chat")
	}
	if got := resp.GetTextContent(); got != "Go is a portable language." {
		t.Errorf("content = %q", got)
	}
	if resp.Usage == nil {
		t.Fatal("usage missing")
	}
	if resp.Usage.InputTokens != 33 || resp.Usage.OutputTokens != 25 {
		t.Errorf("usage = %+v, want input 33 / output 25", resp.Usage)
	}
	if resp.StopReason != StopReasonStop {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, StopReasonStop)
	}
	if resp.ModelName != "qwen2.5:0.5b" {
		t.Errorf("ModelName = %q", resp.ModelName)
	}
}

// TestOllamaNative_Chat_UsageUnknown proves a response without eval counts
// leaves Usage nil (unknown), distinct from a reported zero.
func TestOllamaNative_Chat_UsageUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	resp, err := m.Chat(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage != nil {
		t.Errorf("Usage = %+v, want nil (unknown)", resp.Usage)
	}
}

// TestOllamaNative_Chat_ThinkingOnly proves a response whose content is empty
// but whose message.thinking is set (the default for thinking models such as
// qwen3 since Ollama 0.12.4) surfaces the reasoning as a ThinkingBlock instead
// of returning empty content without error.
func TestOllamaNative_Chat_ThinkingOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":"","thinking":"let me reason"},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":4}`)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	resp, err := m.Chat(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("Content = %+v, want exactly one ThinkingBlock", resp.Content)
	}
	tb, ok := resp.Content[0].(message.ThinkingBlock)
	if !ok {
		t.Fatalf("Content[0] = %T, want message.ThinkingBlock", resp.Content[0])
	}
	if tb.Thinking != "let me reason" {
		t.Errorf("Thinking = %q", tb.Thinking)
	}
	if resp.GetTextContent() != "" {
		t.Errorf("text content should be empty, got %q", resp.GetTextContent())
	}
}

// TestOllamaNative_Chat_ThinkingBeforeContent proves that when both are
// present the ThinkingBlock precedes the TextBlock.
func TestOllamaNative_Chat_ThinkingBeforeContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":"the answer","thinking":"let me reason"},"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	resp, err := m.Chat(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("Content = %+v, want [ThinkingBlock, TextBlock]", resp.Content)
	}
	if tb, ok := resp.Content[0].(message.ThinkingBlock); !ok || tb.Thinking != "let me reason" {
		t.Errorf("Content[0] = %+v, want ThinkingBlock", resp.Content[0])
	}
	if tb, ok := resp.Content[1].(message.TextBlock); !ok || tb.Text != "the answer" {
		t.Errorf("Content[1] = %+v, want TextBlock", resp.Content[1])
	}
}

func TestOllamaNative_Chat_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model 'nope' not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	_, err := m.Chat(context.Background(), testMessages())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tools are rejected, not dropped
// ---------------------------------------------------------------------------

func TestOllamaNative_ToolsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not reach the server when tools are set")
	}))
	defer srv.Close()

	tools := []ToolSchema{{
		Type: "function",
		Function: ToolFunction{
			Name:        "get_weather",
			Description: "Get current weather",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}}

	m := newNativeModel(t, srv, 0)
	_, err := m.Chat(context.Background(), testMessages(), WithTools(tools))
	if err == nil || !strings.Contains(err.Error(), "tools") {
		t.Fatalf("Chat with tools: expected explicit error, got %v", err)
	}
	_, err = m.ChatStream(context.Background(), testMessages(), WithTools(tools))
	if err == nil || !strings.Contains(err.Error(), "tools") {
		t.Fatalf("ChatStream with tools: expected explicit error, got %v", err)
	}
}

func TestOllamaNative_EmptyMessages(t *testing.T) {
	m, err := NewOllamaNativeChatModel(OllamaNativeConfig{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Chat(context.Background(), nil); err == nil {
		t.Error("Chat: expected error for empty messages")
	}
	if _, err := m.ChatStream(context.Background(), nil); err == nil {
		t.Error("ChatStream: expected error for empty messages")
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// Mirrors the shape of a captured real /api/chat stream (see
// /tmp/ollama-samples/chat_stream.ndjson): content deltas with done=false,
// then a final done=true frame with empty content carrying usage.
const ollamaNativeStreamNDJSON = `{"model":"qwen2.5:0.5b","created_at":"2026-10-12T08:46:10.218743Z","message":{"role":"assistant","content":"Hel"},"done":false}
{"model":"qwen2.5:0.5b","created_at":"2026-10-12T08:46:10.222153Z","message":{"role":"assistant","content":"lo"},"done":false}
{"model":"qwen2.5:0.5b","created_at":"2026-10-12T08:46:10.252328Z","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","total_duration":111920791,"load_duration":1423250,"prompt_eval_count":32,"prompt_eval_cached_count":24,"prompt_eval_duration":75597000,"eval_count":13,"eval_duration":33559000}
`

func collectNativeStream(t *testing.T, ch <-chan ChatResponse) []ChatResponse {
	t.Helper()
	var out []ChatResponse
	for r := range ch {
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("stream produced no responses")
	}
	return out
}

func TestOllamaNative_ChatStream_MergesDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, ollamaNativeStreamNDJSON)
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)

	last := resps[len(resps)-1]
	if !last.IsLast {
		t.Fatal("last response should be IsLast")
	}
	if last.Error != nil {
		t.Fatalf("unexpected error: %v", last.Error)
	}
	if got := last.GetTextContent(); got != "Hello" {
		t.Errorf("accumulated content = %q, want %q", got, "Hello")
	}
	if last.Usage == nil {
		t.Fatal("usage from final done frame missing")
	}
	if last.Usage.InputTokens != 32 || last.Usage.OutputTokens != 13 {
		t.Errorf("usage = %+v, want input 32 / output 13", last.Usage)
	}
	if last.StopReason != StopReasonStop {
		t.Errorf("StopReason = %q, want %q", last.StopReason, StopReasonStop)
	}

	// Delta frames carry their chunk, not the accumulated text.
	var deltaText string
	for _, r := range resps[:len(resps)-1] {
		if r.IsLast {
			t.Error("delta response marked IsLast")
		}
		deltaText += r.GetTextContent()
	}
	if deltaText != "Hello" {
		t.Errorf("concatenated deltas = %q, want %q", deltaText, "Hello")
	}
}

// TestOllamaNative_ChatStream_DoneReasonLength proves done_reason "length"
// maps to StopReasonLength on the final response.
func TestOllamaNative_ChatStream_DoneReasonLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"cut"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":5,"eval_count":2}`+"\n")
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)
	last := resps[len(resps)-1]
	if last.StopReason != StopReasonLength {
		t.Errorf("StopReason = %q, want %q", last.StopReason, StopReasonLength)
	}
	if last.Error != nil {
		t.Errorf("done_reason length is a normal completion, got error: %v", last.Error)
	}
}

// TestOllamaNative_ChatStream_Truncated proves a stream that ends without a
// done=true frame (connection dropped mid-generation) yields a final response
// with Error set AND the partial content preserved.
func TestOllamaNative_ChatStream_Truncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"partial"},"done":false}`+"\n")
		// connection ends here: no done frame
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)

	last := resps[len(resps)-1]
	if !last.IsLast {
		t.Error("final response should be IsLast")
	}
	if last.Error == nil {
		t.Fatal("truncated stream must set ChatResponse.Error")
	}
	if got := last.GetTextContent(); got != "partial" {
		t.Errorf("partial content = %q, want %q", got, "partial")
	}
	if last.Usage != nil {
		t.Errorf("truncated stream has no usage, got %+v", last.Usage)
	}
}

// TestOllamaNative_ChatStream_MalformedFrame proves a non-JSON line becomes a
// terminal error on the final response instead of a silent end.
func TestOllamaNative_ChatStream_MalformedFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"pa"},"done":false}`+"\n")
		fmt.Fprint(w, "{bad json\n")
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)
	last := resps[len(resps)-1]
	if last.Error == nil {
		t.Fatal("malformed frame must set ChatResponse.Error")
	}
	if got := last.GetTextContent(); got != "pa" {
		t.Errorf("partial content = %q", got)
	}
}

// TestOllamaNative_ChatStream_ContextCancel proves canceling mid-stream
// terminates the channel promptly.
func TestOllamaNative_ChatStream_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}`+"\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(ctx, testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	first := <-ch
	if first.GetTextContent() != "hi" {
		t.Fatalf("first delta = %q", first.GetTextContent())
	}
	cancel()

	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed: stream terminated
			}
		case <-timeout:
			t.Fatal("stream did not terminate after context cancel")
		}
	}
}

// TestOllamaNative_ConvertMessages_EdgeCases covers the converter branches not
// reachable through ordinary UserMsg construction: a block-less message keeps
// an empty-content turn, a URL-sourced image is skipped (native images are
// base64-only), and hint text folds into the message content.
func TestOllamaNative_ConvertMessages_EdgeCases(t *testing.T) {
	msgs := []*message.Msg{
		nil,
		{ID: "empty", Role: message.RoleUser, Name: "u"},
		{
			ID:   "urlimg",
			Role: message.RoleUser,
			Name: "u",
			Content: []message.ContentBlock{
				message.DataBlock{Type: "data", ID: "d1", Source: message.URLSource{Type: "url", URL: "https://example.com/a.png"}},
			},
		},
		{
			ID:   "hint",
			Role: message.RoleUser,
			Name: "u",
			Content: []message.ContentBlock{
				message.HintBlock{Type: "hint", Hint: "remember this"},
			},
		},
	}

	out := convertMessagesToOllamaNative(msgs)
	if len(out) != 2 {
		t.Fatalf("messages = %+v", out)
	}
	if out[0].Role != "user" || out[0].Content != "" {
		t.Errorf("empty message = %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "remember this" {
		t.Errorf("hint message = %+v", out[1])
	}
	for _, m := range out {
		if len(m.Images) != 0 {
			t.Errorf("URL-sourced image must not reach the wire: %+v", m)
		}
	}
}

func TestOllamaNative_CountTokens(t *testing.T) {
	m, err := NewOllamaNativeChatModel(OllamaNativeConfig{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []*message.Msg{
		message.SystemMsg("system", "You are a helpful assistant."),
		message.UserMsg("user", "What is the capital of France?"),
	}
	if n := m.CountTokens(msgs, nil); n <= 0 {
		t.Errorf("CountTokens = %d, want > 0", n)
	}
	if n := m.CountTokens(nil, nil); n != 0 {
		t.Errorf("CountTokens(nil, nil) = %d, want 0", n)
	}
}

// TestOllamaNative_ChatStream_ZeroUsage locks in the zero-vs-unknown usage
// contract: a done frame that explicitly reports zero eval counts yields a
// non-nil Usage with zero fields, distinct from an absent count (nil Usage).
func TestOllamaNative_ChatStream_ZeroUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":0,"eval_count":0}`+"\n")
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)
	last := resps[len(resps)-1]
	if last.Usage == nil {
		t.Fatal("explicit zero usage must produce a non-nil Usage")
	}
	if last.Usage.InputTokens != 0 || last.Usage.OutputTokens != 0 {
		t.Errorf("usage = %+v, want explicit zeros", last.Usage)
	}
}

// TestOllamaNative_ChatStream_NoDoneReason proves a done frame without a
// done_reason leaves StopReason empty (the provider did not report one).
func TestOllamaNative_ChatStream_NoDoneReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":3,"eval_count":1}`+"\n")
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)
	last := resps[len(resps)-1]
	if last.Error != nil {
		t.Fatalf("unexpected error: %v", last.Error)
	}
	if last.StopReason != "" {
		t.Errorf("StopReason = %q, want empty (no done_reason reported)", last.StopReason)
	}
}

// TestOllamaNative_ChatStream_DrainsAfterDone proves that frames arriving
// after the done frame (misbehaving server) are still consumed: enough
// trailing frames to fill the NDJSON channel buffer would otherwise block
// parseNDJSONStream on send forever, leaking its goroutine.
func TestOllamaNative_ChatStream_DrainsAfterDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"hi"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":1}`+"\n")
		// Far more trailing frames than the NDJSON channel buffer (16).
		for i := 0; i < 100; i++ {
			fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"junk"},"done":false}`+"\n")
		}
	}))
	defer srv.Close()

	before := runtime.NumGoroutine()

	// Disable keep-alives so the connection's read/write loops exit with the
	// body instead of lingering in the idle pool and polluting the count.
	m, err := NewOllamaNativeChatModel(OllamaNativeConfig{
		BaseURL:    srv.URL,
		Model:      "qwen2.5:0.5b",
		HTTPClient: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)

	last := resps[len(resps)-1]
	if last.Error != nil {
		t.Fatalf("unexpected error: %v", last.Error)
	}
	if got := last.GetTextContent(); got != "hi" {
		t.Errorf("content = %q, want %q (trailing frames must not leak into the reply)", got, "hi")
	}
	if last.Usage == nil || last.Usage.InputTokens != 3 || last.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v, want input 3 / output 1", last.Usage)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines still running, baseline %d (producer blocked on send after done?)",
				runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestOllamaNative_ChatStream_ThinkingDeltas proves per-frame thinking deltas
// are published (including thinking-only frames), precede text within the same
// chunk, accumulate independently, and the final response assembles
// ThinkingBlock before TextBlock.
func TestOllamaNative_ChatStream_ThinkingDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":"","thinking":"first "},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":"","thinking":"second"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":"answer","thinking":"!"},"done":false}`+"\n")
		fmt.Fprint(w, `{"model":"qwen3","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":9,"eval_count":4}`+"\n")
	}))
	defer srv.Close()

	m := newNativeModel(t, srv, 0)
	ch, err := m.ChatStream(context.Background(), testMessages())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	resps := collectNativeStream(t, ch)
	if len(resps) != 4 {
		t.Fatalf("got %d responses, want 3 deltas + final: %+v", len(resps), resps)
	}

	// Frame 1 and 2: thinking-only deltas are published, not withheld.
	for i, want := range []string{"first ", "second"} {
		if len(resps[i].Content) != 1 {
			t.Fatalf("delta %d Content = %+v", i, resps[i].Content)
		}
		tb, ok := resps[i].Content[0].(message.ThinkingBlock)
		if !ok || tb.Thinking != want {
			t.Errorf("delta %d = %+v, want ThinkingBlock %q", i, resps[i].Content[0], want)
		}
	}

	// Frame 3: thinking before text within the same chunk.
	if len(resps[2].Content) != 2 {
		t.Fatalf("delta 2 Content = %+v", resps[2].Content)
	}
	if tb, ok := resps[2].Content[0].(message.ThinkingBlock); !ok || tb.Thinking != "!" {
		t.Errorf("delta 2 block 0 = %+v, want ThinkingBlock %q", resps[2].Content[0], "!")
	}
	if tb, ok := resps[2].Content[1].(message.TextBlock); !ok || tb.Text != "answer" {
		t.Errorf("delta 2 block 1 = %+v, want TextBlock %q", resps[2].Content[1], "answer")
	}

	// Final: accumulated ThinkingBlock before TextBlock.
	last := resps[3]
	if !last.IsLast || last.Error != nil {
		t.Fatalf("final = %+v", last)
	}
	if len(last.Content) != 2 {
		t.Fatalf("final Content = %+v, want [ThinkingBlock, TextBlock]", last.Content)
	}
	if tb, ok := last.Content[0].(message.ThinkingBlock); !ok || tb.Thinking != "first second!" {
		t.Errorf("final thinking = %+v, want accumulated %q", last.Content[0], "first second!")
	}
	if tb, ok := last.Content[1].(message.TextBlock); !ok || tb.Text != "answer" {
		t.Errorf("final text = %+v, want %q", last.Content[1], "answer")
	}
	if last.Usage == nil || last.Usage.InputTokens != 9 || last.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want input 9 / output 4", last.Usage)
	}
}
