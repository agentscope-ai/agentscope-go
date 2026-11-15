package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func callToolText(t *testing.T, c *StdioClient, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.CallTool(ctx, name, map[string]any{})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	if len(resp.Content) == 0 {
		t.Fatalf("CallTool(%q): empty tool response", name)
	}
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("CallTool(%q): content[0] is %T, want TextBlock", name, resp.Content[0])
	}
	return tb.Text
}

func newElicitationClient(t *testing.T, handler ElicitationHandler) *StdioClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := fakeMCPServerConfig()
	cfg.ElicitationHandler = handler
	c, err := NewStdioClient(ctx, cfg)
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestStdioClient_ElicitationAccept runs the full server-initiated
// elicitation round trip: the fake server sends a notification (ignored) and
// an elicitation/create request with a string ID mid tools/call, and the
// client must answer with the ID echoed verbatim.
func TestStdioClient_ElicitationAccept(t *testing.T) {
	var gotReq ElicitRequest
	c := newElicitationClient(t, func(_ context.Context, req ElicitRequest) (ElicitResponse, error) {
		gotReq = req
		return ElicitResponse{
			Action:  ElicitAccept,
			Content: map[string]any{"name": "bob"},
		}, nil
	})

	text := callToolText(t, c, "elicit")
	for _, want := range []string{`id:"srv-1"`, "action:accept", "name:bob"} {
		if !strings.Contains(text, want) {
			t.Errorf("tool result %q missing %q", text, want)
		}
	}
	if gotReq.Message != "Need input" {
		t.Errorf("handler message = %q, want %q", gotReq.Message, "Need input")
	}
	if !strings.Contains(string(gotReq.RequestedSchema), `"name"`) {
		t.Errorf("handler schema = %s, want it to carry the requested schema", gotReq.RequestedSchema)
	}
}

func TestStdioClient_ElicitationDecline(t *testing.T) {
	c := newElicitationClient(t, func(_ context.Context, _ ElicitRequest) (ElicitResponse, error) {
		return ElicitResponse{Action: ElicitDecline}, nil
	})
	text := callToolText(t, c, "elicit")
	if !strings.Contains(text, "action:decline") {
		t.Errorf("tool result %q missing decline action", text)
	}
	if strings.Contains(text, "name:") && !strings.HasSuffix(text, "name:") {
		t.Errorf("decline must not carry content, got %q", text)
	}
}

// TestStdioClient_ElicitationNoHandler: a client that never declared the
// elicitation capability must fail the server's elicitation/create fast with
// -32601 instead of hanging the in-flight tool call.
func TestStdioClient_ElicitationNoHandler(t *testing.T) {
	c := newElicitationClient(t, nil)
	text := callToolText(t, c, "elicit")
	if !strings.Contains(text, "error:-32601") {
		t.Errorf("tool result %q missing -32601", text)
	}
}

// TestStdioClient_ElicitationInvalidActionBecomesCancel locks in the
// normalization: a handler returning a bogus action must not put it on the
// wire.
func TestStdioClient_ElicitationInvalidActionBecomesCancel(t *testing.T) {
	c := newElicitationClient(t, func(_ context.Context, _ ElicitRequest) (ElicitResponse, error) {
		return ElicitResponse{Action: "bogus", Content: map[string]any{"name": "bob"}}, nil
	})
	text := callToolText(t, c, "elicit")
	if !strings.Contains(text, "action:cancel") {
		t.Errorf("tool result %q missing cancel action", text)
	}
	if strings.Contains(text, "bob") {
		t.Errorf("content of a non-accept action must not reach the wire, got %q", text)
	}
}

func TestStdioClient_ElicitationHandlerError(t *testing.T) {
	c := newElicitationClient(t, func(_ context.Context, _ ElicitRequest) (ElicitResponse, error) {
		return ElicitResponse{}, errors.New("user prompt failed")
	})
	text := callToolText(t, c, "elicit")
	if !strings.Contains(text, "error:-32603") {
		t.Errorf("tool result %q missing -32603", text)
	}
}

// TestStdioClient_UnknownServerRequest: a server request for an unimplemented
// method gets -32601 so the server's in-flight tool call fails fast.
func TestStdioClient_UnknownServerRequest(t *testing.T) {
	c := newElicitationClient(t, nil)
	text := callToolText(t, c, "unknownreq")
	if !strings.Contains(text, "error:-32601") {
		t.Errorf("tool result %q missing -32601", text)
	}
}

// TestStdioClient_ElicitationCapabilityDeclared: the client declares the
// elicitation capability during initialize exactly when a handler is set.
func TestStdioClient_ElicitationCapabilityDeclared(t *testing.T) {
	withHandler := newElicitationClient(t, func(_ context.Context, _ ElicitRequest) (ElicitResponse, error) {
		return ElicitResponse{Action: ElicitCancel}, nil
	})
	if text := callToolText(t, withHandler, "capcheck"); !strings.Contains(text, "elicitation-capable:true") {
		t.Errorf("with handler, capcheck = %q, want elicitation-capable:true", text)
	}

	withoutHandler := newElicitationClient(t, nil)
	if text := callToolText(t, withoutHandler, "capcheck"); !strings.Contains(text, "elicitation-capable:false") {
		t.Errorf("without handler, capcheck = %q, want elicitation-capable:false", text)
	}
}
