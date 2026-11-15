package thread

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Descriptor describes a participant to the Coordinator and to its peers.
type Descriptor struct {
	// Name identifies the participant. It may not contain "/", which
	// separates the participant from the agent path in an Origin.
	Name string
	// Zone is the participant's trust zone.
	Zone Zone
	// Description says what the participant can do; its peers see it.
	Description string
	// Window is the participant's context window in tokens; 0 means unknown.
	Window int
	// SummaryBudget is the target size in tokens of a summary written for
	// this participant (a cloud fork writing for an edge peer). Zero sizes it
	// from the participant: 5/6 of the summary budget its State reports
	// (stage 0 gave 2,500 of 3,000), else 5/16 of its window, else 1,024.
	SummaryBudget int
	// BriefMaxRunes is the longest brief the participant accepts; 0 means no
	// limit.
	BriefMaxRunes int
	// ToolGroups names the tool groups the participant holds, such as
	// "device" or "files".
	ToolGroups []string
}

// Validate checks the descriptor: a name without "/", zone edge or cloud,
// and no negative window, summary budget or brief limit.
func (d Descriptor) Validate() error { //nolint:gocritic // value receiver keeps the documented API
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("thread: participant name is required")
	}
	if strings.Contains(d.Name, "/") {
		return fmt.Errorf("thread: participant name %q may not contain \"/\"", d.Name)
	}
	if d.Zone != ZoneEdge && d.Zone != ZoneCloud {
		return fmt.Errorf("thread: participant %q has zone %q, want %q or %q", d.Name, d.Zone, ZoneEdge, ZoneCloud)
	}
	if d.Window < 0 {
		return fmt.Errorf("thread: participant %q has a negative window", d.Name)
	}
	if d.SummaryBudget < 0 {
		return fmt.Errorf("thread: participant %q has a negative summary budget", d.Name)
	}
	if d.BriefMaxRunes < 0 {
		return fmt.Errorf("thread: participant %q has a negative brief limit", d.Name)
	}
	return nil
}

// RenderPeers returns the stable text an agent's system prompt carries about
// the other participants: name, zone, window and description, sorted by
// name, excluding self. It is byte-stable for the same input, whatever its
// order, because it sits in a cached prompt prefix. It is empty when there
// are no peers.
func RenderPeers(self string, all []Descriptor) string {
	peers := make([]*Descriptor, 0, len(all))
	for i := range all {
		if all[i].Name != self {
			peers = append(peers, &all[i])
		}
	}
	if len(peers) == 0 {
		return ""
	}
	slices.SortFunc(peers, func(a, b *Descriptor) int {
		return cmp.Or(
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Zone, b.Zone),
			cmp.Compare(a.Window, b.Window),
			cmp.Compare(a.Description, b.Description),
		)
	})
	var sb strings.Builder
	sb.WriteString("Other participants in this session:")
	for _, p := range peers {
		sb.WriteString("\n- ")
		sb.WriteString(oneLine(p.Name))
		sb.WriteString(" (zone ")
		sb.WriteString(string(p.Zone))
		if p.Window > 0 {
			fmt.Fprintf(&sb, ", window %d tokens", p.Window)
		} else {
			sb.WriteString(", window unknown")
		}
		sb.WriteString(")")
		if desc := oneLine(p.Description); desc != "" {
			sb.WriteString(": ")
			sb.WriteString(desc)
		}
	}
	return sb.String()
}
