package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
)

// EmbeddingModel generates vector embeddings from text inputs.
type EmbeddingModel interface {
	Embed(ctx context.Context, texts []string) (*EmbeddingResponse, error)
	ModelName() string
}

// EmbeddingResponse holds the result of an embedding request.
type EmbeddingResponse struct {
	Embeddings [][]float32
	ID         string
	CreatedAt  string
	Usage      *EmbeddingUsage
	Source     string // "api" or "cache"
}

// EmbeddingUsage tracks resource consumption for an embedding call.
type EmbeddingUsage struct {
	Time   float64 // elapsed seconds
	Tokens int     // total tokens consumed; 0 if provider does not report
}

// EmbeddingCache provides optional caching for embedding results.
type EmbeddingCache interface {
	Store(key string, embeddings [][]float32) error
	Retrieve(key string) ([][]float32, bool)
	Remove(key string) error
	Clear() error
}

// AsEmbedder wraps an EmbeddingModel to satisfy the rag.Embedder interface.
func AsEmbedder(m EmbeddingModel) rag.Embedder {
	return &embeddingAdapter{model: m}
}

type embeddingAdapter struct {
	model EmbeddingModel
}

func (a *embeddingAdapter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	resp, err := a.model.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	return resp.Embeddings, nil
}

// CacheKey computes a deterministic SHA-256 hex key from the given parts.
func CacheKey(parts ...any) string {
	data, _ := json.Marshal(parts)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// batchFunc processes a single batch of texts into an EmbeddingResponse.
type batchFunc func(ctx context.Context, texts []string) (*EmbeddingResponse, error)

// batchEmbed splits texts into batches and processes them concurrently.
func batchEmbed(ctx context.Context, texts []string, batchSize int, fn batchFunc) (*EmbeddingResponse, error) {
	if len(texts) == 0 {
		return &EmbeddingResponse{Source: "api"}, nil
	}

	start := time.Now()
	batches := splitBatches(texts, batchSize)

	if len(batches) == 1 {
		resp, err := fn(ctx, batches[0])
		if err != nil {
			return nil, err
		}
		if resp.Usage != nil {
			resp.Usage.Time = time.Since(start).Seconds()
		}
		return resp, nil
	}

	type result struct {
		resp *EmbeddingResponse
		err  error
	}
	results := make([]result, len(batches))
	var wg sync.WaitGroup
	for i, batch := range batches {
		wg.Add(1)
		go func(idx int, b []string) {
			defer wg.Done()
			r, err := fn(ctx, b)
			results[idx] = result{resp: r, err: err}
		}(i, batch)
	}
	wg.Wait()

	var allEmbeddings [][]float32
	var totalTokens int
	for i, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("batch %d: %w", i, r.err)
		}
		allEmbeddings = append(allEmbeddings, r.resp.Embeddings...)
		if r.resp.Usage != nil {
			totalTokens += r.resp.Usage.Tokens
		}
	}

	return &EmbeddingResponse{
		Embeddings: allEmbeddings,
		ID:         timestamp(),
		CreatedAt:  timeNow(),
		Usage: &EmbeddingUsage{
			Time:   time.Since(start).Seconds(),
			Tokens: totalTokens,
		},
		Source: "api",
	}, nil
}

func splitBatches(texts []string, batchSize int) [][]string {
	if batchSize <= 0 || len(texts) <= batchSize {
		return [][]string{texts}
	}
	var batches [][]string
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batches = append(batches, texts[i:end])
	}
	return batches
}

func timestamp() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func timeNow() string {
	return time.Now().Format(time.RFC3339)
}
