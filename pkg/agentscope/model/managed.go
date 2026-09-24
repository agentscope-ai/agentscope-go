package model

import (
	"context"
	"fmt"
	"maps"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// ManagedChatModel is an explicitly bound, single-target ChatModel. Close closes
// its owned idle HTTP connections; Controller.Close cancels active pool work.
// Required ChatModel remains unchanged. Share the managed model across agents.
type ManagedChatModel interface {
	ChatModel
	ManagedDeployment() *inference.Deployment
	Close()
	Capabilities() AdapterCapabilities
	Policy() ManagedPolicy
}
type managedChat struct {
	policy     ManagedPolicy
	inner      ChatModel
	deployment *inference.Deployment
	client     *http.Client
}

// NewManagedChatModel accepts built-in JSON/SSE adapters using the OpenAI Chat,
// Anthropic and Gemini APIs. Responses, custom models and hidden fallback or
// connectivity wrappers are rejected. Configuration and default headers are
// copied, and the concrete adapter's endpoint/model/API must match the binding.
// Managed operations own retries; CallOption retry settings are ignored.
func NewManagedChatModel(base ChatModel, d *inference.Deployment, options ...ManagedOption) (ManagedChatModel, error) {
	inner, client, api, name, endpoint, err := cloneManagedAdapter(base)
	if err != nil {
		return nil, err
	}
	if err = d.ValidateBinding(api, name, endpoint); err != nil {
		client.CloseIdleConnections()
		return nil, err
	}
	m := &managedChat{inner: inner, deployment: d, client: client}
	for _, option := range options {
		if option == nil {
			client.CloseIdleConnections()
			return nil, fmt.Errorf("inference: nil managed option")
		}
		option(&m.policy)
	}
	disabler, hasDisable := inner.(ThinkingDisabler)
	classifier, hasClassifier := inner.(StructuredFallbackClassifier)
	switch {
	case hasDisable && hasClassifier:
		return &managedThinkingClassifier{managedChat: m, disabler: disabler, classifier: classifier}, nil
	case hasDisable:
		return &managedThinking{managedChat: m, disabler: disabler}, nil
	case hasClassifier:
		return &managedClassifier{managedChat: m, classifier: classifier}, nil
	default:
		return m, nil
	}
}
func (m *managedChat) ManagedDeployment() *inference.Deployment { return m.deployment }
func (m *managedChat) Close()                                   { m.client.CloseIdleConnections() }
func (m *managedChat) ContextSize() int                         { return m.deployment.Descriptor().ContextSize }
func (m *managedChat) CountTokens(msgs []*message.Msg, tools []ToolSchema) int {
	return m.inner.CountTokens(msgs, tools)
}
func (m *managedChat) Chat(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (resp *ChatResponse, err error) {
	resolved, err := m.validate(ctx, msgs, opts)
	if err != nil {
		return nil, err
	}
	ctx, finish := m.deployment.Start(ctx, "reasoning")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options := []CallOption{func(o *CallOptions) { *o = resolved }}
	resp, err = m.inner.Chat(ctx, msgs, options...)
	if err == nil && resp == nil {
		err = fmt.Errorf("inference: adapter returned nil response")
	}
	return resp, err
}
func (m *managedChat) ChatStream(ctx context.Context, msgs []*message.Msg, opts ...CallOption) (<-chan ChatResponse, error) {
	resolved, err := m.validate(ctx, msgs, opts)
	if err != nil {
		return nil, err
	}
	ctx, finish := m.deployment.Start(ctx, "reasoning")
	ctx, cancel := context.WithCancel(ctx)
	options := []CallOption{func(o *CallOptions) { *o = resolved }}
	source, err := m.inner.ChatStream(ctx, msgs, options...)
	if err != nil {
		cancel()
		finish(err)
		return nil, err
	}
	out := make(chan ChatResponse)
	go func() {
		var finalErr error
		defer close(out)
		// Cancellation starts cleanup; source closure and parser completion establish
		// completion. finish waits for the parser before releasing the permit.
		defer func() {
			cancel()
			for range source {
			}
			finish(finalErr)
		}()
		terminal := false
		for {
			var part ChatResponse
			var ok bool
			select {
			case <-ctx.Done():
				finalErr = ctx.Err()
				return
			case part, ok = <-source:
			}
			if !ok {
				if !terminal {
					finalErr = fmt.Errorf("inference: adapter stream has no final response")
					select {
					case out <- ChatResponse{IsLast: true, Error: finalErr}:
					case <-ctx.Done():
					}
				}
				return
			}
			if part.IsLast {
				terminal = true
				finalErr = part.Error
				if finalErr == nil {
					finalErr = inference.CurrentOperation(ctx).StreamError()
				}
				part.Error = finalErr
			}
			select {
			case out <- part:
			case <-ctx.Done():
				finalErr = ctx.Err()
				return
			}
			if terminal {
				return
			}
		}
	}()
	return out, nil
}

type managedThinking struct {
	*managedChat
	disabler ThinkingDisabler
}

func (m *managedThinking) DisableThinkingOptions() []CallOption {
	return m.disabler.DisableThinkingOptions()
}

type managedClassifier struct {
	*managedChat
	classifier StructuredFallbackClassifier
}

func (m *managedClassifier) IsStructuredOutputFallbackError(err error) bool {
	return m.classifier.IsStructuredOutputFallbackError(err)
}

type managedThinkingClassifier struct {
	*managedChat
	disabler   ThinkingDisabler
	classifier StructuredFallbackClassifier
}

func (m *managedThinkingClassifier) DisableThinkingOptions() []CallOption {
	return m.disabler.DisableThinkingOptions()
}
func (m *managedThinkingClassifier) IsStructuredOutputFallbackError(err error) bool {
	return m.classifier.IsStructuredOutputFallbackError(err)
}

func cloneManagedAdapter(base ChatModel) (ChatModel, *http.Client, string, string, string, error) {
	var inner ChatModel
	var original *http.Client
	var api, name, endpoint string
	switch m := base.(type) {
	case *OpenAIChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "openai-chat", m.model, m.baseURL
		}
	case *DeepSeekChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "openai-chat", m.model, m.baseURL
		}
	case *XAIChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "xai-chat", m.model, m.baseURL
		}
	case *MoonshotChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "openai-chat", m.model, m.baseURL
		}
	case *OllamaChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "openai-chat", m.model, m.baseURL
		}
	case *DashScopeChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "openai-chat", m.model, m.baseURL
		}
	case *AnthropicChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "anthropic", m.model, m.baseURL
		}
	case *GeminiChatModel:
		if m != nil {
			cp := *m
			cp.defaultHeaders = maps.Clone(m.defaultHeaders)
			inner = &cp
			original = m.httpClient
			api, name, endpoint = "gemini-chat", m.model, m.baseURL
		}
	}
	if inner == nil {
		return nil, nil, "", "", "", fmt.Errorf("inference: unsupported or nil chat adapter %T", base)
	}
	client, err := inference.NewHTTPClient(original)
	if err != nil {
		return nil, nil, "", "", "", err
	}
	switch m := inner.(type) {
	case *OpenAIChatModel:
		m.httpClient = client
	case *DeepSeekChatModel:
		m.httpClient = client
	case *XAIChatModel:
		m.httpClient = client
	case *MoonshotChatModel:
		m.httpClient = client
	case *OllamaChatModel:
		m.httpClient = client
	case *DashScopeChatModel:
		m.httpClient = client
	case *AnthropicChatModel:
		m.httpClient = client
	case *GeminiChatModel:
		m.httpClient = client
	}
	return inner, client, api, name, endpoint, nil
}
