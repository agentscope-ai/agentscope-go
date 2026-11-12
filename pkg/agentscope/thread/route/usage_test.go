package route

import (
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
)

func TestClassifierUsage(t *testing.T) {
	attempt := func(u *classifier.Usage) classifier.Attempt {
		return classifier.Attempt{Kind: classifier.PhysicalAttempt, Usage: u}
	}
	cases := []struct {
		name          string
		resp          *classifier.Response
		calls         int
		nilUsage      bool
		input, output int
		known         bool
	}{
		{name: "nil response", resp: nil, nilUsage: true, known: true},
		{name: "no attempts", resp: &classifier.Response{}, nilUsage: true, known: true},
		{name: "known", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{InputTokens: i64(100), OutputTokens: i64(4)}),
		}}, calls: 1, input: 100, output: 4, known: true},
		{name: "known zero", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{InputTokens: i64(0), OutputTokens: i64(0)}),
		}}, calls: 1, known: true},
		{name: "two attempts summed", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{InputTokens: i64(100), OutputTokens: i64(4)}),
			attempt(&classifier.Usage{InputTokens: i64(50), OutputTokens: i64(2)}),
		}}, calls: 2, input: 150, output: 6, known: true},
		{name: "usage unknown", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{InputTokens: i64(100), OutputTokens: i64(4)}),
			attempt(nil),
		}}, calls: 2, input: 100, output: 4},
		{name: "output count unknown", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{InputTokens: i64(70)}),
		}}, calls: 1, input: 70},
		{name: "input count unknown", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{OutputTokens: i64(9)}),
		}}, calls: 1, output: 9},
		{name: "negative count", resp: &classifier.Response{Attempts: []classifier.Attempt{
			attempt(&classifier.Usage{InputTokens: i64(-5), OutputTokens: i64(1)}),
		}}, calls: 1, output: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, usage, known := ClassifierUsage(tc.resp)
			if calls != tc.calls || known != tc.known || (usage == nil) != tc.nilUsage {
				t.Fatalf("got %d calls, usage %+v, known %v", calls, usage, known)
			}
			if usage != nil && (usage.InputTokens != tc.input || usage.OutputTokens != tc.output) {
				t.Fatalf("usage = %+v, want %d in, %d out", usage, tc.input, tc.output)
			}
		})
	}
}
