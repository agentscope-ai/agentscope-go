package providercontract

import "testing"

// registeredHarnesses is the single list of providers under contract. New
// providers must be added here; every test in this package that iterates
// providers reads this list so none can drift out of the wall.
func registeredHarnesses() []Harness {
	return []Harness{
		OpenAIHarness(),
		AnthropicHarness(),
		DashScopeHarness(),
		GeminiHarness(),
		DeepSeekHarness(),
		MoonshotHarness(),
		OllamaHarness(),
		XAIHarness(),
	}
}

// TestContractWall runs the full behavior contract wall against every
// registered provider. Regressions in usage accounting, streaming lifecycle,
// error taxonomy, thinking wire formats, or the max-tokens wire key fail the
// build gate (HARNESS_DESIGN B1).
func TestContractWall(t *testing.T) {
	harnesses := registeredHarnesses()
	for i := range harnesses {
		h := &harnesses[i]
		t.Run(h.Name, func(t *testing.T) {
			Run(t, h)
		})
	}
}
