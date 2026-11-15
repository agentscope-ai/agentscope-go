package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// FanOutResult holds the outcome of a single agent invocation in FanOut.
type FanOutResult struct {
	AgentName string
	Output    *message.Msg
	Err       error
	Duration  time.Duration
}

type fanOutConfig struct {
	firstN  int
	timeout time.Duration
}

// FanOutOption configures FanOut.
type FanOutOption func(*fanOutConfig)

// FirstN makes FanOut return after the first n results arrive, canceling the
// remaining agents. A value <= 0 disables early return (wait for all).
func FirstN(n int) FanOutOption {
	return func(c *fanOutConfig) { c.firstN = n }
}

// WithFanOutTimeout bounds the total time FanOut waits for results.
func WithFanOutTimeout(d time.Duration) FanOutOption {
	return func(c *fanOutConfig) { c.timeout = d }
}

// FanOut runs the given agents concurrently on the same input and collects
// their results. If FirstN is set, it returns once that many results have
// arrived and cancels the rest. Results are returned in completion order.
func FanOut(ctx context.Context, agents []agent.Agent, input string, opts ...FanOutOption) ([]FanOutResult, error) {
	cfg := &fanOutConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resultCh := make(chan FanOutResult, len(agents))

	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a agent.Agent) {
			defer wg.Done()
			start := time.Now()
			out, err := a.Reply(runCtx, input)
			resultCh <- FanOutResult{
				AgentName: a.ID(),
				Output:    out,
				Err:       err,
				Duration:  time.Since(start),
			}
		}(a)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	want := len(agents)
	if cfg.firstN > 0 && cfg.firstN < want {
		want = cfg.firstN
	}

	results := make([]FanOutResult, 0, want)
	for r := range resultCh {
		results = append(results, r)
		if len(results) >= want {
			cancel() // stop the remaining agents
			break
		}
	}

	if ctx.Err() != nil && len(results) < want {
		return results, ctx.Err()
	}
	return results, nil
}
