package rag

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// QdrantTextIndex is a higher-level Index implementation that:
//   - uses an Embedder to embed document contents and text queries
//   - persists vectors and payloads into a Qdrant collection
//   - performs vector similarity search for text queries.
//
// It builds on top of QdrantIndex but hides the vector plumbing from callers.
type QdrantTextIndex struct {
	qdrant   *QdrantIndex
	embedder Embedder
}

// QdrantTextConfig configures a QdrantTextIndex.
type QdrantTextConfig struct {
	// RequiredFilter is copied and ANDed with every query filter.
	RequiredFilter MetadataFilter
	Client         *qdrant.Client
	Collection     string
	VectorMetaKey  string
	Embedder       Embedder
}

// NewQdrantTextIndex constructs a QdrantTextIndex with an Embedder.
func NewQdrantTextIndex(cfg QdrantTextConfig) (*QdrantTextIndex, error) { //nolint:gocritic // preserve the existing value-config API
	if cfg.Embedder == nil {
		return nil, fmt.Errorf("qdrant: embedder is required for QdrantTextIndex")
	}

	base, err := NewQdrantIndex(QdrantConfig{
		Client:         cfg.Client,
		RequiredFilter: cfg.RequiredFilter,
		Collection:     cfg.Collection,
		VectorMetaKey:  cfg.VectorMetaKey,
	})
	if err != nil {
		return nil, err
	}
	return &QdrantTextIndex{
		qdrant:   base,
		embedder: cfg.Embedder,
	}, nil
}

// AddDocuments embeds each document's Content and stores both the vector and payload in Qdrant.
func (i *QdrantTextIndex) AddDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}

	texts := make([]string, len(docs))
	for idx, d := range docs {
		texts[idx] = d.Content
	}

	vectors, err := i.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("qdrant: embed documents: %w", err)
	}
	if len(vectors) != len(docs) {
		return fmt.Errorf("qdrant: embedder returned %d vectors for %d docs", len(vectors), len(docs))
	}

	for idx := range docs {
		if docs[idx].Meta == nil {
			docs[idx].Meta = make(map[string]any)
		}
		docs[idx].Meta[i.qdrant.vectorMetaKey] = vectors[idx]
		// Also store the original content into payload for retrieval.
		docs[idx].Meta["content"] = docs[idx].Content
	}

	return i.qdrant.AddDocuments(ctx, docs)
}

// Query embeds the input text and performs a vector similarity search in Qdrant.
func (i *QdrantTextIndex) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	return i.QueryWithFilter(ctx, query, topK, nil)
}

// QueryWithFilter embeds query and applies typed metadata conditions in Qdrant
// before selecting topK. Caller conditions are ANDed with RequiredFilter; they
// cannot replace it. Invalid filters fail before the embedding request.
func (i *QdrantTextIndex) QueryWithFilter(ctx context.Context, query string, topK int, filter MetadataFilter) ([]Document, error) {
	wireFilter, err := i.qdrant.queryFilter(filter)
	if err != nil {
		return nil, err
	}
	vecs, err := i.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("qdrant: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("qdrant: embedder returned %d vectors for one query", len(vecs))
	}
	return i.qdrant.queryVector(ctx, vecs[0], topK, wireFilter)
}
