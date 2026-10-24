package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// --- Mock MCP Client ---

type mockMCPClient struct {
	tools  []model.ToolSchema
	result *tool.ToolResponse
	err    error
}

func (m *mockMCPClient) ListTools(_ context.Context) ([]model.ToolSchema, error) {
	return m.tools, m.err
}

func (m *mockMCPClient) CallTool(_ context.Context, name string, input map[string]any) (*tool.ToolResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func (m *mockMCPClient) Close() error { return nil }

// --- Tests ---

func TestConvertToolSchemas(t *testing.T) {
	mcpTools := []mcpTool{
		{
			Name:        "search",
			Description: "Search the web",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		},
		{
			Name:        "calc",
			Description: "Calculate expression",
		},
	}

	schemas := convertToolSchemas(mcpTools)
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}

	if schemas[0].Function.Name != "search" {
		t.Errorf("name = %s, want search", schemas[0].Function.Name)
	}
	if schemas[0].Type != "function" {
		t.Errorf("type = %s, want function", schemas[0].Type)
	}

	// Second tool should get default schema
	if schemas[1].Function.Parameters == nil {
		t.Error("parameters should not be nil for calc")
	}
}

func TestConvertToolResult_Success(t *testing.T) {
	result := toolCallResult{
		Content: []mcpContent{
			{Type: "text", Text: "Found 5 results"},
		},
		IsError: false,
	}

	resp := convertToolResult(result)
	if resp.State != message.ToolResultSuccess {
		t.Errorf("state = %s, want success", resp.State)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d, want 1", len(resp.Content))
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content type = %T, want TextBlock", resp.Content[0])
	}
	if tb.Text != "Found 5 results" {
		t.Errorf("text = %s", tb.Text)
	}
}

func TestConvertToolResult_Error(t *testing.T) {
	result := toolCallResult{
		Content: []mcpContent{
			{Type: "text", Text: "Something went wrong"},
		},
		IsError: true,
	}

	resp := convertToolResult(result)
	if resp.State != message.ToolResultError {
		t.Errorf("state = %s, want error", resp.State)
	}
}

func TestConvertToolResult_Image(t *testing.T) {
	result := toolCallResult{
		Content: []mcpContent{
			{Type: "image", Data: "iVBORw0KGgo=", MimeType: "image/png"},
		},
	}

	resp := convertToolResult(result)
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d", len(resp.Content))
	}
	db, ok := resp.Content[0].(message.DataBlock)
	if !ok {
		t.Fatalf("content type = %T, want DataBlock", resp.Content[0])
	}
	src, ok := db.Source.(message.Base64Source)
	if !ok {
		t.Fatalf("source type = %T, want Base64Source", db.Source)
	}
	if src.MediaType != "image/png" {
		t.Errorf("MediaType = %s", src.MediaType)
	}
}

func TestConvertToolResult_Empty(t *testing.T) {
	result := toolCallResult{Content: nil}
	resp := convertToolResult(result)
	if len(resp.Content) != 1 {
		t.Fatalf("empty result should produce default text block, got %d", len(resp.Content))
	}
}

func TestToolCallResult_StructuredContentWire(t *testing.T) {
	var result toolCallResult
	payload := `{"content":[{"type":"text","text":"x"}],"structuredContent":{"a":1}}`
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatal(err)
	}
	if string(result.StructuredContent) != `{"a":1}` {
		t.Errorf("StructuredContent = %s, want wire JSON preserved", result.StructuredContent)
	}
}

func TestConvertToolResult_StructuredContentFallback(t *testing.T) {
	result := toolCallResult{
		StructuredContent: json.RawMessage(`{"answer":"42"}`),
	}

	resp := convertToolResult(result)
	if resp.State != message.ToolResultSuccess {
		t.Errorf("state = %s, want success", resp.State)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d, want 1", len(resp.Content))
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content type = %T, want TextBlock", resp.Content[0])
	}
	if tb.Text != `{"answer":"42"}` {
		t.Errorf("text = %s, want structured JSON", tb.Text)
	}
}

func TestConvertToolResult_StructuredContentSkippedWhenTextPresent(t *testing.T) {
	result := toolCallResult{
		Content:           []mcpContent{{Type: "text", Text: "42"}},
		StructuredContent: json.RawMessage(`{"answer":"42"}`),
	}

	resp := convertToolResult(result)
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d, want 1 (no structured fallback)", len(resp.Content))
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "42" {
		t.Errorf("content[0] = %+v, want the original text block", resp.Content[0])
	}
}

func TestConvertToolResult_StructuredContentAppendedToImageOnly(t *testing.T) {
	result := toolCallResult{
		Content:           []mcpContent{{Type: "image", Data: "aGVsbG8=", MimeType: "image/png"}},
		StructuredContent: json.RawMessage(`{"width":4}`),
	}

	resp := convertToolResult(result)
	if len(resp.Content) != 2 {
		t.Fatalf("content len = %d, want image plus structured fallback", len(resp.Content))
	}
	if _, ok := resp.Content[0].(message.DataBlock); !ok {
		t.Errorf("content[0] type = %T, want DataBlock", resp.Content[0])
	}
	tb, ok := resp.Content[1].(message.TextBlock)
	if !ok || tb.Text != `{"width":4}` {
		t.Errorf("content[1] = %+v, want structured JSON text", resp.Content[1])
	}
}

