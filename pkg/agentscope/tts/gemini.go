package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// GeminiTTSModel calls the Google Gemini generateContent API with audio
// response modality for text-to-speech synthesis.
type GeminiTTSModel struct {
	apiKey       string
	model        string
	voice        string
	speakingRate float64
	httpClient   *http.Client
}

// GeminiTTSConfig configures GeminiTTSModel.
type GeminiTTSConfig struct {
	APIKey       string
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	Model        string
	Voice        string
	SpeakingRate float64
	HTTPClient   *http.Client
}

// NewGeminiTTSModel creates a TTS model backed by the Gemini generateContent API.
func NewGeminiTTSModel(cfg GeminiTTSConfig) (*GeminiTTSModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("gemini-tts: APIKey is required")
	}
	model := cfg.Model
	if model == "" {
		model = "gemini-2.5-flash-preview-tts"
	}
	voice := cfg.Voice
	if voice == "" {
		voice = "Kore"
	}
	rate := cfg.SpeakingRate
	if rate == 0 {
		rate = 1.0
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	return &GeminiTTSModel{
		apiKey:       apiKey,
		model:        model,
		voice:        voice,
		speakingRate: rate,
		httpClient:   hc,
	}, nil
}

func (m *GeminiTTSModel) ModelName() string {
	return m.model
}

// Gemini API request/response structures for generateContent with audio.

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string         `json:"response_modalities"`
	SpeechConfig       *geminiSpeechCfg `json:"speech_config,omitempty"`
}

type geminiSpeechCfg struct {
	VoiceConfig geminiVoiceConfig `json:"voice_config"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig geminiPrebuiltVoice `json:"prebuilt_voice_config"`
	SpeakingRate        float64             `json:"speaking_rate,omitempty"`
}

type geminiPrebuiltVoice struct {
	VoiceName string `json:"voice_name"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content geminiResponseContent `json:"content"`
}

type geminiResponseContent struct {
	Parts []geminiResponsePart `json:"parts"`
}

type geminiResponsePart struct {
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
	Text       string            `json:"text,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64-encoded
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// Synthesize calls the Gemini generateContent API with audio response modality.
func (m *GeminiTTSModel) Synthesize(ctx context.Context, text string) (*Response, error) {
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{Parts: []geminiPart{{Text: text}}},
		},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: &geminiSpeechCfg{
				VoiceConfig: geminiVoiceConfig{
					PrebuiltVoiceConfig: geminiPrebuiltVoice{
						VoiceName: m.voice,
					},
					SpeakingRate: m.speakingRate,
				},
			},
		},
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("gemini-tts: %w", err)
	}

	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		m.model, m.apiKey,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("gemini-tts: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini-tts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gemini-tts: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var gemResp geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gemResp); err != nil {
		return nil, fmt.Errorf("gemini-tts: decode response: %w", err)
	}

	// Extract audio from the first candidate part that has inlineData.
	for _, cand := range gemResp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				audioBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return nil, fmt.Errorf("gemini-tts: decode audio base64: %w", err)
				}
				result := &Response{
					Content:   audioBytes,
					MediaType: part.InlineData.MimeType,
					IsLast:    true,
				}
				if gemResp.UsageMetadata != nil {
					result.Usage = &Usage{
						InputTokens:  gemResp.UsageMetadata.PromptTokenCount,
						OutputTokens: gemResp.UsageMetadata.CandidatesTokenCount,
					}
				}
				return result, nil
			}
		}
	}

	return nil, fmt.Errorf("gemini-tts: no audio data in response")
}

// SynthesizeStream is not supported for Gemini TTS.
func (m *GeminiTTSModel) SynthesizeStream(_ context.Context, _ string) (<-chan Response, error) {
	return nil, ErrStreamNotSupported
}
