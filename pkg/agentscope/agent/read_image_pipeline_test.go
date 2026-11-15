package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// scriptedToolModel returns one prepared response per call, then a plain text
// reply forever after.
type scriptedToolModel struct {
	mu     sync.Mutex
	calls  int
	script []*model.ChatResponse
}

func (m *scriptedToolModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	m.mu.Unlock()
	if idx < len(m.script) {
		return m.script[idx], nil
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}},
		IsLast:  true,
	}, nil
}

func (m *scriptedToolModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (m *scriptedToolModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func toolCallResp(id, name, input string) *model.ChatResponse {
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: id, Name: name, Input: input,
			State: message.ToolCallPending,
		}},
		IsLast: true,
	}
}

func findToolResult(a *UnifiedAgent, callID string) (message.ToolResultBlock, bool) {
	a.mu.Lock()
	msgs := make([]*message.Msg, len(a.state.Context))
	copy(msgs, a.state.Context)
	a.mu.Unlock()
	for _, m := range msgs {
		for _, b := range m.Content {
			if tr, ok := b.(message.ToolResultBlock); ok && tr.ID == callID {
				return tr, true
			}
		}
	}
	return message.ToolResultBlock{}, false
}

func writePNG(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4}
	if err := os.WriteFile(p, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Upstream #2114 is only useful if the image actually reaches the model. The
// agent's tool pipeline is string-based, and a DataBlock-only tool response was
// flattened to an EMPTY string: the picture never reached any provider and the
// model was told the file was empty. Read now also emits a text placeholder and
// the agent keeps the non-text blocks on ToolResultBlock.Output.
func TestReadImageSurvivesAgentPipeline(t *testing.T) {
	dir := t.TempDir()
	png := writePNG(t, dir, "pixel.png")

	tk := tool.NewToolkit(tool.ReadTool())

	mock := &scriptedToolModel{script: []*model.ChatResponse{
		toolCallResp("tc_img", "Read", fmt.Sprintf(`{"file_path":%q}`, png)),
	}}
	a := NewUnifiedAgent("img", "sys", mock, WithToolkit(tk))

	ch, err := a.ReplyStream(context.Background(), "look at the image")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	tr, ok := findToolResult(a, "tc_img")
	if !ok {
		t.Fatal("tool result for tc_img not stored in context")
	}
	if tr.State != message.ToolResultSuccess {
		t.Errorf("state = %v, want success", tr.State)
	}

	// The regression: an empty tool result told the model the file was empty.
	if tr.GetOutputText() == "" {
		t.Error("tool result text is empty; the model would conclude the file is empty")
	}

	list, ok := tr.Output.([]message.ContentBlock)
	if !ok {
		t.Fatalf("Output = %T (%v), want a []message.ContentBlock carrying the image", tr.Output, tr.Output)
	}
	var db *message.DataBlock
	for i, b := range list {
		if d, isData := b.(message.DataBlock); isData {
			db = &d
			_ = i
		}
	}
	if db == nil {
		t.Fatalf("no DataBlock survived into the context: %#v", list)
	}
	if db.GetMediaType() != "image/png" {
		t.Errorf("media type = %q, want image/png", db.GetMediaType())
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok {
		t.Fatalf("source = %T, want Base64Source", db.Source)
	}
	decoded, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	onDisk, err := os.ReadFile(png)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, onDisk) {
		t.Error("image bytes in context differ from the file")
	}
}

// Concurrent tool batches take a different code path than the sequential one;
// the image must survive there too, alongside a plain text result.
func TestReadImageSurvivesConcurrentBatch(t *testing.T) {
	dir := t.TempDir()
	png := writePNG(t, dir, "shot.png")
	txt := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txt, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tk := tool.NewToolkit(tool.ReadTool())

	mock := &scriptedToolModel{script: []*model.ChatResponse{{
		Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_a", Name: "Read",
				Input: fmt.Sprintf(`{"file_path":%q}`, png), State: message.ToolCallPending},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_b", Name: "Read",
				Input: fmt.Sprintf(`{"file_path":%q}`, txt), State: message.ToolCallPending},
		},
		IsLast: true,
	}}}
	a := NewUnifiedAgent("img2", "sys", mock, WithToolkit(tk))

	ch, err := a.ReplyStream(context.Background(), "read both")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	imgRes, ok := findToolResult(a, "tc_a")
	if !ok {
		t.Fatal("image tool result missing")
	}
	if _, isList := imgRes.Output.([]message.ContentBlock); !isList {
		t.Errorf("image Output = %T, want a block list", imgRes.Output)
	}

	txtRes, ok := findToolResult(a, "tc_b")
	if !ok {
		t.Fatal("text tool result missing")
	}
	// A text-only result must stay a plain string: every existing consumer
	// (formatters, logs, persistence) expects that shape.
	s, isStr := txtRes.Output.(string)
	if !isStr {
		t.Errorf("text Output = %T, want string", txtRes.Output)
	}
	if s != "1\thello" {
		t.Errorf("text Output = %q", s)
	}
}

