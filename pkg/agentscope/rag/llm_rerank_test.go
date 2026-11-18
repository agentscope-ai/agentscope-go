package rag

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

type rerankJudgeModel struct {
	calls  int64
	scores string // JSON for the structured output tool call
}

func (m *rerankJudgeModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	atomic.AddInt64(&m.calls, 1)
	return &model.ChatResponse{
		Content: []message.ContentBlock{message.ToolCallBlock{
			Type: "tool_call", ID: "rr1", Name: "generate_structured_output",
			Input: m.scores, State: message.ToolCallPending,
		}},
		IsLast: true,
	}, nil
}

func (m *rerankJudgeModel) ChatStream(context.Context, []*message.Msg, ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (m *rerankJudgeModel) CountTokens([]*message.Msg, []model.ToolSchema) int { return 1 }

// Upstream #1975: LLM-driven reranking over the existing Reranker interface.
func TestLLMRerankerScoresSortsAndCaps(t *testing.T) {
	judge := &rerankJudgeModel{scores: `{"scores":[{"index":0,"score":0.1},{"index":1,"score":0.9},{"index":2,"score":0.5}]}`}
	r := NewLLMReranker(judge)
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
		{ID: "c", Content: "gamma"},
	}
	out, err := r.Rerank(context.Background(), "q", docs, 2)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("topN cap: got %d, want 2", len(out))
	}
	if out[0].Document.ID != "b" || out[0].Score != 0.9 {
		t.Errorf("out[0] = %s/%v, want b/0.9 (descending)", out[0].Document.ID, out[0].Score)
	}
	if out[1].Document.ID != "c" || out[1].Score != 0.5 {
		t.Errorf("out[1] = %s/%v, want c/0.5", out[1].Document.ID, out[1].Score)
	}
	if atomic.LoadInt64(&judge.calls) != 1 {
		t.Errorf("judge calls = %d, want 1 batched call", judge.calls)
	}
}

func TestLLMRerankerCachesScores(t *testing.T) {
	judge := &rerankJudgeModel{scores: `{"scores":[{"index":0,"score":0.4}]}`}
	r := NewLLMReranker(judge)
	docs := []Document{{ID: "a", Content: "alpha"}}
	for i := 0; i < 3; i++ {
		if _, err := r.Rerank(context.Background(), "q", docs, 5); err != nil {
			t.Fatalf("rerank %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&judge.calls); got != 1 {
		t.Errorf("judge calls = %d, want 1 (cached reranks must not re-burn tokens)", got)
	}
	// A different query re-judges.
	if _, err := r.Rerank(context.Background(), "other", docs, 5); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&judge.calls); got != 2 {
		t.Errorf("judge calls = %d, want 2 after a new query", got)
	}
}

func TestLLMRerankerCacheMaxBoundsBatchWrites(t *testing.T) {
	judge := &rerankJudgeModel{scores: `{"scores":[{"index":0,"score":0.1},{"index":1,"score":0.9},{"index":2,"score":0.3},{"index":3,"score":0.7}]}`}
	r := NewLLMReranker(judge, WithLLMRerankerCacheMax(2))
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
		{ID: "c", Content: "gamma"},
		{ID: "d", Content: "delta"},
	}

	out, err := r.Rerank(context.Background(), "q", docs, len(docs))
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != len(docs) {
		t.Fatalf("rerank returned %d documents, want %d", len(out), len(docs))
	}
	want := []struct {
		id    string
		score float64
	}{
		{id: "b", score: 0.9},
		{id: "d", score: 0.7},
		{id: "c", score: 0.3},
		{id: "a", score: 0.1},
	}
	for i, want := range want {
		if out[i].Document.ID != want.id || out[i].Score != want.score {
			t.Fatalf("out[%d] = %s/%v, want %s/%v", i, out[i].Document.ID, out[i].Score, want.id, want.score)
		}
	}
	if got := len(r.cache); got > 2 {
		t.Fatalf("cache has %d entries; configured capacity is 2", got)
	}
}

