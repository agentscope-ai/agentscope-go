package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// TestHelperFakeMCPServer runs this test binary as a minimal MCP stdio server.
// The echo tool returns the process PID so tests can verify a reconnect spawned
// a fresh subprocess/session.
func TestHelperFakeMCPServer(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_MCP_SERVER") != "1" {
		return
	}
	// Records whether the client's initialize declared the elicitation
	// capability, reported by the capcheck tool.
	sawElicitationCapability := false
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // notification
		}
		var result any
		switch req.Method {
		case "initialize":
			var ip struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			_ = json.Unmarshal(req.Params, &ip)
			_, sawElicitationCapability = ip.Capabilities["elicitation"]
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "returns the server pid",
					"inputSchema": map[string]any{"type": "object"},
				},
			}}
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "die":
				// Simulate a server crash mid-session: exit without replying.
				os.Exit(1)
			case "hang":
				// Simulate an unresponsive server: never reply.
				time.Sleep(10 * time.Minute)
				continue
			case "capcheck":
				result = map[string]any{"content": []any{
					map[string]any{"type": "text", "text": fmt.Sprintf("elicitation-capable:%v", sawElicitationCapability)},
				}}
			case "elicit":
				// Server-initiated traffic mid-call: a notification the
				// client must ignore, then an elicitation/create request with
				// a string ID to verify the client echoes it verbatim.
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"about to elicit"}}`)
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","id":"srv-1","method":"elicitation/create","params":{"message":"Need input","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}}`)
				result = map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "elicit-result:" + readServerRequestResponse(sc)},
				}}
			case "unknownreq":
				// A server request for a method the client does not implement
				// must fail fast with -32601, not hang this tool call.
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","id":"srv-9","method":"server/bogus","params":{}}`)
				result = map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "unknownreq-result:" + readServerRequestResponse(sc)},
				}}
			}
			if result == nil {
				result = map[string]any{"content": []any{
					map[string]any{"type": "text", "text": fmt.Sprintf("pid-%d", os.Getpid())},
				}}
			}
		default:
			result = map[string]any{}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		fmt.Fprintln(os.Stdout, string(resp))
	}
	os.Exit(0)
}

// readServerRequestResponse reads the client's response to a server-initiated
// request and summarizes it as "id:<raw> action:<action> name:<name>" for
// elicitation results or "error:<code>" for error responses.
func readServerRequestResponse(sc *bufio.Scanner) string {
	if !sc.Scan() {
		os.Exit(1)
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Action  string         `json:"action"`
			Content map[string]any `json:"content"`
		} `json:"result"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		os.Exit(1)
	}
	if resp.Error != nil {
		return fmt.Sprintf("error:%d", resp.Error.Code)
	}
	name, _ := resp.Result.Content["name"].(string)
	return fmt.Sprintf("id:%s action:%s name:%s", string(resp.ID), resp.Result.Action, name)
}

func fakeMCPServerConfig() *StdioConfig {
	return &StdioConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperFakeMCPServer"},
		Env:     []string{"GO_WANT_HELPER_MCP_SERVER=1"},
	}
}

func callEcho(t *testing.T, c *StdioClient) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.CallTool(ctx, "echo", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("empty tool response")
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("content[0] is %T, want TextBlock", resp.Content[0])
	}
	return tb.Text
}

func TestStdioClient_ReconnectAfterClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := NewStdioClient(ctx, fakeMCPServerConfig())
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	first := callEcho(t, c)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Calls after close must fail with a clear error, not hang or panic.
	if _, err := c.CallTool(ctx, "echo", map[string]any{}); err == nil {
		t.Fatal("expected error calling a closed client")
	}

	// connect -> close -> connect must start a fresh subprocess/session.
	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	second := callEcho(t, c)
	if first == second {
		t.Errorf("reconnect reused the same server process (%s)", first)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Close must be idempotent.
	if err := c.Close(); err != nil {
		t.Fatalf("Close should be idempotent, got: %v", err)
	}
}

func TestStdioClient_FailedConnectCanBeRetried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := fakeMCPServerConfig()
	cfg.Command = "/nonexistent/agentscope-mcp-fake-binary"

	c, err := NewStdioClient(ctx, cfg)
	if err == nil {
		c.Close()
		t.Fatal("expected NewStdioClient to fail with a bad binary")
	}

	// A failed construction must not poison the config: a fresh client with a
	// working binary must succeed (retry semantics of upstream #2308).
	c, err = NewStdioClient(ctx, fakeMCPServerConfig())
	if err != nil {
		t.Fatalf("retry after failed connect: %v", err)
	}
	defer c.Close()
	callEcho(t, c)
}

// TestStdioClient_CloseUnblocksHungCall locks in the M1 guarantee: Close
// must not deadlock behind an in-flight call that is hung on an unresponsive
// server (the very situation Reconnect exists for).
func TestStdioClient_CloseUnblocksHungCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := NewStdioClient(ctx, fakeMCPServerConfig())
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}

	hangDone := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "hang", map[string]any{})
		hangDone <- err
	}()

	// Give the hung call time to enter its blocking read.
	time.Sleep(500 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked behind a hung in-flight call")
	}

	select {
	case err := <-hangDone:
		if err == nil {
			t.Fatal("expected the hung call to error once the server is killed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call did not return after Close killed the server")
	}

	// The client must be usable again afterwards.
	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect after Close: %v", err)
	}
	callEcho(t, c)
	if err := c.Close(); err != nil {
		t.Fatalf("final Close: %v", err)
	}
}

// TestStdioClient_ServerDeathThenReconnect is the flagship #2308 scenario:
// the server dies mid-session, calls fail, and Reconnect recovers with a
// fresh subprocess/session.
func TestStdioClient_ServerDeathThenReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := NewStdioClient(ctx, fakeMCPServerConfig())
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	defer c.Close()
	first := callEcho(t, c)

	// Server exits without replying; the call must surface an error.
	if _, err := c.CallTool(ctx, "die", map[string]any{}); err == nil {
		t.Fatal("expected an error from a server that exited mid-call")
	}
	// Subsequent calls on the dead stream must also fail, not hang.
	if _, err := c.CallTool(ctx, "echo", map[string]any{}); err == nil {
		t.Fatal("expected calls on a dead server to fail")
	}

	if err := c.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	second := callEcho(t, c)
	if first == second {
		t.Errorf("reconnect reused the same server process (%s)", first)
	}
}
