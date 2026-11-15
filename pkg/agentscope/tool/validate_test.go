package tool

import (
	"encoding/json"
	"testing"
)

func TestValidateInput_RequiredFields(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"}
		},
		"required": ["name"]
	}`)

	// Missing required field
	err := ValidateInput(schema, map[string]any{"age": 25.0})
	if err == nil {
		t.Fatal("expected error for missing required field 'name'")
	}

	// Required field present
	err = ValidateInput(schema, map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All fields present
	err = ValidateInput(schema, map[string]any{"name": "Alice", "age": 25.0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateInput_TypeChecks(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"s": {"type": "string"},
			"n": {"type": "number"},
			"i": {"type": "integer"},
			"b": {"type": "boolean"},
			"a": {"type": "array"},
			"o": {"type": "object"}
		}
	}`)

	// All correct types
	err := ValidateInput(schema, map[string]any{
		"s": "hello",
		"n": 3.14,
		"i": 42.0,
		"b": true,
		"a": []any{1, 2, 3},
		"o": map[string]any{"key": "val"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Wrong string type
	err = ValidateInput(schema, map[string]any{"s": 42})
	if err == nil {
		t.Fatal("expected error for wrong string type")
	}

	// Wrong number type
	err = ValidateInput(schema, map[string]any{"n": "not a number"})
	if err == nil {
		t.Fatal("expected error for wrong number type")
	}

	// Wrong boolean type
	err = ValidateInput(schema, map[string]any{"b": "not a bool"})
	if err == nil {
		t.Fatal("expected error for wrong boolean type")
	}

	// Float where integer expected
	err = ValidateInput(schema, map[string]any{"i": 3.5})
	if err == nil {
		t.Fatal("expected error for non-integer float")
	}

	// Integer as float is OK
	err = ValidateInput(schema, map[string]any{"i": 3.0})
	if err != nil {
		t.Fatalf("integer as float should be OK: %v", err)
	}
}

func TestValidateInput_EmptySchema(t *testing.T) {
	// Empty schema should not cause errors
	err := ValidateInput(nil, map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("empty schema should pass: %v", err)
	}

	err = ValidateInput(json.RawMessage(`{}`), map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("empty object schema should pass: %v", err)
	}
}

func TestValidateInput_NilInput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)
	err := ValidateInput(schema, nil)
	if err != nil {
		t.Fatalf("nil input should pass validation: %v", err)
	}
}

func TestValidateInput_UnknownProperties(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	// Extra properties should be OK (not enforcing additionalProperties)
	err := ValidateInput(schema, map[string]any{"name": "Alice", "extra": 42})
	if err != nil {
		t.Fatalf("unknown properties should be allowed: %v", err)
	}
}

func TestValidateInput_InvalidSchema(t *testing.T) {
	// Invalid JSON schema should not cause errors (skip validation)
	err := ValidateInput(json.RawMessage(`{invalid}`), map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("invalid schema should skip validation: %v", err)
	}
}
