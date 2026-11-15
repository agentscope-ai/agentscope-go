// qdrant_filter creates a temporary collection in a local Qdrant server, queries
// it through a text index, and removes only that collection on exit.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag"
	"github.com/qdrant/go-client/qdrant"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	client, err := qdrant.NewClient(&qdrant.Config{Host: "localhost", Port: 6334})
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	collection := fmt.Sprintf("agentscope_filter_example_%d", time.Now().UnixNano())
	if err := client.CreateCollection(ctx, &qdrant.CreateCollection{CollectionName: collection, VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: 2, Distance: qdrant.Distance_Dot})}); err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := client.DeleteCollection(cleanup, collection); err != nil {
			fmt.Fprintln(os.Stderr, "collection cleanup:", err)
		}
	}()
	index, err := rag.NewQdrantTextIndex(rag.QdrantTextConfig{Client: client, Collection: collection, Embedder: fixtureEmbedder{}, RequiredFilter: rag.MetadataFilter{rag.MetadataEqualString("tenant", "alpha")}})
	if err != nil {
		return err
	}
	docs := []rag.Document{
		{ID: "00000000-0000-0000-0000-000000000001", Content: "eligible", Meta: map[string]any{"tenant": "alpha", "rating": 4.5}},
		{ID: "00000000-0000-0000-0000-000000000002", Content: "other tenant", Meta: map[string]any{"tenant": "beta", "rating": 5.0}},
		{ID: "00000000-0000-0000-0000-000000000003", Content: "below threshold", Meta: map[string]any{"tenant": "alpha", "rating": 2.0}},
	}
	if err := index.AddDocuments(ctx, docs); err != nil {
		return err
	}
	minimum := 4.0
	filter := rag.MetadataFilter{rag.MetadataInRange("rating", rag.NumericRange{GTE: &minimum})}
	// AddDocuments uses Qdrant's asynchronous upsert default. Wait for visibility.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		found, err := index.QueryWithFilter(ctx, "query", 1, filter)
		if err != nil {
			return err
		}
		if len(found) > 0 {
			if found[0].Content != "eligible" {
				return fmt.Errorf("unexpected filtered result %q", found[0].Content)
			}
			fmt.Printf("%s (score %.2f)\n", found[0].Content, found[0].Score)
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Fixed vectors isolate filtering behavior from model quality and network cost.
type fixtureEmbedder struct{}

func (fixtureEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for j, text := range texts {
		weight := float32(1)
		if text == "other tenant" {
			weight = 3
		}
		if text == "below threshold" {
			weight = 2
		}
		vectors[j] = []float32{weight, 0}
	}
	return vectors, nil
}
