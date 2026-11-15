package model

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSecretStr_String(t *testing.T) {
	s := NewSecretStr("my-api-key-123")
	if s.String() != "***" {
		t.Errorf("String() = %q, want %q", s.String(), "***")
	}
}

func TestSecretStr_Value(t *testing.T) {
	s := NewSecretStr("my-api-key-123")
	if s.Value() != "my-api-key-123" {
		t.Errorf("Value() = %q, want %q", s.Value(), "my-api-key-123")
	}
}

func TestSecretStr_GoString(t *testing.T) {
	s := NewSecretStr("secret")
	got := fmt.Sprintf("%#v", s)
	if got != "SecretStr{***}" {
		t.Errorf("GoString() = %q, want %q", got, "SecretStr{***}")
	}
}

func TestSecretStr_MarshalJSON(t *testing.T) {
	s := NewSecretStr("real-secret")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"***"` {
		t.Errorf("MarshalJSON = %s, want %q", b, `"***"`)
	}
}

func TestSecretStr_MarshalJSON_InStruct(t *testing.T) {
	type Config struct {
		APIKey SecretStr `json:"api_key"`
		Name   string    `json:"name"`
	}
	cfg := Config{APIKey: NewSecretStr("sk-1234"), Name: "test"}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"api_key":"***","name":"test"}`
	if string(b) != expected {
		t.Errorf("JSON = %s, want %s", b, expected)
	}
}

func TestSecretStr_MarshalText(t *testing.T) {
	s := NewSecretStr("hidden")
	b, err := s.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "***" {
		t.Errorf("MarshalText = %q, want %q", string(b), "***")
	}
}

func TestSecretStr_IsEmpty(t *testing.T) {
	if !NewSecretStr("").IsEmpty() {
		t.Error("empty secret should report IsEmpty=true")
	}
	if NewSecretStr("x").IsEmpty() {
		t.Error("non-empty secret should report IsEmpty=false")
	}
}

func TestSecretStr_Sprintf(t *testing.T) {
	s := NewSecretStr("real-value")
	got := fmt.Sprintf("key=%s", s)
	if got != "key=***" {
		t.Errorf("Sprintf = %q, should not contain real value", got)
	}
}

func TestSecretStr_UnmarshalJSON(t *testing.T) {
	var s SecretStr
	if err := json.Unmarshal([]byte(`"my-secret-key"`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Value() != "my-secret-key" {
		t.Errorf("Value() = %q, want %q", s.Value(), "my-secret-key")
	}
	// String() should still be redacted.
	if s.String() != "***" {
		t.Errorf("String() = %q, want %q", s.String(), "***")
	}
}

func TestSecretStr_UnmarshalJSON_Null(t *testing.T) {
	var s SecretStr
	err := json.Unmarshal([]byte(`null`), &s)
	// json.Unmarshal of null into a string returns an error in strict mode,
	// but the Go standard library silently sets it to the zero value.
	// Either an error or an empty value is acceptable.
	if err != nil {
		// Null cannot be unmarshalled into a string — this is fine.
		return
	}
	if s.Value() != "" {
		t.Errorf("Value() = %q, want empty string after null unmarshal", s.Value())
	}
}

func TestSecretStr_RoundTrip(t *testing.T) {
	original := NewSecretStr("supersecret")

	// Marshal → always produces "***".
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	// Unmarshal the marshaled value.
	var restored SecretStr
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}

	// The restored value should be "***" (the redacted string), not the original.
	if restored.Value() != "***" {
		t.Errorf("round-trip Value() = %q, want %q", restored.Value(), "***")
	}
}

func TestResolveAPIKey(t *testing.T) {
	// When secret is non-empty, it takes precedence.
	got := ResolveAPIKey("plain-key", NewSecretStr("secret-key"))
	if got != "secret-key" {
		t.Errorf("ResolveAPIKey = %q, want %q", got, "secret-key")
	}

	// When secret is empty, fall back to plain key.
	got = ResolveAPIKey("plain-key", NewSecretStr(""))
	if got != "plain-key" {
		t.Errorf("ResolveAPIKey = %q, want %q", got, "plain-key")
	}

	// Both empty.
	got = ResolveAPIKey("", NewSecretStr(""))
	if got != "" {
		t.Errorf("ResolveAPIKey = %q, want empty", got)
	}
}
