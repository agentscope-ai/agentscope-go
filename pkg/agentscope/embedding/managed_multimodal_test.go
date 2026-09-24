package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func managedEmbeddingFixture(t *testing.T, server *httptest.Server, api string, base EmbeddingModel) (ManagedEmbeddingModel, *inference.Ledger) {
	t.Helper()
	ledger := inference.NewLedger(100)
	c := inference.NewController(ledger)
	t.Cleanup(c.Close)
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 2, MaxQueued: 4, MaxQueuedWork: 100}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "d", Provider: "fixture", API: api, Model: base.ModelName(), Endpoint: server.URL}, PoolID: "p", MaxAttempts: 2, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := NewManagedEmbeddingModel(base, d, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(managed.Close)
	return managed, ledger
}
func embeddingContext() context.Context {
	return inference.WithIdentity(context.Background(), inference.Identity{TenantID: "host"})
}
func TestManagedMultimodalBatchLimitsRetryAndCapabilities(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		var request struct {
			Input struct {
				Contents [][]map[string]string `json:"contents"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		batch := request.Input.Contents[0]
		images, videos := 0, 0
		data := make([]map[string]any, len(batch))
		for i, input := range batch {
			if input["image"] != "" {
				images++
			}
			if input["video"] != "" {
				videos++
			}
			data[i] = map[string]any{"index": i, "embedding": []float32{1}}
		}
		if len(batch) > 20 || images > 5 || videos > 1 {
			t.Errorf("batch exceeded provider limits: %+v", batch)
		}
		json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"embeddings": data}, "usage": map[string]int{"total_tokens": len(batch)}})
	}))
	defer server.Close()
	base, err := NewDashScopeMultimodalEmbeddingModel(DashScopeMultimodalConfig{APIKey: "fixture", Model: "qwen3-vl-embedding", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	managed, ledger := managedEmbeddingFixture(t, server, "dashscope-multimodal", base)
	mm, ok := managed.(MultimodalEmbeddingModel)
	if !ok || !managed.Capabilities().Multimodal || managed.ModelName() != base.ModelName() {
		t.Fatal("optional multimodal contract missing")
	}
	inputs := []MultimodalInput{{Text: "text"}}
	for i := 0; i < 6; i++ {
		inputs = append(inputs, MultimodalInput{DataBlock: &message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", MediaType: "image/png", URL: "https://example.invalid/image.png"}}})
	}
	inputs = append(inputs, MultimodalInput{DataBlock: &message.DataBlock{Type: "data", Source: message.Base64Source{Type: "base64", MediaType: "image/png", Data: "AA=="}}})
	for i := 0; i < 2; i++ {
		inputs = append(inputs, MultimodalInput{DataBlock: &message.DataBlock{Type: "data", Source: message.URLSource{Type: "url", MediaType: "video/mp4", URL: "https://example.invalid/video.mp4"}}})
	}
	response, err := mm.EmbedMultimodal(embeddingContext(), inputs)
	if err != nil || len(response.Embeddings) != len(inputs) {
		t.Fatalf("multimodal response: %+v %v", response, err)
	}
	if sends.Load() != 4 || len(ledger.Snapshot().Attempts) != 4 {
		t.Fatalf("batch retries not individually accounted: %d", sends.Load())
	}
	if _, err := mm.EmbedMultimodal(context.Background(), inputs); !errors.Is(err, inference.ErrIdentityRequired) {
		t.Fatal(err)
	}
	if _, err := managed.Embed(embeddingContext(), []string{"text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mm.EmbedMultimodal(embeddingContext(), nil); err != nil {
		t.Fatal(err)
	}
	if managed.ManagedDeployment().Stats().Active != 0 {
		t.Fatal("permit leaked")
	}
}
func TestManagedMultimodalRejectsInvalidInputsAndIndices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"output":{"embeddings":[{"index":-1,"embedding":[1]}]}}`))
	}))
	defer server.Close()
	base, _ := NewDashScopeMultimodalEmbeddingModel(DashScopeMultimodalConfig{APIKey: "fixture", Model: "multimodal-embedding-v1", BaseURL: server.URL})
	managed, _ := managedEmbeddingFixture(t, server, "dashscope-multimodal", base)
	mm := managed.(MultimodalEmbeddingModel)
	for _, inputs := range [][]MultimodalInput{{{}}, {{Text: "text", DataBlock: &message.DataBlock{}}}, {{DataBlock: &message.DataBlock{Type: "data", Source: message.URLSource{MediaType: "audio/wav", URL: "https://example.invalid"}}}}, {{Text: "text"}}} {
		if _, err := mm.EmbedMultimodal(embeddingContext(), inputs); err == nil {
			t.Fatalf("invalid input/response accepted: %+v", inputs)
		}
	}
}
func TestManagedGeminiEmbeddingAndConstructorBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Requests []any `json:"requests"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		vectors := make([]map[string]any, len(request.Requests))
		for i := range vectors {
			vectors[i] = map[string]any{"values": []float32{1}}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer server.Close()
	base, _ := NewGeminiEmbeddingModel(&GeminiConfig{APIKey: "fixture", Model: "gemini-embedding-2", BaseURL: server.URL, BatchSize: 1})
	managed, ledger := managedEmbeddingFixture(t, server, "gemini-embedding", base)
	response, err := managed.Embed(embeddingContext(), []string{"a", "b"})
	if err != nil || len(response.Embeddings) != 2 {
		t.Fatal(err)
	}
	if ledger.Snapshot().UnknownUsage != 2 || managed.Capabilities().Multimodal {
		t.Fatal("Gemini unknown usage or text-only path overstated")
	}
	if _, err := managed.Embed(context.Background(), []string{"a"}); !errors.Is(err, inference.ErrIdentityRequired) {
		t.Fatal(err)
	}
	if _, err := NewManagedEmbeddingModel(base, managed.ManagedDeployment(), 0); err == nil {
		t.Fatal("unbounded managed workers accepted")
	}
	if _, err := NewManagedEmbeddingModel(managed, managed.ManagedDeployment(), 1); err == nil {
		t.Fatal("opaque adapter accepted")
	}
	if _, err := NewManagedEmbeddingModel(base, nil, 1); err == nil {
		t.Fatal("nil deployment accepted")
	}
	if _, err := NewGeminiEmbeddingModel(&GeminiConfig{MaxConcurrency: -1}); err == nil {
		t.Fatal("negative workers accepted")
	}
	if _, err := NewOpenAIEmbeddingModel(&OpenAICompatConfig{APIKey: "fixture", Model: "m", MaxConcurrency: -1}); err == nil {
		t.Fatal("negative workers accepted")
	}
}
func TestWorkerFailureCancelsAndJoinsStartedBatches(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	failure := errors.New("batch failed")
	_, err := batchEmbedWithWorkers(context.Background(), []string{"a", "b", "c"}, 1, 2, func(ctx context.Context, texts []string) (*EmbeddingResponse, error) {
		if texts[0] == "a" {
			close(started)
			<-ctx.Done()
			close(exited)
			return nil, ctx.Err()
		}
		<-started
		return nil, failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("returned before canceled worker completed")
	}
	for _, bad := range []*EmbeddingResponse{nil, {Embeddings: [][]float32{}}} {
		if _, err := batchEmbedWithWorkers(context.Background(), []string{"a"}, 1, 1, func(context.Context, []string) (*EmbeddingResponse, error) { return bad, nil }); err == nil {
			t.Fatal("invalid batch result accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := batchEmbedWithWorkers(ctx, []string{"a"}, 1, 1, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestManagedEmbeddingValidationFailsLogicalOperation(t *testing.T) {
	for _, api := range []string{"openai-embedding", "gemini-embedding", "dashscope-multimodal"} {
		t.Run(api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) }))
			defer server.Close()
			var base EmbeddingModel
			var err error
			switch api {
			case "openai-embedding":
				base, err = NewOpenAIEmbeddingModel(&OpenAICompatConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			case "gemini-embedding":
				base, err = NewGeminiEmbeddingModel(&GeminiConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			case "dashscope-multimodal":
				base, err = NewDashScopeMultimodalEmbeddingModel(DashScopeMultimodalConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
			}
			if err != nil {
				t.Fatal(err)
			}
			m, ledger := managedEmbeddingFixture(t, server, api, base)
			if _, err := m.Embed(embeddingContext(), []string{"input"}); err == nil {
				t.Fatal("invalid response accepted")
			}
			s := ledger.Snapshot()
			if len(s.Operations) != 1 || s.Operations[0].Outcome != "error" || s.Attempts[0].Outcome != "success" {
				t.Fatalf("logical failure confused with HTTP success: %+v", s)
			}
		})
	}
}
