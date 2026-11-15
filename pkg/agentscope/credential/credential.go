package credential

import (
	"fmt"
	"os"
	"sync"
)

// Credential provides authentication details for a model provider.
type Credential interface {
	Provider() string
	APIKey() string
	BaseURL() string
}

// --- Typed implementations ---

// DashScopeCredential authenticates with DashScope/Qwen.
type DashScopeCredential struct {
	Key string
	URL string
}

func (c *DashScopeCredential) Provider() string { return "dashscope" }
func (c *DashScopeCredential) APIKey() string   { return c.Key }
func (c *DashScopeCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://dashscope.aliyuncs.com/compatible-mode/v1"
}

// OpenAICredential authenticates with OpenAI.
type OpenAICredential struct {
	Key string
	URL string
}

func (c *OpenAICredential) Provider() string { return "openai" }
func (c *OpenAICredential) APIKey() string   { return c.Key }
func (c *OpenAICredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://api.openai.com/v1"
}

// AnthropicCredential authenticates with Anthropic.
type AnthropicCredential struct {
	Key string
	URL string
}

func (c *AnthropicCredential) Provider() string { return "anthropic" }
func (c *AnthropicCredential) APIKey() string   { return c.Key }
func (c *AnthropicCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://api.anthropic.com"
}

// DeepSeekCredential authenticates with DeepSeek.
type DeepSeekCredential struct {
	Key string
	URL string
}

func (c *DeepSeekCredential) Provider() string { return "deepseek" }
func (c *DeepSeekCredential) APIKey() string   { return c.Key }
func (c *DeepSeekCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://api.deepseek.com/v1"
}

// OllamaCredential authenticates with a local Ollama instance.
type OllamaCredential struct {
	URL string
}

func (c *OllamaCredential) Provider() string { return "ollama" }
func (c *OllamaCredential) APIKey() string   { return "" }
func (c *OllamaCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "http://localhost:11434/v1"
}

// GeminiCredential authenticates with Google Gemini.
type GeminiCredential struct {
	Key string
	URL string
}

func (c *GeminiCredential) Provider() string { return "gemini" }
func (c *GeminiCredential) APIKey() string   { return c.Key }
func (c *GeminiCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://generativelanguage.googleapis.com/v1beta"
}

// MoonshotCredential authenticates with Moonshot/Kimi.
type MoonshotCredential struct {
	Key string
	URL string
}

func (c *MoonshotCredential) Provider() string { return "moonshot" }
func (c *MoonshotCredential) APIKey() string   { return c.Key }
func (c *MoonshotCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://api.moonshot.cn"
}

// XAICredential authenticates with xAI/Grok.
type XAICredential struct {
	Key string
	URL string
}

func (c *XAICredential) Provider() string { return "xai" }
func (c *XAICredential) APIKey() string   { return c.Key }
func (c *XAICredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://api.x.ai"
}

// KimiCredential authenticates with Kimi (Moonshot's consumer brand).
type KimiCredential struct {
	Key string
	URL string
}

func (c *KimiCredential) Provider() string { return "kimi" }
func (c *KimiCredential) APIKey() string   { return c.Key }
func (c *KimiCredential) BaseURL() string {
	if c.URL != "" {
		return c.URL
	}
	return "https://api.moonshot.cn/v1"
}

// --- Environment auto-loading ---

// FromEnv attempts to load credentials from environment variables.
// It checks providers in order: Anthropic, DashScope, OpenAI, DeepSeek.
// Returns nil if no credentials are found.
func FromEnv() Credential {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return &AnthropicCredential{Key: key, URL: os.Getenv("ANTHROPIC_BASE_URL")}
	}
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return &DashScopeCredential{Key: key, URL: os.Getenv("DASHSCOPE_BASE_URL")}
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return &OpenAICredential{Key: key, URL: os.Getenv("OPENAI_BASE_URL")}
	}
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		return &DeepSeekCredential{Key: key, URL: os.Getenv("DEEPSEEK_BASE_URL")}
	}
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		return &GeminiCredential{Key: key, URL: os.Getenv("GEMINI_BASE_URL")}
	}
	if key := os.Getenv("MOONSHOT_API_KEY"); key != "" {
		return &MoonshotCredential{Key: key, URL: os.Getenv("MOONSHOT_BASE_URL")}
	}
	if key := os.Getenv("XAI_API_KEY"); key != "" {
		return &XAICredential{Key: key, URL: os.Getenv("XAI_BASE_URL")}
	}
	if key := os.Getenv("KIMI_API_KEY"); key != "" {
		return &KimiCredential{Key: key, URL: os.Getenv("KIMI_BASE_URL")}
	}
	return nil
}

