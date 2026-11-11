package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/middleware"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

func forkEchoTool() tool.Tool {
	return tool.NewFunctionTool("echo", "Echo the input", json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		func(context.Context, map[string]any) (any, error) { return "echoed", nil })
}

// toolThenText answers the first call with a tool call and later calls with text.
func toolThenText(text string) func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
	return func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamToolCalls(message.ToolCallBlock{ID: "c1", Name: "echo"}), nil
		}
		return seamText(text), nil
	}
}

func TestForkCaptureOffByDefault(t *testing.T) {
	m := &seamModel{respond: toolThenText("final")}
	a := NewUnifiedAgent("main", "sys", m, WithToolkit(tool.NewToolkit(forkEchoTool())))
	if _, err := a.Reply(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if a.lastCapture != nil {
		t.Fatal("nothing may be captured without WithForkCapture")
	}
	if _, err := a.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
		t.Fatalf("ForkSnapshot = %v, want ErrNoForkSnapshot", err)
	}
}

func TestForkSnapshotAfterReply(t *testing.T) {
	m := &seamModel{respond: toolThenText("final")}
	a := NewUnifiedAgent("main", "sys", m, WithToolkit(tool.NewToolkit(forkEchoTool())), WithForkCapture())
	if _, err := a.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
		t.Fatalf("no capture yet: %v", err)
	}
	if _, err := a.Reply(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	snap, err := a.ForkSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	last := m.call(1).msgs // the request the snapshot captured
	if len(snap.Messages) != len(last)+1 {
		t.Fatalf("snapshot has %d messages, want the %d captured plus one", len(snap.Messages), len(last))
	}
	for i := range last {
		if snap.Messages[i].Role != last[i].Role || !reflect.DeepEqual(snap.Messages[i].Content, last[i].Content) {
			t.Fatalf("message %d differs from the captured request", i)
		}
	}
	delta := snap.Messages[len(last)]
	if delta.Role != message.RoleAssistant || delta.Name != "main" || msgText(delta) != "final" || len(delta.Content) != 1 {
		t.Fatalf("delta = %+v", delta)
	}
	if !reflect.DeepEqual(snap.Tools, a.toolkit.GetToolSchemas()) {
		t.Fatal("snapshot tools must be the captured schemas")
	}
	// The snapshot is a copy: editing it leaves the agent untouched.
	snap.Messages[1].Content[0] = message.TextBlock{Type: "text", Text: "edited"}
	if msgText(a.state.Context[0]) != "go" {
		t.Fatal("snapshot messages must not alias the context")
	}
}

func TestForkSnapshotDeltaMergesOwnMessages(t *testing.T) {
	ctx := context.Background()
	a := NewUnifiedAgent("edge", "sys", &seamModel{}, WithForkCapture())
	a.state.Context = []*message.Msg{message.UserMsg("u", "question")}
	a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(ctx)})

	asst := message.AssistantMsg("edge", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: "c1", Name: "echo", Input: `{}`}})
	asst.Content = append(asst.Content, message.ToolResultBlock{Type: "tool_result", ID: "c1", Name: "echo", Output: "r1", State: message.ToolResultSuccess})
	more := message.AssistantMsg("edge", "more")
	imported := importedMsg(4, "edge entry")
	after := message.AssistantMsg("edge", "after")
	a.state.Context = append(a.state.Context, asst, more, imported, after)

	snap, err := a.ForkSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 5 {
		t.Fatalf("got %d messages, want system, question, merged own, imported, own", len(snap.Messages))
	}
	merged := snap.Messages[2]
	if merged.Role != message.RoleAssistant || len(merged.Content) != 3 || msgText(merged) != "more" {
		t.Fatalf("own messages must merge into one assistant message: %+v", merged.Content)
	}
	if got := snap.Messages[3]; got.Role != message.RoleUser || msgText(got) != "edge entry" || got == imported {
		t.Fatal("other messages keep their role and are copied")
	}
	if seq, ok := ThreadSeqOf(snap.Messages[3]); !ok || seq != 4 {
		t.Fatal("the copied entry keeps its seq")
	}
	if msgText(snap.Messages[4]) != "after" {
		t.Fatal("own content after another message forms its own assistant message")
	}
}

