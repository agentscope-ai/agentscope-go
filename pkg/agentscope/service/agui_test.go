package service

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestConvertReplyStart(t *testing.T) {
	e := event.NewReplyStartEvent("sess-1", "reply-1", "bot", "assistant")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIRunStarted {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIRunStarted)
	}
	if agui.RunID != "reply-1" {
		t.Errorf("RunID = %q, want %q", agui.RunID, "reply-1")
	}
}

func TestConvertReplyEnd(t *testing.T) {
	e := event.NewReplyEndEvent("sess-1", "reply-1")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIRunFinished {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIRunFinished)
	}
}

func TestConvertExceedMaxIters(t *testing.T) {
	e := event.NewExceedMaxItersEvent("reply-1", "bot")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIRunError {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIRunError)
	}
	if agui.Error == "" {
		t.Error("expected non-empty error")
	}
}

func TestConvertTextBlockDelta(t *testing.T) {
	e := event.NewTextBlockDeltaEvent("reply-1", "blk-1", "hello ")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUITextMsgContent {
		t.Errorf("Type = %q, want %q", agui.Type, AGUITextMsgContent)
	}
	if agui.Delta != "hello " {
		t.Errorf("Delta = %q, want %q", agui.Delta, "hello ")
	}
}

func TestConvertTextBlockStart(t *testing.T) {
	e := event.NewTextBlockStartEvent("reply-1", "blk-1")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUITextMsgStart {
		t.Errorf("Type = %q, want %q", agui.Type, AGUITextMsgStart)
	}
	if agui.Role != "assistant" {
		t.Errorf("Role = %q, want %q", agui.Role, "assistant")
	}
}

func TestConvertTextBlockEnd(t *testing.T) {
	e := event.NewTextBlockEndEvent("reply-1", "blk-1")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUITextMsgEnd {
		t.Errorf("Type = %q, want %q", agui.Type, AGUITextMsgEnd)
	}
}

func TestConvertToolCallStart(t *testing.T) {
	e := event.NewToolCallStartEvent("reply-1", "tc-1", "get_weather")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIToolCallStart {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIToolCallStart)
	}
	if agui.ToolCallID != "tc-1" {
		t.Errorf("ToolCallID = %q, want %q", agui.ToolCallID, "tc-1")
	}
	if agui.ToolCallName != "get_weather" {
		t.Errorf("ToolCallName = %q, want %q", agui.ToolCallName, "get_weather")
	}
}

func TestConvertToolCallDelta(t *testing.T) {
	e := event.NewToolCallDeltaEvent("reply-1", "tc-1", `{"loc`)
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIToolCallArgs {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIToolCallArgs)
	}
	if agui.Args != `{"loc` {
		t.Errorf("Args = %q, want %q", agui.Args, `{"loc`)
	}
}

func TestConvertToolCallEnd(t *testing.T) {
	e := event.NewToolCallEndEvent("reply-1", "tc-1")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIToolCallEnd {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIToolCallEnd)
	}
}

func TestConvertToolResultEnd(t *testing.T) {
	e := event.NewToolResultEndEvent("reply-1", "tc-1", message.ToolResultSuccess)
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIToolCallResult {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIToolCallResult)
	}
	if agui.Result != string(message.ToolResultSuccess) {
		t.Errorf("Result = %q, want %q", agui.Result, string(message.ToolResultSuccess))
	}
}

func TestConvertModelCallStart(t *testing.T) {
	e := event.NewModelCallStartEvent("reply-1", "gpt-4o")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIStepStarted {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIStepStarted)
	}
}

func TestConvertModelCallEnd(t *testing.T) {
	e := event.NewModelCallEndEvent("reply-1", 100, 50)
	agui := ConvertToAGUI(e)
	if agui.Type != AGUIStepFinished {
		t.Errorf("Type = %q, want %q", agui.Type, AGUIStepFinished)
	}
}

func TestConvertUnknownEventToCustom(t *testing.T) {
	e := event.NewDataBlockStartEvent("reply-1", "blk-1", "image/png")
	agui := ConvertToAGUI(e)
	if agui.Type != AGUICustomEvent {
		t.Errorf("Type = %q, want %q", agui.Type, AGUICustomEvent)
	}
	if agui.Name == "" {
		t.Error("expected non-empty Name for custom event")
	}
}
