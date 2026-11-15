package streamablehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// newFixtureServer starts a spec-compliant Streamable HTTP server using the
// SDK's own handler. protocolVersions pins the server's advertised versions;
// empty means the SDK default (latest).
func newFixtureServer(t *testing.T, protocolVersions ...string) *httptest.Server {
	t.Helper()

	var opts *sdkmcp.ServerOptions
	if len(protocolVersions) > 0 {
		opts = &sdkmcp.ServerOptions{SupportedProtocolVersions: protocolVersions}
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fixture", Version: "0.1"}, opts)

	server.AddTool(&sdkmcp.Tool{
		Name:        "get_weather",
		Description: "Get current weather",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{
					"type":        "string",
					"description": "City name",
				},
				"unit": map[string]any{
					"type": "string",
					"enum": []any{"c", "f"},
				},
			},
			"required": []any{"city"},
		},
	}, func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "sunny in " + args.City}},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "fail_tool",
		Description: "Always reports an error result",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "it broke"}},
			IsError: true,
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "image_tool",
		Description: "Returns an image",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.ImageContent{Data: []byte("hello"), MIMEType: "image/png"}},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "audio_tool",
		Description: "Returns audio (no block mapping)",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.AudioContent{Data: []byte("wav"), MIMEType: "audio/wav"}},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "structured_tool",
		Description: "Returns only structured content",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			StructuredContent: map[string]any{"city": "Beijing", "temp_c": 22, "note": "a<b>&c"},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "structured_text_tool",
		Description: "Returns text plus structured content",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: "22C in Beijing"}},
			StructuredContent: map[string]any{"temp_c": 22},
		}, nil
	})

	server.AddTool(&sdkmcp.Tool{
		Name:        "structured_audio_tool",
		Description: "Returns audio plus structured content",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.AudioContent{Data: []byte("wav"), MIMEType: "audio/wav"}},
			StructuredContent: map[string]any{"transcript": "hi"},
		}, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// newElicitFixtureServer serves a single tool that elicits a name mid-call