func TestForkSnapshotRefusals(t *testing.T) {
	ctx := context.Background()
	setup := func() *UnifiedAgent {
		a := NewUnifiedAgent("edge", "sys", &seamModel{}, WithForkCapture())
		a.state.Context = []*message.Msg{message.UserMsg("u", "question"), message.AssistantMsg("edge", "answer")}
		a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(ctx)})
		return a
	}

	open := setup()
	last := open.state.Context[1]
	last.Content = append(last.Content, message.ToolCallBlock{Type: "tool_call", ID: "open", Name: "echo", Input: `{}`})
	if _, err := open.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) || !strings.Contains(err.Error(), "open") {
		t.Fatalf("an open call must refuse the snapshot: %v", err)
	}
	last.Content = append(last.Content, message.ToolResultBlock{Type: "tool_result", ID: "open", Name: "echo", Output: "r"})
	if _, err := open.ForkSnapshot(); err != nil {
		t.Fatalf("a closed call is a safe boundary: %v", err)
	}

	compacted := setup()
	compacted.state.Summary = "a compaction ran"
	if _, err := compacted.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
		t.Fatalf("a changed summary must refuse the snapshot: %v", err)
	}

	moved := setup()
	moved.state.Context = []*message.Msg{message.UserMsg("u", "other"), message.AssistantMsg("edge", "other")}
	if _, err := moved.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
		t.Fatalf("a moved boundary must refuse the snapshot: %v", err)
	}

	reset := setup()
	if err := reset.ApplyThreadUpdate(ctx, ThreadUpdate{Reset: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := reset.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
		t.Fatalf("a reset drops the capture: %v", err)
	}
}

// wireBody decodes a captured request body into raw top-level fields.
func wireBody(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func wireList(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// forkWireProvider is a real OpenAI adapter with canned HTTP replies.
type forkWireProvider struct {
	name     string
	newModel func(url string) (model.ChatModel, error)
	reply    func(text string) string
	toolCall func(id string) string // a reply that calls echo
	messages string                 // field holding the conversation
	maxField string
}

func forkWireProviders() []forkWireProvider {
	return []forkWireProvider{
		{
			name: "chat completions",
			newModel: func(url string) (model.ChatModel, error) {
				return model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "test", Model: "test", BaseURL: url})
			},
			reply: func(text string) string {
				return fmt.Sprintf(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":60,"completion_tokens":5}}`, text)
			},
			toolCall: func(id string) string {
				return fmt.Sprintf(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":5}}`, id)
			},
			messages: "messages",
			maxField: "max_completion_tokens",
		},
		{
			name: "responses",
			newModel: func(url string) (model.ChatModel, error) {
				return model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{APIKey: "test", Model: "test", BaseURL: url})
			},
			reply: func(text string) string {
				return fmt.Sprintf(`{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":60,"output_tokens":5}}`, text)
			},
			toolCall: func(id string) string {
				return fmt.Sprintf(`{"id":"r","status":"completed","output":[{"type":"function_call","id":%q,"call_id":%q,"name":"echo","arguments":"{}"}],"usage":{"input_tokens":60,"output_tokens":5}}`, id, id)
			},
			messages: "input",
			maxField: "max_output_tokens",
		},
	}
}

// TestForkSummaryWireRequest checks the fork request at the HTTP boundary of
// the real OpenAI Chat Completions and Responses adapters: it must be the
// main thread's last request, byte for byte, plus the delta and the prompt,
// with identical tools, tool_choice "none" and the output cap.
func TestForkSummaryWireRequest(t *testing.T) {
	for _, provider := range forkWireProviders() {
		t.Run(provider.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies [][]byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, raw)
				n := len(bodies)
				mu.Unlock()
				text := "main answer"
				if n > 1 {
					text = "the summary"
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, provider.reply(text))
			}))
			defer srv.Close()
			cm, err := provider.newModel(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			a := NewUnifiedAgent("main", "You are the main thread.", cm,
				WithToolkit(tool.NewToolkit(forkEchoTool())), WithForkCapture(), WithModelStreaming(false))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := a.Reply(ctx, "hello"); err != nil {
				t.Fatal(err)
			}
			before := len(a.state.Context)
			snap, err := a.ForkSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			res, err := a.ForkSummary(ctx, snap, ForkRequest{Prompt: "Summarize the work.", Appendix: "Previous summary: none", MaxTokens: 300})
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != "the summary" || res.Calls != 1 || res.Usage == nil || res.Usage.InputTokens != 60 || res.Rejected {
				t.Fatalf("result = %+v", res)
			}
			if len(a.state.Context) != before {
				t.Fatal("the fork must not touch the main thread")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("requests = %d", len(bodies))
			}
			main, fork := wireBody(t, bodies[0]), wireBody(t, bodies[1])
			mainMsgs, forkMsgs := wireList(t, main[provider.messages]), wireList(t, fork[provider.messages])
			if len(forkMsgs) != len(mainMsgs)+2 {
				t.Fatalf("fork carries %d items, want %d (main) + delta + prompt", len(forkMsgs), len(mainMsgs))
			}
			for i := range mainMsgs {
				if compactJSON(t, mainMsgs[i]) != compactJSON(t, forkMsgs[i]) {
					t.Fatalf("item %d differs:\nmain %s\nfork %s", i, mainMsgs[i], forkMsgs[i])
				}
			}
			delta, prompt := string(forkMsgs[len(mainMsgs)]), string(forkMsgs[len(mainMsgs)+1])
			if !strings.Contains(delta, `"assistant"`) || !strings.Contains(delta, "main answer") {
				t.Fatalf("delta item = %s", delta)
			}
			if !strings.Contains(prompt, `"user"`) || !strings.Contains(prompt, `Summarize the work.\n\nPrevious summary: none`) {
				t.Fatalf("prompt item = %s", prompt)
			}
			if main["tools"] == nil || compactJSON(t, main["tools"]) != compactJSON(t, fork["tools"]) {
				t.Fatal("the fork must send the same tools")
			}
			if provider.messages == "input" && compactJSON(t, main["instructions"]) != compactJSON(t, fork["instructions"]) {
				t.Fatal("the fork must send the same instructions")
			}
			if main["tool_choice"] != nil || compactJSON(t, fork["tool_choice"]) != `"none"` {
				t.Fatalf("tool_choice main=%s fork=%s", main["tool_choice"], fork["tool_choice"])
			}
			if compactJSON(t, fork[provider.maxField]) != "300" || main[provider.maxField] != nil {
				t.Fatalf("%s main=%s fork=%s", provider.maxField, main[provider.maxField], fork[provider.maxField])
			}
		})
	}
}

