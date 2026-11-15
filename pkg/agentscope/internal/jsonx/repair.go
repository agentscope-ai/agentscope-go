package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*\n?(.*?)\\s*```")

// RepairAndUnmarshal tries json.Unmarshal first; on failure it attempts
// several heuristic repairs (strip markdown fences, extract object/array,
// fix single quotes, remove trailing commas, close brackets) before
// re-trying.  Only returns an error when every strategy fails.
func RepairAndUnmarshal(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err == nil {
		return nil
	}

	s := string(data)

	for _, attempt := range repairStrategies(s) {
		if json.Unmarshal([]byte(attempt), v) == nil {
			return nil
		}
	}

	return fmt.Errorf("jsonx: unable to parse or repair JSON: %.200s", s)
}

func repairStrategies(s string) []string {
	var candidates []string

	// 1. Strip markdown code fence
	if m := fenceRe.FindStringSubmatch(s); len(m) > 1 {
		candidates = append(candidates, strings.TrimSpace(m[1]))
	}

	// 2. Extract first {...} or [...]
	if extracted := extractBrackets(s, '{', '}'); extracted != "" {
		candidates = append(candidates, extracted)
	}
	if extracted := extractBrackets(s, '[', ']'); extracted != "" {
		candidates = append(candidates, extracted)
	}

	// 3. Apply fixes on original and all candidates so far
	snapshot := make([]string, len(candidates))
	copy(snapshot, candidates)
	snapshot = append(snapshot, s)

	for _, base := range snapshot {
		fixed := applyFixes(base)
		if fixed != base {
			candidates = append(candidates, fixed)
		}
	}

	return candidates
}

func applyFixes(s string) string {
	// single quotes → double quotes (only outside already-doubled strings)
	s = fixSingleQuotes(s)
	// trailing commas
	s = removeTrailingCommas(s)
	// close unclosed brackets
	s = closeBrackets(s)
	return s
}

func removeTrailingCommas(s string) string {
	var buf strings.Builder
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			buf.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
		} else if c == ',' {
			j := i + 1
			// Match the ASCII whitespace accepted by the previous regexp's \s.
			for j < len(s) && strings.ContainsRune(" \t\n\r\f", rune(s[j])) {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				i = j - 1
				continue
			}
		}
		buf.WriteByte(c)
	}
	return buf.String()
}

func extractBrackets(s string, open, close byte) string {
	start := strings.IndexByte(s, open)
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	// unclosed — return from start to end (closeBrackets may fix it)
	if depth > 0 {
		return s[start:]
	}
	return ""
}

// fixSingleQuotes replaces single-quoted keys/values with double-quoted
// ones. It only swaps quotes that look like JSON string delimiters.
func fixSingleQuotes(s string) string {
	if !strings.ContainsRune(s, '\'') {
		return s
	}
	var buf bytes.Buffer
	inDouble := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			buf.WriteByte(c)
			continue
		}
		if c == '\\' {
			esc = true
			buf.WriteByte(c)
			continue
		}
		if c == '"' {
			inDouble = !inDouble
			buf.WriteByte(c)
			continue
		}
		if c == '\'' && !inDouble {
			buf.WriteByte('"')
			continue
		}
		buf.WriteByte(c)
	}
	return buf.String()
}

func closeBrackets(s string) string {
	opens := 0
	arrs := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			opens++
		case '}':
			opens--
		case '[':
			arrs++
		case ']':
			arrs--
		}
	}
	result := s
	for arrs > 0 {
		result += "]"
		arrs--
	}
	for opens > 0 {
		result += "}"
		opens--
	}
	return result
}
