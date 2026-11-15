package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// LLMReranker scores query-document relevance with a ChatModel instead of a
// cross-encoder service (upstream #1975). The model is shown the query and a
// numbered candidate list and must return a structured 0..1 score per
// candidate; results are cached per (query, document) so repeated queries in
// one session do not re-burn tokens. Zero new dependencies: it rides the
// existing model.ChatModel + GenerateStructuredOutput machinery.
//
// Trade-offs versus a real cross-encoder: higher latency and token cost per
// uncached batch, and scores are only as calibrated as the judge model —
// fine for precision-oriented two-stage retrieval over small candidate sets
// (RerankedIndex's FetchMultiplier keeps those small by default).
type LLMReranker struct {
	model          model.ChatModel
	maxDocChars    int
	maxPromptRunes int
	cacheMax       int
	prompt         string

	mu    sync.Mutex
	cache map[string]float64
}

// LLMRerankerOption configures NewLLMReranker.
type LLMRerankerOption func(*LLMReranker)

// WithLLMRerankerDocChars caps how many characters of each document enter
// the judge prompt (default 512). Longer documents are truncated.
func WithLLMRerankerDocChars(n int) LLMRerankerOption {
	return func(r *LLMReranker) {
		if n > 0 {
			r.maxDocChars = n
		}
	}
}

// WithLLMRerankerPromptRunes caps the whole judge prompt in runes (default
// 24000, roughly 6k tokens). The per-document budget is reduced as needed so
// every candidate still enters the call — dropping candidates silently would
// leave them scored 0 and sink them in the ranking.
func WithLLMRerankerPromptRunes(n int) LLMRerankerOption {
	return func(r *LLMReranker) {
		if n > 0 {
			r.maxPromptRunes = n
		}
	}
}

// WithLLMRerankerCacheMax sets the score-cache capacity (default 512
// entries). Overflow drops the whole cache — rerank scores are advisory and
// recomputing is always safe.
func WithLLMRerankerCacheMax(n int) LLMRerankerOption {
	return func(r *LLMReranker) {
		if n > 0 {
			r.cacheMax = n
		}
	}
}

// WithLLMRerankerPrompt overrides the judge instruction (default asks for
// 0..1 relevance scores).
func WithLLMRerankerPrompt(p string) LLMRerankerOption {
	return func(r *LLMReranker) {
		if strings.TrimSpace(p) != "" {
			r.prompt = p
		}
	}
}

const defaultLLMRerankPrompt = "You are a relevance judge for a retrieval pipeline. Score how relevant each candidate document is to the query, from 0 (irrelevant) to 1 (directly answers the query). Judge each candidate independently; do not compare candidates against each other."

// NewLLMReranker creates a Reranker backed by the given chat model.
func NewLLMReranker(m model.ChatModel, opts ...LLMRerankerOption) *LLMReranker {
	r := &LLMReranker{
		model:          m,
		maxDocChars:    512,
		maxPromptRunes: 24000,
		cacheMax:       512,
		prompt:         defaultLLMRerankPrompt,
		cache:          map[string]float64{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

var llmRerankSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"scores": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"index": {"type": "integer", "description": "0-based candidate index"},
					"score": {"type": "number", "description": "relevance from 0 to 1"}
				},
				"required": ["index", "score"]
			}
		}
	},
	"required": ["scores"]
}`)

// Rerank implements Reranker. Cached documents are scored without a model
// call; the remaining candidates are judged in ONE batched call. Input is
// never mutated; output is sorted by descending score and capped at topN.
func (r *LLMReranker) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]ScoredDocument, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}

	scores := make([]float64, len(docs))
	needJudge := make([]int, 0, len(docs))
	for i, d := range docs {
		key := llmRerankCacheKey(query, d)
		r.mu.Lock()
		s, ok := r.cache[key]
		r.mu.Unlock()
		if ok {
			scores[i] = s
		} else {
			needJudge = append(needJudge, i)
		}
	}

	if len(needJudge) > 0 {
		judged, err := r.judge(ctx, query, docs, needJudge)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		if len(r.cache) >= r.cacheMax {
			r.cache = map[string]float64{}
		}
		for pos, idx := range needJudge {
			scores[idx] = judged[pos]
			r.cache[llmRerankCacheKey(query, docs[idx])] = judged[pos]
		}
		r.mu.Unlock()
	}

	out := make([]ScoredDocument, 0, len(docs))
	for i, d := range docs {
		out = append(out, ScoredDocument{Document: d, Score: scores[i]})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

// truncateRunes cuts s to at most n runes, appending an ellipsis when it cut.
// Slicing by bytes would split a multi-byte character in half and emit invalid
// UTF-8 into the prompt; for CJK text a byte budget also holds far fewer
// characters than an option named "DocChars" suggests.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	seen := 0
	for i := range s {
		if seen == n {
			return s[:i] + "…"
		}
		seen++
	}
	return s
}

// judge scores the candidates at the given doc indexes in one model call.
func (r *LLMReranker) judge(ctx context.Context, query string, docs []Document, idxs []int) ([]float64, error) {
	var sb strings.Builder
	sb.WriteString(r.prompt)
	sb.WriteString("\n\nQuery: ")
	sb.WriteString(query)
	sb.WriteString("\n\nCandidates:\n")
	// Share the prompt budget across candidates so the whole call stays
	// bounded regardless of how many documents RerankedIndex handed over.
	docBudget := r.maxDocChars
	if r.maxPromptRunes > 0 && len(idxs) > 0 {
		overhead := utf8.RuneCountInString(r.prompt) + utf8.RuneCountInString(query) + 8*len(idxs)
		perDoc := (r.maxPromptRunes - overhead) / len(idxs)
		if perDoc < 1 {
			perDoc = 1
		}
		if docBudget <= 0 || perDoc < docBudget {
			docBudget = perDoc
		}
	}

	for pos, di := range idxs {
		content := truncateRunes(docs[di].Content, docBudget)
		sb.WriteString("[")
		sb.WriteString(strconv.Itoa(pos))
		sb.WriteString("] ")
		sb.WriteString(content)
		sb.WriteString("\n")
	}

	raw, err := model.GenerateStructuredOutput(ctx, r.model,
		[]*message.Msg{message.UserMsg("user", sb.String())}, llmRerankSchema)
	if err != nil {
		return nil, fmt.Errorf("llm rerank: %w", err)
	}

	var parsed struct {
		Scores []struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		} `json:"scores"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("llm rerank: parse judge result: %w", err)
	}

	out := make([]float64, len(idxs))
	for _, s := range parsed.Scores {
		if s.Index < 0 || s.Index >= len(idxs) {
			continue
		}
		score := s.Score
		if score < 0 {
			score = 0
		} else if score > 1 {
			score = 1
		}
		out[s.Index] = score
	}
	return out, nil
}

func llmRerankCacheKey(query string, d Document) string {
	sum := sha256.Sum256([]byte(query + "\x00" + d.ID + "\x00" + d.Content))
	return hex.EncodeToString(sum[:16])
}
