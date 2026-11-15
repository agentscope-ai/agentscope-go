package rag

import (
	"context"
	"fmt"

	"github.com/qdrant/go-client/qdrant"
)

// QueryVector searches a collection with a precomputed nonempty finite vector.
// Filtering runs before topK in Qdrant. topK <= 0 defaults to 10. The configured
// RequiredFilter is always ANDed with filter, including when filter is nil.
func (i *QdrantIndex) QueryVector(ctx context.Context, vector []float32, topK int, filter MetadataFilter) ([]Document, error) {
	wireFilter, err := i.queryFilter(filter)
	if err != nil {
		return nil, err
	}
	return i.queryVector(ctx, vector, topK, wireFilter)
}
func (i *QdrantIndex) queryVector(ctx context.Context, vector []float32, topK int, filter *qdrant.Filter) ([]Document, error) {
	if len(vector) == 0 {
		return nil, fmt.Errorf("qdrant: query vector must not be empty")
	}
	for _, v := range vector {
		if !finite(float64(v)) {
			return nil, fmt.Errorf("qdrant: query vector must be finite")
		}
	}
	if topK <= 0 {
		topK = 10
	}
	scoreSign, err := i.collectionScoreSign(ctx)
	if err != nil {
		return nil, err
	}
	limit := uint64(topK)
	points, err := i.client.Query(ctx, &qdrant.QueryPoints{CollectionName: i.collection, Query: qdrant.NewQuery(vector...), Limit: &limit, Filter: filter, WithPayload: qdrant.NewWithPayload(true)})
	if err != nil {
		return nil, fmt.Errorf("qdrant: query points: %w", err)
	}
	out := make([]Document, 0, len(points))
	for _, p := range points {
		meta := make(map[string]any, len(p.Payload))
		for k, v := range p.Payload {
			meta[k] = v
		}
		doc := Document{ID: p.GetId().GetUuid(), Meta: meta, Score: scoreSign * float64(p.GetScore())}
		if value := p.Payload["content"]; value != nil {
			doc.Content = value.GetStringValue()
		}
		out = append(out, doc)
	}
	return out, nil
}

// collectionScoreSign makes scores higher-is-better for the unnamed dense
// vector used by queryVector. Read the collection with each query so the score
// reflects the server's configuration.
func (i *QdrantIndex) collectionScoreSign(ctx context.Context) (float64, error) {
	info, err := i.client.GetCollectionInfo(ctx, i.collection)
	if err != nil {
		return 0, fmt.Errorf("qdrant: get collection %q: %w", i.collection, err)
	}
	vectors := info.GetConfig().GetParams().GetVectorsConfig()
	params := vectors.GetParams()
	if params == nil {
		params = vectors.GetParamsMap().GetMap()[""]
	}
	if params == nil {
		return 0, fmt.Errorf("qdrant: collection %q has no unnamed dense vector", i.collection)
	}
	switch metric := params.GetDistance(); metric {
	case qdrant.Distance_Euclid, qdrant.Distance_Manhattan:
		return -1, nil
	case qdrant.Distance_Cosine, qdrant.Distance_Dot:
		return 1, nil
	default:
		return 0, fmt.Errorf("qdrant: collection %q has unsupported distance metric %v", i.collection, metric)
	}
}
