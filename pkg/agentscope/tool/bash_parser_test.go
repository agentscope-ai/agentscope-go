package tool

import (
	"runtime"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
)

func TestIsReadOnlyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	tests := []struct {
		cmd      string
		readOnly bool
	}{
		// Simple read-only commands
		{"ls -la", true},
		{"cat file.txt", true},
		{"head -n 10 file.txt", true},
		{"tail -f log.txt", true},
		{"grep pattern file.txt", true},
		{"find . -name '*.go'", true},
		{"wc -l file.txt", true},
		{"echo hello", true},
		{"pwd", true},
		{"whoami", true},
		{"date", true},
		{"env", true},
		{"ps aux", true},
		{"du -sh .", true},
		{"df -h", true},
		{"tree .", true},

		// Git read-only subcommands
		{"git status", true},
		{"git log --oneline", true},
		{"git diff", true},
		{"git branch -a", true},
		{"git show HEAD", true},
		{"git ls-files", true},

		// Git write subcommands
		{"git push", false},
		{"git commit -m test", false},
		{"git checkout -b new-branch", false},
		{"git merge main", false},
		{"git rebase main", false},

		// Write commands
		{"rm file.txt", false},
		{"mv a.txt b.txt", false},
		{"cp a.txt b.txt", false},
		{"mkdir new_dir", false},
		{"touch new_file", false},
		{"chmod 755 script.sh", false},

		// Complex pipelines (all parts must be read-only)
		{"ls | grep test", true},
		{"cat file.txt | wc -l", true},
		{"ps aux | grep node", true},

		// Pipes with non-read-only commands
		{"ls | rm -f", false},

		// Empty/invalid
		{"", false},
	}

	for _, tt := range tests {
		got := IsReadOnlyCommand(tt.cmd)
		if got != tt.readOnly {
			t.Errorf("IsReadOnlyCommand(%q) = %v, want %v", tt.cmd, got, tt.readOnly)
		}
	}
}

func TestCheckInjectionRisk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	tests := []struct {
		cmd    string
		risky  bool
		substr string // expected substring in reason, empty if not risky
	}{
		// Safe commands
		{"ls -la", false, ""},
		{"echo hello", false, ""},
		{"cat file.txt", false, ""},
		{"grep pattern file.txt", false, ""},

		// Command substitution
		{"echo $(whoami)", true, "command substitution"},
		{"echo `hostname`", true, "command substitution"},

		// eval/exec
		{"eval 'rm -rf /'", true, "eval"},
		{"exec /bin/sh", true, "exec"},

		// source
		{"source script.sh", true, "source"},
		{". ./script.sh", true, "source"},
	}

	for _, tt := range tests {
		risky, reason := CheckInjectionRisk(tt.cmd)
		if risky != tt.risky {
			t.Errorf("CheckInjectionRisk(%q) risky = %v, want %v (reason: %s)", tt.cmd, risky, tt.risky, reason)
		}
		if tt.risky && tt.substr != "" {
			if reason == "" || !containsSubstring(reason, tt.substr) {
				t.Errorf("CheckInjectionRisk(%q) reason = %q, want to contain %q", tt.cmd, reason, tt.substr)
			}
		}
	}
}

func TestCheckDangerousRemoval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	tests := []struct {
		cmd       string
		dangerous bool
	}{
		{"rm file.txt", false},
		{"rm -f file.txt", false},
		{"rm -rf /", true},
		{"rm -rf /etc", true},
		{"rm -rf /home", true},
		{"rm -rf ~", true},
		{"rm -r -f important/", true},
		{"ls -la", false},
		{"echo hello", false},
	}

	for _, tt := range tests {
		got, _ := CheckDangerousRemoval(tt.cmd)
		if got != tt.dangerous {
			t.Errorf("CheckDangerousRemoval(%q) = %v, want %v", tt.cmd, got, tt.dangerous)
		}
	}
}

func TestExtractFilePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	tests := []struct {
		cmd   string
		paths []string
	}{
		{"rm file.txt", []string{"file.txt"}},
		{"rm -f a.txt b.txt", []string{"a.txt", "b.txt"}},
		{"cp src.txt dst.txt", []string{"src.txt", "dst.txt"}},
		{"mv old.txt new.txt", []string{"old.txt", "new.txt"}},
		{"chmod 755 script.sh", []string{"script.sh"}},
		{"mkdir -p new/dir", []string{"new/dir"}},
		{"ls -la", nil},     // ls is not a file command
		{"echo hello", nil}, // echo is not a file command
		{"touch new.txt", []string{"new.txt"}},
	}

	for _, tt := range tests {
		got := ExtractFilePaths(tt.cmd)
		if len(got) != len(tt.paths) {
			t.Errorf("ExtractFilePaths(%q) = %v (len %d), want %v (len %d)", tt.cmd, got, len(got), tt.paths, len(tt.paths))
			continue
		}
		for i := range got {
			if got[i] != tt.paths[i] {
				t.Errorf("ExtractFilePaths(%q)[%d] = %q, want %q", tt.cmd, i, got[i], tt.paths[i])
			}
		}
	}
}

