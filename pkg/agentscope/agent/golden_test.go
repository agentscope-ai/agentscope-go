package agent

// Golden replay seeds (HARNESS_DESIGN B2): scripted scenarios run against
// mock models, with the recorded model-call tape compared against a golden
// file. Any change to prompt construction, tool handling, HITL flow, or
// compression that alters what the model sees fails here. Regenerate with
// -golden-update after deliberate behavior changes.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

var goldenUpdate = flag.Bool("golden-update", false, "regenerate golden replay tapes")

// normalizeTape strips run-specific fields (timestamps, durations, random
// message IDs, random reply IDs) so tapes compare deterministically.
func normalizeTape(t *replay.Tape) []byte {
	type normEntry struct {
		Index     int             `json:"index"`
		AgentName string          `json:"agent_name"`
		ModelName string          `json:"model_name"`
		ReplyID   string          `json:"reply_id,omitempty"`
		Messages  json.RawMessage `json:"messages"`
		Tools     json.RawMessage `json:"tools,omitempty"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error,omitempty"`
	}
	out := make([]normEntry, 0, len(t.Entries))
	for i := range t.Entries {
		e := &t.Entries[i]
		out = append(out, normEntry{
			Index:     e.Index,
			AgentName: e.AgentName,
			ModelName: e.ModelName,
			ReplyID:   "golden-reply",
			Messages:  normalizeJSONScrub(e.Messages),
			Tools:     e.Tools,
			Response:  normalizeJSONScrub(e.Response),
			Error:     e.Error,
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return b
}

// normalizeJSONScrub replaces random message IDs with sequential ones and
// strips wall-clock fields from arbitrary JSON (messages or responses).
func normalizeJSONScrub(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	counter := 0
	scrubGolden(v, &counter)
	b, _ := json.Marshal(v)
	return b
}

func scrubGolden(v any, counter *int) {
	switch t := v.(type) {
	case map[string]any:
		// Message/block objects carry random UUID IDs and wall-clock
		// timestamps. UUID-shaped ids are replaced with sequential ones;
		// deterministic ids (e.g. scripted tool-call ids) are kept.
		if id, ok := t["id"].(string); ok && looksLikeUUID(id) {
			*counter++
			t["id"] = fmt.Sprintf("id-%d", *counter)
		}
		for _, k := range []string{"timestamp", "finished_at", "created_at"} {
			delete(t, k)
		}
		for _, child := range t {
			scrubGolden(child, counter)
		}
	case []any:
		for _, child := range t {
			scrubGolden(child, counter)
		}
	}
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".json")
	if *goldenUpdate {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden tape regenerated: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden tape missing (run with -golden-update): %v", err)
	}
	var wantV, gotV any
	if err := json.Unmarshal(want, &wantV); err != nil {
		t.Fatalf("golden tape invalid JSON: %v", err)
	}
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("recorded tape invalid JSON: %v", err)
	}
	if !reflect.DeepEqual(wantV, gotV) {
		t.Errorf("recorded tape diverges from golden %s\nwant: %.800s\n got: %.800s", name, want, got)
	}
}

func echoToolFixture(name string) tool.Tool {
	return tool.NewFunctionTool(name, "echoes input", json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		func(_ context.Context, input map[string]any) (any, error) {
			return "echo:" + name, nil
		})
}

// TestGolden_MultiToolBatch locks the multi-tool-call batch flow.
func TestGolden_MultiToolBatch(t *testing.T) {
	rec := replay.NewRecorder()
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_a", Name: "echo_a", Input: `{"x":"1"}`, State: message.ToolCallPending},
			message.ToolCallBlock{Type: "tool_call", ID: "tc_b", Name: "echo_b", Input: `{"x":"2"}`, State: message.ToolCallPending},
		}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "batch done"}}, IsLast: true},
	}}
	a := NewUnifiedAgent("golden-batch", "You are helpful.", mock,
		WithToolkit(tool.NewToolkit(echoToolFixture("echo_a"), echoToolFixture("echo_b"))),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithMiddlewares(rec),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)

	ch, err := a.ReplyStream(context.Background(), "run both tools")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	compareGolden(t, "multi_tool_batch", normalizeTape(rec.Tape()))
}

