package console

import (
	"bytes"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func newTestRenderer(t *testing.T, v Verbosity) (*Renderer, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	r := NewRenderer(WithVerbosity(v), WithWriter(&buf), WithColor(false))
	return r, &buf
}

func TestRenderer_StreamsTextDeltas(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewTextBlockStartEvent("reply-1", "blk-1"))
	r.Render(event.NewTextBlockDeltaEvent("reply-1", "blk-1", "Hello, "))
	r.Render(event.NewTextBlockDeltaEvent("reply-1", "blk-1", "world"))
	r.Render(event.NewTextBlockEndEvent("reply-1", "blk-1"))
	r.Render(event.NewReplyEndEvent("sess", "reply-1"))

	out := buf.String()
	if !strings.Contains(out, "Hello, world") {
		t.Errorf("streamed text missing:\n%q", out)
	}
	if !strings.Contains(out, "bot") {
		t.Errorf("reply start should show the agent name:\n%q", out)
	}
}

func TestRenderer_QuietOnlyShowsText(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityQuiet)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewThinkingBlockStartEvent("reply-1", "th-1"))
	r.Render(event.NewThinkingBlockDeltaEvent("reply-1", "th-1", "secret thoughts"))
	r.Render(event.NewThinkingBlockEndEvent("reply-1", "th-1"))
	r.Render(event.NewTextBlockStartEvent("reply-1", "blk-1"))
	r.Render(event.NewTextBlockDeltaEvent("reply-1", "blk-1", "visible"))
	r.Render(event.NewTextBlockEndEvent("reply-1", "blk-1"))
	r.Render(event.NewModelCallEndEvent("reply-1", 10, 20))

	out := buf.String()
	if !strings.Contains(out, "visible") {
		t.Errorf("quiet mode must still show reply text:\n%q", out)
	}
	if strings.Contains(out, "secret thoughts") || strings.Contains(out, "tokens") || strings.Contains(out, "Thinking") {
		t.Errorf("quiet mode leaked non-text output:\n%q", out)
	}
}

func TestRenderer_ToolCallAndResult(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewToolCallStartEvent("reply-1", "tc-1", "get_weather"))
	r.Render(event.NewToolCallEndEvent("reply-1", "tc-1"))
	r.Render(event.NewToolResultStartEvent("reply-1", "tc-1", "get_weather"))
	r.Render(event.NewToolResultTextDeltaEvent("reply-1", "tc-1", "sunny, 18C"))
	r.Render(event.NewToolResultEndEvent("reply-1", "tc-1", message.ToolResultSuccess))

	out := buf.String()
	if !strings.Contains(out, "get_weather") {
		t.Errorf("tool call/result must show the tool name:\n%q", out)
	}
	if !strings.Contains(out, "sunny, 18C") {
		t.Errorf("tool result output missing:\n%q", out)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("success icon missing:\n%q", out)
	}
}

func TestRenderer_ToolResultTruncation(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(WithVerbosity(VerbosityDefault), WithWriter(&buf), WithColor(false), WithMaxToolResultLines(3))
	long := strings.Repeat("line\n", 10)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewToolCallStartEvent("reply-1", "tc-1", "big"))
	r.Render(event.NewToolResultStartEvent("reply-1", "tc-1", "big"))
	r.Render(event.NewToolResultTextDeltaEvent("reply-1", "tc-1", long))
	r.Render(event.NewToolResultEndEvent("reply-1", "tc-1", message.ToolResultSuccess))

	out := buf.String()
	if !strings.Contains(out, "+7 more lines") {
		t.Errorf("truncation notice missing:\n%q", out)
	}
}

func TestRenderer_DebugVerbosityShowsLifecycle(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityDebug)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewModelCallStartEvent("reply-1", "qwen-plus"))
	r.Render(event.NewModelCallEndEvent("reply-1", 10, 20))
	r.Render(event.NewReplyEndEvent("sess", "reply-1"))

	out := buf.String()
	if !strings.Contains(out, "model call") || !strings.Contains(out, "qwen-plus") {
		t.Errorf("debug model-call line missing:\n%q", out)
	}
	if !strings.Contains(out, "tokens: 10 in / 20 out") {
		t.Errorf("token note missing:\n%q", out)
	}
}

func TestRenderer_HITLNotice(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewRequireUserConfirmEvent("reply-1", []message.ToolCallBlock{
		{Type: "tool_call", ID: "tc-1", Name: "bash", Input: `{"command":"ls"}`, State: message.ToolCallAsking},
	}))

	out := buf.String()
	if !strings.Contains(out, "awaiting user confirmation") {
		t.Errorf("HITL notice missing:\n%q", out)
	}
	if !strings.Contains(out, "bash") || !strings.Contains(out, "ls") {
		t.Errorf("HITL tool call detail missing:\n%q", out)
	}
}

func TestRenderer_LastMsgAccumulates(t *testing.T) {
	r, _ := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewTextBlockStartEvent("reply-1", "blk-1"))
	r.Render(event.NewTextBlockDeltaEvent("reply-1", "blk-1", "accumulated"))
	r.Render(event.NewTextBlockEndEvent("reply-1", "blk-1"))

	msg := r.LastMsg()
	if msg == nil {
		t.Fatal("LastMsg must accumulate the reply")
	}
	if msg.ID != "reply-1" {
		t.Errorf("LastMsg.ID = %q, want reply-1", msg.ID)
	}
	txt := msg.GetTextContent("")
	if txt == nil || *txt != "accumulated" {
		t.Errorf("LastMsg text = %v, want accumulated", txt)
	}
}

