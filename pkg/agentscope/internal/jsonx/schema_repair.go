package jsonx

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// RepairWithSchema repairs malformed tool-call JSON and coerces values
// toward the tool's declared input schema (upstream #2496). Python leans on
// the json_repair[schema] library; Go stays zero-dependency: syntax repair
// comes from RepairAndUnmarshal, then a schema walk fixes the classic LLM
// type slips — numbers quoted as strings, booleans as "true", a lone value
// where an array belongs, an object serialized into a string.
//
// Coercion never drops keys: every input key survives in the output (the
// upstream library explicitly rejects key-losing repairs, and so do we).
// An empty or unparsable schema degrades to plain syntax repair.
func RepairWithSchema(data []byte, schema json.RawMessage, out *map[string]any) error {
	if err := RepairAndUnmarshal(data, out); err != nil {
		return err
	}
	if len(schema) == 0 || out == nil || *out == nil {
		return nil
	}
	var sch map[string]any
	if err := json.Unmarshal(schema, &sch); err != nil {
		return nil // no usable schema: syntax repair only
	}
	*out = coerceObject(*out, sch)
	return nil
}

// coerceObject coerces declared properties toward the schema and returns the
// result. It is copy-on-write: the map passed in is returned unchanged when
// nothing needed coercing, and a shallow copy is made the first time a value
// actually changes. Toolkit.CallTool receives its argument map from an
// external caller, so rewriting it in place would mutate data the caller still
// owns.
func coerceObject(obj map[string]any, schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return obj
	}
	var out map[string]any
	for key, val := range obj {
		ps, ok := props[key].(map[string]any)
		if !ok {
			// Undeclared keys pass through untouched for ValidateInput to
			// accept or reject; coercion never adds or drops keys.
			continue
		}
		nv := coerceValue(val, ps)
		if reflect.DeepEqual(nv, val) {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(obj))
			for k, v := range obj {
				out[k] = v
			}
		}
		out[key] = nv
	}
	if out == nil {
		return obj
	}
	return out
}

func coerceValue(val any, schema map[string]any) any {
	typ, _ := schema["type"].(string)
	switch typ {
	case "string":
		switch v := val.(type) {
		case string, nil:
			return val
		case float64:
			if v == math.Trunc(v) && !math.IsInf(v, 0) {
				return strconv.FormatInt(int64(v), 10)
			}
			return strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(v)
		default:
			if b, err := json.Marshal(val); err == nil {
				return string(b)
			}
			return val
		}
	case "number":
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return val // validation will reject it loudly
			}
			return v
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				// ParseFloat also accepts "Inf", "NaN" and overflowing
				// literals such as "1e999". None of those can be marshaled
				// back to JSON, and the float64 branch above already refuses
				// them; the string path must be consistent or a quoted
				// "NaN" would slip through validation and blow up later.
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return val
				}
				return f
			}
			return val
		default:
			return val
		}
	case "integer":
		switch v := val.(type) {
		case float64:
			return v
		case string:
			s := strings.TrimSpace(v)
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return float64(i)
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil &&
				f == math.Trunc(f) && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f
			}
			return val
		default:
			return val
		}
	case "boolean":
		switch v := val.(type) {
		case bool:
			return v
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "true":
				return true
			case "false":
				return false
			}
			return val
		default:
			return val
		}
	case "array":
		arr, isSlice := val.([]any)
		if !isSlice {
			if val == nil {
				return nil
			}
			// A TYPED slice is already an array: the assertion above only
			// matches []any, but ValidateInput's checkType accepts []string,
			// []float64 and []int as arrays, and Go callers pass those
			// directly. Wrapping one would turn a list of N items into a
			// one-item list containing the list — silent corruption on a
			// public API. Leave it for validation, which already accepts it.
			switch reflect.ValueOf(val).Kind() {
			case reflect.Slice, reflect.Array:
				return val
			}
			// A genuinely lone value where an array belongs: wrap it. The
			// wrapper is freshly allocated, so it is private to this call.
			arr = []any{val}
		}
		is, hasItems := schema["items"].(map[string]any)
		if !hasItems {
			return arr
		}
		// Copy-on-write: the caller's slice must not be rewritten in place.
		var out []any
		for i, item := range arr {
			nv := coerceValue(item, is)
			if reflect.DeepEqual(nv, item) {
				continue
			}
			if out == nil {
				out = make([]any, len(arr))
				copy(out, arr)
			}
			out[i] = nv
		}
		if out == nil {
			return arr
		}
		return out
	case "object":
		switch v := val.(type) {
		case map[string]any:
			return coerceObject(v, schema)
		case string:
			var m map[string]any
			if json.Unmarshal([]byte(v), &m) == nil {
				// m is freshly parsed here, so it is already private.
				return coerceObject(m, schema)
			}
			return val
		default:
			return val
		}
	}
	return val
}

// CoerceToSchema applies the schema-guided type coercion to an already
// parsed argument map (upstream #2496 applies repair+coercion on EVERY
// tool call, not only when JSON parsing fails — models routinely quote
// numbers or stringify booleans in otherwise-valid JSON).
//
// It returns the coerced map and never writes to the one passed in: callers
// must use the return value. The input map is returned as-is when nothing
// needed coercing, so the common case does not allocate.
func CoerceToSchema(obj map[string]any, schema json.RawMessage) map[string]any {
	if obj == nil || len(schema) == 0 {
		return obj
	}
	var sch map[string]any
	if err := json.Unmarshal(schema, &sch); err != nil {
		return obj
	}
	return coerceObject(obj, sch)
}
