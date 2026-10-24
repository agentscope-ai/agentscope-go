package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// mcpNameRegexp validates MCP server names. Names must consist of
// alphanumeric characters, hyphens, and underscores only.
var mcpNameRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateMCPName checks whether the given name is a valid MCP server name.
// Valid names match the pattern ^[a-zA-Z0-9_-]+$.
func ValidateMCPName(name string) error {
	if name == "" {
		return fmt.Errorf("mcp: name must not be empty")
	}
	if !mcpNameRegexp.MatchString(name) {
		return fmt.Errorf("mcp: name %q is invalid (must match %s)", name, mcpNameRegexp.String())
	}
	return nil
}

// Client defines the interface for communicating with an MCP server.
type Client interface {
	ListTools(ctx context.Context) ([]model.ToolSchema, error)
	CallTool(ctx context.Context, name string, input map[string]any) (*tool.ToolResponse, error)
	Close() error
}

// Elicitation actions for ElicitResponse.Action, per the MCP specification.
const (
	// ElicitAccept indicates the user submitted the form/confirmed the action.
	ElicitAccept = "accept"
	// ElicitDecline indicates the user explicitly declined the action.
	ElicitDecline = "decline"
	// ElicitCancel indicates the user dismissed without making a choice.
	ElicitCancel = "cancel"
)

// ElicitRequest carries an MCP elicitation/create request: the server asks
// the client to prompt the user for information in the middle of a tool
// call (codex Op::ResolveElicitation parity). Only form-mode elicitation is
// represented; URL-mode requests arrive with an empty RequestedSchema.
type ElicitRequest struct {
	// Message is the human-readable prompt from the server.
	Message string
	// RequestedSchema is the JSON Schema for the requested form data, carried
	// verbatim. Nil when the server did not supply one.
	RequestedSchema json.RawMessage
}

// ElicitResponse is the client's answer to an elicitation request.
type ElicitResponse struct {
	// Action is one of ElicitAccept, ElicitDecline or ElicitCancel. Any other
	// value (including the empty string) is normalized to ElicitCancel before
	// reaching the wire.
	Action string
	// Content holds the submitted form data; it is sent to the server only
	// when Action is ElicitAccept.
	Content map[string]any
}

// ElicitationHandler resolves a server-initiated elicitation request, for
// example by prompting the user and returning their choice. A handler error
// is reported to the server as a JSON-RPC internal error.
type ElicitationHandler func(ctx context.Context, req ElicitRequest) (ElicitResponse, error)

// StdioConfig configures a subprocess-based (stdio) MCP connection.
type StdioConfig struct {
	Command string
	Args    []string
	Env     []string
	WorkDir string

	// ElicitationHandler, when non-nil, enables MCP elicitation: the client
	// declares the elicitation capability during initialize and resolves
	// elicitation/create requests the server sends while a call is in
	// flight. The handler runs synchronously on the client's wire read loop,
	// so while it blocks (e.g. waiting on user input) other calls on the
	// same client cannot proceed. Nil disables elicitation; a server that
	// still sends elicitation/create receives a -32601 (method not found)
	// error.
	ElicitationHandler ElicitationHandler

	// Stateful currently has no implementation (no reader): the subprocess
	// stays alive across calls until Close regardless of this field.
	Stateful         bool
	EnableTools      []string // whitelist; empty = all tools
	DisableTools     []string // blacklist; applied after EnableTools
	ExecutionTimeout int      // per-tool call timeout in seconds; 0 = no timeout
}

// HttpConfig configures an HTTP JSON-RPC MCP connection.
//
// The transport is a bare JSON-RPC POST for simple 2024-11-05-era servers:
// there is no SSE or session support, and no inbound channel — server-
// initiated requests such as elicitation/create cannot arrive, so
// elicitation is not supported here. For the spec Streamable HTTP transport
// (2025-03-26 and later), including elicitation support, use the
// mcp/streamablehttp subpackage (upstream #2311).
type HttpConfig struct {
	URL     string
	Headers map[string]string
	Timeout int // seconds, 0 = default (30)

	// Stateful currently has no implementation (no reader); session semantics
	// live in the mcp/streamablehttp subpackage.
	Stateful         bool
	EnableTools      []string
	DisableTools     []string
	ExecutionTimeout int
}

// JSON-RPC 2.0 types for MCP protocol.

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonrpcEnvelope classifies an inbound line before full decoding: a
// non-empty Method marks server-initiated traffic (a request when ID is
// present, a notification otherwise). ID is kept raw because server-chosen
// request IDs may be strings — unlike client IDs, which are ints — and must
// be echoed verbatim in the response.
type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// jsonrpcRawResponse responds to a server-initiated request, echoing the
// server-chosen ID verbatim.
type jsonrpcRawResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// MCP protocol types

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *mcpAnnotations `json:"annotations,omitempty"`
}

type mcpAnnotations struct {
	ReadOnlyHint bool `json:"readOnlyHint"`
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type toolCallResult struct {
	Content           []mcpContent    `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
}

type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}
