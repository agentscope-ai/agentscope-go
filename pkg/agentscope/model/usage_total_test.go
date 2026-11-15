package model

import "testing"

func TestEffectiveInputTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage ChatUsage
		want  int
	}{
		{"zero", ChatUsage{}, 0},
		{"input only", ChatUsage{InputTokens: 100}, 100},
		{"anthropic-style total", ChatUsage{InputTokens: 50, CacheCreationInputTokens: 300, CacheInputTokens: 700, TotalInputTokens: 1050}, 1050},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.EffectiveInputTokens(); got != tc.want {
				t.Errorf("EffectiveInputTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