// TestGolden_HITLParkResume locks the ASK → park → confirm → resume flow.
func TestGolden_HITLParkResume(t *testing.T) {
	rec := replay.NewRecorder()
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_h", Name: "echo_a", Input: `{"x":"h"}`, State: message.ToolCallPending},
		}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "hitl done"}}, IsLast: true},
	}}
	a := NewUnifiedAgent("golden-hitl", "You are helpful.", mock,
		WithToolkit(tool.NewToolkit(echoToolFixture("echo_a"))),
		WithPermissionContext(permission.NewContext(permission.ModeDefault)),
		WithMiddlewares(rec),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)

	ch, err := a.ReplyStream(context.Background(), "use the tool")
	if err != nil {
		t.Fatal(err)
	}
	confirmed := false
	for evt := range ch {
		if ce, ok := evt.(event.RequireUserConfirmEvent); ok && !confirmed {
			confirmed = true
			a.SubmitUserConfirm(&event.UserConfirmResultEvent{
				ConfirmResults: []event.ConfirmResult{{
					Confirmed: true,
					ToolCall:  ce.ToolCalls[0],
				}},
			})
		}
	}
	if !confirmed {
		t.Fatal("no RequireUserConfirmEvent observed")
	}
	compareGolden(t, "hitl_park_resume", normalizeTape(rec.Tape()))
}

// TestGolden_ExternalTool locks the submit → external result flow.
func TestGolden_ExternalTool(t *testing.T) {
	rec := replay.NewRecorder()
	mock := &mockChatModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{
			message.ToolCallBlock{Type: "tool_call", ID: "tc_e", Name: "ext_tool", Input: `{}`, State: message.ToolCallPending},
		}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "ext done"}}, IsLast: true},
	}}
	a := NewUnifiedAgent("golden-ext", "You are helpful.", mock,
		WithToolkit(tool.NewToolkit(newHITLMockTool("ext_tool", true))),
		WithPermissionContext(permission.NewContext(permission.ModeBypass)),
		WithMiddlewares(rec),
		WithReactConfig(ReactConfig{MaxIters: 4}),
	)

	ch, err := a.ReplyStream(context.Background(), "call external")
	if err != nil {
		t.Fatal(err)
	}
	submitted := false
	for evt := range ch {
		if _, ok := evt.(event.RequireExternalExecutionEvent); ok && !submitted {
			submitted = true
			a.SubmitExternalResult(&event.ExternalExecutionResultEvent{
				ExecutionResults: []message.ToolResultBlock{{
					Type:   "tool_result",
					ID:     "tc_e",
					Name:   "ext_tool",
					Output: []message.ContentBlock{message.TextBlock{Type: "text", Text: "external output"}},
					State:  message.ToolResultSuccess,
				}},
			})
		}
	}
	if !submitted {
		t.Fatal("no RequireExternalExecutionEvent observed")
	}
	compareGolden(t, "external_tool", normalizeTape(rec.Tape()))
}

// TestGolden_CompressionSummary locks that an over-threshold context issues
// a summary model call before the next reasoning step.
func TestGolden_CompressionSummary(t *testing.T) {
	rec := replay.NewRecorder()
	summaryResp := model.ChatResponse{Content: []message.ContentBlock{
		message.ToolCallBlock{Type: "tool_call", ID: "tc_sum", Name: "generate_structured_output",
			Input: `{"task_overview":"t","current_state":"s","important_discoveries":"d","next_steps":"n","context_to_preserve":"c"}`,
			State: message.ToolCallPending},
	}, IsLast: true}
	finalResp := model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "after compress"}}, IsLast: true}
	mock := &bigTokenMockModel{responses: []model.ChatResponse{summaryResp, finalResp}}

	a := NewUnifiedAgent("golden-compress", "You are helpful.", mock,
		WithMiddlewares(rec),
		WithReactConfig(ReactConfig{MaxIters: 4}),
		WithContextConfig(&ContextConfig{TriggerRatio: 0.8, ReserveRatio: 0.1, ContextSize: 1000}),
	)

	ch, err := a.ReplyStream(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	compareGolden(t, "compression_summary", normalizeTape(rec.Tape()))
}

// bigTokenMockModel reports a token count above any sensible threshold so
// compression triggers deterministically.
type bigTokenMockModel struct {
	responses []model.ChatResponse
	callCount int
}

func (m *bigTokenMockModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	if m.callCount >= len(m.responses) {
		return &model.ChatResponse{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "done"}}, IsLast: true}, nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return &resp, nil
}

func (m *bigTokenMockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}

func (m *bigTokenMockModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 900 }

func (m *bigTokenMockModel) ContextSize() int { return 1000 }

var _ = time.Second
