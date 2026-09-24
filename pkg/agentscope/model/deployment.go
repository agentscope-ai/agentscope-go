package model

import (
	"context"
	"fmt"
	"slices"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// AdapterCapabilities describes the implemented adapter path, not model-card
// metadata or server support. Media types describe direct message inputs; tool
// result media can be rendered as placeholders. Source formats remain subject
// to the adapter's documented URL/base64 rules. NativeJSONSchema is currently
// false for these adapters; structured output uses synthetic tool calls.
type AdapterCapabilities struct {
	InputMediaTypes  []string
	OutputMediaTypes []string
	Tools            bool
	Streaming        bool
	NativeJSONSchema bool
}

// ManagedPolicy is host configuration, independently of adapter capabilities.
// An empty tenant allowlist permits any authenticated identity supplied by the
// host. It never authenticates input itself. DenyTools rejects tool schemas.
type ManagedPolicy struct {
	AllowedTenants []string
	DenyTools      bool
}
type ManagedOption func(*ManagedPolicy)

func WithManagedPolicy(policy ManagedPolicy) ManagedOption {
	policy.AllowedTenants = slices.Clone(policy.AllowedTenants)
	return func(p *ManagedPolicy) { *p = policy; p.AllowedTenants = slices.Clone(policy.AllowedTenants) }
}
func (m *managedChat) Capabilities() AdapterCapabilities {
	c := AdapterCapabilities{InputMediaTypes: []string{"text/plain", "image/*", "audio/wav", "audio/mp3", "audio/mpeg"}, OutputMediaTypes: []string{"text/plain"}, Tools: true, Streaming: true}
	switch m.inner.(type) {
	case *AnthropicChatModel:
		c.InputMediaTypes = []string{"text/plain", "image/*"}
	case *GeminiChatModel:
		c.InputMediaTypes = []string{"text/plain"}
	case *DashScopeChatModel:
		c.InputMediaTypes = append(c.InputMediaTypes, "video/*")
		c.OutputMediaTypes = append(c.OutputMediaTypes, "audio/wav")
	case *OpenAIChatModel:
		c.OutputMediaTypes = append(c.OutputMediaTypes, "audio/wav")
	}
	return c
}
func (m *managedChat) Policy() ManagedPolicy {
	p := m.policy
	p.AllowedTenants = slices.Clone(p.AllowedTenants)
	return p
}
func (m *managedChat) validate(ctx context.Context, msgs []*message.Msg, opts []CallOption) (CallOptions, error) {
	if len(m.policy.AllowedTenants) > 0 && !slices.Contains(m.policy.AllowedTenants, inference.IdentityFromContext(ctx).TenantID) {
		return CallOptions{}, fmt.Errorf("inference: tenant is not allowed for this target")
	}
	var options CallOptions
	for _, opt := range opts {
		if opt == nil {
			return CallOptions{}, fmt.Errorf("inference: nil call option")
		}
		opt(&options)
	}
	if m.policy.DenyTools && len(options.Tools) > 0 {
		return CallOptions{}, fmt.Errorf("inference: tools are denied by host policy")
	}
	if options.ResponseFormat != nil {
		return CallOptions{}, fmt.Errorf("inference: native response_format is not implemented by this adapter; use structured tool output")
	}
	if len(msgs) == 0 {
		return CallOptions{}, fmt.Errorf("inference: messages are required")
	}
	if _, gemini := m.inner.(*GeminiChatModel); gemini {
		for _, msg := range msgs {
			if msg == nil {
				continue
			}
			for _, block := range msg.Content {
				if _, media := block.(message.DataBlock); media {
					return CallOptions{}, fmt.Errorf("inference: Gemini chat adapter accepts text inputs only")
				}
			}
		}
	}
	options.MaxRetries = 0
	options.RetryDelay = 0
	return options, nil
}

// GetModelCardFor performs provider-qualified lookup and returns an owned copy.
// Provider is the embedded catalog directory (for example openai or anthropic),
// not the API family or deployment ID. Cards are upstream metadata, not verified
// limits of a configured server. GetModelCard retains its legacy name lookup.
func GetModelCardFor(provider, name string) (*ModelCard, error) {
	cards := loadEmbeddedModelCards()
	for i := range cards {
		card := cards[i]
		if card.Provider == provider && card.Name == name {
			card.InputTypes = slices.Clone(card.InputTypes)
			card.OutputTypes = slices.Clone(card.OutputTypes)
			if card.DeprecatedAt != nil {
				value := *card.DeprecatedAt
				card.DeprecatedAt = &value
			}
			return &card, nil
		}
	}
	return nil, fmt.Errorf("model card %q for provider %q not found", name, provider)
}
