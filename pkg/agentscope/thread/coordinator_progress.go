package thread

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

// planEvent names the custom event an agent's update_plan tool emits
// (agent.WithPlanTool).
const planEvent = "plan_updated"

// requestQuoteRunes bounds the user's request a failover or restart note
// quotes. The request itself is in the context the import built.
const requestQuoteRunes = 120

// planText renders a plan_updated event's plan as numbered steps with their
// status, then its explanation; "" for any other event or an empty plan.
func planText(ev event.Event) string {
	ce, ok := ev.(event.CustomEvent)
	if !ok || ce.Name != planEvent {
		return ""
	}
	items, _ := ce.Value["plan"].([]any)
	var b strings.Builder
	n := 0
	for _, it := range items {
		m, _ := it.(map[string]any)
		step, _ := m["step"].(string)
		if strings.TrimSpace(step) == "" {
			continue
		}
		status, _ := m["status"].(string)
		n++
		fmt.Fprintf(&b, "%d. [%s] %s\n", n, status, oneLine(step))
	}
	if n == 0 {
		return ""
	}
	if x, _ := ce.Value["explanation"].(string); strings.TrimSpace(x) != "" {
		b.WriteString(oneLine(x))
	}
	return strings.TrimSpace(b.String())
}

// recordProgress writes the plan an activation's agent just published as a
// progress entry, so the log holds where the run stands if it fails or the
// process stops.
func (c *Coordinator) recordProgress(a *activation, plan string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	ps := a.part
	a.progressN++
	stored := c.commitLocked(ps.name, []Entry{{
		Kind:     KindProgress,
		Origin:   Origin{Participant: ps.name},
		BasedOn:  a.trigger.Seq,
		Run:      a.id,
		Content:  textContent(plan),
		DedupKey: fmt.Sprintf("progress:%s:%d", a.id, a.progressN),
	}}, nil)
	if len(stored) == 1 && c.answersCycleLocked(a) {
		p := cloneEntry(&stored[0])
		c.cycle.progress = &p
	}
}

// quoteRequest quotes the start of a request on one line.
func quoteRequest(s string) string {
	return fmt.Sprintf("%q", truncateRunes(oneLine(s), requestQuoteRunes))
}

// failoverNote is a failover's input: one line that says who failed and
// points at what the import already brought into the context (the failed
// marker, the summary and entries covering the failed run, its newest
// progress) instead of repeating it.
func failoverNote(a *activation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s failed: %s", a.failFrom, a.failReason)
	if a.failSeq != 0 {
		fmt.Fprintf(&b, " (#%d)", a.failSeq)
	}
	fmt.Fprintf(&b, ". You now answer the user: continue their request #%d %s from where %s left off. "+
		"What %s did is in the summary and the entries above", a.cycleInput, quoteRequest(a.userText), a.failFrom, a.failFrom)
	if a.progress != 0 {
		fmt.Fprintf(&b, "; its latest progress is #%d", a.progress)
	}
	b.WriteString(".]")
	return b.String()
}

// restartNote is the input of an activation that resumes a request the
// stopped process was answering. Nothing that ran before the restart is
// re-run: the participant checks the actual state first.
func restartNote(a *activation, hostNote string) string {
	var b strings.Builder
	switch {
	case len(a.recoverWith) == 0:
		fmt.Fprintf(&b, "[The session restarted while the user's request #%d %s was being answered", a.cycleInput, quoteRequest(a.userText))
	default:
		// The joined inputs are quoted with the request, in log order.
		msgs := append([]*Entry{{Seq: a.cycleInput, Content: textContent(a.userText)}}, a.recoverWith...)
		slices.SortFunc(msgs, func(x, y *Entry) int { return cmp.Compare(x.Seq, y.Seq) })
		b.WriteString("[The session restarted while the user's messages ")
		for i, e := range msgs {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "#%d %s", e.Seq, quoteRequest(e.Text()))
		}
		b.WriteString(" were being answered")
	}
	if line := truncateRunes(oneLine(hostNote), 200); line != "" {
		fmt.Fprintf(&b, " (host: %s)", line)
	}
	b.WriteString(". Work done before the restart may or may not have taken effect: check the actual state before you redo anything")
	if a.failSeq != 0 {
		fmt.Fprintf(&b, ". A failure before it is recorded in #%d", a.failSeq)
	}
	if a.progress != 0 {
		fmt.Fprintf(&b, ". The latest progress is #%d", a.progress)
	}
	b.WriteString(". Continue the request.]")
	return b.String()
}
