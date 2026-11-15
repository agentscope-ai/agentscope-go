package jsonx

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

var puritySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"count":   {"type": "integer"},
		"ratio":   {"type": "number"},
		"enabled": {"type": "boolean"},
		"tags":    {"type": "array", "items": {"type": "integer"}},
		"nested":  {"type": "object", "properties": {"n": {"type": "integer"}}},
		"free":    {"type": "string"}
	}
}`)

// CoerceToSchema runs on EVERY tool call with the caller's own argument map.
// Toolkit.CallTool receives that map from an external caller, so rewriting it in
// place mutated data the caller still owned (and could still be logging,
// retrying, or forwarding).
func TestCoerceToSchemaDoesNotMutateCallerMap(t *testing.T) {
	original := map[string]any{
		"count":   "5",
		"ratio":   "0.5",
		"enabled": "true",
		"tags":    []any{"1", "2"},
		"nested":  map[string]any{"n": "7"},
		"free":    "text",
	}
	snapshot := map[string]any{
		"count":   "5",
		"ratio":   "0.5",
		"enabled": "true",
		"tags":    []any{"1", "2"},
		"nested":  map[string]any{"n": "7"},
		"free":    "text",
	}

	got := CoerceToSchema(original, puritySchema)

	if !reflect.DeepEqual(original, snapshot) {
		t.Errorf("the caller's map was rewritten:\n got %#v\nwant %#v", original, snapshot)
	}
	// Nested structures must not be rewritten either: a shallow copy at the
	// top level would still alias the inner map and slice.
	if nested, ok := original["nested"].(map[string]any); !ok || nested["n"] != "7" {
		t.Errorf("nested map was mutated: %#v", original["nested"])
	}
	if tags, ok := original["tags"].([]any); !ok || tags[0] != "1" {
		t.Errorf("nested slice was mutated: %#v", original["tags"])
	}

	// The returned map must actually be coerced.
	if got["count"] != float64(5) {
		t.Errorf("count = %#v, want float64(5)", got["count"])
	}
	if got["ratio"] != 0.5 {
		t.Errorf("ratio = %#v, want 0.5", got["ratio"])
	}
	if got["enabled"] != true {
		t.Errorf("enabled = %#v, want true", got["enabled"])
	}
	if !reflect.DeepEqual(got["tags"], []any{float64(1), float64(2)}) {
		t.Errorf("tags = %#v, want [1 2] as numbers", got["tags"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["n"] != float64(7) {
		t.Errorf("nested = %#v, want n coerced to 7", got["nested"])
	}
	if got["free"] != "text" {
		t.Errorf("free = %#v, want unchanged", got["free"])
	}
}

// When nothing needs coercing the input map is returned unchanged (no copy, no
// allocation), which is the overwhelmingly common case.
func TestCoerceToSchemaReturnsSameMapWhenNothingChanges(t *testing.T) {
	in := map[string]any{"count": float64(5), "free": "text"}
	got := CoerceToSchema(in, puritySchema)
	if !sameMap(got, in) {
		t.Error("an already-conforming map should be returned as-is")
	}
}

func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// Keys the schema does not declare must survive untouched: coercion is a type
// fix, not a filter, and dropping keys is exactly what the upstream repair
// library refuses to do.
func TestCoerceToSchemaKeepsUndeclaredKeys(t *testing.T) {
	in := map[string]any{"undeclared": "keep me", "count": "1"}
	got := CoerceToSchema(in, puritySchema)
	if got["undeclared"] != "keep me" {
		t.Errorf("undeclared key = %#v, want it passed through", got["undeclared"])
	}
	if len(got) != 2 {
		t.Errorf("got %d keys, want 2 (no key may be dropped)", len(got))
	}
}

// strconv.ParseFloat happily accepts "Inf", "NaN" and overflowing literals such
// as "1e999". Those cannot be marshaled back to JSON, and the float64 branch
// already refuses them — the string branch must be consistent, or a quoted
// "NaN" slips through validation and blows up at the next json.Marshal.
func TestCoerceToSchemaRejectsNonFiniteNumbers(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf", "1e999", "-1e999", "nan", "inf"} {
		for _, typ := range []string{"number", "integer"} {
			schema := json.RawMessage(`{"type":"object","properties":{"v":{"type":"` + typ + `"}}}`)
			in := map[string]any{"v": bad}
			got := CoerceToSchema(in, schema)
			if f, ok := got["v"].(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
				t.Errorf("%s as %s coerced to non-finite %v", bad, typ, f)
			}
			// The value must survive unchanged so validation rejects it
			// loudly instead of a later marshal failing.
			if got["v"] != bad {
				t.Errorf("%s as %s became %#v, want it left for validation", bad, typ, got["v"])
			}
		}
	}
	// A genuinely numeric string still coerces.
	schema := json.RawMessage(`{"type":"object","properties":{"v":{"type":"number"}}}`)
	if got := CoerceToSchema(map[string]any{"v": "1.5"}, schema); got["v"] != 1.5 {
		t.Errorf("v = %#v, want 1.5", got["v"])
	}
}

// RepairWithSchema owns the map it just parsed, so it may coerce in place; the
// result still has to be correct.
func TestRepairWithSchemaCoercesParsedOutput(t *testing.T) {
	var out map[string]any
	if err := RepairWithSchema([]byte(`{"count":"3","tags":["4"]}`), puritySchema, &out); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if out["count"] != float64(3) {
		t.Errorf("count = %#v, want 3", out["count"])
	}
	if !reflect.DeepEqual(out["tags"], []any{float64(4)}) {
		t.Errorf("tags = %#v, want [4]", out["tags"])
	}
}

// A lone value where an array belongs is wrapped, and the wrapper must be a
// fresh slice (never the caller's, if the caller somehow passed one).
func TestCoerceToSchemaWrapsLoneValueIntoArray(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"tags":{"type":"array","items":{"type":"integer"}}}}`)
	got := CoerceToSchema(map[string]any{"tags": "9"}, schema)
	if !reflect.DeepEqual(got["tags"], []any{float64(9)}) {
		t.Errorf("tags = %#v, want [9]", got["tags"])
	}
}

