package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestBoundedEmbeddingWorkersPreserveOrderAndJoin(t *testing.T) {
	var active, peak atomic.Int32
	texts := []string{"a", "b", "c", "d", "e"}
	resp, err := batchEmbedWithWorkers(context.Background(), texts, 1, 2, func(_ context.Context, batch []string) (*EmbeddingResponse, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		return &EmbeddingResponse{Embeddings: [][]float32{{float32(batch[0][0])}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 || active.Load() != 0 {
		t.Fatal("worker bound or ownership violated")
	}
	for i, v := range resp.Embeddings {
		if v[0] != float32(texts[i][0]) {
			t.Fatal("output order changed")
		}
	}
}
func TestManagedEmbeddingsSharePhysicalAdmission(t *testing.T) {
	var active, peak atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float32{float32(req.Input[i][0])}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"total_tokens": len(data)}})
	}))
	defer server.Close()
	controller := inference.NewController(inference.NewLedger(100))
	defer controller.Close()
	if err := controller.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 2, MaxQueued: 8, MaxQueuedWork: 100}); err != nil {
		t.Fatal(err)
	}
	d, err := controller.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: "e", Provider: "fixture", API: "openai-embedding", Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 2, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	base, err := NewOpenAIEmbeddingModel(&OpenAICompatConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := NewManagedEmbeddingModel(base, d, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	ctx := inference.WithIdentity(context.Background(), inference.Identity{TenantID: "t"})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := managed.Embed(ctx, []string{"a", "b", "c", "d"}); errs <- err }()
	}
	<-entered
	<-entered
	deadline := time.Now().Add(time.Second)
	for d.Stats().Queued < 2 {
		if time.Now().After(deadline) {
			close(release)
			wg.Wait()
			t.Fatalf("expected shared queue: %+v", d.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() > 2 || d.Stats().Active != 0 {
		t.Fatalf("physical request limit violated: peak=%d %+v", peak.Load(), d.Stats())
	}
}
func TestEmbeddingCapabilitiesReflectInputPath(t *testing.T) {
	gemini, _ := NewGeminiEmbeddingModel(&GeminiConfig{APIKey: "fixture", Model: "gemini-embedding-2"})
	if gemini.Capabilities().Multimodal {
		t.Fatal("model name invented unsupported Gemini input path")
	}
	mm, _ := NewDashScopeMultimodalEmbeddingModel(DashScopeMultimodalConfig{APIKey: "fixture", Model: "qwen3-vl-embedding"})
	if !mm.Capabilities().Multimodal {
		t.Fatal("multimodal input path not exposed")
	}
}

func TestManagedChatAndEmbeddingSharePoolAndIsolateCache(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var embeddings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		}
		embeddings.Add(1)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{"total_tokens":1}}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := inference.NewController(nil)
	defer c.Close()
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1, MaxQueued: 2, MaxQueuedWork: 100}); err != nil {
		t.Fatal(err)
	}
	register := func(id, api string) *inference.Deployment {
		d, err := c.Register(&inference.DeploymentConfig{Descriptor: inference.Descriptor{ID: id, Provider: "fixture", API: api, Model: "fixture", Endpoint: server.URL}, PoolID: "p", MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	chatBase, _ := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	chat, err := model.NewManagedChatModel(chatBase, register("chat", "openai-chat"))
	if err != nil {
		t.Fatal(err)
	}
	defer chat.Close()
	cache, err := NewFileEmbeddingCache(t.TempDir(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	embedBase, _ := NewOpenAIEmbeddingModel(&OpenAICompatConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL, Cache: cache})
	e1, err := NewManagedEmbeddingModel(embedBase, register("e1", "openai-embedding"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer e1.Close()
	e2, err := NewManagedEmbeddingModel(embedBase, register("e2", "openai-embedding"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	tenantA := inference.WithIdentity(ctx, inference.Identity{TenantID: "a"})
	done := make(chan error, 2)
	go func() { _, err := chat.Chat(tenantA, []*message.Msg{message.UserMsg("user", "hi")}); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { _, err := e1.Embed(tenantA, []string{"text"}); done <- err }()
	for e1.ManagedDeployment().Stats().Queued == 0 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if embeddings.Load() != 0 {
		t.Fatal("embedding bypassed active chat permit")
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, call := range []struct {
		m      ManagedEmbeddingModel
		ctx    context.Context
		source string
	}{
		{e1, tenantA, "cache"},
		{e1, inference.WithIdentity(ctx, inference.Identity{TenantID: "b"}), "api"},
		{e2, tenantA, "api"},
	} {
		r, err := call.m.Embed(call.ctx, []string{"text"})
		if err != nil || r.Source != call.source {
			t.Fatalf("cache namespace: %+v %v", r, err)
		}
	}
	if embeddings.Load() != 3 || e1.ManagedDeployment().Stats().Active != 0 {
		t.Fatal("cache hit sent or permit leaked")
	}
}
