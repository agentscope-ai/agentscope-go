package tts

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// TTSModelCard describes a TTS model's capabilities and configuration.
type TTSModelCard struct {
	Name         string   `yaml:"name"          json:"name"`
	Label        string   `yaml:"label"         json:"label"`
	Provider     string   `yaml:"-"             json:"provider"`
	Status       string   `yaml:"status"        json:"status"`
	SampleRate   int      `yaml:"sample_rate"   json:"sample_rate"`
	OutputFormat string   `yaml:"output_format" json:"output_format"`
	Languages    []string `yaml:"languages"     json:"languages"`
}

//go:embed models
var ttsModelsFS embed.FS

// ListTTSModels returns all embedded TTS model cards, optionally filtered
// by provider. Models are sorted by provider then name.
func ListTTSModels(providers ...string) []TTSModelCard {
	providerSet := make(map[string]bool, len(providers))
	for _, p := range providers {
		providerSet[strings.ToLower(p)] = true
	}

	var cards []TTSModelCard

	entries, err := fs.ReadDir(ttsModelsFS, "models")
	if err != nil {
		return nil
	}

	for _, providerDir := range entries {
		if !providerDir.IsDir() {
			continue
		}
		provider := providerDir.Name()
		if len(providerSet) > 0 && !providerSet[provider] {
			continue
		}

		yamlFiles, err := fs.ReadDir(ttsModelsFS, path.Join("models", provider))
		if err != nil {
			continue
		}

		for _, f := range yamlFiles {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
				continue
			}
			card, err := loadTTSCard(ttsModelsFS, path.Join("models", provider, f.Name()), provider)
			if err != nil {
				continue
			}
			cards = append(cards, *card)
		}
	}

	sort.Slice(cards, func(i, j int) bool {
		if cards[i].Provider != cards[j].Provider {
			return cards[i].Provider < cards[j].Provider
		}
		return cards[i].Name < cards[j].Name
	})

	return cards
}

// GetTTSModelCard returns a specific TTS model card by name.
func GetTTSModelCard(name string) (*TTSModelCard, error) {
	cards := ListTTSModels()
	for i := range cards {
		if cards[i].Name == name {
			return &cards[i], nil
		}
	}
	return nil, fmt.Errorf("tts model card %q not found", name)
}

func loadTTSCard(fsys fs.FS, path string, provider string) (*TTSModelCard, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var card TTSModelCard
	if err := yaml.Unmarshal(data, &card); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if card.Name == "" {
		return nil, fmt.Errorf("tts model card %s missing name", path)
	}

	card.Provider = provider

	if card.Status == "" {
		card.Status = "active"
	}

	return &card, nil
}