// toolChoiceMiddleware sets the tool choice of every model call, standing in
// for whatever made the main thread send one.
type toolChoiceMiddleware struct {
	middleware.BaseMiddleware
	choice *model.ToolChoice
}

func (m *toolChoiceMiddleware) OnModelCall(ctx context.Context, in *middleware.ModelCallInput, next middleware.ModelCallHandler) (*model.ChatResponse, error) {
	in.ToolChoice = m.choice
	return next(ctx, in)
}

// TestForkSummaryKeepToolChoiceWire checks KeepToolChoice at the HTTP
// boundary: the fork sends the main request's own tool_choice (absent when
// the main request had none), and a tool-calling answer is rewritten in the
// same fork after an error result and the no-tools feedback.
func TestForkSummaryKeepToolChoiceWire(t *testing.T) {
	for _, provider := range forkWireProviders() {
		for _, captured := range []bool{true, false} {
			name := provider.name + "/main without tool_choice"
			if captured {
				name = provider.name + "/main with tool_choice"
			}
			t.Run(name, func(t *testing.T) {
				var mu sync.Mutex
				var bodies [][]byte
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					mu.Lock()
					bodies = append(bodies, raw)
					n := len(bodies)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					switch n {
					case 1:
						fmt.Fprint(w, provider.reply("main answer"))
					case 2:
						fmt.Fprint(w, provider.toolCall("call_fork"))
					default:
						fmt.Fprint(w, provider.reply("the summary"))
					}
				}))
				defer srv.Close()
				cm, err := provider.newModel(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				opts := []AgentOption{WithToolkit(tool.NewToolkit(forkEchoTool())), WithForkCapture(), WithModelStreaming(false)}
				if captured {
					opts = append(opts, WithMiddlewares(&toolChoiceMiddleware{choice: &model.ToolChoice{Mode: "auto"}}))
				}
				a := NewUnifiedAgent("main", "You are the main thread.", cm, opts...)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if _, err := a.Reply(ctx, "hello"); err != nil {
					t.Fatal(err)
				}
				snap, err := a.ForkSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				res, err := a.ForkSummary(ctx, snap, ForkRequest{Prompt: "Summarize the work.", MaxRewrites: 1, KeepToolChoice: true})
				if err != nil {
					t.Fatal(err)
				}
				if res.Text != "the summary" || res.Calls != 2 || res.Rewrites != 1 || res.Rejected {
					t.Fatalf("result = %+v", res)
				}

				mu.Lock()
				defer mu.Unlock()
				if len(bodies) != 3 {
					t.Fatalf("requests = %d", len(bodies))
				}
				main, fork1, fork2 := wireBody(t, bodies[0]), wireBody(t, bodies[1]), wireBody(t, bodies[2])
				for i, fork := range []map[string]json.RawMessage{fork1, fork2} {
					if captured {
						if main["tool_choice"] == nil || compactJSON(t, main["tool_choice"]) != compactJSON(t, fork["tool_choice"]) {
							t.Fatalf("fork %d tool_choice = %s, main sent %s", i+1, fork["tool_choice"], main["tool_choice"])
						}
					} else if main["tool_choice"] != nil || fork["tool_choice"] != nil {
						t.Fatalf("no tool_choice in the main request, none in the fork: main=%s fork %d=%s", main["tool_choice"], i+1, fork["tool_choice"])
					}
					if compactJSON(t, main["tools"]) != compactJSON(t, fork["tools"]) {
						t.Fatalf("fork %d must send the same tools", i+1)
					}
				}
				mainMsgs := wireList(t, main[provider.messages])
				first, second := wireList(t, fork1[provider.messages]), wireList(t, fork2[provider.messages])
				if len(first) != len(mainMsgs)+2 {
					t.Fatalf("first fork carries %d items, want %d (main) + delta + prompt", len(first), len(mainMsgs))
				}
				for i := range mainMsgs {
					if compactJSON(t, mainMsgs[i]) != compactJSON(t, first[i]) {
						t.Fatalf("item %d differs from the main request", i)
					}
				}
				if len(second) <= len(first)+1 {
					t.Fatalf("the rewrite must append the tool turn and the feedback: %d then %d items", len(first), len(second))
				}
				for i := range first {
					if compactJSON(t, first[i]) != compactJSON(t, second[i]) {
						t.Fatalf("item %d changed between the fork calls", i)
					}
				}
				tail := ""
				for _, item := range second[len(first) : len(second)-1] {
					tail += string(item)
				}
				if strings.Count(tail, "call_fork") < 2 || !strings.Contains(tail, "Not executed") {
					t.Fatalf("the tool call and its error result must precede the feedback: %s", tail)
				}
				last := string(second[len(second)-1])
				if !strings.Contains(last, `"user"`) || !strings.Contains(last, "Do not call tools. Answer with the summary text only.") {
					t.Fatalf("feedback item = %s", last)
				}
			})
		}
	}
}

