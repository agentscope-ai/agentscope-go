// Command rundiff contrasts two RunJSONL run logs (HARNESS_DESIGN A4):
// it aligns the event streams by type (LCS) and prints where the runs
// diverge. Useful for comparing a reply before/after a prompt or model
// change.
//
// Usage:
//
//	rundiff <base.jsonl> <candidate.jsonl>
package main

import (
	"fmt"
	"os"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: rundiff <base.jsonl> <candidate.jsonl>")
		os.Exit(2)
	}
	load := func(path string) []replay.RunEvent {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		return replay.ParseRunLog(data)
	}
	a, b := load(os.Args[1]), load(os.Args[2])
	lines, truncated := replay.DiffRunLogs(a, b)
	fmt.Printf("base: %d events, candidate: %d events\n\n", len(a), len(b))
	fmt.Println(replay.FormatRunDiff(lines))
	if truncated {
		fmt.Println("\nnote: runs exceeded the alignment bound; only the first 2000 events of each run were compared — divergence after that point is NOT shown")
	}
}
