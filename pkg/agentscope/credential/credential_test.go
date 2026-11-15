package credential

import (
	"testing"
)

func TestDashScopeCredential(t *testing.T) {
	c := &DashScopeCredential{Key: "sk-abc", URL: ""}
	if c.Provider() != "dashscope" {
		t.Fatalf("provider = %q", c.Provider())
	}
	if c.APIKey() != "sk-abc" {
		t.Fatalf("apikey = %q", c.APIKey())
	}
	if c.BaseURL() != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("baseurl = %q", c.BaseURL())
	}

	c2 := &DashScopeCredential{Key: "k", URL: "http://custom"}
	if c2.BaseURL() != "http://custom" {
		t.Fatalf("custom url = %q", c2.BaseURL())
	}
}

func TestOpenAICredential(t *testing.T) {
	c := &OpenAICredential{Key: "sk-test"}
	if c.Provider() != "openai" {
		t.Fatal()
	}
	if c.BaseURL() != "https://api.openai.com/v1" {
		t.Fatalf("url = %q", c.BaseURL())
	}
}

func TestAnthropicCredential(t *testing.T) {
	c := &AnthropicCredential{Key: "sk-ant"}
	if c.Provider() != "anthropic" {
		t.Fatal()
	}
	if c.BaseURL() != "https://api.anthropic.com" {
		t.Fatalf("url = %q", c.BaseURL())
	}
}

func TestDeepSeekCredential(t *testing.T) {
	c := &DeepSeekCredential{Key: "sk-ds"}
	if c.Provider() != "deepseek" {
		t.Fatal()
	}
	if c.BaseURL() != "https://api.deepseek.com/v1" {
		t.Fatalf("url = %q", c.BaseURL())
	}
}

func TestOllamaCredential(t *testing.T) {
	c := &OllamaCredential{}
	if c.Provider() != "ollama" {
		t.Fatal()
	}
	if c.APIKey() != "" {
		t.Fatal("ollama should have no api key")
	}
	if c.BaseURL() != "http://localhost:11434/v1" {
		t.Fatalf("url = %q", c.BaseURL())
	}
}

func TestFromMap(t *testing.T) {
	tests := []struct {
		m        map[string]string
		provider string
		wantErr  bool
	}{
		{map[string]string{"provider": "openai", "api_key": "k"}, "openai", false},
		{map[string]string{"provider": "dashscope", "api_key": "k"}, "dashscope", false},
		{map[string]string{"provider": "anthropic", "api_key": "k"}, "anthropic", false},
		{map[string]string{"provider": "deepseek", "api_key": "k"}, "deepseek", false},
		{map[string]string{"provider": "ollama"}, "ollama", false},
		{map[string]string{"provider": "unknown"}, "", true},
	}
	for _, tt := range tests {
		c, err := FromMap(tt.m)
		if tt.wantErr {
			if err == nil {
				t.Errorf("FromMap(%v) expected error", tt.m)
			}
			continue
		}
		if err != nil {
			t.Errorf("FromMap(%v): %v", tt.m, err)
			continue
		}
		if c.Provider() != tt.provider {
			t.Errorf("provider = %q, want %q", c.Provider(), tt.provider)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register("main", &OpenAICredential{Key: "sk-1"})
	r.Register("backup", &DashScopeCredential{Key: "sk-2"})

	c, err := r.Get("main")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey() != "sk-1" {
		t.Fatalf("key = %q", c.APIKey())
	}

	_, err = r.Get("missing")
	if err == nil {
		t.Fatal("expected error for missing credential")
	}

	names := r.List()
	if len(names) != 2 {
		t.Fatalf("list len = %d", len(names))
	}
}

func TestFromEnvForProvider_Ollama(t *testing.T) {
	c := FromEnvForProvider("ollama")
	if c == nil {
		t.Fatal("ollama should always return a credential")
	}
	if c.Provider() != "ollama" {
		t.Fatalf("provider = %q", c.Provider())
	}
}