func TestExtractCommandPrefixes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	tests := []struct {
		cmd      string
		max      int
		expected []string
	}{
		{"git status", 3, []string{"git status:*"}},
		{"npm install", 3, []string{"npm install:*"}},
		{"ls -la", 3, []string{"ls:*"}},
		{"echo hello && cat file.txt", 3, []string{"echo hello:*", "cat file.txt:*"}},
	}

	for _, tt := range tests {
		got := ExtractCommandPrefixes(tt.cmd, tt.max)
		if len(got) != len(tt.expected) {
			t.Errorf("ExtractCommandPrefixes(%q, %d) = %v, want %v", tt.cmd, tt.max, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("ExtractCommandPrefixes(%q, %d)[%d] = %q, want %q", tt.cmd, tt.max, i, got[i], tt.expected[i])
			}
		}
	}
}

func TestCheckSedConstraints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	dangerousFiles := []string{".bashrc", ".env"}

	tests := []struct {
		cmd       string
		violation bool
		substr    string
	}{
		// Safe sed: no -i, no dangerous files
		{"sed 's/foo/bar/' file.txt", false, ""},
		{"sed -n '1,10p' file.txt", false, ""},
		{"sed -e 's/a/b/' file.txt", false, ""},

		// In-place edit
		{"sed -i 's/foo/bar/' file.txt", true, "in-place"},
		{"sed --in-place 's/foo/bar/' file.txt", true, "in-place"},
		{"sed -i.bak 's/foo/bar/' file.txt", true, "in-place"},

		// Dangerous target file
		{"sed 's/foo/bar/' .bashrc", true, "dangerous file"},
		{"sed 's/foo/bar/' .env", true, "dangerous file"},

		// Non-sed command
		{"grep pattern file", false, ""},
	}

	for _, tt := range tests {
		violation, reason := CheckSedConstraints(tt.cmd, dangerousFiles)
		if violation != tt.violation {
			t.Errorf("CheckSedConstraints(%q) violation = %v, want %v (reason: %s)", tt.cmd, violation, tt.violation, reason)
		}
		if tt.violation && tt.substr != "" {
			if !containsSubstring(reason, tt.substr) {
				t.Errorf("CheckSedConstraints(%q) reason = %q, want to contain %q", tt.cmd, reason, tt.substr)
			}
		}
	}
}

func TestIsPowerShellReadOnly(t *testing.T) {
	tests := []struct {
		cmd      string
		readOnly bool
	}{
		{"Get-ChildItem", true},
		{"gci -Path .", true},
		{"Get-Content file.txt", true},
		{"Get-Process | Format-Table", true},
		{"Get-ChildItem | Where-Object { $_.Length -gt 0 } | Sort-Object", true},
		{"Write-Host hello", true},
		{"Remove-Item file.txt", false},
		{"Set-Content file.txt -Value test", false},
		{"New-Item -Path test.txt", false},
		{"Stop-Process -Id 1234", false},
		{"", false},
	}

	for _, tt := range tests {
		got := isPowerShellReadOnly(tt.cmd)
		if got != tt.readOnly {
			t.Errorf("isPowerShellReadOnly(%q) = %v, want %v", tt.cmd, got, tt.readOnly)
		}
	}
}

func TestCheckWindowsReadOnlyCmd(t *testing.T) {
	tests := []struct {
		cmd      string
		readOnly bool
	}{
		{"dir", true},
		{"type file.txt", true},
		{"find /i test", true},
		{"echo hello", true},
		{"del file.txt", false},
		{"move a.txt b.txt", false},
		{"", false},
	}

	for _, tt := range tests {
		got := checkWindowsReadOnly(tt.cmd, platform.ShellCmd)
		if got != tt.readOnly {
			t.Errorf("checkWindowsReadOnly(%q, Cmd) = %v, want %v", tt.cmd, got, tt.readOnly)
		}
	}
}

func TestPowerShellInjectionPatterns(t *testing.T) {
	tests := []struct {
		cmd   string
		risky bool
	}{
		{"Invoke-Expression 'Get-Date'", true},
		{"iex 'something'", true},
		{"& $variable", true},
		{"powershell -e ZW5jb2Rl", true},
		{"pwsh -e ZW5jb2Rl", true},
		{"Invoke-Command -ScriptBlock { rm -rf / }", true},
		{"Get-ChildItem", false},
		{"echo hello", false},
	}

	for _, tt := range tests {
		risky := false
		for _, re := range powerShellInjectionPatterns {
			if re.MatchString(tt.cmd) {
				risky = true
				break
			}
		}
		if risky != tt.risky {
			t.Errorf("powerShellInjectionPatterns(%q) risky = %v, want %v", tt.cmd, risky, tt.risky)
		}
	}
}

func TestPowerShellDangerousRemovalPatterns(t *testing.T) {
	tests := []struct {
		cmd       string
		dangerous bool
	}{
		{"Remove-Item C:\\temp -Recurse", true},
		{"del /s C:\\temp", true},
		{"rmdir /s /q C:\\temp", true},
		{"rd /S C:\\temp", true},
		{"Remove-Item file.txt", false},
		{"Get-ChildItem", false},
	}

	for _, tt := range tests {
		dangerous := false
		for _, re := range powerShellDangerousRemovalPatterns {
			if re.MatchString(tt.cmd) {
				dangerous = true
				break
			}
		}
		if dangerous != tt.dangerous {
			t.Errorf("powerShellDangerousRemovalPatterns(%q) dangerous = %v, want %v", tt.cmd, dangerous, tt.dangerous)
		}
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsCI(s, substr))
}

func containsCI(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if toLower(s[i+j]) != toLower(sub[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func toLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}
