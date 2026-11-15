package mcp

import (
	"os"
	"strings"
	"testing"
)

// TestMinimalMCPEnv_ExcludesSecrets proves the MCP stdio subprocess env does not
// inherit arbitrary secrets from the parent process, while still carrying the
// essentials (PATH) it needs to run.
func TestMinimalMCPEnv_ExcludesSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-must-not-leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-also-secret")

	env := minimalMCPEnv()
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "OPENAI_API_KEY") || strings.Contains(joined, "ANTHROPIC_API_KEY") {
		t.Fatalf("secret env var leaked into MCP subprocess env: %v", env)
	}
	if os.Getenv("PATH") != "" && !strings.Contains(joined, "PATH=") {
		t.Errorf("PATH should be forwarded to MCP subprocess")
	}
}
