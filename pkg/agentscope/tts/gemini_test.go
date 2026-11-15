package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewGeminiTTSModel_EmptyAPIKey(t *testing.T) {
	_, err := NewGeminiTTSModel(GeminiTTSConfig{APIKey: ""})
	if err == nil {
		t.Fatal("expected error for empty API key, got nil")
	}
	if err.Error() != "gemini-tts: APIKey is required" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestNewGeminiTTSModel_Defaults(t *testing.T) {
	m, err := NewGeminiTTSModel(GeminiTTSConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ModelName() != "gemini-2.5-flash-preview-tts" {
		t.Errorf("expected default model 'gemini-2.5-flash-preview-tts', got %q", m.ModelName())
	}
	if m.voice != "Kore" {
		t.Errorf("expected default voice 'Kore', got %q", m.voice)
	}
	if m.speakingRate != 1.0 {
		t.Errorf("expected default speakingRate 1.0, got %f", m.speakingRate)
	}
}

func TestNewGeminiTTSModel_CustomValues(t *testing.T) {
	m, err := NewGeminiTTSModel(GeminiTTSConfig{
		APIKey:       "key-123",
		Model:        "custom-model",
		Voice:        "Zephyr",
		SpeakingRate: 1.5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ModelName() != "custom-model" {
		t.Errorf("expected model 'custom-model', got %q", m.ModelName())
	}
	if m.voice != "Zephyr" {
		t.Errorf("expected voice 'Zephyr', got %q", m.voice)
	}
	if m.speakingRate != 1.5 {
		t.Errorf("expected speakingRate 1.5, got %f", m.speakingRate)
	}
}

func TestGeminiTTSModel_Synthesize_MockServer(t *testing.T) {
	audioPayload := []byte("fake-audio-pcm-data")
	b64Audio := base64.StdEncoding.EncodeToString(audioPayload)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}

		// Decode and verify request body
		var reqBody geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if len(reqBody.Contents) == 0 || len(reqBody.Contents[0].Parts) == 0 {
			t.Error("request missing content parts")
		}
		if reqBody.Contents[0].Parts[0].Text != "Hello world" {
			t.Errorf("expected text 'Hello world', got %q", reqBody.Contents[0].Parts[0].Text)
		}

		resp := geminiResponse{
			Candidates: []geminiCandidate{
				{
					Content: geminiResponseContent{
						Parts: []geminiResponsePart{
							{
								InlineData: &geminiInlineData{
									MimeType: "audio/L16;rate=24000",
									Data:     b64Audio,
								},
							},
						},
					},
				},
			},
			UsageMetadata: &geminiUsage{
				PromptTokenCount:     10,
				CandidatesTokenCount: 200,
				TotalTokenCount:      210,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Create model pointing at mock server.
	// We need to override the URL, but the implementation builds the URL internally.
	// Instead, we create a custom HTTP client with a transport that redirects.
	transport := &urlRewriteTransport{
		base:      http.DefaultTransport,
		targetURL: server.URL,
	}

	m, err := NewGeminiTTSModel(GeminiTTSConfig{
		APIKey: "test-key",
		HTTPClient: &http.Client{
			Transport: transport,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error creating model: %v", err)
	}

	resp, err := m.Synthesize(context.Background(), "Hello world")
	if err != nil {
		t.Fatalf("Synthesize error: %v", err)
	}

	if !bytes.Equal(resp.Content, audioPayload) {
		t.Errorf("expected audio content %q, got %q", audioPayload, resp.Content)
	}
	if resp.MediaType != "audio/L16;rate=24000" {
		t.Errorf("expected media type 'audio/L16;rate=24000', got %q", resp.MediaType)
	}
	if !resp.IsLast {
		t.Error("expected IsLast to be true")
	}
	if resp.Usage == nil {
		t.Fatal("expected usage metadata")
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 200 {
		t.Errorf("expected 200 output tokens, got %d", resp.Usage.OutputTokens)
	}
}

func TestGeminiTTSModel_Synthesize_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "server error"}`))
	}))
	defer server.Close()

	m, err := NewGeminiTTSModel(GeminiTTSConfig{
		APIKey: "test-key",
		HTTPClient: &http.Client{
			Transport: &urlRewriteTransport{
				base:      http.DefaultTransport,
				targetURL: server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = m.Synthesize(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !contains(err.Error(), "HTTP 500") {
		t.Errorf("expected error to mention HTTP 500, got: %v", err)
	}
}

func TestGeminiTTSModel_SynthesizeStream_NotSupported(t *testing.T) {
	m, err := NewGeminiTTSModel(GeminiTTSConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = m.SynthesizeStream(context.Background(), "test")
	if err == nil {
		t.Fatal("expected ErrStreamNotSupported")
	}
	if !errors.Is(err, ErrStreamNotSupported) {
		t.Errorf("expected ErrStreamNotSupported, got: %v", err)
	}
}

// --- helpers ---

// urlRewriteTransport redirects all requests to a target URL (httptest server).
type urlRewriteTransport struct {
	base      http.RoundTripper
	targetURL string
}

func (t *urlRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite the URL to point at the test server, preserving path and query.
	req.URL.Scheme = "http"
	req.URL.Host = t.targetURL[len("http://"):]
	return t.base.RoundTrip(req)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
