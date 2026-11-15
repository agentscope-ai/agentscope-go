package metrics

import "testing"

// TestInMemoryCounter_LabelAware proves labeled increments are tracked per series
// (previously all labels were ignored and collapsed into one total).
func TestInMemoryCounter_LabelAware(t *testing.T) {
	c := &InMemoryCounter{}
	c.Inc("openai")
	c.Inc("openai")
	c.Inc("anthropic")

	if got := c.ValueFor("openai"); got != 2 {
		t.Errorf("openai series = %v, want 2", got)
	}
	if got := c.ValueFor("anthropic"); got != 1 {
		t.Errorf("anthropic series = %v, want 1", got)
	}
	if got := c.Value(); got != 3 {
		t.Errorf("total = %v, want 3", got)
	}
	if got := c.ValueFor("gemini"); got != 0 {
		t.Errorf("absent series = %v, want 0", got)
	}
}
