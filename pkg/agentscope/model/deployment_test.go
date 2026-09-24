package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestQualifiedModelCardIsOwned(t *testing.T) {
	cards := ListModels("openai")
	if len(cards) == 0 {
		t.Fatal("no cards")
	}
	card, err := GetModelCardFor("openai", cards[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	card.InputTypes[0] = "corrupted"
	again, err := GetModelCardFor("openai", cards[0].Name)
	if err != nil || again.InputTypes[0] == "corrupted" {
		t.Fatal("qualified card aliases cache")
	}
	if _, err := GetModelCardFor("missing", cards[0].Name); err == nil {
		t.Fatal("provider ignored")
	}
}
func TestManagedCapabilitiesAndHostPolicy(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	m, _, _ := managedFixture(t, server, 1)
	base, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	tenants := []string{"other"}
	restricted, err := NewManagedChatModel(base, m.ManagedDeployment(), WithManagedPolicy(ManagedPolicy{AllowedTenants: tenants, DenyTools: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer restricted.Close()
	tenants[0] = "tenant"
	if _, err := restricted.Chat(managedContext(), []*message.Msg{message.UserMsg("user", "hi")}); err == nil {
		t.Fatal("policy aliases input or allows wrong tenant")
	}
	capabilities := restricted.Capabilities()
	capabilities.InputMediaTypes[0] = "mutated"
	if restricted.Capabilities().InputMediaTypes[0] == "mutated" {
		t.Fatal("capabilities alias internal state")
	}
	if !capabilities.Streaming || !capabilities.Tools {
		t.Fatal("adapter capabilities missing")
	}
}

func TestManagedCallOptionsEvaluatedOnce(t *testing.T) {
	var wireTools int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []json.RawMessage `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		wireTools = len(request.Tools)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	template, _, _ := managedFixture(t, server, 1)
	base, _ := NewOpenAIChatModel(OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL})
	m, err := NewManagedChatModel(base, template.ManagedDeployment(), WithManagedPolicy(ManagedPolicy{DenyTools: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	calls := 0
	_, err = m.Chat(managedContext(), []*message.Msg{message.UserMsg("u", "hi")}, func(o *CallOptions) {
		calls++
		if calls > 1 {
			o.Tools = []ToolSchema{{Type: "function", Function: ToolFunction{Name: "forbidden"}}}
		}
	})
	if err != nil || calls != 1 || wireTools != 0 {
		t.Fatalf("option repeated or policy bypassed: calls=%d tools=%d err=%v", calls, wireTools, err)
	}
}
func TestGeminiCapabilitiesDoNotInventWireMedia(t *testing.T) {
	m := &managedChat{inner: &GeminiChatModel{}}
	for _, input := range m.Capabilities().InputMediaTypes {
		if input != "text/plain" {
			t.Fatalf("unimplemented Gemini input advertised: %s", input)
		}
	}
}
