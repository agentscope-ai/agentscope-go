package embedding

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// GeminiConfig holds configuration for the Gemini embedding model.
type GeminiConfig struct {
	MaxConcurrency int // Positive bounds batch workers; zero preserves the legacy unbounded default.
	APIKey         string
	SecretAPIKey   model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL        string
	Model          string
	Dimensions     int // 0 = provider default
	BatchSize      int // 0 = 100
	HTTPClient     *http.Client
	Cache          EmbeddingCache // optional
}

// GeminiEmbeddingModel implements EmbeddingModel using Google's
// batchEmbedContents API.
type GeminiEmbeddingModel struct {
	maxConcurrency int
	managed        *inference.Deployment
	apiKey         string
	baseURL        string
	model          string
	dimensions     int
	batchSize      int
	client         *http.Client
	cache          EmbeddingCache
}

// NewGeminiEmbeddingModel creates an embedding model using Google Gemini's API.
func NewGeminiEmbeddingModel(cfg *GeminiConfig) (*GeminiEmbeddingModel, error) {
	if cfg.MaxConcurrency < 0 {
		return nil, fmt.Errorf("embedding: negative concurrency")
	}
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("embedding: Gemini API key is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("embedding: model is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultEmbeddingTimeout}
	}
	return &GeminiEmbeddingModel{
		maxConcurrency: cfg.MaxConcurrency,
		apiKey:         apiKey,
		baseURL:        cfg.BaseURL,
		model:          cfg.Model,
		dimensions:     cfg.Dimensions,
		batchSize:      cfg.BatchSize,
		client:         client,
		cache:          cfg.Cache,
	}, nil
}

func (m *GeminiEmbeddingModel) ModelName() string         { return m.model }
func (m *GeminiEmbeddingModel) Dimensions() int           { return m.dimensions }
func (m *GeminiEmbeddingModel) SetCache(c EmbeddingCache) { m.cache = c }

func (m *GeminiEmbeddingModel) Embed(ctx context.Context, texts []string) (*EmbeddingResponse, error) {
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

func (m *GeminiEmbeddingModel) callAPI(ctx context.Context, texts []string) (result *EmbeddingResponse, resultErr error) {
	if m.managed != nil {
		var finish func(error)
		ctx, finish = m.managed.StartIndependent(ctx, "embedding")
		defer func() { finish(resultErr) }()
	}
	url := fmt.Sprintf("%s/models/%s:batchEmbedContents?key=%s", m.baseURL, m.model, m.apiKey)

	requests := make([]geminiBatchRequest, len(texts))
	for i, text := range texts {
		req := geminiBatchRequest{
			Model: "models/" + m.model,
			Content: geminiContent{
				Parts: []geminiPart{{Text: text}},
			},
		}
		if m.dimensions > 0 {
			req.OutputDimensionality = m.dimensions
		}
		requests[i] = req
	}

	reqBody := geminiBatchEmbedRequest{Requests: requests}
	var resp geminiBatchEmbedResponse

	headers := map[string]string{"Content-Type": "application/json"}

	if err := httpx.DoJSONRequest(ctx, m.client, http.MethodPost, url, reqBody, &resp, headers); err != nil {
		return nil, fmt.Errorf("Gemini embedding API call failed: %w", err)
	}
	if m.managed != nil && len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embedding: response count differs from input")
	}

	embeddings := make([][]float32, len(resp.Embeddings))
	for i, e := range resp.Embeddings {
		embeddings[i] = e.Values
	}

	return &EmbeddingResponse{
		Embeddings: embeddings,
		ID:         timestamp(),
		CreatedAt:  timeNow(),
		Usage:      &EmbeddingUsage{},
		Source:     "api",
	}, nil
}

// --- wire types for Gemini batchEmbedContents API ---

type geminiBatchEmbedRequest struct {
	Requests []geminiBatchRequest `json:"requests"`
}

type geminiBatchRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	OutputDimensionality int           `json:"outputDimensionality,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []geminiEmbeddingResult `json:"embeddings"`
}

type geminiEmbeddingResult struct {
	Values []float32 `json:"values"`
}