func TestForkSummaryKeepToolChoiceRewrites(t *testing.T) {
	calls := func(n int) func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
			if call < n {
				return seamToolCalls(message.ToolCallBlock{ID: fmt.Sprintf("x%d", call), Name: "echo"}), nil
			}
			return seamText("summary"), nil
		}
	}

	a, m, snap := forkFixture(t, calls(1))
	snap.toolChoice = &model.ToolChoice{Mode: "auto"}
	res, err := a.ForkSummary(context.Background(), snap, ForkRequest{Prompt: "Summarize.", MaxRewrites: 1, KeepToolChoice: true})
	if err != nil || res.Text != "summary" || res.Rewrites != 1 || res.Calls != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	first, second := m.call(1), m.call(2)
	for _, c := range []seamCall{first, second} {
		if c.opts.ToolChoice == nil || c.opts.ToolChoice.Mode != "auto" {
			t.Fatalf("the captured tool choice must be sent: %+v", c.opts.ToolChoice)
		}
	}
	if len(second.msgs) != len(first.msgs)+2 {
		t.Fatalf("the rewrite continues the same fork: %d then %d messages", len(first.msgs), len(second.msgs))
	}
	turn, feedback := second.msgs[len(first.msgs)], second.msgs[len(first.msgs)+1]
	callBlocks, results := turn.GetContentBlocks(message.ContentBlockToolCall), turn.GetContentBlocks(message.ContentBlockToolResult)
	if turn.Role != message.RoleAssistant || len(callBlocks) != 1 || len(results) != 1 {
		t.Fatalf("tool turn = %+v", turn.Content)
	}
	if r := results[0].(message.ToolResultBlock); r.ID != "x0" || r.State != message.ToolResultError {
		t.Fatalf("every call needs an error result: %+v", r)
	}
	if feedback.Role != message.RoleUser || msgText(feedback) != "Do not call tools. Answer with the summary text only." {
		t.Fatalf("feedback = %q", msgText(feedback))
	}

	// The captured request had no tool choice: none is sent.
	a, m, snap = forkFixture(t, calls(0))
	if _, err := a.ForkSummary(context.Background(), snap, ForkRequest{Prompt: "Summarize.", KeepToolChoice: true}); err != nil {
		t.Fatal(err)
	}
	if tc := m.call(1).opts.ToolChoice; tc != nil {
		t.Fatalf("tool choice = %+v, want none", tc)
	}

	// Rewrites run out: the fork fails as without KeepToolChoice.
	a, _, snap = forkFixture(t, calls(5))
	res, err = a.ForkSummary(context.Background(), snap, ForkRequest{Prompt: "Summarize.", MaxRewrites: 2, KeepToolChoice: true})
	if err == nil || !strings.Contains(err.Error(), "called a tool") || res.Calls != 3 || res.Rewrites != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	// Tool-call rewrites and validation rewrites share MaxRewrites.
	a, _, snap = forkFixture(t, calls(1))
	res, err = a.ForkSummary(context.Background(), snap, ForkRequest{
		Prompt: "Summarize.", MaxRewrites: 1, KeepToolChoice: true,
		Validate: func(string) (string, bool) { return "", false },
	})
	if err != nil || !res.Rejected || res.Text != "summary" || res.Calls != 2 || res.Rewrites != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func forkFixture(t *testing.T, respond func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error)) (*UnifiedAgent, *seamModel, *ForkSnapshot) {
	t.Helper()
	m := &seamModel{}
	m.respond = func(call int, msgs []*message.Msg, o *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return seamText("main answer"), nil
		}
		return respond(call-1, msgs, o)
	}
	a := NewUnifiedAgent("main", "sys", m, WithForkCapture(), WithToolkit(tool.NewToolkit(forkEchoTool())),
		WithModelConfig(ModelConfig{RetryDelay: time.Millisecond}))
	if _, err := a.Reply(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	snap, err := a.ForkSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return a, m, snap
}

