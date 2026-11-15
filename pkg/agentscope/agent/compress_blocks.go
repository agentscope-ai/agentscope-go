package agent

import (
	"slices"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type compressionBoundary struct {
	msg, block, position int
}

func terminalCompressionResult(state message.ToolResultState) bool {
	switch state {
	case message.ToolResultSuccess, message.ToolResultError, message.ToolResultDenied, message.ToolResultInterrupted:
		return true
	default:
		return false
	}
}

// splitContentBlocksForCompression plans a contiguous prefix/suffix without
// mutating the snapshot. Interior cuts follow completed results, so the next
// exchange keeps its preceding thinking (including signatures), text and media.
// Call/result intervals rule out cuts through interleaved or cross-message
// groups. Missing terminal results pin the suffix, even above the target.
func (a *UnifiedAgent) splitContentBlocksForCompression(msgs, base []*message.Msg, budget int, tools []model.ToolSchema) ([]*message.Msg, []*message.Msg) {
	if budget <= 0 {
		return ownCompressionMessages(msgs), nil // existing wholesale emergency policy
	}

	var cuts []compressionBoundary
	calls, results, allResults := make(map[string]int), make(map[string]int), make(map[string]int)
	position := 0
	preludeStart := -1
	ambiguous := false
	for mi, msg := range msgs {
		cuts = append(cuts, compressionBoundary{msg: mi, position: position})
		if msg == nil || len(msg.Content) == 0 {
			position++
			continue
		}
		ownAssistant := msg.Role == message.RoleAssistant && msg.Name == a.name
		if !ownAssistant {
			preludeStart = -1
		} else if preludeStart < 0 {
			preludeStart = position
		}
		for bi, block := range msg.Content {
			switch b := block.(type) {
			case message.ToolCallBlock:
				if _, exists := calls[b.ID]; exists || b.ID == "" {
					ambiguous = true
				}
				calls[b.ID] = position
				if ownAssistant {
					// Adjacent assistant messages can hold one tool-use turn.
					// Its leading reasoning (including signatures), text and media
					// must travel with the call, even across message boundaries.
					calls[b.ID] = preludeStart
				}
			case message.ToolResultBlock:
				allResults[b.ID] = position
				if terminalCompressionResult(b.State) {
					if _, exists := results[b.ID]; exists {
						ambiguous = true
					}
					results[b.ID] = position
					if ownAssistant {
						preludeStart = position + 1
						if bi+1 < len(msg.Content) {
							cuts = append(cuts, compressionBoundary{msg: mi, block: bi + 1, position: position + 1})
						}
					}
				}
			}
			position++
		}
	}
	cuts = append(cuts, compressionBoundary{msg: len(msgs), position: position})
	if ambiguous {
		// Reused IDs cannot be paired reliably. Do not invent a boundary; the
		// caller can apply its existing reserve-zero fallback.
		return nil, ownCompressionMessages(msgs)
	}

	// Range additions mark every cut inside a complete exchange. Overlapping
	// intervals make parallel groups indivisible without quadratic pair repair.
	blocked := make([]int, position+2)
	pinned, orphanEnd := position, 0
	for id, call := range calls {
		if result, ok := results[id]; ok {
			blocked[min(call, result)+1]++
			blocked[max(call, result)+1]--
		} else {
			pinned = min(pinned, call)
		}
	}
	for id, result := range allResults {
		if _, ok := calls[id]; !ok {
			orphanEnd = max(orphanEnd, result+1)
		}
	}
	for i := 1; i <= position; i++ {
		blocked[i] += blocked[i-1]
	}

	best := -1
	latestPinned := -1
	for i := len(cuts) - 1; i >= 0; i-- {
		cut := cuts[i]
		if cut.position > pinned || cut.position < orphanEnd || blocked[cut.position] != 0 {
			continue
		}
		if latestPinned < 0 {
			latestPinned = i
		}
		_, reserved := cutCompressionMessages(msgs, cut)
		candidate := append(slices.Clone(base), reserved...)
		// CountTokens has no monotonicity contract; a larger suffix can have
		// a smaller estimate, so examine every eligible boundary.
		if a.model.CountTokens(candidate, tools) <= budget {
			best = i
		}
	}
	if best < 0 && pinned < position {
		best = latestPinned // protect an unfinished group even if it exceeds the target
	}
	if best < 0 {
		return ownCompressionMessages(msgs), nil
	}
	compressed, reserved := cutCompressionMessages(msgs, cuts[best])
	return ownCompressionMessages(compressed), ownCompressionMessages(reserved)
}

// The temporary fragments borrow immutable blocks for sizing. Only the final
// plan is owned. Usage belongs to the retained fragment of a split message;
// duplicating it on the prefix would double-count historical model calls.
func cutCompressionMessages(msgs []*message.Msg, cut compressionBoundary) ([]*message.Msg, []*message.Msg) {
	compressed := slices.Clone(msgs[:cut.msg])
	reserved := slices.Clone(msgs[cut.msg:])
	if cut.block > 0 {
		prefix, suffix := *msgs[cut.msg], *msgs[cut.msg]
		prefix.Content, suffix.Content = prefix.Content[:cut.block], suffix.Content[cut.block:]
		prefix.Usage = nil
		compressed = append(compressed, &prefix)
		reserved[0] = &suffix
	}
	return compressed, reserved
}

// These are the containers the agent may mutate after publication (append
// content, update call states, accumulate usage). Nested block metadata/output
// is preserved without rewriting it, just as in image-limit copy-on-write.
func ownCompressionMessages(msgs []*message.Msg) []*message.Msg {
	out := slices.Clone(msgs)
	for i, msg := range msgs {
		if msg == nil {
			continue
		}
		cp := *msg
		cp.Content = slices.Clone(msg.Content)
		if msg.Usage != nil {
			usage := *msg.Usage
			cp.Usage = &usage
		}
		out[i] = &cp
	}
	return out
}
