package memory

import (
	"strings"
	"testing"
)

// Upstream #2513: the default instructions carried a malformed </search>
// closing tag that leaked into every system prompt using agentic memory.
func TestDefaultInstructionsHaveNoStraySearchTag(t *testing.T) {
	if strings.Contains(DefaultAgenticMemoryInstructions, "</search>") {
		t.Error("default instructions still contain the malformed </search> tag")
	}
	want := "glob=\"*.md\"\n# or Bash command:"
	if !strings.Contains(DefaultAgenticMemoryInstructions, want) {
		t.Errorf("expected the fixed grep snippet %q in instructions", want)
	}
}
