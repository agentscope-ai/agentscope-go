package formatter

import (
	"regexp"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// The Chat Completions API only accepts message names matching
// ^[^\s<|\/>]+$. Legal names must keep their dashes and dots, and each run
// of illegal characters must collapse to one underscore (upstream #3548).
var apiMessageNamePattern = regexp.MustCompile(`^[^\s<|\\/>]+$`)

func TestMultiAgentNameSanitizer(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Research Assistant", "Research_Assistant"},
		{"John Doe", "John_Doe"},
		{"Team-1.v2", "Team-1.v2"}, // legal dash and dot are preserved
		{"a  b", "a_b"},            // a run of illegal characters collapses
		{"a\tb\nc", "a_b_c"},
		{"a<b", "a_b"},
		{"a|b", "a_b"},
		{`a\b`, "a_b"},
		{"a/b", "a_b"},
		{"a>b", "a_b"},
		{"<|/>", "_"},
		{"mixed  <|\\/> end", "mixed_end"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []*message.Msg{message.UserMsg(tc.name, "hi")}
			result, err := NewOpenAIFormatter().FormatMultiAgent(msgs, "current")
			if err != nil {
				t.Fatal(err)
			}
			if len(result) != 1 {
				t.Fatalf("formatted into %d messages: %v", len(result), result)
			}
			got, ok := result[0]["name"].(string)
			if !ok || got != tc.want {
				t.Fatalf("name = %q (present=%v), want %q", got, ok, tc.want)
			}
			if !apiMessageNamePattern.MatchString(got) {
				t.Fatalf("sanitized name %q still violates the API pattern", got)
			}
		})
	}
}
