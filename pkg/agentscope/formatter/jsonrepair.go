package formatter

import (
	"encoding/json"
	"strings"
)

// jsonLoadsWithRepair attempts to parse a JSON string. If it fails, it tries
// a best-effort repair (stripping trailing commas, closing unclosed braces)
// before falling back to an empty object. This prevents crashes from
// truncated tool-call inputs caused by interrupted streaming or context
// compression.
func jsonLoadsWithRepair(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}

	// First try direct parse
	var result any
	if err := json.Unmarshal([]byte(s), &result); err == nil {
		return result
	}

	// Best-effort repair: close unclosed braces/brackets
	repaired := repairJSON(s)
	if repaired != s {
		if err := json.Unmarshal([]byte(repaired), &result); err == nil {
			return result
		}
	}

	// Fallback: empty object
	return map[string]any{}
}

// repairJSON does simple structural repair on truncated JSON:
// - Counts open { [ vs close } ] and appends missing closers
// - Strips trailing commas before closers
func repairJSON(s string) string {
	var openBraces, openBrackets int
	inString := false
	escaped := false

	for _, c := range s {
		if escaped {
			escaped = false
			continue
		}
		if c == 92 { // backslash
			escaped = true
			continue
		}
		if c == 34 { // double quote
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case 123: // {
			openBraces++
		case 125: // }
			openBraces--
		case 91: // [
			openBrackets++
		case 93: // ]
			openBrackets--
		}
	}

	// Strip trailing comma and whitespace
	s = strings.TrimRight(s, " \t\n\r")
	s = strings.TrimSuffix(s, ",")
	s = strings.TrimRight(s, " \t\n\r")

	// Append missing closers
	for i := 0; i < openBrackets; i++ {
		s += "]"
	}
	for i := 0; i < openBraces; i++ {
		s += "}"
	}

	return s
}
