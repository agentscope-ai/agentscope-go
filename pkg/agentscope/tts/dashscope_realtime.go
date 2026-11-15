package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/sirupsen/logrus"
)

const dashScopeRealtimeTTSURL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text2audio/generation"

// DashScopeRealtimeConfig configures a DashScope realtime TTS model.
type DashScopeRealtimeConfig struct {
	APIKey       string
	SecretAPIKey model.SecretStr // Preferred over APIKey. Use model.NewSecretStr(key).
	Model        string          // e.g. "cosyvoice-v2"
	Voice        string          // e.g. "longxiaochun"
	Format       string          // e.g. "pcm", "mp3"
	BaseURL      string
}

// DashScopeRealtimeTTS implements RealtimeModel using DashScope's streaming API.
// It buffers text chunks and flushes them as streaming synthesis requests.
type DashScopeRealtimeTTS struct {
	cfg       DashScopeRealtimeConfig
	client    *http.Client
	connected bool
	buffer    []string
	mu        sync.Mutex
}

// NewDashScopeRealtimeTTS creates a realtime TTS model.
func NewDashScopeRealtimeTTS(cfg *DashScopeRealtimeConfig) *DashScopeRealtimeTTS {
	cfg.APIKey = model.ResolveAPIKey(cfg.APIKey, cfg.SecretAPIKey)
	if cfg.Model == "" {
		cfg.Model = "cosyvoice-v2"
	}
	if cfg.Format == "" {
		cfg.Format = "mp3"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = dashScopeRealtimeTTSURL
	}
	return &DashScopeRealtimeTTS{
		cfg:    *cfg,
		client: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (t *DashScopeRealtimeTTS) ModelName() string { return t.cfg.Model }

func (t *DashScopeRealtimeTTS) Synthesize(ctx context.Context, text string) (*Response, error) {
	return t.synthesizeHTTP(ctx, text)
}

func (t *DashScopeRealtimeTTS) SynthesizeStream(ctx context.Context, text string) (<-chan Response, error) {
	ch := make(chan Response, 8)
	go func() {
		defer close(ch)
		resp, err := t.synthesizeHTTP(ctx, text)
		if err != nil {
			logrus.WithError(err).Error("dashscope realtime tts: synthesize failed")
			return
		}
		resp.IsLast = true
		ch <- *resp
	}()
	return ch, nil
}

func (t *DashScopeRealtimeTTS) Connect(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connected = true
	t.buffer = nil
	return nil
}

func (t *DashScopeRealtimeTTS) Push(ctx context.Context, text string) (*Response, error) {
	t.mu.Lock()
	if !t.connected {
		t.mu.Unlock()
		return nil, fmt.Errorf("dashscope realtime tts: not connected")
	}
	t.mu.Unlock()

	return t.synthesizeHTTP(ctx, text)
}

func (t *DashScopeRealtimeTTS) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connected = false
	t.buffer = nil
	return nil
}

func (t *DashScopeRealtimeTTS) synthesizeHTTP(ctx context.Context, text string) (*Response, error) {
	body := map[string]any{
		"model": t.cfg.Model,
		"input": map[string]any{
			"text": text,
		},
		"parameters": map[string]any{
			"format": t.cfg.Format,
		},
	}
	if t.cfg.Voice != "" {
		body["parameters"].(map[string]any)["voice"] = t.cfg.Voice
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.BaseURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	req.Header.Set("X-DashScope-DataInspection", "enable")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashscope realtime tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dashscope realtime tts: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dashscope realtime tts: HTTP %d: %s", resp.StatusCode, string(audioData))
	}

	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "audio/" + t.cfg.Format
	}

	return &Response{
		Content:   audioData,
		MediaType: mediaType,
		IsLast:    false,
	}, nil
}

// Compile-time checks.
var _ RealtimeModel = (*DashScopeRealtimeTTS)(nil)
