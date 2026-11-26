package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func runOnce(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestOfflineSession(t *testing.T) {
	out := runOnce(t)
	for _, want := range []string{
		"  #2  owner_change coordinator none -> cloud (router)",
		"  #3  reply        cloud       Plan: stay at Hotel Aurora (rated 9.1) and visit Belem on Saturday.",
		"  #6  owner_change cloud       cloud -> edge (transfer)",
		"  #7  summary      cloud       for edge, through #6:",
		"  #8  reply        edge        The living room lamp is on.",
		"  #9  user_input   user        Set the bedroom thermostat to 21 degrees.",
		"  #10 reply        edge        Done: the bedroom thermostat is set to 21 degrees.",
		"  #12 assign       edge        task t-1 to cloud: Look up the Lisbon Oceanarium opening hours.",
		"  #14 report       cloud       task t-1 to edge: The Oceanarium opens at 10:00 every day.",
		"  #15 reply        edge        The cloud says the Oceanarium opens at 10:00 every day.",
		"Owner: edge (epoch 2)",
		"  router       calls 2 (unknown usage 2), input 0, output 0 tokens",
		"  fork_peer    calls 1 (unknown usage 0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The researcher's answer reaches the master, never the log.
	if strings.Contains(out, "Hotel Tejo has 7.4") {
		t.Errorf("the member's text became an entry:\n%s", out)
	}
	if strings.Contains(out, "#16 ") {
		t.Errorf("the session wrote more entries than scripted:\n%s", out)
	}
}

func TestOfflineSessionIsDeterministic(t *testing.T) {
	first, second := runOnce(t), runOnce(t)
	if first != second {
		t.Fatalf("two runs differ:\n%s\n---\n%s", first, second)
	}
}
