package jsonx

import (
	"encoding/json"
	"testing"
)

func mustSchema(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestRepairWithSchemaCoercesTypes(t *testing.T) {
	schema := mustSchema(t, `{
		"type":"object",
		"properties":{
			"limit":{"type":"integer"},
			"ratio":{"type":"number"},
			"flag":{"type":"boolean"},
			"name":{"type":"string"},
			"tags":{"type":"array","items":{"type":"string"}},
			"meta":{"type":"object","properties":{"k":{"type":"integer"}}}
		}
	}`)

	var out map[string]any
	raw := []byte(`{"limit":"5","ratio":"0.25","flag":"true","name":42,"tags":"solo","meta":"{\"k\":\"7\"}"}`)
	if err := RepairWithSchema(raw, schema, &out); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if v, ok := out["limit"].(float64); !ok || v != 5 {
		t.Errorf("limit = %v (%T), want 5", out["limit"], out["limit"])
	}
	if v, ok := out["ratio"].(float64); !ok || v != 0.25 {
		t.Errorf("ratio = %v, want 0.25", out["ratio"])
	}
	if v, ok := out["flag"].(bool); !ok || !v {
		t.Errorf("flag = %v, want true", out["flag"])
	}
	if v, ok := out["name"].(string); !ok || v != "42" {
		t.Errorf("name = %v, want \"42\"", out["name"])
	}
	arr, ok := out["tags"].([]any)
	if !ok || len(arr) != 1 || arr[0] != "solo" {
		t.Errorf("tags = %v, want [solo] (lone value wrapped)", out["tags"])
	}
	meta, ok := out["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %T, want object parsed from string", out["meta"])
	}
	if v, ok := meta["k"].(float64); !ok || v != 7 {
		t.Errorf("meta.k = %v, want 7", meta["k"])
	}
}

func TestRepairWithSchemaSyntaxPlusCoercion(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"offset":{"type":"integer"}}}`)
	var out map[string]any
	// Trailing comma (syntax) + quoted integer (type).
	if err := RepairWithSchema([]byte(`{"offset":"3",}`), schema, &out); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if v, _ := out["offset"].(float64); v != 3 {
		t.Errorf("offset = %v, want 3", out["offset"])
	}
}

func TestRepairWithSchemaKeepsAllKeys(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"a":{"type":"integer"}}}`)
	var out map[string]any
	if err := RepairWithSchema([]byte(`{"a":"1","b":"untouched","c":null}`), schema, &out); err != nil {
		t.Fatalf("repair: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := out[k]; !ok {
			t.Errorf("key %q lost — coercion must never drop keys", k)
		}
	}
}

func TestRepairWithSchemaUncoercibleLeftAlone(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"n":{"type":"integer"}}}`)
	var out map[string]any
	if err := RepairWithSchema([]byte(`{"n":"not-a-number"}`), schema, &out); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if v, ok := out["n"].(string); !ok || v != "not-a-number" {
		t.Errorf("uncoercible value should stay as-is, got %v", out["n"])
	}
}

func TestRepairWithSchemaEmptySchemaDegrades(t *testing.T) {
	var out map[string]any
	if err := RepairWithSchema([]byte(`{"a":1,}`), nil, &out); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if out["a"] == nil {
		t.Error("plain syntax repair should still work")
	}
}
