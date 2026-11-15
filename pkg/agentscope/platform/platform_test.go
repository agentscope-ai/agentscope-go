package platform

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestShellTypeString(t *testing.T) {
	cases := []struct {
		st   ShellType
		want string
	}{
		{ShellBash, "bash"},
		{ShellZsh, "zsh"},
		{ShellSh, "sh"},
		{ShellPowerShell, "powershell"},
		{ShellCmd, "cmd"},
	}
	for _, tc := range cases {
		if got := tc.st.String(); got != tc.want {
			t.Errorf("ShellType(%d).String() = %q, want %q", tc.st, got, tc.want)
		}
	}
}

func TestDeriveExecArgsBash(t *testing.T) {
	s := Shell{Type: ShellBash, Path: "/bin/bash"}
	args := s.DeriveExecArgs("echo hello")
	if len(args) != 3 || args[0] != "/bin/bash" || args[1] != "-c" || args[2] != "echo hello" {
		t.Errorf("DeriveExecArgs = %v, want [/bin/bash -c echo hello]", args)
	}
}

func TestDeriveExecArgsZsh(t *testing.T) {
	s := Shell{Type: ShellZsh, Path: "/bin/zsh"}
	args := s.DeriveExecArgs("ls -la")
	if len(args) != 3 || args[0] != "/bin/zsh" || args[1] != "-c" {
		t.Errorf("DeriveExecArgs = %v, want [/bin/zsh -c ...]", args)
	}
}

func TestDeriveExecArgsPowerShell(t *testing.T) {
	s := Shell{Type: ShellPowerShell, Path: "pwsh"}
	args := s.DeriveExecArgs("Get-Process")
	if len(args) != 4 || args[0] != "pwsh" || args[1] != "-NoProfile" || args[2] != "-Command" || args[3] != "Get-Process" {
		t.Errorf("DeriveExecArgs = %v, want [pwsh -NoProfile -Command Get-Process]", args)
	}
}

func TestDeriveExecArgsCmd(t *testing.T) {
	s := Shell{Type: ShellCmd, Path: "cmd.exe"}
	args := s.DeriveExecArgs("dir")
	if len(args) != 3 || args[0] != "cmd.exe" || args[1] != "/c" || args[2] != "dir" {
		t.Errorf("DeriveExecArgs = %v, want [cmd.exe /c dir]", args)
	}
}

func TestIsUnixShell(t *testing.T) {
	if !(Shell{Type: ShellBash}).IsUnix() {
		t.Error("bash should be unix")
	}
	if !(Shell{Type: ShellZsh}).IsUnix() {
		t.Error("zsh should be unix")
	}
	if (Shell{Type: ShellCmd}).IsUnix() {
		t.Error("cmd should not be unix")
	}
	if (Shell{Type: ShellPowerShell}).IsUnix() {
		t.Error("powershell should not be unix")
	}
}

func TestDetectReturnsValidShell(t *testing.T) {
	s := Detect()
	if s.Path == "" {
		t.Error("Detect() returned empty path")
	}
	if s.Type.String() == "unknown" {
		t.Error("Detect() returned unknown shell type")
	}
}

func TestDetectShellTypeFromPath(t *testing.T) {
	cases := []struct {
		path string
		want ShellType
	}{
		{"/bin/bash", ShellBash},
		{"/usr/bin/bash", ShellBash},
		{"/bin/zsh", ShellZsh},
		{"/usr/local/bin/zsh", ShellZsh},
		{"/bin/sh", ShellSh},
		{"C:\\Windows\\System32\\cmd.exe", ShellCmd},
		{"cmd.exe", ShellCmd},
		{"pwsh", ShellPowerShell},
		{"powershell.exe", ShellPowerShell},
		{"C:\\Program Files\\PowerShell\\7\\pwsh.exe", ShellPowerShell},
	}
	for _, tc := range cases {
		got := DetectShellTypeFromPath(tc.path)
		if got != tc.want {
			t.Errorf("DetectShellTypeFromPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	got := NormalizePath("a/b/c")
	if strings.Contains(got, "\\") && runtime.GOOS != "windows" {
		t.Errorf("NormalizePath produced backslashes on non-windows: %q", got)
	}
}

func TestPowerShellDangerousPatterns(t *testing.T) {
	dangerous := []string{
		"Remove-Item C:\\Users -Recurse -Force",
		"Format-Volume -DriveLetter C",
		"Stop-Process -Name explorer -Force",
		"Set-ExecutionPolicy Unrestricted",
		"Invoke-Expression $malicious",
		`Start-Process cmd -Verb RunAs`,
	}
	for _, cmd := range dangerous {
		if ok, _ := CheckPowerShellDangerous(cmd); !ok {
			t.Errorf("should be dangerous: %q", cmd)
		}
	}

	safe := []string{
		"Get-ChildItem .",
		"Get-Process",
		"Write-Host 'hello'",
		"Select-String -Pattern 'foo' -Path *.go",
	}
	for _, cmd := range safe {
		if ok, _ := CheckPowerShellDangerous(cmd); ok {
			t.Errorf("should not be dangerous: %q", cmd)
		}
	}
}

// TestCommandRunsViaDetectedShell is a cross-platform smoke test: whatever
// shell Detect() picks (sh/bash on Unix, pwsh/powershell/cmd on Windows),
// commands must run and produce output — including arguments with embedded
// spaces, the classic breakage zone for argv construction through
// pwsh -Command / cmd /c (evaluator M9).
func TestCommandRunsViaDetectedShell(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := Command(ctx, `echo "agentscope platform smoke with spaces"`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Command failed: %v (output: %s)", err, out)
	}
	// The unquoted substring must survive on every shell: sh/bash/zsh and
	// pwsh strip the quotes; cmd.exe keeps them but the words still appear
	// contiguously.
	if !strings.Contains(string(out), "agentscope platform smoke with spaces") {
		t.Errorf("output = %q, want it to contain the spaced marker", out)
	}
}

func TestDetectSkipsNonPOSIXShellEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("$SHELL handling is Unix-only")
	}
	// fish/tcsh are not POSIX: detect() must not invoke them as
	// "<shell> -c <posix command>" (evaluator M4) — it falls back to the
	// bash/zsh/sh chain instead.
	for _, shell := range []string{"/usr/bin/fish", "/usr/local/bin/tcsh", "/opt/xonsh"} {
		t.Setenv("SHELL", shell)
		got := detect()
		if got.Path == shell {
			t.Errorf("SHELL=%s: detect() returned the non-POSIX shell %q (type %s)", shell, got.Path, got.Type)
		}
		switch got.Type {
		case ShellBash, ShellZsh, ShellSh:
		default:
			t.Errorf("SHELL=%s: detect() returned non-Unix shell type %s", shell, got.Type)
		}
	}
	// Known shells are still honored.
	t.Setenv("SHELL", "/bin/zsh")
	if got := detect(); got.Type != ShellZsh || got.Path != "/bin/zsh" {
		t.Errorf("SHELL=/bin/zsh: got %+v", got)
	}
}
