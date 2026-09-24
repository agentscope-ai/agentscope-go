package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func managedFixture(t *testing.T, server *httptest.Server, attempts int) (ManagedChatModel, *inference.Controller, *inference.Ledger) {
	t.Helper()
	l := inference.NewLedger(50)
	c := inference.NewController(l)
	t.Cleanup(c.Close)
	if err := c.RegisterPool(inference.PoolConfig{ID: "p", MaxActive: 1, MaxQueued: 1, MaxQueuedWork: 100}); err != nil {
		t.Fatal(err)
	}
	d, err := c.Register(&inference.DeploymentConfig{PoolID: "p", Descriptor: inference.Descriptor{ID: "d", Provider: "openai", API: "openai-chat", Model: "fixture", Endpoint: server.URL, ContextSize: 1234}, MaxAttempts: attempts, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	base, err := NewOpenAIChatModel(OpenAIConfig{APIKey: "fixture", Model: "fixture", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManagedChatModel(base, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, c, l
}
func managedContext() context.Context {
	return inference.WithIdentity(context.Background(), inference.Identity{TenantID: "tenant"})
}
func TestManagedRetryCapAtSendBoundary(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	m, _, ledger := managedFixture(t, server, 2)
	_, err := m.Chat(managedContext(), []*message.Msg{message.UserMsg("user", "hi")}, WithRetries(3, time.Nanosecond))
	if err == nil || sends.Load() != 2 || len(ledger.Snapshot().Attempts) != 2 {
		t.Fatalf("multiplied retries: sends=%d err=%v", sends.Load(), err)
	}
	if ResolveContextSize(m, 99) != 1234 {
		t.Fatal("configured context missing")
	}
	if _, ok := m.(ThinkingDisabler); ok {
		t.Fatal("wrapper invented thinking capability")
	}
}
func TestManagedStreamRetainsPermitForSlowConsumer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	m, _, ledger := managedFixture(t, server, 1)
	ctx, cancel := context.WithCancel(managedContext())
	stream, err := m.ChatStream(ctx, []*message.Msg{message.UserMsg("user", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(ledger.Snapshot().Attempts) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no send")
		}
		time.Sleep(time.Millisecond)
	}
	if m.ManagedDeployment().Stats().Active != 1 {
		t.Fatal("permit released before consumer")
	}
	cancel()
	for range stream {
	}
	if m.ManagedDeployment().Stats().Active != 0 {
		t.Fatal("canceled stream leaked permit")
	}
}
func TestManagedStreamRejectsCleanTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer server.Close()
	m, _, _ := managedFixture(t, server, 1)
	stream, err := m.ChatStream(managedContext(), []*message.Msg{message.UserMsg("user", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	var terminal ChatResponse
	for part := range stream {
		terminal = part
	}
	if terminal.Error == nil || !terminal.IsLast {
		t.Fatal("truncated stream looked complete")
	}
}
func TestManagedRejectsOpaqueModels(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	m, _, _ := managedFixture(t, server, 1)
	if _, err := NewManagedChatModel(m, m.ManagedDeployment()); err == nil {
		t.Fatal("double wrapper accepted")
	}
	ctx, cancel := context.WithCancel(managedContext())
	cancel()
	_, err := m.Chat(ctx, []*message.Msg{message.UserMsg("user", "hi")})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestManagedStructuredStrategiesShareAttemptCap(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Write([]byte(`{"choices":[{"message":{"content":"no JSON"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`))
	}))
	defer server.Close()
	m, _, ledger := managedFixture(t, server, 2)
	_, _, err := GenerateStructuredOutputWithUsage(inference.WithPurpose(managedContext(), "summary"), m, []*message.Msg{message.UserMsg("user", "hi")}, []byte(`{"type":"object"}`))
	if err == nil || sends.Load() != 2 {
		t.Fatalf("strategy reset attempt cap: %d %v", sends.Load(), err)
	}
	s := ledger.Snapshot()
	if len(s.Operations) != 1 || s.Attempts[0].Purpose != "summary" || s.Attempts[1].Purpose != "repair" {
		t.Fatalf("missing operation/purpose attribution: %+v", s)
	}
}
