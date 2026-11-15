// Command replayview is a terminal viewer for RunJSONL run logs
// (HARNESS_DESIGN G). It steps through the recorded event stream of a
// reply so you can inspect exactly what happened: blocks, tool calls,
// model calls, errors.
//
// Usage:
//
//	replayview <runlog.jsonl>
//
// Commands: n (next), p (previous), g <n> (goto), f (filter toggle model
// calls), q (quit).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: replayview <runlog.jsonl>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	events := replay.ParseRunLog(data)
	if len(events) == 0 {
		fmt.Fprintln(os.Stderr, "no events parsed")
		os.Exit(1)
	}

	fmt.Printf("loaded %d events from %s\n", len(events), os.Args[1])
	fmt.Println("commands: n=next p=prev g<N>=goto f=toggle model_call filter q=quit")

	cur := 0
	showModelCalls := true
	stdin := bufio.NewReader(os.Stdin)
	for {
		show(events, cur, showModelCalls)
		fmt.Print("> ")
		line, err := stdin.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		switch {
		case cmd == "q" || cmd == "quit":
			return
		case cmd == "n" || cmd == "":
			cur = nextVisible(events, cur, 1, showModelCalls)
		case cmd == "p":
			cur = nextVisible(events, cur, -1, showModelCalls)
		case strings.HasPrefix(cmd, "g"):
			if n, err := strconv.Atoi(strings.TrimPrefix(cmd, "g")); err == nil && n >= 0 && n < len(events) {
				cur = n
			}
		case cmd == "f":
			showModelCalls = !showModelCalls
			fmt.Printf("model_call lines: %v\n", showModelCalls)
		}
	}
}

func visible(e replay.RunEvent, showModelCalls bool) bool {
	return showModelCalls || e.Type != "model_call"
}

func nextVisible(events []replay.RunEvent, cur, dir int, showModelCalls bool) int {
	for i := cur + dir; i >= 0 && i < len(events); i += dir {
		if visible(events[i], showModelCalls) {
			return i
		}
	}
	return cur
}

func show(events []replay.RunEvent, i int, showModelCalls bool) {
	e := events[i]
	fmt.Printf("\n[%d/%d] %s\n", i+1, len(events), e.Type)
	var pretty json.RawMessage = e.Data
	if b, err := json.MarshalIndent(pretty, "  ", "  "); err == nil {
		fmt.Println("  " + string(b))
	}
}