func TestForkSummaryRewriteLoop(t *testing.T) {
	drafts := []string{"draft one", "draft two"}
	a, m, snap := forkFixture(t, func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		return seamText(drafts[call]), nil
	})
	res, err := a.ForkSummary(context.Background(), snap, ForkRequest{
		Prompt:      "Summarize.",
		MaxTokens:   50,
		MaxRewrites: 2,
		Validate: func(text string) (string, bool) {
			if text == "draft one" {
				return "Too vague; name the files.", false
			}
			return "", true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "draft two" || res.Rewrites != 1 || res.Calls != 2 || res.Rejected {
		t.Fatalf("result = %+v", res)
	}
	if res.Usage.InputTokens != 14 || res.Usage.OutputTokens != 4 {
		t.Fatalf("usage must be summed over both calls: %+v", res.Usage)
	}
	first, second := m.call(1), m.call(2)
	if len(second.msgs) != len(first.msgs)+2 {
		t.Fatalf("the rewrite continues the same fork: %d then %d messages", len(first.msgs), len(second.msgs))
	}
	for i := range first.msgs {
		if !reflect.DeepEqual(first.msgs[i].Content, second.msgs[i].Content) {
			t.Fatalf("message %d changed between rewrites", i)
		}
	}
	if got := second.msgs[len(first.msgs)]; got.Role != message.RoleAssistant || msgText(got) != "draft one" {
		t.Fatal("the rejected candidate follows as the assistant's answer")
	}
	if got := second.msgs[len(first.msgs)+1]; got.Role != message.RoleUser || msgText(got) != "Too vague; name the files." {
		t.Fatal("the feedback follows as a user message")
	}
	for _, c := range []seamCall{first, second} {
		if c.stream || c.opts.ToolChoice == nil || c.opts.ToolChoice.Mode != "none" || c.opts.MaxTokens == nil || *c.opts.MaxTokens != 50 || len(c.opts.Tools) != 1 {
			t.Fatalf("fork call options = %+v (stream=%v)", c.opts, c.stream)
		}
	}
}

func TestForkSummaryRejectedAfterRewrites(t *testing.T) {
	a, _, snap := forkFixture(t, func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		return seamText(fmt.Sprintf("candidate %d", call)), nil
	})
	res, err := a.ForkSummary(context.Background(), snap, ForkRequest{
		Prompt:      "Summarize.",
		MaxRewrites: 1,
		Validate:    func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rejected || res.Text != "candidate 1" || res.Rewrites != 1 || res.Calls != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestForkSummaryFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		resp  func(call int) (*model.ChatResponse, error)
		guard func(context.Context, string) error
		want  string
		calls int
	}{
		{name: "tool call", resp: func(int) (*model.ChatResponse, error) {
			return seamToolCalls(message.ToolCallBlock{ID: "x", Name: "echo"}), nil
		}, want: "called a tool", calls: 1},
		{name: "partial response", resp: func(int) (*model.ChatResponse, error) {
			r := seamText("cut")
			r.Error = errors.New("response incomplete: max_output_tokens")
			return r, nil
		}, want: "incomplete", calls: 1},
		{name: "length stop", resp: func(int) (*model.ChatResponse, error) {
			r := seamText("cut")
			r.StopReason = model.StopReasonLength
			return r, nil
		}, want: "incomplete", calls: 1},
		{name: "empty text", resp: func(int) (*model.ChatResponse, error) { return seamText("  "), nil }, want: "no text", calls: 1},
		{name: "guard", resp: func(int) (*model.ChatResponse, error) { return seamText("secret"), nil },
			guard: func(_ context.Context, text string) error { return fmt.Errorf("blocked %q", text) }, want: "guard", calls: 1},
		{name: "permanent error", resp: func(int) (*model.ChatResponse, error) { return nil, errors.New("HTTP 400 bad request") }, want: "400", calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, snap := forkFixture(t, func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
				return tc.resp(call)
			})
			res, err := a.ForkSummary(context.Background(), snap, ForkRequest{Prompt: "Summarize.", Guard: tc.guard})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if res == nil || res.Calls != tc.calls || res.Text != "" {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestForkSummaryRetriesTransientErrors(t *testing.T) {
	a, _, snap := forkFixture(t, func(call int, _ []*message.Msg, _ *model.CallOptions) (*model.ChatResponse, error) {
		if call == 0 {
			return nil, errors.New("HTTP 503 overloaded")
		}
		return seamText("summary"), nil
	})
	res, err := a.ForkSummary(context.Background(), snap, ForkRequest{Prompt: "Summarize."})
	if err != nil || res.Text != "summary" || res.Calls != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForkSummaryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, _, snap := forkFixture(t, func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		cancel()
		return nil, context.Canceled
	})
	_, err := a.ForkSummary(ctx, snap, ForkRequest{Prompt: "Summarize."})
	if !errors.Is(err, context.Canceled) || err != ctx.Err() {
		t.Fatalf("cancellation must return ctx.Err(), got %v", err)
	}
}

func TestForkSummaryLeavesStateAndStatsAlone(t *testing.T) {
	a, _, snap := forkFixture(t, func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		return seamText("summary"), nil
	})
	before := append([]*message.Msg(nil), a.state.Context...)
	stats := *a.stats
	if _, err := a.ForkSummary(context.Background(), snap, ForkRequest{Prompt: "Summarize."}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, a.state.Context) || a.state.Summary != "" || *a.stats != stats {
		t.Fatal("ForkSummary must not change agent state or turn stats")
	}
}

// tailFixture builds an agent whose context holds eight 100-byte text
// messages (104 tokens each under charTokens) and a snapshot of it, then
// appends one more message after the snapshot.
func tailFixture(t *testing.T) (*UnifiedAgent, *ForkSnapshot, []*message.Msg, *message.Msg) {
	t.Helper()
	ctx := context.Background()
	a := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithForkCapture())
	var msgs []*message.Msg
	for i := 0; i < 8; i++ {
		msgs = append(msgs, message.UserMsg("u", fmt.Sprintf("m%d:%s", i, strings.Repeat("x", 97))))
	}
	a.state.Context = append([]*message.Msg(nil), msgs...)
	a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(ctx)})
	snap, err := a.ForkSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	after := message.UserMsg("u", "after")
	a.state.Context = append(a.state.Context, after)
	return a, snap, msgs, after
}