// tool_choice "none" is a request, not a guarantee. When a provider ignores it
// and returns tool calls anyway, those calls will never be executed — storing
// them would make the NEXT reply pick them up as pending work and run ghost
// tool calls.
type disobedientFinalModel struct {
	mu    sync.Mutex
	calls int
}

func (m *disobedientFinalModel) Chat(_ context.Context, _ []*message.Msg, opts ...model.CallOption) (*model.ChatResponse, error) {
	co := &model.CallOptions{}
	for _, o := range opts {
		o(co)
	}
	m.mu.Lock()
	m.calls++
	n := m.calls
	mode := ""
	if co.ToolChoice != nil {
		mode = co.ToolChoice.Mode
	}
	m.mu.Unlock()

	if mode == "none" {
		// Ignores tool_choice and returns BOTH a summary and a tool call.
		return &model.ChatResponse{
			Content: []message.ContentBlock{
				message.TextBlock{Type: "text", Text: "summary text"},
				message.ToolCallBlock{Type: "tool_call", ID: fmt.Sprintf("ghost%d", n),
					Name: "ghost_tool", Input: "{}", State: message.ToolCallPending},
			},
			IsLast: true,
		}, nil
	}
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: fmt.Sprintf("tc%d", n), Name: "ghost_tool", Input: "{}",
			State: message.ToolCallPending,
		}},
		IsLast: true,
	}, nil
}

func (m *disobedientFinalModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (m *disobedientFinalModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 10 }

func TestForcedFinalSummaryDropsUnexecutedToolCalls(t *testing.T) {
	mock := &disobedientFinalModel{}
	a := NewUnifiedAgent("ghost", "sys", mock, WithReactConfig(ReactConfig{MaxIters: 1}))

	ch, err := a.ReplyStream(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	a.mu.Lock()
	msgs := make([]*message.Msg, len(a.state.Context))
	copy(msgs, a.state.Context)
	a.mu.Unlock()

	// Every tool call stored in the context must have a matching result: an
	// unmatched one is exactly what the next Reply would pick up and execute.
	results := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if tr, ok := b.(message.ToolResultBlock); ok {
				results[tr.ID] = true
			}
		}
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			tc, ok := b.(message.ToolCallBlock)
			if !ok {
				continue
			}
			if !results[tc.ID] {
				t.Errorf("context holds tool call %q (%s) with no result; "+
					"the next reply would run it as a ghost call", tc.ID, tc.Name)
			}
		}
	}

	// The summary text itself must still be stored.
	last := msgs[len(msgs)-1]
	if last.GetTextContent("\n") == nil {
		t.Error("the finalization summary text was dropped along with the tool call")
	}
	// And nothing may be left executable.
	if calls := a.getExecutableToolCalls(); len(calls) != 0 {
		t.Errorf("%d executable tool call(s) left pending after finalization", len(calls))
	}
}

func blockTypes(blocks []message.ContentBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, string(b.GetType()))
	}
	return out
}