// FromEnvForProvider loads a credential for a specific provider from env vars.
func FromEnvForProvider(provider string) Credential {
	switch provider {
	case "dashscope":
		if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
			return &DashScopeCredential{Key: key, URL: os.Getenv("DASHSCOPE_BASE_URL")}
		}
	case "openai":
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			return &OpenAICredential{Key: key, URL: os.Getenv("OPENAI_BASE_URL")}
		}
	case "anthropic":
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return &AnthropicCredential{Key: key, URL: os.Getenv("ANTHROPIC_BASE_URL")}
		}
	case "deepseek":
		if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
			return &DeepSeekCredential{Key: key, URL: os.Getenv("DEEPSEEK_BASE_URL")}
		}
	case "ollama":
		return &OllamaCredential{URL: os.Getenv("OLLAMA_BASE_URL")}
	case "gemini":
		if key := os.Getenv("GEMINI_API_KEY"); key != "" {
			return &GeminiCredential{Key: key, URL: os.Getenv("GEMINI_BASE_URL")}
		}
	case "moonshot":
		if key := os.Getenv("MOONSHOT_API_KEY"); key != "" {
			return &MoonshotCredential{Key: key, URL: os.Getenv("MOONSHOT_BASE_URL")}
		}
	case "xai":
		if key := os.Getenv("XAI_API_KEY"); key != "" {
			return &XAICredential{Key: key, URL: os.Getenv("XAI_BASE_URL")}
		}
	case "kimi":
		if key := os.Getenv("KIMI_API_KEY"); key != "" {
			return &KimiCredential{Key: key, URL: os.Getenv("KIMI_BASE_URL")}
		}
	}
	return nil
}

// --- Registry ---

// Registry stores named credentials for lookup.
type Registry struct {
	mu    sync.RWMutex
	creds map[string]Credential
}

// NewRegistry creates an empty credential registry.
func NewRegistry() *Registry {
	return &Registry{creds: make(map[string]Credential)}
}

// Register adds a credential under the given name.
func (r *Registry) Register(name string, cred Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creds[name] = cred
}

// Get retrieves a credential by name.
func (r *Registry) Get(name string) (Credential, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.creds[name]
	if !ok {
		return nil, fmt.Errorf("credential %q not registered", name)
	}
	return c, nil
}

// List returns all registered credential names.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.creds))
	for k := range r.creds {
		out = append(out, k)
	}
	return out
}

// FromMap creates a Credential from a map (e.g., deserialized config).
// Requires a "provider" key to discriminate type.
func FromMap(m map[string]string) (Credential, error) {
	provider := m["provider"]
	key := m["api_key"]
	url := m["base_url"]

	switch provider {
	case "dashscope":
		return &DashScopeCredential{Key: key, URL: url}, nil
	case "openai":
		return &OpenAICredential{Key: key, URL: url}, nil
	case "anthropic":
		return &AnthropicCredential{Key: key, URL: url}, nil
	case "deepseek":
		return &DeepSeekCredential{Key: key, URL: url}, nil
	case "ollama":
		return &OllamaCredential{URL: url}, nil
	case "gemini":
		return &GeminiCredential{Key: key, URL: url}, nil
	case "moonshot":
		return &MoonshotCredential{Key: key, URL: url}, nil
	case "xai":
		return &XAICredential{Key: key, URL: url}, nil
	case "kimi":
		return &KimiCredential{Key: key, URL: url}, nil
	default:
		return nil, fmt.Errorf("unknown provider: %q", provider)
	}
}
