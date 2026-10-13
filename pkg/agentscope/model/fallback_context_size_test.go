package model

import "testing"

// namedModel reports a model name but no ContextSize, so its window can only
// come from the embedded model card for that name.
type namedModel struct {
	*connMockModel
}

func (m *namedModel) ModelName() string { return m.name }

// cardModel returns a namedModel for a card with the given window.
func cardModel(t *testing.T, name string, wantWindow int) ChatModel {
	t.Helper()
	card, err := GetModelCard(name)
	if err != nil {
		t.Fatalf("fixture card missing: %v", err)
	}
	if card.ContextSize != wantWindow {
		t.Fatalf("fixture card %s window = %d, want %d", name, card.ContextSize, wantWindow)
	}
	return &namedModel{connMockModel: &connMockModel{name: name}}
}

func TestResolveContextSize_FallbackChatModel(t *testing.T) {
	o3 := cardModel(t, "o3", 200000)
	localFirst := NewConnectivityAwareModel(windowModel("local", 8192), windowModel("cloud", 0))
	largeCAM := NewConnectivityAwareModel(windowModel("local", 262144), windowModel("cloud", 200000))
	tests := []struct {
		name     string
		model    ChatModel
		fallback int
		want     int
	}{
		{"large first, small second", NewFallbackChain(windowModel("large", 200000), windowModel("small", 8192)), testContextFallback, 8192},
		{"small first, large second", NewFallbackChain(windowModel("small", 8192), windowModel("large", 200000)), testContextFallback, 8192},
		{"silent later member", NewFallbackChain(windowModel("large", 200000), windowModel("silent", 0)), testContextFallback, testContextFallback},
		{"silent first member", NewFallbackChain(windowModel("silent", 0), windowModel("small", 32000)), testContextFallback, 32000},
		{"silent member, small caller fallback", NewFallbackChain(windowModel("large", 200000), windowModel("silent", 0), windowModel("small", 32000)), 4096, 4096},
		{"declared above caller fallback", NewFallbackChain(windowModel("larger", 262144), windowModel("large", 200000)), testContextFallback, 200000},
		{"card window of a later member", NewFallbackChain(windowModel("larger", 262144), o3), testContextFallback, 200000},
		{"connectivity-aware first member", NewFallbackChain(localFirst, windowModel("large", 200000)), testContextFallback, 8192},
		{"connectivity-aware member above caller fallback", NewFallbackChain(largeCAM, windowModel("larger", 262144)), testContextFallback, 200000},
		{"nested chain", NewFallbackChain(windowModel("large", 200000), NewFallbackChain(windowModel("mid", 32000), windowModel("small", 8192))), testContextFallback, 8192},
		{"primary and fallback fields", NewFallbackChatModel(windowModel("large", 200000), windowModel("small", 8192)), testContextFallback, 8192},
		{"empty chain", NewFallbackChain(), testContextFallback, testContextFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveContextSize(tt.model, tt.fallback); got != tt.want {
				t.Errorf("ResolveContextSize = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFallbackChatModel_MetadataReportsFirstModel(t *testing.T) {
	chain := NewFallbackChain(windowModel("large", 200000), windowModel("small", 8192))
	if got := chain.ContextSize(); got != 200000 {
		t.Errorf("ContextSize = %d, want first model's 200000", got)
	}
	if got := NewFallbackChain(windowModel("silent", 0), windowModel("small", 8192)).ContextSize(); got != 0 {
		t.Errorf("ContextSize with silent first model = %d, want 0", got)
	}
	if got := NewFallbackChain(cardModel(t, "o3", 200000), windowModel("small", 8192)).ModelName(); got != "o3" {
		t.Errorf("ModelName = %q, want first model's %q", got, "o3")
	}
	if got := chain.ModelName(); got != "" {
		t.Errorf("ModelName with unnamed first model = %q, want empty", got)
	}
}