func TestLLMRerankerCacheMaxBoundsBatchAfterOverflow(t *testing.T) {
	judge := &rerankJudgeModel{scores: `{"scores":[{"index":0,"score":0.1},{"index":1,"score":0.9},{"index":2,"score":0.3}]}`}
	r := NewLLMReranker(judge, WithLLMRerankerCacheMax(2))
	r.cache["old-a"] = 0.2
	r.cache["old-b"] = 0.4
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
		{ID: "c", Content: "gamma"},
	}

	out, err := r.Rerank(context.Background(), "q", docs, len(docs))
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != len(docs) {
		t.Fatalf("rerank returned %d documents, want %d", len(out), len(docs))
	}
	want := []struct {
		id    string
		score float64
	}{
		{id: "b", score: 0.9},
		{id: "c", score: 0.3},
		{id: "a", score: 0.1},
	}
	for i, want := range want {
		if out[i].Document.ID != want.id || out[i].Score != want.score {
			t.Fatalf("out[%d] = %s/%v, want %s/%v", i, out[i].Document.ID, out[i].Score, want.id, want.score)
		}
	}
	if got := len(r.cache); got > 2 {
		t.Fatalf("cache has %d entries after overflow; configured capacity is 2", got)
	}
	if _, ok := r.cache["old-a"]; ok {
		t.Fatal("overflow should clear the old cache entries")
	}
}

func TestLLMRerankerCacheMaxUsesRemainingCapacity(t *testing.T) {
	judge := &rerankJudgeModel{scores: `{"scores":[{"index":0,"score":0.9},{"index":1,"score":0.1}]}`}
	r := NewLLMReranker(judge, WithLLMRerankerCacheMax(2))
	docs := []Document{
		{ID: "cached", Content: "already scored"},
		{ID: "b", Content: "beta"},
		{ID: "c", Content: "gamma"},
	}
	r.cache[llmRerankCacheKey("q", docs[0])] = 0.5

	out, err := r.Rerank(context.Background(), "q", docs, len(docs))
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	want := []struct {
		id    string
		score float64
	}{
		{id: "b", score: 0.9},
		{id: "cached", score: 0.5},
		{id: "c", score: 0.1},
	}
	if len(out) != len(want) {
		t.Fatalf("rerank returned %d documents, want %d", len(out), len(want))
	}
	for i, want := range want {
		if out[i].Document.ID != want.id || out[i].Score != want.score {
			t.Fatalf("out[%d] = %s/%v, want %s/%v", i, out[i].Document.ID, out[i].Score, want.id, want.score)
		}
	}
	if got := len(r.cache); got != 2 {
		t.Fatalf("cache has %d entries, want capacity 2", got)
	}
	if _, ok := r.cache[llmRerankCacheKey("q", docs[1])]; !ok {
		t.Fatal("first uncached document was not admitted into the remaining slot")
	}
	if _, ok := r.cache[llmRerankCacheKey("q", docs[2])]; ok {
		t.Fatal("second uncached document should not exceed the remaining capacity")
	}
	if got := atomic.LoadInt64(&judge.calls); got != 1 {
		t.Fatalf("judge calls = %d, want 1", got)
	}
}

func TestLLMRerankerClampsAndDefaults(t *testing.T) {
	judge := &rerankJudgeModel{scores: `{"scores":[{"index":0,"score":7},{"index":99,"score":0.5}]}`}
	r := NewLLMReranker(judge)
	out, err := r.Rerank(context.Background(), "q", []Document{{ID: "a", Content: "x"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Score != 1 {
		t.Errorf("out-of-range score must clamp to 1, got %v", out[0].Score)
	}
	// Missing judge entries default to 0.
	judge2 := &rerankJudgeModel{scores: `{"scores":[]}`}
	r2 := NewLLMReranker(judge2)
	out2, err := r2.Rerank(context.Background(), "q", []Document{{ID: "a", Content: "x"}, {ID: "b", Content: "y"}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, sd := range out2 {
		if sd.Score != 0 {
			t.Errorf("unscored doc %s = %v, want 0", sd.Document.ID, sd.Score)
		}
	}
}

func TestLLMRerankerEmptyInput(t *testing.T) {
	r := NewLLMReranker(&rerankJudgeModel{scores: `{}`})
	out, err := r.Rerank(context.Background(), "q", nil, 3)
	if err != nil || out != nil {
		t.Errorf("empty input: out=%v err=%v", out, err)
	}
	_ = fmt.Sprint()
}
