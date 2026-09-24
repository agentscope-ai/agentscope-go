package embedding

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const (
	defaultEmbeddingTimeout = 60 * time.Second
	defaultBatchSize        = 256
)

// OpenAICompatConfig holds configuration for OpenAI-compatible embedding models.
// Works with OpenAI, DashScope, Ollama, and other compatible providers.
type OpenAICompatConfig struct {
	MaxConcurrency int // Positive bounds batch workers; zero preserves the legacy unbounded default.
	APIKey         string
	SecretAPIKey   model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL        string
	Model          string
	Dimensions     int  // 0 = provider default
	OmitDimensions bool // true = never send dimensions to API (some providers reject it)
	BatchSize      int  // 0 = constructor-specific default
	HTTPClient     *http.Client
	Cache          EmbeddingCache // optional
}

// OpenAICompatEmbeddingModel implements EmbeddingModel using the OpenAI-compatible
// embedding API (/embeddings endpoint).
type OpenAICompatEmbeddingModel struct {
	maxConcurrency int
	managed        *inference.Deployment
	apiKey         string
	baseURL        string
	model          string
	dimensions     int
	passDimensions bool
	batchSize      int
	client         *http.Client
	cache          EmbeddingCache
}

func newOpenAICompat(cfg *OpenAICompatConfig) (*OpenAICompatEmbeddingModel, error) {
	if cfg.MaxConcurrency < 0 {
		return nil, fmt.Errorf("embedding: negative concurrency")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("embedding: model is required")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("embedding: base URL is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultEmbeddingTimeout}
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	return &OpenAICompatEmbeddingModel{
		maxConcurrency: cfg.MaxConcurrency,
		apiKey:         apiKey,
		baseURL:        cfg.BaseURL,
		model:          cfg.Model,
		dimensions:     cfg.Dimensions,
		passDimensions: !cfg.OmitDimensions,
		batchSize:      batchSize,
		client:         client,
		cache:          cfg.Cache,
	}, nil
}

// NewOpenAIEmbeddingModel creates an embedding model using OpenAI's API.
func NewOpenAIEmbeddingModel(cfg *OpenAICompatConfig) (*OpenAICompatEmbeddingModel, error) {
	if model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey) == "" {
		return nil, fmt.Errorf("embedding: OpenAI API key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 2048
	}
	return newOpenAICompat(cfg)
}

// NewDashScopeEmbeddingModel creates an embedding model using DashScope's
// OpenAI-compatible API.
func NewDashScopeEmbeddingModel(cfg *OpenAICompatConfig) (*OpenAICompatEmbeddingModel, error) {
	if model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey) == "" {
		return nil, fmt.Errorf("embedding: DashScope API key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 10
	}
	return newOpenAICompat(cfg)
}

// NewOllamaEmbeddingModel creates an embedding model using Ollama's
// OpenAI-compatible API. No API key is required.
func NewOllamaEmbeddingModel(cfg *OpenAICompatConfig) (*OpenAICompatEmbeddingModel, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:11434/v1"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	return newOpenAICompat(cfg)
}

func (m *OpenAICompatEmbeddingModel) ModelName() string         { return m.model }
func (m *OpenAICompatEmbeddingModel) Dimensions() int           { return m.dimensions }
func (m *OpenAICompatEmbeddingModel) SetCache(c EmbeddingCache) { m.cache = c }

func (m *OpenAICompatEmbeddingModel) Embed(ctx context.Context, texts []string) (*EmbeddingResponse, error) {
	if len(texts) == 0 {
		return &EmbeddingResponse{Source: "api"}, nil
	}

	if m.cache != nil {
		key := embeddingKey(ctx, m.managed, m.model, m.dimensions, texts)
		if embeddings, ok := m.cache.Retrieve(key); ok {
			return &EmbeddingResponse{
				Embeddings: embeddings,
				ID:         timestamp(),
				CreatedAt:  timeNow(),
				Usage:      &EmbeddingUsage{},
				Source:     "cache",
			}, nil
		}
	}

	resp, err := batchEmbedWithWorkers(ctx, texts, m.batchSize, m.maxConcurrency, m.callAPI)
	if err != nil {
		return nil, err
	}

	if m.cache != nil {
		key := embeddingKey(ctx, m.managed, m.model, m.dimensions, texts)
		_ = m.cache.Store(key, resp.Embeddings)
	}

	return resp, nil
}

func (m *OpenAICompatEmbeddingModel) callAPI(ctx context.Context, texts []string) (result *EmbeddingResponse, resultErr error) {
	if m.managed != nil {
		var finish func(error)
		ctx, finish = m.managed.StartIndependent(ctx, "embedding")
		defer func() { finish(resultErr) }()
	}
	url := m.baseURL + "/embeddings"

	req := openAIEmbeddingRequest{
		Model:          m.model,
		Input:          texts,
		EncodingFormat: "float",
	}
	if m.dimensions > 0 && m.passDimensions {
		req.Dimensions = m.dimensions
	}

	headers := map[string]string{
		"Content-Type": "application/json",
	}
	if m.apiKey != "" {
		headers["Authorization"] = "Bearer " + m.apiKey
	}

	var resp openAIEmbeddingResponse
	if err := httpx.DoJSONRequest(ctx, m.client, http.MethodPost, url, req, &resp, headers); err != nil {
		return nil, fmt.Errorf("embedding API call failed: %w", err)
	}

	sort.Slice(resp.Data, func(i, j int) bool {
		return resp.Data[i].Index < resp.Data[j].Index
	})

	if m.managed != nil {
		if len(resp.Data) != len(texts) {
			return nil, fmt.Errorf("embedding: response count differs from input")
		}
		for i, item := range resp.Data {
			if item.Index != i {
				return nil, fmt.Errorf("embedding: missing or duplicate response index")
			}
		}
	}
	embeddings := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		if d.Embedding != nil {
			embeddings[i] = d.Embedding
		} else if d.DenseEmbedding != nil {
			embeddings[i] = d.DenseEmbedding
		}
	}

	var tokens int
	if resp.Usage != nil {
		tokens = resp.Usage.TotalTokens
	}

	return &EmbeddingResponse{
		Embeddings: embeddings,
		ID:         timestamp(),
		CreatedAt:  timeNow(),
		Usage: &EmbeddingUsage{
			Tokens: tokens,
		},
		Source: "api",
	}, nil
}

// --- wire types for OpenAI-compatible embedding API ---

type openAIEmbeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type openAIEmbeddingResponse struct {
	Object string                `json:"object"`
	Data   []openAIEmbeddingData `json:"data"`
	Model  string                `json:"model"`
	Usage  *openAIEmbeddingUsage `json:"usage"`
}

type openAIEmbeddingData struct {
	Object         string    `json:"object"`
	Index          int       `json:"index"`
	Embedding      []float32 `json:"embedding"`
	DenseEmbedding []float32 `json:"dense_embedding"`
}

type openAIEmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