func TestApplySummaryKeepsTail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opt    SummaryApplyOptions
		window int // ContextConfig.ContextSize, 0 for an unknown window
		first  int // index of the first kept snapshot message
	}{
		{"token floor", SummaryApplyOptions{MinTailTokens: 250, MinTailTextMessages: 1, MaxTailTokens: 1000}, 0, 5},
		{"text floor", SummaryApplyOptions{MinTailTokens: 10, MinTailTextMessages: 4, MaxTailTokens: 1000}, 0, 4},
		{"token cap", SummaryApplyOptions{MinTailTokens: 10000, MinTailTextMessages: 50, MaxTailTokens: 350}, 0, 5},
		{"defaults", SummaryApplyOptions{}, 0, 0},
		// A 2000-token window caps the default tail at 400 tokens: three of
		// the 104-token messages, not the whole context.
		{"defaults capped by the window", SummaryApplyOptions{}, 2000, 5},
		// Explicit values are not capped.
		{"explicit values with a window", SummaryApplyOptions{MinTailTokens: 10, MinTailTextMessages: 4, MaxTailTokens: 1000}, 2000, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saver := newFakeStateSaver()
			a, snap, msgs, after := tailFixture(t)
			if tc.window > 0 {
				a.contextCfg = &ContextConfig{ContextSize: tc.window}
			}
			a.stateSaver = saver
			a.lastModelInputTokens = 777
			a.pendingPrecompress = &precompressResult{summary: "stale"}
			version := a.compactionVersion
			if err := a.ApplySummary(context.Background(), snap, "the summary", tc.opt); err != nil {
				t.Fatal(err)
			}
			want := append(append([]*message.Msg(nil), msgs[tc.first:]...), after)
			if !reflect.DeepEqual(a.state.Context, want) {
				t.Fatalf("kept %d messages, want %d", len(a.state.Context), len(want))
			}
			for i := range want {
				if a.state.Context[i] != want[i] {
					t.Fatal("kept messages must be the live ones")
				}
			}
			if a.state.Summary != "the summary" || a.lastModelInputTokens != 0 || a.pendingPrecompress != nil || a.compactionVersion == version {
				t.Fatal("summary, token floor, precompression or compaction version not updated")
			}
			if saved := saver.states[a.state.SessionID]; saved == nil || saved.Summary != "the summary" {
				t.Fatal("ApplySummary must persist the new state")
			}
			if _, err := a.ForkSnapshot(); !errors.Is(err, ErrNoForkSnapshot) {
				t.Fatal("the old capture is stale after the summary changed")
			}
		})
	}
}

