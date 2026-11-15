// Package streamablehttp implements the mcp.Client interface over the MCP
// Streamable HTTP transport (spec 2025-03-26 and later), backed by the
// official Model Context Protocol Go SDK. It lives in its own subpackage so
// the SDK dependency stays opt-in: importers of pkg/agentscope/mcp alone do
// not compile the SDK. Use it for spec-compliant Streamable HTTP servers; the
// bare JSON-RPC POST transport in pkg/agentscope/mcp (HttpClient) remains for
// simple 2024-11-05-era servers.
package streamablehttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// Config configures a Streamable HTTP MCP connection.
type Config struct {
	BaseURL string // Required. The MCP endpoint URL.
	// HTTPClient is an optional custom HTTP client. Its Timeout, when set,
	// caps every request, including long-lived SSE streams opened by the SDK.
	HTTPClient *http.Client
	// Headers are applied to every request (e.g. Authorization), unless the
	// SDK already set the same header.
	Headers map[string]string
	// ProtocolVersion pins the version offered in the initialize handshake
	// (e.g. "2025-03-26"). Empty negotiates automatically from the SDK's
	// supported versions. Exposed for testing and debugging.
	ProtocolVersion string
	// ElicitationHandler, when non-nil, resolves elicitation/create requests
	// the server sends mid-call; the SDK then declares the elicitation
	// capability automatically. Nil disables elicitation: a server that
	// still sends elicitation/create gets an error reply.
	ElicitationHandler mcp.ElicitationHandler
}

// Client communicates with an MCP server over Streamable HTTP. Session state
// (Mcp-Session-Id, protocol version negotiation) is managed by the SDK.
// CallTool is safe for concurrent use: the SDK correlates in-flight requests
// internally. Close is idempotent and concurrency safe.
type Client struct {
	session *sdkmcp.ClientSession
}

var _ mcp.Client = (*Client)(nil)

// NewClient connects to the server and initializes the MCP session. The
// passed ctx governs the connect handshake only, not the session lifetime:
// the SDK detaches the connection from it, so request-scoped contexts are
// safe to pass here. The session lives until Close.
func NewClient(ctx context.Context, cfg *Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("streamablehttp: BaseURL is required")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if len(cfg.Headers) > 0 {
		httpClient = withHeaders(httpClient, cfg.Headers)
	}

	transport := &sdkmcp.StreamableClientTransport{
		Endpoint:   cfg.BaseURL,
		HTTPClient: httpClient,
	}
	var clientOpts *sdkmcp.ClientOptions
	if cfg.ElicitationHandler != nil {
		clientOpts = &sdkmcp.ClientOptions{
			ElicitationHandler: adaptElicitationHandler(cfg.ElicitationHandler),
		}
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agentscope-go", Version: "2.0"}, clientOpts)
	var opts *sdkmcp.ClientSessionOptions
	if cfg.ProtocolVersion != "" {
		opts = &sdkmcp.ClientSessionOptions{ProtocolVersion: cfg.ProtocolVersion}
	}
	session, err := client.Connect(ctx, transport, opts)
	if err != nil {
		return nil, fmt.Errorf("streamablehttp: connect: %w", err)
	}
	return &Client{session: session}, nil
}

// adaptElicitationHandler bridges the SDK's elicitation callback to the
// transport-neutral mcp.ElicitationHandler. The SDK validates the accepted
// content against the requested schema before replying to the server.
func adaptElicitationHandler(h mcp.ElicitationHandler) func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
	return func(ctx context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		var elicitReq mcp.ElicitRequest
		if req.Params != nil {
			elicitReq.Message = req.Params.Message
			if req.Params.RequestedSchema != nil {
				// The SDK decoded the schema into a generic value; re-marshal
				// to carry it verbatim.
				schema, err := json.Marshal(req.Params.RequestedSchema)
				if err != nil {
					return nil, fmt.Errorf("streamablehttp: marshal requested schema: %w", err)
				}
				elicitReq.RequestedSchema = schema
			}
		}
		res, err := h(ctx, elicitReq)
		if err != nil {
			return nil, err
		}
		action := res.Action
		switch action {
		case mcp.ElicitAccept, mcp.ElicitDecline, mcp.ElicitCancel:
		default:
			// A handler bug must not put an invalid action on the wire;
			// treat it as a dismissal.
			action = mcp.ElicitCancel
		}
		result := &sdkmcp.ElicitResult{Action: action}
		if action == mcp.ElicitAccept {
			result.Content = res.Content
		}
		return result, nil
	}
}

