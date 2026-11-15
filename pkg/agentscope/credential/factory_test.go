package credential

import (
	"testing"
)

func TestFactory_FromMap_Builtin(t *testing.T) {
	f := NewFactory()

	tests := []struct {
		m        map[string]string
		provider string
	}{
		{map[string]string{"provider": "openai", "api_key": "sk-1"}, "openai"},
		{map[string]string{"provider": "anthropic", "api_key": "sk-ant"}, "anthropic"},
		{map[string]string{"provider": "dashscope", "api_key": "sk-ds"}, "dashscope"},
		{map[string]string{"provider": "deepseek", "api_key": "sk-dp"}, "deepseek"},
		{map[string]string{"provider": "gemini", "api_key": "sk-gm"}, "gemini"},
		{map[string]string{"provider": "moonshot", "api_key": "sk-ms"}, "moonshot"},
		{map[string]string{"provider": "xai", "api_key": "sk-xai"}, "xai"},
		{map[string]string{"provider": "ollama"}, "ollama"},
	}

	for _, tt := range tests {
		cred, err := f.FromMap(tt.m)
		if err != nil {
			t.Errorf("FromMap(%v): %v", tt.m, err)
			continue
		}
		if cred.Provider() != tt.provider {
			t.Errorf("provider = %q, want %q", cred.Provider(), tt.provider)
		}
	}
}

func TestFactory_FromMap_MissingProvider(t *testing.T) {
	f := NewFactory()
	_, err := f.FromMap(map[string]string{"api_key": "sk-1"})
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestFactory_FromMap_UnknownProvider(t *testing.T) {
	f := NewFactory()
	_, err := f.FromMap(map[string]string{"provider": "nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestFactory_ListSchemas(t *testing.T) {
	f := NewFactory()
	schemas := f.ListSchemas()

	if len(schemas) != 9 {
		t.Fatalf("expected 9 schemas, got %d", len(schemas))
	}

	// Should be sorted by provider name
	for i := 1; i < len(schemas); i++ {
		if schemas[i].Provider < schemas[i-1].Provider {
			t.Errorf("schemas not sorted: %q before %q", schemas[i-1].Provider, schemas[i].Provider)
		}
	}
}

func TestFactory_GetSchema(t *testing.T) {
	f := NewFactory()

	schema, ok := f.GetSchema("openai")
	if !ok {
		t.Fatal("expected openai schema")
	}
	if schema.Provider != "openai" {
		t.Errorf("provider = %q", schema.Provider)
	}
	if len(schema.Fields) < 1 {
		t.Error("expected at least 1 field")
	}

	_, ok = f.GetSchema("nonexistent")
	if ok {
		t.Error("expected false for nonexistent provider")
	}
}

func TestFactory_HasProvider(t *testing.T) {
	f := NewFactory()

	if !f.HasProvider("openai") {
		t.Error("should have openai")
	}
	if f.HasProvider("nonexistent") {
		t.Error("should not have nonexistent")
	}
}

func TestFactory_Register_Custom(t *testing.T) {
	f := NewFactory()

	schema := CredentialSchema{
		Provider:    "custom",
		Description: "Custom provider",
		Fields: []FieldSchema{
			{Name: "token", Description: "Auth token", Required: true, Secret: true},
		},
	}

	err := f.Register(schema, func(m map[string]string) (Credential, error) {
		return &OpenAICredential{Key: m["token"], URL: "https://custom.example.com"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if !f.HasProvider("custom") {
		t.Error("should have custom provider after register")
	}

	cred, err := f.FromMap(map[string]string{"provider": "custom", "token": "my-token"})
	if err != nil {
		t.Fatal(err)
	}
	if cred.APIKey() != "my-token" {
		t.Errorf("APIKey = %q, want %q", cred.APIKey(), "my-token")
	}
}

func TestFactory_Register_Errors(t *testing.T) {
	f := NewFactory()

	err := f.Register(CredentialSchema{}, nil)
	if err == nil {
		t.Error("expected error for empty provider name")
	}

	err = f.Register(CredentialSchema{Provider: "x"}, nil)
	if err == nil {
		t.Error("expected error for nil constructor")
	}
}
