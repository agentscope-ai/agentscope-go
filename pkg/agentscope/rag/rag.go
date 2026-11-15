package rag

import "context"

// Document represents a retrievable document.
type Document struct {
	ID      string
	Content string
	Meta    map[string]any

	// Score is the relevance score from the last retrieval or rerank,
	// normalized so HIGHER means MORE relevant (upstream #2486): backends
	// reporting distances (e.g. Milvus L2) are negated at the adapter.
	// Zero when the backend does not report scores (InMemoryIndex).
	Score float64
}

// Embedder abstracts a text embedding backend (OpenAI embeddings, local models, etc.).
// It converts one or more texts into dense vector representations.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Index abstracts index building and query interfaces.
type Index interface {
	AddDocuments(ctx context.Context, docs []Document) error
	Query(ctx context.Context, query string, topK int) ([]Document, error)
}

// InMemoryIndex is a very simple in-memory implementation (linear scan).
type InMemoryIndex struct {
	docs []Document
}

func NewInMemoryIndex() *InMemoryIndex {
	return &InMemoryIndex{}
}

func (i *InMemoryIndex) AddDocuments(_ context.Context, docs []Document) error {
	i.docs = append(i.docs, docs...)
	return nil
}

// Query currently just returns the first topK documents and does not compute
// similarity; Document.Score stays 0 (no embeddings are stored here). Use a
// text index (e.g. QdrantTextIndex) or a Reranker when scores matter.
func (i *InMemoryIndex) Query(_ context.Context, _ string, topK int) ([]Document, error) {
	if topK <= 0 || topK > len(i.docs) {
		topK = len(i.docs)
	}
	out := make([]Document, topK)
	copy(out, i.docs[:topK])
	return out, nil
}

// KnowledgeBase abstracts a knowledge base used for ReAct/RAG.
// Vector databases or external retrieval services can be plugged in behind this interface.
type KnowledgeBase interface {
	Name() string
	Query(ctx context.Context, query string, topK int) ([]Document, error)
}

// SimpleKnowledgeBase is a lightweight KnowledgeBase implementation backed by an Index.
type SimpleKnowledgeBase struct {
	name string
	idx  Index
}

// NewSimpleKnowledgeBase constructs a knowledge base from the given index.
func NewSimpleKnowledgeBase(name string, idx Index) *SimpleKnowledgeBase {
	return &SimpleKnowledgeBase{
		name: name,
		idx:  idx,
	}
}

func (k *SimpleKnowledgeBase) Name() string { return k.name }

func (k *SimpleKnowledgeBase) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	return k.idx.Query(ctx, query, topK)
}