// An array that needs no coercion must not be copied, so the common path stays
// allocation-free.
func TestCoerceToSchemaLeavesConformingArrayAlone(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"tags":{"type":"array","items":{"type":"integer"}}}}`)
	in := map[string]any{"tags": []any{float64(1), float64(2)}}
	got := CoerceToSchema(in, schema)
	if !reflect.DeepEqual(got["tags"], []any{float64(1), float64(2)}) {
		t.Errorf("tags = %#v", got["tags"])
	}
	if !reflect.DeepEqual(in, map[string]any{"tags": []any{float64(1), float64(2)}}) {
		t.Errorf("input mutated: %#v", in)
	}
}

// ValidateInput's checkType accepts []string, []float64 and []int as arrays, and
// Go callers pass those directly. The array branch only type-asserts []any, so
// without a guard a typed slice was wrapped into a ONE-element list containing
// the list — silent corruption of a legitimate value on a public API.
func TestCoerceToSchemaPreservesTypedSlices(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {"tags": {"type": "array", "items": {"type": "string"}}}
	}`)

	cases := []struct {
		name string
		in   any
	}{
		{"[]string", []string{"a", "b"}},
		{"[]int", []int{1, 2}},
		{"[]float64", []float64{1.5, 2.5}},
		{"array", [2]string{"x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CoerceToSchema(map[string]any{"tags": tc.in}, schema)
			if !reflect.DeepEqual(got["tags"], tc.in) {
				t.Errorf("tags = %#v, want it preserved as %#v", got["tags"], tc.in)
			}
			// Specifically: it must not have become a one-element wrapper.
			if list, ok := got["tags"].([]any); ok && len(list) == 1 {
				t.Errorf("typed slice was wrapped into a single-element []any: %#v", list)
			}
		})
	}

	// An untyped []any still gets per-element coercion.
	got := CoerceToSchema(map[string]any{"tags": []any{"a", "b"}}, schema)
	if !reflect.DeepEqual(got["tags"], []any{"a", "b"}) {
		t.Errorf("[]any tags = %#v", got["tags"])
	}

	// A genuinely lone value is still wrapped, which is the point of the branch.
	loneSchema := json.RawMessage(`{
		"type": "object",
		"properties": {"n": {"type": "array", "items": {"type": "integer"}}}
	}`)
	got = CoerceToSchema(map[string]any{"n": "7"}, loneSchema)
	if !reflect.DeepEqual(got["n"], []any{float64(7)}) {
		t.Errorf("lone value = %#v, want [7] wrapped and coerced", got["n"])
	}
}
