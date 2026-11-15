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
}
