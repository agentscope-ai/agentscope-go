package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// StdioServer implements the MCP server side, communicating via JSON-RPC over
// stdin/stdout. This allows agentscope-go tools and agents to be exposed as
// MCP servers to Claude Desktop, Cursor, Windsurf, and other MCP clients.
type StdioServer struct {
	mu      sync.RWMutex
	tools   map[string]tool.Tool
	name    string
	version string
	reader  io.Reader
	writer  io.Writer
}

// StdioServerConfig configures a StdioServer.
type StdioServerConfig struct {
	Name    string // server name reported during initialize
	Version string // server version
	Reader  io.Reader
	Writer  io.Writer
}

// NewStdioServer creates a new MCP stdio server with the given tools.
func NewStdioServer(tools []tool.Tool, cfg StdioServerConfig) *StdioServer {
	s := &StdioServer{
		tools:   make(map[string]tool.Tool),
		name:    cfg.Name,
		version: cfg.Version,
		reader:  cfg.Reader,
		writer:  cfg.Writer,
	}
	if s.name == "" {
		s.name = "agentscope-go"
	}
	if s.version == "" {
		s.version = "2.0"
	}
	if s.reader == nil {
		s.reader = os.Stdin
	}
	if s.writer == nil {
		s.writer = os.Stdout
	}
	for _, t := range tools {
		s.tools[t.Name()] = t
	}
	return s
}

// RegisterTool dynamically adds a tool to the server.
func (s *StdioServer) RegisterTool(t tool.Tool) {
	s.mu.Lock()
	s.tools[t.Name()] = t
	s.mu.Unlock()
}

// Serve reads JSON-RPC requests from stdin and writes responses to stdout.
// It blocks until ctx is canceled or stdin is closed.
func (s *StdioServer) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(0, -32700, "parse error")
			continue
		}

		if req.ID == 0 && req.Method != "" {
			// Notification — no response expected
			continue
		}

		resp := s.handleRequest(ctx, req)
		if resp != nil {
			s.writeResponse(resp)
		}
	}

	return scanner.Err()
}

func (s *StdioServer) handleRequest(ctx context.Context, req jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
	case "ping":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{}`),
		}
	default:
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonrpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}
	}
}

func (s *StdioServer) handleInitialize(id int) *jsonrpcResponse {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    s.name,
			"version": s.version,
		},
	}
	data, _ := json.Marshal(result)
	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  data,
	}
}

func (s *StdioServer) handleToolsList(id int) *jsonrpcResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tools []mcpTool
	for _, t := range s.tools {
		tools = append(tools, mcpTool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}

	result := toolListResult{Tools: tools}
	data, _ := json.Marshal(result)
	return &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  data,
	}
}

func (s *StdioServer) handleToolsCall(ctx context.Context, id int, params any) *jsonrpcResponse {
	paramsData, err := json.Marshal(params)
	if err != nil {
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &jsonrpcError{Code: -32602, Message: "invalid params"},
		}
	}

	var callParams toolCallParams
	if err := json.Unmarshal(paramsData, &callParams); err != nil {
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &jsonrpcError{Code: -32602, Message: "invalid params"},
		}
	}

	s.mu.RLock()
	t, ok := s.tools[callParams.Name]
	s.mu.RUnlock()
	if !ok {
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &jsonrpcError{Code: -32602, Message: fmt.Sprintf("tool %q not found", callParams.Name)},
		}
	}

	resp, execErr := t.Execute(ctx, callParams.Arguments)
	if execErr != nil {
		result := toolCallResult{
			Content: []mcpContent{{Type: "text", Text: execErr.Error()}},
			IsError: true,
		}
		data, _ := json.Marshal(result)
		return &jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: data}
	}

	var content []mcpContent
	for _, b := range resp.Content {
		switch block := b.(type) {
		case message.TextBlock:
			content = append(content, mcpContent{Type: "text", Text: block.Text})
		case message.DataBlock:
			if src, ok := block.Source.(message.Base64Source); ok {
				content = append(content, mcpContent{
					Type:     "image",
					Data:     src.Data,
					MimeType: src.MediaType,
				})
			}
		}
	}
	if len(content) == 0 {
		content = []mcpContent{{Type: "text", Text: ""}}
	}

	result := toolCallResult{
		Content: content,
		IsError: resp.State == message.ToolResultError,
	}
	data, _ := json.Marshal(result)
	return &jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: data}
}

func (s *StdioServer) writeResponse(resp *jsonrpcResponse) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_, _ = s.writer.Write(data)
}

func (s *StdioServer) writeError(id int, code int, msg string) {
	resp := &jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: msg},
	}
	s.writeResponse(resp)
}