// TestOfferSummaryAppliesAtTheTrigger: an offered summary changes nothing
// until the compression trigger, which installs it without a model call,
// keeping ApplySummary's tail and everything appended since. An offer the
// context outgrew falls through to the synchronous path.
func TestOfferSummaryAppliesAtTheTrigger(t *testing.T) {
	a, snap, msgs, after := tailFixture(t)
	opt := SummaryApplyOptions{MinTailTokens: 250, MinTailTextMessages: 1, MaxTailTokens: 1000}
	before := append([]*message.Msg(nil), a.state.Context...)
	if err := a.OfferSummary(context.Background(), snap, "the notes", opt); err != nil {
		t.Fatal(err)
	}
	if a.state.Summary != "" || !reflect.DeepEqual(before, a.state.Context) {
		t.Fatal("an offer must not change the context before the trigger")
	}
	later := message.UserMsg("u", "later")
	a.state.Context = append(a.state.Context, later)
	if !a.applyReadyPrecompress(context.Background()) {
		t.Fatal("the trigger did not apply the offer")
	}
	want := append(append([]*message.Msg(nil), msgs[5:]...), after, later)
	if a.state.Summary != "the notes" || !reflect.DeepEqual(a.state.Context, want) {
		t.Fatalf("summary %q, kept %d messages, want %d", a.state.Summary, len(a.state.Context), len(want))
	}

	b, snapB, _, _ := tailFixture(t)
	if err := b.OfferSummary(context.Background(), snapB, "the notes", opt); err != nil {
		t.Fatal(err)
	}
	b.state.Summary = "compacted meanwhile"
	if b.applyReadyPrecompress(context.Background()) || b.state.Summary != "compacted meanwhile" {
		t.Fatal("a stale offer must fall through")
	}
	if err := b.OfferSummary(context.Background(), snapB, "x", opt); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("offer on a stale snapshot: %v", err)
	}
	if err := b.OfferSummary(context.Background(), nil, "x", opt); err == nil {
		t.Fatal("nil snapshot")
	}
}

// TestOfferSummarySurvivesAReload: the offer is part of the checkpointed
// state, so an agent rebuilt from it (a team master is rebuilt for every
// activation) still applies it at the trigger, matching by message ID.
func TestOfferSummarySurvivesAReload(t *testing.T) {
	saver := newFakeStateSaver()
	a, snap, _, _ := tailFixture(t)
	a.stateSaver = saver
	opt := SummaryApplyOptions{MinTailTokens: 250, MinTailTextMessages: 1, MaxTailTokens: 1000}
	if err := a.OfferSummary(context.Background(), snap, "the notes", opt); err != nil {
		t.Fatal(err)
	}
	saved := saver.states[a.state.SessionID]
	if saved == nil || saved.SummaryOffer == nil || saved.SummaryOffer.Text != "the notes" {
		t.Fatal("an offer made between replies must be persisted")
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var st AgentState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	b := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithState(&st))
	if !b.applyReadyPrecompress(context.Background()) {
		t.Fatal("the reloaded offer was not applied")
	}
	if b.state.Summary != "the notes" || len(b.state.Context) != 4 || b.state.SummaryOffer != nil {
		t.Fatalf("summary %q, %d messages kept, offer %+v", b.state.Summary, len(b.state.Context), b.state.SummaryOffer)
	}
}

func TestApplySummaryStale(t *testing.T) {
	a, snap, _, _ := tailFixture(t)
	a.state.Summary = "compacted meanwhile"
	before := append([]*message.Msg(nil), a.state.Context...)
	if err := a.ApplySummary(context.Background(), snap, "x", SummaryApplyOptions{}); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("err = %v", err)
	}
	if a.state.Summary != "compacted meanwhile" || !reflect.DeepEqual(before, a.state.Context) {
		t.Fatal("a stale snapshot must change nothing")
	}

	b, snapB, _, _ := tailFixture(t)
	b.state.Context = b.state.Context[2:] // the boundary moved
	if err := b.ApplySummary(context.Background(), snapB, "x", SummaryApplyOptions{}); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("err = %v", err)
	}
	if err := b.ApplySummary(context.Background(), nil, "x", SummaryApplyOptions{}); err == nil {
		t.Fatal("nil snapshot")
	}
}

// TestSummaryRefusesAnEmptySnapshot: a snapshot taken while the context was
// empty covers no message to replace. ApplySummary refuses it as OfferSummary
// does, instead of installing the summary over messages it never saw.
func TestSummaryRefusesAnEmptySnapshot(t *testing.T) {
	ctx := context.Background()
	a := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithForkCapture())
	a.state.Summary = "earlier summary"
	a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(ctx)})
	snap, err := a.ForkSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.contextLen != 0 {
		t.Fatalf("snapshot covers %d messages, want none", snap.contextLen)
	}
	a.state.Context = append(a.state.Context, message.UserMsg("u", "after"))
	before := append([]*message.Msg(nil), a.state.Context...)
	version := a.compactionVersion
	refused := func(err error) bool {
		return err != nil && !errors.Is(err, ErrStaleSnapshot) && strings.Contains(err.Error(), "no context to replace")
	}
	if err := a.ApplySummary(ctx, snap, "x", SummaryApplyOptions{}); !refused(err) {
		t.Fatalf("ApplySummary err = %v, want a refusal", err)
	}
	if err := a.OfferSummary(ctx, snap, "x", SummaryApplyOptions{}); !refused(err) {
		t.Fatalf("OfferSummary err = %v, want a refusal", err)
	}
	if a.state.Summary != "earlier summary" || !reflect.DeepEqual(before, a.state.Context) ||
		a.state.SummaryOffer != nil || a.compactionVersion != version {
		t.Fatal("an empty snapshot must change nothing")
	}
}

