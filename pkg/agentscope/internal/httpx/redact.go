package httpx

import "net/url"

// sensitiveQueryParams are query-parameter names whose values must never be
// logged (API keys, tokens, credentials embedded in URLs).
var sensitiveQueryParams = map[string]bool{
	"key":          true,
	"api_key":      true,
	"apikey":       true,
	"access_token": true,
	"token":        true,
	"password":     true,
	"secret":       true,
	"sig":          true,
	"signature":    true,
}

// redactURL returns u with the values of any sensitive query parameters masked,
// for safe logging. A URL that does not parse or has no sensitive params is
// returned unchanged.
func redactURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := parsed.Query()
	changed := false
	for name := range q {
		if sensitiveQueryParams[toLowerASCII(name)] {
			q.Set(name, "REDACTED")
			changed = true
		}
	}
	if !changed {
		return u
	}
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
