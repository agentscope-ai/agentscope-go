package model

import (
	"encoding/json"
	"testing"
)

// Cards' parameter_overrides must parse and reach the card JSON (review note on #154).
func numEq(v any, want float64) bool {
	switch n := v.(type) {
	case float64:
		return n == want
	case int:
		return float64(n) == want
	case int64:
		return float64(n) == want
	}
	return false
}

func TestModelCardParameterOverridesJSON(t *testing.T) {
	card, err := GetModelCard("claude-opus-4-6")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	overrides, ok := m["parameter_overrides"].(map[string]any)
	if !ok {
		t.Fatalf("parameter_overrides missing from card JSON: %s", raw)
	}
	mt, ok := overrides["max_tokens"].(map[string]any)
	if !ok || !numEq(mt["maximum"], 128000) {
		t.Fatalf("max_tokens override = %v", overrides["max_tokens"])
	}
	te, ok := overrides["thinking_enable"].(map[string]any)
	if !ok || te["hidden"] != true {
		t.Fatalf("thinking_enable override = %v", overrides["thinking_enable"])
	}
	tm, ok := overrides["thinking_mode"].(map[string]any)
	if !ok || tm["default"] != "adaptive" || tm["anyOf"] == nil {
		t.Fatalf("thinking_mode override = %v", overrides["thinking_mode"])
	}
}

func TestModelCardParameterOverridesField(t *testing.T) {
	card, err := GetModelCard("claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if card.ParameterOverrides == nil {
		t.Fatal("ParameterOverrides not populated")
	}
	mt, ok := card.ParameterOverrides["max_tokens"].(map[string]any)
	if !ok || !numEq(mt["maximum"], 65536) {
		t.Fatalf("max_tokens override = %v", card.ParameterOverrides["max_tokens"])
	}
	for _, hidden := range []string{"thinking_mode", "thinking_display", "reasoning_effort"} {
		ov, ok := card.ParameterOverrides[hidden].(map[string]any)
		if !ok || ov["hidden"] != true {
			t.Fatalf("%s override = %v, want hidden", hidden, card.ParameterOverrides[hidden])
		}
	}
}
