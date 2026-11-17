package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// Check the budget independently of the implementation, in quarter units.
func checkToolTextBudget(t *testing.T, text string, limit int) {
	t.Helper()
	quarters := 0
	for _, b := range []byte(text) {
		if b < utf8.RuneSelf {
			quarters++
		} else {
			quarters += 4
		}
	}
	if !utf8.ValidString(text) || quarters > 4*limit {
		t.Fatalf("output exceeds budget %d or is invalid UTF-8: %d quarter units, %q", limit, quarters, text)
	}
}

func TestTruncateToolResultBudget(t *testing.T) {
	for _, tt := range []struct {
		name, input, head, tail string
		limit                   int
	}{
		{"ASCII marker", "START" + strings.Repeat("x", 1000) + "END", "START", "END", 100},
		{"mixed scripts", strings.Repeat("中", 100) + strings.Repeat("x", 80) + "END", "中", "END", 100},
		{"tiny budget", strings.Repeat("中", 20), "", "", 1},
		{"zero budget", strings.Repeat("中", 20), "", "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, cut := TruncateToolResult(tt.input, tt.limit)
			if !cut {
				t.Fatalf("expected truncation for %q", tt.name)
			}
			checkToolTextBudget(t, got, tt.limit)
			if !strings.HasPrefix(got, tt.head) || !strings.HasSuffix(got, tt.tail) {
				t.Fatalf("missing head or tail: %q", got)
			}
			if again, recut := TruncateToolResult(got, tt.limit); recut || again != got {
				t.Fatalf("fitted output was truncated again: %q", again)
			}
		})
	}
	if got, cut := TruncateToolResult("中abcd", 4); cut || got != "中abcd" {
		t.Fatalf("exactly at budget: got %q, truncated=%v", got, cut)
	}
}

func TestToolResultBudgetWithOffload(t *testing.T) {
	input := "START" + strings.Repeat("中x", 100) + "END"
	const path = "/workspace/offloaded/tool_result_call.txt"
	for _, tt := range []struct {
		name     string
		limit    int
		wantPath bool
	}{
		{"full reminder", 60, true},
		{"compact reminder keeps both ends", 32, true},
		{"compact reminder", 15, true},
		{"path too large", 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			off := &fakeOffloader{}
			a := UnifiedAgent{contextCfg: &ContextConfig{ToolResultLimit: tt.limit}, offloader: off}
			got := a.truncateToolResult(context.Background(), input, "call")
			checkToolTextBudget(t, got, tt.limit)
			if strings.Contains(got, path) != tt.wantPath {
				t.Fatalf("full path present=%v, want %v: %q", !tt.wantPath, tt.wantPath, got)
			}
			if tt.limit >= 32 && (!strings.HasPrefix(got, "START") || !strings.Contains(got, "END\n")) {
				t.Fatalf("expected both ends before the offload reference: %q", got)
			}
			if len(off.contents) != 1 || off.contents[0] != input {
				t.Fatal("expected the original result to be offloaded exactly once")
			}
		})
	}
}
