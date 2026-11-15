package embedding

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EmbeddingModelCard describes an embedding model's capabilities.
type EmbeddingModelCard struct {
	Name       string `yaml:"name"       json:"name"`
	Label      string `yaml:"label"      json:"label"`
	Provider   string `yaml:"-"          json:"provider"`
	Status     string `yaml:"status"     json:"status"`
	Dimensions int    `yaml:"dimensions" json:"dimensions"`
	MaxTokens  int    `yaml:"max_tokens" json:"max_tokens"`
}

//go:embed models
var embeddingModelsFS embed.FS

// ListEmbeddingModels returns all embedded embedding model cards, optionally
// filtered by provider. Models are sorted by provider then name.
func ListEmbeddingModels(providers ...string) []EmbeddingModelCard {
	providerSet := make(map[string]bool, len(providers))
	for _, p := range providers {
		providerSet[strings.ToLower(p)] = true
	}

	var cards []EmbeddingModelCard

	entries, err := fs.ReadDir(embeddingModelsFS, "models")
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

		yamlFiles, err := fs.ReadDir(embeddingModelsFS, path.Join("models", provider))
		if err != nil {
			continue
		}

		for _, f := range yamlFiles {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
				continue
			}
			card, err := loadEmbeddingCard(embeddingModelsFS, path.Join("models", provider, f.Name()), provider)
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

// GetEmbeddingModelCard returns a specific embedding model card by name.
func GetEmbeddingModelCard(name string) (*EmbeddingModelCard, error) {
	cards := ListEmbeddingModels()
	for i := range cards {
		if cards[i].Name == name {
			return &cards[i], nil
		}
	}
	return nil, fmt.Errorf("embedding model card %q not found", name)
}

func loadEmbeddingCard(fsys fs.FS, path string, provider string) (*EmbeddingModelCard, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var card EmbeddingModelCard
	if err := yaml.Unmarshal(data, &card); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if card.Name == "" {
		return nil, fmt.Errorf("embedding model card %s missing name", path)
	}

	card.Provider = provider

	if card.Status == "" {
		card.Status = "active"
	}

	return &card, nil
}
