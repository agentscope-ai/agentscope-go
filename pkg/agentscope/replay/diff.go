package replay

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RunEvent is one parsed line of a RunJSONL log (HARNESS_DESIGN A3/A4).
type RunEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ParseRunLog parses JSONL run-log bytes into events.
func ParseRunLog(data []byte) []RunEvent {
	var out []RunEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var outer struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &outer); err != nil {
			continue
		}
		out = append(out, RunEvent{Type: outer.Type, Data: outer.Data})
	}
	return out
}

// eventTypeName extracts the inner event type for "event" lines so alignment
// works on semantic types, not just line types.
func eventTypeName(e RunEvent) string {
	if e.Type != "event" {
		return e.Type
	}
	var inner struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(e.Data, &inner); err == nil && inner.EventType != "" {
		return inner.EventType
	}
	return "event"
}

// RunDiffLine is one aligned difference entry.
type RunDiffLine struct {
	Op    string // "same" | "a_only" | "b_only"
	Index int    // position in the respective run
	Type  string
}

// DiffRunLogs aligns two run logs by event-type sequence (LCS edit-distance
// alignment, HARNESS_DESIGN A4) — index-based alignment fails exactly where
// runs diverge, so we align on the longest common subsequence of types.
// diffMaxProduct bounds the LCS alignment table (HARNESS review M11):
// RunJSONL records every streaming delta, so long replies would otherwise
// allocate an O(n×m) table. Beyond the bound both inputs are truncated to
// equal-size prefixes and the returned truncated flag is true — callers
// must surface this, because runs that only diverge AFTER the truncation
// point compare as identical (HARNESS review L-1).
const diffMaxProduct = 4_000_000

func DiffRunLogs(a, b []RunEvent) (lines []RunDiffLine, truncated bool) {
	if len(a)*len(b) > diffMaxProduct {
		truncated = true
		limit := 2000
		if len(a) > limit {
			a = a[:limit]
		}
		if len(b) > limit {
			b = b[:limit]
		}
	}
	na, nb := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		na[i] = eventTypeName(a[i])
	}
	for i := range b {
		nb[i] = eventTypeName(b[i])
	}

	// LCS dynamic program.
	n, m := len(na), len(nb)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if na[i] == nb[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var out []RunDiffLine
	i, j := 0, 0
	for i < n && j < m {
		if na[i] == nb[j] {
			out = append(out, RunDiffLine{Op: "same", Index: i, Type: na[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			out = append(out, RunDiffLine{Op: "a_only", Index: i, Type: na[i]})
			i++
		} else {
			out = append(out, RunDiffLine{Op: "b_only", Index: j, Type: nb[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, RunDiffLine{Op: "a_only", Index: i, Type: na[i]})
	}
	for ; j < m; j++ {
		out = append(out, RunDiffLine{Op: "b_only", Index: j, Type: nb[j]})
	}
	return out, truncated
}

// FormatRunDiff renders the diff human-readable.
func FormatRunDiff(lines []RunDiffLine) string {
	var sb strings.Builder
	diverged := false
	for _, l := range lines {
		switch l.Op {
		case "same":
			if !diverged {
				continue // leading common prefix stays quiet
			}
			fmt.Fprintf(&sb, "  %s\n", l.Type)
		case "a_only":
			diverged = true
			fmt.Fprintf(&sb, "- %s (run A #%d)\n", l.Type, l.Index)
		case "b_only":
			diverged = true
			fmt.Fprintf(&sb, "+ %s (run B #%d)\n", l.Type, l.Index)
		}
	}
	if sb.Len() == 0 {
		return "runs are structurally identical"
	}
	return sb.String()
}