// Consumers that rebuild a message purely from the event stream (channel
// gateways, the console renderer, replay tapes) must see the image too, not
// just the text placeholder. That is what tool_result_data_delta is for; the
// agent never emitted it, so event-only consumers silently lost the picture.
func TestReadImageReachesEventStreamConsumers(t *testing.T) {
	dir := t.TempDir()
	png := writePNG(t, dir, "event.png")
	onDisk, err := os.ReadFile(png)
	if err != nil {
		t.Fatal(err)
	}

	tk := tool.NewToolkit(tool.ReadTool())
	mock := &scriptedToolModel{script: []*model.ChatResponse{
		toolCallResp("tc_ev", "Read", fmt.Sprintf(`{"file_path":%q}`, png)),
	}}
	a := NewUnifiedAgent("img3", "sys", mock, WithToolkit(tk))

	ch, err := a.ReplyStream(context.Background(), "look at the image")
	if err != nil {
		t.Fatal(err)
	}

	// Msg.AppendEvent ignores events whose ReplyID does not match the
	// message's own ID, so the rebuilt message has to adopt the reply ID
	// from the stream — exactly what a consumer does.
	var rebuilt *message.Msg
	sawDataDelta := false
	for evt := range ch {
		if e, ok := evt.(event.ToolResultDataDeltaEvent); ok {
			sawDataDelta = true
			if err := e.Validate(); err != nil {
				t.Errorf("emitted an invalid data delta event: %v", err)
			}
			if e.MediaType != "image/png" {
				t.Errorf("media type = %q, want image/png", e.MediaType)
			}
		}
		if rebuilt == nil {
			if ri, ok := evt.(interface{ GetReplyID() string }); ok && ri.GetReplyID() != "" {
				rebuilt = message.NewMsg("img3", message.RoleAssistant, "")
				rebuilt.ID = ri.GetReplyID()
			}
		}
		if rebuilt != nil {
			rebuilt.AppendEvent(evt)
		}
	}

	if !sawDataDelta {
		t.Fatal("no tool_result_data_delta event was emitted for the image")
	}
	if rebuilt == nil {
		t.Fatal("no event carried a reply id")
	}

	var found *message.DataBlock
	var text string
	for _, b := range rebuilt.Content {
		tr, ok := b.(message.ToolResultBlock)
		if !ok || tr.ID != "tc_ev" {
			continue
		}
		text = tr.GetOutputText()
		list, isList := tr.Output.([]message.ContentBlock)
		if !isList {
			t.Fatalf("rebuilt Output = %T, want a block list", tr.Output)
		}
		for _, sub := range list {
			if db, isData := sub.(message.DataBlock); isData {
				d := db
				found = &d
			}
		}
	}
	if found == nil {
		t.Fatalf("the rebuilt message lost the image; blocks = %v", blockTypes(rebuilt.Content))
	}
	src, ok := found.Source.(message.Base64Source)
	if !ok {
		t.Fatalf("source = %T, want Base64Source", found.Source)
	}
	decoded, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, onDisk) {
		t.Error("image bytes rebuilt from events differ from the file")
	}
	if text == "" {
		t.Error("the rebuilt tool result has no text; consumers would show nothing")
	}
}

// The model reads the image from ToolResultBlock.Output; the event exists for
// stream consumers, which cannot render a few hundred KB of base64 but do store
// it (run logs serialize every event verbatim, and the flight recorder's tail is
// capped by count, not bytes). Above the cap the event is dropped and the block
// stays on the output.
func TestReadImageAboveEventCapStaysOutOfEventStream(t *testing.T) {
	dir := t.TempDir()
	// Raw bytes chosen so the base64 form clears maxEventInlineDataBytes while
	// staying under tool.MaxInlineImageBytes (otherwise Read would not inline
	// it at all and the test would prove nothing).
	raw := make([]byte, maxEventInlineDataBytes) // base64 => ~4/3 of this
	for i := range raw {
		raw[i] = byte(i%251) + 1 // non-zero, so base64 has no padding shortcuts
	}
	if b64len := (len(raw) + 2) / 3 * 4; b64len <= maxEventInlineDataBytes {
		t.Fatalf("test payload too small: base64 would be %d bytes", b64len)
	}
	if len(raw) > tool.MaxInlineImageBytes {
		t.Fatalf("test payload too large: Read would not inline %d bytes", len(raw))
	}
	png := filepath.Join(dir, "big.png")
	if err := os.WriteFile(png, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	tk := tool.NewToolkit(tool.ReadTool())
	mock := &scriptedToolModel{script: []*model.ChatResponse{
		toolCallResp("tc_big", "Read", fmt.Sprintf(`{"file_path":%q}`, png)),
	}}
	a := NewUnifiedAgent("img4", "sys", mock, WithToolkit(tk))

	ch, err := a.ReplyStream(context.Background(), "look at the image")
	if err != nil {
		t.Fatal(err)
	}
	dataDeltas := 0
	for evt := range ch {
		if _, ok := evt.(event.ToolResultDataDeltaEvent); ok {
			dataDeltas++
		}
	}
	if dataDeltas != 0 {
		t.Errorf("%d oversized data delta event(s) reached the stream, want 0", dataDeltas)
	}

	// The model must still get the image.
	tr, ok := findToolResult(a, "tc_big")
	if !ok {
		t.Fatal("tool result missing")
	}
	list, isList := tr.Output.([]message.ContentBlock)
	if !isList {
		t.Fatalf("Output = %T, want a block list", tr.Output)
	}
	found := false
	for _, b := range list {
		if db, isData := b.(message.DataBlock); isData {
			src, isB64 := db.Source.(message.Base64Source)
			if !isB64 {
				t.Fatalf("source = %T, want Base64Source", db.Source)
			}
			decoded, err := base64.StdEncoding.DecodeString(src.Data)
			if err != nil {
				t.Fatalf("base64 decode: %v", err)
			}
			if !bytes.Equal(decoded, raw) {
				t.Error("image bytes on the tool result differ from the file")
			}
			found = true
		}
	}
	if !found {
		t.Error("the oversized image was dropped from the tool result too")
	}
	if tr.GetOutputText() == "" {
		t.Error("the text placeholder must survive regardless of the event cap")
	}
}
