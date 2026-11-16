package route

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ClassifierUsage sums a classifier response's accounting for the thread
// ledger, where the Coordinator records it as thread.UsageClassifier. calls
// counts every attempt, physical or simulated. usage sums the input and
// output tokens the attempts reported; known is false when any attempt's
// usage or token count is missing (or negative), in which case usage holds
// only the known part. A nil response or one without attempts gives
// (0, nil, true).
func ClassifierUsage(resp *classifier.Response) (calls int, usage *model.ChatUsage, known bool) {
	if resp == nil || len(resp.Attempts) == 0 {
		return 0, nil, true
	}
	usage = &model.ChatUsage{}
	known = true
	add := func(dst *int, n *int64) {
		if n == nil || *n < 0 {
			known = false
			return
		}
		*dst += int(*n)
	}
	for i := range resp.Attempts {
		u := resp.Attempts[i].Usage
		if u == nil {
			known = false
			continue
		}
		add(&usage.InputTokens, u.InputTokens)
		add(&usage.OutputTokens, u.OutputTokens)
	}
	return len(resp.Attempts), usage, known
}
