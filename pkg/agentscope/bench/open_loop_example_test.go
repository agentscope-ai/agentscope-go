package bench_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
)

func ExampleRunner_RunOpenLoop() {
	report, err := bench.NewRunner().RunOpenLoop(context.Background(), &bench.OpenLoopScenario{
		Name:           "offline-arrivals",
		ArrivalOffsets: []time.Duration{0, time.Millisecond, 2 * time.Millisecond},
		MaxInFlight:    3,
		Timeout:        time.Second,
		DrainTimeout:   time.Second,
		Run: func(_ context.Context, iteration int) error {
			if iteration == 2 {
				return errors.New("simulated task failure")
			}
			return nil
		},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("offered: %d\n", report.Offered)
	for _, result := range report.Results {
		fmt.Printf("%d: %s\n", result.Iteration, result.Status)
	}
	// Output:
	// offered: 3
	// 1: succeeded
	// 2: failed
	// 3: succeeded
}
