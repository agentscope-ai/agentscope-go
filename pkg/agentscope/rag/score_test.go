package rag

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

type scoreTestEmbedder struct{}

func (scoreTestEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2, 0.3, 0.4}
	}
	return out, nil
}

// Upstream #2486: ES _score must land on Document.Score (higher = better).
func TestElasticsearchQueryPopulatesScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"hits":{"hits":[
			{"_id":"e1","_score":1.87,"_source":{"doc_id":"d1","content":"near","meta":{"k":"v"}}},
			{"_id":"e2","_score":1.02,"_source":{"doc_id":"d2","content":"far"}}
		]}}`)
	}))
	defer srv.Close()

	idx, err := NewElasticsearchIndex(&ElasticsearchConfig{
		Addresses: []string{srv.URL}, IndexName: "docs", Dims: 4,
	}, scoreTestEmbedder{})
	if err != nil {
		t.Fatalf("new index: %v", err)
	}
	docs, err := idx.Query(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d", len(docs))
	}
	if docs[0].Score != 1.87 || docs[1].Score != 1.02 {
		t.Errorf("scores = %v/%v, want 1.87/1.02 from _score", docs[0].Score, docs[1].Score)
	}
}

// Milvus L2 reports a distance (lower = closer) and must be negated;
// IP/COSINE pass through (upstream #2486 normalization).
func TestMilvusNormalizeScore(t *testing.T) {
	l2 := &MilvusIndex{cfg: MilvusConfig{MetricType: "L2"}}
	if got := l2.normalizeScore(0.25); got != -0.25 {
		t.Errorf("L2 normalize(0.25) = %v, want -0.25", got)
	}
	cos := &MilvusIndex{cfg: MilvusConfig{MetricType: "COSINE"}}
	if got := cos.normalizeScore(0.8); got != 0.8 {
		t.Errorf("COSINE normalize(0.8) = %v, want 0.8", got)
	}
	ip := &MilvusIndex{cfg: MilvusConfig{MetricType: ""}}
	if got := ip.normalizeScore(0.6); got != 0.6 {
		t.Errorf("default normalize(0.6) = %v, want 0.6", got)
	}
}

// The rerank score must survive RerankedIndex.Query on Document.Score —
// one score model across the pipeline (evaluator finding on #2486).
func TestRerankedIndexCarriesScore(t *testing.T) {
	base := &stubScoreIndex{docs: []Document{{ID: "a", Content: "x", Score: 0.1}, {ID: "b", Content: "y", Score: 0.2}}}
	rr := &stubReranker{}
	idx := NewRerankedIndex(base, rr, 2)
	out, err := idx.Query(context.Background(), "q", 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %d", len(out))
	}
	// stubReranker reverses order with scores 0.9/0.8.
	if out[0].ID != "b" || out[0].Score != 0.9 {
		t.Errorf("out[0] = %s/%v, want b/0.9 (rerank score, not base score)", out[0].ID, out[0].Score)
	}
	if out[1].ID != "a" || out[1].Score != 0.8 {
		t.Errorf("out[1] = %s/%v, want a/0.8", out[1].ID, out[1].Score)
	}
}

type stubScoreIndex struct{ docs []Document }

func (s *stubScoreIndex) AddDocuments(_ context.Context, docs []Document) error {
	s.docs = append(s.docs, docs...)
	return nil
}
func (s *stubScoreIndex) Query(_ context.Context, _ string, topK int) ([]Document, error) {
	if topK > 0 && topK < len(s.docs) {
		return s.docs[:topK], nil
	}
	return s.docs, nil
}

type stubReranker struct{}

func (stubReranker) Rerank(_ context.Context, _ string, docs []Document, topN int) ([]ScoredDocument, error) {
	out := make([]ScoredDocument, 0, len(docs))
	for i := len(docs) - 1; i >= 0; i-- {
		out = append(out, ScoredDocument{Document: docs[i], Score: 0.9 - 0.1*float64(len(out))})
	}
	if topN > 0 && topN < len(out) {
		out = out[:topN]
	}
	return out, nil
}