// ListTools queries the MCP server for available tools.
func (c *Client) ListTools(ctx context.Context) ([]model.ToolSchema, error) {
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("streamablehttp: list tools: %w", err)
	}

	schemas := make([]model.ToolSchema, 0, len(result.Tools))
	for _, t := range result.Tools {
		// Client-side InputSchema holds the default JSON unmarshaling of the
		// server's schema (a map[string]any); re-marshal to carry it as-is.
		params, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("streamablehttp: marshal input schema of %q: %w", t.Name, err)
		}
		if t.InputSchema == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		schemas = append(schemas, model.ToolSchema{
			Type: "function",
			Function: model.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return schemas, nil
}

// CallTool invokes a tool on the MCP server.
func (c *Client) CallTool(ctx context.Context, name string, input map[string]any) (*tool.ToolResponse, error) {
	result, err := c.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: input,
	})
	if err != nil {
		return nil, fmt.Errorf("streamablehttp: call tool %q: %w", name, err)
	}
	return convertResult(result), nil
}

// convertResult maps an SDK tool result to a ToolResponse, mirroring the
// StdioClient conversion: text and image content blocks, IsError to the
// result state, and a guaranteed non-empty block list. Structured content
// without any wire text content is surfaced as JSON text instead of being
// dropped.
func convertResult(result *sdkmcp.CallToolResult) *tool.ToolResponse {
	var blocks []message.ContentBlock
	hasText := false
	for _, content := range result.Content {
		switch c := content.(type) {
		case *sdkmcp.TextContent:
			hasText = true
			blocks = append(blocks, message.TextBlock{Type: "text", Text: c.Text})
		case *sdkmcp.ImageContent:
			// The SDK decodes the wire base64 into raw bytes; Base64Source
			// wants the encoded form again.
			blocks = append(blocks, message.DataBlock{
				Type: "data",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      base64.StdEncoding.EncodeToString(c.Data),
					MediaType: c.MIMEType,
				},
			})
		default:
			// Audio, resource links and embedded resources have no block
			// mapping here; keep them visible as text instead of dropping.
			if data, err := json.Marshal(c); err == nil {
				blocks = append(blocks, message.TextBlock{Type: "text", Text: string(data)})
			}
		}
	}

	if result.StructuredContent != nil && !hasText {
		if text := marshalStructured(result.StructuredContent); text != "" {
			blocks = append(blocks, message.TextBlock{Type: "text", Text: text})
		}
	}

	state := message.ToolResultSuccess
	if result.IsError {
		state = message.ToolResultError
	}
	if len(blocks) == 0 {
		blocks = []message.ContentBlock{message.TextBlock{Type: "text", Text: ""}}
	}
	return &tool.ToolResponse{Content: blocks, State: state}
}

// marshalStructured renders structured content as JSON without HTML escaping,
// matching how the StdioClient converter passes the wire JSON through.
func marshalStructured(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// Close terminates the session, including the SDK's best-effort session
// DELETE. It is idempotent and safe to call concurrently.
func (c *Client) Close() error {
	return c.session.Close()
}

// headerTransport injects configured headers into every request, without
// overriding headers the SDK already set (such as protocol headers).
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(req)
}

// withHeaders returns a shallow clone of c whose transport injects headers.
func withHeaders(c *http.Client, headers map[string]string) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *c
	clone.Transport = headerTransport{headers: headers, base: base}
	return &clone
}
