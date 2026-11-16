package rag

import (
	"context"
	"testing"
)

func TestInMemoryIndexAddAndQuery(t *testing.T) {
	idx := NewInMemoryIndex()
	docs := []Document{
		{ID: "1", Content: "a"},
		{ID: "2", Content: "b"},
	}
	if err := idx.AddDocuments(context.Background(), docs); err != nil {
		t.Fatalf("AddDocuments: %v", err)
	}

	got, err := idx.Query(context.Background(), "ignored", 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(got))
	}
	if got[0].ID != "1" {
		t.Fatalf("unexpected first doc: %+v", got[0])
	}
}

func TestInMemoryIndexQueryRequiresPositiveTopK(t *testing.T) {
	ctx := context.Background()
	idx := NewInMemoryIndex()
	if docs, err := idx.Query(ctx, "", 1); err != nil || len(docs) != 0 {
		t.Fatalf("empty index: docs=%v err=%v", docs, err)
	}
	if err := idx.AddDocuments(ctx, []Document{{ID: "one"}}); err != nil {
		t.Fatal(err)
	}
	for _, topK := range []int{0, -1} {
		if docs, err := idx.Query(ctx, "", topK); err == nil || docs != nil {
			t.Errorf("topK=%d: docs=%v err=%v, want error and no documents", topK, docs, err)
		}
	}
	if docs, err := idx.Query(ctx, "", 2); err != nil || len(docs) != 1 || docs[0].ID != "one" {
		t.Fatalf("oversized positive topK: docs=%v err=%v", docs, err)
	}
}

func TestSimpleKnowledgeBase(t *testing.T) {
	idx := NewInMemoryIndex()
	_ = idx.AddDocuments(context.Background(), []Document{
		{ID: "1", Content: "hello"},
	})
	kb := NewSimpleKnowledgeBase("kb", idx)

	if kb.Name() != "kb" {
		t.Fatalf("unexpected name: %s", kb.Name())
	}
	docs, err := kb.Query(context.Background(), "x", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "1" {
		t.Fatalf("unexpected docs: %+v", docs)
	}
	if docs, err := kb.Query(context.Background(), "x", 0); err == nil || docs != nil {
		t.Fatalf("zero topK: docs=%v err=%v; want an error and no documents", docs, err)
	}
}

type recordingQueryIndex struct {
	calls int
	query string
	topK  int
}

func (*recordingQueryIndex) AddDocuments(context.Context, []Document) error { return nil }

func (i *recordingQueryIndex) Query(_ context.Context, query string, topK int) ([]Document, error) {
	i.calls++
	i.query, i.topK = query, topK
	return []Document{{ID: "match"}}, nil
}

func TestSimpleKnowledgeBaseQueryTopK(t *testing.T) {
	for _, tt := range []struct {
		name string
		topK int
	}{
		{"zero", 0},
		{"negative", -1},
		{"positive", 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			idx := &recordingQueryIndex{}
			kb := NewSimpleKnowledgeBase("kb", idx)
			docs, err := kb.Query(context.Background(), "query", tt.topK)
			if tt.topK <= 0 {
				if err == nil || docs != nil || idx.calls != 0 {
					t.Fatalf("docs=%v err=%v backend calls=%d; want an error before querying the index", docs, err, idx.calls)
				}
				return
			}
			if err != nil || len(docs) != 1 || docs[0].ID != "match" {
				t.Fatalf("docs=%v err=%v; want the index result", docs, err)
			}
			if idx.calls != 1 || idx.query != "query" || idx.topK != tt.topK {
				t.Fatalf("backend calls=%d query=%q topK=%d; want one call with the original arguments", idx.calls, idx.query, idx.topK)
			}
		})
	}
}
