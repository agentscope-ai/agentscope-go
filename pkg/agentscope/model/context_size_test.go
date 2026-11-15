package model

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// testContextFallback mirrors the agent's default window for models that
// report none.
const testContextFallback = 128000

// sizedMockModel is a connectivity mock that reports a context window.
type sizedMockModel struct {
	*connMockModel
	contextSize int
}

func (m *sizedMockModel) ContextSize() int { return m.contextSize }

func TestOllamaChatModel_DeclaredContextSize(t *testing.T) {
	m, err := NewOllamaChatModel(OllamaConfig{Model: "qwen2.5:0.5b", ContextSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveContextSize(m, testContextFallback); got != 8192 {
		t.Errorf("ResolveContextSize = %d, want declared 8192", got)
	}
}

func TestOllamaChatModel_UndeclaredContextSizeIgnoresCard(t *testing.T) {
	// The embedded qwen3:14b card advertises the model's maximum window,
	// which says nothing about the num_ctx the server actually runs with.
	if _, err := GetModelCard("qwen3:14b"); err != nil {
		t.Fatalf("fixture card missing: %v", err)
	}
	m, err := NewOllamaChatModel(OllamaConfig{Model: "qwen3:14b"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveContextSize(m, testContextFallback); got != testContextFallback {
		t.Errorf("ResolveContextSize = %d, want fallback %d", got, testContextFallback)
	}
}

func TestOllamaConfig_NegativeContextSize(t *testing.T) {
	_, err := NewOllamaChatModel(OllamaConfig{Model: "m", ContextSize: -1})
	if err == nil || !strings.Contains(err.Error(), "ContextSize") {
		t.Fatalf("expected ContextSize error, got %v", err)
	}
}

func TestOllamaContextSize_NotSentOnWire(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			undeclared := captureOllamaRequest(t, 0, streaming)
			declared := captureOllamaRequest(t, 8192, streaming)
			if !bytes.Equal(declared, undeclared) {
				t.Errorf("declared ContextSize changed the request body:\ndeclared:   %s\nundeclared: %s", declared, undeclared)
			}
		})
	}
}

// captureOllamaRequest returns the body of the single request the adapter
// sends for one Chat or ChatStream call.
func captureOllamaRequest(t *testing.T, contextSize int, streaming bool) []byte {
	t.Helper()
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		select {
		case captured <- body:
		default:
			t.Error("adapter sent more than one request")
		}
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, openAIStreamResponseSSE())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAIResponseJSON())
	}))
	defer srv.Close()

	m, err := NewOllamaChatModel(OllamaConfig{Model: "qwen2.5:0.5b", BaseURL: srv.URL, ContextSize: contextSize})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs := []*message.Msg{message.UserMsg("user", "hello")}
	if streaming {
		ch, err := m.ChatStream(ctx, msgs, WithMaxTokens(64))
		if err != nil {
			t.Fatal(err)
		}
		for resp := range ch {
			if resp.Error != nil {
				t.Fatalf("stream error: %v", resp.Error)
			}
		}
	} else if _, err := m.Chat(ctx, msgs, WithMaxTokens(64)); err != nil {
		t.Fatal(err)
	}
	return <-captured
}

// windowModel returns a mock reporting size tokens, or one implementing
// neither ContextSizer nor ModelNamer when size is zero.
func windowModel(name string, size int) ChatModel {
	if size == 0 {
		return &connMockModel{name: name}
	}
	return &sizedMockModel{connMockModel: &connMockModel{name: name}, contextSize: size}
}

func TestResolveContextSize_ConnectivityAwareModel(t *testing.T) {
	tests := []struct {
		name         string
		local, cloud int // 0: the member reports no window
		want         int
	}{
		{"declared local, silent cloud", 8192, 0, 8192},
		{"silent local, small cloud", 0, 32000, 32000},
		{"large local, silent cloud", 262144, 0, testContextFallback},
		{"silent local, large cloud", 0, 200000, testContextFallback},
		{"both declared", 8192, 200000, 8192},
		{"both declared above fallback", 262144, 200000, 200000},
		{"neither declared", 0, 0, testContextFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, err := NewOllamaChatModel(OllamaConfig{Model: "m", ContextSize: tt.local})
			if err != nil {
				t.Fatal(err)
			}
			cam := NewConnectivityAwareModel(local, windowModel("cloud", tt.cloud))
			if got := ResolveContextSize(cam, testContextFallback); got != tt.want {
				t.Errorf("ResolveContextSize = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveContextSize_ConnectivityAwareModelIgnoresCircuitState(t *testing.T) {
	local := windowModel("local", 8192)
	cloud := &sizedMockModel{connMockModel: &connMockModel{name: "cloud", failCount: 2}, contextSize: 200000}
	cam := NewConnectivityAwareModel(local, cloud,
		WithFailureThreshold(2),
		WithRecoveryTimeout(time.Hour),
	)
	check := func(state string) {
		t.Helper()
		if got := ResolveContextSize(cam, testContextFallback); got != 8192 {
			t.Errorf("%s: ResolveContextSize = %d, want 8192", state, got)
		}
	}

	check("closed")

	ctx := context.Background()
	msgs := []*message.Msg{message.UserMsg("user", "hello")}
	for i := 0; i < 2; i++ {
		_, _ = cam.Chat(ctx, msgs)
	}
	if cam.ActiveModel() != "local" {
		t.Fatalf("expected open circuit, got active model %q", cam.ActiveModel())
	}
	check("open")

	// Age the last failure past the recovery timeout instead of sleeping.
	cam.cb.mu.Lock()
	cam.cb.lastFailure = time.Now().Add(-2 * time.Hour)
	cam.cb.mu.Unlock()
	if got := cam.cb.getState(); got != connHalfOpen {
		t.Fatalf("breaker state = %v, want half-open", got)
	}
	check("half-open")
}
