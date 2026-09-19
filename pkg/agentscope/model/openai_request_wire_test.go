package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// agentscope-go#8: openAIChatRequest.MaxTokens had no json tag, so every
// adapter that assigns it (Ollama, DashScope, DeepSeek, Moonshot, xAI)
// serialized the limit as "MaxTokens" — a key no server reads — and
// WithMaxTokens was a silent no-op. Unset, the field leaked as
// "MaxTokens":null on every request, including OpenAI's.
func TestOpenAIChatRequest_MaxTokensWireKey(t *testing.T) {
	n := 77
	withLimit, err := json.Marshal(openAIChatRequest{Model: "m", MaxTokens: &n})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withLimit), `"max_tokens":77`) {
		t.Errorf("max_tokens missing from request: %s", withLimit)
	}
	if strings.Contains(string(withLimit), "MaxTokens") {
		t.Errorf("Go field name leaked onto the wire: %s", withLimit)
	}

	unset, err := json.Marshal(openAIChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unset), "max_tokens") || strings.Contains(string(unset), "MaxTokens") {
		t.Errorf("unset limit must be omitted entirely, got: %s", unset)
	}
}
