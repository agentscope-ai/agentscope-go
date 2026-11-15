package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

type echoTool struct {
	tool.BaseTool
}

func (t *echoTool) Execute(_ context.Context, args map[string]any) (*tool.ToolResponse, error) {
	text, _ := args["text"].(string)
	return &tool.ToolResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "echo: " + text}},
		State:   message.ToolResultSuccess,
	}, nil
}

func newEchoTool() tool.Tool {
	return &echoTool{
		BaseTool: tool.BaseTool{
			ToolName:        "echo",
			ToolDescription: "Echoes back input",
			ToolSchema:      json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		},
	}
}

func TestStdioServer_Initialize(t *testing.T) {
	req := jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: initializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      clientInfo{Name: "test", Version: "1.0"},
	}}
	reqData, _ := json.Marshal(req)

	input := string(reqData) + "\n"
	var output bytes.Buffer

	srv := NewStdioServer(nil, StdioServerConfig{
		Reader: strings.NewReader(input),
		Writer: &output,
	})

	err := srv.Serve(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v (raw: %s)", err, output.String())
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	var result map[string]any
	_ = json.Unmarshal(resp.Result, &result)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocol version = %v", result["protocolVersion"])
	}
}

func TestStdioServer_ToolsList(t *testing.T) {
	echo := newEchoTool()
	srv := NewStdioServer([]tool.Tool{echo}, StdioServerConfig{})

	req := jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}
	reqData, _ := json.Marshal(req)

	var output bytes.Buffer
	srv.reader = strings.NewReader(string(reqData) + "\n")
	srv.writer = &output

	_ = srv.Serve(context.Background())

	var resp jsonrpcResponse
	_ = json.Unmarshal(output.Bytes(), &resp)

	var result toolListResult
	_ = json.Unmarshal(resp.Result, &result)

	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "echo" {
		t.Fatalf("tool name = %s", result.Tools[0].Name)
	}
}

func TestStdioServer_ToolsCall(t *testing.T) {
	echo := newEchoTool()
	srv := NewStdioServer([]tool.Tool{echo}, StdioServerConfig{})

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: toolCallParams{
			Name:      "echo",
			Arguments: map[string]any{"text": "hello"},
		},
	}
	reqData, _ := json.Marshal(req)

	var output bytes.Buffer
	srv.reader = strings.NewReader(string(reqData) + "\n")
	srv.writer = &output

	_ = srv.Serve(context.Background())

	var resp jsonrpcResponse
	_ = json.Unmarshal(output.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("error: %s", resp.Error.Message)
	}

	var result toolCallResult
	_ = json.Unmarshal(resp.Result, &result)

	if result.IsError {
		t.Fatal("unexpected error result")
	}
	if len(result.Content) == 0 || result.Content[0].Text != "echo: hello" {
		t.Fatalf("unexpected content: %+v", result.Content)
	}
}

func TestStdioServer_RegisterTool(t *testing.T) {
	srv := NewStdioServer(nil, StdioServerConfig{})
	if len(srv.tools) != 0 {
		t.Fatal("should start with no tools")
	}

	srv.RegisterTool(newEchoTool())
	if len(srv.tools) != 1 {
		t.Fatal("should have 1 tool after register")
	}
}
