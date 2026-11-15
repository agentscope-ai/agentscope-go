package tool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func TestBashTool_CheckPermissions_ReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool().(*bashTool)
	ctx := permission.NewContext(permission.ModeDefault)

	// Read-only command should be allowed
	dec := bt.CheckPermissions(map[string]any{"command": "ls -la"}, ctx)
	if dec.Behavior != permission.BehaviorAllow {
		t.Errorf("ls -la: behavior = %s, want allow", dec.Behavior)
	}

	dec = bt.CheckPermissions(map[string]any{"command": "git status"}, ctx)
	if dec.Behavior != permission.BehaviorAllow {
		t.Errorf("git status: behavior = %s, want allow", dec.Behavior)
	}
}

func TestBashTool_CheckPermissions_Dangerous(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool().(*bashTool)
	ctx := permission.NewContext(permission.ModeDefault)

	// Dangerous commands should ask
	dec := bt.CheckPermissions(map[string]any{"command": "rm -rf /"}, ctx)
	if dec.Behavior != permission.BehaviorAsk {
		t.Errorf("rm -rf /: behavior = %s, want ask", dec.Behavior)
	}
	if !dec.BypassImmune {
		t.Error("rm -rf /: should be bypass immune")
	}
}

func TestBashTool_CheckPermissions_InjectionRisk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool().(*bashTool)
	ctx := permission.NewContext(permission.ModeDefault)

	// Injection risk should be caught
	dec := bt.CheckPermissions(map[string]any{"command": "echo $(cat /etc/passwd)"}, ctx)
	if dec.Behavior != permission.BehaviorAsk {
		t.Errorf("injection: behavior = %s, want ask", dec.Behavior)
	}
	if !dec.BypassImmune {
		t.Error("injection: should be bypass immune")
	}
}

func TestBashTool_CheckPermissions_AcceptEdits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool().(*bashTool)
	ctx := permission.NewContext(permission.ModeAcceptEdits)

	// Normal (non-dangerous) commands should be allowed in AcceptEdits mode
	dec := bt.CheckPermissions(map[string]any{"command": "npm install"}, ctx)
	if dec.Behavior != permission.BehaviorAllow {
		t.Errorf("npm install in AcceptEdits: behavior = %s, want allow", dec.Behavior)
	}

	// Dangerous commands should still ask even in AcceptEdits mode
	dec = bt.CheckPermissions(map[string]any{"command": "rm -rf /"}, ctx)
	if dec.Behavior != permission.BehaviorAsk {
		t.Errorf("rm -rf / in AcceptEdits: behavior = %s, want ask", dec.Behavior)
	}
}

func TestBashTool_CheckReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool().(*bashTool)

	if !bt.CheckReadOnly(map[string]any{"command": "ls -la"}) {
		t.Error("ls -la should be read-only")
	}
	if bt.CheckReadOnly(map[string]any{"command": "rm file.txt"}) {
		t.Error("rm should not be read-only")
	}
}

func TestBashTool_MatchRule(t *testing.T) {
	bt := BashTool().(*bashTool)

	tests := []struct {
		rule    string
		cmd     string
		matches bool
	}{
		{"", "anything", true},
		{"git *", "git status", true},
		{"git *", "git push", true},
		{"git *", "npm install", false},
		{"npm install:*", "npm install express", true},
		{"npm install:*", "npm test", false},
		{"echo hello", "echo hello", true},
		{"echo hello", "echo world", false},
	}

	for _, tt := range tests {
		got := bt.MatchRule(tt.rule, map[string]any{"command": tt.cmd})
		if got != tt.matches {
			t.Errorf("MatchRule(%q, %q) = %v, want %v", tt.rule, tt.cmd, got, tt.matches)
		}
	}
}

func TestBashTool_GenerateSuggestions(t *testing.T) {
	bt := BashTool().(*bashTool)

	rules := bt.GenerateSuggestions(map[string]any{"command": "git status"})
	if len(rules) == 0 {
		t.Fatal("expected at least one suggestion")
	}
	found := false
	for _, r := range rules {
		if r.RuleContent == "git status:*" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'git status:*' suggestion, got %v", rules)
	}
}

