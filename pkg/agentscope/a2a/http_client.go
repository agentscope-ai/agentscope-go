package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// HTTPClient is a reference implementation that calls a remote agent service via HTTP/JSON.
// Protocol convention:
//
//	POST {Endpoint} with body {"messages":[...]}, returns {"reply": {...}}.
type HTTPClient struct {
	Endpoint   string
	HTTPClient *http.Client
}

func (c *HTTPClient) Call(ctx context.Context, msgs []*message.Msg) (*message.Msg, error) {
	if c.Endpoint == "" {
		return nil, fmt.Errorf("a2a: empty endpoint")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}

	payload := map[string]any{"messages": msgs}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("a2a: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("a2a: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("a2a: unexpected status %d", resp.StatusCode)
	}

	var data struct {
		Reply *message.Msg `json:"reply"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("a2a: decode response: %w", err)
	}
	return data.Reply, nil
}
