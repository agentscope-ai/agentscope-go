package embedding

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// batchEmbedWithWorkers preserves the legacy unbounded default when workers=0.
// Positive worker counts bound local batch work, including retry backoff.
func batchEmbedWithWorkers(ctx context.Context, texts []string, batchSize, workers int, fn batchFunc) (*EmbeddingResponse, error) {
	if workers == 0 {
		return batchEmbed(ctx, texts, batchSize, fn)
	}
	if workers < 0 {
		return nil, fmt.Errorf("embedding: negative concurrency")
	}
	if len(texts) == 0 {
		return &EmbeddingResponse{Source: "api"}, nil
	}
	batches := splitBatches(texts, batchSize)
	sizes := make([]int, len(batches))
	for i, batch := range batches {
		sizes[i] = len(batch)
	}
	return runEmbeddingBatches(ctx, sizes, workers, func(ctx context.Context, i int) (*EmbeddingResponse, error) { return fn(ctx, batches[i]) })
}
func runEmbeddingBatches(ctx context.Context, sizes []int, workers int, fn func(context.Context, int) (*EmbeddingResponse, error)) (*EmbeddingResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(sizes) == 0 {
		return &EmbeddingResponse{Source: "api"}, nil
	}
	if workers < 1 {
		return nil, fmt.Errorf("embedding: concurrency must be positive")
	}
	if workers > len(sizes) {
		workers = len(sizes)
	}
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]*EmbeddingResponse, len(sizes))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var first error
	var once sync.Once
	fail := func(err error) { once.Do(func() { first = err; cancel() }) }
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				resp, err := fn(ctx, i)
				if err == nil && (resp == nil || len(resp.Embeddings) != sizes[i]) {
					err = fmt.Errorf("invalid embedding count for batch %d", i)
				}
				if err != nil {
					fail(fmt.Errorf("batch %d: %w", i, err))
					return
				}
				results[i] = resp
			}
		}()
	}
schedule:
	for i := range sizes {
		select {
		case <-ctx.Done():
			break schedule
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response := &EmbeddingResponse{ID: timestamp(), CreatedAt: timeNow(), Source: "api", Usage: &EmbeddingUsage{Time: time.Since(start).Seconds()}}
	for _, result := range results {
		response.Embeddings = append(response.Embeddings, result.Embeddings...)
		if result.Usage == nil {
			response.Usage = nil
		} else if response.Usage != nil {
			response.Usage.Tokens += result.Usage.Tokens
		}
	}
	return response, nil
}
