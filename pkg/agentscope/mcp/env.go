package mcp

import (
	"os"
	"strings"
)

// mcpEnvAllowlist is the set of environment variable names forwarded to MCP
// stdio subprocesses by default. It carries what a process needs to run and
// resolve tools/paths, but deliberately excludes application secrets (API keys,
// tokens) that happen to live in the parent environment.
var mcpEnvAllowlist = map[string]bool{
	"PATH":     true,
	"HOME":     true,
	"USER":     true,
	"LOGNAME":  true,
	"SHELL":    true,
	"LANG":     true,
	"LC_ALL":   true,
	"LC_CTYPE": true,
	"TERM":     true,
	"TMPDIR":   true,
	"TMP":      true,
	"TEMP":     true,
	// Windows essentials.
	"SYSTEMROOT":   true,
	"PATHEXT":      true,
	"HOMEDRIVE":    true,
	"HOMEPATH":     true,
	"WINDIR":       true,
	"COMSPEC":      true,
	"APPDATA":      true,
	"LOCALAPPDATA": true,
}

// minimalMCPEnv returns the parent process environment filtered to the
// allowlist, so an MCP subprocess does not inherit secrets by default.
func minimalMCPEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if mcpEnvAllowlist[strings.ToUpper(kv[:i])] {
			out = append(out, kv)
		}
	}
	return out
}
