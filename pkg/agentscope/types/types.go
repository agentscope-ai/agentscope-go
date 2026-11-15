package types

// JSONValue represents any JSON-serializable value.
type JSONValue interface{}

// JSONObject is a generic JSON object.
type JSONObject map[string]JSONValue
