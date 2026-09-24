package embedding

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
)

// Capabilities describes the implemented input path, independently of a model
// name or model card. Gemini's current Go embedding path accepts text only.
type Capabilities struct {
	InputMediaTypes []string
	Multimodal      bool
}

// CapabilityProvider is optional; EmbeddingModel remains text-compatible.
type CapabilityProvider interface{ Capabilities() Capabilities }

// MultimodalEmbeddingModel is the optional typed multimodal input interface.
type MultimodalEmbeddingModel interface {
	EmbeddingModel
	EmbedMultimodal(context.Context, []MultimodalInput) (*EmbeddingResponse, error)
}

func (m *OpenAICompatEmbeddingModel) Capabilities() Capabilities {
	return Capabilities{InputMediaTypes: []string{"text/plain"}}
}
func (m *GeminiEmbeddingModel) Capabilities() Capabilities {
	return Capabilities{InputMediaTypes: []string{"text/plain"}}
}
func (m *DashScopeMultimodalEmbeddingModel) Capabilities() Capabilities {
	return Capabilities{InputMediaTypes: []string{"text/plain", "image/*", "video/*"}, Multimodal: true}
}

// ManagedEmbeddingModel binds each batch to shared admission and owns a fixed
// per-call worker limit. Cache namespaces include tenant and deployment identity.
// The supplied cache must be safe for the configured concurrent callers.
type ManagedEmbeddingModel interface {
	EmbeddingModel
	CapabilityProvider
	ManagedDeployment() *inference.Deployment
	Close()
}
type managedEmbedding struct {
	inner      EmbeddingModel
	deployment *inference.Deployment
	client     *http.Client
}

func (m *managedEmbedding) ManagedDeployment() *inference.Deployment { return m.deployment }
func (m *managedEmbedding) Close()                                   { m.client.CloseIdleConnections() }
func (m *managedEmbedding) ModelName() string                        { return m.inner.ModelName() }
func (m *managedEmbedding) Capabilities() Capabilities {
	return m.inner.(CapabilityProvider).Capabilities()
}
func (m *managedEmbedding) Embed(ctx context.Context, texts []string) (*EmbeddingResponse, error) {
	if inference.IdentityFromContext(ctx).TenantID == "" {
		return nil, inference.ErrIdentityRequired
	}
	return m.inner.Embed(ctx, texts)
}

type managedMultimodalEmbedding struct {
	*managedEmbedding
	multimodal MultimodalEmbeddingModel
}

func (m *managedMultimodalEmbedding) EmbedMultimodal(ctx context.Context, inputs []MultimodalInput) (*EmbeddingResponse, error) {
	if inference.IdentityFromContext(ctx).TenantID == "" {
		return nil, inference.ErrIdentityRequired
	}
	return m.multimodal.EmbedMultimodal(ctx, inputs)
}

// NewManagedEmbeddingModel copies a supported built-in adapter. workers must be
// positive; each retry of each batch is separately admitted. Opaque wrappers are
// rejected. Existing unmanaged constructor defaults remain unchanged.
func NewManagedEmbeddingModel(base EmbeddingModel, d *inference.Deployment, workers int) (ManagedEmbeddingModel, error) {
	if workers < 1 {
		return nil, fmt.Errorf("embedding: managed concurrency must be positive")
	}
	var inner EmbeddingModel
	var original *http.Client
	var api, name, endpoint string
	switch m := base.(type) {
	case *OpenAICompatEmbeddingModel:
		if m != nil {
			cp := *m
			cp.maxConcurrency = workers
			cp.managed = d
			inner = &cp
			original = m.client
			api, name, endpoint = "openai-embedding", m.model, m.baseURL
		}
	case *GeminiEmbeddingModel:
		if m != nil {
			cp := *m
			cp.maxConcurrency = workers
			cp.managed = d
			inner = &cp
			original = m.client
			api, name, endpoint = "gemini-embedding", m.model, m.baseURL
		}
	case *DashScopeMultimodalEmbeddingModel:
		if m != nil {
			cp := *m
			cp.maxConcurrency = workers
			cp.managed = d
			inner = &cp
			original = m.client
			api, name, endpoint = "dashscope-multimodal", m.model, m.baseURL
		}
	}
	if inner == nil {
		return nil, fmt.Errorf("embedding: unsupported or nil managed adapter %T", base)
	}
	if err := d.ValidateBinding(api, name, endpoint); err != nil {
		return nil, err
	}
	client, err := inference.NewHTTPClient(original)
	if err != nil {
		return nil, err
	}
	switch m := inner.(type) {
	case *OpenAICompatEmbeddingModel:
		m.client = client
	case *GeminiEmbeddingModel:
		m.client = client
	case *DashScopeMultimodalEmbeddingModel:
		m.client = client
	}
	managed := &managedEmbedding{inner: inner, deployment: d, client: client}
	if mm, ok := inner.(MultimodalEmbeddingModel); ok {
		return &managedMultimodalEmbedding{managedEmbedding: managed, multimodal: mm}, nil
	}
	return managed, nil
}
func embeddingKey(ctx context.Context, d *inference.Deployment, name string, dimensions int, texts []string) string {
	if d == nil {
		return CacheKey(name, dimensions, texts)
	}
	return CacheKey("managed-v1", d.Descriptor().ID, inference.IdentityFromContext(ctx).TenantID, name, dimensions, texts)
}

func (m *DashScopeMultimodalEmbeddingModel) embedManagedMultimodal(ctx context.Context, inputs []MultimodalInput) (*EmbeddingResponse, error) {
	limits := getLimits(m.model)
	var batches [][]MultimodalInput
	var current []MultimodalInput
	images, videos := 0, 0
	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
			images = 0
			videos = 0
		}
	}
	for _, input := range inputs {
		if (input.Text == "") == (input.DataBlock == nil) {
			return nil, fmt.Errorf("embedding: each input must contain either text or a data block")
		}
		image, video := 0, 0
		if input.DataBlock != nil {
			formatted := m.formatDataBlock(input.DataBlock)
			if formatted == nil {
				return nil, fmt.Errorf("embedding: unsupported multimodal input")
			}
			if _, ok := formatted["image"]; ok {
				image = 1
			}
			if _, ok := formatted["video"]; ok {
				video = 1
			}
		}
		if len(current) == limits.maxElements || images+image > limits.maxImages || videos+video > limits.maxVideos {
			flush()
		}
		current = append(current, input)
		images += image
		videos += video
	}
	flush()
	sizes := make([]int, len(batches))
	for i, b := range batches {
		sizes[i] = len(b)
	}
	return runEmbeddingBatches(ctx, sizes, m.maxConcurrency, func(ctx context.Context, i int) (*EmbeddingResponse, error) {
		return m.embedMultimodalBatch(ctx, batches[i])
	})
}
