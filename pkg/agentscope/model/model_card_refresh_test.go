package model

import (
	"testing"
)

// The parity refresh of the embedded cards (#138 item 5): every card added
// from Python AgentScope at 3fef100e must load with its provider, a
// non-zero context window and the right card type; claude-sonnet-4-5 flips
// to deprecated per upstream.
func TestModelCardParityRefresh(t *testing.T) {
	want := map[string]string{
		"claude-fable-5-1":             "anthropic",
		"claude-haiku-5-5":             "anthropic",
		"claude-opus-5-5":              "anthropic",
		"claude-sonnet-5-5":            "anthropic",
		"gpt-6-luna":                   "openai",
		"gpt-6-sol":                    "openai",
		"gpt-6-astra":                  "openai",
		"gpt-6.1-sol":                  "openai",
		"gemini-3.7-flash":             "gemini",
		"gemini-3.8-flash":             "gemini",
		"gemini-4-argon":               "gemini",
		"gemini-4-carbon":              "gemini",
		"kimi-k3.1-pro":                "moonshot",
		"kimi-k3.1-pro-highspeed":      "moonshot",
		"deepseek-v4.1-flash":          "dashscope",
		"glm-5.3":                      "dashscope",
		"qwen3.8-flash":                "dashscope",
		"qwen3.8-omni-flash":           "dashscope",
		"gemma4":                       "ollama",
		"llama3.2:3b":                  "ollama",
		"phi4-mini":                    "ollama",
		"qwen3:8b":                     "ollama",
		"qwen3.8":                      "ollama",
		"deepseek-flash":               "deepseek",
		"grok-4.6":                     "xai",
		"grok-4.7":                     "xai",
		"doubao-seed-2-1-lite-260915":  "volcengine",
		"doubao-seed-2-1-pro-260628":   "volcengine",
		"doubao-seed-2-1-pro-260915":   "volcengine",
		"doubao-seed-2-1-turbo-260628": "volcengine",
		"doubao-seed-evolving":         "volcengine",
		"MiniMax-M2.7":                 "minimax",
		"MiniMax-M2.7-highspeed":       "minimax",
		"MiniMax-M3":                   "minimax",
	}
	for name, provider := range want {
		card, err := GetModelCard(name)
		if err != nil {
			t.Errorf("GetModelCard(%q): %v", name, err)
			continue
		}
		if card.Provider != provider {
			t.Errorf("card %q provider = %q, want %q", name, card.Provider, provider)
		}
		if card.ContextSize <= 0 {
			t.Errorf("card %q context_size = %d, want > 0", name, card.ContextSize)
		}
	}
}

func TestModelCardSonnet45Deprecated(t *testing.T) {
	card, err := GetModelCard("claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != ModelStatusDeprecated {
		t.Errorf("status = %q, want deprecated", card.Status)
	}
	if card.DeprecatedAt == nil {
		t.Error("deprecated_at not set")
	}
}
