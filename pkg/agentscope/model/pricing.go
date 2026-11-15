package model

import (
	_ "embed"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Price is USD per 1M tokens (HARNESS_DESIGN D1). The built-in table is an
// overlay maintained separately from upstream model-card syncs; callers can
// always override with SetPrice (overrides win).
type Price struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

//go:embed pricing/default.yaml
var defaultPriceYAML []byte

var (
	priceMu       sync.RWMutex
	priceTable    = map[string]Price{}
	priceOverride = map[string]Price{}
)

func init() {
	var doc struct {
		AsOf   string           `yaml:"as_of"`
		Models map[string]Price `yaml:"models"`
	}
	if err := yaml.Unmarshal(defaultPriceYAML, &doc); err == nil {
		for name, p := range doc.Models {
			priceTable[name] = p
		}
	}
}

// SetPrice overrides (or adds) the price for a model name. Overrides always
// win over the built-in overlay.
func SetPrice(modelName string, p Price) {
	priceMu.Lock()
	defer priceMu.Unlock()
	priceOverride[modelName] = p
}

// ResolvePrice returns the price for a model: overrides first, then the
// built-in overlay (exact match, then prefix match for versioned names like
// "claude-sonnet-4-5-20260101"). ok=false means unknown — callers should
// treat cost as unknown rather than zero.
func ResolvePrice(modelName string) (Price, bool) {
	priceMu.RLock()
	defer priceMu.RUnlock()
	if p, ok := priceOverride[modelName]; ok {
		return p, true
	}
	if p, ok := priceTable[modelName]; ok {
		return p, true
	}
	// Longest-prefix-wins so versioned names resolve deterministically
	// (gpt-4o-mini-* must match gpt-4o-mini, not gpt-4o; HARNESS review M7).
	best := ""
	var bestPrice Price
	for name, p := range priceTable {
		if strings.HasPrefix(modelName, name) && len(name) > len(best) {
			best = name
			bestPrice = p
		}
	}
	if best != "" {
		return bestPrice, true
	}
	return Price{}, false
}

// CostUSD computes the USD cost of one usage sample at price p.
func (p Price) CostUSD(usage *ChatUsage) float64 {
	if usage == nil {
		return 0
	}
	cost := float64(usage.InputTokens)*p.Input/1e6 +
		float64(usage.OutputTokens)*p.Output/1e6 +
		float64(usage.CacheInputTokens)*p.CacheRead/1e6 +
		float64(usage.CacheCreationInputTokens)*p.CacheWrite/1e6
	return cost
}
