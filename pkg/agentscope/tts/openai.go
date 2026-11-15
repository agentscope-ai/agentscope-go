package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// OpenAITTSModel calls OpenAI Audio Speech API (/v1/audio/speech).
type OpenAITTSModel struct {
	apiKey         string
	baseURL        string
	model          string
	voice          string
	responseFormat string
	instructions   string
	httpClient     *http.Client
	defaultHeaders map[string]string
}

// OpenAITTSConfig configures OpenAITTSModel.
type OpenAITTSConfig struct {
	APIKey         string
	SecretAPIKey   model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL        string
	Model          string
	Voice          string
	ResponseFormat string
	Instructions   string
	HTTPClient     *http.Client
	DefaultHeaders map[string]string
}

// NewOpenAITTSModel creates a TTS model backed by OpenAI Audio Speech API.
func NewOpenAITTSModel(cfg *OpenAITTSConfig) (*OpenAITTSModel, error) {
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("openai-tts: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai-tts: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	voice := cfg.Voice
	if voice == "" {
		voice = "alloy"
	}
	fmtStr := cfg.ResponseFormat
	if fmtStr == "" {
		fmtStr = "mp3"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &OpenAITTSModel{
		apiKey:         apiKey,
		baseURL:        base,
		model:          cfg.Model,
		voice:          voice,
		responseFormat: fmtStr,
		instructions:   cfg.Instructions,
		httpClient:     hc,
		defaultHeaders: cfg.DefaultHeaders,
	}, nil
}

func (m *OpenAITTSModel) ModelName() string {
	return m.model
}

type openAISpeechRequest struct {
	Model          string `json:"model"`
	Voice          string `json:"voice"`
	Input          string `json:"input"`
	ResponseFormat string `json:"response_format,omitempty"`
	Instructions   string `json:"instructions,omitempty"`
}

// mediaTypeForFormat maps response_format to MIME type.
func mediaTypeForFormat(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "audio/mpeg"
	}
}

// Synthesize calls the OpenAI Audio Speech API and returns the audio as a single Response.
func (m *OpenAITTSModel) Synthesize(ctx context.Context, text string) (*Response, error) {
	reqBody := openAISpeechRequest{
		Model:          m.model,
		Voice:          m.voice,
		Input:          text,
		ResponseFormat: m.responseFormat,
		Instructions:   m.instructions,
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai-tts: %w", err)
	}

	url := m.baseURL + "/v1/audio/speech"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("openai-tts: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	for k, v := range m.defaultHeaders {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai-tts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai-tts: HTTP %d: %s", resp.StatusCode, string(body))
	}

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai-tts: read body: %w", err)
	}
	return &Response{
		Content:   audioBytes,
		MediaType: mediaTypeForFormat(m.responseFormat),
		IsLast:    true,
	}, nil
}

// SynthesizeStream calls the API with streaming and returns chunks.
// OpenAI TTS supports streaming via chunked transfer encoding.
func (m *OpenAITTSModel) SynthesizeStream(ctx context.Context, text string) (<-chan Response, error) {
	reqBody := openAISpeechRequest{
		Model:          m.model,
		Voice:          m.voice,
		Input:          text,
		ResponseFormat: m.responseFormat,
		Instructions:   m.instructions,
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai-tts: %w", err)
	}

	url := m.baseURL + "/v1/audio/speech"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("openai-tts: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	for k, v := range m.defaultHeaders {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai-tts: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai-tts: HTTP %d: %s", resp.StatusCode, string(body))
	}

	out := make(chan Response, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case out <- Response{
					Content:   chunk,
					MediaType: mediaTypeForFormat(m.responseFormat),
					IsLast:    false,
				}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				// Send terminal marker on both EOF and non-EOF errors so the
				// consumer knows the stream has ended.
				select {
				case out <- Response{
					Content:   nil,
					MediaType: mediaTypeForFormat(m.responseFormat),
					IsLast:    true,
				}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	return out, nil
}