// and greets with it, so elicitation tests exercise the full server-initiated
// round trip without touching the shared fixture's tool list.
func newElicitFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "elicit-fixture", Version: "0.1"}, nil)
	server.AddTool(&sdkmcp.Tool{
		Name:        "elicit_name",
		Description: "Elicits a name mid-call and greets with it",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		res, err := req.Session.Elicit(ctx, &sdkmcp.ElicitParams{
			Message: "Your name?",
			RequestedSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
			},
		})
		if err != nil {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "elicit error: " + err.Error()}},
				IsError: true,
			}, nil
		}
		name, _ := res.Content["name"].(string)
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "action:" + res.Action + " name:" + name}},
		}, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func mustClient(t *testing.T, srv *httptest.Server, cfg *Config) *Client {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.BaseURL = srv.URL
	c, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewClient_RequiresBaseURL(t *testing.T) {
	if _, err := NewClient(context.Background(), &Config{}); err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
}

// TestListTools_Mapping proves the SDK tool shape maps to model.ToolSchema,
// including nested parameters (properties, enum, required).
func TestListTools_Mapping(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	schemas, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 7 {
		t.Fatalf("got %d tools, want 7", len(schemas))
	}

	var found bool
	for _, s := range schemas {
		if s.Function.Name != "get_weather" {
			continue
		}
		found = true
		if s.Type != "function" {
			t.Errorf("Type = %q", s.Type)
		}
		if s.Function.Description != "Get current weather" {
			t.Errorf("Description = %q", s.Function.Description)
		}
		var schema struct {
			Type       string `json:"type"`
			Properties struct {
				City struct {
					Type string `json:"type"`
				} `json:"city"`
				Unit struct {
					Enum []string `json:"enum"`
				} `json:"unit"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(s.Function.Parameters, &schema); err != nil {
			t.Fatalf("parameters not valid JSON: %v", err)
		}
		if schema.Type != "object" || schema.Properties.City.Type != "string" {
			t.Errorf("nested parameters not preserved: %s", s.Function.Parameters)
		}
		if len(schema.Properties.Unit.Enum) != 2 || len(schema.Required) != 1 || schema.Required[0] != "city" {
			t.Errorf("enum/required not preserved: %s", s.Function.Parameters)
		}
	}
	if !found {
		t.Fatal("get_weather missing from ListTools")
	}
}

func TestCallTool_TextResult(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "get_weather", map[string]any{"city": "Beijing"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("State = %q", resp.State)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("Content = %+v", resp.Content)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "sunny in Beijing" {
		t.Errorf("Content[0] = %+v", resp.Content[0])
	}
}

// TestCallTool_ErrorResult proves an isError tool result maps to
// ToolResultError with the error text preserved (not a Go error).
func TestCallTool_ErrorResult(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "fail_tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("State = %q, want error", resp.State)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "it broke" {
		t.Errorf("Content[0] = %+v", resp.Content[0])
	}
}

// TestCallTool_ImageResult proves image content maps to a DataBlock with the
// base64 re-encoded (the SDK decodes the wire base64 into raw bytes).
func TestCallTool_ImageResult(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "image_tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("Content = %+v", resp.Content)
	}
	db, ok := resp.Content[0].(message.DataBlock)
	if !ok {
		t.Fatalf("Content[0] = %T, want DataBlock", resp.Content[0])
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok || src.MediaType != "image/png" || src.Data != "aGVsbG8=" {
		t.Errorf("Source = %+v, want base64 image/png aGVsbG8=", db.Source)
	}
}

// TestCallTool_UnmappedContent proves content without a block mapping (audio,
// resources) stays visible as text instead of being dropped.
func TestCallTool_UnmappedContent(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "audio_tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("Content = %+v", resp.Content)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || !strings.Contains(tb.Text, "audio/wav") {
		t.Errorf("Content[0] = %+v, want text carrying the audio payload", resp.Content[0])
	}
}

// TestCallTool_StructuredContentFallback proves a result carrying only
// structured content stays visible: the converter serializes it into a text
// block instead of dropping it.
func TestCallTool_StructuredContentFallback(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "structured_tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("State = %q", resp.State)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("Content = %+v", resp.Content)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("Content[0] = %T, want TextBlock", resp.Content[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(tb.Text), &decoded); err != nil {
		t.Fatalf("fallback text is not JSON: %v", err)
	}
	if decoded["city"] != "Beijing" || decoded["temp_c"] != float64(22) {
		t.Errorf("decoded = %+v, want the structured payload", decoded)
	}
	if !strings.Contains(tb.Text, "a<b>&c") {
		t.Errorf("text = %q, want HTML characters unescaped", tb.Text)
	}
}

// TestCallTool_StructuredContentSkippedWhenTextPresent proves the fallback
// does not duplicate structured content the server already serialized into a
// text block.
func TestCallTool_StructuredContentSkippedWhenTextPresent(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "structured_text_tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("Content = %+v, want only the original text block", resp.Content)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "22C in Beijing" {
		t.Errorf("Content[0] = %+v", resp.Content[0])
	}
}

// TestCallTool_StructuredContentWithAudioOnly proves non-text content does
// not suppress the structured fallback: audio plus structured content yields
// the textified audio block and the structured JSON text.
func TestCallTool_StructuredContentWithAudioOnly(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "structured_audio_tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("Content = %+v, want audio text plus structured fallback", resp.Content)
	}
	tb, ok := resp.Content[1].(message.TextBlock)
	if !ok {
		t.Fatalf("Content[1] = %T, want TextBlock", resp.Content[1])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(tb.Text), &decoded); err != nil {
		t.Fatalf("fallback text is not JSON: %v", err)
	}
	if decoded["transcript"] != "hi" {
		t.Errorf("decoded = %+v, want the structured payload", decoded)
	}
}

// TestDualEra_Negotiation connects with automatic negotiation to a server
// pinned to the old spec era (2025-03-26) and to a default (latest-era)
// server, proving both generations work through the same client.
func TestDualEra_Negotiation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions []string
	}{
		{"old_era_2025_03_26", []string{"2025-03-26"}},
		{"latest_era_default", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFixtureServer(t, tc.versions...)
			c := mustClient(t, srv, nil)

			if _, err := c.ListTools(context.Background()); err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			resp, err := c.CallTool(context.Background(), "get_weather", map[string]any{"city": "Shanghai"})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if tb, ok := resp.Content[0].(message.TextBlock); !ok || tb.Text != "sunny in Shanghai" {
				t.Errorf("Content[0] = %+v", resp.Content[0])
			}
		})
	}
}

// TestPinnedProtocolVersion pins the client's offered version to the old era
// against a default server (which supports both).
func TestPinnedProtocolVersion(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, &Config{ProtocolVersion: "2025-03-26"})
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools with pinned version: %v", err)
	}
}

// TestHeaders proves configured headers reach the server and the SDK sends
// the MCP-Protocol-Version header after initialization. The recording
// transport sits client-side, so it observes exactly what goes on the wire.
func TestHeaders(t *testing.T) {
	rt := &recordingTransport{base: http.DefaultTransport}
	srv := newFixtureServer(t)
	c := mustClient(t, srv, &Config{
		HTTPClient: &http.Client{Transport: rt},
		Headers:    map[string]string{"Authorization": "Bearer test-token"},
	})
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.authSeen {
		t.Error("configured Authorization header did not reach the wire")
	}
	if !rt.versionSeen {
		t.Error("MCP-Protocol-Version header not observed on any request")
	}
}

type recordingTransport struct {
	base                  http.RoundTripper
	mu                    sync.Mutex
	authSeen, versionSeen bool
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if req.Header.Get("Authorization") == "Bearer test-token" {
		t.authSeen = true
	}
	if req.Header.Get("MCP-Protocol-Version") != "" {
		t.versionSeen = true
	}
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}

// TestLifecycle covers concurrent CallTool, idempotent Close, calls after
// Close failing, and the handshake-only context contract.
func TestLifecycle(t *testing.T) {
	srv := newFixtureServer(t)

	// The connect ctx governs the handshake only: canceling it afterwards
	// must not kill the session.
	ctx, cancel := context.WithCancel(context.Background())
	c, err := NewClient(ctx, &Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("session died with the handshake ctx: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.CallTool(context.Background(), "get_weather", map[string]any{"city": "x"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent CallTool: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}
	if _, err := c.CallTool(context.Background(), "get_weather", map[string]any{"city": "x"}); err == nil {
		t.Fatal("CallTool after Close must fail")
	}
}

// TestClose_IdempotentConcurrent exercises Close racing with itself.
func TestClose_IdempotentConcurrent(t *testing.T) {
	srv := newFixtureServer(t)
	c := mustClient(t, srv, nil)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Close()
		}()
	}
	wg.Wait()
}

// TestSDKConnectionClosedError documents the post-Close error shape: it wraps
// the SDK's connection-closed error, so errors.As-style checks keep working.
func TestSDKConnectionClosedError(t *testing.T) {
	srv := newFixtureServer(t)
	c, err := NewClient(context.Background(), &Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	_, err = c.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error after Close")
	}
	if !errors.Is(err, sdkmcp.ErrConnectionClosed) {
		t.Errorf("error should wrap sdkmcp.ErrConnectionClosed, got: %v", err)
	}
}

// TestElicitation_Accept runs the full server-initiated elicitation round
// trip: the fixture server elicits mid tools/call, the client-side handler
// resolves it, and the accepted content reaches the server.
func TestElicitation_Accept(t *testing.T) {
	srv := newElicitFixtureServer(t)

	var gotReq mcp.ElicitRequest
	c := mustClient(t, srv, &Config{
		ElicitationHandler: func(_ context.Context, req mcp.ElicitRequest) (mcp.ElicitResponse, error) {
			gotReq = req
			return mcp.ElicitResponse{
				Action:  mcp.ElicitAccept,
				Content: map[string]any{"name": "ada"},
			}, nil
		},
	})

	resp, err := c.CallTool(context.Background(), "elicit_name", nil)
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "action:accept name:ada" {
		t.Errorf("Content[0] = %+v", resp.Content[0])
	}
	if gotReq.Message != "Your name?" {
		t.Errorf("handler message = %q, want %q", gotReq.Message, "Your name?")
	}
	if !strings.Contains(string(gotReq.RequestedSchema), `"name"`) {
		t.Errorf("handler schema = %s, want it to carry the requested schema", gotReq.RequestedSchema)
	}
}

func TestElicitation_Decline(t *testing.T) {
	srv := newElicitFixtureServer(t)
	c := mustClient(t, srv, &Config{
		ElicitationHandler: func(_ context.Context, _ mcp.ElicitRequest) (mcp.ElicitResponse, error) {
			return mcp.ElicitResponse{Action: mcp.ElicitDecline}, nil
		},
	})

	resp, err := c.CallTool(context.Background(), "elicit_name", nil)
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "action:decline name:" {
		t.Errorf("Content[0] = %+v", resp.Content[0])
	}
}

// TestElicitation_NoHandler: without a handler the client never declares the
// capability, and the server's elicitation attempt fails fast instead of
// hanging the in-flight tool call.
func TestElicitation_NoHandler(t *testing.T) {
	srv := newElicitFixtureServer(t)
	c := mustClient(t, srv, nil)

	resp, err := c.CallTool(context.Background(), "elicit_name", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("State = %q, want error", resp.State)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || !strings.Contains(tb.Text, "elicit error:") {
		t.Errorf("Content[0] = %+v", resp.Content[0])
	}
}