// TestApplySummaryRechecksAfterCounting: the tail is counted outside the
// lock, so a change made meanwhile (another between-replies call) must make
// the apply stale instead of being overwritten or folded in.
func TestApplySummaryRechecksAfterCounting(t *testing.T) {
	for name, change := range map[string]func(*UnifiedAgent) error{
		"message appended": func(a *UnifiedAgent) error {
			return a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Messages: []*message.Msg{importedMsg(9, "late entry")}})
		},
		"summary changed": func(a *UnifiedAgent) error {
			a.mu.Lock()
			a.state.Summary = "compacted meanwhile"
			a.mu.Unlock()
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, snap, _, _ := tailFixture(t)
			var once sync.Once
			a.model.(*seamModel).tokens = func(msgs []*message.Msg) int {
				once.Do(func() {
					if err := change(a); err != nil {
						t.Error(err)
					}
				})
				return charTokens(msgs)
			}
			a.mu.Lock()
			wantSummary := a.state.Summary
			a.mu.Unlock()
			err := a.ApplySummary(context.Background(), snap, "the summary", SummaryApplyOptions{MinTailTokens: 250, MinTailTextMessages: 1, MaxTailTokens: 1000})
			if !errors.Is(err, ErrStaleSnapshot) {
				t.Fatalf("err = %v, want ErrStaleSnapshot", err)
			}
			if name == "summary changed" {
				wantSummary = "compacted meanwhile"
			}
			if a.state.Summary != wantSummary {
				t.Fatalf("summary = %q, want %q", a.state.Summary, wantSummary)
			}
			if name == "message appended" && (len(a.state.Context) != 10 || msgText(a.state.Context[9]) != "late entry") {
				t.Fatalf("the concurrent update must survive whole, context has %d messages", len(a.state.Context))
			}
		})
	}
}

func TestApplySummaryRepairsToolPairs(t *testing.T) {
	ctx := context.Background()
	call := func(id string, state message.ToolCallState) *message.Msg {
		return message.AssistantMsg("edge", []message.ContentBlock{message.ToolCallBlock{Type: "tool_call", ID: id, Name: "echo", Input: `{}`, State: state}})
	}
	result := func(id string) *message.Msg {
		return message.AssistantMsg("edge", []message.ContentBlock{message.ToolResultBlock{Type: "tool_result", ID: id, Name: "echo", Output: strings.Repeat("r", 200), State: message.ToolResultSuccess}})
	}
	opt := SummaryApplyOptions{MinTailTokens: 100, MinTailTextMessages: 1, MaxTailTokens: 10000}
	apply := func(msgs []*message.Msg) []*message.Msg {
		t.Helper()
		a := NewUnifiedAgent("edge", "sys", &seamModel{tokens: charTokens}, WithForkCapture())
		a.state.Context = msgs
		a.captureForkRequest(&middleware.ModelCallInput{Messages: a.prepareModelInput(ctx)})
		snap, err := a.ForkSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := a.ApplySummary(ctx, snap, "summary", opt); err != nil {
			t.Fatal(err)
		}
		return a.state.Context
	}

	// Growth stops on the result message; its call would be cut away, so the
	// tail must not start there.
	start, callA, resultA, tail := message.UserMsg("u", "start"), call("A", message.ToolCallFinished), result("A"), message.AssistantMsg("edge", "after tool")
	kept := apply([]*message.Msg{start, callA, resultA, tail})
	for _, m := range kept {
		if m == resultA && (len(kept) == 0 || kept[0] == resultA) {
			t.Fatal("the tail starts on a tool result whose call was cut away")
		}
	}
	if !reflect.DeepEqual(kept, []*message.Msg{tail}) && !reflect.DeepEqual(kept, []*message.Msg{callA, resultA, tail}) {
		t.Fatalf("unexpected tail of %d messages", len(kept))
	}

	// A call still awaiting its result is never cut away.
	parked := call("B", message.ToolCallAsking)
	kept = apply([]*message.Msg{start, parked, message.UserMsg("u", strings.Repeat("y", 200)), tail})
	if len(kept) != 3 || kept[0] != parked {
		t.Fatalf("the parked call must stay in the tail, kept %d messages", len(kept))
	}
}
