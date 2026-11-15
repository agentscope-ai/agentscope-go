// Package streamcheck validates structural invariants of recorded agent
// event streams (HARNESS_DESIGN B3): start/end pairing for replies, blocks,
// tool calls, and tool results. It is the single invariant implementation;
// test helpers (agenttest) delegate here instead of carrying their own.
//
// Two usage modes:
//   - test assertions on recorded streams;
//   - optional dev-mode runtime validation middleware (opt-in; production
//     keeps it off for zero overhead).
package streamcheck

import (
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// Validate checks all stream invariants and returns a combined error
// describing every violation (nil when the stream is well-formed).
// The input must be a complete, ordered stream as emitted for one or more
// replies; truncated streams legitimately fail (that is the point).
func Validate(events []event.Event) error {
	var issues []string
	issues = append(issues, ReplyPairingIssues(events)...)
	issues = append(issues, BlockPairingIssues(events)...)
	issues = append(issues, OrphanDeltaIssues(events)...)
	issues = append(issues, ToolCallPairingIssues(events)...)
	issues = append(issues, ToolPairingIssues(events)...)
	if len(issues) == 0 {
		return nil
	}
	return fmt.Errorf("streamcheck: %d invariant violation(s): %s",
		len(issues), strings.Join(issues, "; "))
}

// ReplyPairingIssues reports ReplyStart/ReplyEnd mismatches.
func ReplyPairingIssues(events []event.Event) []string {
	var issues []string
	open := map[string]int{} // reply_id -> start count
	for _, ev := range events {
		switch e := ev.(type) {
		case event.ReplyStartEvent:
			open[e.ReplyID]++
			if open[e.ReplyID] > 1 {
				issues = append(issues, fmt.Sprintf("duplicate ReplyStart for reply %q", e.ReplyID))
			}
		case event.ReplyEndEvent:
			if open[e.ReplyID] == 0 {
				issues = append(issues, fmt.Sprintf("ReplyEnd without ReplyStart for reply %q", e.ReplyID))
			} else {
				open[e.ReplyID]--
			}
		}
	}
	for id, n := range open {
		for ; n > 0; n-- {
			issues = append(issues, fmt.Sprintf("ReplyStart without ReplyEnd for reply %q", id))
		}
	}
	return issues
}

// BlockPairingIssues reports content-block Start/End mismatches (text,
// thinking, data blocks) keyed by block ID.
func BlockPairingIssues(events []event.Event) []string {
	var issues []string
	open := map[string]int{}
	start := func(id string) {
		open[id]++
		if open[id] > 1 {
			issues = append(issues, fmt.Sprintf("duplicate block start for block %q", id))
		}
	}
	end := func(id string) {
		if open[id] == 0 {
			issues = append(issues, fmt.Sprintf("block end without start for block %q", id))
		} else {
			open[id]--
		}
	}
	for _, ev := range events {
		switch e := ev.(type) {
		case event.TextBlockStartEvent:
			start(e.BlockID)
		case event.TextBlockEndEvent:
			end(e.BlockID)
		case event.ThinkingBlockStartEvent:
			start(e.BlockID)
		case event.ThinkingBlockEndEvent:
			end(e.BlockID)
		case event.DataBlockStartEvent:
			start(e.BlockID)
		case event.DataBlockEndEvent:
			end(e.BlockID)
		}
	}
	for id, n := range open {
		for ; n > 0; n-- {
			issues = append(issues, fmt.Sprintf("block start without end for block %q", id))
		}
	}
	return issues
}

// ToolCallPairingIssues reports ToolCallStart/ToolCallEnd mismatches keyed
// by tool call ID.
func ToolCallPairingIssues(events []event.Event) []string {
	var issues []string
	open := map[string]int{}
	for _, ev := range events {
		switch e := ev.(type) {
		case event.ToolCallStartEvent:
			open[e.ToolCallID]++
		case event.ToolCallEndEvent:
			if open[e.ToolCallID] == 0 {
				issues = append(issues, fmt.Sprintf("ToolCallEnd without ToolCallStart for call %q", e.ToolCallID))
			} else {
				open[e.ToolCallID]--
			}
		}
	}
	for id, n := range open {
		for ; n > 0; n-- {
			issues = append(issues, fmt.Sprintf("ToolCallStart without ToolCallEnd for call %q", id))
		}
	}
	return issues
}

// ToolPairingIssues reports ToolResultStart/ToolResultEnd mismatches keyed
// by tool call ID (the classic "missing tool result" defect class).
func ToolPairingIssues(events []event.Event) []string {
	var issues []string
	open := map[string]int{}
	for _, ev := range events {
		switch e := ev.(type) {
		case event.ToolResultStartEvent:
			open[e.ToolCallID]++
		case event.ToolResultEndEvent:
			if open[e.ToolCallID] == 0 {
				issues = append(issues, fmt.Sprintf("tool result end without start for call %q", e.ToolCallID))
			} else {
				open[e.ToolCallID]--
			}
		}
	}
	for id, n := range open {
		for ; n > 0; n-- {
			issues = append(issues, fmt.Sprintf("tool result started but not ended for call %q", id))
		}
	}
	return issues
}

// OrphanDeltaIssues reports content deltas that arrive without a matching
// block-start event (HARNESS review M9; the design promises "无孤儿 delta").
func OrphanDeltaIssues(events []event.Event) []string {
	var issues []string
	openBlocks := map[string]bool{}
	for _, ev := range events {
		switch e := ev.(type) {
		case event.TextBlockStartEvent:
			openBlocks[e.BlockID] = true
		case event.TextBlockEndEvent:
			delete(openBlocks, e.BlockID)
		case event.ThinkingBlockStartEvent:
			openBlocks[e.BlockID] = true
		case event.ThinkingBlockEndEvent:
			delete(openBlocks, e.BlockID)
		case event.DataBlockStartEvent:
			openBlocks[e.BlockID] = true
		case event.DataBlockEndEvent:
			delete(openBlocks, e.BlockID)
		case event.TextBlockDeltaEvent:
			if !openBlocks[e.BlockID] {
				issues = append(issues, fmt.Sprintf("text delta without block start for block %q", e.BlockID))
			}
		case event.ThinkingBlockDeltaEvent:
			if !openBlocks[e.BlockID] {
				issues = append(issues, fmt.Sprintf("thinking delta without block start for block %q", e.BlockID))
			}
		case event.DataBlockDeltaEvent:
			if !openBlocks[e.BlockID] {
				issues = append(issues, fmt.Sprintf("data delta without block start for block %q", e.BlockID))
			}
		}
	}
	return issues
}
