package embedding

import (
	"testing"
)

func TestListEmbeddingModels_All(t *testing.T) {
	cards := ListEmbeddingModels()
	if len(cards) == 0 {
		t.Fatal("expected at least one embedding model card")
	}

	// Verify all cards have basic fields
	for _, c := range cards {
		if c.Name == "" {
			t.Error("found card with empty name")
		}
		if c.Provider == "" {
			t.Errorf("card %q has empty provider", c.Name)
		}
		if c.Dimensions <= 0 {
			t.Errorf("card %q has invalid dimensions: %d", c.Name, c.Dimensions)
		}
	}
}

func TestListEmbeddingModels_FilterProvider(t *testing.T) {
	cards := ListEmbeddingModels("openai")
	for _, c := range cards {
		if c.Provider != "openai" {
			t.Errorf("expected provider openai, got %q", c.Provider)
		}
	}
	if len(cards) == 0 {
		t.Error("expected at least one openai embedding model")
	}
}

func TestListEmbeddingModels_Sorted(t *testing.T) {
	cards := ListEmbeddingModels()
	for i := 1; i < len(cards); i++ {
		prev := cards[i-1].Provider + "/" + cards[i-1].Name
		curr := cards[i].Provider + "/" + cards[i].Name
		if prev > curr {
			t.Errorf("cards not sorted: %q before %q", prev, curr)
		}
	}
}

func TestGetEmbeddingModelCard_Found(t *testing.T) {
	card, err := GetEmbeddingModelCard("text-embedding-3-small")
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "text-embedding-3-small" {
		t.Errorf("name = %q", card.Name)
	}
	if card.Provider != "openai" {
		t.Errorf("provider = %q", card.Provider)
	}
	if card.Dimensions != 1536 {
		t.Errorf("dimensions = %d, want 1536", card.Dimensions)
	}
}

func TestGetEmbeddingModelCard_NotFound(t *testing.T) {
	_, err := GetEmbeddingModelCard("nonexistent-model")
	if err == nil {
		t.Fatal("expected error for nonexistent model")
	}
}
