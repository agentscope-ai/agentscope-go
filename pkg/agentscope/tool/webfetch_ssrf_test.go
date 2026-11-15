package tool

import (
	"context"
	"net"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

func TestIsDisallowedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.0.1", "172.16.5.5", "169.254.169.254", "0.0.0.0", "fe80::1"}
	for _, s := range blocked {
		if !isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be disallowed (SSRF target)", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"}
	for _, s := range allowed {
		if isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be allowed", s)
		}
	}
}

// TestWebFetch_BlocksLoopbackSSRF proves the default WebFetch client refuses to
// connect to a loopback/metadata address (the 169.254.169.254 cloud-metadata
// endpoint being the canonical exfiltration target).
func TestWebFetch_BlocksLoopbackSSRF(t *testing.T) {
	ft := WebFetchTool()
	resp, err := ft.Execute(context.Background(), map[string]any{"url": "127.0.0.1:9"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Fatalf("expected SSRF fetch to loopback to be blocked, got state %q", resp.State)
	}
}
