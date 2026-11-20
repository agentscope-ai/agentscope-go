package workspace

import (
	"os/exec"
	"testing"
)

// TestShellQuoteNeutralizesShellInjection executes the quoted payload in a
// real shell and asserts it round-trips literally. The container file ops
// (Docker/Apple) build `sh -c` commands with shellQuote; %q would leave $,
// backticks and quote-breakout active inside shell double quotes.
func TestShellQuoteNeutralizesShellInjection(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	for _, payload := range []string{
		"$(echo injected)",
		"`echo injected`",
		`"; echo injected; "`,
		"$HOME",
		"a b;c|d&e",
		"it's a $(test)",
		"/tmp/$(touch pwned)/file",
	} {
		cmd := exec.Command("sh", "-c", "printf '%s' "+shellQuote(payload))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("shellQuote(%q) exec: %v", payload, err)
		}
		if string(out) != payload {
			t.Errorf("shellQuote(%q) did not round-trip literally: got %q", payload, out)
		}
	}
}