func TestWriteTool_CheckPermissions(t *testing.T) {
	wt := WriteTool().(*writeTool)

	// Dangerous path should ask
	dec := wt.CheckPermissions(map[string]any{"file_path": ".env"}, permission.NewContext(permission.ModeDefault))
	if dec.Behavior != permission.BehaviorAsk {
		t.Errorf(".env write: behavior = %s, want ask", dec.Behavior)
	}
	if !dec.BypassImmune {
		t.Error(".env write: should be bypass immune")
	}

	// Normal path in AcceptEdits should allow
	dec = wt.CheckPermissions(map[string]any{"file_path": "src/main.go"}, permission.NewContext(permission.ModeAcceptEdits))
	if dec.Behavior != permission.BehaviorAllow {
		t.Errorf("main.go in AcceptEdits: behavior = %s, want allow", dec.Behavior)
	}

	// Normal path in Default should passthrough
	dec = wt.CheckPermissions(map[string]any{"file_path": "src/main.go"}, permission.NewContext(permission.ModeDefault))
	if dec.Behavior != permission.BehaviorPassthrough {
		t.Errorf("main.go in Default: behavior = %s, want passthrough", dec.Behavior)
	}
}

func TestEditTool_CheckPermissions(t *testing.T) {
	et := EditTool().(*editTool)

	// Dangerous path should ask
	dec := et.CheckPermissions(map[string]any{"file_path": ".bashrc"}, permission.NewContext(permission.ModeDefault))
	if dec.Behavior != permission.BehaviorAsk {
		t.Errorf(".bashrc edit: behavior = %s, want ask", dec.Behavior)
	}

	// Normal path in AcceptEdits should allow
	dec = et.CheckPermissions(map[string]any{"file_path": "src/main.go"}, permission.NewContext(permission.ModeAcceptEdits))
	if dec.Behavior != permission.BehaviorAllow {
		t.Errorf("main.go in AcceptEdits: behavior = %s, want allow", dec.Behavior)
	}
}

func TestReadTool_CheckPermissions(t *testing.T) {
	rt := ReadTool().(*readTool)

	// Read should always passthrough
	dec := rt.CheckPermissions(map[string]any{"file_path": ".env"}, permission.NewContext(permission.ModeDefault))
	if dec.Behavior != permission.BehaviorPassthrough {
		t.Errorf("read .env: behavior = %s, want passthrough", dec.Behavior)
	}
}

func TestWriteTool_MatchRule(t *testing.T) {
	wt := WriteTool().(*writeTool)

	// Create temp to get absolute path resolution working
	dir, _ := os.MkdirTemp("", "test-match-*")
	defer os.RemoveAll(dir)
	abs := filepath.Join(dir, "test.go")

	// Empty rule matches all
	if !wt.MatchRule("", map[string]any{"file_path": abs}) {
		t.Error("empty rule should match all")
	}

	// Base name match
	if !wt.MatchRule("*.go", map[string]any{"file_path": abs}) {
		t.Error("*.go should match test.go")
	}

	if wt.MatchRule("*.txt", map[string]any{"file_path": abs}) {
		t.Error("*.txt should not match test.go")
	}
}

func TestWriteTool_GenerateSuggestions(t *testing.T) {
	wt := WriteTool().(*writeTool)

	rules := wt.GenerateSuggestions(map[string]any{"file_path": "/home/user/project/src/main.go"})
	if len(rules) == 0 {
		t.Fatal("expected suggestion")
	}
	if rules[0].ToolName != "Write" {
		t.Errorf("tool_name = %q, want 'Write'", rules[0].ToolName)
	}
	if rules[0].RuleContent == "" {
		t.Error("rule_content should contain parent dir pattern")
	}
}

func TestReadTool_LineTruncation(t *testing.T) {
	// Create a file with a very long line
	dir := t.TempDir()
	path := filepath.Join(dir, "longline.txt")
	longLine := make([]byte, 3000)
	for i := range longLine {
		longLine[i] = 'A'
	}
	content := string(longLine) + "\nshort line\n"
	_ = os.WriteFile(path, []byte(content), 0o644)

	rt := ReadTool()
	resp, err := rt.Execute(context.Background(), map[string]any{"file_path": path})
	if err != nil {
		t.Fatal(err)
	}
	text := getResponseText(resp)

	// The long line should be truncated
	if !containsCI(text, "[truncated]") {
		t.Error("expected [truncated] marker in output for long line")
	}

	// The short line should be intact
	if !containsCI(text, "short line") {
		t.Error("short line should be intact")
	}
}
