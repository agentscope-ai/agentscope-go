package rag

import "context"

// ScoredDocument is a Document annotated with a relevance score from a reranker.
type ScoredDocument struct {
	Document Document
	Score    float64
}

// Reranker reorders a set of candidate documents by relevance to a query.
// Typical implementations call a cross-encoder model (Cohere Rerank, Jina
// Reranker, BGE Reranker, etc.) or a local model to compute query-document
// relevance scores.
//
// The Reranker sits between initial retrieval (Index.Query) and the final
// context assembly, improving precision by discarding low-relevance documents
// that passed the vector similarity threshold.
type Reranker interface {
	// Rerank scores and reorders docs by relevance to query, returning at
	// most topN results sorted by descending score. Implementations must
	// not mutate the input slice.
	Rerank(ctx context.Context, query string, docs []Document, topN int) ([]ScoredDocument, error)
}

// RerankedIndex wraps an Index and applies a Reranker to Query results.
// This provides a drop-in upgrade path: replace an Index with a
// RerankedIndex to improve retrieval precision without changing callers.
type RerankedIndex struct {
	base     Index
	reranker Reranker
	// FetchMultiplier controls how many documents are fetched from the base
	// index before reranking. The base index is queried for topN * FetchMultiplier
	// documents. Default: 3.
	FetchMultiplier int
}

// NewRerankedIndex creates an Index that fetches extra candidates from base
// then reranks them. If multiplier <= 0 it defaults to 3.
func NewRerankedIndex(base Index, reranker Reranker, multiplier int) *RerankedIndex {
	if multiplier <= 0 {
		multiplier = 3
	}
	return &RerankedIndex{
		base:            base,
		reranker:        reranker,
		FetchMultiplier: multiplier,
	}
}

func (r *RerankedIndex) AddDocuments(ctx context.Context, docs []Document) error {
	return r.base.AddDocuments(ctx, docs)
}

func (r *RerankedIndex) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	// Fetch more candidates than needed so the reranker has room to reorder.
	fetchN := topK * r.FetchMultiplier
	candidates, err := r.base.Query(ctx, query, fetchN)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	scored, err := r.reranker.Rerank(ctx, query, candidates, topK)
	if err != nil {
		return nil, err
	}

	out := make([]Document, 0, len(scored))
	for _, sd := range scored {
		doc := sd.Document
		// Carry the rerank score onto the Document so Query callers see the
		// same higher-is-more-relevant number the reranker produced
		// (upstream #2486; keeps one score model across the pipeline).
		doc.Score = sd.Score
		out = append(out, doc)
	}
	return out, nil
}
