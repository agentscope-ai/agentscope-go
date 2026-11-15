package tool

import (
	"encoding/json"
	"testing"
)

func TestValidateInput_Enum(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"op":{"type":"string","enum":["read","write"]}},
		"required":["op"]
	}`)
	if err := ValidateInput(schema, map[string]any{"op": "read"}); err != nil {
		t.Errorf("valid enum value rejected: %v", err)
	}
	if err := ValidateInput(schema, map[string]any{"op": "delete"}); err == nil {
		t.Error("out-of-enum value accepted; expected rejection")
	}
}

func TestValidateInput_StringLengthAndPattern(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"name":{"type":"string","minLength":2,"maxLength":5,"pattern":"^[a-z]+$"}}
	}`)
	if err := ValidateInput(schema, map[string]any{"name": "abc"}); err != nil {
		t.Errorf("valid string rejected: %v", err)
	}
	if err := ValidateInput(schema, map[string]any{"name": "a"}); err == nil {
		t.Error("too-short string accepted")
	}
	if err := ValidateInput(schema, map[string]any{"name": "abcdef"}); err == nil {
		t.Error("too-long string accepted")
	}
	if err := ValidateInput(schema, map[string]any{"name": "AB1"}); err == nil {
		t.Error("pattern-violating string accepted")
	}
}

func TestValidateInput_NumberBounds(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"n":{"type":"number","minimum":1,"maximum":10}}
	}`)
	if err := ValidateInput(schema, map[string]any{"n": 5.0}); err != nil {
		t.Errorf("in-range number rejected: %v", err)
	}
	if err := ValidateInput(schema, map[string]any{"n": 0.0}); err == nil {
		t.Error("below-minimum number accepted")
	}
	if err := ValidateInput(schema, map[string]any{"n": 99.0}); err == nil {
		t.Error("above-maximum number accepted")
	}
}

func TestValidateInput_NestedAndArray(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"cfg":{"type":"object","properties":{"mode":{"type":"string","enum":["x","y"]}},"required":["mode"]},
			"tags":{"type":"array","items":{"type":"string","enum":["a","b"]}}
		}
	}`)
	if err := ValidateInput(schema, map[string]any{"cfg": map[string]any{"mode": "x"}, "tags": []any{"a", "b"}}); err != nil {
		t.Errorf("valid nested rejected: %v", err)
	}
	if err := ValidateInput(schema, map[string]any{"cfg": map[string]any{"mode": "z"}}); err == nil {
		t.Error("invalid nested enum accepted")
	}
	if err := ValidateInput(schema, map[string]any{"cfg": map[string]any{}}); err == nil {
		t.Error("missing nested required field accepted")
	}
	if err := ValidateInput(schema, map[string]any{"cfg": map[string]any{"mode": "x"}, "tags": []any{"a", "c"}}); err == nil {
		t.Error("invalid array item enum accepted")
	}
}

func TestValidateInput_BackwardCompat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"p":{"type":"string"}},"required":["p"]}`)
	if err := ValidateInput(schema, map[string]any{}); err == nil {
		t.Error("missing required still must fail")
	}
	if err := ValidateInput(schema, map[string]any{"p": 5}); err == nil {
		t.Error("wrong type still must fail")
	}
	if err := ValidateInput(schema, map[string]any{"p": "ok"}); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
}
