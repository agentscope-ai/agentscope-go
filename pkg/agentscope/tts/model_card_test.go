package tts

import (
	"testing"
)

func TestListTTSModels_All(t *testing.T) {
	cards := ListTTSModels()
	if len(cards) == 0 {
		t.Fatal("expected at least one TTS model card")
	}

	for _, c := range cards {
		if c.Name == "" {
			t.Error("found card with empty name")
		}
		if c.Provider == "" {
			t.Errorf("card %q has empty provider", c.Name)
		}
	}
}

func TestListTTSModels_FilterProvider(t *testing.T) {
	cards := ListTTSModels("openai")
	for _, c := range cards {
		if c.Provider != "openai" {
			t.Errorf("expected provider openai, got %q", c.Provider)
		}
	}
	if len(cards) == 0 {
		t.Error("expected at least one openai TTS model")
	}
}

func TestListTTSModels_Sorted(t *testing.T) {
	cards := ListTTSModels()
	for i := 1; i < len(cards); i++ {
		prev := cards[i-1].Provider + "/" + cards[i-1].Name
		curr := cards[i].Provider + "/" + cards[i].Name
		if prev > curr {
			t.Errorf("cards not sorted: %q before %q", prev, curr)
		}
	}
}

func TestGetTTSModelCard_Found(t *testing.T) {
	card, err := GetTTSModelCard("tts-1")
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "tts-1" {
		t.Errorf("name = %q", card.Name)
	}
	if card.Provider != "openai" {
		t.Errorf("provider = %q", card.Provider)
	}
}

func TestGetTTSModelCard_NotFound(t *testing.T) {
	_, err := GetTTSModelCard("nonexistent-tts-model")
	if err == nil {
		t.Fatal("expected error for nonexistent model")
	}
}

func TestGetTTSModelCard_CosyVoice(t *testing.T) {
	card, err := GetTTSModelCard("cosyvoice-v2")
	if err != nil {
		t.Fatal(err)
	}
	if card.Provider != "dashscope" {
		t.Errorf("provider = %q, want dashscope", card.Provider)
	}
	if len(card.Languages) == 0 {
		t.Error("expected languages to be populated")
	}
}
