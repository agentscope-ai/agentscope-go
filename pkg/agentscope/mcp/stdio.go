package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// StdioClient communicates with an MCP server via subprocess stdin/stdout.
//
// Two locks split responsibilities: lifeMu guards process-lifetime state
// (cmd/stdin/stdout/cancel/closed), while mu serializes wire I/O and is held
// by call() across its blocking read loop. A hung server can therefore block
// a call indefinitely while it holds mu; Close and Reconnect must never
// require mu, so tearing down the process can never deadlock behind an
// in-flight call. Killing the process closes its stdout, which unblocks the
// hung read with EOF.
type StdioClient struct {
	cfg    *StdioConfig // retained for Reconnect
	nextID int

	lifeMu sync.Mutex // guards the fields below
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	cancel context.CancelFunc // kills the subprocess
	closed bool

	mu sync.Mutex // wire serialization; held across the blocking read loop
}

// NewStdioClient starts the MCP server subprocess and initializes the
// session. The passed ctx governs the startup handshake only, not the
// subprocess lifetime: the server process lives until Close or Reconnect, so
// request-scoped contexts are safe to pass here.
func NewStdioClient(ctx context.Context, cfg *StdioConfig) (*StdioClient, error) {
	c := &StdioClient{cfg: cfg, nextID: 1}
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// start launches the MCP server subprocess and initializes a fresh session.
// Transports are one-shot: start always builds a new process and pipes
// (upstream fix #2308 — connect/close/connect must start fresh).
//
// The subprocess lifetime is deliberately decoupled from the caller's ctx:
// callers commonly pass a request-scoped ctx to Reconnect, and canceling it
// must not kill a healthy connection. The process lives until Close, a later
// Reconnect, or being displaced by a concurrent start (in which case it is
// killed, never leaked). Pass a ctx with the lifetime of the initial
// handshake only.
func (c *StdioClient) start(ctx context.Context) error {
	cfg := c.cfg
	procCtx, cancel := context.WithCancel(context.Background())
	handedOff := false // true once cancel is stored on the client
	defer func() {
		if !handedOff {
			cancel()
		}
	}()
	cmd := exec.CommandContext(procCtx, cfg.Command, cfg.Args...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	// Do NOT inherit the full parent environment (it may contain API keys and
	// other secrets). Start from a minimal safe allowlist and layer any
	// caller-provided vars on top (later entries win for duplicate keys).
	cmd.Env = append(minimalMCPEnv(), cfg.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp stdio: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp stdio: start: %w", err)
	}

	// The default bufio.Scanner token limit is 64 KiB; a single tool result
	// larger than that (common with base64-encoded images) would permanently
	// error the scanner and kill the client. Allow up to 10 MiB per message.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	c.lifeMu.Lock()
	oldCmd, oldCancel := c.cmd, c.cancel
	c.cmd = cmd
	c.stdin = stdin
	c.stdout = sc
	c.cancel = cancel
	c.closed = false
	c.lifeMu.Unlock()
	handedOff = true

	// Defensive cleanup for the documented-but-possible concurrent
	// Reconnect race: if we displaced a live process, kill and reap it so
	// no subprocess is ever orphaned.
	if oldCmd != nil {
		if oldCancel != nil {
			oldCancel()
		}
		_ = oldCmd.Wait()
	}

	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

// Reconnect closes the current server subprocess (if any) and starts a fresh
// one, re-initializing the session. It mirrors upstream fix #2308: a closed
// or failed client can always be reconnected, including when a previous call
// is hung on an unresponsive server — Close kills the subprocess, which
// unblocks the in-flight read. Reconnect must not be called concurrently
// with itself: a racing start kills the displaced process, and the loser's
// failed initialization then calls Close, which tears down the winner's
// process too — both calls fail and the client ends up closed (no leak, no
// hang; one more Reconnect recovers). Calls are safe to interleave with
// CallTool/Close. The passed ctx governs the reconnect handshake only, not
// the new subprocess lifetime.
func (c *StdioClient) Reconnect(ctx context.Context) error {
	_ = c.Close()
	return c.start(ctx)
}

func (c *StdioClient) initialize(ctx context.Context) error {
	capabilities := map[string]any{}
	if c.cfg.ElicitationHandler != nil {
		// Elicitation is capability-gated: declare it so the server knows it
		// may send elicitation/create while a call is in flight.
		capabilities["elicitation"] = map[string]any{}
	}
	_, err := c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    capabilities,
		ClientInfo:      clientInfo{Name: "agentscope-go", Version: "2.0"},
	})
	if err != nil {
		return fmt.Errorf("mcp stdio: initialize: %w", err)
	}

	// Send initialized notification (no id, no response expected)
	notif := jsonrpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	data, _ := json.Marshal(notif)
	c.lifeMu.Lock()
	stdin := c.stdin
	c.lifeMu.Unlock()
	if stdin == nil {
		return fmt.Errorf("mcp stdio: client is closed")
	}
	c.mu.Lock()
	_, err = stdin.Write(append(data, '\n'))
	c.mu.Unlock()
	return err
}

func (c *StdioClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.lifeMu.Lock()
	if c.closed {
		c.lifeMu.Unlock()
		return nil, fmt.Errorf("mcp stdio: client is closed (call Reconnect to restart the server)")
	}
	id := c.nextID
	c.nextID++
	stdin := c.stdin
	sc := c.stdout
	c.lifeMu.Unlock()

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	// Serialize wire I/O. The read loop below blocks holding mu; Close and
	// Reconnect never take mu, so they stay responsive even while this call
	// is hung. Snapshot the handles first so a concurrent Reconnect swapping
	// the fields cannot redirect this call to a different generation.
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return nil, err
	}

	for sc.Scan() {
		line := sc.Bytes()
		var env jsonrpcEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if env.Method != "" {
			// Server-initiated traffic: notifications (no id) carry no reply
			// and are ignored; requests are answered inline so the server's
			// in-flight tool call can proceed.
			if len(env.ID) > 0 {
				c.answerServerRequest(ctx, stdin, &env)
			}
			continue
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mcp stdio: read: %w", err)
	}
	return nil, fmt.Errorf("mcp stdio: unexpected end of stream")
}

// answerServerRequest handles a server-initiated JSON-RPC request that
// arrives while a call holds the wire mutex. The response echoes the request
// ID verbatim (server IDs may be strings, unlike client IDs). Unknown
// methods get -32601 so a server feature the client does not implement fails
// fast instead of hanging the server's in-flight tool call.
func (c *StdioClient) answerServerRequest(ctx context.Context, stdin io.Writer, env *jsonrpcEnvelope) {
	resp := jsonrpcRawResponse{JSONRPC: "2.0", ID: env.ID}
	switch env.Method {
	case "elicitation/create":
		result, rpcErr := c.resolveElicitation(ctx, env.Params)
		resp.Result = result
		resp.Error = rpcErr
	default:
		resp.Error = &jsonrpcError{Code: -32601, Message: "method not found: " + env.Method}
	}
	data, err := json.Marshal(resp)
	if err != nil {
		// The shapes above always marshal; drop rather than risk corrupting
		// the stream with a partial line.
		return
	}
	_, _ = stdin.Write(append(data, '\n'))
}

// resolveElicitation runs the configured ElicitationHandler for an
// elicitation/create request. The handler runs synchronously on the wire
// read loop: while it blocks (e.g. waiting on user input), other calls on
// this client cannot proceed.
func (c *StdioClient) resolveElicitation(ctx context.Context, params json.RawMessage) (any, *jsonrpcError) {
	handler := c.cfg.ElicitationHandler
	if handler == nil {
		return nil, &jsonrpcError{Code: -32601, Message: "client does not support elicitation"}
	}
	var p struct {
		Message         string          `json:"message"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &jsonrpcError{Code: -32602, Message: "invalid elicitation params: " + err.Error()}
	}
	res, err := handler(ctx, ElicitRequest{Message: p.Message, RequestedSchema: p.RequestedSchema})
	if err != nil {
		return nil, &jsonrpcError{Code: -32603, Message: "elicitation handler: " + err.Error()}
	}
	action := res.Action
	switch action {
	case ElicitAccept, ElicitDecline, ElicitCancel:
	default:
		// A handler bug must not put an invalid action on the wire; treat it
		// as a dismissal.
		action = ElicitCancel
	}
	result := map[string]any{"action": action}
	if action == ElicitAccept && res.Content != nil {
		result["content"] = res.Content
	}
	return result, nil
}

// ListTools queries the MCP server for available tools.
func (c *StdioClient) ListTools(ctx context.Context) ([]model.ToolSchema, error) {
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}

	var toolList toolListResult
	if err := json.Unmarshal(result, &toolList); err != nil {
		return nil, fmt.Errorf("mcp stdio: parse tools: %w", err)
	}

	return convertToolSchemas(toolList.Tools), nil
}

// CallTool invokes a tool on the MCP server.
func (c *StdioClient) CallTool(ctx context.Context, name string, input map[string]any) (*tool.ToolResponse, error) {
	result, err := c.call(ctx, "tools/call", toolCallParams{
		Name:      name,
		Arguments: input,
	})
	if err != nil {
		return nil, err
	}

	var callResult toolCallResult
	if err := json.Unmarshal(result, &callResult); err != nil {
		return nil, fmt.Errorf("mcp stdio: parse result: %w", err)
	}

	return convertToolResult(callResult), nil
}

// Close terminates the MCP server subprocess. It is idempotent and safe to
// call while a CallTool is in flight: the process is killed outside the wire
// mutex, so a read hung on an unresponsive server is unblocked (EOF) instead
// of deadlocking Close. Call Reconnect to start a new session afterwards.
func (c *StdioClient) Close() error {
	c.lifeMu.Lock()
	if c.closed {
		c.lifeMu.Unlock()
		return nil
	}
	c.closed = true
	cmd, stdin, cancel := c.cmd, c.stdin, c.cancel
	c.cmd, c.stdin, c.stdout, c.cancel = nil, nil, nil, nil
	c.lifeMu.Unlock()

	if cancel != nil {
		cancel() // kills the subprocess (exec.CommandContext)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil {
		// Ordering contract — do not reorder: the process must be killed
		// before Wait. Killing closes the pipe's write end, so any reader
		// blocked on StdoutPipe gets EOF and finishes; Wait then only
		// reaps. Calling Wait first would violate the StdoutPipe contract
		// and could leave a reader blocked on a live pipe.
		_ = cmd.Wait()
	}
	return nil
}

// Shared conversion helpers

func convertToolSchemas(tools []mcpTool) []model.ToolSchema {
	schemas := make([]model.ToolSchema, len(tools))
	for i, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		schemas[i] = model.ToolSchema{
			Type: "function",
			Function: model.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		}
	}
	return schemas
}

func convertToolResult(result toolCallResult) *tool.ToolResponse {
	var blocks []message.ContentBlock
	hasText := false
	for _, c := range result.Content {
		if c.Type == "text" {
			hasText = true
		}
		switch c.Type {
		case "text":
			blocks = append(blocks, message.TextBlock{Type: "text", Text: c.Text})
		case "image":
			blocks = append(blocks, message.DataBlock{
				Type: "data",
				Source: message.Base64Source{
					Type:      "base64",
					Data:      c.Data,
					MediaType: c.MimeType,
				},
			})
		default:
			blocks = append(blocks, message.TextBlock{Type: "text", Text: c.Text})
		}
	}

	// Mirror upstream AgentScope: when a result carries structuredContent
	// without any wire text content, surface it as JSON text instead of
	// dropping it. The check uses the wire types, not the converted blocks:
	// unknown types (audio, resources) are textified above, and counting
	// those would still drop the structured payload.
	if text := structuredText(result.StructuredContent); text != "" && !hasText {
		blocks = append(blocks, message.TextBlock{Type: "text", Text: text})
	}

	state := message.ToolResultSuccess
	if result.IsError {
		state = message.ToolResultError
	}
	if len(blocks) == 0 {
		blocks = []message.ContentBlock{message.TextBlock{Type: "text", Text: ""}}
	}

	return &tool.ToolResponse{
		Content: blocks,
		State:   state,
	}
}

// structuredText returns the wire JSON of a structuredContent field, or ""
// when the field is absent or null.
func structuredText(raw json.RawMessage) string {
	s := string(raw)
	if s == "" || s == "null" {
		return ""
	}
	return s
}
