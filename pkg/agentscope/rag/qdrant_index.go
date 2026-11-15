package rag

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
	"github.com/sirupsen/logrus"
)

// QdrantIndex is an Index implementation backed by a Qdrant collection.
// It assumes that embeddings are provided externally and passed in via
// the Document's Meta under a configurable key.
type QdrantIndex struct {
	client         *qdrant.Client
	collection     string
	vectorMetaKey  string
	requiredFilter MetadataFilter
}

// QdrantConfig configures a QdrantIndex.
type QdrantConfig struct {
	// RequiredFilter is copied at construction and ANDed with every vector query.
	RequiredFilter MetadataFilter
	Client         *qdrant.Client
	Collection     string
	VectorMetaKey  string // key in Document.Meta where []float32 vector is stored
}

// NewQdrantIndex constructs a Qdrant-backed Index.
func NewQdrantIndex(cfg QdrantConfig) (*QdrantIndex, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("qdrant: client is required")
	}
	if cfg.Collection == "" {
		return nil, fmt.Errorf("qdrant: collection is required")
	}
	key := cfg.VectorMetaKey
	if key == "" {
		key = "vector"
	}
	index := &QdrantIndex{
		client:         cfg.Client,
		collection:     cfg.Collection,
		vectorMetaKey:  key,
		requiredFilter: append(MetadataFilter(nil), cfg.RequiredFilter...),
	}
	if _, err := index.queryFilter(nil); err != nil {
		return nil, err
	}
	return index, nil
}

// AddDocuments upserts documents into the Qdrant collection.
// It expects a []float32 vector in doc.Meta[vectorMetaKey].
func (i *QdrantIndex) AddDocuments(ctx context.Context, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}

	points := make([]*qdrant.PointStruct, 0, len(docs))
	for _, d := range docs {
		rawVec, ok := d.Meta[i.vectorMetaKey]
		if !ok {
			return fmt.Errorf("qdrant: document %s is missing vector in meta[%s]", d.ID, i.vectorMetaKey)
		}
		vec, ok := rawVec.([]float32)
		if !ok {
			return fmt.Errorf("qdrant: document %s meta[%s] must be []float32", d.ID, i.vectorMetaKey)
		}

		if len(vec) == 0 {
			return fmt.Errorf("qdrant: document %s has an empty vector", d.ID)
		}
		for _, value := range vec {
			if !finite(float64(value)) {
				return fmt.Errorf("qdrant: document %s has a nonfinite vector", d.ID)
			}
		}
		payload := map[string]any{}
		// Vectors belong in the native vector field; []float32 is not a
		// supported payload value. Preserve other metadata, including content.
		for k, v := range d.Meta {
			if k != i.vectorMetaKey {
				payload[k] = v
			}
		}
		encoded, err := qdrant.TryValueMap(payload)
		if err != nil {
			return fmt.Errorf("qdrant: document %s metadata: %w", d.ID, err)
		}

		points = append(points, &qdrant.PointStruct{
			Id: &qdrant.PointId{PointIdOptions: &qdrant.PointId_Uuid{Uuid: d.ID}},
			// Vector.Data is deprecated upstream; dense vectors go through
			// the Vector_Dense oneof (qdrant points.proto).
			Vectors: &qdrant.Vectors{VectorsOptions: &qdrant.Vectors_Vector{Vector: &qdrant.Vector{Vector: &qdrant.Vector_Dense{Dense: &qdrant.DenseVector{Data: vec}}}}},
			Payload: encoded,
		})
	}

	res, err := i.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: i.collection,
		Points:         points,
	})
	if err != nil {
		return fmt.Errorf("qdrant: upsert points: %w", err)
	}
	logrus.WithFields(logrus.Fields{
		"collection": i.collection,
		"count":      len(docs),
		"status":     res.GetStatus().String(),
	}).Info("qdrant: documents upserted")
	return nil
}

// Query cannot embed text. Use QueryVector with precomputed vectors, or
// QdrantTextIndex for the Index text-query interface.
func (i *QdrantIndex) Query(ctx context.Context, query string, topK int) ([]Document, error) {
	_ = query // semantic search is driven by the vector, not raw text, in this implementation.
	_ = topK  // topK would be used in a full implementation
	return nil, fmt.Errorf("qdrant: Query requires a vector-based implementation; use QueryVector or QdrantTextIndex")
}
