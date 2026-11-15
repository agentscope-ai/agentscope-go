package model

import (
	"encoding/json"
	"testing"
)

func sanitizeMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	out := SanitizeSchemaForGemini(json.RawMessage(raw))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal sanitized: %v (%s)", err, out)
	}
	return m
}

// Upstream #2437: nullable type arrays from Pydantic/OpenAPI 3.1 schemas.
func TestGeminiSanitizeNullableTypeArray(t *testing.T) {
	m := sanitizeMap(t, `{"type":["string","null"],"description":"d"}`)
	if m["type"] != "string" {
		t.Errorf(`["string","null"] should simplify to "string", got %v`, m["type"])
	}
	if m["description"] != "d" {
		t.Error("sibling keys must survive")
	}
}

func TestGeminiSanitizeMultiTypeArray(t *testing.T) {
	m := sanitizeMap(t, `{"type":["string","number"]}`)
	if _, has := m["type"]; has {
		t.Errorf("multi-type array should move to anyOf, type still present: %v", m["type"])
	}
	anyOf, ok := m["anyOf"].([]any)
	if !ok || len(anyOf) != 2 {
		t.Fatalf("anyOf = %v, want 2 entries", m["anyOf"])
	}
	first := anyOf[0].(map[string]any)
	if first["type"] != "string" {
		t.Errorf("anyOf[0] = %v, want string", first)
	}
}

func TestGeminiSanitizeNullOnlyArray(t *testing.T) {
	m := sanitizeMap(t, `{"type":["null"]}`)
	if m["type"] != "object" {
		t.Errorf(`["null"] should become "object", got %v`, m["type"])
	}
}

func TestGeminiSanitizeStringNullRegression(t *testing.T) {
	m := sanitizeMap(t, `{"type":"null"}`)
	if m["type"] != "object" {
		t.Errorf(`"null" should become "object", got %v`, m["type"])
	}
}

func TestGeminiSanitizeAnyOfNullArrayEntry(t *testing.T) {
	// anyOf entries shaped {"type":["null"]} must also be filtered.
	m := sanitizeMap(t, `{"anyOf":[{"type":"string"},{"type":["null"]}]}`)
	if _, has := m["anyOf"]; has {
		t.Errorf("single surviving anyOf branch should be inlined, got %v", m["anyOf"])
	}
	if m["type"] != "string" {
		t.Errorf("type = %v, want string", m["type"])
	}
}

func TestGeminiSanitizeMultiTypeWithExistingAnyOfUnchanged(t *testing.T) {
	// Python raises rather than dropping the conjunctive constraint; the Go
	// sanitizer leaves the node unchanged so Gemini rejects it loudly.
	raw := `{"type":["string","number"],"anyOf":[{"minLength":2}]}`
	m := sanitizeMap(t, raw)
	arr, ok := m["type"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("type array should be left unchanged, got %v", m["type"])
	}
}

func TestGeminiSanitizeNestedPropertyNullable(t *testing.T) {
	m := sanitizeMap(t, `{"type":"object","properties":{"p":{"type":["integer","null"]}}}`)
	props := m["properties"].(map[string]any)
	p := props["p"].(map[string]any)
	if p["type"] != "integer" {
		t.Errorf("nested nullable array not sanitized: %v", p["type"])
	}
}
