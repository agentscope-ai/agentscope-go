package agenttest

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event/streamcheck"
)

// AssertEventPresent fails the test if no event of the given type is found.
func AssertEventPresent(t *testing.T, events []event.Event, eventType event.EventType) {
	t.Helper()
	for _, ev := range events {
		if ev.GetEventType() == eventType {
			return
		}
	}
	t.Errorf("expected event type %s not found in %d events", eventType, len(events))
}

// AssertEventAbsent fails the test if an event of the given type is found.
func AssertEventAbsent(t *testing.T, events []event.Event, eventType event.EventType) {
	t.Helper()
	for _, ev := range events {
		if ev.GetEventType() == eventType {
			t.Errorf("unexpected event type %s found", eventType)
			return
		}
	}
}

// AssertNoMissingToolResults checks that every ToolResultStartEvent has a
// matching ToolResultEndEvent with the same ToolCallID.
func AssertNoMissingToolResults(t *testing.T, events []event.Event) {
	t.Helper()
	// Delegates to the single invariant implementation (HARNESS_DESIGN B3)
	// so test helpers and runtime validation cannot drift apart.
	for _, issue := range streamcheck.ToolPairingIssues(events) {
		t.Error(issue)
	}
}

// CollectEvents drains an event channel into a slice.
func CollectEvents(ch <-chan event.Event) []event.Event {
	var events []event.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}
