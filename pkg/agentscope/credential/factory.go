package credential

import (
	"fmt"
	"sort"
	"sync"
)

// FieldSchema describes a single field in a credential schema.
type FieldSchema struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"` // true for API keys and tokens
}

// CredentialSchema describes the fields needed to create a credential for a
// specific provider.
type CredentialSchema struct {
	Provider    string        `json:"provider"`
	Description string        `json:"description"`
	Fields      []FieldSchema `json:"fields"`
}

// ConstructorFunc is a function that creates a Credential from a map of
// field values.
type ConstructorFunc func(fields map[string]string) (Credential, error)

// Factory provides dynamic credential creation and schema introspection.
// Use Register to add new provider types, FromMap to create credentials,
// and ListSchemas to enumerate available providers.
type Factory struct {
	mu           sync.RWMutex
	constructors map[string]ConstructorFunc
	schemas      map[string]CredentialSchema
}

// NewFactory creates a Factory pre-loaded with all built-in providers.
func NewFactory() *Factory {
	f := &Factory{
		constructors: make(map[string]ConstructorFunc),
		schemas:      make(map[string]CredentialSchema),
	}
	f.registerBuiltins()
	return f
}

// Register adds a credential provider to the factory. The schema describes
// the fields required, and the constructor creates instances from field maps.
func (f *Factory) Register(schema CredentialSchema, constructor ConstructorFunc) error {
	if schema.Provider == "" {
		return fmt.Errorf("credential factory: provider name is required")
	}
	if constructor == nil {
		return fmt.Errorf("credential factory: constructor is required for provider %q", schema.Provider)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.constructors[schema.Provider] = constructor
	f.schemas[schema.Provider] = schema
	return nil
}

// FromMap creates a Credential from a map of field values. The map must
// contain a "provider" key to select the constructor.
func (f *Factory) FromMap(m map[string]string) (Credential, error) {
	provider := m["provider"]
	if provider == "" {
		return nil, fmt.Errorf("credential factory: missing 'provider' key")
	}

	f.mu.RLock()
	constructor, ok := f.constructors[provider]
	f.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("credential factory: unknown provider %q", provider)
	}

	return constructor(m)
}

// ListSchemas returns all registered credential schemas, sorted by provider name.
func (f *Factory) ListSchemas() []CredentialSchema {
	f.mu.RLock()
	defer f.mu.RUnlock()

	schemas := make([]CredentialSchema, 0, len(f.schemas))
	for _, s := range f.schemas {
		schemas = append(schemas, s)
	}

	sort.Slice(schemas, func(i, j int) bool {
		return schemas[i].Provider < schemas[j].Provider
	})
	return schemas
}

// GetSchema returns the credential schema for a specific provider.
func (f *Factory) GetSchema(provider string) (CredentialSchema, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, ok := f.schemas[provider]
	return s, ok
}

// HasProvider reports whether the factory knows about the given provider.
func (f *Factory) HasProvider(provider string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.constructors[provider]
	return ok
}

// registerBuiltins registers all built-in credential providers.
func (f *Factory) registerBuiltins() {
	f.constructors["openai"] = func(m map[string]string) (Credential, error) {
		return &OpenAICredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["openai"] = CredentialSchema{
		Provider:    "openai",
		Description: "OpenAI API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["anthropic"] = func(m map[string]string) (Credential, error) {
		return &AnthropicCredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["anthropic"] = CredentialSchema{
		Provider:    "anthropic",
		Description: "Anthropic API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["dashscope"] = func(m map[string]string) (Credential, error) {
		return &DashScopeCredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["dashscope"] = CredentialSchema{
		Provider:    "dashscope",
		Description: "DashScope/Qwen API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["deepseek"] = func(m map[string]string) (Credential, error) {
		return &DeepSeekCredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["deepseek"] = CredentialSchema{
		Provider:    "deepseek",
		Description: "DeepSeek API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["gemini"] = func(m map[string]string) (Credential, error) {
		return &GeminiCredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["gemini"] = CredentialSchema{
		Provider:    "gemini",
		Description: "Google Gemini API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["moonshot"] = func(m map[string]string) (Credential, error) {
		return &MoonshotCredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["moonshot"] = CredentialSchema{
		Provider:    "moonshot",
		Description: "Moonshot/Kimi API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["xai"] = func(m map[string]string) (Credential, error) {
		return &XAICredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["xai"] = CredentialSchema{
		Provider:    "xai",
		Description: "xAI/Grok API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}

	f.constructors["ollama"] = func(m map[string]string) (Credential, error) {
		return &OllamaCredential{URL: m["base_url"]}, nil
	}
	f.schemas["ollama"] = CredentialSchema{
		Provider:    "ollama",
		Description: "Ollama local model server",
		Fields: []FieldSchema{
			{Name: "base_url", Description: "Server URL", Required: false},
		},
	}

	f.constructors["kimi"] = func(m map[string]string) (Credential, error) {
		return &KimiCredential{Key: m["api_key"], URL: m["base_url"]}, nil
	}
	f.schemas["kimi"] = CredentialSchema{
		Provider:    "kimi",
		Description: "Kimi API credentials",
		Fields: []FieldSchema{
			{Name: "api_key", Description: "API key", Required: true, Secret: true},
			{Name: "base_url", Description: "Base URL override", Required: false},
		},
	}
}