func TestConvertToolResult_StructuredContentNull(t *testing.T) {
	result := toolCallResult{StructuredContent: json.RawMessage(`null`)}

	resp := convertToolResult(result)
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d, want default block", len(resp.Content))
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != "" {
		t.Errorf("content[0] = %+v, want empty default block", resp.Content[0])
	}
}

func TestConvertToolResult_StructuredContentErrorState(t *testing.T) {
	result := toolCallResult{
		IsError:           true,
		StructuredContent: json.RawMessage(`{"code":"E_FAIL"}`),
	}

	resp := convertToolResult(result)
	if resp.State != message.ToolResultError {
		t.Errorf("state = %s, want error", resp.State)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d, want structured fallback", len(resp.Content))
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok || tb.Text != `{"code":"E_FAIL"}` {
		t.Errorf("content[0] = %+v, want structured JSON text", resp.Content[0])
	}
}

func TestConvertToolResult_StructuredContentWithAudioOnly(t *testing.T) {
	result := toolCallResult{
		Content:           []mcpContent{{Type: "audio", Data: "aGk=", MimeType: "audio/wav"}},
		StructuredContent: json.RawMessage(`{"transcript":"hi"}`),
	}

	resp := convertToolResult(result)
	// The audio block is textified by the default branch; the structured
	// payload must still surface as JSON text.
	if len(resp.Content) != 2 {
		t.Fatalf("content len = %d, want audio placeholder plus structured fallback", len(resp.Content))
	}
	tb, ok := resp.Content[1].(message.TextBlock)
	if !ok || tb.Text != `{"transcript":"hi"}` {
		t.Errorf("content[1] = %+v, want structured JSON text", resp.Content[1])
	}
}

func TestMCPTool_Execute(t *testing.T) {
	mock := &mockMCPClient{
		result: tool.NewTextResponse("mock result"),
	}

	schema := model.ToolSchema{
		Type: "function",
		Function: model.ToolFunction{
			Name:        "mock_tool",
			Description: "A mock tool",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}

	mcpTool := NewMCPTool(mock, schema)

	if mcpTool.Name() != "mock_tool" {
		t.Errorf("name = %s", mcpTool.Name())
	}

	resp, err := mcpTool.Execute(context.Background(), map[string]any{"query": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("state = %s", resp.State)
	}
}

func TestNewMCPToolkit(t *testing.T) {
	mock := &mockMCPClient{
		tools: []model.ToolSchema{
			{Type: "function", Function: model.ToolFunction{Name: "tool1", Description: "First"}},
			{Type: "function", Function: model.ToolFunction{Name: "tool2", Description: "Second"}},
		},
	}

	tk, err := NewMCPToolkit(context.Background(), mock)
	if err != nil {
		t.Fatal(err)
	}

	schemas := tk.GetToolSchemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 tool schemas, got %d", len(schemas))
	}
}

func TestHttpClient_MockServer(t *testing.T) {
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCount++

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "test-server", "version": "1.0"},
			}
		case "tools/list":
			result = toolListResult{
				Tools: []mcpTool{
					{Name: "echo", Description: "Echo input", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)},
				},
			}
		case "tools/call":
			result = toolCallResult{
				Content: []mcpContent{{Type: "text", Text: "echoed"}},
			}
		}

		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		resp.Result, _ = json.Marshal(result)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := NewHttpClient(context.Background(), &HttpConfig{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Function.Name != "echo" {
		t.Errorf("tool name = %s", tools[0].Function.Name)
	}

	resp, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("state = %s", resp.State)
	}
}

func TestValidateMCPName(t *testing.T) {
	valid := []string{
		"my-server",
		"server_1",
		"MCP-Server",
		"test123",
		"a",
		"A_B-C",
	}
	for _, name := range valid {
		if err := ValidateMCPName(name); err != nil {
			t.Errorf("ValidateMCPName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"has space",
		"has.dot",
		"slash/name",
		"emoji\U0001f600",
		"tab\there",
		"new\nline",
	}
	for _, name := range invalid {
		if err := ValidateMCPName(name); err == nil {
			t.Errorf("ValidateMCPName(%q) = nil, want error", name)
		}
	}
}

func TestWithExecutionTimeout(t *testing.T) {
	mock := &mockMCPClient{
		tools: []model.ToolSchema{
			{Type: "function", Function: model.ToolFunction{Name: "slow_tool", Description: "Slow"}},
		},
		result: tool.NewTextResponse("ok"),
	}

	tk, err := NewMCPToolkit(context.Background(), mock, WithExecutionTimeout(10))
	if err != nil {
		t.Fatal(err)
	}

	schemas := tk.GetToolSchemas()
	if len(schemas) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(schemas))
	}
}

func TestMergeToolkits(t *testing.T) {
	t1 := tool.NewToolkit(tool.NewFunctionTool("a", "desc a", json.RawMessage(`{}`),
		func(ctx context.Context, input map[string]any) (any, error) { return nil, nil }))
	t2 := tool.NewToolkit(tool.NewFunctionTool("b", "desc b", json.RawMessage(`{}`),
		func(ctx context.Context, input map[string]any) (any, error) { return nil, nil }))

	merged := MergeToolkits(t1, t2)
	schemas := merged.GetToolSchemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 tools after merge, got %d", len(schemas))
	}
}
