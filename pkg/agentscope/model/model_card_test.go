package model

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestListModelsAll(t *testing.T) {
	cards := ListModels()
	if len(cards) == 0 {
		t.Fatal("ListModels() returned no cards")
	}
	t.Logf("loaded %d model cards total", len(cards))

	// Verify sorted by provider then name
	for i := 1; i < len(cards); i++ {
		prev := cards[i-1]
		cur := cards[i]
		if prev.Provider > cur.Provider || (prev.Provider == cur.Provider && prev.Name > cur.Name) {
			t.Errorf("cards not sorted: [%d] %s/%s > [%d] %s/%s", i-1, prev.Provider, prev.Name, i, cur.Provider, cur.Name)
		}
	}
}

func TestListModelsByProvider(t *testing.T) {
	tests := []struct {
		provider string
		minCount int
	}{
		{"anthropic", 7},
		{"dashscope", 7},
		{"openai", 9},
		{"deepseek", 4},
		{"ollama", 4},
		{"gemini", 4},
		{"moonshot", 5},
		{"xai", 4},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			cards := ListModels(tt.provider)
			if len(cards) < tt.minCount {
				t.Errorf("ListModels(%q) returned %d cards, want >= %d", tt.provider, len(cards), tt.minCount)
			}
			for _, c := range cards {
				if c.Provider != tt.provider {
					t.Errorf("card %q has provider %q, want %q", c.Name, c.Provider, tt.provider)
				}
			}
		})
	}
}

func TestListModelsMultipleProviders(t *testing.T) {
	cards := ListModels("anthropic", "openai")
	if len(cards) < 16 {
		t.Errorf("expected >= 16 cards for anthropic+openai, got %d", len(cards))
	}
	for _, c := range cards {
		if c.Provider != "anthropic" && c.Provider != "openai" {
			t.Errorf("unexpected provider %q", c.Provider)
		}
	}
}

func TestGetModelCard(t *testing.T) {
	card, err := GetModelCard("claude-opus-4-6")
	if err != nil {
		t.Fatalf("GetModelCard(claude-opus-4-6): %v", err)
	}
	if card.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", card.Provider)
	}
	if card.ContextSize != 1000000 {
		t.Errorf("context_size = %d, want 1000000", card.ContextSize)
	}
	if card.OutputSize != 128000 {
		t.Errorf("output_size = %d, want 128000", card.OutputSize)
	}
	if !card.SupportsThinking() {
		t.Error("expected SupportsThinking() = true")
	}
	if !card.SupportsImages() {
		t.Error("expected SupportsImages() = true")
	}
}

func TestGetModelCardNotFound(t *testing.T) {
	_, err := GetModelCard("nonexistent-model")
	if err == nil {
		t.Error("expected error for nonexistent model")
	}
}

func TestModelCardCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		thinking bool
		images   bool
		audio    bool
		video    bool
	}{
		{"claude-opus-4-6", true, true, false, false},
		{"gpt-4.1", false, true, false, false},
		{"qwen3.5-omni-plus", true, true, true, true},
		{"deepseek-chat", false, false, false, false},
		{"gemini-2.5-pro", true, true, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card, err := GetModelCard(tt.name)
			if err != nil {
				t.Fatalf("GetModelCard(%q): %v", tt.name, err)
			}
			if got := card.SupportsThinking(); got != tt.thinking {
				t.Errorf("SupportsThinking() = %v, want %v", got, tt.thinking)
			}
			if got := card.SupportsImages(); got != tt.images {
				t.Errorf("SupportsImages() = %v, want %v", got, tt.images)
			}
			if got := card.SupportsAudio(); got != tt.audio {
				t.Errorf("SupportsAudio() = %v, want %v", got, tt.audio)
			}
			if got := card.SupportsVideo(); got != tt.video {
				t.Errorf("SupportsVideo() = %v, want %v", got, tt.video)
			}
		})
	}
}

func TestModelCardFields(t *testing.T) {
	card, err := GetModelCard("deepseek-reasoner")
	if err != nil {
		t.Fatalf("GetModelCard: %v", err)
	}
	if card.Status != ModelStatusSunset {
		t.Errorf("status = %q, want %q", card.Status, ModelStatusSunset)
	}
	if card.DeprecatedAt == nil {
		t.Error("expected DeprecatedAt to be set")
	}
	if card.Type != ModelCardTypeChat {
		t.Errorf("type = %q, want %q", card.Type, ModelCardTypeChat)
	}
}

func TestModelCardDeprecatedAtFormats(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // expected RFC3339 (UTC); empty means DeprecatedAt must be nil
	}{
		{"rfc3339", "name: m\ndeprecated_at: \"2026-07-24T00:00:00Z\"\n", "2026-07-24T00:00:00Z"},
		{"naive-iso", "name: m\ndeprecated_at: \"2026-07-24T00:00:00\"\n", "2026-07-24T00:00:00Z"},
		{"date-only", "name: m\ndeprecated_at: \"2026-07-24\"\n", "2026-07-24T00:00:00Z"},
		{"space-separated", "name: m\ndeprecated_at: \"2026-07-24 00:00:00\"\n", "2026-07-24T00:00:00Z"},
		{"unquoted-naive", "name: m\ndeprecated_at: 2026-07-24T10:30:00\n", "2026-07-24T10:30:00Z"},
		{"absent", "name: m\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var card ModelCard
			if err := yaml.Unmarshal([]byte(tc.doc), &card); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tc.want == "" {
				if card.DeprecatedAt != nil {
					t.Fatalf("DeprecatedAt = %v, want nil", card.DeprecatedAt)
				}
				return
			}
			if card.DeprecatedAt == nil {
				t.Fatal("DeprecatedAt = nil, want set")
			}
			if got := card.DeprecatedAt.UTC().Format(time.RFC3339); got != tc.want {
				t.Errorf("DeprecatedAt = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestModelCardDeprecatedAtInvalid(t *testing.T) {
	var card ModelCard
	err := yaml.Unmarshal([]byte("name: m\ndeprecated_at: \"not-a-date\"\n"), &card)
	if err == nil {
		t.Fatal("expected error for invalid deprecated_at, got nil")
	}
}
