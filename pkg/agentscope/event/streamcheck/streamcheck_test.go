package streamcheck

import (
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func wellFormedStream() []event.Event {
	return []event.Event{
		event.NewReplyStartEvent("s", "r1", "agent", message.RoleAssistant),
		event.NewTextBlockStartEvent("r1", "b1"),
		event.NewTextBlockEndEvent("r1", "b1"),
		event.NewToolCallStartEvent("r1", "tc1", "search"),
		event.NewToolCallEndEvent("r1", "tc1"),
		event.NewToolResultStartEvent("r1", "tc1", "search"),
		event.NewToolResultEndEvent("r1", "tc1", message.ToolResultSuccess),
		event.NewReplyEndEvent("s", "r1"),
	}
}

func TestValidate_WellFormedPasses(t *testing.T) {
	if err := Validate(wellFormedStream()); err != nil {
		t.Errorf("well-formed stream rejected: %v", err)
	}
}

func TestValidate_DetectsMissingToolResult(t *testing.T) {
	events := wellFormedStream()
	// Drop the ToolResultEnd (index 6).
	events = append(events[:6], events[7:]...)
	err := Validate(events)
	if err == nil {
		t.Fatal("missing tool result not detected")
	}
	if !strings.Contains(err.Error(), "tool result started but not ended") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_DetectsUnpairedReplyAndBlock(t *testing.T) {
	events := []event.Event{
		event.NewReplyStartEvent("s", "r1", "agent", message.RoleAssistant),
		event.NewTextBlockStartEvent("r1", "b1"),
		event.NewReplyEndEvent("s", "r2"), // end without start
	}
	err := Validate(events)
	if err == nil {
		t.Fatal("violations not detected")
	}
	msg := err.Error()
	for _, want := range []string{"ReplyStart without ReplyEnd", "block start without end", "ReplyEnd without ReplyStart"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing violation %q in: %s", want, msg)
		}
	}
}

func TestToolPairingIssues_Delegatable(t *testing.T) {
	events := wellFormedStream()
	if issues := ToolPairingIssues(events); len(issues) != 0 {
		t.Errorf("clean stream reported issues: %v", issues)
	}
	broken := []event.Event{event.NewToolResultStartEvent("r1", "tcX", "tool")}
	issues := ToolPairingIssues(broken)
	if len(issues) != 1 || !strings.Contains(issues[0], "tcX") {
		t.Errorf("issues = %v", issues)
	}
}
