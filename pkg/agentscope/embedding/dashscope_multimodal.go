package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

var multimodalPrefixes = []string{
	"multimodal-embedding-",
	"tongyi-embedding-vision-",
	"qwen3-vl-embedding",
	"qwen2.5-vl-embedding",
}

// IsMultimodalModel returns true if the model name routes to the multimodal API.
func IsMultimodalModel(modelName string) bool {
	for _, prefix := range multimodalPrefixes {
		if strings.HasPrefix(modelName, prefix) {
			return true
		}
	}
	return false
}

type multimodalLimits struct {
	maxElements int
	maxImages   int
	maxVideos   int
}

var modelLimits = map[string]multimodalLimits{
	"qwen3-vl-embedding":            {20, 5, 1},
	"qwen2.5-vl-embedding":          {20, 5, 1},
	"tongyi-embedding-vision-plus":  {20, 64, 8},
	"tongyi-embedding-vision-flash": {20, 64, 8},
	"multimodal-embedding-v1":       {20, 1, 1},
}

var defaultLimits = multimodalLimits{20, 1, 1}

func getLimits(model string) multimodalLimits {
	if l, ok := modelLimits[model]; ok {
		return l
	}
	return defaultLimits
}

// MultimodalInput represents a text or data block for multimodal embedding.
type MultimodalInput struct {
	Text      string             // non-empty for text input
	DataBlock *message.DataBlock // non-nil for image/video input
}

// DashScopeMultimodalConfig configures a DashScope multimodal embedding model.
type DashScopeMultimodalConfig struct {
	APIKey       string
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL      string
	Model        string
	HTTPClient   *http.Client
}

// DashScopeMultimodalEmbeddingModel embeds text and DataBlock (images/video)
// via the DashScope native multimodal embedding API.
type DashScopeMultimodalEmbeddingModel struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
	limits  multimodalLimits
}

// NewDashScopeMultimodalEmbeddingModel creates a multimodal embedding model.
func NewDashScopeMultimodalEmbeddingModel(cfg DashScopeMultimodalConfig) (*DashScopeMultimodalEmbeddingModel, error) {
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("embedding: DashScope API key is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("embedding: model name is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://dashscope.aliyuncs.com/api/v1"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &DashScopeMultimodalEmbeddingModel{
		apiKey:  apiKey,
		baseURL: cfg.BaseURL,
		model:   cfg.Model,
		client:  client,
		limits:  getLimits(cfg.Model),
	}, nil
}

func (m *DashScopeMultimodalEmbeddingModel) ModelName() string { return m.model }

// EmbedMultimodal embeds a batch of multimodal inputs.
// Each input can be text, an image DataBlock, or a video DataBlock.
// If the input exceeds the model's element limits, it is automatically
// split into sub-batches, each processed separately, and the results merged.
func (m *DashScopeMultimodalEmbeddingModel) EmbedMultimodal(ctx context.Context, inputs []MultimodalInput) (*EmbeddingResponse, error) {
	if len(inputs) == 0 {
		return &EmbeddingResponse{}, nil
	}

	limits := getLimits(m.model)
	if len(inputs) <= limits.maxElements {
		return m.embedMultimodalBatch(ctx, inputs)
	}

	// Split into sub-batches that respect maxElements.
	allEmbeddings := make([][]float32, len(inputs))
	totalTokens := 0

	for start := 0; start < len(inputs); start += limits.maxElements {
		end := start + limits.maxElements
		if end > len(inputs) {
			end = len(inputs)
		}
		batch := inputs[start:end]

		resp, err := m.embedMultimodalBatch(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("batch [%d:%d]: %w", start, end, err)
		}

		for i, emb := range resp.Embeddings {
			allEmbeddings[start+i] = emb
		}
		if resp.Usage != nil {
			totalTokens += resp.Usage.Tokens
		}
	}

	return &EmbeddingResponse{
		Embeddings: allEmbeddings,
		Usage:      &EmbeddingUsage{Tokens: totalTokens},
	}, nil
}

// embedMultimodalBatch sends a single batch of inputs to the API.
func (m *DashScopeMultimodalEmbeddingModel) embedMultimodalBatch(ctx context.Context, inputs []MultimodalInput) (*EmbeddingResponse, error) {
	contents := make([]map[string]any, 0, len(inputs))
	for _, inp := range inputs {
		if inp.Text != "" {
			contents = append(contents, map[string]any{
				"text": inp.Text,
			})
		} else if inp.DataBlock != nil {
			content := m.formatDataBlock(inp.DataBlock)
			if content != nil {
				contents = append(contents, content)
			}
		}
	}

	reqBody := map[string]any{
		"model": m.model,
		"input": map[string]any{
			"contents": []any{contents},
		},
		"parameters": map[string]any{
			"text_type": "query",
		},
	}

	body, _ := json.Marshal(reqBody)
	url := m.baseURL + "/services/embeddings/multimodal-embedding/multimodal-embedding"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("multimodal embed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Output struct {
			Embeddings []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"embeddings"`
		} `json:"output"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode multimodal response: %w", err)
	}

	embeddings := make([][]float32, len(inputs))
	for _, emb := range result.Output.Embeddings {
		if emb.Index < len(embeddings) {
			embeddings[emb.Index] = emb.Embedding
		}
	}

	return &EmbeddingResponse{
		Embeddings: embeddings,
		Usage:      &EmbeddingUsage{Tokens: result.Usage.TotalTokens},
	}, nil
}

// Embed implements EmbeddingModel for text-only inputs.
// For multimodal inputs, use EmbedMultimodal.
func (m *DashScopeMultimodalEmbeddingModel) Embed(ctx context.Context, texts []string) (*EmbeddingResponse, error) {
	inputs := make([]MultimodalInput, len(texts))
	for i, t := range texts {
		inputs[i] = MultimodalInput{Text: t}
	}
	return m.EmbedMultimodal(ctx, inputs)
}

func (m *DashScopeMultimodalEmbeddingModel) formatDataBlock(db *message.DataBlock) map[string]any {
	mt := db.GetMediaType()
	switch {
	case strings.HasPrefix(mt, "image/"):
		switch src := db.Source.(type) {
		case message.URLSource:
			return map[string]any{"image": src.URL}
		case message.Base64Source:
			return map[string]any{"image": "data:" + mt + ";base64," + src.Data}
		}
	case strings.HasPrefix(mt, "video/"):
		if src, ok := db.Source.(message.URLSource); ok {
			return map[string]any{"video": src.URL}
		}
	}
	return nil
}

// Compile-time check.
var _ EmbeddingModel = (*DashScopeMultimodalEmbeddingModel)(nil)
