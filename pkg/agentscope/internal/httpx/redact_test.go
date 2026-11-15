package httpx

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/v1?key=SECRET123":              "key",
		"https://x.com/a?api_key=abc&token=def":                 "api_key",
		"https://generativelanguage.googleapis.com/m?key=AIzaX": "key",
	}
	for in := range cases {
		out := redactURL(in)
		for _, secret := range []string{"SECRET123", "abc", "def", "AIzaX"} {
			if strings.Contains(out, secret) {
				t.Errorf("redactURL(%q) leaked secret %q: %s", in, secret, out)
			}
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("redactURL(%q) did not mark redaction: %s", in, out)
		}
	}

	// URLs without sensitive params are returned essentially unchanged (path kept).
	plain := "https://api.example.com/v1/models"
	if redactURL(plain) != plain {
		t.Errorf("redactURL altered a plain URL: %q -> %q", plain, redactURL(plain))
	}
}
