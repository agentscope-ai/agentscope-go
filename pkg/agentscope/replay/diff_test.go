package replay

import (
	"strings"
	"testing"
)

func TestParseRunLog_RoundTrip(t *testing.T) {
	log := strings.Join([]string{
		`{"type":"reply_input","ts":"2026-09-01T00:00:00Z","data":{"agent":"a"}}`,
		`{"type":"event","ts":"2026-09-01T00:00:01Z","data":{"event_type":"reply_start"}}`,
		`garbage line skipped`,
		`{"type":"model_call","ts":"2026-09-01T00:00:02Z","data":{"model":"m"}}`,
	}, "\n")
	events := ParseRunLog([]byte(log))
	if len(events) != 3 {
		t.Fatalf("parsed %d lines, want 3", len(events))
	}
}

func TestDiffRunLogs_AlignsDivergedStreams(t *testing.T) {
	a := []RunEvent{
		{Type: "event", Data: []byte(`{"event_type":"reply_start"}`)},
		{Type: "event", Data: []byte(`{"event_type":"tool_call_start"}`)},
		{Type: "event", Data: []byte(`{"event_type":"tool_result_start"}`)},
		{Type: "event", Data: []byte(`{"event_type":"reply_end"}`)},
	}
	b := []RunEvent{
		{Type: "event", Data: []byte(`{"event_type":"reply_start"}`)},
		{Type: "event", Data: []byte(`{"event_type":"reply_end"}`)},
	}
	lines, truncated := DiffRunLogs(a, b)
	if truncated {
		t.Fatal("small inputs must not be truncated")
	}
	text := FormatRunDiff(lines)
	if !strings.Contains(text, "- tool_call_start") || !strings.Contains(text, "- tool_result_start") {
		t.Errorf("diff missing removed events:\n%s", text)
	}

	identicalLines, identicalTruncated := DiffRunLogs(a, a)
	if identicalTruncated {
		t.Fatal("small identical inputs must not be truncated")
	}
	if got := FormatRunDiff(identicalLines); got != "runs are structurally identical" {
		t.Errorf("identical runs diffed as: %s", got)
	}
}

func TestDiffRunLogs_TruncationFlagged(t *testing.T) {
	// HARNESS review L-1: oversized inputs are aligned on truncated
	// prefixes; the caller must be told, because divergence AFTER the
	// truncation point would otherwise compare as identical.
	mk := func(n int, divergeAtEnd bool) []RunEvent {
		ev := make([]RunEvent, 0, n)
		for i := 0; i < n; i++ {
			ev = append(ev, RunEvent{Type: "event", Data: []byte(`{"event_type":"text_delta"}`)})
		}
		if divergeAtEnd {
			ev[n-1] = RunEvent{Type: "event", Data: []byte(`{"event_type":"reply_end"}`)}
		}
		return ev
	}
	a := mk(3000, false)
	b := mk(3000, true) // divergence only in the truncated tail
	lines, truncated := DiffRunLogs(a, b)
	if !truncated {
		t.Fatal("oversized inputs must set the truncated flag")
	}
	if len(lines) == 0 {
		t.Fatal("truncated diff still returns aligned prefix lines")
	}
	// The identical-prefix comparison of the truncated inputs must not be
	// mistaken for a full-run verdict by callers that ignore the flag.
	_, smallTrunc := DiffRunLogs(mk(10, false), mk(10, true))
	if smallTrunc {
		t.Fatal("inputs under the bound must not be truncated")
	}
}
