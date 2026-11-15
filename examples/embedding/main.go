package main

import (
	"context"
	"fmt"
	"math"
	"os"

	as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/embedding"
)

// This example demonstrates the embedding model system.
// It generates text embeddings and computes cosine similarity to find
// semantically related texts. Supports OpenAI, DashScope, Ollama, and Gemini.

func main() {
	as.Init()

	model, err := loadEmbeddingModelFromEnv()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Printf("Using embedding model: %s\n\n", model.ModelName())

	ctx := context.Background()

	// Embed a batch of texts.
	texts := []string{
		"The cat sat on the mat.",
		"A feline rested on a rug.",
		"The stock market crashed yesterday.",
		"Dogs are loyal companions.",
		"Financial markets experienced a downturn.",
	}

	resp, err := model.Embed(ctx, texts)
	if err != nil {
		fmt.Println("Embed error:", err)
		return
	}

	fmt.Printf("Embedded %d texts (dims=%d)\n", len(resp.Embeddings), len(resp.Embeddings[0]))
	if resp.Usage != nil {
		fmt.Printf("Usage: %.2fs, %d tokens\n", resp.Usage.Time, resp.Usage.Tokens)
	}
	fmt.Println()

	// Compute pairwise cosine similarity to show semantic relatedness.
	fmt.Println("=== Cosine Similarity Matrix ===")
	fmt.Printf("%-45s", "")
	for i := range texts {
		fmt.Printf("[%d]   ", i)
	}
	fmt.Println()

	for i, t := range texts {
		label := t
		if len(label) > 42 {
			label = label[:42] + "..."
		}
		fmt.Printf("[%d] %-42s", i, label)
		for j := range texts {
			sim := cosineSimilarity(resp.Embeddings[i], resp.Embeddings[j])
			fmt.Printf("%.3f ", sim)
		}
		fmt.Println()
	}

	// Find the most similar pair (excluding self).
	fmt.Println("\n=== Most Similar Pairs ===")
	query := texts[0]
	fmt.Printf("Query: %q\n", query)
	for i := 1; i < len(texts); i++ {
		sim := cosineSimilarity(resp.Embeddings[0], resp.Embeddings[i])
		fmt.Printf("  %.4f — %q\n", sim, texts[i])
	}

	// Demonstrate the AsEmbedder bridge for rag.Embedder interface.
	embedder := embedding.AsEmbedder(model)
	singleResp, err := embedder.Embed(ctx, []string{"test query"})
	if err != nil {
		fmt.Println("\nAsEmbedder error:", err)
		return
	}
	fmt.Printf("\nAsEmbedder bridge: embedded 1 text → %d dimensions\n", len(singleResp[0]))
}

func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func loadEmbeddingModelFromEnv() (embedding.EmbeddingModel, error) {
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return embedding.NewDashScopeEmbeddingModel(&embedding.OpenAICompatConfig{
			APIKey: key,
			Model:  "text-embedding-v3",
		})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return embedding.NewOpenAIEmbeddingModel(&embedding.OpenAICompatConfig{
			APIKey: key,
			Model:  "text-embedding-3-small",
		})
	}
	return nil, fmt.Errorf("set DASHSCOPE_API_KEY or OPENAI_API_KEY for embedding models")
}
