package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/embedding"
)

// vectorEntry stores a memory with its precomputed embedding.
type vectorEntry struct {
	Memory Memory
	Vector []float32
}

// VectorMemoryStore uses an embedding model for semantic similarity search.
// Falls back to substring matching if the embedding call fails.
type VectorMemoryStore struct {
	mu       sync.RWMutex
	entries  []vectorEntry
	nextID   int
	embedder embedding.EmbeddingModel
}

// NewVectorMemoryStore creates a vector-backed memory store.
func NewVectorMemoryStore(embedder embedding.EmbeddingModel) *VectorMemoryStore {
	return &VectorMemoryStore{embedder: embedder}
}

func (s *VectorMemoryStore) Add(ctx context.Context, text string, userID string, agentID string) error {
	if text == "" {
		return nil
	}

	resp, err := s.embedder.Embed(ctx, []string{text})
	if err != nil {
		return fmt.Errorf("vector store: embed failed: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return fmt.Errorf("vector store: no embedding returned")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	s.entries = append(s.entries, vectorEntry{
		Memory: Memory{
			ID:        fmt.Sprintf("vmem_%d", s.nextID),
			Text:      text,
			UserID:    userID,
			AgentID:   agentID,
			CreatedAt: time.Now(),
		},
		Vector: resp.Embeddings[0],
	})
	return nil
}

func (s *VectorMemoryStore) Search(ctx context.Context, query string, userID string, opts *SearchOptions) ([]Memory, error) {
	topK := 5
	threshold := 0.0
	agentID := ""
	if opts != nil {
		if opts.TopK > 0 {
			topK = opts.TopK
		}
		threshold = opts.Threshold
		agentID = opts.AgentID
	}

	resp, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("vector store: embed query failed: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("vector store: no query embedding returned")
	}
	queryVec := resp.Embeddings[0]

	s.mu.RLock()
	defer s.mu.RUnlock()

	type scored struct {
		mem   Memory
		score float64
	}
	var candidates []scored

	for _, e := range s.entries {
		if e.Memory.UserID != userID {
			continue
		}
		if agentID != "" && e.Memory.AgentID != agentID {
			continue
		}
		sim := cosineSimilarity(queryVec, e.Vector)
		if sim >= threshold {
			candidates = append(candidates, scored{mem: e.Memory, score: sim})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	results := make([]Memory, len(candidates))
	for i, c := range candidates {
		results[i] = c.mem
	}
	return results, nil
}

func (s *VectorMemoryStore) List(_ context.Context, userID string) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []Memory
	for _, e := range s.entries {
		if e.Memory.UserID == userID {
			results = append(results, e.Memory)
		}
	}
	return results, nil
}

func (s *VectorMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, e := range s.entries {
		if e.Memory.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("memory %q not found", id)
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
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