func TestRenderer_UnknownEventsSilentExceptDebug(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewToolExecStartEvent("reply-1", "sess", "tc-1", "bash", "ls", "local"))
	if strings.Contains(buf.String(), "tool_exec_start") {
		t.Errorf("unknown events must be silent at default verbosity:\n%q", buf.String())
	}

	r2, buf2 := newTestRenderer(t, VerbosityDebug)
	r2.Render(event.NewToolExecStartEvent("reply-1", "sess", "tc-1", "bash", "ls", "local"))
	if !strings.Contains(buf2.String(), "tool_exec_start") {
		t.Errorf("debug verbosity must show unknown events:\n%q", buf2.String())
	}
}

func TestRenderer_LastMsgAccumulatesToolResultDataDelta(t *testing.T) {
	// HARNESS review M-2: streamed tool-result data deltas must land in
	// the accumulated message, not vanish from LastMsg.
	r, _ := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewToolResultStartEvent("reply-1", "tc-1", "imgtool"))
	r.Render(event.NewToolResultDataDeltaEvent("reply-1", "tc-1", "blk-d", "image/png", "aGVsbG8=", ""))
	r.Render(event.NewToolResultEndEvent("reply-1", "tc-1", message.ToolResultSuccess))

	msg := r.LastMsg()
	if msg == nil {
		t.Fatal("no accumulated message")
	}
	var found bool
	for _, b := range msg.Content {
		if tr, ok := b.(message.ToolResultBlock); ok && tr.ID == "tc-1" {
			blocks, ok := tr.Output.([]message.ContentBlock)
			if !ok || len(blocks) == 0 {
				t.Fatalf("data delta lost from tool result output: %v", tr.Output)
			}
			if _, ok := blocks[0].(message.DataBlock); !ok {
				t.Fatalf("expected DataBlock in output, got %T", blocks[0])
			}
			found = true
		}
	}
	if !found {
		t.Fatal("tool result block not accumulated")
	}
}

func TestRenderer_LastMsgAccumulatesHint(t *testing.T) {
	r, _ := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewHintBlockEvent("reply-1", "h-1", "system", "take care"))

	msg := r.LastMsg()
	if msg == nil {
		t.Fatal("no accumulated message")
	}
	var found bool
	for _, b := range msg.Content {
		if hb, ok := b.(message.HintBlock); ok && hb.Hint == "take care" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hint block not accumulated, content=%v", msg.Content)
	}
}

func TestRenderer_LastMsgInterleavedDataThenTextDeltas(t *testing.T) {
	// HARNESS review R6-M1: a text delta arriving after data deltas must
	// merge into the promoted block list, not clobber it.
	r, _ := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewToolResultStartEvent("reply-1", "tc-1", "mixed"))
	r.Render(event.NewToolResultDataDeltaEvent("reply-1", "tc-1", "blk-d", "image/png", "aGVsbG8=", ""))
	r.Render(event.NewToolResultTextDeltaEvent("reply-1", "tc-1", "caption text"))

	msg := r.LastMsg()
	if msg == nil {
		t.Fatal("no accumulated message")
	}
	for _, b := range msg.Content {
		if tr, ok := b.(message.ToolResultBlock); ok && tr.ID == "tc-1" {
			blocks, ok := tr.Output.([]message.ContentBlock)
			if !ok || len(blocks) != 2 {
				t.Fatalf("output must keep the data block and gain a text block, got %v", tr.Output)
			}
			if _, ok := blocks[0].(message.DataBlock); !ok {
				t.Errorf("first block must stay a DataBlock, got %T", blocks[0])
			}
			tb, ok := blocks[1].(message.TextBlock)
			if !ok || tb.Text != "caption text" {
				t.Errorf("second block must be the merged text, got %v", blocks[1])
			}
			return
		}
	}
	t.Fatal("tool result block not found")
}

func TestRenderer_ReplyEndErrorAndInterrupted(t *testing.T) {
	r, buf := newTestRenderer(t, VerbosityDefault)
	r.Render(event.NewReplyStartEvent("sess", "reply-1", "bot", message.RoleAssistant))
	r.Render(event.NewReplyEndEventWithError("sess", "reply-1", "rate_limit", "slow down"))
	out := buf.String()
	if !strings.Contains(out, "rate_limit") || !strings.Contains(out, "slow down") {
		t.Errorf("error end must render type + message:\n%q", out)
	}
	if !r.SawReplyEnd() {
		t.Error("SawReplyEnd must be true after ReplyEndEvent")
	}

	// Python parity: the interrupted notice is gated at verbosity >=
	// default; errors render at every verbosity (covered above).
	r2, buf2 := newTestRenderer(t, VerbosityQuiet)
	r2.Render(event.NewReplyStartEvent("sess", "reply-2", "bot", message.RoleAssistant))
	r2.Render(event.NewReplyEndEventWithReason("sess", "reply-2", "interrupted"))
	if strings.Contains(buf2.String(), "interrupted") {
		t.Errorf("quiet mode must not show the interrupted notice:\n%q", buf2.String())
	}
	r3, buf3 := newTestRenderer(t, VerbosityDefault)
	r3.Render(event.NewReplyStartEvent("sess", "reply-3", "bot", message.RoleAssistant))
	r3.Render(event.NewReplyEndEventWithReason("sess", "reply-3", "interrupted"))
	if !strings.Contains(buf3.String(), "interrupted") {
		t.Errorf("default verbosity must show the interrupted notice:\n%q", buf3.String())
	}
}
