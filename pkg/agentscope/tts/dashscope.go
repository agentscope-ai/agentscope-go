package tts

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/internal/httpx"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

const (
	defaultTTSSampleRate    = 24000
	defaultTTSChannels      = 1
	defaultTTSBitsPerSample = 16
	defaultTTSTimeout       = 60 * time.Second
)

// DashScopeConfig holds configuration for the DashScope TTS model.
type DashScopeConfig struct {
	APIKey       string
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	BaseURL      string          // default: https://dashscope.aliyuncs.com
	Model        string          // e.g. "qwen3-tts-flash"
	Voice        string          // e.g. "Cherry"
	HTTPClient   *http.Client
}

// DashScopeTTSModel implements the Model interface using DashScope's
// multimodal generation API for text-to-speech synthesis.
type DashScopeTTSModel struct {
	apiKey  string
	baseURL string
	model   string
	voice   string
	client  *http.Client
}

// NewDashScopeTTSModel creates a DashScope TTS model.
func NewDashScopeTTSModel(cfg DashScopeConfig) (*DashScopeTTSModel, error) { //nolint:gocritic // stable API: value receiver for backward compat
	apiKey := model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("tts: DashScope API key is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("tts: model is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://dashscope.aliyuncs.com"
	}
	if cfg.Voice == "" {
		cfg.Voice = "Cherry"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTTSTimeout}
	}
	return &DashScopeTTSModel{
		apiKey:  apiKey,
		baseURL: cfg.BaseURL,
		model:   cfg.Model,
		voice:   cfg.Voice,
		client:  client,
	}, nil
}

func (m *DashScopeTTSModel) ModelName() string { return m.model }

// Synthesize generates audio for the given text in a single request.
func (m *DashScopeTTSModel) Synthesize(ctx context.Context, text string) (*Response, error) {
	url := m.baseURL + "/api/v1/services/aigc/multimodal-generation/generation"

	reqBody := dashScopeTTSRequest{
		Model: m.model,
		Input: dashScopeTTSInput{
			Messages: []dashScopeTTSMessage{
				{Role: "user", Content: []dashScopeTTSContent{{Text: text}}},
			},
		},
		Parameters: dashScopeTTSParams{
			Voice: m.voice,
		},
	}

	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + m.apiKey,
	}

	start := time.Now()
	var resp dashScopeTTSResponse
	if err := httpx.DoJSONRequest(ctx, m.client, http.MethodPost, url, reqBody, &resp, headers); err != nil {
		return nil, fmt.Errorf("DashScope TTS API call failed: %w", err)
	}

	audioData := resp.Output.Audio.Data
	if audioData == "" {
		return &Response{Content: []byte{}, MediaType: "audio/wav", IsLast: true}, nil
	}

	pcm, err := base64.StdEncoding.DecodeString(audioData)
	if err != nil {
		return nil, fmt.Errorf("decode audio: %w", err)
	}

	wav := wrapPCMInWAV(pcm, defaultTTSSampleRate, defaultTTSChannels, defaultTTSBitsPerSample)

	var usage *Usage
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		usage = &Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			Time:         time.Since(start).Seconds(),
		}
	}

	return &Response{
		Content:   wav,
		MediaType: "audio/wav",
		IsLast:    true,
		Usage:     usage,
	}, nil
}

// SynthesizeStream is not supported for the non-realtime DashScope model.
func (m *DashScopeTTSModel) SynthesizeStream(_ context.Context, _ string) (<-chan Response, error) {
	return nil, ErrStreamNotSupported
}

// --- wire types ---

type dashScopeTTSRequest struct {
	Model      string             `json:"model"`
	Input      dashScopeTTSInput  `json:"input"`
	Parameters dashScopeTTSParams `json:"parameters"`
}

type dashScopeTTSInput struct {
	Messages []dashScopeTTSMessage `json:"messages"`
}

type dashScopeTTSMessage struct {
	Role    string                `json:"role"`
	Content []dashScopeTTSContent `json:"content"`
}

type dashScopeTTSContent struct {
	Text string `json:"text"`
}

type dashScopeTTSParams struct {
	Voice string `json:"voice"`
}

type dashScopeTTSResponse struct {
	Output struct {
		Audio struct {
			Data string `json:"data"`
		} `json:"audio"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// wrapPCMInWAV wraps raw PCM audio data in a proper WAV container.
func wrapPCMInWAV(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	dataSize := len(pcm)
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	buf := make([]byte, 44+dataSize)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataSize))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // PCM chunk size
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // PCM format
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:36], uint16(bitsPerSample))
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))
	copy(buf[44:], pcm)
	return buf
}

// streamingWAVHeader returns a 44-byte WAV header with unknown data size
// (0xFFFFFFFF), suitable for streaming audio where the total length is unknown.
